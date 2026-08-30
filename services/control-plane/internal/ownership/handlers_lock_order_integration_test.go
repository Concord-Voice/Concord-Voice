//go:build integration

package ownership_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestExpiredOwnershipVsDeleteServerHasNoDeadlock drives actual scheduled
// completion while deletion owns the server row. pg_stat_activity is used only
// as a state barrier proving completion reached its blocked server-row SELECT.
func TestExpiredOwnershipVsDeleteServerHasNoDeadlock(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "expiry_lock_owner")
	target := ts.CreateTestUser(t, "expiry_lock_target")
	serverID := ts.CreateTestServer(t, owner.ID, "expiry lock server")
	ts.AddMemberToServer(t, serverID, target.ID, "member")
	transferID := uuid.NewString()
	_, err := ts.DB.ExecContext(context.Background(), `INSERT INTO ownership_transfers
		(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
		VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() - INTERVAL '1 second')`,
		transferID, serverID, owner.ID, target.ID, uuid.NewString())
	require.NoError(t, err)
	logs := ts.CaptureLogs(t)

	deleteTx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := deleteTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback server deletion transaction: %v", rollbackErr)
		}
	})
	var lockedOwner string
	require.NoError(t, deleteTx.QueryRowContext(context.Background(), `SELECT owner_id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedOwner))
	completionDone := make(chan struct{})
	go func() { ts.CompleteExpiredTransfers(context.Background()); close(completionDone) }()
	require.Eventually(t, func() bool {
		var waiting int
		err := ts.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM pg_stat_activity WHERE datname = current_database() AND wait_event_type = 'Lock' AND query ILIKE '%SELECT id FROM servers%FOR UPDATE%'`).Scan(&waiting)
		return err == nil && waiting > 0
	}, 2*time.Second, 10*time.Millisecond)
	_, deleteErr := deleteTx.ExecContext(context.Background(), `DELETE FROM servers WHERE id = $1`, serverID)
	require.NoError(t, deleteErr)
	require.NoError(t, deleteTx.Commit())
	select {
	case <-completionDone:
	case <-time.After(3 * time.Second):
		t.Fatal("scheduled completion did not return")
	}
	require.NotContains(t, logs.String(), "ownership_expiry_completion")
	var status string
	err = ts.DB.QueryRowContext(context.Background(), `SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status)
	// Deletion wins; its FK cascade removes the transfer row and completion has
	// no changed row on which to run post-commit reconciliation.
	require.ErrorIs(t, err, sql.ErrNoRows)
	var serverCount int
	require.NoError(t, ts.DB.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM servers WHERE id = $1`, serverID).Scan(&serverCount))
	require.Zero(t, serverCount)
}
