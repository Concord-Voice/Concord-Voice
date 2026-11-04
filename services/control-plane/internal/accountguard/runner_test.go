package accountguard

// Task 10 (#3680 PR3): the two-pass runner. The R4 commit table is the oracle, one test per
// row. Every input is built from real rows in the scratch database and every outcome comes
// from decide.Decide itself; rnProve drives each fixture through loadInputTx + Decide + the
// applier (rolled back) before a runner test trusts it, so a wrong fixture cannot pass
// silently. The fence is the real credepoch.Fence over the scratch Redis, observed through
// the cred_epoch:<id> key and a command log; the presence forced clear is the real service.

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

const rnSecret = "synthetic-runner-input-alpha-000001" // pragma: allowlist secret

var (
	errRnAmbiguous  = errors.New("injected ambiguous commit")
	errRnRevalidate = errors.New("injected revalidate failure")
)

// ---------------------------------------------------------------------------
// Recording fakes
// ---------------------------------------------------------------------------

type rnFamilies struct {
	mu     sync.Mutex
	closed []uuid.UUID
}

func (f *rnFamilies) DisconnectFamilyContext(_ context.Context, _, family uuid.UUID) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = append(f.closed, family)
	return nil
}

func (f *rnFamilies) got() []uuid.UUID {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uuid.UUID(nil), f.closed...)
}

type rnPerms struct {
	mu    sync.Mutex
	users []string
}

// *rbac.Resolver is the production bumper: its BumpUserPermissionGeneration retries and then
// drops the generation, which the bare PermissionCache.BumpUser does not (#3454).
var _ PermBumper = (*rbac.Resolver)(nil)

func (p *rnPerms) BumpUserPermissionGeneration(_ context.Context, userID string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.users = append(p.users, userID)
	return nil
}

func (p *rnPerms) got() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.users...)
}

type rnMint struct {
	User  uuid.UUID
	Epoch string
}

type rnMinter struct {
	mu    sync.Mutex
	calls []rnMint
}

var rnContinuation = Continuation{AccessToken: "test-access-token", RefreshToken: "test-refresh-token"}

func (m *rnMinter) MintContinuation(_ context.Context, userID uuid.UUID, epoch string) (Continuation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, rnMint{userID, epoch})
	return rnContinuation, nil
}

func (m *rnMinter) got() []rnMint {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]rnMint(nil), m.calls...)
}

// rnSink is both the notice sink and the flags resyncer of the test registry.
type rnSink struct {
	mu      sync.Mutex
	notices int
	resyncs int
	err     error // returned by InsertTx when set
}

func (s *rnSink) InsertTx(context.Context, *sql.Tx, uuid.UUID, map[int]uuid.UUID, []decide.Notice) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices++
	return s.err
}

func (s *rnSink) ResyncTx(context.Context, *sql.Tx, uuid.UUID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resyncs++
	return nil
}

func (s *rnSink) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.notices, s.resyncs = 0, 0
}

type rnEvict struct {
	mu    sync.Mutex
	calls [][3]string
}

func (e *rnEvict) record(_ context.Context, user, epoch, superseded string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.calls = append(e.calls, [3]string{user, epoch, superseded})
}

func (e *rnEvict) got() [][3]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([][3]string(nil), e.calls...)
}

type rnDelivery struct{ err error }

func (d *rnDelivery) DeliverCustomText(_ context.Context, plan presencehistory.DeliveryPlan) (presencehistory.DeliveryAck, error) {
	if d.err != nil {
		return presencehistory.DeliveryAck{}, d.err
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

// rnCmdLog records the fence's Redis commands on one user's cred_epoch key: "begin" for the
// SET that publishes the blocked marker, "script" for the commit and rollback scripts.
type rnCmdLog struct {
	mu sync.Mutex
	ev []rnEvent
}

type rnEvent struct{ key, kind string }

func (l *rnCmdLog) DialHook(next redis.DialHook) redis.DialHook { return next }

func (l *rnCmdLog) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		l.note(cmd)
		return next(ctx, cmd)
	}
}

func (l *rnCmdLog) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return func(ctx context.Context, cmds []redis.Cmder) error {
		for _, c := range cmds {
			l.note(c)
		}
		return next(ctx, cmds)
	}
}

func (l *rnCmdLog) note(cmd redis.Cmder) {
	args := cmd.Args()
	name := strings.ToLower(cmd.Name())
	idx, kind := -1, ""
	switch name {
	case "set":
		idx, kind = 1, "begin"
	case "eval", "evalsha":
		idx, kind = 3, "script"
	}
	if idx < 0 || len(args) <= idx {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ev = append(l.ev, rnEvent{fmt.Sprint(args[idx]), kind})
}

// seq is the user's fence command sequence with consecutive scripts collapsed (an EVALSHA
// that falls back to EVAL is one script run).
func (l *rnCmdLog) seq(user uuid.UUID) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, e := range l.ev {
		if e.key != credepoch.Key(user.String()) {
			continue
		}
		if e.kind == "script" && len(out) > 0 && out[len(out)-1] == "script" {
			continue
		}
		out = append(out, e.kind)
	}
	return out
}

// ---------------------------------------------------------------------------
// Environment
// ---------------------------------------------------------------------------

type rnEnv struct {
	t        *testing.T
	db       *sql.DB
	rdb      *redis.Client
	cmds     *rnCmdLog
	fence    *credepoch.Fence
	presence *presencehistory.Service
	fam      *rnFamilies
	perms    *rnPerms
	minter   *rnMinter
	sink     *rnSink
	evict    *rnEvict
	deliver  *rnDelivery
	deps     Deps
	reg      registry
	mu       sync.Mutex
	applied  int // applier calls, all kinds
}

func (e *rnEnv) appliedCalls() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.applied
}

func newRnEnv(t *testing.T) *rnEnv {
	t.Helper()
	db := snapDB(t)
	rdb := redistest.Client(t)
	e := &rnEnv{
		t: t, db: db, rdb: rdb, cmds: &rnCmdLog{}, fam: &rnFamilies{}, perms: &rnPerms{},
		minter: &rnMinter{}, sink: &rnSink{}, evict: &rnEvict{}, deliver: &rnDelivery{},
	}
	rdb.AddHook(e.cmds)
	e.fence = credepoch.New(db, rdb, logger.NewWithWriter(io.Discard))
	e.fence.SetPostCommitEvictor(e.evict.record)
	e.presence = presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, e.presence.BindDelivery(e.deliver))
	e.deps = Deps{
		DB: db, Keys: NewKeyRing(db, rnSecret), Fence: e.fence, Presence: e.presence,
		Families: e.fam, Perms: e.perms, Minter: e.minter,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	e.reg = productionRegistry()
	e.reg.notices = e.sink
	e.reg.resync = e.sink
	// productionRegistry leaves EffectResyncMFAFlags present but unwired (PR6 owns it), so
	// decide's remove/contest follow-up would be refused whole. The test registry wires it to
	// the recording resyncer, which is the only way a PermGenBump outcome can be applied here.
	e.reg.effects[effectKey{Kind: decide.EffectResyncMFAFlags}] = wiredApplier(
		func(ctx context.Context, tx *sql.Tx, a applyCtx, _ decide.Effect) error {
			return e.sink.ResyncTx(ctx, tx, a.userID)
		})
	// Every wired applier records its calls, so a test can prove covers refused before any
	// effect was applied.
	for k, ap := range e.reg.effects {
		if !ap.wired {
			continue
		}
		inner := ap.apply
		ap.apply = func(ctx context.Context, tx *sql.Tx, a applyCtx, ef decide.Effect) error {
			e.mu.Lock()
			e.applied++
			e.mu.Unlock()
			return inner(ctx, tx, a, ef)
		}
		e.reg.effects[k] = ap
	}
	// The marker is a singleton the truncate does not reset: leave it unset after every test.
	t.Cleanup(func() {
		_, err := db.Exec(`UPDATE accountguard_state SET activated_at = NULL`)
		assert.NoError(t, err, "reset the activation marker")
	})
	return e
}

func (e *rnEnv) runner() *Runner { return newTestRunner(e.deps, e.reg) }

// rnAccount is a user with one factor of every kind, a live session family, and a presence
// settings row carrying a Custom Status.
type rnAccount struct {
	snapAccount
	ident loginfamily.AccessIdentity
	fam   uuid.UUID
}

func (e *rnEnv) account(webM snapMaturity) rnAccount {
	t := e.t
	t.Helper()
	leg := snapLegacyMaturity()
	acct := snapBuild(t, e.db, newSnapUser(), leg, webM, leg, leg)
	ctx := testdb.ActorContext(context.Background(), t, e.db, acct.user.id.String())
	ident, ok := loginfamily.AccessIdentityFromContext(ctx)
	require.True(t, ok)
	var fam uuid.UUID
	require.NoError(t, e.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid = $1`, ident.SID).Scan(&fam))
	_, err := e.db.Exec(`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text) VALUES ($1, 1, 'visible')`, acct.user.id)
	require.NoError(t, err)
	return rnAccount{snapAccount: acct, ident: ident, fam: fam}
}

func (a rnAccount) loginProof() decide.Proof {
	return decide.Proof{Used: []decide.CredRef{a.user.pwRef, a.totp}}
}

func (a rnAccount) id() uuid.UUID { return a.user.id }

// rnImmature is a WebAuthn enrolled an hour ago at C2 that matures in two days: contestable.
func rnImmature() snapMaturity {
	now := time.Now()
	return snapMaturity{maturesAt: now.Add(48 * time.Hour), enrolledAt: now.Add(-time.Hour), enrolClass: decide.C2}
}

// rnPendingCancelReq inserts a pending credential_remove of the TOTP at origin class C1.
func (e *rnEnv) pendingRemove(a rnAccount) uuid.UUID {
	r := snapOpenReq(decide.ReqCredentialRemove, a.totp)
	r.originClass = decide.C1
	snapInsertReq(e.t, e.db, a.id(), r)
	return r.id
}

// breakGlass inserts a due, un-held break_glass at C2 that resets factors outside its
// origins (the address), with a pending password.
func (e *rnEnv) breakGlass(a rnAccount) uuid.UUID {
	opened := time.Now().Add(-10 * 24 * time.Hour)
	r := snapOpenReq(decide.ReqBreakGlass, decide.CredRef{})
	r.originClass = decide.C2
	r.resetFactors = true
	r.openedAt = opened
	r.completesAt = opened.Add(decide.DeltaCP)
	r.origins = []decide.CredRef{a.user.addrRef}
	snapInsertReq(e.t, e.db, a.id(), r)
	_, err := e.db.Exec(`UPDATE account_pending_requests SET pending_password_hash = $1 WHERE id = $2`, appPendingHash, r.id)
	require.NoError(e.t, err)
	_, err = e.db.Exec(`INSERT INTO presence_override_preferences (user_id, category, encrypted_data) VALUES ($1, 'custom_text', 'x')`, a.id())
	require.NoError(e.t, err)
	return r.id
}

// rnRev is a scripted Revalidate: call n returns proofs[min(n, last)].
type rnRev struct {
	mu     sync.Mutex
	calls  int
	proofs []decide.Proof
	nows   []time.Time
	failOn int // 1-based call that returns an error; 0 never
}

func (r *rnRev) fn() func(context.Context, *sql.Tx, time.Time) (decide.Proof, error) {
	return func(_ context.Context, _ *sql.Tx, now time.Time) (decide.Proof, error) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.calls++
		r.nows = append(r.nows, now)
		if r.failOn == r.calls {
			return decide.Proof{}, errRnRevalidate
		}
		i := min(r.calls-1, len(r.proofs)-1)
		return r.proofs[i], nil
	}
}

func (r *rnRev) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (e *rnEnv) req(a rnAccount, act decide.Action, rev *rnRev, session bool) Request {
	r := Request{UserID: a.id(), Act: act, Revalidate: rev.fn()}
	if session {
		id := a.ident
		r.Session = &id
		r.TokenEpoch = id.CredentialEpoch
	}
	return r
}

// rnPolicy wraps a policy, counting calls and recording the clock it was handed.
type rnPolicy struct {
	mu    sync.Mutex
	calls int
	nows  []time.Time
	over  func(n int, in decide.Input) (decide.Outcome, bool)
}

func (p *rnPolicy) fn() func(decide.Input) decide.Outcome {
	return func(in decide.Input) decide.Outcome {
		p.mu.Lock()
		p.calls++
		n := p.calls
		p.nows = append(p.nows, in.Now)
		p.mu.Unlock()
		if p.over != nil {
			if o, ok := p.over(n, in); ok {
				return o
			}
		}
		return decide.Decide(in)
	}
}

func (p *rnPolicy) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// ---------------------------------------------------------------------------
// Fixture proof
// ---------------------------------------------------------------------------

type rnWant struct {
	row                       decide.Row
	rotate, sets, cont, perms bool
}

// prove drives a fixture through the loader, decide.Decide and the applier (rolled back) and
// asserts the row and flags the test depends on. A fixture that cannot produce its row, or
// whose outcome the registry cannot apply, fails here, not inside the runner.
func (e *rnEnv) prove(a rnAccount, act decide.Action, proof decide.Proof, want rnWant) {
	t := e.t
	t.Helper()
	ctx := context.Background()
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	now := time.Now()
	in, err := loadInputTx(ctx, tx, a.id(), snapKeyID, now)
	require.NoError(t, err)
	in.Act, in.Proof = act, proof
	o := decide.Decide(in)
	require.Equal(t, want.row, o.Row, "fixture row")
	require.Equal(t, decide.VerdictApplied, o.Verdict, "fixture verdict")
	require.Equal(t, want.rotate, o.RotateEpoch, "fixture RotateEpoch")
	require.Equal(t, want.sets, o.SetsPassword, "fixture SetsPassword")
	require.Equal(t, want.cont, o.Continuation, "fixture Continuation")
	require.Equal(t, want.perms, o.PermGenBump, "fixture PermGenBump")
	ac := applyCtx{
		userID: a.id(), now: now, outcome: o, reqs: in.Reqs, newGroupID: uuid.New(),
		requestIDs: map[int]uuid.UUID{}, passwordWrites: new(int),
	}
	for i, ef := range o.Effects {
		if ef.Kind == decide.EffectOpen {
			ac.requestIDs[i] = uuid.New()
		}
	}
	require.NoError(t, e.reg.covers(ac), "fixture outcome is applicable")
	for _, i := range applyOrder(o) {
		require.NoError(t, e.reg.apply(ctx, tx, ac, i), "fixture effect %d applies", i)
	}
	require.NoError(t, e.reg.finish(ac))
	// The proof applied the outcome once through the recording sink: a runner test counts the
	// runner's calls only.
	e.sink.reset()
	e.mu.Lock()
	e.applied = 0
	e.mu.Unlock()
}

// ---------------------------------------------------------------------------
// State readers
// ---------------------------------------------------------------------------

func (e *rnEnv) epoch(id uuid.UUID) string {
	var s string
	require.NoError(e.t, e.db.QueryRow(`SELECT COALESCE(credential_epoch, '') FROM users WHERE id = $1`, id).Scan(&s))
	return s
}

// fenceState is the user's cred_epoch:<id> value, "" when the key is absent.
func (e *rnEnv) fenceState(id uuid.UUID) string {
	v, err := e.rdb.Get(context.Background(), credepoch.Key(id.String())).Result()
	if errors.Is(err, redis.Nil) {
		return ""
	}
	require.NoError(e.t, err)
	return v
}

func (e *rnEnv) familyTerminal(fam uuid.UUID) bool {
	var term bool
	require.NoError(e.t, e.db.QueryRow(`SELECT terminal_at IS NOT NULL FROM login_families WHERE family_id = $1`, fam).Scan(&term))
	return term
}

func (e *rnEnv) reqStatus(id uuid.UUID) string { return appReadReq(e.t, e.db, id).Status }

func (e *rnEnv) customTier(id uuid.UUID) int {
	var n int
	require.NoError(e.t, e.db.QueryRow(`SELECT custom_text_tier FROM user_presence_settings WHERE user_id = $1`, id).Scan(&n))
	return n
}

func (e *rnEnv) count(query string, args ...any) int {
	var n int
	require.NoError(e.t, e.db.QueryRow(query, args...).Scan(&n))
	return n
}

func (e *rnEnv) webauthnExists(a rnAccount) bool {
	return e.count(`SELECT COUNT(*) FROM user_mfa_webauthn WHERE id = $1`, a.web.ID) == 1
}

func (e *rnEnv) webauthnContested(a rnAccount) bool {
	return e.count(`SELECT COUNT(*) FROM user_mfa_webauthn WHERE id = $1 AND maturity_contested_at IS NOT NULL`, a.web.ID) == 1
}

func (e *rnEnv) ssoExists(a rnAccount) bool {
	return e.count(`SELECT COUNT(*) FROM user_sso_identities WHERE id = $1`, a.sso.ID) == 1
}

// ---------------------------------------------------------------------------
// R4 table, plain strategy
// ---------------------------------------------------------------------------

// Row: plain / definite / RotateEpoch yes. T1: Commit, close, no permgen (no flag), Minter.
func TestRunner_PlainDefiniteRotateCommitsClosesAndMints(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	prior := e.epoch(a.id())

	var heldAtRevalidate, heldAtCommit []string
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.beforeCommit = func() { heldAtCommit = append(heldAtCommit, e.fenceState(a.id())) }
	req := e.req(a, decide.SignOutOthers{}, rev, true)
	inner := req.Revalidate
	req.Revalidate = func(ctx context.Context, tx *sql.Tx, now time.Time) (decide.Proof, error) {
		heldAtRevalidate = append(heldAtRevalidate, e.fenceState(a.id()))
		return inner(ctx, tx, now)
	}

	res, err := r.Run(context.Background(), req)
	require.NoError(t, err)

	assert.Equal(t, decide.RowT1, res.Outcome.Row)
	require.NotNil(t, res.Continuation)
	assert.Equal(t, rnContinuation, *res.Continuation)
	now := e.epoch(a.id())
	assert.NotEqual(t, prior, now)
	assert.NotEmpty(t, now)
	assert.Equal(t, "active:"+now, e.fenceState(a.id()), "Commit publishes the new epoch")
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "one Begin, one Commit, no Rollback")
	assert.Equal(t, []rnMint{{a.id(), now}}, e.minter.got(), "minted bound to the committed epoch")
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got(), "the session's family is closed")
	assert.True(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.perms.got(), "T1 has no permgen flag")
	assert.Equal(t, 1, e.customTier(a.id()), "no forced clear without SetsPassword")
	// Redis is touched only outside the transaction: pass 1 saw no marker, pass 2 did.
	require.Len(t, heldAtRevalidate, 2)
	assert.Empty(t, heldAtRevalidate[0])
	assert.True(t, strings.HasPrefix(heldAtRevalidate[1], "blocked:"), "Begin precedes the pass-2 transaction")
	require.Len(t, heldAtCommit, 1)
	assert.True(t, strings.HasPrefix(heldAtCommit[0], "blocked:"), "the marker is held at commit")
}

// Row: plain / definite / RotateEpoch yes, permgen if flag, no continuation. T1b.
func TestRunner_PlainDefiniteRotateWithPermgenBumpsAndMintsNothing(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(rnImmature())
	proof := a.loginProof()
	e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})

	res, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.NoError(t, err)

	assert.Nil(t, res.Continuation)
	assert.Empty(t, e.minter.got(), "T1b carries no continuation")
	assert.Equal(t, []string{a.id().String()}, e.perms.got())
	assert.True(t, e.webauthnContested(a), "the contest is written")
	assert.Equal(t, "active:"+e.epoch(a.id()), e.fenceState(a.id()))
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got())
	assert.Equal(t, 1, e.sink.notices)
	assert.Equal(t, 1, e.sink.resyncs)
}

// Row: plain / definite / no rotate, no permgen flag. T7 cancel.
func TestRunner_PlainDefiniteNoRotateTouchesNoFence(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	act := decide.Cancel{IDs: []string{reqID.String()}}
	proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7})
	prior := e.epoch(a.id())

	res, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.NoError(t, err)

	assert.Equal(t, decide.RowT7, res.Outcome.Row)
	assert.Nil(t, res.Continuation)
	assert.Equal(t, "cancelled", e.reqStatus(reqID))
	assert.Empty(t, e.cmds.seq(a.id()), "no fence command at all")
	assert.Empty(t, e.fenceState(a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Empty(t, e.fam.got())
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.minter.got())
	assert.Equal(t, 1, e.sink.notices)
}

// Row: plain / definite / no rotate, permgen if flag. T2 immediate removal.
func TestRunner_PlainDefiniteNoRotateWithPermgenBumps(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	act := decide.Remove{Target: a.sso}
	proof := a.loginProof()
	e.prove(a, act, proof, rnWant{row: decide.RowT2, perms: true})

	_, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.NoError(t, err)

	assert.False(t, e.ssoExists(a), "the removal is written")
	assert.Equal(t, []string{a.id().String()}, e.perms.got())
	assert.Empty(t, e.cmds.seq(a.id()))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.minter.got())
	assert.Equal(t, 1, e.sink.resyncs)
}

// Brief extra: a non-rotating outcome never begins an op, at any instant.
func TestRunner_NonRotatingNeverBegins(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	act := decide.Cancel{IDs: []string{reqID.String()}}
	proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7})

	var duringCommit string
	r := e.runner()
	r.beforeCommit = func() { duringCommit = e.fenceState(a.id()) }
	_, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, false))
	require.NoError(t, err)

	assert.Equal(t, "cancelled", e.reqStatus(reqID))
	assert.Empty(t, duringCommit, "no blocked: marker while committing")
	assert.NotContains(t, e.fenceState(a.id()), "blocked:")
	assert.Empty(t, e.cmds.seq(a.id()))
}

// Row: plain / ambiguous / rotate: ReconcileCommittedEviction (marker kept, evictor asked),
// families closed, no minter.
func TestRunner_PlainAmbiguousRotateReconcilesAndCloses(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	prior := e.epoch(a.id())

	r := e.runner()
	r.commitPlain = func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return errRnAmbiguous
	}
	res, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.Error(t, err)
	rnAssertUnknownCommit(t, err)

	now := e.epoch(a.id())
	require.NotEqual(t, prior, now, "the injected commit really committed")
	assert.Nil(t, res.Continuation)
	assert.Empty(t, e.minter.got(), "no continuation is minted on an ambiguous commit")
	assert.True(t, strings.HasPrefix(e.fenceState(a.id()), "blocked:"), "neither Commit nor Rollback: the marker stays")
	assert.Equal(t, []string{"begin"}, e.cmds.seq(a.id()))
	assert.Equal(t, [][3]string{{a.id().String(), now, prior}}, e.evict.got(), "ReconcileCommittedEviction asked for the exact generation")
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got(), "possibly-durable families are closed")
}

// Row: plain / ambiguous / no rotate: no fence, no close, permgen if flag.
func TestRunner_PlainAmbiguousNoRotateStillBumpsPermgen(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	act := decide.Remove{Target: a.sso}
	proof := a.loginProof()
	e.prove(a, act, proof, rnWant{row: decide.RowT2, perms: true})

	r := e.runner()
	r.commitPlain = func(tx *sql.Tx) error {
		require.NoError(t, tx.Commit())
		return errRnAmbiguous
	}
	_, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.Error(t, err)
	rnAssertUnknownCommit(t, err)

	assert.False(t, e.ssoExists(a))
	assert.Equal(t, []string{a.id().String()}, e.perms.got())
	assert.Empty(t, e.cmds.seq(a.id()))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.minter.got())
}

// Row: any / definite rollback / rotating, op held: Rollback, no close, no permgen, no mint.
func TestRunner_DefiniteRollbackAfterBeginRollsFenceBack(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(rnImmature())
	proof := a.loginProof()
	e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})
	prior := e.epoch(a.id())

	rev := &rnRev{proofs: []decide.Proof{proof}, failOn: 2} // pass 2 fails after Begin
	res, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, rev, true))
	require.ErrorIs(t, err, errRnRevalidate)
	assert.Equal(t, 2, rev.count(), "the failure came from pass 2")

	assert.Nil(t, res.Continuation)
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()))
	assert.Empty(t, e.fenceState(a.id()), "Rollback clears the blocked marker")
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.webauthnContested(a), "nothing written")
	assert.Empty(t, e.perms.got(), "no permgen on a definite rollback")
	assert.Empty(t, e.fam.got())
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.minter.got())
}

// Row: any / definite rollback with no op held: nothing fenced, nothing bumped.
func TestRunner_DefiniteRollbackWithoutOpTouchesNoFence(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	act := decide.Remove{Target: a.sso}
	proof := a.loginProof()
	e.prove(a, act, proof, rnWant{row: decide.RowT2, perms: true})

	rev := &rnRev{proofs: []decide.Proof{proof}, failOn: 1}
	_, err := e.runner().Run(context.Background(), e.req(a, act, rev, true))
	require.ErrorIs(t, err, errRnRevalidate)
	assert.Equal(t, 1, rev.count())

	assert.True(t, e.ssoExists(a), "rolled back")
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.cmds.seq(a.id()))
	assert.Empty(t, e.fam.got())
}

// ---------------------------------------------------------------------------
// R4 table, forced strategy (SetsPassword): T11 break-glass completion
// ---------------------------------------------------------------------------

func (e *rnEnv) forcedFixture() (rnAccount, uuid.UUID, decide.Action) {
	a := e.account(snapLegacyMaturity())
	reqID := e.breakGlass(a)
	act := decide.Complete{ID: reqID.String()}
	e.prove(a, act, decide.Proof{}, rnWant{row: decide.RowT11, rotate: true, sets: true, perms: true})
	return a, reqID, act
}

func (e *rnEnv) passwordHash(id uuid.UUID) string {
	var h string
	require.NoError(e.t, e.db.QueryRow(`SELECT password_hash FROM users WHERE id = $1`, id).Scan(&h))
	return h
}

// Row: forced / Err==nil / rotate: Commit, close (RequiresDisconnect), permgen, no mint.
func TestRunner_ForcedAcknowledgedCommitsClosesAndBumps(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())

	var heldAtCommit string
	r := e.runner()
	r.beforeCommit = func() { heldAtCommit = e.fenceState(a.id()) }
	res, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
	require.NoError(t, err)

	now := e.epoch(a.id())
	assert.NotEqual(t, prior, now)
	assert.Equal(t, decide.RowT11, res.Outcome.Row)
	assert.Nil(t, res.Continuation)
	assert.Equal(t, appPendingHash, e.passwordHash(a.id()), "the pending password is installed")
	assert.Equal(t, "completed", e.reqStatus(reqID))
	assert.False(t, e.webauthnExists(a), "the reset removes the factor")
	assert.Equal(t, 0, e.customTier(a.id()), "SetsPassword runs the presence forced clear")
	assert.Equal(t, 0, e.count(`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, a.id()), "override preferences are discarded")
	assert.True(t, strings.HasPrefix(heldAtCommit, "blocked:"))
	assert.Equal(t, "active:"+now, e.fenceState(a.id()), "Commit")
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()))
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got())
	assert.Equal(t, []string{a.id().String()}, e.perms.got())
	assert.Empty(t, e.minter.got())
}

// Row: forced / ForcedClearRolledBack / rotate: Rollback, no close, no permgen, no mint.
func TestRunner_ForcedRolledBackRollsFenceBackAndClosesNothing(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())
	priorHash := e.passwordHash(a.id())

	restore := e.presence.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errRnAmbiguous },
	})
	t.Cleanup(restore)
	_, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
	require.Error(t, err)
	rnAssertProvenRollback(t, err)

	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Equal(t, priorHash, e.passwordHash(a.id()))
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()))
	assert.Empty(t, e.fenceState(a.id()), "Rollback")
	assert.Empty(t, e.fam.got(), "a proven rollback closes nothing")
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.minter.got())
	assert.Empty(t, e.evict.got())
}

// Row: forced / other error / rotate: ReconcileCommittedEviction, close, permgen, no mint.
func TestRunner_ForcedOtherErrorReconcilesClosesAndBumps(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())
	e.deliver.err = errors.New("injected delivery failure")

	_, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
	require.Error(t, err)
	rnAssertUnknownCommit(t, err)

	now := e.epoch(a.id())
	require.NotEqual(t, prior, now, "the commit was durable")
	assert.Equal(t, "completed", e.reqStatus(reqID))
	assert.True(t, strings.HasPrefix(e.fenceState(a.id()), "blocked:"), "neither Commit nor Rollback")
	assert.Equal(t, []string{"begin"}, e.cmds.seq(a.id()))
	assert.Equal(t, [][3]string{{a.id().String(), now, prior}}, e.evict.got())
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got(), "RequiresDisconnect: possibly durable")
	assert.Equal(t, []string{a.id().String()}, e.perms.got())
	assert.Empty(t, e.minter.got())
}

// ---------------------------------------------------------------------------
// Named extras
// ---------------------------------------------------------------------------

func rnSetMarker(t *testing.T, e *rnEnv) {
	t.Helper()
	_, err := e.db.Exec(`UPDATE accountguard_state SET activated_at = now()`)
	require.NoError(t, err)
}

func TestRunner_ActivatedMarkerRefusesBeforeDecide(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	prior := e.epoch(a.id())
	rnSetMarker(t, e)

	pol := &rnPolicy{}
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))

	require.ErrorIs(t, err, ErrActivated)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Zero(t, pol.count(), "the policy is never consulted")
	assert.Zero(t, rev.count(), "the proof is never revalidated")
	assert.Empty(t, e.cmds.seq(a.id()), "no Begin")
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.familyTerminal(a.fam))
}

func TestRunner_Pass2IsAuthoritative(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	prior := e.epoch(a.id())

	// Valid on pass 1, stale (nothing proven) on pass 2.
	rev := &rnRev{proofs: []decide.Proof{proof, {}}}
	res, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))

	require.NoError(t, err, "a refusal is an outcome, not an error")
	assert.Equal(t, decide.RowT1, res.Outcome.Row)
	assert.Equal(t, decide.VerdictRefused, res.Outcome.Verdict)
	assert.Nil(t, res.Continuation)
	assert.Equal(t, 2, rev.count(), "pass 2 stands: it is not retried")
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "the op is settled by Rollback")
	assert.Empty(t, e.fenceState(a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.minter.got())
}

func TestRunner_UnwiredRefusedBeforeBegin(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(rnImmature())
	proof := a.loginProof()
	e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})
	prior := e.epoch(a.id())

	// The production registry has no sink and no resyncer: T1b emits both.
	r := newTestRunner(e.deps, productionRegistry())
	rev := &rnRev{proofs: []decide.Proof{proof}}
	_, err := r.Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, rev, true))

	require.ErrorIs(t, err, ErrEffectUnwired)
	status, code := HTTPStatus(err)
	assert.Equal(t, 500, status)
	assert.Equal(t, "guard_effect_unwired", code)
	assert.Equal(t, 1, rev.count(), "pass 2 never starts")
	assert.Empty(t, e.cmds.seq(a.id()), "no Begin")
	assert.False(t, e.webauthnContested(a))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Empty(t, e.perms.got())
}

func TestRunner_RetryAfterAssertionViolationSettlesOp(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})

	bogus := decide.Outcome{
		Row: decide.RowT7, Verdict: decide.VerdictApplied, Source: decide.SourceUserSession,
		Effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: reqID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1}},
	}
	// Call 2 is pass 2 of the first attempt: an outcome with effects that does not rotate.
	pol := &rnPolicy{over: func(n int, _ decide.Input) (decide.Outcome, bool) { return bogus, n == 2 }}
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.policy = pol.fn()
	res, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))

	require.NoError(t, err)
	assert.Equal(t, 4, pol.count(), "pass 1, violating pass 2, pass 1 again, pass 2")
	require.NotNil(t, res.Continuation)
	assert.Equal(t, []string{"begin", "script", "begin", "script"}, e.cmds.seq(a.id()),
		"the violating attempt's op is settled by Rollback before the retry's Begin")
	assert.Equal(t, "pending", e.reqStatus(reqID), "the violating outcome wrote nothing")
	assert.Equal(t, "active:"+e.epoch(a.id()), e.fenceState(a.id()))
	assert.Len(t, e.minter.got(), 1)
}

func TestRunner_RetryExhaustedIsContract(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	proof := a.loginProof()
	prior := e.epoch(a.id())

	bogus := decide.Outcome{
		Row: decide.RowT7, Verdict: decide.VerdictApplied, Source: decide.SourceUserSession,
		Effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: reqID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1}},
	}
	pol := &rnPolicy{over: func(n int, _ decide.Input) (decide.Outcome, bool) { return bogus, n%2 == 0 }}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.ErrorIs(t, err, ErrContract)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 500, status)
	assert.Equal(t, 4, pol.count(), "retried exactly once")
	assert.Equal(t, []string{"begin", "script", "begin", "script"}, e.cmds.seq(a.id()))
	assert.Empty(t, e.fenceState(a.id()))
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.Equal(t, prior, e.epoch(a.id()))
}

func TestRunner_ProductionPolicyRefusesAll(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	act := decide.Cancel{IDs: []string{reqID.String()}}
	proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7}) // decide.Decide WOULD apply this
	prior := e.epoch(a.id())

	t.Run("marker unset: the legacy stub refuses and writes nothing", func(t *testing.T) {
		res, err := New(e.deps).Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.NoError(t, err)
		assert.Equal(t, decide.RowNone, res.Outcome.Row)
		assert.Equal(t, decide.VerdictRefused, res.Outcome.Verdict)
		assert.Equal(t, "pending", e.reqStatus(reqID))
		assert.Equal(t, prior, e.epoch(a.id()))
		assert.Empty(t, e.cmds.seq(a.id()))
		assert.Empty(t, e.perms.got())
	})
	t.Run("marker set: refused as activated", func(t *testing.T) {
		rnSetMarker(t, e)
		_, err := New(e.deps).Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.ErrorIs(t, err, ErrActivated)
	})
}

func TestRunner_SessionPassGuardsFamily(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	e.revokeFamilies(a)
	prior := e.epoch(a.id())

	pol := &rnPolicy{}
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))

	require.ErrorIs(t, err, loginfamily.ErrDenied)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 401, status)
	assert.Zero(t, pol.count())
	assert.Zero(t, rev.count())
	assert.Empty(t, e.cmds.seq(a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Empty(t, e.minter.got())
}

// revokeFamilies terminalizes every family of the account through the real authority.
func (e *rnEnv) revokeFamilies(a rnAccount) {
	t := e.t
	t.Helper()
	ctx := context.Background()
	tx, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	barrier, err := loginfamily.AcquireWriterBarrierTx(ctx, tx)
	require.NoError(t, err)
	set, err := loginfamily.LockUsersTx(ctx, tx, barrier, []uuid.UUID{a.id()}, loginfamily.UserNoKeyUpdate)
	require.NoError(t, err)
	user, err := set.ForUser(a.id())
	require.NoError(t, err)
	targets, err := loginfamily.CaptureUserTargetsTx(ctx, tx, user, nil)
	require.NoError(t, err)
	_, err = loginfamily.RevokeFamiliesTx(ctx, tx, targets, loginfamily.AuthCause{Scope: loginfamily.ScopeUser, Reason: loginfamily.ReasonExplicitRevoke})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.True(t, e.familyTerminal(a.fam))
}

func TestRunner_MissingUserIs401(t *testing.T) {
	e := newRnEnv(t)
	rev := &rnRev{proofs: []decide.Proof{{}}}
	req := Request{UserID: uuid.New(), Act: decide.SignOutOthers{}, Revalidate: rev.fn()}
	_, err := e.runner().Run(context.Background(), req)
	require.Error(t, err)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 401, status)
	assert.Zero(t, rev.count())
}

func TestRunner_LockTimeoutMapsTo503(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})

	ctx := context.Background()
	holder, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, a.id())
	require.NoError(t, err)

	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	_, err = e.runner().Run(cctx, e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.Error(t, err)
	status, code := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Equal(t, "lock_conflict", code)
	assert.Empty(t, e.cmds.seq(a.id()))
}

// The clock is read before the row-lock waits of loadInputTx (Task 8 carry M2): a runner that
// read it afterwards would hand decide a "now" later than the instant the lock was released.
// Both instants are database clocks, and the wait is observed, not slept.
func TestRunner_NowIsReadBeforeRowLockWaits(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	act := decide.Cancel{IDs: []string{reqID.String()}}
	proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7})

	ctx := context.Background()
	holder, err := e.db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = holder.Rollback() })
	_, err = holder.ExecContext(ctx, `SELECT 1 FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, a.id())
	require.NoError(t, err)

	pol := &rnPolicy{}
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.policy = pol.fn()
	done := make(chan error, 1)
	go func() {
		_, runErr := r.Run(ctx, e.req(a, act, rev, true))
		done <- runErr
	}()

	// Wait until a backend is blocked on the factor row's lock.
	require.Eventually(t, func() bool {
		return e.count(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database() AND pid <> pg_backend_pid()
			  AND wait_event_type = 'Lock' AND query ILIKE '%user_mfa_totp%'`) > 0
	}, 10*time.Second, 10*time.Millisecond, "the runner never blocked on the factor row")
	select {
	case runErr := <-done:
		t.Fatalf("the run finished while the factor row was locked: %v", runErr)
	default:
	}
	var release time.Time
	require.NoError(t, holder.QueryRowContext(ctx, `SELECT clock_timestamp()`).Scan(&release))
	require.NoError(t, holder.Commit())
	require.NoError(t, <-done)

	require.Equal(t, 1, pol.count())
	require.Equal(t, 1, rev.count())
	assert.True(t, pol.nows[0].Before(release), "Input.Now precedes the lock release")
	assert.True(t, rev.nows[0].Equal(pol.nows[0]), "Revalidate and Decide share one now")
}

// covers runs before any write: two holds on one request are refused whole, and neither lands.
func TestRunner_CoversRunsBeforeAnyWrite(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	proof := a.loginProof()

	pol := &rnPolicy{over: func(_ int, in decide.Input) (decide.Outcome, bool) {
		return decide.Outcome{
			Row: decide.RowT11, Verdict: decide.VerdictHeld, Source: decide.SourceUserSession,
			Effects: []decide.Effect{
				{Kind: decide.EffectHold, RequestID: reqID.String(), HeldMs: 5, HeldSince: in.Now},
				{Kind: decide.EffectHold, RequestID: reqID.String(), HeldMs: 7, HeldSince: in.Now},
			},
		}, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.Complete{ID: reqID.String()}, &rnRev{proofs: []decide.Proof{proof}}, false))

	require.ErrorIs(t, err, ErrContract)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 500, status)
	row := appReadReq(t, e.db, reqID)
	assert.Zero(t, row.HeldMs)
	assert.Nil(t, row.HeldSince, "no hold was written")
}

// finish runs before commit: an outcome that sets a password whose effects wrote none is
// refused, and the effects it did apply roll back.
func TestRunner_FinishGuardsPasswordWrites(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	proof := a.loginProof()
	prior := e.epoch(a.id())
	priorHash := e.passwordHash(a.id())

	pol := &rnPolicy{over: func(int, decide.Input) (decide.Outcome, bool) {
		return decide.Outcome{
			Row: decide.RowT11, Verdict: decide.VerdictApplied, Source: decide.SourceBreakGlass,
			RotateEpoch: true, SetsPassword: true, EnrolClass: decide.C2,
			Effects: []decide.Effect{{Kind: decide.EffectCancel, RequestID: reqID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1}},
		}, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.Complete{ID: reqID.String()}, &rnRev{proofs: []decide.Proof{proof}}, false))

	require.ErrorIs(t, err, ErrContract)
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Equal(t, priorHash, e.passwordHash(a.id()))
	assert.Empty(t, e.fenceState(a.id()))
	assert.Empty(t, e.fam.got())
}

// Folds apply first (ruling 16): in Outcome order the cancel ends the request and the fold
// that follows finds a terminal row.
func TestRunner_AppliesHoldFoldsBeforeTheCancel(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	r0 := snapOpenReq(decide.ReqCredentialRemove, a.totp)
	r0.originClass = decide.C1
	r0.heldSince = time.Now().Add(-time.Hour)
	snapInsertReq(t, e.db, a.id(), r0)
	proof := a.loginProof()

	pol := &rnPolicy{over: func(int, decide.Input) (decide.Outcome, bool) {
		return decide.Outcome{
			Row: decide.RowT7, Verdict: decide.VerdictApplied, Source: decide.SourceUserSession,
			Effects: []decide.Effect{
				{Kind: decide.EffectCancel, RequestID: r0.id.String(), Reason: decide.CancelProof, CancellerClass: decide.C1},
				{Kind: decide.EffectHold, RequestID: r0.id.String(), HeldMs: 5},
			},
		}, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.Cancel{IDs: []string{r0.id.String()}}, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.NoError(t, err)
	row := appReadReq(t, e.db, r0.id)
	assert.Equal(t, "cancelled", row.Status)
	assert.EqualValues(t, 5, row.HeldMs, "the fold ran while the row was still pending")
}

// Ruling 9: the R1 prefix order, pinned in source (Task 13 asserts the lock modes in PG).
func TestRunner_LockPrefixSourceOrder(t *testing.T) {
	src, err := os.ReadFile("runner.go")
	require.NoError(t, err)
	s := string(src)
	order := []string{"AcquireWriterBarrierTx(", "LockUsersTx(", "GuardAfterUserLockTx(", "LockSubjectTx("}
	last := -1
	for _, needle := range order {
		i := strings.Index(s, needle)
		require.GreaterOrEqual(t, i, 0, "%s is called", needle)
		require.Greater(t, i, last, "%s comes after the previous step", needle)
		last = i
	}
	// decideTx: the authority prefix, then the marker, then the address locks, then the snapshot.
	// The database-level order is pinned by integration_test.go's staged holders.
	calls := []string{"lockAuthorityTx(ctx, tx, req)", "refuseActivatedTx(ctx, tx)", "lockAddressesTx(ctx, tx, nil)", "loadInputTx(ctx, tx,"}
	last = -1
	for _, needle := range calls {
		i := strings.Index(s, needle)
		require.GreaterOrEqual(t, i, 0, "%s is called", needle)
		require.Greater(t, i, last, "%s comes after the previous step", needle)
		last = i
	}
	assert.Contains(t, s, "UserNoKeyUpdate", "the users lock is FOR NO KEY UPDATE")
	assert.Contains(t, s, "sql.LevelReadCommitted", "the plain strategy opens at READ COMMITTED")
	assert.Contains(t, s, "lock_timeout", "SET LOCAL lock_timeout leads the transaction")
}

// ---------------------------------------------------------------------------
// HTTPStatus
// ---------------------------------------------------------------------------

func TestHTTPStatus_MapsEverySentinel(t *testing.T) {
	denied := loginfamily.NewError(loginfamily.ReasonAccessIdentity, nil)
	unavailable := loginfamily.NewError(loginfamily.ReasonSQLUnavailable, nil)
	contract := loginfamily.NewError(loginfamily.ReasonCapabilityInvalid, nil)
	type row struct {
		name   string
		err    error
		status int
		code   string // "" = only the status is pinned
	}
	base := []row{
		{"activated", fmt.Errorf("x: %w", ErrActivated), 503, "guard_unavailable"},
		{"lock conflict sentinel", fmt.Errorf("x: %w", ErrLockConflict), 503, "lock_conflict"},
		{"deadlock 40P01", fmt.Errorf("x: %w", &pq.Error{Code: "40P01"}), 503, "lock_conflict"},
		{"lock not available 55P03", fmt.Errorf("x: %w", &pq.Error{Code: "55P03"}), 503, "lock_conflict"},
		{"loginfamily unavailable", unavailable, 503, "service_unavailable"},
		{"55P03 inside loginfamily unavailable", loginfamily.NewError(loginfamily.ReasonSQLUnavailable, &pq.Error{Code: "55P03"}), 503, "lock_conflict"},
		{"effect unwired", fmt.Errorf("x: %w", ErrEffectUnwired), 500, "guard_effect_unwired"},
		{"loginfamily denied", denied, 401, "authentication_required"},
		{"loginfamily contract", contract, 500, "guard_contract"},
		{"accountguard contract", fmt.Errorf("x: %w", ErrContract), 500, "guard_contract"},
		{"stale", fmt.Errorf("x: %w", ErrStale), 500, "guard_stale"},
		{"source refused", fmt.Errorf("x: %w", ErrSourceRefused), 500, "guard_source_refused"},
		{"key unavailable", fmt.Errorf("x: %w", ErrKeyUnavailable), 503, "key_unavailable"},
		{"token invalid", fmt.Errorf("x: %w", ErrTokenInvalid), 401, "guard_token_invalid"},
		{"challenge invalid", fmt.Errorf("x: %w", ErrChallengeInvalid), 401, "challenge_invalid"},
		{"presence readiness", fmt.Errorf("x: %w", &presencehistory.ServiceError{Status: 503, Code: "activity_history_delivery_unavailable"}), 503, "presence_unavailable"},
		{"unrecognized", errors.New("anything else"), 500, "internal_error"},
		{"nil maps to a 500", nil, 500, "internal_error"},
		{"503 class wins over 401", errors.Join(denied, unavailable), 503, "service_unavailable"},
		{"503 class wins over 500", errors.Join(ErrContract, ErrActivated), 503, "guard_unavailable"},
		{"401 wins over 500", errors.Join(ErrContract, denied), 401, "authentication_required"},
	}

	// One outage, one status (final review M2): a connection-class driver failure is the same
	// transient outage loginfamily answers 503 for at the barrier, so it must not read as a
	// contract break. Each form is mapped bare-wrapped and inside withheld, which is how the
	// runner carries a driver error.
	connForms := []struct {
		name string
		err  error
	}{
		{"driver.ErrBadConn", driver.ErrBadConn},
		{"sql.ErrConnDone", sql.ErrConnDone},
		{"a net.Error", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}},
		{"pq 08000 connection exception", &pq.Error{Code: "08000"}},
		{"pq 08003 connection does not exist", &pq.Error{Code: "08003"}},
		{"pq 08006 connection failure", &pq.Error{Code: "08006"}},
		{"pq 57P01 admin shutdown", &pq.Error{Code: "57P01"}},
		{"pq 57P02 crash shutdown", &pq.Error{Code: "57P02"}},
		{"pq 57P03 cannot connect now", &pq.Error{Code: "57P03"}},
	}
	connRows := make([]row, 0, 3*len(connForms))
	for _, f := range connForms {
		connRows = append(connRows,
			row{"connection class, wrapped: " + f.name, fmt.Errorf("x: %w", f.err), 503, "service_unavailable"},
			row{"connection class, withheld: " + f.name, withhold(f.err), 503, "service_unavailable"},
			row{"connection class, wrapped withheld: " + f.name, fmt.Errorf("x: %w", withhold(f.err)), 503, "service_unavailable"})
	}
	extraRows := []row{
		// An applier's dbFault keeps only the SQLSTATE (redactDBError), and the connection class
		// reads it from there as it would from the driver's own error.
		{"applier dbFault 57P01 admin shutdown", redactDBError(&pq.Error{Code: "57P01"}), 503, "service_unavailable"},
		{"applier dbFault 57P01, wrapped and withheld", fmt.Errorf("x: %w", withhold(redactDBError(&pq.Error{Code: "57P01"}))), 503, "service_unavailable"},
		{"applier dbFault 23505 stays 500", redactDBError(&pq.Error{Code: "23505", Constraint: "some_unique"}), 500, "internal_error"},
		{"applier dbFault 23505, wrapped and withheld, stays 500", fmt.Errorf("x: %w", withhold(redactDBError(&pq.Error{Code: "23505"}))), 500, "internal_error"},
		// Go 1.27's net timeout error reports context.DeadlineExceeded: a context error is the
		// caller's own deadline, not an outage, even under net.Error.
		{"net timeout reporting a context deadline stays 500",
			&net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}, 500, "internal_error"},
		{"net timeout reporting a context deadline, withheld, stays 500",
			withhold(&net.OpError{Op: "read", Net: "tcp", Err: context.DeadlineExceeded}), 500, "internal_error"},
		// Not a connection failure: the driver ran the statement and refused it.
		{"pq class 23 constraint violation stays 500", fmt.Errorf("x: %w", &pq.Error{Code: "23505"}), 500, "internal_error"},
		{"pq class 23 withheld stays 500", withhold(&pq.Error{Code: "23503"}), 500, "internal_error"},
		{"pq syntax error stays 500", fmt.Errorf("x: %w", &pq.Error{Code: "42601"}), 500, "internal_error"},
		// Precedence: a contract-break sentinel wins over a connection cause beneath it, and the
		// 503 classes still lead the 401s.
		{"stale over a connection error stays 500", fmt.Errorf("x: %w: %w", ErrStale, driver.ErrBadConn), 500, "guard_stale"},
		{"stale withheld over a connection error stays 500", fmt.Errorf("%w: %w", ErrStale, withhold(&pq.Error{Code: "08006"})), 500, "guard_stale"},
		{"contract over a connection error stays 500", fmt.Errorf("%w: %w", ErrContract, sql.ErrConnDone), 500, "guard_contract"},
		{"effect unwired over a connection error stays 500", fmt.Errorf("%w: %w", ErrEffectUnwired, driver.ErrBadConn), 500, "guard_effect_unwired"},
		{"source refused over a connection error stays 500", fmt.Errorf("%w: %w", ErrSourceRefused, driver.ErrBadConn), 500, "guard_source_refused"},
		{"a connection error wins over 401", errors.Join(denied, driver.ErrBadConn), 503, "service_unavailable"},
		{"lock conflict still leads a connection error", errors.Join(ErrLockConflict, driver.ErrBadConn), 503, "lock_conflict"},
		// A context error is the caller's own deadline or cancel, not an outage (a context error
		// satisfies net.Error's method set, so the connection class must not claim it).
		{"context deadline stays 500", fmt.Errorf("x: %w", context.DeadlineExceeded), 500, "internal_error"},
		{"context deadline withheld stays 500", withhold(context.DeadlineExceeded), 500, "internal_error"},
		{"context canceled stays 500", fmt.Errorf("x: %w", context.Canceled), 500, "internal_error"},
		{"context canceled withheld stays 500", withhold(context.Canceled), 500, "internal_error"},
		// One outage, one status: a connection cause anywhere in the chain is the outage, even
		// beneath a stepup.Error whose own status is 500.
		{"subject refusal 500 over a connection cause is one outage",
			withhold(subjectError(&stepup.Error{Status: 500, Cause: driver.ErrBadConn})), 503, "service_unavailable"},
		{"subject refusal 500 over a pq connection cause, wrapped",
			fmt.Errorf("lock subject: %w", withhold(subjectError(&stepup.Error{Status: 500, Cause: &pq.Error{Code: "57P01"}}))), 503, "service_unavailable"},
		// subjectError's non-401 branch: only a 503 refusal is retryable.
		{"subject refusal 401 stays a denial", withhold(subjectError(&stepup.Error{Status: 401})), 401, "authentication_required"},
		{"subject refusal 503 is service_unavailable", withhold(subjectError(&stepup.Error{Status: 503})), 503, "service_unavailable"},
		{"subject refusal 503, wrapped", fmt.Errorf("lock subject: %w", withhold(subjectError(&stepup.Error{Status: 503}))), 503, "service_unavailable"},
		{"subject refusal 500 stays 500", withhold(subjectError(&stepup.Error{Status: 500})), 500, "internal_error"},
		{"subject refusal 500, wrapped", fmt.Errorf("lock subject: %w", withhold(subjectError(&stepup.Error{Status: 500}))), 500, "internal_error"},
	}
	cases := slices.Concat(base, connRows, extraRows)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, code := HTTPStatus(c.err)
			assert.Equal(t, c.status, status)
			assert.Equal(t, c.code, code)
		})
	}
}

func TestHTTPStatus_BodyNeverCarriesTheCause(t *testing.T) {
	driverDetail := "user-9f1c-and-driver-detail"
	for _, c := range []struct {
		err    error
		status int
	}{
		{fmt.Errorf("%s: %w", driverDetail, ErrStale), 500},
		{fmt.Errorf("%s: %w", driverDetail, &pq.Error{Code: "40P01", Message: driverDetail}), 503},
		{errors.New(driverDetail), 500},
	} {
		status, code := HTTPStatus(c.err)
		assert.Equal(t, c.status, status)
		assert.NotEmpty(t, code)
		assert.NotContains(t, code, driverDetail)
	}
}

// ---------------------------------------------------------------------------
// Fix round: rulings 18, 19, 20
// ---------------------------------------------------------------------------

// Ruling 18: a forced-path readiness refusal (here: the presence service has no delivery
// bound) is "try again", not a server fault. Nothing is written and the op is rolled back.
func TestRunner_ForcedReadinessFailureIs503AndRollsBack(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())
	priorHash := e.passwordHash(a.id())

	d := e.deps
	d.Presence = presencehistory.NewService(e.db, presencehistory.DisclosureState{}, false) // no delivery bound
	rev := &rnRev{proofs: []decide.Proof{{}}}
	_, err := newTestRunner(d, e.reg).Run(context.Background(), e.req(a, act, rev, false))

	require.Error(t, err)
	status, code := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Equal(t, "presence_unavailable", code)
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "Begin, then Rollback")
	assert.Empty(t, e.fenceState(a.id()), "no blocked: marker left")
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Equal(t, priorHash, e.passwordHash(a.id()))
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.True(t, e.webauthnExists(a))
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.minter.got())
}

// Ruling 19: the activation marker is read FOR SHARE, so an activation UPDATE queues behind
// any in-flight run (and a run behind it reads the new value).
func TestRunner_ActivationMarkerIsReadForShare(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	act := decide.Cancel{IDs: []string{reqID.String()}}
	proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7})

	activate := func(lockTimeout string) error {
		ctx := context.Background()
		tx, err := e.db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }() // undoes a successful update
		_, err = tx.ExecContext(ctx, `SELECT set_config('lock_timeout', $1, true)`, lockTimeout)
		require.NoError(t, err)
		_, err = tx.ExecContext(ctx, `UPDATE accountguard_state SET activated_at = now()`)
		return err
	}

	var duringRun error
	r := e.runner()
	r.beforeCommit = func() { duringRun = activate("200ms") }
	_, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.NoError(t, err)

	var pqErr *pq.Error
	require.ErrorAs(t, duringRun, &pqErr, "the activation must not get through while a run is in flight")
	assert.Equal(t, "55P03", string(pqErr.Code))
	assert.Equal(t, "cancelled", e.reqStatus(reqID))
	assert.NoError(t, activate("1s"), "once the run is over the activation proceeds")
}

// Ruling 20: a T1b whose contest write goes stale on pass 2 rolls back whole: no permgen
// bump, no marker left, nothing written. Kills a bump in plain pass 2's failure branch.
func TestRunner_PlainPass2StaleWriteBumpsNoPermgenAndLeavesNoMarker(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(rnImmature())
	proof := a.loginProof()
	e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})
	prior := e.epoch(a.id())

	// Pass 2 (policy call 2) names a version the row does not hold.
	pol := &rnPolicy{over: func(n int, in decide.Input) (decide.Outcome, bool) {
		if n != 2 {
			return decide.Outcome{}, false
		}
		o := decide.Decide(in)
		for i := range o.Effects {
			if o.Effects[i].Kind == decide.EffectContest {
				o.Effects[i].Cred.Version++
			}
		}
		return o, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	res, err := r.Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.ErrorIs(t, err, ErrStale)
	assert.Equal(t, Result{}, res, "an error carries no half-meaningful outcome")
	assert.Equal(t, 2, pol.count())
	assert.Empty(t, e.perms.got(), "no permgen bump on a definite rollback")
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()))
	assert.Empty(t, e.fenceState(a.id()), "no blocked: marker left")
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.webauthnContested(a))
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.minter.got())
}

// ---------------------------------------------------------------------------
// Review fix round: rulings 22-29
// ---------------------------------------------------------------------------

// rnAssertUnknownCommit pins the answer to an outcome that may be durable: 503
// service_unavailable AND the closed reason commit_unknown, so a caller that asks "did it land"
// is told it cannot know. The status alone would pass a proven rollback too.
func rnAssertUnknownCommit(t *testing.T, err error) {
	t.Helper()
	status, code := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Equal(t, "service_unavailable", code)
	var lfErr *loginfamily.Error
	if assert.ErrorAs(t, err, &lfErr, "the runner classifies an unknown commit as a loginfamily error") {
		assert.Equal(t, loginfamily.ReasonCommitUnknown, lfErr.Reason())
	}
}

// rnAssertProvenRollback pins the answer to a failure the runner proved rolled back: the same
// 503 as an unknown commit, but the closed reason sql_unavailable, so nothing can read it as a
// possibly durable write.
func rnAssertProvenRollback(t *testing.T, err error) {
	t.Helper()
	status, code := HTTPStatus(err)
	assert.Equal(t, 503, status)
	assert.Equal(t, "service_unavailable", code)
	var lfErr *loginfamily.Error
	if assert.ErrorAs(t, err, &lfErr, "the runner classifies a proven rollback as a loginfamily error") {
		assert.Equal(t, loginfamily.ReasonSQLUnavailable, lfErr.Reason())
	}
}

// Ruling 22: no password write outside a rotation and the forced clear. decide never emits
// these shapes, so a fake policy does. Each must break pass 2's assertion on both attempts
// (command sequence: Begin, Rollback, Begin, Rollback) and end in ErrContract with nothing
// written. The command sequence is what separates the retry from a late finish() failure.
func rnPasswordGuard(t *testing.T, pass1, pass2 decide.Outcome) {
	t.Helper()
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	prior, priorHash := e.epoch(a.id()), e.passwordHash(a.id())

	pol := &rnPolicy{over: func(n int, _ decide.Input) (decide.Outcome, bool) {
		if n%2 == 1 {
			return pass1, true
		}
		return pass2, true
	}}
	rev := &rnRev{proofs: []decide.Proof{{}}}
	res, err := newTestRunnerWithPolicy(e.deps, e.reg, pol.fn()).Run(
		context.Background(), e.req(a, decide.SignOutOthers{}, rev, false))

	require.ErrorIs(t, err, ErrContract)
	status, code := HTTPStatus(err)
	assert.Equal(t, 500, status)
	assert.Equal(t, "guard_contract", code)
	assert.Equal(t, Result{}, res)
	assert.Equal(t, 4, pol.count(), "pass 1, the violating pass 2, then one retry of both")
	assert.Equal(t, []string{"begin", "script", "begin", "script"}, e.cmds.seq(a.id()), "each attempt's op is settled by Rollback")
	assert.Empty(t, e.fenceState(a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Equal(t, priorHash, e.passwordHash(a.id()), "no password written")
	assert.Equal(t, 1, e.customTier(a.id()), "no forced clear committed")
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.minter.got())
}

func rnSets(rotate bool) decide.Outcome {
	return decide.Outcome{
		Row: decide.RowT11, Verdict: decide.VerdictApplied, Source: decide.SourceBreakGlass,
		RotateEpoch: rotate, SetsPassword: true, EnrolClass: decide.C2,
	}
}

func rnRotateOnly() decide.Outcome {
	return decide.Outcome{Row: decide.RowT1, Verdict: decide.VerdictApplied, Source: decide.SourceUserSession, RotateEpoch: true}
}

func TestRunner_PasswordGuards(t *testing.T) {
	t.Run("a password without a rotation never commits in pass 1", func(t *testing.T) {
		// Mutant: pass 1's `o.RotateEpoch || o.SetsPassword` reduced to `o.RotateEpoch`.
		rnPasswordGuard(t, rnSets(false), rnSets(false))
	})
	t.Run("pass 1 chose the plain strategy and pass 2 sets a password", func(t *testing.T) {
		// Mutant: the `o.SetsPassword != forced` clause deleted. // pragma: allowlist secret
		rnPasswordGuard(t, rnRotateOnly(), rnSets(true))
	})
	t.Run("pass 1 chose the forced clear and pass 2 sets no password", func(t *testing.T) {
		// Mutant: the same clause deleted; the rotation would then commit with a forced clear
		// and no password.
		rnPasswordGuard(t, rnSets(true), rnRotateOnly())
	})
}

// Ruling 24: covers refuses the whole outcome before any applier runs.
func TestRunner_CoversRefusalReachesNoApplier(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	proof := a.loginProof()

	pol := &rnPolicy{over: func(int, decide.Input) (decide.Outcome, bool) {
		return decide.Outcome{
			Row: decide.RowT7, Verdict: decide.VerdictApplied, Source: decide.SourceUserSession,
			Effects: []decide.Effect{
				{Kind: decide.EffectCancel, RequestID: reqID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1},
				{Kind: decide.EffectRestorePrimary, Cred: a.user.addrRef}, // unwired (C5)
			},
		}, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	_, err := r.Run(context.Background(), e.req(a, decide.Cancel{IDs: []string{reqID.String()}}, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.ErrorIs(t, err, ErrEffectUnwired)
	assert.Zero(t, e.appliedCalls(), "no applier ran: covers refused before the first write")
	assert.Equal(t, "pending", e.reqStatus(reqID))
}

// Ruling 26: the forced path's error also returns the zero Result.
func TestRunner_ForcedPass2ErrorReturnsZeroResult(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())

	// Pass 2 (policy call 2) names a removal at a version the row does not hold.
	pol := &rnPolicy{over: func(n int, in decide.Input) (decide.Outcome, bool) {
		if n != 2 {
			return decide.Outcome{}, false
		}
		o := decide.Decide(in)
		for i := range o.Effects {
			if o.Effects[i].Kind == decide.EffectRemove {
				o.Effects[i].Cred.Version++
			}
		}
		return o, true
	}}
	r := e.runner()
	r.policy = pol.fn()
	res, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))

	require.ErrorIs(t, err, ErrStale)
	assert.Equal(t, Result{}, res)
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.Empty(t, e.fenceState(a.id()))
	assert.Empty(t, e.perms.got())
}

// Ruling 27: a driver error's text never reaches the error Run returns.
func TestRunner_DriverErrorTextIsRedacted(t *testing.T) {
	leak := "leak-me-9f1c duplicate key value (secret@example.test)"
	driverErr := func() error {
		return &pq.Error{Code: "23505", Message: leak, Detail: leak, Constraint: "some_constraint"}
	}
	t.Run("pass 1 notice sink", func(t *testing.T) {
		e := newRnEnv(t)
		a := e.account(snapLegacyMaturity())
		reqID := e.pendingRemove(a)
		act := decide.Cancel{IDs: []string{reqID.String()}}
		proof := decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
		e.prove(a, act, proof, rnWant{row: decide.RowT7})
		e.sink.err = driverErr()
		_, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "leak-me")
		assert.NotContains(t, err.Error(), "secret@example.test")
		assert.Equal(t, "pending", e.reqStatus(reqID))
	})
	t.Run("pass 2 notice sink", func(t *testing.T) {
		e := newRnEnv(t)
		a := e.account(rnImmature())
		proof := a.loginProof()
		e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})
		e.sink.err = driverErr()
		_, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "leak-me")
		assert.NotContains(t, err.Error(), "secret@example.test")
		assert.Empty(t, e.fenceState(a.id()))
	})
	t.Run("revalidate", func(t *testing.T) {
		e := newRnEnv(t)
		a := e.account(snapLegacyMaturity())
		req := e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{{}}}, true)
		req.Revalidate = func(context.Context, *sql.Tx, time.Time) (decide.Proof, error) {
			return decide.Proof{}, driverErr()
		}
		_, err := e.runner().Run(context.Background(), req)
		require.Error(t, err)
		assert.NotContains(t, err.Error(), "leak-me")
		assert.NotContains(t, err.Error(), "secret@example.test")
	})
}

// Ruling 28: a refused Complete is counted: one WARN, class complete_refused, user_id only.
func TestRunner_CompleteRefusalIsLogged(t *testing.T) {
	logged := func(e *rnEnv, buf *strings.Builder) []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			require.NoError(e.t, json.Unmarshal([]byte(line), &m))
			if m["failure_class"] == "complete_refused" {
				out = append(out, m)
			}
		}
		return out
	}
	run := func(t *testing.T, act decide.Action) (*rnEnv, rnAccount, *strings.Builder, Result, error) {
		e := newRnEnv(t)
		a := e.account(snapLegacyMaturity())
		var buf strings.Builder
		e.deps.Log = slog.New(slog.NewJSONHandler(&buf, nil))
		res, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
		return e, a, &buf, res, err
	}
	t.Run("a Complete naming no request is counted once", func(t *testing.T) {
		e, a, buf, res, err := run(t, decide.Complete{ID: uuid.NewString()})
		require.NoError(t, err)
		require.Equal(t, decide.RowNone, res.Outcome.Row)
		lines := logged(e, buf)
		require.Len(t, lines, 1)
		assert.Equal(t, "WARN", lines[0]["level"])
		assert.Equal(t, a.id().String(), lines[0]["user_id"])
		keys := make([]string, 0, len(lines[0]))
		for k := range lines[0] {
			keys = append(keys, k)
		}
		assert.ElementsMatch(t, []string{"time", "level", "msg", "failure_class", "user_id"}, keys, "no other attribute")
	})
	t.Run("another refused action is not counted", func(t *testing.T) {
		e, _, buf, res, err := run(t, decide.Cancel{IDs: []string{uuid.NewString()}})
		require.NoError(t, err)
		require.Equal(t, decide.RowNone, res.Outcome.Row)
		assert.Empty(t, logged(e, buf))
	})
}

// Ruling 32: pass 2 checks covers before any write too. Pass 1 accepts a rotating outcome;
// pass 2's outcome holds a wired effect and an unwired one, so it is refused whole before
// sourceTx, the rotation or the first applier.
func TestRunner_Pass2CoversRefusalReachesNoApplier(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	reqID := e.pendingRemove(a)
	prior := e.epoch(a.id())

	pass2 := rnRotateOnly()
	pass2.Effects = []decide.Effect{
		{Kind: decide.EffectCancel, RequestID: reqID.String(), Reason: decide.CancelProof, CancellerClass: decide.C1},
		{Kind: decide.EffectRestorePrimary, Cred: a.user.addrRef}, // unwired (C5)
	}
	pol := &rnPolicy{over: func(n int, _ decide.Input) (decide.Outcome, bool) {
		if n == 1 {
			return rnRotateOnly(), true // pass 1: covers accepts it
		}
		return pass2, true
	}}
	rev := &rnRev{proofs: []decide.Proof{{}}}
	res, err := newTestRunnerWithPolicy(e.deps, e.reg, pol.fn()).Run(
		context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))

	require.ErrorIs(t, err, ErrEffectUnwired)
	status, code := HTTPStatus(err)
	assert.Equal(t, 500, status)
	assert.Equal(t, "guard_effect_unwired", code)
	assert.Equal(t, Result{}, res)
	assert.Equal(t, 2, pol.count(), "pass 1, then pass 2: not a retry")
	assert.Zero(t, e.appliedCalls(), "no applier ran: pass 2's covers refused before the first write")
	assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "Begin, then Rollback")
	assert.Empty(t, e.fenceState(a.id()))
	assert.Equal(t, "pending", e.reqStatus(reqID))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.perms.got())
	assert.Empty(t, e.minter.got())
}

// ---------------------------------------------------------------------------
// Red-team fix round (#3680 PR3): RT-8, the striped sender gate
// ---------------------------------------------------------------------------

// rnSenderStripe restates presencehistory.senderGateIndex (operations.go): the sender gate is one
// of 64 stripes chosen by a hash of the sender id. rnStripeMate proves the restatement against
// the real gate, so a drift in the hash fails the fixture and never lets a test pass vacuously.
func rnSenderStripe(id uuid.UUID) uint32 {
	var h uint32
	for _, v := range id {
		h = h*33 + uint32(v)
	}
	return h % 64
}

// rnStripeMate returns a user id, not victim, whose sender gate is the victim's stripe: holding
// the mate's gate blocks anyone who waits for the victim's.
func rnStripeMate(t *testing.T, e *rnEnv, victim uuid.UUID) uuid.UUID {
	t.Helper()
	var mate uuid.UUID
	for mate == uuid.Nil {
		if c := uuid.New(); c != victim && rnSenderStripe(c) == rnSenderStripe(victim) {
			mate = c
		}
	}
	probeCtx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var waitErr error
	require.NoError(t, e.presence.WithSender(context.Background(), mate, func() error {
		waitErr = e.presence.WithSender(probeCtx, victim, func() error { return nil })
		return nil
	}))
	require.ErrorIs(t, waitErr, context.DeadlineExceeded, "fixture: the mate's gate really is the victim's stripe")
	return mate
}

// rnHoldGateAndRun runs the forced break-glass completion while the caller holds the sender gate of
// held, under a caller context of callerBound, and returns Run's error and how long it took.
func rnHoldGateAndRun(t *testing.T, e *rnEnv, a rnAccount, act decide.Action, held uuid.UUID, callerBound time.Duration) (time.Duration, error) {
	t.Helper()
	var (
		runErr error
		took   time.Duration
	)
	require.NoError(t, e.presence.WithSender(context.Background(), held, func() error {
		ctx, cancel := context.WithTimeout(context.Background(), callerBound)
		defer cancel()
		start := time.Now()
		_, runErr = e.runner().Run(ctx, e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
		took = time.Since(start)
		return nil
	}))
	return took, runErr
}

// RT-8 (CWE-833): a caller holding ANY sender gate on req.UserID's stripe used to hang pass 2,
// with blocked: published, until its context expired. The gate ACQUIRE is now bounded by
// decide.LockTimeout; a wait that times out is a definite rollback and a 503 lock_conflict.
func TestRunner_ForcedPass2SenderGateHeldAcrossTheStripeIsLockConflict(t *testing.T) {
	for _, tc := range []struct {
		name string
		held func(t *testing.T, e *rnEnv, victim uuid.UUID) uuid.UUID
	}{
		{"another user's gate on req.UserID's stripe", rnStripeMate},
		{"req.UserID's own gate", func(_ *testing.T, _ *rnEnv, victim uuid.UUID) uuid.UUID { return victim }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRnEnv(t)
			a, reqID, act := e.forcedFixture()
			prior := e.epoch(a.id())
			priorHash := e.passwordHash(a.id())
			held := tc.held(t, e, a.id())

			took, err := rnHoldGateAndRun(t, e, a, act, held, 2*decide.LockTimeout)

			require.ErrorIs(t, err, ErrLockConflict, "a held gate is a lock conflict, not the caller's deadline")
			assert.NotErrorIs(t, err, context.DeadlineExceeded)
			status, code := HTTPStatus(err)
			assert.Equal(t, 503, status)
			assert.Equal(t, "lock_conflict", code)
			assert.Less(t, took, decide.LockTimeout+time.Second, "bounded by LockTimeout, not the caller's context")
			assert.Empty(t, e.fenceState(a.id()), "the fence op is rolled back: blocked: cleared")
			assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "Begin, then Rollback")
			assert.Equal(t, prior, e.epoch(a.id()), "no rotation committed")
			assert.Equal(t, priorHash, e.passwordHash(a.id()), "no password written")
			assert.Equal(t, "pending", e.reqStatus(reqID), "the request is untouched")
			assert.True(t, e.webauthnExists(a), "no factor removed")
			assert.Empty(t, e.fam.got(), "a proven rollback closes nothing")
			assert.Empty(t, e.perms.got())
			assert.Empty(t, e.minter.got())
			assert.Empty(t, e.evict.got())
		})
	}
}

// RT-8, legitimate path: the bound is on waiting for the gate. A gate that frees inside
// LockTimeout lets pass 2 proceed and commit.
func TestRunner_ForcedPass2WaitsOutAGateThatFreesInsideTheBound(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())
	mate := rnStripeMate(t, e, a.id())

	runCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var (
		res    Result
		runErr error
	)
	done := make(chan struct{})
	require.NoError(t, e.presence.WithSender(context.Background(), mate, func() error {
		go func() {
			defer close(done)
			res, runErr = e.runner().Run(runCtx, e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))
		}()
		// Pass 2 publishes blocked: before it waits for the gate; hold it a little longer.
		deadline := time.Now().Add(regWaitBound)
		for !strings.HasPrefix(e.fenceState(a.id()), "blocked:") && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
		}
		require.True(t, strings.HasPrefix(e.fenceState(a.id()), "blocked:"), "fixture: pass 2 reached the gate")
		time.Sleep(500 * time.Millisecond)
		select {
		case <-done:
			t.Fatal("fixture: Run finished while the gate was still held")
		default:
		}
		return nil
	}))
	<-done

	require.NoError(t, runErr)
	assert.Equal(t, decide.RowT11, res.Outcome.Row)
	assert.NotEqual(t, prior, e.epoch(a.id()), "the rotation committed")
	assert.Equal(t, "completed", e.reqStatus(reqID))
	assert.Equal(t, appPendingHash, e.passwordHash(a.id()))
}

// RT-8, legitimate path: the bound covers the acquire and nothing else. A pass-2 work step that
// takes longer than LockTimeout still commits under the caller's own context.
func TestRunner_ForcedPass2WorkLongerThanTheLockTimeoutStillCommits(t *testing.T) {
	e := newRnEnv(t)
	a, reqID, act := e.forcedFixture()
	prior := e.epoch(a.id())

	var slept int
	r := e.runner()
	r.beforeCommit = func() {
		slept++
		time.Sleep(decide.LockTimeout + 500*time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := r.Run(ctx, e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))

	require.NoError(t, err, "the work runs on the caller's context, not on the gate's bound")
	assert.Equal(t, 1, slept, "fixture: the slow step ran once, before the commit")
	assert.Equal(t, decide.RowT11, res.Outcome.Row)
	assert.NotEqual(t, prior, e.epoch(a.id()))
	assert.Equal(t, "active:"+e.epoch(a.id()), e.fenceState(a.id()), "Commit")
	assert.Equal(t, "completed", e.reqStatus(reqID))
	assert.Equal(t, appPendingHash, e.passwordHash(a.id()))
}
