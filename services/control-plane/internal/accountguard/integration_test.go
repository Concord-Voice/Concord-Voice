package accountguard

// Task 13 (#3680 PR3): the runner against real PostgreSQL and Redis (AC-int). These tests reuse
// the rnEnv fixture family of runner_test.go and add what only a live database can show: the
// lock order and modes the runner takes, a cancel that commits between the snapshot and the
// request lock, the atomicity of a completion, the commit strategy a SetsPassword outcome
// takes, the durable rotation evidence, and the continuation's dependence on a definite commit.
// Every fixture is proven through loadInputTx + decide.Decide (rnEnv.prove) before a runner
// test trusts it.
//
// Row locks do not survive in pg_locks (a tuple lock is released into the row header), so a
// lock's MODE is read by probing it from a second connection with NOWAIT: KEY SHARE conflicts
// only with UPDATE, SHARE with UPDATE and NO KEY UPDATE, NO KEY UPDATE with those two and SHARE,
// and UPDATE with everything. The four probes therefore tell the four modes apart. pgrowlocks
// is not used: it needs a contrib extension the scratch database need not carry.

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/loginfamily"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

const itWait = 30 * time.Second

var errItInjected = errors.New("injected applier failure")

// ---------------------------------------------------------------------------
// Row-lock probes
// ---------------------------------------------------------------------------

// itConflicts reports whether a FOR <mode> NOWAIT probe of base is refused by a held lock.
// base selects one row (SELECT 1 FROM ...); a probe that finds no row is a broken fixture.
func itConflicts(t *testing.T, db *sql.DB, base, mode string, args ...any) bool {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	var one int
	err = tx.QueryRowContext(ctx, base+" FOR "+mode+" NOWAIT", args...).Scan(&one)
	if err == nil {
		return false
	}
	var pqErr *pq.Error
	require.ErrorAs(t, err, &pqErr, "a probe failed for a reason other than a lock conflict")
	require.Equal(t, "55P03", string(pqErr.Code))
	return true
}

// itHeldMode is the strongest row-lock mode another transaction holds on the row base selects:
// "none", "KEY SHARE", "SHARE", "NO KEY UPDATE" or "UPDATE".
func itHeldMode(t *testing.T, db *sql.DB, base string, args ...any) string {
	t.Helper()
	switch {
	case itConflicts(t, db, base, "KEY SHARE", args...):
		return "UPDATE"
	case itConflicts(t, db, base, "NO KEY UPDATE", args...):
		if itConflicts(t, db, base, "SHARE", args...) {
			return "NO KEY UPDATE"
		}
		return "SHARE"
	case itConflicts(t, db, base, "UPDATE", args...):
		return "KEY SHARE"
	}
	return "none"
}

// itRes is one lockable row of the runner's order.
type itRes struct {
	name, base string
	args       []any
}

func itResources(a rnAccount, reqs ...uuid.UUID) []itRes {
	rs := []itRes{
		{"rollout", `SELECT 1 FROM login_family_rollout WHERE id = TRUE`, nil},
		{"users", `SELECT 1 FROM users WHERE id = $1`, []any{a.id()}},
		{"family", `SELECT 1 FROM login_families WHERE family_id = $1`, []any{a.fam}},
		{"state", `SELECT 1 FROM accountguard_state WHERE singleton`, nil},
	}
	for i, id := range reqs {
		rs = append(rs, itRes{"req" + string(rune('1'+i)), `SELECT 1 FROM account_pending_requests WHERE id = $1`, []any{id}})
	}
	return append(rs,
		itRes{"totp", `SELECT 1 FROM user_mfa_totp WHERE user_id = $1`, []any{a.id()}},
		itRes{"webauthn", `SELECT 1 FROM user_mfa_webauthn WHERE user_id = $1`, []any{a.id()}},
		itRes{"sso", `SELECT 1 FROM user_sso_identities WHERE user_id = $1`, []any{a.id()}},
		itRes{"recovery", `SELECT 1 FROM user_recovery_keys WHERE user_id = $1`, []any{a.id()}},
	)
}

// itModes reads the held mode of each named resource.
func itModes(t *testing.T, db *sql.DB, rs []itRes, names ...string) map[string]string {
	t.Helper()
	out := make(map[string]string, len(names))
	for _, n := range names {
		found := false
		for _, r := range rs {
			if r.name == n {
				out[n] = itHeldMode(t, db, r.base, r.args...)
				found = true
			}
		}
		require.True(t, found, "unknown resource %s", n)
	}
	return out
}

// ---------------------------------------------------------------------------
// Holder, pause and run plumbing
// ---------------------------------------------------------------------------

// itHolder is a second connection holding a lock the runner will queue behind.
type itHolder struct {
	tx  *sql.Tx
	pid int
}

func itHold(t *testing.T, db *sql.DB, lockSQL string, args ...any) *itHolder {
	t.Helper()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	h := &itHolder{tx: tx}
	// Bounded: a lock a failed earlier test left behind fails this one in seconds, not at the
	// go test timeout. The transaction itself outlives the statement's context.
	ctx, cancel := context.WithTimeout(context.Background(), itWait)
	defer cancel()
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&h.pid))
	_, err = tx.ExecContext(ctx, lockSQL, args...)
	require.NoError(t, err)
	return h
}

// waitBlocked waits until some backend is blocked behind this holder, which proves the runner
// has taken every lock that precedes the one held here.
func (h *itHolder) waitBlocked(t *testing.T, db *sql.DB) {
	t.Helper()
	require.Eventually(t, func() bool {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE wait_event_type = 'Lock' AND $1 = ANY(pg_blocking_pids(pid))`, h.pid).Scan(&n); err != nil {
			return false
		}
		return n > 0
	}, itWait, 10*time.Millisecond, "the runner never blocked behind the holder")
}

type itRun struct {
	res Result
	err error
}

func itGo(r *Runner, req Request) <-chan itRun {
	ch := make(chan itRun, 1)
	go func() {
		res, err := r.Run(context.Background(), req)
		ch <- itRun{res, err}
	}()
	return ch
}

func itWaitRun(t *testing.T, ch <-chan itRun) itRun {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(itWait):
		t.Fatal("the run did not finish")
		return itRun{}
	}
}

// itPause parks a runner inside its transaction at beforeCommit.
type itPause struct {
	reached chan struct{}
	release chan struct{}
	once    sync.Once
	let     sync.Once
}

func newItPause(t *testing.T) *itPause {
	p := &itPause{reached: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(p.free)
	return p
}

func (p *itPause) hook() func() {
	return func() {
		p.once.Do(func() { close(p.reached) })
		<-p.release
	}
}

func (p *itPause) free() { p.let.Do(func() { close(p.release) }) }

func (p *itPause) wait(t *testing.T) {
	t.Helper()
	select {
	case <-p.reached:
	case <-time.After(itWait):
		t.Fatal("the runner never reached beforeCommit")
	}
}

// ---------------------------------------------------------------------------
// In-transaction observer
// ---------------------------------------------------------------------------

// itObs wraps a registry so every applier and the notice sink record, inside the runner's own
// transaction, the auth-change source and the writer-barrier marker in force, and can be made to
// fail on the Nth call (after doing its write).
type itObs struct {
	mu      sync.Mutex
	sources []string
	markers []string
	calls   int
	failAt  int // 1-based call that fails after its write; 0 never
}

func (o *itObs) note(ctx context.Context, tx *sql.Tx) (int, error) {
	var src, marker string
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(current_setting('concord.auth_change_source', true), ''),
		COALESCE(current_setting('concord.loginfamily_authority', true), '')`).Scan(&src, &marker); err != nil {
		return 0, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.sources = append(o.sources, src)
	o.markers = append(o.markers, marker)
	o.calls++
	return o.calls, nil
}

func (o *itObs) fail(n int) error {
	if o.failAt != 0 && n == o.failAt {
		return errItInjected
	}
	return nil
}

type itSink struct {
	inner NoticeInserter
	obs   *itObs
}

func (s *itSink) InsertTx(ctx context.Context, tx *sql.Tx, u uuid.UUID, ids map[int]uuid.UUID, ns []decide.Notice) error {
	n, err := s.obs.note(ctx, tx)
	if err != nil {
		return err
	}
	if err := s.inner.InsertTx(ctx, tx, u, ids, ns); err != nil {
		return err
	}
	return s.obs.fail(n)
}

// wrap returns a copy of reg whose wired effect appliers and notice sink report to o.
func (o *itObs) wrap(reg registry) registry {
	out := reg
	out.effects = make(map[effectKey]applier, len(reg.effects))
	for k, ap := range reg.effects {
		if ap.wired {
			inner := ap.apply
			ap.apply = func(ctx context.Context, tx *sql.Tx, a applyCtx, ef decide.Effect) error {
				n, err := o.note(ctx, tx)
				if err != nil {
					return err
				}
				if err := inner(ctx, tx, a, ef); err != nil {
					return err
				}
				return o.fail(n)
			}
		}
		out.effects[k] = ap
	}
	if !isNilInterface(reg.notices) {
		out.notices = &itSink{inner: reg.notices, obs: o}
	}
	return out
}

func (o *itObs) snapshot() (sources, markers []string) {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]string(nil), o.sources...), append([]string(nil), o.markers...)
}

// itOutcome is the outcome decide gives the fixture, read the way rnEnv.prove reads it.
func itOutcome(t *testing.T, e *rnEnv, a rnAccount, act decide.Action, proof decide.Proof) decide.Outcome {
	t.Helper()
	ctx := context.Background()
	tx, err := e.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	in, err := loadInputTx(ctx, tx, a.id(), snapKeyID, time.Now())
	require.NoError(t, err)
	in.Act, in.Proof = act, proof
	return decide.Decide(in)
}

// itCancelFixture is an account with two open requests, of which act cancels the first: the
// second is locked by the loader and never written.
func itCancelFixture(t *testing.T, e *rnEnv) (a rnAccount, r1, r2 uuid.UUID, act decide.Action, proof decide.Proof) {
	t.Helper()
	a = e.account(snapLegacyMaturity())
	r1 = e.pendingRemove(a)
	second := snapOpenReq(decide.ReqCredentialRemove, a.sso)
	second.originClass = decide.C1
	snapInsertReq(t, e.db, a.id(), second)
	act = decide.Cancel{IDs: []string{r1.String()}}
	proof = decide.Proof{Used: []decide.CredRef{a.user.pwRef}}
	e.prove(a, act, proof, rnWant{row: decide.RowT7})
	return a, r1, second.id, act, proof
}

// ---------------------------------------------------------------------------
// AC-int: lock order and modes
// ---------------------------------------------------------------------------

// R5: barrier, users (NO KEY UPDATE), the actor's family, the activation marker (SHARE), the
// request rows (UPDATE), then the factor rows (NO KEY UPDATE) in table order. Each stage parks
// the runner behind a holder on the NEXT lock of the order and reads what the runner already
// holds; the last stage reads the full set at beforeCommit. A lock acquired out of order shows
// as held in an earlier stage than its place, or as missing in a later one.
func TestRunner_LockOrderViaPgLocks(t *testing.T) {
	const (
		share, nku, upd, none = "SHARE", "NO KEY UPDATE", "UPDATE", "none"
	)
	runBehind := func(t *testing.T, holdSQL func(a rnAccount) (string, []any), want map[string]string) {
		t.Helper()
		e := newRnEnv(t)
		a, r1, r2, act, proof := itCancelFixture(t, e)
		lockSQL, args := holdSQL(a)
		h := itHold(t, e.db, lockSQL, args...)
		rs := itResources(a, r1, r2)
		done := itGo(e.runner(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		h.waitBlocked(t, e.db)
		names := make([]string, 0, len(want))
		for n := range want {
			names = append(names, n)
		}
		// The parked runner waits under SET LOCAL lock_timeout = decide.LockTimeout (3 s), so
		// every probe below must finish inside it. Each probe has its own, longer deadline
		// (itConflicts, 10 s), so a starved host shows here as a clear failure, not a hang.
		got := itModes(t, e.db, rs, names...)
		select {
		case early := <-done:
			t.Fatalf("the runner left its wait before the probes finished (lock_timeout %v): %v", decide.LockTimeout, early.err)
		default:
		}
		require.NoError(t, h.tx.Rollback())
		out := itWaitRun(t, done)
		require.NoError(t, out.err)
		assert.Equal(t, want, got)
		assert.Equal(t, "cancelled", e.reqStatus(r1), "released, the run completes")
	}

	t.Run("behind the barrier: nothing after it is held", func(t *testing.T) {
		runBehind(t, func(rnAccount) (string, []any) {
			return `SELECT 1 FROM login_family_rollout WHERE id = TRUE FOR UPDATE`, nil
		}, map[string]string{"users": none, "family": none, "state": none, "req1": none, "totp": none})
	})
	t.Run("behind the users row: the barrier is held; the family, the marker and everything later are not", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{
			"rollout": share, "family": none, "state": none, "req1": none, "req2": none,
			"totp": none, "webauthn": none, "sso": none, "recovery": none,
		})
	})
	t.Run("behind the family: barrier and users are held; the marker and everything later are not", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM login_families WHERE family_id = $1 FOR UPDATE`, []any{a.fam}
		}, map[string]string{
			"rollout": share, "users": nku, "state": none, "req1": none, "req2": none,
			"totp": none, "webauthn": none, "sso": none, "recovery": none,
		})
	})
	t.Run("behind the marker: barrier, users and family are held; no request or factor row is", func(t *testing.T) {
		runBehind(t, func(rnAccount) (string, []any) {
			return `SELECT 1 FROM accountguard_state WHERE singleton FOR UPDATE`, nil
		}, map[string]string{
			"rollout": share, "users": nku, "family": nku, "req1": none, "req2": none,
			"totp": none, "webauthn": none, "sso": none, "recovery": none,
		})
	})
	t.Run("behind the request rows: barrier, users, family and marker are held; no factor row is", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM account_pending_requests WHERE user_id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{
			"rollout": share, "users": nku, "family": nku, "state": share,
			"totp": none, "webauthn": none, "sso": none, "recovery": none,
		})
	})
	t.Run("behind the first factor row: the request rows are held, the later factor tables are not", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM user_mfa_totp WHERE user_id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{
			"rollout": share, "users": nku, "family": nku, "state": share,
			"req1": upd, "req2": upd, "webauthn": none, "sso": none, "recovery": none,
		})
	})
	t.Run("behind the last factor row: every earlier factor table is held", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM user_recovery_keys WHERE user_id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{
			"rollout": share, "users": nku, "family": nku, "state": share, "req1": upd, "req2": upd,
			"totp": nku, "webauthn": nku, "sso": nku,
		})
	})
	t.Run("behind the second factor table: the first is held, the third and fourth are not", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM user_mfa_webauthn WHERE user_id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{"totp": nku, "sso": none, "recovery": none})
	})
	t.Run("behind the third factor table: the first two are held, the fourth is not", func(t *testing.T) {
		runBehind(t, func(a rnAccount) (string, []any) {
			return `SELECT 1 FROM user_sso_identities WHERE user_id = $1 FOR UPDATE`, []any{a.id()}
		}, map[string]string{"totp": nku, "webauthn": nku, "recovery": none})
	})

	t.Run("at commit: every lock is held in its mode, the barrier marker is set, no advisory lock", func(t *testing.T) {
		e := newRnEnv(t)
		a, r1, r2, act, proof := itCancelFixture(t, e)
		obs := &itObs{}
		r := newTestRunner(e.deps, obs.wrap(e.reg))
		p := newItPause(t)
		r.beforeCommit = p.hook()
		done := itGo(r, e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		p.wait(t)

		got := itModes(t, e.db, itResources(a, r1, r2),
			"rollout", "users", "family", "state", "req2", "totp", "webauthn", "sso", "recovery")
		assert.Equal(t, map[string]string{
			"rollout": share, "users": nku, "family": nku, "state": share, "req2": upd,
			"totp": nku, "webauthn": nku, "sso": nku, "recovery": nku,
		}, got)
		// The cancelled row was locked UPDATE by the loader and then written without touching a
		// key column, which leaves the lock at NO KEY UPDATE: never weaker.
		assert.Contains(t, []string{nku, upd}, itHeldMode(t, e.db, `SELECT 1 FROM account_pending_requests WHERE id = $1`, r1))

		// pg_locks: the paused backend holds a relation lock on every table of the order, and the
		// cancel is a write on the request table. PR3 names no address, so it holds no advisory lock.
		var holders, pid int
		require.NoError(t, e.db.QueryRow(`SELECT COUNT(DISTINCT l.pid), COALESCE(MIN(l.pid), 0)
			FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE c.relname = 'accountguard_state' AND l.mode = 'RowShareLock' AND l.granted
			  AND l.pid <> pg_backend_pid()`).Scan(&holders, &pid))
		require.Equal(t, 1, holders, "exactly one backend holds the marker: the paused runner")
		rels := func(mode string) []string {
			rows, err := e.db.Query(`SELECT DISTINCT c.relname FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				WHERE l.pid = $1 AND l.locktype = 'relation' AND l.mode = $2 AND l.granted`, pid, mode)
			require.NoError(t, err)
			defer func() { _ = rows.Close() }()
			var out []string
			for rows.Next() {
				var n string
				require.NoError(t, rows.Scan(&n))
				out = append(out, n)
			}
			require.NoError(t, rows.Err())
			return out
		}
		assert.Subset(t, rels("RowShareLock"), []string{
			"login_family_rollout", "users", "login_families", "accountguard_state",
			"account_pending_requests", "user_mfa_totp", "user_mfa_webauthn", "user_sso_identities", "user_recovery_keys",
		})
		assert.Contains(t, rels("RowExclusiveLock"), "account_pending_requests")
		var advisory int
		require.NoError(t, e.db.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE pid = $1 AND locktype = 'advisory'`, pid).Scan(&advisory))
		assert.Zero(t, advisory)

		p.free()
		require.NoError(t, itWaitRun(t, done).err)
		sources, markers := obs.snapshot()
		require.NotEmpty(t, markers, "the cancel applied an effect")
		for _, m := range markers {
			assert.NotEmpty(t, m, "the writer-barrier marker is set before any write")
		}
		for _, s := range sources {
			assert.Equal(t, string(decide.SourceUserSession), s)
		}
	})
}

// ---------------------------------------------------------------------------
// AC-int: a cancel that commits between the snapshot and the request lock
// ---------------------------------------------------------------------------

func TestRunner_CancelBetweenScanAndLockWins(t *testing.T) {
	e := newRnEnv(t)
	var logs bytes.Buffer
	e.deps.Log = slog.New(slog.NewJSONHandler(&logs, nil))
	a, reqID, act := e.forcedFixture() // a due break-glass Complete: T11 unless something intervenes
	prior, priorHash := e.epoch(a.id()), e.passwordHash(a.id())
	history := e.count(`SELECT COUNT(*) FROM user_credential_history WHERE user_id = $1`, a.id())
	override := e.count(`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, a.id())

	// The canceller holds the request row and has written its cancel, uncommitted.
	canceller := itHold(t, e.db, `UPDATE account_pending_requests
		SET status = 'cancelled', cancelled_at = now(), cancel_reason = 'proof', canceller_class = 1,
		    pending_password_hash = NULL WHERE id = $1`, reqID) // pragma: allowlist secret
	rev := &rnRev{proofs: []decide.Proof{{}}}
	done := itGo(e.runner(), e.req(a, act, rev, false))
	canceller.waitBlocked(t, e.db) // the runner is queued on the request lock, behind the cancel
	require.NoError(t, canceller.tx.Commit())
	out := itWaitRun(t, done)

	require.NoError(t, out.err, "a refusal is an outcome, not an error")
	assert.Equal(t, decide.VerdictRefused, out.res.Outcome.Verdict)
	assert.Equal(t, decide.RowNone, out.res.Outcome.Row, "Decide saw the request cancelled")
	assert.Nil(t, out.res.Continuation)
	assert.Equal(t, 1, rev.count(), "no pass 2")
	assert.Equal(t, "cancelled", e.reqStatus(reqID))
	assert.Equal(t, priorHash, e.passwordHash(a.id()))
	assert.Equal(t, history, e.count(`SELECT COUNT(*) FROM user_credential_history WHERE user_id = $1`, a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.True(t, e.webauthnExists(a), "the reset removed no factor")
	assert.Equal(t, override, e.count(`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, a.id()))
	assert.Equal(t, 1, e.customTier(a.id()), "no forced clear")
	assert.Empty(t, e.cmds.seq(a.id()), "no fence op")
	assert.False(t, e.familyTerminal(a.fam))
	assert.Empty(t, e.fam.got())
	assert.Empty(t, e.perms.got())
	assert.Equal(t, 1, strings.Count(logs.String(), `"failure_class":"complete_refused"`), "one refused completion, counted")
}

// ---------------------------------------------------------------------------
// AC-int: a completion is all or nothing
// ---------------------------------------------------------------------------

func TestRunner_CompletionAllOrNothing(t *testing.T) {
	assertNothingLanded := func(t *testing.T, e *rnEnv, a rnAccount, reqID uuid.UUID, prior, priorHash string, history, override int) {
		t.Helper()
		assert.Equal(t, priorHash, e.passwordHash(a.id()), "no password change")
		assert.Equal(t, history, e.count(`SELECT COUNT(*) FROM user_credential_history WHERE user_id = $1`, a.id()), "no history row")
		assert.Equal(t, "pending", e.reqStatus(reqID), "the request is still pending")
		assert.Equal(t, prior, e.epoch(a.id()))
		assert.True(t, e.webauthnExists(a))
		assert.Equal(t, override, e.count(`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, a.id()))
		assert.Equal(t, 1, e.customTier(a.id()), "the forced clear never ran: the failure came before the commit")
		assert.Equal(t, []string{"begin", "script"}, e.cmds.seq(a.id()), "the op is settled by Rollback")
		assert.Empty(t, e.fenceState(a.id()))
		assert.False(t, e.familyTerminal(a.fam))
		assert.Empty(t, e.fam.got())
		assert.Empty(t, e.perms.got())
		assert.Empty(t, e.minter.got())
		assert.Empty(t, e.evict.got())
	}
	setup := func(t *testing.T) (*rnEnv, rnAccount, uuid.UUID, decide.Action, string, string, int, int) {
		e := newRnEnv(t)
		a, reqID, act := e.forcedFixture()
		return e, a, reqID, act, e.epoch(a.id()), e.passwordHash(a.id()),
			e.count(`SELECT COUNT(*) FROM user_credential_history WHERE user_id = $1`, a.id()),
			e.count(`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, a.id())
	}

	t.Run("the last effect applier fails after its write", func(t *testing.T) {
		e, a, reqID, act, prior, priorHash, history, override := setup(t)
		order := applyOrder(itOutcome(t, e, a, act, decide.Proof{}))
		require.Greater(t, len(order), 1, "a completion has more than one applier")
		obs := &itObs{failAt: len(order)}
		res, err := newTestRunner(e.deps, obs.wrap(e.reg)).Run(context.Background(),
			e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))

		require.ErrorIs(t, err, errItInjected)
		assert.Equal(t, Result{}, res)
		assert.Equal(t, len(order), obs.calls, "every applier ran: the failure was the last one")
		assertNothingLanded(t, e, a, reqID, prior, priorHash, history, override)
	})
	t.Run("the notice sink fails after every applier wrote", func(t *testing.T) {
		e, a, reqID, act, prior, priorHash, history, override := setup(t)
		order := applyOrder(itOutcome(t, e, a, act, decide.Proof{}))
		obs := &itObs{failAt: len(order) + 1} // the sink's call
		res, err := newTestRunner(e.deps, obs.wrap(e.reg)).Run(context.Background(),
			e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))

		require.ErrorIs(t, err, errItInjected)
		assert.Equal(t, Result{}, res)
		assert.Equal(t, len(order)+1, obs.calls)
		assertNothingLanded(t, e, a, reqID, prior, priorHash, history, override)
	})
}

// ---------------------------------------------------------------------------
// AC-int: ambiguous and non-rotating commits
// ---------------------------------------------------------------------------

func TestRunner_AmbiguousRotatingCommitReconcilesAndCloses(t *testing.T) {
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
	assert.Equal(t, Result{}, res)
	now := e.epoch(a.id())
	require.NotEqual(t, prior, now, "the injected commit was durable")
	assert.Equal(t, [][3]string{{a.id().String(), now, prior}}, e.evict.got(), "ReconcileCommittedEviction asked for the exact generation")
	assert.Equal(t, []uuid.UUID{a.fam}, e.fam.got(), "CloseCommittedFamilies ran")
	assert.True(t, e.familyTerminal(a.fam))
	state := e.fenceState(a.id())
	assert.True(t, strings.HasPrefix(state, "blocked:"), "the blocked: marker is kept, not committed or rolled back")
	ttl, terr := e.rdb.TTL(context.Background(), credepoch.Key(a.id().String())).Result()
	require.NoError(t, terr)
	assert.True(t, ttl > 0 && ttl <= 5*time.Minute, "the kept marker expires on its own bound: %v", ttl)
	assert.Equal(t, []string{"begin"}, e.cmds.seq(a.id()), "neither Commit nor Rollback")
	assert.Empty(t, e.minter.got())
}

func TestRunner_ValidNonRotatingProofLeavesNoBlockedMarker(t *testing.T) {
	e := newRnEnv(t)
	a, r1, r2, act, proof := itCancelFixture(t, e)

	var duringCommit []string
	r := e.runner()
	r.beforeCommit = func() { duringCommit = append(duringCommit, e.fenceState(a.id())) }
	res, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))

	require.NoError(t, err)
	assert.Equal(t, decide.RowT7, res.Outcome.Row)
	assert.Equal(t, decide.VerdictApplied, res.Outcome.Verdict)
	assert.Equal(t, "cancelled", e.reqStatus(r1))
	assert.Equal(t, "pending", e.reqStatus(r2), "only the named request was cancelled")
	assert.Equal(t, []string{""}, duringCommit, "no marker at the instant of commit")
	assert.Empty(t, e.fenceState(a.id()), "no marker after it")
	assert.Empty(t, e.cmds.seq(a.id()), "no Redis command on the user's epoch key at all")
	assert.Empty(t, e.evict.got())
}

// ---------------------------------------------------------------------------
// AC-int: the strategy a SetsPassword outcome commits through
// ---------------------------------------------------------------------------

func TestRunner_SetsPasswordCommitsThroughForcedClear(t *testing.T) {
	t.Run("T11 sets a password: CompleteForcedSecurityClear commits, the plain COMMIT never runs", func(t *testing.T) {
		e := newRnEnv(t)
		a, reqID, act := e.forcedFixture()
		var plain int
		var seq []string // beforeCommit, then every commit the presence boundary performs
		r := e.runner()
		r.commitPlain = func(tx *sql.Tx) error { plain++; return tx.Commit() }
		r.beforeCommit = func() { seq = append(seq, "before") }
		restore := e.presence.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Commit: func(tx *sql.Tx) error { seq = append(seq, "forced"); return tx.Commit() },
		})
		t.Cleanup(restore)

		res, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{{}}}, false))

		require.NoError(t, err)
		assert.True(t, res.Outcome.SetsPassword)
		assert.Zero(t, plain, "the plain strategy never committed")
		// The first commit is the runner's transaction; the claim's own transaction commits
		// through the same boundary afterwards.
		require.GreaterOrEqual(t, len(seq), 2)
		assert.Equal(t, []string{"before", "forced"}, seq[:2], "the runner's commit went through the forced clear")
		assert.Equal(t, appPendingHash, e.passwordHash(a.id()))
		assert.Equal(t, "completed", e.reqStatus(reqID))
		assert.Equal(t, 0, e.customTier(a.id()), "the forced clear ran in the same transaction")
	})
	t.Run("T1 sets no password: the plain COMMIT runs and the forced clear never does", func(t *testing.T) {
		e := newRnEnv(t)
		a := e.account(snapLegacyMaturity())
		proof := a.loginProof()
		e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
		var plain, forcedCommits int
		r := e.runner()
		r.commitPlain = func(tx *sql.Tx) error { plain++; return tx.Commit() }
		restore := e.presence.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Commit: func(tx *sql.Tx) error { forcedCommits++; return tx.Commit() },
		})
		t.Cleanup(restore)

		_, err := r.Run(context.Background(), e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{proof}}, true))

		require.NoError(t, err)
		assert.Equal(t, 1, plain)
		assert.Zero(t, forcedCommits)
		assert.Equal(t, 1, e.customTier(a.id()), "no forced clear")
	})
}

// ---------------------------------------------------------------------------
// AC-int: rotation evidence
// ---------------------------------------------------------------------------

func TestRunner_RotationRevokesFamiliesAndEnqueuesEjection(t *testing.T) {
	const ejections = `SELECT credential_epoch, superseded_credential_epoch, attempts, failure_class
		FROM credential_epoch_voice_ejections WHERE user_id = $1`
	type ejection struct {
		epoch, superseded string
		attempts          int
		failure           sql.NullString
	}
	readEjections := func(t *testing.T, e *rnEnv, id uuid.UUID) []ejection {
		rows, err := e.db.Query(ejections, id)
		require.NoError(t, err)
		defer func() { _ = rows.Close() }()
		var out []ejection
		for rows.Next() {
			var x ejection
			require.NoError(t, rows.Scan(&x.epoch, &x.superseded, &x.attempts, &x.failure))
			out = append(out, x)
		}
		require.NoError(t, rows.Err())
		return out
	}

	t.Run("a T1b rotation terminalizes every old-epoch family and enqueues one ejection", func(t *testing.T) {
		e := newRnEnv(t)
		a := e.account(rnImmature())
		// A second session family under the same epoch.
		ctx := testdb.ActorContext(context.Background(), t, e.db, a.id().String())
		ident, ok := loginfamily.AccessIdentityFromContext(ctx)
		require.True(t, ok)
		var fam2 uuid.UUID
		require.NoError(t, e.db.QueryRow(`SELECT family_id FROM login_family_sids WHERE sid = $1`, ident.SID).Scan(&fam2))
		require.NotEqual(t, a.fam, fam2, "a second family")
		proof := a.loginProof()
		e.prove(a, decide.SignOutEverywhere{}, proof, rnWant{row: decide.RowT1b, rotate: true, perms: true})
		prior := e.epoch(a.id())
		require.Empty(t, readEjections(t, e, a.id()), "no obligation before the run")
		require.False(t, e.familyTerminal(a.fam))
		require.False(t, e.familyTerminal(fam2))

		_, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutEverywhere{}, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.NoError(t, err)

		now := e.epoch(a.id())
		require.NotEqual(t, prior, now)
		assert.True(t, e.familyTerminal(a.fam))
		assert.True(t, e.familyTerminal(fam2))
		assert.ElementsMatch(t, []uuid.UUID{a.fam, fam2}, e.fam.got(), "both families' sockets are fenced")
		got := readEjections(t, e, a.id())
		require.Len(t, got, 1, "exactly one ejection row")
		assert.Equal(t, ejection{epoch: now, superseded: prior}, got[0], "the exact generation, undelivered")
	})
	t.Run("a non-rotating run revokes nothing and enqueues nothing", func(t *testing.T) {
		e := newRnEnv(t)
		a, _, _, act, proof := itCancelFixture(t, e)
		_, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.NoError(t, err)
		assert.Empty(t, readEjections(t, e, a.id()))
		assert.False(t, e.familyTerminal(a.fam))
		assert.Empty(t, e.fam.got())
	})
}

// ---------------------------------------------------------------------------
// AC-int: a continuation only follows a definite commit
// ---------------------------------------------------------------------------

// itMinter records, at the instant it is asked, the epoch another connection can read: a mint
// that ran before the commit would see the superseded epoch.
type itMinter struct {
	e     *rnEnv
	mu    sync.Mutex
	asked []itAsk
}

type itAsk struct{ epoch, durable, fence string }

func (m *itMinter) MintContinuation(_ context.Context, userID uuid.UUID, epoch string) (Continuation, error) {
	durable := m.e.epoch(userID)
	fence := m.e.fenceState(userID)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.asked = append(m.asked, itAsk{epoch, durable, fence})
	return rnContinuation, nil
}

func (m *itMinter) got() []itAsk {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]itAsk(nil), m.asked...)
}

func TestRunner_ContinuationOnlyAfterDefiniteCommit(t *testing.T) {
	setup := func(t *testing.T) (*rnEnv, rnAccount, decide.Proof, *itMinter) {
		e := newRnEnv(t)
		a := e.account(snapLegacyMaturity())
		proof := a.loginProof()
		e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
		m := &itMinter{e: e}
		e.deps.Minter = m
		return e, a, proof, m
	}
	req := func(e *rnEnv, a rnAccount, proof decide.Proof, failOn int) Request {
		return e.req(a, decide.SignOutOthers{}, &rnRev{proofs: []decide.Proof{proof}, failOn: failOn}, true)
	}

	t.Run("a definite commit mints once, bound to the committed epoch, after the commit", func(t *testing.T) {
		e, a, proof, m := setup(t)
		res, err := e.runner().Run(context.Background(), req(e, a, proof, 0))
		require.NoError(t, err)
		now := e.epoch(a.id())
		require.NotNil(t, res.Continuation)
		assert.Equal(t, []itAsk{{epoch: now, durable: now, fence: "active:" + now}}, m.got(),
			"at mint time the new epoch was durable and published")
	})
	t.Run("an ambiguous commit mints nothing", func(t *testing.T) {
		e, a, proof, m := setup(t)
		r := e.runner()
		r.commitPlain = func(tx *sql.Tx) error {
			require.NoError(t, tx.Commit())
			return errRnAmbiguous
		}
		res, err := r.Run(context.Background(), req(e, a, proof, 0))
		require.Error(t, err)
		assert.Equal(t, Result{}, res)
		assert.Empty(t, m.got())
	})
	t.Run("a definite rollback mints nothing", func(t *testing.T) {
		e, a, proof, m := setup(t)
		prior := e.epoch(a.id())
		res, err := e.runner().Run(context.Background(), req(e, a, proof, 2))
		require.ErrorIs(t, err, errRnRevalidate)
		assert.Equal(t, Result{}, res)
		assert.Empty(t, m.got())
		assert.Equal(t, prior, e.epoch(a.id()))
	})
	t.Run("a pass-2 refusal mints nothing", func(t *testing.T) {
		e, a, proof, m := setup(t)
		rev := &rnRev{proofs: []decide.Proof{proof, {}}}
		res, err := e.runner().Run(context.Background(), e.req(a, decide.SignOutOthers{}, rev, true))
		require.NoError(t, err)
		assert.Nil(t, res.Continuation)
		assert.Empty(t, m.got())
	})
	t.Run("a non-rotating outcome mints nothing", func(t *testing.T) {
		e := newRnEnv(t)
		m := &itMinter{e: e}
		e.deps.Minter = m
		a, _, _, act, proof := itCancelFixture(t, e)
		res, err := e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
		require.NoError(t, err)
		assert.Nil(t, res.Continuation)
		assert.Empty(t, m.got())
	})
}

// ---------------------------------------------------------------------------
// Task 10 carries
// ---------------------------------------------------------------------------

// Every outcome's writes are attributed to its own decide.Source. Nothing records the setting
// durably (no migration reads concord.auth_change_source), so it is read inside the runner's
// transaction by the wrapped appliers, after sourceTx and before each write.
func TestRunner_PerOutcomeSourceAttribution(t *testing.T) {
	type tc struct {
		name    string
		build   func(t *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool)
		source  decide.Source
		session bool
	}
	cases := []tc{
		{"T7 cancel", func(t *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool) {
			a, _, _, act, proof := itCancelFixture(t, e)
			return a, act, proof, rnWant{row: decide.RowT7}, true
		}, decide.SourceUserSession, true},
		{"T7 cancel, run by a worker (no session)", func(t *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool) {
			a, _, _, act, proof := itCancelFixture(t, e)
			return a, act, proof, rnWant{row: decide.RowT7}, false
		}, decide.SourceUserSession, false},
		{"T2 remove", func(_ *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool) {
			a := e.account(snapLegacyMaturity())
			return a, decide.Remove{Target: a.sso}, a.loginProof(), rnWant{row: decide.RowT2, perms: true}, true
		}, decide.SourceUserSession, true},
		{"T1b sign out everywhere", func(_ *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool) {
			a := e.account(rnImmature())
			return a, decide.SignOutEverywhere{}, a.loginProof(), rnWant{row: decide.RowT1b, rotate: true, perms: true}, true
		}, decide.SourceUserSession, true},
		{"T11 break-glass completion", func(_ *testing.T, e *rnEnv) (rnAccount, decide.Action, decide.Proof, rnWant, bool) {
			a := e.account(snapLegacyMaturity())
			id := e.breakGlass(a)
			return a, decide.Complete{ID: id.String()}, decide.Proof{}, rnWant{row: decide.RowT11, rotate: true, sets: true, perms: true}, false
		}, decide.SourceBreakGlass, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := newRnEnv(t)
			a, act, proof, want, session := c.build(t, e)
			e.prove(a, act, proof, want)
			obs := &itObs{}
			res, err := newTestRunner(e.deps, obs.wrap(e.reg)).Run(context.Background(),
				e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, session))
			require.NoError(t, err)

			assert.Equal(t, c.source, res.Outcome.Source)
			sources, _ := obs.snapshot()
			require.NotEmpty(t, sources, "an applier ran inside the transaction")
			for _, s := range sources {
				assert.Equal(t, string(res.Outcome.Source), s, "the write ran under the run's own source")
			}
		})
	}
}

// A session request whose token epoch is stale is a 401: the subject step's refusal, mapped by
// subjectError, reaches HTTPStatus as a denial.
func TestRunner_TokenEpochMismatchIs401(t *testing.T) {
	e := newRnEnv(t)
	a := e.account(snapLegacyMaturity())
	proof := a.loginProof()
	e.prove(a, decide.SignOutOthers{}, proof, rnWant{row: decide.RowT1, rotate: true, cont: true})
	prior := e.epoch(a.id())

	pol := &rnPolicy{}
	rev := &rnRev{proofs: []decide.Proof{proof}}
	r := e.runner()
	r.policy = pol.fn()
	req := e.req(a, decide.SignOutOthers{}, rev, true)
	req.TokenEpoch = strings.Repeat("ab", 16) // not the user's epoch
	res, err := r.Run(context.Background(), req)

	require.ErrorIs(t, err, loginfamily.ErrDenied)
	var lfErr *loginfamily.Error
	require.ErrorAs(t, err, &lfErr)
	assert.Equal(t, loginfamily.ReasonEpochMismatch, lfErr.Reason())
	var suErr *stepup.Error
	require.ErrorAs(t, err, &suErr, "the subject refusal is the cause")
	assert.Equal(t, 401, suErr.Status)
	status, _ := HTTPStatus(err)
	assert.Equal(t, 401, status)
	assert.Equal(t, Result{}, res)
	assert.Zero(t, pol.count())
	assert.Zero(t, rev.count())
	assert.Empty(t, e.cmds.seq(a.id()))
	assert.Equal(t, prior, e.epoch(a.id()))
	assert.False(t, e.familyTerminal(a.fam))
}

// A worker (Session == nil) skips the family guard and the subject's epoch match: it runs while
// users.credential_epoch matches no token, and locks no family row. The same account refuses a
// session request, which is the control that the match would otherwise have run.
func TestRunner_SessionlessRequestSkipsTheSessionSQL(t *testing.T) {
	e := newRnEnv(t)
	a, r1, _, act, proof := itCancelFixture(t, e)
	_, err := e.db.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, strings.Repeat("cd", 16), a.id())
	require.NoError(t, err)

	// Control: with a session, the epoch the token carries no longer matches.
	_, err = e.runner().Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, true))
	require.Error(t, err)
	status, _ := HTTPStatus(err)
	require.Equal(t, 401, status, "the session path runs the epoch match")
	require.Equal(t, "pending", e.reqStatus(r1))

	var familyMode string
	rs := itResources(a, r1)
	r := e.runner()
	r.beforeCommit = func() { familyMode = itHeldMode(t, e.db, rs[2].base, rs[2].args...) }
	res, err := r.Run(context.Background(), e.req(a, act, &rnRev{proofs: []decide.Proof{proof}}, false))

	require.NoError(t, err, "the sessionless run does not match a token epoch")
	assert.Equal(t, decide.RowT7, res.Outcome.Row)
	assert.Equal(t, "cancelled", e.reqStatus(r1))
	assert.Equal(t, "none", familyMode, "no family row is locked")
	assert.False(t, e.familyTerminal(a.fam))
}
