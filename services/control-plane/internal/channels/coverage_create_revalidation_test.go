package channels_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func createChannelRequest(ts *testhelpers.TestServer, user testhelpers.TestUser, serverID, name string, groupID ...string) *httptest.ResponseRecorder {
	body := map[string]interface{}{
		"server_id":    serverID,
		"name":         name,
		"type":         "text",
		"wrapped_keys": map[string]string{user.ID: testhelpers.ValidCiphertext()},
	}
	if len(groupID) == 1 {
		body["group_id"] = groupID[0]
	}
	return ts.DoRequest("POST", pathChannels, body, testhelpers.AuthHeaders(user.AccessToken))
}

// holdServerForCreate parks a request after its preflight and before the
// in-transaction server/member/authority revalidation.
func holdServerForCreate(t *testing.T, ts *testhelpers.TestServer, serverID string, request func() *httptest.ResponseRecorder) (*sql.Tx, *sql.DB, <-chan *httptest.ResponseRecorder) {
	t.Helper()
	barrier, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	_, err = barrier.Exec(`SELECT pg_advisory_xact_lock($1)`, key)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = barrier.Rollback()
	})
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	probe.SetMaxOpenConns(6)
	probe.SetMaxIdleConns(6)
	require.NoError(t, probe.Ping())
	t.Cleanup(func() { _ = probe.Close() })
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() { response <- request() }()
	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
	return barrier, probe, response
}

func TestCreateChannel_RevalidatesServerStateAfterPreflight(t *testing.T) {
	t.Run("server disappeared", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		barrier, probe, response := holdServerForCreate(t, ts, serverID, func() *httptest.ResponseRecorder {
			return createChannelRequest(ts, owner, serverID, "server-gone")
		})
		_, err := probe.Exec(`DELETE FROM servers WHERE id = $1`, serverID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusNotFound, (<-response).Code)
	})

	t.Run("membership snapshot changed", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		member := ts.CreateTestUser(t, "revalidationmember")
		ts.AddMemberToServer(t, serverID, member.ID, roleMember)
		barrier, probe, response := holdServerForCreate(t, ts, serverID, func() *httptest.ResponseRecorder {
			return createChannelRequest(ts, owner, serverID, "membership-changed")
		})
		_, err := probe.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusConflict, (<-response).Code)
	})

	t.Run("live authority lost", func(t *testing.T) {
		ts, _, serverID := setupWithServer(t)
		actor := ts.CreateTestUser(t, "create-revalidation-actor")
		ts.AddMemberToServer(t, serverID, actor.ID, roleMember)
		roleID := ts.CreateTestRole(t, serverID, "channel-creator", 1, int64(rbac.PermManageChannels))
		ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
		barrier, probe, response := holdServerForCreate(t, ts, serverID, func() *httptest.ResponseRecorder {
			return createChannelRequest(ts, actor, serverID, "authority-lost")
		})
		_, err := probe.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, actor.ID, roleID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusForbidden, (<-response).Code)
	})

	t.Run("target group disappeared", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		groupID := createGroup(t, ts, serverID, "disappearing-group", owner.AccessToken)
		barrier, probe, response := holdServerForCreate(t, ts, serverID, func() *httptest.ResponseRecorder {
			return createChannelRequest(ts, owner, serverID, "group-gone", groupID)
		})
		_, err := probe.Exec(`DELETE FROM channel_groups WHERE id = $1`, groupID)
		require.NoError(t, err)
		require.NoError(t, barrier.Commit())
		assert.Equal(t, http.StatusBadRequest, (<-response).Code)
	})
}

func TestGetPendingKeyRequests_DMPendingRequiresCurrentUnblockedConversation(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "dm-pending-caller")
	requester := ts.CreateTestUser(t, "dm-pending-requester")
	conversationID := ts.CreateDMConversation(t, caller.ID, requester.ID)
	ts.SeedDMKey(t, conversationID, caller.ID, 1)
	_, err := ts.DB.Exec(`INSERT INTO dm_pending_key_requests (conversation_id, user_id) VALUES ($1, $2)`, conversationID, requester.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := testhelpers.JSONField[[]interface{}](t, body, "pending_requests")
	require.Len(t, requests, 1)

	ts.CreateFriendship(t, caller.ID, requester.ID, "blocked")
	w = ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	requests = testhelpers.JSONField[[]interface{}](t, body, "pending_requests")
	assert.Empty(t, requests, "blocked DM conversations must not expose pending requests")
}

func TestGetPendingKeyRequests_DMPendingAvailabilityErrorFailsRequest(t *testing.T) {
	ts := setupTS(t)
	caller := ts.CreateTestUser(t, "dm-pending-availability-caller")
	requester := ts.CreateTestUser(t, "dm-pending-availability-requester")
	conversationID := ts.CreateDMConversation(t, caller.ID, requester.ID)
	ts.SeedDMKey(t, conversationID, caller.ID, 1)
	_, err := ts.DB.Exec(`INSERT INTO dm_pending_key_requests (conversation_id, user_id) VALUES ($1, $2)`, conversationID, requester.ID)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`ALTER TABLE dm_conversations RENAME TO dm_conversations_hidden_for_pending_availability_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_conversations_hidden_for_pending_availability_test RENAME TO dm_conversations`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_conversations rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(caller.AccessToken))
	assert.Equal(t, http.StatusInternalServerError, w.Code, "DM availability errors must not return a truncated queue")
}
