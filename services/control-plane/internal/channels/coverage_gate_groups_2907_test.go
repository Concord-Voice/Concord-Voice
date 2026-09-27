package channels_test

import (
	"context"
	"database/sql"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestGroupMutation_RechecksMembershipAfterPreflight keeps the visibility
// lock held until the request reaches its mutation transaction. The request
// must fail closed after the actor loses membership, without writing a group.
func TestGroupMutation_RechecksMembershipAfterPreflight(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		barrier, probe := holdGroupMutationVisibility(t, ts, serverID)
		response := make(chan int, 1)
		go func() {
			w := ts.DoRequest(http.MethodPost, groupsPath(serverID), map[string]interface{}{"name": "must-not-create"}, testhelpers.AuthHeaders(owner.AccessToken))
			response <- w.Code
		}()
		dbtest.WaitForAdvisoryLockWaiter(t, probe, mustGroupVisibilityKey(t, serverID))
		_, err := probe.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusForbidden, <-response)
		assert.Equal(t, 0, rowCount(t, ts, `SELECT COUNT(*) FROM channel_groups WHERE server_id = $1 AND name = 'must-not-create'`, serverID))
	})

	t.Run("update", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		groupID := createGroup(t, ts, serverID, "unchanged", owner.AccessToken)
		barrier, probe := holdGroupMutationVisibility(t, ts, serverID)
		response := make(chan int, 1)
		go func() {
			w := ts.DoRequest(http.MethodPatch, groupPath(serverID, groupID), map[string]interface{}{"name": "must-not-update"}, testhelpers.AuthHeaders(owner.AccessToken))
			response <- w.Code
		}()
		dbtest.WaitForAdvisoryLockWaiter(t, probe, mustGroupVisibilityKey(t, serverID))
		_, err := probe.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusForbidden, <-response)
		var name string
		require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channel_groups WHERE id = $1`, groupID).Scan(&name))
		assert.Equal(t, "unchanged", name)
	})
}

func holdGroupMutationVisibility(t *testing.T, ts *testhelpers.TestServer, serverID string) (*sql.Tx, *sql.DB) {
	t.Helper()
	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	key := mustGroupVisibilityKey(t, serverID)
	_, err = barrier.Exec(`SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, err)
	t.Cleanup(func() { _ = barrier.Rollback() })
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(2)
	probe.SetMaxIdleConns(2)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { _ = probe.Close() })
	return barrier, probe
}

func mustGroupVisibilityKey(t *testing.T, serverID string) int64 {
	t.Helper()
	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	return key
}
