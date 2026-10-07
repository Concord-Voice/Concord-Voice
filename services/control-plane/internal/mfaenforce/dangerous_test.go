package mfaenforce_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/mfaenforce"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/permgen"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// dangerBit is one permission bit; which one does not matter to the store.
const dangerBit int64 = 1 << 5

// graceEnv is a miniredis-backed grace store and budget. miniredis, not
// redistest, for its frozen clock: the no-sliding test reads a TTL exactly.
type graceEnv struct {
	mr     *miniredis.Miniredis
	store  stepup.GraceStore
	budget stepup.Budget
	actor  stepup.GraceActor
	scope  stepup.GraceScope
}

func newGraceEnv(t *testing.T, actorID, serverID string) graceEnv {
	t.Helper()
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, mr.Set(permgen.UserKey(actorID), "ugen"))
	require.NoError(t, mr.Set(permgen.ServerKey(serverID), "sgen"))
	return graceEnv{
		mr: mr, store: stepup.NewGraceStore(rdb), budget: stepup.DangerousActionBudget(rdb),
		actor: stepup.GraceActor{UserID: uuid.MustParse(actorID), SessionID: uuid.NewString()},
		scope: stepup.DangerousActionGraceScope(uuid.MustParse(serverID), dangerBit),
	}
}

func (e graceEnv) read() stepup.GraceRead {
	return e.store.Read(context.Background(), e.actor, e.scope)
}

// granted is a pre-read that holds a grace of the given strength.
func (e graceEnv) granted(t *testing.T, strength stepup.GraceStrength) stepup.GraceRead {
	t.Helper()
	require.NoError(t, e.store.Grant(context.Background(), e.read(), strength))
	return e.read()
}

func (e graceEnv) graceKey(t *testing.T) string {
	t.Helper()
	for _, k := range e.mr.Keys() {
		if strings.HasPrefix(k, "stepup:grace:") {
			return k
		}
	}
	return ""
}

// requireOutcome runs Require on a fresh gate for actor on server.
func requireOutcome(
	t *testing.T, db *sql.DB, serverID, actorID string, in mfaenforce.Confirm, fires bool,
) (mfaenforce.Outcome, *stepup.Error) {
	t.Helper()
	g, tx := lockGate(t, db, serverID, actorID)
	in.ActorID = actorID
	return mfaenforce.Require(context.Background(), tx, g, in, fires)
}

func requireMFARequired(t *testing.T, e *stepup.Error) {
	t.Helper()
	require.NotNil(t, e)
	require.Equal(t, http.StatusForbidden, e.Status)
	require.Equal(t, true, e.Body["mfa_required"])
}

// TestRequire_AsksNothingWhenTheGateDoesNotFire: on a server that does not
// enforce, or for a predicate that did not fire, Require consults neither the
// grace nor the verifier and confirms nothing. Mutation: drop either half of
// `!g.Enforcing || !fires` and a row reaches ConfirmTx and is refused.
func TestRequire_AsksNothingWhenTheGateDoesNotFire(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	for name, tc := range map[string]struct {
		enforcing, fires bool
	}{"not enforcing": {false, true}, "predicate did not fire": {true, false}} {
		spy := &spyVerifier{valid: true}
		out, e := requireOutcome(t, db, createServer(t, db, owner, tc.enforcing), owner,
			mfaenforce.Confirm{Verifier: spy, Purpose: stepup.PurposeChannelDelete}, tc.fires)
		require.Nil(t, e, name)
		require.Equal(t, mfaenforce.Unconfirmed, out, name)
		require.False(t, out.Confirmed(), name)
		require.Zero(t, spy.calls, name)
	}
}

// TestRequire_ValidGraceConfirmsWithoutAVerifierCall: a valid mfa grace for an
// eligible purpose confirms with no code and no verifier call, and reports
// GraceCovered so Settle grants nothing. Mutation: delete the Covers branch
// and the empty code is refused with mfa_required.
func TestRequire_ValidGraceConfirmsWithoutAVerifierCall(t *testing.T) {
	db := gateTestDB(t)
	_, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	server := createServer(t, db, owner, true)
	env := newGraceEnv(t, owner, server)

	spy := &spyVerifier{}
	out, e := requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Verifier: spy, Purpose: stepup.PurposeChannelDelete, Grace: env.granted(t, stepup.GraceStrengthMFA)}, true)
	require.Nil(t, e)
	require.Equal(t, mfaenforce.GraceCovered, out)
	require.True(t, out.Confirmed())
	require.Zero(t, spy.calls, "a grace-covered action calls no verifier")

	// A code sent beside a valid grace is ignored, not verified.
	spy = &spyVerifier{valid: false}
	out, e = requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Verifier: spy, Purpose: stepup.PurposeChannelDelete, Code: "000000", Grace: env.read()}, true)
	require.Nil(t, e)
	require.Equal(t, mfaenforce.GraceCovered, out)
	require.Zero(t, spy.calls)
}

// TestRequire_GraceThatCannotCoverFallsBackToAPrompt: each way a stored grace
// fails the server rule ends at ConfirmTx, which prompts for a code.
func TestRequire_GraceThatCannotCoverFallsBackToAPrompt(t *testing.T) {
	db := gateTestDB(t)
	_, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	server := createServer(t, db, owner, true)

	cases := map[string]func(env graceEnv) (stepup.Purpose, stepup.GraceRead){
		"always-fresh purpose": func(env graceEnv) (stepup.Purpose, stepup.GraceRead) {
			return stepup.PurposeServerDelete, env.granted(t, stepup.GraceStrengthMFA)
		},
		"password-strength grace": func(env graceEnv) (stepup.Purpose, stepup.GraceRead) {
			return stepup.PurposeChannelDelete, env.granted(t, stepup.GraceStrengthPassword)
		},
		"no grace read (Redis down)": func(graceEnv) (stepup.Purpose, stepup.GraceRead) {
			return stepup.PurposeChannelDelete, stepup.GraceRead{}
		},
	}
	for name, setup := range cases {
		purpose, read := setup(newGraceEnv(t, owner, server))
		spy := &spyVerifier{}
		out, e := requireOutcome(t, db, server, owner, mfaenforce.Confirm{Verifier: spy, Purpose: purpose, Grace: read}, true)
		requireMFARequired(t, e)
		require.Equal(t, mfaenforce.Unconfirmed, out, name)
	}
}

// TestRequire_UnenrolledActorIsNotCoveredByAStaleGrace: a valid-looking mfa
// grace does not cover an actor who no longer has an inline factor (a bump
// that never landed); they get the enrollment refusal instead.
func TestRequire_UnenrolledActorIsNotCoveredByAStaleGrace(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	server := createServer(t, db, owner, true)
	env := newGraceEnv(t, owner, server)
	out, e := requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Verifier: &spyVerifier{}, Purpose: stepup.PurposeChannelDelete, Grace: env.granted(t, stepup.GraceStrengthMFA)}, true)
	requireEnrollmentRequired(t, e)
	require.Equal(t, mfaenforce.Unconfirmed, out)
}

// TestRequire_VerifiesTheCodeUnderTheRoutesPurpose: with no grace, the gate is
// ConfirmTx under the route's purpose: Verified on a good code, the plain 403
// on a bad one, and a 500 for an unwired verifier.
func TestRequire_VerifiesTheCodeUnderTheRoutesPurpose(t *testing.T) {
	db := gateTestDB(t)
	_, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	server := createServer(t, db, owner, true)

	good := &spyVerifier{valid: true}
	out, e := requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Verifier: good, Purpose: stepup.PurposeMemberBan, Code: "123456"}, true)
	require.Nil(t, e)
	require.Equal(t, mfaenforce.Verified, out)
	require.True(t, out.Confirmed())
	require.Equal(t, stepup.PurposeMemberBan, good.purpose)

	out, e = requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Verifier: &spyVerifier{valid: false}, Purpose: stepup.PurposeMemberBan, Code: "123456"}, true)
	require.NotNil(t, e)
	require.Equal(t, http.StatusForbidden, e.Status)
	require.Equal(t, stepup.ErrMsgInvalidMFACode, e.Body["error"])
	require.Equal(t, mfaenforce.Unconfirmed, out)

	out, e = requireOutcome(t, db, server, owner,
		mfaenforce.Confirm{Purpose: stepup.PurposeMemberBan, Code: "123456"}, true)
	require.NotNil(t, e)
	require.Equal(t, http.StatusInternalServerError, e.Status)
	require.Equal(t, mfaenforce.Unconfirmed, out)
}

// TestSettle_GrantsAndClearsOnlyForAVerifiedOutcome: Verified clears the
// budget and grants a GraceTTL grace; GraceCovered and Unconfirmed write
// nothing, so a covered action leaves the existing grace's TTL where it was
// (no sliding). Mutation: settle on outcome.Confirmed() instead of Verified
// and the GraceCovered row's TTL is reset to the full window.
func TestSettle_GrantsAndClearsOnlyForAVerifiedOutcome(t *testing.T) {
	ctx := context.Background()
	log := logger.NewWithWriter(&bytes.Buffer{})
	actor, server := uuid.NewString(), uuid.NewString()

	t.Run("verified", func(t *testing.T) {
		env := newGraceEnv(t, actor, server)
		require.Nil(t, env.budget.Consume(ctx, actor))
		mfaenforce.Settle(ctx, log, mfaenforce.Verified, env.budget, env.store, env.read(), actor)
		require.False(t, env.mr.Exists(env.budget.Key(actor)), "the budget is cleared")
		require.Equal(t, stepup.GraceTTL, env.mr.TTL(env.graceKey(t)))
		require.True(t, env.read().Covers(stepup.PurposeChannelDelete, stepup.GraceServerRule, enrolledTOTP))
	})

	t.Run("grace-covered", func(t *testing.T) {
		env := newGraceEnv(t, actor, server)
		read := env.granted(t, stepup.GraceStrengthMFA)
		env.mr.FastForward(4 * time.Minute)
		require.Nil(t, env.budget.Consume(ctx, actor))
		mfaenforce.Settle(ctx, log, mfaenforce.GraceCovered, env.budget, env.store, read, actor)
		require.Equal(t, stepup.GraceTTL-4*time.Minute, env.mr.TTL(env.graceKey(t)), "a covered action must not slide the window")
		require.True(t, env.mr.Exists(env.budget.Key(actor)), "only a verified factor clears the budget")
	})

	t.Run("unconfirmed", func(t *testing.T) {
		env := newGraceEnv(t, actor, server)
		require.Nil(t, env.budget.Consume(ctx, actor))
		mfaenforce.Settle(ctx, log, mfaenforce.Unconfirmed, env.budget, env.store, env.read(), actor)
		require.Empty(t, env.graceKey(t))
		require.True(t, env.mr.Exists(env.budget.Key(actor)))
	})
}

// TestSettle_FailuresAreWarnedWithFixedClasses: a dead Redis changes no
// response; each failed write is one Warn carrying its fixed failure_class.
func TestSettle_FailuresAreWarnedWithFixedClasses(t *testing.T) {
	actor := uuid.NewString()
	read := newGraceEnv(t, actor, uuid.NewString()).read()
	// The fast-failing dead-client options from [internal]rules/tests.md: a
	// closed miniredis with default retries would spend the whole SettleTimeout.
	dead := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1, DialerRetries: 1})
	t.Cleanup(func() { _ = dead.Close() })
	var buf bytes.Buffer
	mfaenforce.Settle(context.Background(), logger.NewWithWriter(&buf), mfaenforce.Verified,
		stepup.DangerousActionBudget(dead), stepup.NewGraceStore(dead), read, actor)
	out := buf.String()
	require.Contains(t, out, "level=WARN")
	require.Contains(t, out, "failure_class="+mfaenforce.FailureClassGateBudgetClear)
	require.Contains(t, out, "failure_class="+stepup.FailureClassGraceGrant)
}

func lockConflict() error { return &pq.Error{Code: "55P03"} }

// TestWriteError_ClassifiesInOrder pins A-4's order and bodies. The second row
// is why IsLockConflict comes first: a users-row timeout arrives inside a
// *stepup.Error's Cause, and answering that 500 would hide a retryable 503.
func TestWriteError_ClassifiesInOrder(t *testing.T) {
	gin.SetMode(gin.TestMode)
	gone := func(c *gin.Context) { c.JSON(http.StatusForbidden, gin.H{"error": "route's vanished-server answer"}) }
	cases := []struct {
		name       string
		err        error
		serverGone func(*gin.Context)
		status     int
		body       string
		log        string
	}{
		{"lock conflict", fmt.Errorf("lock: %w", lockConflict()), gone, 503,
			`{"error":"The server is busy. Try again.","lock_conflict":true}`, "failure_class=mfa_gate_lock"},
		{"lock conflict inside a step-up error",
			&stepup.Error{Status: 500, Body: gin.H{"error": stepup.ErrMsgVerificationFailed}, Cause: lockConflict()}, gone, 503,
			`{"error":"The server is busy. Try again.","lock_conflict":true}`, "failure_class=mfa_gate_lock"},
		{"refusal", stepup.EnrollmentRequired(), gone, 403,
			`{"error":"` + stepup.ErrMsgMFAEnrollmentRequired + `","mfa_enrollment_required":true}`, ""},
		{"budget 429", &stepup.Error{Status: 429, Body: gin.H{"error": stepup.ErrMsgTooManyAttempts, "step_up_budget_exhausted": true}},
			gone, 429, `{"error":"Too many verification attempts","step_up_budget_exhausted":true}`, ""},
		{"verifier fault", &stepup.Error{Status: 500, Body: gin.H{"error": stepup.ErrMsgVerificationFailed}, Cause: errors.New("store down")},
			gone, 500, `{"error":"Verification failed"}`, "failure_class=mfa_gate_failed"},
		{"vanished server", fmt.Errorf("gate: %w", mfaenforce.ErrServerNotFound), gone, 403,
			`{"error":"route's vanished-server answer"}`, ""},
		{"vanished server, no route answer", mfaenforce.ErrServerNotFound, nil, 500,
			`{"error":"Internal server error"}`, "failure_class=mfa_gate_failed"},
		{"anything else", errors.New("boom"), gone, 500, `{"error":"Internal server error"}`, "failure_class=mfa_gate_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			var buf bytes.Buffer
			mfaenforce.WriteError(c, logger.NewWithWriter(&buf), tc.err, tc.serverGone)
			require.Equal(t, tc.status, w.Code)
			require.JSONEq(t, tc.body, w.Body.String())
			if tc.status == 503 {
				require.Equal(t, "1", w.Header().Get("Retry-After"))
			}
			if tc.log == "" {
				require.Empty(t, buf.String(), "a 4xx outcome is not logged")
			} else {
				require.Contains(t, buf.String(), tc.log)
			}
		})
	}
}

// gateObservation is what a WithGateTx test's fn saw. fn only records: a
// require inside it would FailNow mid-transaction, and before the
// unconditional rollback that left the gate's locks held under the testdb
// cleanup's TRUNCATE until go test's timeout.
type gateObservation struct {
	gate              mfaenforce.Gate
	lockTimeout, idle string
	toggleBlocked     bool
	err               error
}

// TestWithGateTx_CommitsBoundsAndHoldsTheGate: fn runs under both bounds and
// the gate's server lock, so the toggle's UPDATE waits on it; its write
// commits; and the idle bound is set only for a transaction that holds an
// external write. The probe carries its own 100ms lock_timeout.
func TestWithGateTx_CommitsBoundsAndHoldsTheGate(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	server := createServer(t, db, owner, true)
	ctx := t.Context()
	var baselineIdle string
	require.NoError(t, db.QueryRow(`SELECT current_setting('idle_in_transaction_session_timeout')`).Scan(&baselineIdle))

	for _, holds := range []bool{false, true} {
		spec := mfaenforce.GateSpec{ServerID: server, ActorID: owner, UserLock: stepup.LockForShare,
			ServerLock: mfaenforce.ServerForNoKeyUpdate, HoldsExternalWrite: holds}
		name := fmt.Sprintf("renamed-%t", holds)
		var seen gateObservation
		err := mfaenforce.WithGateTx(ctx, db, spec, func(tx *sql.Tx, g mfaenforce.Gate) error {
			seen.gate = g
			if seen.err = tx.QueryRowContext(ctx,
				`SELECT current_setting('lock_timeout'), current_setting('idle_in_transaction_session_timeout')`,
			).Scan(&seen.lockTimeout, &seen.idle); seen.err != nil {
				return seen.err
			}
			seen.toggleBlocked, seen.err = toggleWaits(ctx, db, server)
			if seen.err != nil {
				return seen.err
			}
			_, err := tx.ExecContext(ctx, `UPDATE servers SET name = $2 WHERE id = $1`, server, name)
			return err
		})
		require.NoError(t, err)
		require.True(t, seen.gate.Enforcing)
		require.Equal(t, owner, seen.gate.OwnerID)
		require.Equal(t, "3s", seen.lockTimeout)
		if holds {
			require.Equal(t, "10s", seen.idle)
		} else {
			require.Equal(t, baselineIdle, seen.idle, "only an upload sets the idle bound")
		}
		require.True(t, seen.toggleBlocked, "the toggle must wait on the gate")
		var got string
		require.NoError(t, db.QueryRow(`SELECT name FROM servers WHERE id = $1`, server).Scan(&got))
		require.Equal(t, name, got)
	}
}

// toggleWaits runs the toggle's UPDATE in its own transaction under a 100ms
// lock_timeout and reports whether a held lock refused it. It returns errors
// instead of failing the test, because it runs inside a gate transaction.
func toggleWaits(ctx context.Context, db *sql.DB, serverID string) (bool, error) {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, probeLockTimeout); err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, `UPDATE servers SET enforce_mfa_dangerous_actions = FALSE WHERE id = $1`, serverID)
	var pqErr *pq.Error
	if errors.As(err, &pqErr) && pqErr.Code == "55P03" {
		return true, nil
	}
	return false, err
}

// TestWithGateTx_RollsBackAndRestoresTheFactor: an error after a verified
// confirmation rolls everything back, the spent backup code included (A-1),
// and the route's own error comes back unwrapped. The caller settles only on
// a nil error, so a rolled-back action never grants grace.
func TestWithGateTx_RollsBackAndRestoresTheFactor(t *testing.T) {
	db := gateTestDB(t)
	verifier, ring := newVerifier(t, db)
	owner := createUser(t, db)
	enrollTOTP(t, db, ring, owner)
	server := createServer(t, db, owner, true)
	env := newGraceEnv(t, owner, server)
	ctx := t.Context()
	routeErr := errors.New("the route's own failure")

	var outcome mfaenforce.Outcome
	var refusal *stepup.Error
	var spentInTx bool
	spec := mfaenforce.GateSpec{ServerID: server, ActorID: owner, UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForNoKeyUpdate}
	err := mfaenforce.WithGateTx(ctx, db, spec, func(tx *sql.Tx, g mfaenforce.Gate) error {
		outcome, refusal = mfaenforce.Require(ctx, tx, g, mfaenforce.Confirm{
			Verifier: verifier, ActorID: owner, Purpose: stepup.PurposeServerUpdate, Code: testBackupCode, Grace: env.read(),
		}, true)
		if refusal != nil {
			return refusal
		}
		var err error
		if spentInTx, err = backupCodeUsedOn(ctx, tx, owner); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE servers SET name = 'rolled back' WHERE id = $1`, server); err != nil {
			return err
		}
		return routeErr
	})
	require.Nil(t, refusal)
	require.Same(t, routeErr, err, "the route's error is returned as is")
	require.Equal(t, mfaenforce.Verified, outcome)
	require.True(t, spentInTx, "precondition: the code was spent on the transaction")
	require.False(t, backupCodeUsed(t, db, owner), "the rollback restores the backup code")
	var name string
	require.NoError(t, db.QueryRow(`SELECT name FROM servers WHERE id = $1`, server).Scan(&name))
	require.NotEqual(t, "rolled back", name)
	require.Empty(t, env.graceKey(t), "nothing settled a rolled-back action")
}

func backupCodeUsedOn(ctx context.Context, tx *sql.Tx, userID string) (bool, error) {
	var used []bool
	err := tx.QueryRowContext(ctx, `SELECT backup_codes_used FROM user_mfa_totp WHERE user_id = $1`, userID).Scan(pq.Array(&used))
	return len(used) == 1 && used[0], err
}

// TestWithGateTx_ReleasesTheGateWhenFnPanics: the rollback is unconditional,
// so a panic in fn (which gin's recovery would swallow) does not leave the
// servers row locked. Mutation: make the rollback conditional on a returned
// error and the toggle probe below finds the row still held.
func TestWithGateTx_ReleasesTheGateWhenFnPanics(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	server := createServer(t, db, owner, true)
	spec := mfaenforce.GateSpec{ServerID: server, ActorID: owner, UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForShare}
	require.Panics(t, func() {
		_ = mfaenforce.WithGateTx(t.Context(), db, spec, func(*sql.Tx, mfaenforce.Gate) error { panic("route bug") })
	})
	blocked, err := toggleWaits(t.Context(), db, server)
	require.NoError(t, err)
	require.False(t, blocked, "the panicking gate transaction must have rolled back")
}

// TestWithGateTx_JoinsAFailedRollback: a rollback that itself fails is joined
// onto fn's error rather than discarded, and fn's error is still found by
// errors.Is. The transaction's backend is terminated under it, so the deferred
// ROLLBACK meets a dead connection. Mutation: discard the rollback's result
// and the joined "roll back gate transaction" error is gone.
func TestWithGateTx_JoinsAFailedRollback(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	server := createServer(t, db, owner, true)
	routeErr := errors.New("the route's own failure")
	spec := mfaenforce.GateSpec{ServerID: server, ActorID: owner, UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForShare}
	err := mfaenforce.WithGateTx(t.Context(), db, spec, func(tx *sql.Tx, _ mfaenforce.Gate) error {
		terminateTxBackend(t, db, tx)
		return routeErr
	})
	require.ErrorIs(t, err, routeErr, "fn's error survives the join")
	require.ErrorContains(t, err, "roll back gate transaction")
}

// terminateTxBackend ends tx's server process from another connection and
// waits for it to exit, so tx's next statement, ROLLBACK included, fails.
func terminateTxBackend(t *testing.T, db *sql.DB, tx *sql.Tx) {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid))
	var gone bool
	require.NoError(t, db.QueryRowContext(t.Context(), `SELECT pg_terminate_backend($1, 5000)`, pid).Scan(&gone))
	require.True(t, gone, "the backend must have exited before fn returns")
}

// TestWithGateTx_GateErrorsComeBackUnwrapped: a vanished server reaches
// WriteError as ErrServerNotFound, and fn never runs.
func TestWithGateTx_GateErrorsComeBackUnwrapped(t *testing.T) {
	db := gateTestDB(t)
	owner := createUser(t, db)
	called := false
	err := mfaenforce.WithGateTx(t.Context(), db, mfaenforce.GateSpec{
		ServerID: uuid.NewString(), ActorID: owner, UserLock: stepup.LockForShare, ServerLock: mfaenforce.ServerForShare,
	}, func(*sql.Tx, mfaenforce.Gate) error { called = true; return nil })
	require.ErrorIs(t, err, mfaenforce.ErrServerNotFound)
	require.False(t, called)
}

// TestIsGateError pins the classes WriteError answers with the gate's own
// status, wrapped or not, and that a route's own failure is not one of them.
// Mutation: drop any one disjunct from IsGateError.
func TestIsGateError(t *testing.T) {
	lockConflict := &pq.Error{Code: "55P03"}
	stepErr := stepup.EnrollmentRequired()
	for name, tc := range map[string]struct {
		err  error
		want bool
	}{
		"lock conflict":            {lockConflict, true},
		"wrapped lock conflict":    {fmt.Errorf("gate: %w", lockConflict), true},
		"step-up error":            {stepErr, true},
		"wrapped step-up error":    {fmt.Errorf("gate: %w", stepErr), true},
		"server not found":         {mfaenforce.ErrServerNotFound, true},
		"wrapped server not found": {fmt.Errorf("gate: %w", mfaenforce.ErrServerNotFound), true},
		"route's own failure":      {errors.New("insert failed"), false},
		"nil":                      {nil, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.want, mfaenforce.IsGateError(tc.err))
		})
	}
}
