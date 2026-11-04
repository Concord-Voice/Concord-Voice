package accountguard

// ---------------------------------------------------------------------------
// The applier API these tests were written against (Task 9, #3680 PR3). The producer
// implements to it; names the brief and rulings already fix are used verbatim.
//
//	type applyCtx struct {
//	    userID              uuid.UUID
//	    now                 time.Time               // the database clock; every "now" a statement writes
//	    outcome             decide.Outcome          // Outcome.EnrolClass is the break-glass password's class
//	    reqs                []decide.Req            // the same slice covers() reads; keys a Complete by ReqKind
//	    pendingPasswordHash string                  // break_glass OPEN only (ruling 3)
//	    newGroupID          uuid.UUID               // the outcome's one new dispute group
//	    requestIDs          map[int]uuid.UUID       // effect index -> request id (see below)
//	    // plus whatever unexported fields the producer needs (for example the effect index)
//	}
//
//	func (r registry) covers(a applyCtx) error                                  // reads a.outcome, a.reqs
//	func (r registry) apply(ctx context.Context, tx *sql.Tx, a applyCtx, i int) error  // e = a.outcome.Effects[i]
//	func applyOrder(o decide.Outcome) []int  // indices; hold-folds on existing requests first (ruling 16)
//
// apply is the dispatcher. e is a.outcome.Effects[i]. It builds the
// effectKey of e (Cred for Remove and Contest, Req for Complete, read from a.reqs by
// e.RequestID), looks it up, refuses with ErrEffectUnwired when the key is absent or not
// wired (before touching tx), and otherwise runs applier.apply(ctx, tx, a, e). It is
// defence in depth behind covers; Task 10 calls covers first, then apply per effect in
// Outcome.Effects order.
//
// Request ids. The CALLER mints them (ruling 3, doc.go "New IDs"): a.requestIDs[i] holds
// the id Open must insert for the EffectOpen at index i, and Open leaves the entry as it
// found it. An Open with no entry is a contract error (ErrContract). Every other effect
// names its request by e.RequestID (canonical uuid text).
//
// Errors. Zero rows where exactly one is expected wraps ErrStale (guarded transitions,
// Remove, ReleaseReservation). Contest is idempotent and is the one exception: a row that
// is already contested is not an error. Multi-target Open wraps ErrContract.
//
// Password writes (rulings 10 and 12). One writer, setPasswordTx, runs from exactly two
// places: Complete of a break_glass (hash read from the request row's pending value) and
// Open of a break_glass whose Opening is completed (hash from a.pendingPasswordHash; empty is
// ErrContract). Each run increments the counter behind a.passwordWrites, a *int the CALLER
// allocates (tests use appCtx; applyCtx is passed by value, so the counter must be a pointer).
// r.finish(a) returns ErrContract unless that count is 1 when a.outcome.SetsPassword and 0
// when not. ReturnToPending clears disputed_at and dispute_group_id. Contest has the same
// material_version fence as Remove: a missing row or a wrong version is ErrStale; only an
// already-contested row at the right version is a no-op.
//
// Every test below drives apply through productionRegistry() in a real transaction against
// the scratch database, then reads the rows back.
// ---------------------------------------------------------------------------

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/lib/pq/pqerror"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/accountguard/decide"
)

const (
	appPendingHash = "$argon2id$v=19$m=65536,t=3,p=4$cGVuZGluZ3NhbHQ$cGVuZGluZ2hhc2g" // pragma: allowlist secret
	appDecoyHash   = "$argon2id$v=19$m=65536,t=3,p=4$ZGVjb3lzYWx0$ZGVjb3loYXNo"       // pragma: allowlist secret
	appOldHash     = "$argon2id$v=19$m=65536,t=3,p=4$b2xkc2FsdA$b2xkaGFzaA"           // pragma: allowlist secret
	appPendingAddr = "pending@example.test"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func appPtr[T any](v T) *T { return &v }

func appTimePtr(v time.Time) *time.Time {
	if v.IsZero() {
		return nil
	}
	u := v.UTC()
	return &u
}

func appUTC(p *time.Time) *time.Time {
	if p == nil {
		return nil
	}
	u := p.UTC()
	return &u
}

func appBytes(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }

// appCtx is an applyCtx at snapNow for the user, with the given requests.
func appCtx(userID uuid.UUID, reqs ...decide.Req) applyCtx {
	return applyCtx{userID: userID, now: snapNow, reqs: reqs, requestIDs: map[int]uuid.UUID{}, passwordWrites: new(int)}
}

func appReqFor(r snapReq) decide.Req {
	return decide.Req{ID: r.id.String(), Kind: r.kind, Status: r.status}
}

// appRun applies one effect (at index 0) in its own transaction through the production
// registry: committed on success, rolled back on error.
func appRun(t *testing.T, db *sql.DB, a applyCtx, eff decide.Effect) error {
	t.Helper()
	if len(a.outcome.Effects) == 0 {
		a.outcome.Effects = []decide.Effect{eff}
	}
	if a.requestIDs == nil {
		a.requestIDs = map[int]uuid.UUID{}
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	if err := regApply(ctx, productionRegistry(), tx, a, 0, eff); err != nil {
		require.NoError(t, tx.Rollback())
		return err
	}
	require.NoError(t, tx.Commit())
	return nil
}

// appReqRow is one account_pending_requests row, every column.
type appReqRow struct {
	UserID, Kind, Status               string
	TargetKind, TargetID               *string
	TargetVersion                      *int64
	ResetFactors                       bool
	OriginClass                        int
	OriginFP, ReqFP, CoarseFP          []byte
	FPKeyID                            *int16
	DisputeGroup                       *string
	OpenedAt, CompletesAt              time.Time
	HeldMs                             int64
	HeldSince, DisputedAt, CompletedAt *time.Time
	CancelledAt                        *time.Time
	CancelReason                       *string
	CancellerClass                     *int
	PendingHash, PendingAddress        *string
}

const appReqSQL = `SELECT user_id::text, kind, status, target_kind, target_id, target_material_version,
	reset_factors, origin_class, origin_fingerprint, request_fingerprint, coarse_fingerprint,
	fingerprint_key_id, dispute_group_id::text, opened_at, completes_at, held_ms, held_since,
	disputed_at, completed_at, cancelled_at, cancel_reason, canceller_class,
	pending_password_hash, pending_address
	FROM account_pending_requests WHERE id = $1`

type appQuerier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

func appReadReq(t *testing.T, q appQuerier, id uuid.UUID) appReqRow {
	t.Helper()
	var r appReqRow
	err := q.QueryRowContext(context.Background(), appReqSQL, id).Scan(
		&r.UserID, &r.Kind, &r.Status, &r.TargetKind, &r.TargetID, &r.TargetVersion,
		&r.ResetFactors, &r.OriginClass, &r.OriginFP, &r.ReqFP, &r.CoarseFP,
		&r.FPKeyID, &r.DisputeGroup, &r.OpenedAt, &r.CompletesAt, &r.HeldMs, &r.HeldSince,
		&r.DisputedAt, &r.CompletedAt, &r.CancelledAt, &r.CancelReason, &r.CancellerClass,
		&r.PendingHash, &r.PendingAddress,
	)
	require.NoError(t, err)
	r.OpenedAt, r.CompletesAt = r.OpenedAt.UTC(), r.CompletesAt.UTC()
	r.HeldSince, r.DisputedAt, r.CompletedAt, r.CancelledAt =
		appUTC(r.HeldSince), appUTC(r.DisputedAt), appUTC(r.CompletedAt), appUTC(r.CancelledAt)
	return r
}

func appNilIfEmpty(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	return b
}

// appRowOf is the row snapInsertReq stores for r. A test starts from it and edits the
// columns the transition is meant to change.
func appRowOf(t *testing.T, userID uuid.UUID, r snapReq) appReqRow {
	t.Helper()
	row := appReqRow{
		UserID: userID.String(), Kind: snapSpell(t, r.kind), Status: snapSpell(t, r.status),
		ResetFactors: r.resetFactors, OriginClass: int(r.originClass),
		OriginFP: appBytes(7), ReqFP: appNilIfEmpty(r.fingerprint), CoarseFP: appNilIfEmpty(r.coarse),
		OpenedAt: r.openedAt.UTC(), CompletesAt: r.completesAt.UTC(), HeldMs: r.heldMs,
		HeldSince: appTimePtr(r.heldSince), DisputedAt: appTimePtr(r.disputedAt),
		CompletedAt: appTimePtr(r.completedAt), CancelledAt: appTimePtr(r.cancelledAt),
	}
	if r.target.Kind != decide.CredUnknown {
		row.TargetKind = appPtr(snapSpell(t, r.target.Kind))
		row.TargetID = appPtr(r.target.ID)
		row.TargetVersion = appPtr(r.target.Version)
	}
	if r.fpKeyID != 0 {
		row.FPKeyID = appPtr(r.fpKeyID)
	}
	if r.disputeGroup != uuid.Nil {
		row.DisputeGroup = appPtr(r.disputeGroup.String())
	}
	if r.cancelReason != decide.CancelNone {
		row.CancelReason = appPtr(snapSpell(t, r.cancelReason))
	}
	if r.cancellerClass != decide.ClassNone {
		row.CancellerClass = appPtr(int(r.cancellerClass))
	}
	return row
}

// appSetPayload stores the two sealed payloads a pending request can carry.
func appSetPayload(t *testing.T, db *sql.DB, id uuid.UUID, hash, addr string) {
	t.Helper()
	res, err := db.Exec(`UPDATE account_pending_requests SET pending_password_hash = $2, pending_address = $3 WHERE id = $1`,
		id, hash, addr)
	require.NoError(t, err)
	n, err := res.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, n)
}

// appRich is a pending request with everything a terminal CHECK could trip over: an open
// hold, both fingerprints with their key id, both payloads and a settled held_ms.
func appRich(t *testing.T, db *sql.DB, userID uuid.UUID, kind decide.ReqKind, target decide.CredRef, hold bool) snapReq {
	t.Helper()
	r := snapOpenReq(kind, target)
	r.fingerprint, r.coarse, r.fpKeyID = appBytes(1), appBytes(2), snapKeyID
	r.heldMs = 5000
	if hold {
		r.heldSince = snapNow.Add(-time.Hour)
	}
	snapInsertReq(t, db, userID, r)
	appSetPayload(t, db, r.id, appPendingHash, appPendingAddr)
	return r
}

func appRichRow(t *testing.T, userID uuid.UUID, r snapReq) appReqRow {
	row := appRowOf(t, userID, r)
	row.PendingHash, row.PendingAddress = appPtr(appPendingHash), appPtr(appPendingAddr)
	return row
}

func appOrigins(t *testing.T, q *sql.DB, id uuid.UUID) []decide.CredRef {
	t.Helper()
	rows, err := q.Query(`SELECT credential_kind, credential_id, material_version
		FROM account_pending_request_origins WHERE request_id = $1`, id)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var out []decide.CredRef
	for rows.Next() {
		var kind, ref string
		var version int64
		require.NoError(t, rows.Scan(&kind, &ref, &version))
		var k decide.CredKind
		require.NoError(t, k.UnmarshalText([]byte(kind)))
		out = append(out, decide.CredRef{Kind: k, ID: ref, Version: version})
	}
	require.NoError(t, rows.Err())
	return snapSortedRefs(out)
}

// The factor tables, each read by the credential's own id (user_id for the two keyed by user).
var appFactorSQL = map[decide.CredKind]string{
	decide.CredTOTP: `SELECT matures_at, maturity_contested_at, material_version, enrolled_at, enrol_class
		FROM user_mfa_totp WHERE user_id = $1::uuid`,
	decide.CredWebAuthn: `SELECT matures_at, maturity_contested_at, material_version, enrolled_at, enrol_class
		FROM user_mfa_webauthn WHERE id = $1::uuid`,
	decide.CredSSO: `SELECT matures_at, maturity_contested_at, material_version, enrolled_at, enrol_class
		FROM user_sso_identities WHERE id = $1::uuid`,
	decide.CredRecoveryKey: `SELECT matures_at, maturity_contested_at, material_version, enrolled_at, enrol_class
		FROM user_recovery_keys WHERE user_id = $1::uuid`,
}

var appFactorKinds = []decide.CredKind{decide.CredTOTP, decide.CredWebAuthn, decide.CredSSO, decide.CredRecoveryKey}

type appFactorState struct {
	MaturesAt   time.Time
	ContestedAt *time.Time
	Version     int64
	EnrolledAt  time.Time
	EnrolClass  *int
}

// appFactor reads a factor row; the bool is false when there is no such row.
func appFactor(t *testing.T, db *sql.DB, ref decide.CredRef) (appFactorState, bool) {
	t.Helper()
	var s appFactorState
	err := db.QueryRow(appFactorSQL[ref.Kind], ref.ID).Scan(&s.MaturesAt, &s.ContestedAt, &s.Version, &s.EnrolledAt, &s.EnrolClass)
	if err == sql.ErrNoRows {
		return appFactorState{}, false
	}
	require.NoError(t, err)
	s.MaturesAt, s.EnrolledAt, s.ContestedAt = s.MaturesAt.UTC(), s.EnrolledAt.UTC(), appUTC(s.ContestedAt)
	return s, true
}

const appFactorCountSQL = `SELECT
	(SELECT count(*) FROM user_mfa_totp WHERE user_id = $1)
	+ (SELECT count(*) FROM user_mfa_webauthn WHERE user_id = $1)
	+ (SELECT count(*) FROM user_sso_identities WHERE user_id = $1)
	+ (SELECT count(*) FROM user_recovery_keys WHERE user_id = $1)`

func appFactorCount(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(appFactorCountSQL, userID).Scan(&n))
	return n
}

func appRefOf(a snapAccount, k decide.CredKind) decide.CredRef {
	switch k {
	case decide.CredTOTP:
		return a.totp
	case decide.CredWebAuthn:
		return a.web
	case decide.CredSSO:
		return a.sso
	default:
		return a.rec
	}
}

// appImmature gives a factor row distinctive provenance, so a write that disturbs any of it shows.
func appImmature() snapMaturity {
	return snapMaturity{maturesAt: snapNow.Add(24 * time.Hour), enrolledAt: snapNow.Add(-time.Hour), enrolClass: decide.C1}
}

func appFactorAccount(t *testing.T, db *sql.DB) snapAccount {
	t.Helper()
	m := appImmature()
	return snapBuild(t, db, newSnapUser(), m, m, m, m)
}

// ---- break-glass ----

type appUserPw struct {
	Hash       string
	EnrolledAt time.Time
	EnrolClass *int
	Version    int64
}

func appReadUserPw(t *testing.T, q appQuerier, id uuid.UUID) appUserPw {
	t.Helper()
	var u appUserPw
	err := q.QueryRowContext(context.Background(),
		`SELECT password_hash, password_enrolled_at, password_enrol_class, password_version FROM users WHERE id = $1`, id,
	).Scan(&u.Hash, &u.EnrolledAt, &u.EnrolClass, &u.Version)
	require.NoError(t, err)
	u.EnrolledAt = u.EnrolledAt.UTC()
	return u
}

type appHistRow struct {
	Kind                  string
	Hash, Address         *string
	RetiredAt             time.Time
	WasMature             bool
	WasPrimary, WasVerify *bool
}

func appReadHistory(t *testing.T, db *sql.DB, userID uuid.UUID) []appHistRow {
	t.Helper()
	rows, err := db.Query(`SELECT kind, password_hash, address, retired_at, was_mature, was_primary, was_verified
		FROM user_credential_history WHERE user_id = $1 ORDER BY retired_at, id`, userID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var out []appHistRow
	for rows.Next() {
		var h appHistRow
		require.NoError(t, rows.Scan(&h.Kind, &h.Hash, &h.Address, &h.RetiredAt, &h.WasMature, &h.WasPrimary, &h.WasVerify))
		h.RetiredAt = h.RetiredAt.UTC()
		out = append(out, h)
	}
	require.NoError(t, rows.Err())
	return out
}

// appPasswordHistory lists the retired password values of a user, earliest first.
func appPasswordHistory(t *testing.T, db *sql.DB, userID uuid.UUID) []string {
	t.Helper()
	rows, err := db.Query(`SELECT password_hash FROM user_credential_history
		WHERE user_id = $1 AND kind = 'password' ORDER BY retired_at, id`, userID)
	require.NoError(t, err)
	defer func() { assert.NoError(t, rows.Close()) }()
	var out []string
	for rows.Next() {
		var h string
		require.NoError(t, rows.Scan(&h))
		out = append(out, h)
	}
	require.NoError(t, rows.Err())
	return out
}

func appSeedPasswordHistory(t *testing.T, db *sql.DB, userID uuid.UUID, n int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_credential_history (user_id, kind, password_hash, retired_at, was_mature)
		SELECT $1, 'password', 'h-' || g::text, $2::timestamptz + (g::text || ' minutes')::interval, true
		FROM generate_series(0, $3::int - 1) g`, userID, snapNow.Add(-240*time.Hour), n)
	require.NoError(t, err)
}

func appSeedAddressHistory(t *testing.T, db *sql.DB, userID uuid.UUID, n int) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO user_credential_history (user_id, kind, address, retired_at, was_mature, was_primary, was_verified)
		SELECT $1, 'address', 'old-' || g::text || '@example.test',
		       $2::timestamptz + (g::text || ' minutes')::interval, true, false, true
		FROM generate_series(0, $3::int - 1) g`, userID, snapNow.Add(-480*time.Hour), n)
	require.NoError(t, err)
}

// appBreakGlass is an account holding all four factors, with an old stored password whose
// maturity is the argument, and a pending break-glass carrying a pending password, a hold,
// fingerprints and reset_factors.
func appBreakGlass(t *testing.T, db *sql.DB, oldMatures time.Time) (snapAccount, snapReq) {
	t.Helper()
	u := newSnapUser()
	u.passwordHash = appOldHash                                              // pragma: allowlist secret
	u.password = snapMaturity{maturesAt: oldMatures, enrolledAt: snapLegacy} // pragma: allowlist secret
	m := snapLegacyMaturity()
	acct := snapBuild(t, db, u, m, m, m, m)

	r := snapOpenReq(decide.ReqBreakGlass, decide.CredRef{})
	r.originClass = decide.C1
	r.resetFactors = true
	r.fingerprint, r.coarse, r.fpKeyID = appBytes(1), appBytes(2), snapKeyID
	r.heldMs, r.heldSince = 5000, snapNow.Add(-time.Hour)
	snapInsertReq(t, db, acct.user.id, r)
	appSetPayload(t, db, r.id, appPendingHash, appPendingAddr)
	return acct, r
}

// appBreakGlassCtx is the context of the Complete: the outcome class differs from the row's
// origin class, and the open-only pending value is a decoy that must not be used.
func appBreakGlassCtx(acct snapAccount, r snapReq) (applyCtx, decide.Effect) {
	a := appCtx(acct.user.id, appReqFor(r))
	a.outcome = decide.Outcome{Row: decide.RowT11, Verdict: decide.VerdictApplied, EnrolClass: decide.C3, SetsPassword: true}
	a.pendingPasswordHash = appDecoyHash // pragma: allowlist secret
	return a, decide.Effect{Kind: decide.EffectComplete, RequestID: r.id.String()}
}

// appUserUpdates is the number of rows this transaction has updated in users.
func appUserUpdates(t *testing.T, tx *sql.Tx) int64 {
	t.Helper()
	var n int64
	require.NoError(t, tx.QueryRow(`SELECT n_tup_upd FROM pg_stat_xact_user_tables WHERE relname = 'users'`).Scan(&n))
	return n
}

// ---------------------------------------------------------------------------
// Open
// ---------------------------------------------------------------------------

func appOpenSetup(t *testing.T) (*sql.DB, snapAccount, []decide.CredRef) {
	t.Helper()
	db := snapDB(t)
	acct := snapBuildLegacy(t, db)
	// Canonical order: kind by number (password 1, webauthn 7).
	return db, acct, []decide.CredRef{acct.user.pwRef, acct.web}
}

func appCandidate() decide.Candidate {
	return decide.Candidate{Keyed: appBytes(3), Coarse: appBytes(4), KeyID: snapKeyID}
}

// appWantOpen is the row an Open of o must insert.
func appWantOpen(t *testing.T, userID uuid.UUID, o decide.Opening) appReqRow {
	t.Helper()
	fp, err := OriginFingerprint(o.Origins)
	require.NoError(t, err)
	row := appReqRow{
		UserID: userID.String(), Kind: snapSpell(t, o.Kind), Status: snapSpell(t, o.Status),
		ResetFactors: o.ResetFactors, OriginClass: int(o.OriginClass), OriginFP: fp[:],
		ReqFP: appNilIfEmpty(o.Candidate.Keyed), CoarseFP: appNilIfEmpty(o.Candidate.Coarse),
		OpenedAt: o.OpenedAt.UTC(), CompletesAt: o.CompletesAt.UTC(),
		DisputedAt: appTimePtr(o.DisputedAt), CompletedAt: appTimePtr(o.CompletedAt),
	}
	if len(o.Candidate.Keyed) > 0 {
		row.FPKeyID = appPtr(o.Candidate.KeyID)
	}
	if len(o.Targets) == 1 {
		row.TargetKind = appPtr(snapSpell(t, o.Targets[0].Kind))
		row.TargetID = appPtr(o.Targets[0].ID)
		row.TargetVersion = appPtr(o.Targets[0].Version)
	}
	return row
}

func appPendingOpening(kind decide.ReqKind, targets, origins []decide.CredRef) decide.Opening {
	return decide.Opening{
		Kind: kind, Status: decide.StatusPending, Targets: targets, Origins: origins,
		OriginClass: decide.C2, OpenedAt: snapNow, CompletesAt: snapNow.Add(72 * time.Hour),
		Candidate: appCandidate(),
	}
}

func TestApply_OpenWritesRequestAndOrigins(t *testing.T) {
	t.Run("credential_remove", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0] = id
		a.pendingPasswordHash = appPendingHash // pragma: allowlist secret (for a break-glass only; must not be stored here)

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}))

		assert.Equal(t, appWantOpen(t, acct.user.id, o), appReadReq(t, db, id))
		assert.Equal(t, snapSortedRefs(origins), appOrigins(t, db, id))
		assert.Equal(t, id, a.requestIDs[0], "Open leaves the id it was given in place")
	})

	t.Run("break_glass takes the pending password from applyCtx", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqBreakGlass, nil, origins)
		o.ResetFactors = true
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0] = id
		a.pendingPasswordHash = appPendingHash // pragma: allowlist secret

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}))

		want := appWantOpen(t, acct.user.id, o)
		want.PendingHash = appPtr(appPendingHash)
		assert.Equal(t, want, appReadReq(t, db, id))
		assert.Equal(t, snapSortedRefs(origins), appOrigins(t, db, id))
	})

	t.Run("disputed open with no group uses the outcome's new group", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		o.Status, o.DisputedAt = decide.StatusDisputed, snapNow
		id, group := uuid.New(), uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0], a.newGroupID = id, group

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}))

		want := appWantOpen(t, acct.user.id, o)
		want.DisputeGroup = appPtr(group.String())
		assert.Equal(t, want, appReadReq(t, db, id))
	})

	t.Run("disputed open with an explicit group keeps it", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		o.Status, o.DisputedAt = decide.StatusDisputed, snapNow
		id, existing := uuid.New(), uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0], a.newGroupID = id, uuid.New()

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o, Group: existing.String()}))

		want := appWantOpen(t, acct.user.id, o)
		want.DisputeGroup = appPtr(existing.String())
		assert.Equal(t, want, appReadReq(t, db, id))
	})

	t.Run("a pending open ignores Group", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0], a.newGroupID = id, uuid.New()

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o, Group: uuid.NewString()}))

		assert.Equal(t, appWantOpen(t, acct.user.id, o), appReadReq(t, db, id))
	})

	t.Run("a completed (T6c) open stores no fingerprint, key id or payload", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := decide.Opening{
			Kind: decide.ReqBreakGlass, Status: decide.StatusCompleted, Origins: origins, OriginClass: decide.C3,
			OpenedAt: snapNow, CompletesAt: snapNow, CompletedAt: snapNow,
		}
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0] = id
		a.pendingPasswordHash = appPendingHash // pragma: allowlist secret
		a.outcome.EnrolClass = decide.C3       // the completed row also writes the password (ruling 10)

		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}))

		got := appReadReq(t, db, id)
		assert.Equal(t, appWantOpen(t, acct.user.id, o), got)
		assert.Nil(t, got.FPKeyID)
		assert.Nil(t, got.PendingHash)
	})

	t.Run("an Open with no id from the caller is a contract error", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		err := appRun(t, db, appCtx(acct.user.id), decide.Effect{Kind: decide.EffectOpen, Open: o})
		assert.ErrorIs(t, err, ErrContract)
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE user_id = $1`, acct.user.id).Scan(&n))
		assert.Zero(t, n)
	})
}

func TestApply_OpenRefusesMultiTarget(t *testing.T) {
	db, acct, origins := appOpenSetup(t)
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	o := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp, acct.sso}, origins)
	a := appCtx(acct.user.id)
	a.requestIDs[0] = uuid.New()
	e := decide.Effect{Kind: decide.EffectOpen, Open: o}
	a.outcome.Effects = []decide.Effect{e}

	err = regApply(ctx, productionRegistry(), tx, a, 0, e)
	assert.ErrorIs(t, err, ErrContract)

	// Refused before any write: nothing visible inside the same transaction.
	var requests, originRows int
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE user_id = $1`, acct.user.id).Scan(&requests))
	require.NoError(t, tx.QueryRow(`SELECT count(*) FROM account_pending_request_origins`).Scan(&originRows))
	assert.Zero(t, requests)
	assert.Zero(t, originRows)
}

// ---------------------------------------------------------------------------
// Cancel, Dispute, ReturnToPending, Hold
// ---------------------------------------------------------------------------

func TestApply_CancelClearsPayloadKeepsFingerprintOnlyForProof(t *testing.T) {
	for _, tc := range []struct {
		reason decide.CancelReason
		class  decide.Class
		keep   bool
	}{
		{decide.CancelProof, decide.C2, true},
		{decide.CancelDisputeResolved, decide.C3, true},
		{decide.CancelSuperseded, decide.ClassNone, false},
		{decide.CancelHoldCap, decide.ClassNone, false},
		{decide.CancelDisputeExpired, decide.ClassNone, false},
	} {
		t.Run(snapSpell(t, tc.reason), func(t *testing.T) {
			db := snapDB(t)
			acct := appFactorAccount(t, db)
			r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, true)

			a := appCtx(acct.user.id, appReqFor(r))
			require.NoError(t, appRun(t, db, a, decide.Effect{
				Kind: decide.EffectCancel, RequestID: r.id.String(), Reason: tc.reason, CancellerClass: tc.class,
			}))

			want := appRichRow(t, acct.user.id, r)
			want.Status = "cancelled"
			want.CancelledAt = appTimePtr(snapNow)
			want.CancelReason = appPtr(snapSpell(t, tc.reason))
			if tc.class != decide.ClassNone {
				want.CancellerClass = appPtr(int(tc.class))
			}
			want.PendingHash, want.PendingAddress, want.HeldSince = nil, nil, nil
			if !tc.keep {
				want.ReqFP, want.CoarseFP, want.FPKeyID = nil, nil, nil
			}
			assert.Equal(t, want, appReadReq(t, db, r.id))
		})
	}
}

func TestApply_DisputeOnDisputedWritesGroupOnly(t *testing.T) {
	t.Run("already disputed: only dispute_group_id changes", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := snapDisputedReq(acct.totp)
		r.fingerprint, r.coarse, r.fpKeyID, r.heldMs = appBytes(1), appBytes(2), snapKeyID, 700
		snapInsertReq(t, db, acct.user.id, r)
		merged := uuid.New()

		a := appCtx(acct.user.id, appReqFor(r))
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: r.id.String(), Group: merged.String()}))

		want := appRowOf(t, acct.user.id, r)
		want.DisputeGroup = appPtr(merged.String())
		got := appReadReq(t, db, r.id)
		assert.Equal(t, want, got)
		assert.Equal(t, "disputed", got.Status)
		assert.Equal(t, appTimePtr(snapNow.Add(-2*time.Hour)), got.DisputedAt, "disputed_at is not rewritten")
	})

	t.Run("empty group uses the outcome's new group", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := snapDisputedReq(acct.totp)
		snapInsertReq(t, db, acct.user.id, r)
		fresh := uuid.New()

		a := appCtx(acct.user.id, appReqFor(r))
		a.newGroupID = fresh
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: r.id.String()}))

		want := appRowOf(t, acct.user.id, r)
		want.DisputeGroup = appPtr(fresh.String())
		assert.Equal(t, want, appReadReq(t, db, r.id))
	})

	t.Run("a pending request becomes disputed now", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
		r.fingerprint, r.coarse, r.fpKeyID = appBytes(1), appBytes(2), snapKeyID
		snapInsertReq(t, db, acct.user.id, r)
		group := uuid.New()

		a := appCtx(acct.user.id, appReqFor(r))
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: r.id.String(), Group: group.String()}))

		want := appRowOf(t, acct.user.id, r)
		want.Status, want.DisputedAt, want.DisputeGroup = "disputed", appTimePtr(snapNow), appPtr(group.String())
		assert.Equal(t, want, appReadReq(t, db, r.id))
	})
}

func TestApply_ReturnToPendingMovesADisputedRowBack(t *testing.T) {
	db := snapDB(t)
	acct := appFactorAccount(t, db)
	r := snapDisputedReq(acct.totp)
	r.fingerprint, r.coarse, r.fpKeyID, r.heldMs = appBytes(1), appBytes(2), snapKeyID, 700
	snapInsertReq(t, db, acct.user.id, r)

	a := appCtx(acct.user.id, appReqFor(r))
	require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectReturnToPending, RequestID: r.id.String()}))

	got := appReadReq(t, db, r.id)
	// Ruling 12: disputed_at and dispute_group_id are cleared on the way back.
	want := appRowOf(t, acct.user.id, r)
	want.Status = "pending"
	want.DisputedAt, want.DisputeGroup = nil, nil
	assert.Equal(t, want, got)
}

func TestApply_HoldOpensAndFoldsAHold(t *testing.T) {
	t.Run("a non-zero HeldSince opens a hold", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
		r.heldMs = 1000
		snapInsertReq(t, db, acct.user.id, r)
		since := snapNow.Add(-90 * time.Minute)

		a := appCtx(acct.user.id, appReqFor(r))
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectHold, RequestID: r.id.String(), HeldMs: 4000, HeldSince: since}))

		want := appRowOf(t, acct.user.id, r)
		want.HeldMs, want.HeldSince = 4000, appTimePtr(since)
		assert.Equal(t, want, appReadReq(t, db, r.id))
	})

	t.Run("a zero HeldSince folds the hold", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
		r.heldMs, r.heldSince = 4000, snapNow.Add(-time.Hour)
		snapInsertReq(t, db, acct.user.id, r)

		a := appCtx(acct.user.id, appReqFor(r))
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectHold, RequestID: r.id.String(), HeldMs: 9000}))

		want := appRowOf(t, acct.user.id, r)
		want.HeldMs, want.HeldSince = 9000, nil
		assert.Equal(t, want, appReadReq(t, db, r.id))
	})
}

// A transition guarded on status writes nothing and reports ErrStale for a terminal row.
func TestApply_GuardedTransitionsRefuseTerminalRows(t *testing.T) {
	effects := map[string]func(id uuid.UUID) decide.Effect{
		"cancel": func(id uuid.UUID) decide.Effect {
			return decide.Effect{Kind: decide.EffectCancel, RequestID: id.String(), Reason: decide.CancelSuperseded}
		},
		"dispute": func(id uuid.UUID) decide.Effect {
			return decide.Effect{Kind: decide.EffectDispute, RequestID: id.String(), Group: uuid.NewString()}
		},
		"return_to_pending": func(id uuid.UUID) decide.Effect {
			return decide.Effect{Kind: decide.EffectReturnToPending, RequestID: id.String()}
		},
		"hold": func(id uuid.UUID) decide.Effect {
			return decide.Effect{Kind: decide.EffectHold, RequestID: id.String(), HeldMs: 10}
		},
		"complete": func(id uuid.UUID) decide.Effect {
			return decide.Effect{Kind: decide.EffectComplete, RequestID: id.String()}
		},
	}
	for name, mk := range effects {
		for _, terminal := range []string{"completed", "cancelled"} {
			t.Run(name+" on a "+terminal+" row", func(t *testing.T) {
				db := snapDB(t)
				acct := appFactorAccount(t, db)
				r := snapCompletedReq(acct.totp)
				if terminal == "cancelled" {
					r = snapCancelledReq(decide.CancelSuperseded, snapNow.Add(-time.Hour), acct.totp)
				}
				snapInsertReq(t, db, acct.user.id, r)
				before := appReadReq(t, db, r.id)

				a := appCtx(acct.user.id, decide.Req{ID: r.id.String(), Kind: decide.ReqCredentialRemove, Status: r.status})
				err := appRun(t, db, a, mk(r.id))

				assert.ErrorIs(t, err, ErrStale)
				assert.Equal(t, before, appReadReq(t, db, r.id))
			})
		}
	}
}

// ---------------------------------------------------------------------------
// Complete
// ---------------------------------------------------------------------------

func TestApply_CompleteCredentialRemoveMarksCompletedAndLeavesTheCredential(t *testing.T) {
	db := snapDB(t)
	acct := appFactorAccount(t, db)
	r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, true)

	a := appCtx(acct.user.id, appReqFor(r))
	require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectComplete, RequestID: r.id.String()}))

	want := appRichRow(t, acct.user.id, r)
	want.Status, want.CompletedAt = "completed", appTimePtr(snapNow)
	want.PendingHash, want.PendingAddress, want.HeldSince = nil, nil, nil
	want.ReqFP, want.CoarseFP, want.FPKeyID = nil, nil, nil
	assert.Equal(t, want, appReadReq(t, db, r.id))

	// Ruling 1: the removal arrives as its own EffectRemove; Complete removes nothing.
	assert.Equal(t, 4, appFactorCount(t, db, acct.user.id))
	_, present := appFactor(t, db, acct.totp)
	assert.True(t, present, "the request's target is still there")
}

func TestApply_BreakGlassWritesPasswordClassAndHistory(t *testing.T) {
	for _, tc := range []struct {
		name       string
		oldMatures time.Time
		wasMature  bool
	}{
		{"old password was mature", snapLegacy, true},
		{"old password was still immature", snapNow.Add(24 * time.Hour), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := snapDB(t)
			acct, r := appBreakGlass(t, db, tc.oldMatures)
			before := appReadUserPw(t, db, acct.user.id)
			require.Equal(t, appOldHash, before.Hash)
			a, complete := appBreakGlassCtx(acct, r)

			// F2: the hash, enrolled_at and enrol_class land in ONE users UPDATE.
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			updatesBefore := appUserUpdates(t, tx)
			a.outcome.Effects = []decide.Effect{complete}
			require.NoError(t, regApply(ctx, productionRegistry(), tx, a, 0, complete))
			assert.EqualValues(t, 1, appUserUpdates(t, tx)-updatesBefore, "exactly one UPDATE of users")
			require.NoError(t, tx.Commit())

			after := appReadUserPw(t, db, acct.user.id)
			assert.Equal(t, appPendingHash, after.Hash, "the row's pending value, not applyCtx's")
			assert.Equal(t, snapNow, after.EnrolledAt)
			assert.Equal(t, appPtr(int(decide.C3)), after.EnrolClass, "Outcome.EnrolClass, not the request's origin class")
			assert.NotEqual(t, before.Version, after.Version, "a changed hash moves the material version")

			assert.Equal(t, []appHistRow{{
				Kind: "password", Hash: appPtr(appOldHash), RetiredAt: snapNow, WasMature: tc.wasMature,
			}}, appReadHistory(t, db, acct.user.id))

			want := appRowOf(t, acct.user.id, r)
			want.Status, want.CompletedAt = "completed", appTimePtr(snapNow)
			want.PendingHash, want.PendingAddress, want.HeldSince = nil, nil, nil
			want.ReqFP, want.CoarseFP, want.FPKeyID = nil, nil, nil
			assert.Equal(t, want, appReadReq(t, db, r.id))

			// Ruling 1: reset_factors is true here and the factors are still there; their
			// removals arrive as their own effects.
			assert.Equal(t, 4, appFactorCount(t, db, acct.user.id))
		})
	}
}

func TestApply_HistoryTrimmedTo32(t *testing.T) {
	require.Equal(t, 32, decide.HistoryMaxPasswords)
	for _, seeded := range []int{3, 31, 32, 36} {
		t.Run(fmt.Sprintf("%d retired passwords already stored", seeded), func(t *testing.T) {
			db := snapDB(t)
			acct, r := appBreakGlass(t, db, snapLegacy)
			other := newSnapUser()
			other.insert(t, db)

			appSeedPasswordHistory(t, db, acct.user.id, seeded)
			appSeedAddressHistory(t, db, acct.user.id, 40) // another kind: never trimmed
			appSeedPasswordHistory(t, db, other.id, 40)    // another account: never trimmed
			a, complete := appBreakGlassCtx(acct, r)

			require.NoError(t, appRun(t, db, a, complete))

			// The 32 latest of seeded+1 retired passwords survive: the new one is the latest.
			start := max(0, seeded+1-decide.HistoryMaxPasswords)
			var want []string
			for i := start; i < seeded; i++ {
				want = append(want, fmt.Sprintf("h-%d", i))
			}
			want = append(want, appOldHash)
			got := appPasswordHistory(t, db, acct.user.id)
			assert.Equal(t, want, got)
			assert.LessOrEqual(t, len(got), decide.HistoryMaxPasswords)

			var addresses, otherPasswords int
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_credential_history WHERE user_id = $1 AND kind = 'address'`, acct.user.id).Scan(&addresses))
			require.NoError(t, db.QueryRow(`SELECT count(*) FROM user_credential_history WHERE user_id = $1`, other.id).Scan(&otherPasswords))
			assert.Equal(t, 40, addresses)
			assert.Equal(t, 40, otherPasswords)
		})
	}
}

// ---------------------------------------------------------------------------
// ReleaseReservation, Remove, Contest
// ---------------------------------------------------------------------------

func appReservation(t *testing.T, db *sql.DB, userID uuid.UUID, address string) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO account_address_reservations (address, user_id, reason, displaced_at, floor_until)
		VALUES ($1, $2, 'displaced_primary', $3, $4)`, address, userID, snapNow.Add(-48*time.Hour), snapNow.Add(-time.Hour))
	require.NoError(t, err)
}

func appReservationCount(t *testing.T, db *sql.DB, address string) int {
	t.Helper()
	var n int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_address_reservations WHERE address = $1`, address).Scan(&n))
	return n
}

func TestApply_ReleaseReservationScopedToTheUser(t *testing.T) {
	db := snapDB(t)
	owner, intruder := newSnapUser(), newSnapUser()
	owner.insert(t, db)
	intruder.insert(t, db)
	mine, theirs := "mine-"+owner.id.String()[:8]+"@example.test", "theirs-"+intruder.id.String()[:8]+"@example.test"
	appReservation(t, db, owner.id, mine)
	appReservation(t, db, intruder.id, theirs)
	release := func(address string) decide.Effect {
		return decide.Effect{Kind: decide.EffectReleaseReservation, Cred: decide.CredRef{Kind: decide.CredAddress, ID: address}}
	}

	t.Run("another account's reservation is not released", func(t *testing.T) {
		err := appRun(t, db, appCtx(owner.id), release(theirs))
		assert.ErrorIs(t, err, ErrStale)
		assert.Equal(t, 1, appReservationCount(t, db, theirs))
	})
	t.Run("the account's own reservation is released and only it", func(t *testing.T) {
		require.NoError(t, appRun(t, db, appCtx(owner.id), release(mine)))
		assert.Zero(t, appReservationCount(t, db, mine))
		assert.Equal(t, 1, appReservationCount(t, db, theirs))
	})
	t.Run("releasing it again finds nothing", func(t *testing.T) {
		assert.ErrorIs(t, appRun(t, db, appCtx(owner.id), release(mine)), ErrStale)
	})
}

func TestApply_RemoveVersionFenced(t *testing.T) {
	for _, kind := range appFactorKinds {
		t.Run(snapSpell(t, kind), func(t *testing.T) {
			db := snapDB(t)
			acct := appFactorAccount(t, db)
			other := appFactorAccount(t, db)
			ref := appRefOf(acct, kind)
			a := appCtx(acct.user.id)
			remove := func(r decide.CredRef) decide.Effect { return decide.Effect{Kind: decide.EffectRemove, Cred: r} }

			for _, delta := range []int64{1, -1} {
				stale := ref
				stale.Version += delta
				err := appRun(t, db, a, remove(stale))
				assert.ErrorIs(t, err, ErrStale, "version %+d", delta)
				_, present := appFactor(t, db, ref)
				assert.True(t, present, "a wrong version deletes nothing (version %+d)", delta)
			}

			// A credential of another account is never removed through this account's context.
			err := appRun(t, db, a, remove(appRefOf(other, kind)))
			assert.ErrorIs(t, err, ErrStale)
			_, otherPresent := appFactor(t, db, appRefOf(other, kind))
			assert.True(t, otherPresent)

			require.NoError(t, appRun(t, db, a, remove(ref)))
			_, present := appFactor(t, db, ref)
			assert.False(t, present)
			assert.Equal(t, 3, appFactorCount(t, db, acct.user.id), "only that row went")
			assert.Equal(t, 4, appFactorCount(t, db, other.user.id))
		})
	}
}

func TestApply_ContestIdempotent(t *testing.T) {
	for _, kind := range appFactorKinds {
		t.Run(snapSpell(t, kind), func(t *testing.T) {
			db := snapDB(t)
			acct := appFactorAccount(t, db)
			ref := appRefOf(acct, kind)
			before, ok := appFactor(t, db, ref)
			require.True(t, ok)
			require.Nil(t, before.ContestedAt)
			contest := decide.Effect{Kind: decide.EffectContest, Cred: ref}

			a := appCtx(acct.user.id)

			// Ruling 12: the same version fence as Remove. A wrong version contests nothing.
			for _, delta := range []int64{1, -1} {
				stale := ref
				stale.Version += delta
				err := appRun(t, db, a, decide.Effect{Kind: decide.EffectContest, Cred: stale})
				assert.ErrorIs(t, err, ErrStale, "version %+d", delta)
				assert.Equal(t, before, mustFactor(t, db, ref), "a wrong version changes nothing (version %+d)", delta)
			}

			require.NoError(t, appRun(t, db, a, contest))

			want := before
			want.ContestedAt = appTimePtr(snapNow)
			assert.Equal(t, want, mustFactor(t, db, ref), "only maturity_contested_at changes")

			// A second contest, later, is not an error and does not move the first stamp.
			later := a
			later.now = snapNow.Add(time.Hour)
			require.NoError(t, appRun(t, db, later, contest))
			assert.Equal(t, want, mustFactor(t, db, ref))

			// The account's other factors are not contested.
			for _, k := range appFactorKinds {
				if k == kind {
					continue
				}
				s, present := appFactor(t, db, appRefOf(acct, k))
				require.True(t, present)
				assert.Nil(t, s.ContestedAt, snapSpell(t, k))
			}

			// A credential that is gone cannot be contested.
			require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectRemove, Cred: ref}))
			assert.ErrorIs(t, appRun(t, db, a, contest), ErrStale)
		})
	}
}

func mustFactor(t *testing.T, db *sql.DB, ref decide.CredRef) appFactorState {
	t.Helper()
	s, ok := appFactor(t, db, ref)
	require.True(t, ok)
	return s
}

// ---------------------------------------------------------------------------
// The password writer (rulings 10 and 12)
// ---------------------------------------------------------------------------

// appT6cOpening is a break-glass Opening written already completed (T6c).
func appT6cOpening(origins []decide.CredRef) decide.Opening {
	return decide.Opening{
		Kind: decide.ReqBreakGlass, Status: decide.StatusCompleted, Origins: origins, OriginClass: decide.C3,
		OpenedAt: snapNow, CompletesAt: snapNow, CompletedAt: snapNow,
	}
}

// appT6cAccount is an account whose stored password is appOldHash, mature.
func appT6cAccount(t *testing.T, db *sql.DB) snapAccount {
	t.Helper()
	u := newSnapUser()
	u.passwordHash = appOldHash // pragma: allowlist secret
	m := snapLegacyMaturity()
	return snapBuild(t, db, u, m, m, m, m)
}

func TestApply_CompletedBreakGlassOpenSetsPassword(t *testing.T) {
	t.Run("writes the completed row, the password, its class and the history", func(t *testing.T) {
		db := snapDB(t)
		acct := appT6cAccount(t, db)
		o := appT6cOpening([]decide.CredRef{acct.user.pwRef, acct.web})
		before := appReadUserPw(t, db, acct.user.id)
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0] = id
		a.pendingPasswordHash = appPendingHash // pragma: allowlist secret
		a.outcome = decide.Outcome{Row: decide.RowT6c, Verdict: decide.VerdictApplied, EnrolClass: decide.C3, SetsPassword: true}
		e := decide.Effect{Kind: decide.EffectOpen, Open: o}
		a.outcome.Effects = []decide.Effect{e}

		// F2 again: one users UPDATE carries the hash, the time and the class.
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		updatesBefore := appUserUpdates(t, tx)
		require.NoError(t, regApply(ctx, productionRegistry(), tx, a, 0, e))
		assert.EqualValues(t, 1, appUserUpdates(t, tx)-updatesBefore, "exactly one UPDATE of users")
		require.NoError(t, tx.Commit())

		assert.Equal(t, appWantOpen(t, acct.user.id, o), appReadReq(t, db, id), "completed, no fingerprint, NULL pending value")
		after := appReadUserPw(t, db, acct.user.id)
		assert.Equal(t, appPendingHash, after.Hash)
		assert.Equal(t, snapNow, after.EnrolledAt)
		assert.Equal(t, appPtr(int(decide.C3)), after.EnrolClass)
		assert.NotEqual(t, before.Version, after.Version)
		assert.Equal(t, []appHistRow{{
			Kind: "password", Hash: appPtr(appOldHash), RetiredAt: snapNow, WasMature: true,
		}}, appReadHistory(t, db, acct.user.id))
		assert.NoError(t, productionRegistry().finish(a), "one password write for SetsPassword")
	})

	t.Run("an empty pending password is a contract error and writes nothing", func(t *testing.T) {
		db := snapDB(t)
		acct := appT6cAccount(t, db)
		o := appT6cOpening([]decide.CredRef{acct.user.pwRef, acct.web})
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0] = id

		err := appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o})

		assert.ErrorIs(t, err, ErrContract)
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE user_id = $1`, acct.user.id).Scan(&n))
		assert.Zero(t, n)
		assert.Equal(t, appOldHash, appReadUserPw(t, db, acct.user.id).Hash)
		assert.Empty(t, appReadHistory(t, db, acct.user.id))
	})
}

func TestApply_SetsPasswordMismatchIsContract(t *testing.T) {
	reg := productionRegistry()

	t.Run("SetsPassword with no password write", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, false)
		a := appCtx(acct.user.id, appReqFor(r))
		a.outcome.SetsPassword = true // pragma: allowlist secret
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectComplete, RequestID: r.id.String()}))
		assert.ErrorIs(t, reg.finish(a), ErrContract)
	})

	t.Run("a password write with SetsPassword false", func(t *testing.T) {
		db := snapDB(t)
		acct, r := appBreakGlass(t, db, snapLegacy)
		a, complete := appBreakGlassCtx(acct, r)
		a.outcome.SetsPassword = false // pragma: allowlist secret
		require.NoError(t, appRun(t, db, a, complete))
		assert.ErrorIs(t, reg.finish(a), ErrContract)
	})

	t.Run("matching: one write with SetsPassword", func(t *testing.T) {
		db := snapDB(t)
		acct, r := appBreakGlass(t, db, snapLegacy)
		a, complete := appBreakGlassCtx(acct, r)
		require.NoError(t, appRun(t, db, a, complete))
		assert.NoError(t, reg.finish(a))
	})

	t.Run("matching: no write without SetsPassword", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, false)
		a := appCtx(acct.user.id, appReqFor(r))
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectComplete, RequestID: r.id.String()}))
		assert.NoError(t, reg.finish(a))
	})

	t.Run("two password writes are one too many", func(t *testing.T) {
		db := snapDB(t)
		acct, r := appBreakGlass(t, db, snapLegacy)
		a, complete := appBreakGlassCtx(acct, r)
		require.NoError(t, appRun(t, db, a, complete))
		a.requestIDs[0] = uuid.New()
		a.pendingPasswordHash = appPendingHash // pragma: allowlist secret
		o := appT6cOpening([]decide.CredRef{acct.user.pwRef, acct.web})
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}))
		assert.ErrorIs(t, reg.finish(a), ErrContract)
	})
}

// ---------------------------------------------------------------------------
// The terminal CHECKs
// ---------------------------------------------------------------------------

// TestApply_TerminalCheckHoldsAfterEveryTransition drives every transition from a request
// that carries everything the account_pending_requests CHECKs could object to (an open
// hold, both fingerprints with their key id, both payloads), and requires that no statement
// trips one. Outcomes are applied in applyOrder, in the shape decide emits them: a Cancel,
// Dispute or Complete sorts ahead of the fold of the hold it ends (ruling 16).
func TestApply_TerminalCheckHoldsAfterEveryTransition(t *testing.T) {
	step := func(t *testing.T, db *sql.DB, a applyCtx, effects ...decide.Effect) {
		t.Helper()
		a.outcome.Effects = effects
		require.NoError(t, appApplyWith(t, db, productionRegistry(), a), "effects %v", effects)
	}
	fold := func(id string, ms int64) decide.Effect {
		return decide.Effect{Kind: decide.EffectHold, RequestID: id, HeldMs: ms}
	}

	t.Run("hold, dispute with fold, dispute again, return, cancel with fold", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, false)
		id := r.id.String()
		a := appCtx(acct.user.id, appReqFor(r))
		a.newGroupID = uuid.New()

		step(t, db, a, decide.Effect{Kind: decide.EffectHold, RequestID: id, HeldMs: 6000, HeldSince: snapNow.Add(-time.Minute)})
		step(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: id}, fold(id, 66000))
		step(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: id, Group: uuid.NewString()})
		step(t, db, a, decide.Effect{Kind: decide.EffectReturnToPending, RequestID: id})
		step(t, db, a, decide.Effect{Kind: decide.EffectCancel, RequestID: id, Reason: decide.CancelProof, CancellerClass: decide.C2}, fold(id, 70000))
	})

	for _, reason := range []decide.CancelReason{
		decide.CancelProof, decide.CancelDisputeResolved, decide.CancelSuperseded, decide.CancelHoldCap, decide.CancelDisputeExpired,
	} {
		class := decide.ClassNone
		if reason == decide.CancelProof || reason == decide.CancelDisputeResolved {
			class = decide.C2
		}
		t.Run("cancel "+snapSpell(t, reason)+" from pending with a hold", func(t *testing.T) {
			db := snapDB(t)
			acct := appFactorAccount(t, db)
			r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, true)
			id := r.id.String()
			step(t, db, appCtx(acct.user.id, appReqFor(r)),
				decide.Effect{Kind: decide.EffectCancel, RequestID: id, Reason: reason, CancellerClass: class}, fold(id, 3605000))
		})
		t.Run("cancel "+snapSpell(t, reason)+" from disputed", func(t *testing.T) {
			db := snapDB(t)
			acct := appFactorAccount(t, db)
			r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, true)
			id := r.id.String()
			a := appCtx(acct.user.id, appReqFor(r))
			a.newGroupID = uuid.New()
			step(t, db, a, decide.Effect{Kind: decide.EffectDispute, RequestID: id}, fold(id, 3605000))
			step(t, db, a, decide.Effect{Kind: decide.EffectCancel, RequestID: id, Reason: reason, CancellerClass: class})
		})
	}

	t.Run("complete credential_remove from pending with a hold", func(t *testing.T) {
		db := snapDB(t)
		acct := appFactorAccount(t, db)
		r := appRich(t, db, acct.user.id, decide.ReqCredentialRemove, acct.totp, true)
		id := r.id.String()
		step(t, db, appCtx(acct.user.id, appReqFor(r)), decide.Effect{Kind: decide.EffectComplete, RequestID: id}, fold(id, 3605000))
	})

	t.Run("complete break_glass from pending with a hold", func(t *testing.T) {
		db := snapDB(t)
		acct, r := appBreakGlass(t, db, snapLegacy)
		a, complete := appBreakGlassCtx(acct, r)
		step(t, db, a, complete, fold(r.id.String(), 3605000))
	})

	t.Run("open pending, disputed and completed rows", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		pending := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
		disputed := pending
		disputed.Status, disputed.DisputedAt = decide.StatusDisputed, snapNow
		completed := decide.Opening{
			Kind: decide.ReqBreakGlass, Status: decide.StatusCompleted, Origins: origins, OriginClass: decide.C3,
			OpenedAt: snapNow, CompletesAt: snapNow, CompletedAt: snapNow,
		}
		for _, o := range []decide.Opening{pending, disputed, completed} {
			a := appCtx(acct.user.id)
			a.requestIDs[0], a.newGroupID, a.pendingPasswordHash = uuid.New(), uuid.New(), appPendingHash
			a.outcome.EnrolClass = decide.C3
			step(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o})
		}
	})
}

// ---------------------------------------------------------------------------
// Producer hardening (Task 9): two guards apply.go adds beyond the brief
// ---------------------------------------------------------------------------

// TestApply_DriverErrorsCarryNoRowContent: a constraint failure on a row that carries a
// pending password comes back as its SQLSTATE and constraint only. The control runs the same
// failure raw and shows the driver's DETAIL quoting the hash, so the assertion is not vacuous.
func TestApply_DriverErrorsCarryNoRowContent(t *testing.T) {
	db := snapDB(t)
	acct := appFactorAccount(t, db)
	r := appRich(t, db, acct.user.id, decide.ReqBreakGlass, decide.CredRef{}, false)

	var pqErr *pq.Error
	_, rawErr := db.Exec(`UPDATE account_pending_requests SET held_ms = -1 WHERE id = $1`, r.id)
	require.ErrorAs(t, rawErr, &pqErr, "control: the raw statement fails in the driver")
	require.Contains(t, pqErr.Detail, appPendingHash, "control: the driver's DETAIL quotes the row")

	err := appRun(t, db, appCtx(acct.user.id, appReqFor(r)),
		decide.Effect{Kind: decide.EffectHold, RequestID: r.id.String(), HeldMs: -1})
	require.Error(t, err)
	assert.False(t, errors.As(err, &pqErr), "the driver error must not be reachable through the chain")
	var fault *dbFault
	require.ErrorAs(t, err, &fault)
	assert.Equal(t, pqerror.CheckViolation, fault.code)
	assert.NotContains(t, fmt.Sprintf("%+v", err), appPendingHash)
}

// TestApply_SetPasswordRefusesALostEnrolClass: the 000189 trigger resets the class to NULL
// when a write's enrolled_at equals the stored one. setPasswordTx reads the class back and
// refuses, rather than leave the new password at the rank floor (red-team F2).
func TestApply_SetPasswordRefusesALostEnrolClass(t *testing.T) {
	db := snapDB(t)
	acct := appT6cAccount(t, db)
	a := appCtx(acct.user.id)
	a.outcome.EnrolClass = decide.C3
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })

	noClass := a
	noClass.outcome.EnrolClass = decide.ClassNone
	assert.ErrorIs(t, setPasswordTx(ctx, tx, noClass, appPendingHash), ErrContract, "no class is refused, never written as NULL")
	require.NoError(t, setPasswordTx(ctx, tx, a, appPendingHash), "control: a first write keeps its class")
	assert.ErrorIs(t, setPasswordTx(ctx, tx, a, appDecoyHash), ErrContract,
		"a second write at the same enrolled_at loses its class to the trigger")
	assert.Equal(t, 1, *a.passwordWrites, "a refused write is not counted")
}

// TestApply_CompleteRefusesARequestOfAnotherKind: a Complete is keyed by the kind reqs names
// and its UPDATE is guarded on that kind, so reqs that misname a break-glass as a
// credential_remove complete nothing and write no password. The control completes the same
// row under its own kind.
func TestApply_CompleteRefusesARequestOfAnotherKind(t *testing.T) {
	db := snapDB(t)
	acct, r := appBreakGlass(t, db, snapLegacy)
	before := appReadReq(t, db, r.id)
	complete := decide.Effect{Kind: decide.EffectComplete, RequestID: r.id.String()}

	misnamed := appCtx(acct.user.id, decide.Req{ID: r.id.String(), Kind: decide.ReqCredentialRemove, Status: decide.StatusPending})
	assert.ErrorIs(t, appRun(t, db, misnamed, complete), ErrStale)
	assert.Equal(t, before, appReadReq(t, db, r.id))
	assert.Equal(t, appOldHash, appReadUserPw(t, db, acct.user.id).Hash)

	a, _ := appBreakGlassCtx(acct, r)
	require.NoError(t, appRun(t, db, a, complete), "control: the row's own kind completes it")
}

// TestApply_NonCanonicalIDsAreContractErrors: an id that is not canonical uuid text names no
// row the snapshot read, and is refused before any write.
func TestApply_NonCanonicalIDsAreContractErrors(t *testing.T) {
	db := snapDB(t)
	acct := appFactorAccount(t, db)
	r := snapOpenReq(decide.ReqCredentialRemove, acct.totp)
	snapInsertReq(t, db, acct.user.id, r)
	before := appReadReq(t, db, r.id)

	braced := "{" + r.id.String() + "}"
	a := appCtx(acct.user.id, decide.Req{ID: braced, Kind: decide.ReqCredentialRemove, Status: decide.StatusPending})
	assert.ErrorIs(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectHold, RequestID: braced, HeldMs: 10}), ErrContract)
	assert.Equal(t, before, appReadReq(t, db, r.id))

	web := acct.web
	web.ID = "{" + web.ID + "}"
	assert.ErrorIs(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectRemove, Cred: web}), ErrContract)
	_, present := appFactor(t, db, acct.web)
	assert.True(t, present)

	require.NoError(t, appRun(t, db, appCtx(acct.user.id), decide.Effect{Kind: decide.EffectRemove, Cred: acct.web}),
		"control: the canonical id removes it")
}

// TestApply_ContractRefusalsWriteNothing: each input the Open applier, or the dispatcher in
// front of it, refuses before any write. The control writes the unedited opening.
func TestApply_ContractRefusalsWriteNothing(t *testing.T) {
	db, acct, origins := appOpenSetup(t)
	base := appPendingOpening(decide.ReqCredentialRemove, []decide.CredRef{acct.totp}, origins)
	countByID := func(t *testing.T, id uuid.UUID) int {
		t.Helper()
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE id = $1`, id).Scan(&n))
		return n
	}
	for _, tc := range []struct {
		name string
		edit func(a *applyCtx, o *decide.Opening)
	}{
		{"zero completes_at", func(_ *applyCtx, o *decide.Opening) { o.CompletesAt = time.Time{} }},
		{"zero opened_at", func(_ *applyCtx, o *decide.Opening) { o.OpenedAt = time.Time{} }},
		{"no origin class", func(_ *applyCtx, o *decide.Opening) { o.OriginClass = decide.ClassNone }},
		{"no origins", func(_ *applyCtx, o *decide.Opening) { o.Origins = nil }},
		{"a credential_remove opened completed", func(_ *applyCtx, o *decide.Opening) {
			o.Status, o.CompletedAt, o.Candidate = decide.StatusCompleted, snapNow, decide.Candidate{}
		}},
		{"a pending break-glass with no pending password", func(_ *applyCtx, o *decide.Opening) {
			o.Kind, o.Targets = decide.ReqBreakGlass, nil
		}},
		{"a disputed open with no group", func(_ *applyCtx, o *decide.Opening) {
			o.Status, o.DisputedAt = decide.StatusDisputed, snapNow
		}},
		{"no clock", func(a *applyCtx, _ *decide.Opening) { a.now = time.Time{} }},
		{"no account", func(a *applyCtx, _ *decide.Opening) { a.userID = uuid.Nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := appCtx(acct.user.id)
			id := uuid.New()
			a.requestIDs[0] = id
			o := base
			tc.edit(&a, &o)
			assert.ErrorIs(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: o}), ErrContract)
			assert.Zero(t, countByID(t, id))
		})
	}
	t.Run("control: the unedited opening is written", func(t *testing.T) {
		a := appCtx(acct.user.id)
		id := uuid.New()
		a.requestIDs[0] = id
		require.NoError(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: base}))
		assert.Equal(t, 1, countByID(t, id))
	})
}

// ---------------------------------------------------------------------------
// Apply order (ruling 16), execRows (ruling 17), argon2id (ruling 20)
// ---------------------------------------------------------------------------

// appApplyWith applies a.outcome through reg in applyOrder, in one transaction: committed
// when every effect applies, rolled back at the first error.
func appApplyWith(t *testing.T, db *sql.DB, reg registry, a applyCtx) error {
	t.Helper()
	if a.requestIDs == nil {
		a.requestIDs = map[int]uuid.UUID{}
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	for _, i := range applyOrder(a.outcome) {
		if err := reg.apply(ctx, tx, a, i); err != nil {
			require.NoError(t, tx.Rollback())
			return err
		}
	}
	require.NoError(t, tx.Commit())
	return nil
}

func appKinds(effects []decide.Effect) []decide.EffectKind {
	out := make([]decide.EffectKind, len(effects))
	for i, e := range effects {
		out[i] = e.Kind
	}
	return out
}

// TestApplyOrder_HoistsOnlyFoldsOfExistingRequests: a fold is an EffectHold with a zero
// HeldSince and a non-empty RequestID. Only those move, ahead of everything else, in their
// own relative order; every other effect keeps its Outcome position.
func TestApplyOrder_HoistsOnlyFoldsOfExistingRequests(t *testing.T) {
	cancel := decide.Effect{Kind: decide.EffectCancel, RequestID: "r1", Reason: decide.CancelHoldCap}
	foldOf := func(id string) decide.Effect { return decide.Effect{Kind: decide.EffectHold, RequestID: id, HeldMs: 7} }
	for _, tc := range []struct {
		name    string
		effects []decide.Effect
		want    []int
	}{
		{"no effects", nil, []int{}},
		{"no hold: identity", []decide.Effect{cancel, {Kind: decide.EffectDispute, RequestID: "r2"}}, []int{0, 1}},
		{"a fold after a cancel is hoisted", []decide.Effect{cancel, foldOf("r1")}, []int{1, 0}},
		{"several folds keep their relative order", []decide.Effect{cancel, foldOf("r2"), {Kind: decide.EffectDispute, RequestID: "r3"}, foldOf("r3")}, []int{1, 3, 0, 2}},
		{"a fold with no RequestID is not hoisted", []decide.Effect{cancel, foldOf("")}, []int{0, 1}},
		{"an opening hold is not hoisted", []decide.Effect{cancel, {Kind: decide.EffectHold, RequestID: "r1", HeldMs: 7, HeldSince: snapNow}}, []int{0, 1}},
		{"mixed: only the existing-request fold moves", []decide.Effect{
			cancel, foldOf(""), {Kind: decide.EffectHold, RequestID: "r1", HeldSince: snapNow}, foldOf("r2"),
		}, []int{3, 0, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, applyOrder(decide.Outcome{Effects: tc.effects}))
		})
	}
}

// appDecideInput is the Input decide.Decide completes the request over, on the account's own
// credentials (all legacy, so mature).
func appDecideInput(acct snapAccount, req decide.Req) decide.Input {
	in := acct.wantInput()
	in.Act = decide.Complete{ID: req.ID}
	in.Reqs = []decide.Req{req}
	return in
}

// TestApply_HoldFoldAppliedBeforeTerminal: the fold decide sorts AFTER the Cancel or Complete
// that ends the hold would find the row terminal, so applyOrder runs it first. Both outcomes
// are decide's own, not hand-built.
func TestApply_HoldFoldAppliedBeforeTerminal(t *testing.T) {
	t.Run("t11/hold-cap-with-an-open-hold-folds-it", func(t *testing.T) {
		db := snapDB(t)
		acct := snapBuildLegacy(t, db)
		r := snapOpenReq(decide.ReqCredentialRemove, acct.sso)
		r.originClass = decide.C1
		r.openedAt, r.completesAt, r.heldSince = snapNow.Add(-72*time.Hour), snapNow, snapNow.Add(-72*time.Hour)
		snapInsertReq(t, db, acct.user.id, r)
		req := decide.Req{
			ID: r.id.String(), Kind: decide.ReqCredentialRemove, Status: decide.StatusPending, OriginClass: decide.C1,
			Origins: []decide.CredRef{acct.user.pwRef}, Targets: []decide.CredRef{acct.sso},
			OpenedAt: r.openedAt, CompletesAt: r.completesAt, HeldSince: r.heldSince, MatureNoticesSettled: true,
		}

		o := decide.Decide(appDecideInput(acct, req))
		require.Equal(t, []decide.EffectKind{decide.EffectCancel, decide.EffectHold}, appKinds(o.Effects), "decide's canonical order")
		require.Equal(t, decide.CancelHoldCap, o.Effects[0].Reason)
		require.EqualValues(t, 259200000, o.Effects[1].HeldMs)
		require.True(t, o.Effects[1].HeldSince.IsZero(), "a fold, not an opening hold")
		assert.Equal(t, []int{1, 0}, applyOrder(o))

		a := appCtx(acct.user.id, appReqFor(r))
		a.outcome = o
		require.NoError(t, appApplyWith(t, db, productionRegistry(), a))

		want := appRowOf(t, acct.user.id, r)
		want.Status, want.CancelledAt = "cancelled", appTimePtr(snapNow)
		want.CancelReason = appPtr("hold_cap")
		want.HeldMs, want.HeldSince = 259200000, nil
		assert.Equal(t, want, appReadReq(t, db, r.id))
	})

	t.Run("completeIfDue: t11/completes-after-folding-the-hold", func(t *testing.T) {
		db := snapDB(t)
		acct := snapBuildLegacy(t, db)
		r := snapOpenReq(decide.ReqCredentialRemove, acct.sso)
		r.originClass = decide.C1
		r.openedAt, r.completesAt, r.heldSince = snapNow.Add(-83*time.Hour), snapNow.Add(-11*time.Hour), snapNow.Add(-10*time.Hour)
		snapInsertReq(t, db, acct.user.id, r)
		req := decide.Req{
			ID: r.id.String(), Kind: decide.ReqCredentialRemove, Status: decide.StatusPending, OriginClass: decide.C1,
			Origins: []decide.CredRef{acct.user.pwRef}, Targets: []decide.CredRef{acct.sso},
			OpenedAt: r.openedAt, CompletesAt: r.completesAt, HeldSince: r.heldSince,
			AcceptedAddressNotice: true, MatureNoticesSettled: true,
		}

		o := decide.Decide(appDecideInput(acct, req))
		require.Equal(t, []decide.EffectKind{
			decide.EffectRemove, decide.EffectComplete, decide.EffectHold, decide.EffectResyncMFAFlags,
		}, appKinds(o.Effects), "decide's canonical order: the Hold sorts after the Complete")
		require.EqualValues(t, 36000000, o.Effects[2].HeldMs)
		require.True(t, o.Effects[2].HeldSince.IsZero())
		assert.Equal(t, []int{2, 0, 1, 3}, applyOrder(o))

		// The resync is PR6's; a recording resyncer and a no-op applier stand in for it.
		reg := regWire(regResyncKey)
		reg.resync = &regSink{}
		a := appCtx(acct.user.id, appReqFor(r))
		a.outcome = o
		require.NoError(t, appApplyWith(t, db, reg, a))

		want := appRowOf(t, acct.user.id, r)
		want.Status, want.CompletedAt = "completed", appTimePtr(snapNow)
		want.HeldMs, want.HeldSince = 36000000, nil
		assert.Equal(t, want, appReadReq(t, db, r.id))
		_, present := appFactor(t, db, acct.sso)
		assert.False(t, present, "the completion's own Remove ran")
	})
}

// TestApply_ExecRowsMoreRowsThanWantedIsContract (ruling 17): a statement that touches more
// rows than it named is a defect, not a race, so it is ErrContract and never ErrStale.
func TestApply_ExecRowsMoreRowsThanWantedIsContract(t *testing.T) {
	db := snapDB(t)
	u := newSnapUser()
	u.insert(t, db)
	appReservation(t, db, u.id, "one-"+u.id.String()[:8]+"@example.test")
	appReservation(t, db, u.id, "two-"+u.id.String()[:8]+"@example.test")
	const query = `DELETE FROM account_address_reservations WHERE user_id = $1`
	run := func(t *testing.T, want int64) error {
		t.Helper()
		ctx := context.Background()
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer func() { require.NoError(t, tx.Rollback()) }()
		return execRows(ctx, tx, want, query, u.id)
	}

	err := run(t, 1)
	assert.ErrorIs(t, err, ErrContract)
	assert.NotErrorIs(t, err, ErrStale)
	assert.NoError(t, run(t, 2), "control: exactly the wanted count")
	assert.ErrorIs(t, run(t, 3), ErrStale, "fewer than wanted stays ErrStale")
}

// TestApply_NonArgon2idHashIsContract (ruling 20): a pending password that is not an
// $argon2id$ hash is refused in every path that would store or install it, and the error
// never carries the value.
func TestApply_NonArgon2idHashIsContract(t *testing.T) {
	const bad = "plaintext-password-fixture" // pragma: allowlist secret
	noValue := func(t *testing.T, err error) {
		t.Helper()
		require.ErrorIs(t, err, ErrContract)
		assert.NotContains(t, err.Error(), bad)
		assert.NotContains(t, fmt.Sprintf("%+v", err), bad)
	}

	t.Run("T6c open", func(t *testing.T) {
		db := snapDB(t)
		acct := appT6cAccount(t, db)
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0], a.pendingPasswordHash = id, bad // pragma: allowlist secret
		a.outcome.EnrolClass = decide.C3
		noValue(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: appT6cOpening([]decide.CredRef{acct.user.pwRef, acct.web})}))
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE id = $1`, id).Scan(&n))
		assert.Zero(t, n)
		assert.Equal(t, appOldHash, appReadUserPw(t, db, acct.user.id).Hash)
		assert.Empty(t, appReadHistory(t, db, acct.user.id))
	})

	t.Run("pending break-glass open", func(t *testing.T) {
		db, acct, origins := appOpenSetup(t)
		id := uuid.New()
		a := appCtx(acct.user.id)
		a.requestIDs[0], a.pendingPasswordHash = id, bad // pragma: allowlist secret
		noValue(t, appRun(t, db, a, decide.Effect{Kind: decide.EffectOpen, Open: appPendingOpening(decide.ReqBreakGlass, nil, origins)}))
		var n int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM account_pending_requests WHERE id = $1`, id).Scan(&n))
		assert.Zero(t, n)
	})

	t.Run("T11 complete reads the row's pending value", func(t *testing.T) {
		db := snapDB(t)
		acct, r := appBreakGlass(t, db, snapLegacy)
		appSetPayload(t, db, r.id, bad, appPendingAddr)
		a, complete := appBreakGlassCtx(acct, r)
		noValue(t, appRun(t, db, a, complete))
		assert.Equal(t, "pending", appReadReq(t, db, r.id).Status)
		assert.Equal(t, appOldHash, appReadUserPw(t, db, acct.user.id).Hash)
		assert.Empty(t, appReadHistory(t, db, acct.user.id))
	})
}
