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
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehook"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// --- stub driver -----------------------------------------------------------

type stubDriver struct{ rollbackErr error }

func (d *stubDriver) Open(string) (driver.Conn, error) { return &stubConn{d: d}, nil }

type stubConn struct{ d *stubDriver }

func (c *stubConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("prepare not supported")
}
func (c *stubConn) Close() error              { return nil }
func (c *stubConn) Begin() (driver.Tx, error) { return &stubTx{d: c.d}, nil }

type stubTx struct{ d *stubDriver }

func (t *stubTx) Commit() error   { return nil }
func (t *stubTx) Rollback() error { return t.d.rollbackErr }

var driverSeq atomic.Int64

// openStubDB registers a fresh driver name per call so tests never share state.
func openStubDB(t *testing.T, rollbackErr error) *sql.DB {
	t.Helper()

	name := "presencehook-stub-" + strconv.FormatInt(driverSeq.Add(1), 10)
	sql.Register(name, &stubDriver{rollbackErr: rollbackErr})

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

// A rollback that fails for any OTHER reason is a real failure and is logged.
func TestRollbackUnlessDoneLogsGenuineFailure(t *testing.T) {
	db := openStubDB(t, errors.New("connection reset"))
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	log, buf := bufferedLogger()

	presencehook.RollbackUnlessDone(tx, log)

	assert.Contains(t, buf.String(), "rollback failed")
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
