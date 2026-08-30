//go:build integration

package ownership

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type ownershipCaptureFailurePlan struct{}

func (ownershipCaptureFailurePlan) HasWork() bool { return true }

type ownershipCaptureFailureRecheck struct {
	captureErr error
	captures   int
	executes   int
	abandons   []string
}

func (r *ownershipCaptureFailureRecheck) PrepareCapture(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return ownershipCaptureFailurePlan{}, nil
}
func (r *ownershipCaptureFailureRecheck) PrepareCaptureStrict(context.Context, string, []string, *string) (rbac.PresenceRecheckPlan, error) {
	return ownershipCaptureFailurePlan{}, nil
}
func (r *ownershipCaptureFailureRecheck) CaptureVisibility(context.Context, *sql.Tx, rbac.PresenceRecheckPlan) error {
	r.captures++
	return r.captureErr
}
func (r *ownershipCaptureFailureRecheck) Execute(rbac.PresenceRecheckPlan) { r.executes++ }
func (r *ownershipCaptureFailureRecheck) Abandon(_ rbac.PresenceRecheckPlan, cause string) {
	r.abandons = append(r.abandons, cause)
}

func TestWithOwnershipCapture_CaptureFailureRollsBackBeforeWrite(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	wantErr := errors.New("capture failed")
	recheck := &ownershipCaptureFailureRecheck{captureErr: wantErr}
	h := &Handler{db: db, log: logger.New("ownership-test")}
	h.SetPresenceRecheck(recheck)
	writeCalled := false

	_, changed, err := h.withOwnershipCapture(context.Background(), uuid.NewString(), func(context.Context, *sql.Tx) (bool, error) {
		writeCalled = true
		return true, nil
	})
	require.ErrorIs(t, err, wantErr)
	require.False(t, changed)
	require.Equal(t, 1, recheck.captures)
	require.False(t, writeCalled)
	require.Zero(t, recheck.executes)
	require.Empty(t, recheck.abandons)
}

func TestCompleteExpiredTransfer_ZeroRowIsNoOp(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	ownerID := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'capture test server', $2)`, serverID, ownerID)
	require.NoError(t, err)
	recheck := &ownershipCaptureFailureRecheck{}
	h := &Handler{db: db, log: logger.New("ownership-test"), presenceRecheck: recheck}
	changed, err := h.completeExpiredTransfer(context.Background(), expiredTransfer{
		id: uuid.NewString(), serverID: serverID, fromUserID: ownerID.String(), toUserID: uuid.NewString(),
	})
	require.NoError(t, err)
	require.False(t, changed)
	require.Equal(t, 1, recheck.captures)
	require.Zero(t, recheck.executes)
	require.Empty(t, recheck.abandons)
}
