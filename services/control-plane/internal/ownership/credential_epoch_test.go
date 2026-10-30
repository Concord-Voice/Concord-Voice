package ownership_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Rotate after actual family admission commits. Each interactive ownership
// mutation must reach its transactional epoch guard before changing state.
func TestOwnershipMutationsRejectResetFirstCredentialEpoch(t *testing.T) {
	t.Run("initiate", func(t *testing.T) {
		ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-init-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-init-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Initiate")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, owner.ID, nil, nil)
		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		boundary.RequireRotated(t)
		seen, guardErr := boundary.Hook.Report()
		require.NoError(t, guardErr)
		require.Equal(t, 1, seen, "ownership GuardTx must run after family admission")

		var pending int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&pending))
		assert.Zero(t, pending, "stale token must not create an ownership transfer")
	})

	t.Run("confirm", func(t *testing.T) {
		ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-confirm-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-confirm-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Confirm")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, owner.ID, nil, nil)
		w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		boundary.RequireRotated(t)
		seen, guardErr := boundary.Hook.Report()
		require.NoError(t, guardErr)
		require.Equal(t, 1, seen, "ownership GuardTx must run after family admission")

		var currentOwner, status string
		require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwner))
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
		assert.Equal(t, owner.ID, currentOwner, "stale token must not transfer ownership")
		assert.Equal(t, keyPending, status, "stale token must leave the pending transfer unchanged")
	})

	t.Run("cancel", func(t *testing.T) {
		ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-cancel-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-cancel-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Cancel")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, owner.ID, nil, nil)
		w = ts.DoRequest(http.MethodDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		boundary.RequireRotated(t)
		seen, guardErr := boundary.Hook.Report()
		require.NoError(t, guardErr)
		require.Equal(t, 1, seen, "ownership GuardTx must run after family admission")

		var status string
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
		assert.Equal(t, keyPending, status, "stale token must not cancel the pending transfer")
	})

	t.Run("reverse", func(t *testing.T) {
		ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-reverse-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-reverse-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Reverse")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
		w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, w.Body.String())

		var reversalToken string
		require.NoError(t, ts.DB.QueryRow(`SELECT reversal_token FROM ownership_transfers WHERE server_id = $1 AND status = 'completed'`, serverID).Scan(&reversalToken))
		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, owner.ID, nil, nil)
		w = ts.DoRequest(http.MethodPost, pathOwnershipReverse+reversalToken, map[string]interface{}{
			keyPassword: testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		boundary.RequireRotated(t)
		seen, guardErr := boundary.Hook.Report()
		require.NoError(t, guardErr)
		require.Equal(t, 1, seen, "ownership GuardTx must run after family admission")

		var currentOwner, status string
		require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwner))
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
		assert.Equal(t, target.ID, currentOwner, "stale token must not reverse ownership")
		assert.Equal(t, "completed", status, "stale token must leave the completed transfer unchanged")
	})
}

// A mutation which acquired the actor lock before a reset must finish before
// that reset advances the epoch. Holding the server row makes the ordering
// observable: the transfer is waiting after its user lock, while the reset is
// blocked by that same lock.
func TestInitiateTransferMutationFirstSerializesCredentialReset(t *testing.T) {
	ts, boundary := testhelpers.SetupStaleBoundaryServer(t)
	owner := ts.CreateTestUser(t, "ownership-epoch-mutation-owner")
	target := ts.CreateTestUser(t, "ownership-epoch-mutation-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Mutation First")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	require.NoError(t, boundary.ArmControl(owner.AccessToken))
	boundary.Hook.ArmArg([]string{`SELECT credential_epoch FROM users WHERE id = $1 FOR SHARE`}, owner.ID, nil, nil)

	serverLock, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() {
		if rollbackErr := serverLock.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("release server lock: %v", rollbackErr)
		}
	}()
	var lockedServerID string
	require.NoError(t, serverLock.QueryRow(`SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedServerID))

	transferDone := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		transferDone <- w.Code
	}()

	// The transfer holds the user lock and waits only on the server lock.
	require.Eventually(t, func() bool {
		var waiting bool
		err := ts.DB.QueryRow(`
			SELECT EXISTS(
				SELECT 1
				FROM pg_stat_activity activity
				JOIN pg_locks locks ON locks.pid = activity.pid
				JOIN pg_class relation ON relation.oid = locks.relation
				WHERE relation.relname = 'servers'
					AND locks.mode = 'RowShareLock'
					AND activity.wait_event_type = 'Lock'
			)`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "transfer did not reach its server lock after locking users")

	require.NoError(t, boundary.Check())
	seen, guardErr := boundary.Hook.Report()
	require.NoError(t, guardErr)
	require.Equal(t, 1, seen, "transfer passed GuardTx before the later reset")

	resetDone := make(chan string, 1)
	go func() {
		defer close(resetDone)
		resetDone <- ts.RotateCredentialEpoch(t, owner.ID)
	}()
	select {
	case <-resetDone:
		require.Fail(t, "credential reset bypassed transfer user lock")
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, serverLock.Rollback())
	select {
	case status := <-transferDone:
		require.Equal(t, http.StatusCreated, status)
	case <-time.After(5 * time.Second):
		t.Fatal("transfer did not finish after releasing server lock")
	}
	select {
	case epoch, ok := <-resetDone:
		require.True(t, ok, "public rotation failed before returning an epoch")
		require.Len(t, epoch, 32)
	case <-time.After(6 * time.Second):
		t.Fatal("credential reset did not finish after transfer")
	}

	var transferCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM ownership_transfers WHERE server_id = $1 AND status = 'pending'`, serverID).Scan(&transferCount))
	assert.Equal(t, 1, transferCount, "mutation admitted under E1 must commit before the later reset")
}
