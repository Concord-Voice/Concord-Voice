package ownership_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/auth"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An entry-time cache hit can still admit the token issued before a destructive
// credential reset. Each interactive ownership mutation must recheck the
// durable epoch in its write transaction before changing ownership state.
func TestOwnershipMutationsRejectResetFirstCredentialEpoch(t *testing.T) {
	t.Run("initiate", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-init-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-init-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Initiate")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(staleToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

		var pending int
		require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&pending))
		assert.Zero(t, pending, "stale token must not create an ownership transfer")
	})

	t.Run("confirm", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-confirm-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-confirm-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Confirm")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
		w = ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnershipConfirm, nil, testhelpers.AuthHeaders(staleToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

		var currentOwner, status string
		require.NoError(t, ts.DB.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&currentOwner))
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
		assert.Equal(t, owner.ID, currentOwner, "stale token must not transfer ownership")
		assert.Equal(t, keyPending, status, "stale token must leave the pending transfer unchanged")
	})

	t.Run("cancel", func(t *testing.T) {
		ts := setupTS(t)
		owner := ts.CreateTestUser(t, "ownership-epoch-cancel-owner")
		target := ts.CreateTestUser(t, "ownership-epoch-cancel-target")
		serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Cancel")
		ts.AddMemberToServer(t, serverID, target.ID, keyMember)

		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code, w.Body.String())

		staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
		w = ts.DoRequest(http.MethodDelete, pathServersPrefix+serverID+pathTransferOwnership, nil, testhelpers.AuthHeaders(staleToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

		var status string
		require.NoError(t, ts.DB.QueryRow(`SELECT status FROM ownership_transfers WHERE server_id = $1`, serverID).Scan(&status))
		assert.Equal(t, keyPending, status, "stale token must not cancel the pending transfer")
	})

	t.Run("reverse", func(t *testing.T) {
		ts := setupTS(t)
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
		staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
		w = ts.DoRequest(http.MethodPost, pathOwnershipReverse+reversalToken, map[string]interface{}{
			keyPassword: testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(staleToken))
		require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

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
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ownership-epoch-mutation-owner")
	target := ts.CreateTestUser(t, "ownership-epoch-mutation-target")
	serverID := ts.CreateTestServer(t, owner.ID, "Ownership Epoch Mutation First")
	ts.AddMemberToServer(t, serverID, target.ID, keyMember)

	const beforeEpoch = "ownership-epoch-before"
	const afterEpoch = "ownership-epoch-after"
	_, err := ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, beforeEpoch, owner.ID)
	require.NoError(t, err)
	require.NoError(t, ts.Redis.Set(context.Background(), credepoch.Key(owner.ID), "active:"+beforeEpoch, time.Minute).Err())
	token, err := auth.GenerateAccessToken(owner.ID, testhelpers.TestJWTSecret, true, beforeEpoch, "")
	require.NoError(t, err)

	serverLock, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = serverLock.Rollback() }()
	var lockedServerID string
	require.NoError(t, serverLock.QueryRow(`SELECT id FROM servers WHERE id = $1 FOR UPDATE`, serverID).Scan(&lockedServerID))

	transferDone := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathServersPrefix+serverID+pathTransferOwnership, map[string]interface{}{
			keyTargetUserID: target.ID,
			keyPassword:     testhelpers.TestAuthPlaintext,
		}, testhelpers.AuthHeaders(token))
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

	resetDone := make(chan error, 1)
	go func() {
		_, resetErr := ts.DB.Exec(`UPDATE users SET credential_epoch = $1 WHERE id = $2`, afterEpoch, owner.ID)
		resetDone <- resetErr
	}()
	select {
	case resetErr := <-resetDone:
		require.Failf(t, "credential reset bypassed transfer user lock", "reset returned early: %v", resetErr)
	case <-time.After(100 * time.Millisecond):
	}

	require.NoError(t, serverLock.Rollback())
	require.Equal(t, http.StatusCreated, <-transferDone)
	require.NoError(t, <-resetDone)

	var transferCount int
	require.NoError(t, ts.DB.QueryRow(`SELECT count(*) FROM ownership_transfers WHERE server_id = $1 AND status = 'pending'`, serverID).Scan(&transferCount))
	assert.Equal(t, 1, transferCount, "mutation admitted under E1 must commit before the later reset")
}
