// Package presencehook_test exercises the shared #2446 handler plumbing through
// its exported surface, which is the same surface the consumer packages use.
//
// The transaction terminals run against a stub database/sql driver rather than a
// real database: sql.ErrTxDone is produced by database/sql itself, so the
// already-committed path — the NORMAL successful path, because Complete owns the
// commit — is reproducible with no external dependency.
package presencehook_test

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// --- stub driver -----------------------------------------------------------

type stubDriver struct {
	rollbackErr error
	beginErr    error
}

func (d *stubDriver) Open(string) (driver.Conn, error) { return &stubConn{d: d}, nil }

type stubConn struct{ d *stubDriver }

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *stubConn) Close() error { return nil }
func (c *stubConn) Begin() (driver.Tx, error) {
	if c.d.beginErr != nil {
		return nil, c.d.beginErr
	}
	return &stubTx{d: c.d}, nil
}

type stubTx struct{ d *stubDriver }

func (t *stubTx) Commit() error   { return nil }
func (t *stubTx) Rollback() error { return t.d.rollbackErr }

var driverSeq atomic.Int64

// openStubDB registers a fresh driver name per call so tests never share state.
func openStubDB(t *testing.T, rollbackErr error) *sql.DB {
	t.Helper()
	return openStubDBWith(t, &stubDriver{rollbackErr: rollbackErr})
}

func openStubDBWith(t *testing.T, d *stubDriver) *sql.DB {
	t.Helper()

	name := "presencehook-stub-" + strconv.FormatInt(driverSeq.Add(1), 10)
	sql.Register(name, d)

	db, err := sql.Open(name, "")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func bufferedLogger() (*logger.Logger, *bytes.Buffer) {
	buf := &bytes.Buffer{}
	return logger.NewWithWriter(buf), buf
}

// --- capture double --------------------------------------------------------

// stubCapture records what the plumbing hands it. It never touches a real
// transaction, so it exercises the wiring without a database.
type stubCapture struct {
	subjects   []presencecapture.Subject
	abandons   []string
	completes  int
	captureErr error
	planOut    presencecapture.Plan
	completeIn error
}

// WithGatedTx satisfies the interface without taking a gate or opening a
// transaction. NOTHING ROUTES THROUGH THIS DOUBLE'S COPY: every test above
// drives Capture / Complete / Abandon with a transaction it opened itself, and
// the gated-path tests below use gatedCaptureStub instead. It therefore keeps
// its sentinel rather than becoming a no-op — a double that swallowed work and
// reported success would let a later task move a handler onto the gated path
// with the work never run, which is the hole the sentinel was added to close.
func (*stubCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	return errors.New("stubCapture: WithGatedTx is not implemented by this double")
}

func (c *stubCapture) CaptureInTx(
	_ context.Context, _ *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	c.subjects = append(c.subjects, subject)
	if c.captureErr != nil {
		return nil, c.captureErr
	}
	return c.planOut, nil
}

func (c *stubCapture) Complete(_ context.Context, _ *sql.Tx, _ presencecapture.Plan) error {
	c.completes++
	return c.completeIn
}

func (c *stubCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	c.abandons = append(c.abandons, string(cause))
}

// --- RollbackUnlessDone ----------------------------------------------------

// An open transaction really is rolled back, and nothing is logged.
func TestRollbackUnlessDoneRollsBackOpenTransaction(t *testing.T) {
	db := openStubDB(t, nil)
	tx, err := db.Begin()
	require.NoError(t, err)
	log, buf := bufferedLogger()

	presencehook.RollbackUnlessDone(tx, log)

	assert.ErrorIs(t, tx.Commit(), sql.ErrTxDone, "the transaction must already be finished")
	assert.Empty(t, buf.String(), "a successful rollback must log nothing")
}

// The already-committed case is the NORMAL successful path: Complete owns the
// commit, so the deferred rollback always fires against a finished transaction.
// Logging there would put a spurious error on every 2xx.
func TestRollbackUnlessDoneToleratesCommittedTransaction(t *testing.T) {
	db := openStubDB(t, nil)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	log, buf := bufferedLogger()

	assert.NotPanics(t, func() { presencehook.RollbackUnlessDone(tx, log) })
	assert.Empty(t, buf.String(), "sql.ErrTxDone is not an error condition")
}

// A rollback that fails for any OTHER reason is a real failure, and it reaches
// the sink as BOTH the fixed failure_class and the driver error — the shape
// internal/friends/handlers.go already uses for its capture terminal. The
// constant is the stable grep target; the error is the only record of the
// cause, because this defer has no return value to carry it.
func TestRollbackUnlessDoneLogsGenuineFailureWithClassAndCause(t *testing.T) {
	db := openStubDB(t, errors.New("connection reset"))
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	log, buf := bufferedLogger()

	presencehook.RollbackUnlessDone(tx, log)

	logged := buf.String()
	assert.Contains(t, logged, "rollback failed")
	assert.Contains(t, logged, "gated_rollback",
		"the fixed class is what makes the failure greppable")
	assert.Contains(t, logged, "connection reset",
		"the driver error is the only record of why the discard failed")
}

// --- Spec.Subject ----------------------------------------------------------

func TestSubjectParsesBothEndpoints(t *testing.T) {
	principal, counterpart := uuid.New(), uuid.New()

	subject, err := presencehook.Spec{
		Family:        presencecapture.FamilyFriendshipRemove,
		Posture:       presencecapture.FailConservativeDegrade,
		PrincipalID:   principal.String(),
		CounterpartID: counterpart.String(),
	}.Subject()

	require.NoError(t, err)
	assert.Equal(t, presencecapture.FamilyFriendshipRemove, subject.Family)
	assert.Equal(t, presencecapture.FailConservativeDegrade, subject.FailPosture)
	assert.Equal(t, principal, subject.Principal)
	assert.Equal(t, counterpart, subject.Counterpart)
}

// A family with no counterpart carries uuid.Nil, which the bridge drops from the
// focal set. An empty string is therefore not an error.
func TestSubjectOmitsEmptyCounterpart(t *testing.T) {
	subject, err := presencehook.Spec{
		Family:      presencecapture.FamilyFriendsOfFriendsToggle,
		PrincipalID: uuid.NewString(),
	}.Subject()

	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, subject.Counterpart)
	assert.Equal(t, presencecapture.FailClosedBlockWrite, subject.FailPosture,
		"the zero posture must stay fail-closed")
}

// A malformed endpoint fails CLOSED rather than capturing against uuid.Nil,
// which would silently reconcile only half the mutation.
func TestSubjectRejectsMalformedEndpoints(t *testing.T) {
	t.Run("malformed principal", func(t *testing.T) {
		_, err := presencehook.Spec{PrincipalID: "not-a-uuid"}.Subject()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse capture principal")
	})

	t.Run("malformed counterpart", func(t *testing.T) {
		_, err := presencehook.Spec{
			PrincipalID:   uuid.NewString(),
			CounterpartID: "not-a-uuid",
		}.Subject()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse capture counterpart")
	})
}

// --- Capture ---------------------------------------------------------------

// An unwired capture must not touch the transaction at all: that is what keeps a
// replica without the hook behaving exactly as it did before #2446.
func TestCaptureUnwiredIsNoOp(t *testing.T) {
	plan, err := presencehook.Capture(context.Background(), nil, nil, presencehook.Spec{
		Family:      presencecapture.FamilyFriendshipAccept,
		PrincipalID: "not-even-a-uuid",
	})

	require.NoError(t, err, "an unwired handler must not fail on an ID it never had to parse")
	assert.Nil(t, plan)
}

func TestCaptureForwardsSubjectWhenWired(t *testing.T) {
	capture := &stubCapture{}
	principal, counterpart := uuid.New(), uuid.New()

	_, err := presencehook.Capture(context.Background(), capture, nil, presencehook.Spec{
		Family:        presencecapture.FamilyBlock,
		Posture:       presencecapture.FailConservativeDegrade,
		PrincipalID:   principal.String(),
		CounterpartID: counterpart.String(),
	})

	require.NoError(t, err)
	require.Len(t, capture.subjects, 1)
	assert.Equal(t, presencecapture.FamilyBlock, capture.subjects[0].Family)
	assert.Equal(t, principal, capture.subjects[0].Principal)
	assert.Equal(t, counterpart, capture.subjects[0].Counterpart)
}

func TestCaptureFailsClosedOnMalformedID(t *testing.T) {
	capture := &stubCapture{}

	_, err := presencehook.Capture(context.Background(), capture, nil, presencehook.Spec{
		Family:      presencecapture.FamilyFriendshipAccept,
		PrincipalID: "not-a-uuid",
	})

	require.Error(t, err)
	assert.Empty(t, capture.subjects, "a malformed endpoint must never reach the bridge")
}

func TestCapturePropagatesBridgeError(t *testing.T) {
	sentinel := errors.New("capture read failed")
	capture := &stubCapture{captureErr: sentinel}

	_, err := presencehook.Capture(context.Background(), capture, nil, presencehook.Spec{
		Family:      presencecapture.FamilyFriendshipAccept,
		PrincipalID: uuid.NewString(),
	})

	assert.ErrorIs(t, err, sentinel)
}

// --- Complete --------------------------------------------------------------

// An unwired Complete commits directly, so the caller NEVER calls tx.Commit()
// itself on either path.
func TestCompleteCommitsWhenUnwired(t *testing.T) {
	db := openStubDB(t, nil)
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	require.NoError(t, presencehook.Complete(context.Background(), nil, tx, nil))
	assert.ErrorIs(t, tx.Commit(), sql.ErrTxDone, "the transaction must already be committed")
}

func TestCompleteSurfacesUnwiredCommitFailure(t *testing.T) {
	db := openStubDB(t, nil)
	tx, err := db.Begin()
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	assert.ErrorIs(t, presencehook.Complete(context.Background(), nil, tx, nil), sql.ErrTxDone)
}

// The wired terminal owns the commit; the plumbing must delegate rather than
// commit itself, which is what lets the durable rail swap in at one site.
func TestCompleteDelegatesToWiredTerminal(t *testing.T) {
	sentinel := errors.New("terminal failed")
	capture := &stubCapture{completeIn: sentinel}
	db := openStubDB(t, nil)
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	err = presencehook.Complete(context.Background(), capture, tx, nil)

	assert.ErrorIs(t, err, sentinel)
	assert.Equal(t, 1, capture.completes)
	assert.NoError(t, tx.Rollback(), "the plumbing must not have finished the transaction")
}

// --- Abandon ---------------------------------------------------------------

func TestAbandonForwardsCauseWhenWired(t *testing.T) {
	capture := &stubCapture{}

	presencehook.Abandon(capture, nil, "write_failed")

	assert.Equal(t, []string{"write_failed"}, capture.abandons)
}

func TestAbandonUnwiredIsNoOp(t *testing.T) {
	assert.NotPanics(t, func() { presencehook.Abandon(nil, nil, "write_failed") })
}

// --- Classify --------------------------------------------------------------

func TestClassifyMapsEachTerminal(t *testing.T) {
	t.Run("nil error is not a failure", func(t *testing.T) {
		failure := presencehook.Classify(nil)
		assert.Equal(t, http.StatusOK, failure.Status)
		assert.Empty(t, failure.Code)
		assert.Zero(t, failure.RetryAfter)
	})

	t.Run("pending is 503 with a retry delay", func(t *testing.T) {
		failure := presencehook.Classify(
			fmt.Errorf("capture: %w", &presencecapture.PendingError{After: 12 * time.Second}))
		assert.Equal(t, http.StatusServiceUnavailable, failure.Status)
		assert.Equal(t, "presence_operation_pending", failure.Code)
		assert.Equal(t, 12*time.Second, failure.RetryAfter)
	})

	t.Run("post-commit delivery is 503 with no retry delay", func(t *testing.T) {
		failure := presencehook.Classify(
			fmt.Errorf("terminal: %w", presencecapture.ErrPostCommitDelivery))
		assert.Equal(t, http.StatusServiceUnavailable, failure.Status)
		assert.Equal(t, "delivery", failure.Code)
		assert.Zero(t, failure.RetryAfter)
	})

	t.Run("anything else is 500", func(t *testing.T) {
		failure := presencehook.Classify(errors.New("connection reset"))
		assert.Equal(t, http.StatusInternalServerError, failure.Status)
		assert.Equal(t, "internal", failure.Code)
		assert.Zero(t, failure.RetryAfter)
	})
}

// A post-commit delivery failure must NEVER surface as a 500. The mutation is
// durable and visible, so "Failed to remove friend" for a friendship that WAS
// removed is a correctness lie that invites a duplicate-action retry against
// state that already changed. It must never surface as a 2xx either.
func TestClassifyNeverReportsACommittedMutationAsAFailedOne(t *testing.T) {
	failure := presencehook.Classify(
		fmt.Errorf("dispatch friendship removal: %w", presencecapture.ErrPostCommitDelivery))

	assert.NotEqual(t, http.StatusInternalServerError, failure.Status,
		"a committed mutation must not be reported as a failed one")
	assert.NotEqual(t, http.StatusOK, failure.Status,
		"a failed delivery must not be reported as a clean success")
	assert.Equal(t, http.StatusServiceUnavailable, failure.Status)
}

// Failure.Code is where a wrapped database or delivery error would first reach
// a log sink: the handler puts it straight into the structured log's fixed
// failure_class field. [internal]rules/backend.md's #2446 bullet requires a closed
// vocabulary there, so the classifier must derive Code from the SENTINEL and
// never from the error's text — which here carries a user ID and Custom Status
// content precisely so an echo would be visible. That payload is what makes
// observability.md principle #2 (No PII) apply to this test; observability.md
// says nothing about failure_class itself.
func TestClassifyNeverEchoesTheUnderlyingError(t *testing.T) {
	const leak = "user 8f14e45f custom status lunch at Rosa's"
	closedVocabulary := []string{"presence_operation_pending", "delivery", "internal"}

	for name, cause := range map[string]error{
		"pending":     &presencecapture.PendingError{After: time.Second},
		"post-commit": presencecapture.ErrPostCommitDelivery,
		"unclassified": errors.New(
			"pq: duplicate key value violates unique constraint"),
	} {
		t.Run(name, func(t *testing.T) {
			failure := presencehook.Classify(fmt.Errorf("%s: %w", leak, cause))

			assert.Contains(t, closedVocabulary, failure.Code,
				"failure_class must stay a closed vocabulary")
			assert.NotContains(t, failure.Code, "lunch",
				"the error's text must never reach the log field")
			assert.NotContains(t, failure.Code, "8f14e45f")
		})
	}
}

// --- RetryAfter ------------------------------------------------------------

func TestRetryAfterHeaderRoundsUpAndNeverYieldsZero(t *testing.T) {
	for name, tc := range map[string]struct {
		delay time.Duration
		want  string
	}{
		"a producer that derived no delay floors at 1": {0, "1"},
		"a negative delay floors at 1":                 {-time.Second, "1"},
		"a sub-second delay floors at 1":               {10 * time.Millisecond, "1"},
		"a fractional delay rounds up":                 {1500 * time.Millisecond, "2"},
		"a whole-second delay is exact":                {30 * time.Second, "30"},
	} {
		t.Run(name, func(t *testing.T) {
			failure := presencehook.Classify(&presencecapture.PendingError{After: tc.delay})
			require.Equal(t, "presence_operation_pending", failure.Code)

			retry, ok := failure.RetryAfterHeader()

			require.True(t, ok, "the pending terminal is the one that carries a retry")
			assert.Equal(t, tc.want, retry)
		})
	}
}

// The form this method replaces — RetryAfterSeconds(failure.RetryAfter) — floored
// at 1 for EVERY terminal, so the obvious handler shape emitted 500 internal with
// Retry-After: 1 and told a client to re-drive a failure that does not resolve on
// its own, once per second. Only the self-resolving terminal may carry the header.
func TestRetryAfterHeaderIsWithheldFromEveryNonPendingTerminal(t *testing.T) {
	for name, cause := range map[string]error{
		"internal":             errors.New("connection reset"),
		"post-commit delivery": fmt.Errorf("terminal: %w", presencecapture.ErrPostCommitDelivery),
		"no failure at all":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			failure := presencehook.Classify(cause)
			require.NotEqual(t, "presence_operation_pending", failure.Code)

			retry, ok := failure.RetryAfterHeader()

			assert.False(t, ok,
				"a terminal that does not resolve on its own must promise no retry")
			assert.Empty(t, retry, "the caller must have nothing to stamp on the header")
		})
	}
}

func TestRetryAfterReadsThePendingDelay(t *testing.T) {
	assert.Equal(t, 5*time.Second,
		presencehook.RetryAfter(&presencecapture.PendingError{After: 5 * time.Second}))
	assert.Equal(t, 5*time.Second, presencehook.RetryAfter(
		fmt.Errorf("capture: %w", &presencecapture.PendingError{After: 5 * time.Second})),
		"the delay must survive a handler's wrap")
	assert.Zero(t, presencehook.RetryAfter(errors.New("connection reset")))
	assert.Zero(t, presencehook.RetryAfter(presencecapture.ErrCapturePending),
		"a bare sentinel carries no delay and must not invent one")
	assert.Zero(t, presencehook.RetryAfter(&presencecapture.PendingError{After: -time.Second}),
		"a producer that could not derive a delay must not yield a negative one")
	assert.Zero(t, presencehook.RetryAfter(nil))
}

// --- WithGatedTx -----------------------------------------------------------

// gatedCaptureStub records that WithGatedTx was the entry point and hands work
// a nil transaction, which is enough to prove delegation without a database.
type gatedCaptureStub struct {
	presencecapture.GraphPresenceCapture
	gotSubject presencecapture.Subject
	calls      int
	// refuse stands in for a gate that was never taken: the terminal returns
	// before work runs, exactly as the durable rail's pending marker does.
	refuse error
}

func (g *gatedCaptureStub) WithGatedTx(
	_ context.Context, subject presencecapture.Subject, work func(tx *sql.Tx) error,
) error {
	g.calls++
	g.gotSubject = subject
	if g.refuse != nil {
		return g.refuse
	}
	return work(nil)
}

func TestWithGatedTxDelegatesToTheCapture(t *testing.T) {
	capture := &gatedCaptureStub{}
	principal := uuid.New()
	log, buf := bufferedLogger()
	ran := false

	err := presencehook.WithGatedTx(context.Background(), capture, nil, log, presencehook.Spec{
		Family:      presencecapture.FamilyFriendsOfFriendsToggle,
		Posture:     presencecapture.FailClosedBlockWrite,
		PrincipalID: principal.String(),
	}, func(*sql.Tx) error {
		ran = true
		return nil
	})

	require.NoError(t, err)
	assert.True(t, ran)
	assert.Equal(t, 1, capture.calls)
	assert.Equal(t, presencecapture.FamilyFriendsOfFriendsToggle, capture.gotSubject.Family)
	assert.Equal(t, presencecapture.FailClosedBlockWrite, capture.gotSubject.FailPosture)
	assert.Equal(t, principal, capture.gotSubject.Principal)
	assert.Equal(t, uuid.Nil, capture.gotSubject.Counterpart)
	assert.Empty(t, buf.String(), "the wired path opens no transaction and logs nothing")
}

func TestWithGatedTxRejectsAMalformedIDBeforeOpeningAnything(t *testing.T) {
	capture := &gatedCaptureStub{}
	log, _ := bufferedLogger()

	err := presencehook.WithGatedTx(context.Background(), capture, nil, log, presencehook.Spec{
		Family:      presencecapture.FamilyBlock,
		PrincipalID: "not-a-uuid",
	}, func(*sql.Tx) error {
		t.Fatal("work must not run for a malformed principal ID")
		return nil
	})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse capture principal")
	assert.Equal(t, 0, capture.calls, "no gate and no transaction may be taken")
}

// The capture's terminal must reach the caller unaltered, because the handler
// classifies it with errors.Is. A wrapper that swallowed or re-typed it would
// turn a retryable 503 into a give-up 500.
func TestWithGatedTxSurfacesTheCaptureTerminalForClassification(t *testing.T) {
	pending := fmt.Errorf("begin topology batch: %w",
		&presencecapture.PendingError{After: 7 * time.Second})
	capture := &gatedCaptureStub{refuse: pending}
	log, _ := bufferedLogger()

	err := presencehook.WithGatedTx(context.Background(), capture, nil, log, presencehook.Spec{
		Family:      presencecapture.FamilyFriendshipRemove,
		PrincipalID: uuid.NewString(),
	}, func(*sql.Tx) error {
		t.Fatal("work must not run when the gate refused")
		return nil
	})

	require.ErrorIs(t, err, presencecapture.ErrCapturePending)
	failure := presencehook.Classify(err)
	assert.Equal(t, http.StatusServiceUnavailable, failure.Status)
	assert.Equal(t, "presence_operation_pending", failure.Code)
	retry, ok := failure.RetryAfterHeader()
	require.True(t, ok, "a pending terminal must reach the client with a retry delay")
	assert.Equal(t, "7", retry)
}

// The unwired fallback is the pre-#2446-PR-2 shape — db.BeginTx plus the
// deferred RollbackUnlessDone — so a replica without the hook behaves as it
// did before. These pin the whole branch, including the path a successful
// handler actually takes.
func TestWithGatedTxUnwiredOpensItsOwnTransaction(t *testing.T) {
	t.Run("work commits and the deferred discard stays silent", func(t *testing.T) {
		db := openStubDB(t, nil)
		log, buf := bufferedLogger()
		var handed *sql.Tx

		err := presencehook.WithGatedTx(context.Background(), nil, db, log, presencehook.Spec{
			Family:      presencecapture.FamilyFriendshipRemove,
			PrincipalID: uuid.NewString(),
		}, func(tx *sql.Tx) error {
			require.NotNil(t, tx, "the fallback must hand work a live transaction")
			handed = tx
			// Stands in for Complete, which owns the COMMIT on both paths.
			return tx.Commit()
		})

		require.NoError(t, err, "a committed mutation must not be reported as a failure")
		require.NotNil(t, handed)
		assert.ErrorIs(t, handed.Commit(), sql.ErrTxDone, "work's commit must have landed")
		assert.Empty(t, buf.String(),
			"sql.ErrTxDone is the normal successful path and must log nothing")
	})

	t.Run("work's error reaches the caller and the transaction is discarded", func(t *testing.T) {
		db := openStubDB(t, nil)
		log, buf := bufferedLogger()
		sentinel := errors.New("remove friendship")
		var handed *sql.Tx

		err := presencehook.WithGatedTx(context.Background(), nil, db, log, presencehook.Spec{
			Family:      presencecapture.FamilyBlock,
			PrincipalID: uuid.NewString(),
		}, func(tx *sql.Tx) error {
			handed = tx
			return sentinel
		})

		require.ErrorIs(t, err, sentinel)
		require.NotNil(t, handed)
		assert.ErrorIs(t, handed.Commit(), sql.ErrTxDone,
			"the deferred rollback must have discarded the transaction")
		assert.Empty(t, buf.String(), "a clean rollback logs nothing")
	})

	// An unwired handler must not start failing on an ID it never had to parse
	// before, which is the same guarantee Capture gives.
	t.Run("a malformed ID is not parsed at all", func(t *testing.T) {
		db := openStubDB(t, nil)
		log, _ := bufferedLogger()
		ran := false

		err := presencehook.WithGatedTx(context.Background(), nil, db, log, presencehook.Spec{
			Family:      presencecapture.FamilyFriendshipAccept,
			PrincipalID: "not-even-a-uuid",
		}, func(tx *sql.Tx) error {
			ran = true
			return tx.Commit()
		})

		require.NoError(t, err)
		assert.True(t, ran, "the unwired path parses nothing and must still run work")
	})
}

func TestWithGatedTxUnwiredFailsClosedWithoutARoute(t *testing.T) {
	t.Run("no database", func(t *testing.T) {
		log, _ := bufferedLogger()

		err := presencehook.WithGatedTx(context.Background(), nil, nil, log,
			presencehook.Spec{PrincipalID: uuid.NewString()},
			func(*sql.Tx) error {
				t.Fatal("work must not run without a transaction to run it in")
				return nil
			})

		require.Error(t, err)
		assert.Contains(t, err.Error(), "requires a database")
	})

	t.Run("begin fails", func(t *testing.T) {
		beginErr := errors.New("connection reset")
		db := openStubDBWith(t, &stubDriver{beginErr: beginErr})
		log, _ := bufferedLogger()

		err := presencehook.WithGatedTx(context.Background(), nil, db, log,
			presencehook.Spec{PrincipalID: uuid.NewString()},
			func(*sql.Tx) error {
				t.Fatal("work must not run when the transaction never opened")
				return nil
			})

		require.ErrorIs(t, err, beginErr, "the driver's cause must stay reachable")
		assert.Contains(t, err.Error(), "begin unhooked graph mutation")
		assert.Equal(t, http.StatusInternalServerError, presencehook.Classify(err).Status)
	})
}
