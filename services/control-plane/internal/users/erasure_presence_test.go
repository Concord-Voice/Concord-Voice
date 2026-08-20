package users_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencecapture"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// erasureCapture records what the erasure path hands the capture, and can be
// told to fail so the fail-closed contract is exercised on a REAL read against a
// REAL transaction rather than a fabricated plan.
type erasureCapture struct {
	subjects  []presencecapture.Subject
	completed int
	abandoned []presencecapture.Cause
	failWith  error
}

func (e *erasureCapture) WithGatedTx(
	context.Context, presencecapture.Subject, func(*sql.Tx) error,
) error {
	// The erasure path must NEVER reach this: it already holds this user's
	// sender gate through presenceHistory.WithSender, and taking it again would
	// self-deadlock on a buffered-1 channel with no timeout and no detector.
	panic("erasure must not call WithGatedTx; it is already gated")
}

func (e *erasureCapture) CaptureInTx(
	_ context.Context, tx *sql.Tx, subject presencecapture.Subject,
) (presencecapture.Plan, error) {
	e.subjects = append(e.subjects, subject)
	if e.failWith != nil {
		// A real read on the caller's real transaction, so the failure is the
		// genuine article rather than an inert branch.
		var scratch int
		_ = tx.QueryRow(`SELECT 1`).Scan(&scratch)
		return nil, e.failWith
	}
	return nil, nil
}

func (e *erasureCapture) Complete(_ context.Context, tx *sql.Tx, _ presencecapture.Plan) error {
	e.completed++
	return tx.Commit()
}

func (e *erasureCapture) Abandon(_ presencecapture.Plan, cause presencecapture.Cause) {
	e.abandoned = append(e.abandoned, cause)
}

// The erasure capture must run under the user-row lock and name the erased
// principal. Getting the principal wrong reconciles somebody else's audience.
func TestErasureCapturesTheErasedPrincipal(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasecapture1")

	capture := &erasureCapture{}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetGraphPresenceCapture(capture)

	require.NoError(t, svc.DeleteAccount(context.Background(), user.ID))

	require.Len(t, capture.subjects, 1, "exactly one capture per erasure")
	require.Equal(t, user.ID, capture.subjects[0].Principal.String(),
		"the principal is the erased user")
	require.Equal(t, presencecapture.FailClosedBlockWrite, capture.subjects[0].FailPosture,
		"erasure fails closed: an unreconcilable audience must not be erased past")
	require.Equal(t, 1, capture.completed,
		"Complete owns the commit; a bare tx.Commit would leave the plan undispatched")
}

// AC-11. A capture failure must leave the users row intact. The erasure path is
// the one place where failing open would destroy the very rows needed to clear.
func TestErasureFailsClosedOnCaptureFailure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "erasefailclosed1")

	capture := &erasureCapture{failWith: errors.New("forced capture failure")}
	svc := users.NewAccountService(ts.DB, logger.New("test"))
	svc.SetGraphPresenceCapture(capture)

	err := svc.DeleteAccount(context.Background(), user.ID)
	require.Error(t, err, "a capture failure must fail the erasure")

	var rows int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, user.ID).Scan(&rows))
	require.Equal(t, 1, rows, "the account must survive a capture failure")
	require.Zero(t, capture.completed, "nothing may commit on the failed path")
}

// The subject constant is exported precisely so publisher and subscriber cannot
// drift. If this ever changes, the voice subscriber stops listening and the
// cross-replica clear silently stops working with every test still green.
func TestErasureClearSubjectIsStable(t *testing.T) {
	require.Equal(t, "presence.erasure.cleared", users.NATSSubjectPresenceErasureCleared)
	require.NotEqual(t, uuid.Nil.String(), users.NATSSubjectPresenceErasureCleared)
}
