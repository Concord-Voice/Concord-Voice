package channels_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/channels"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	_ "github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStaleAuthority_ChannelMutationsRejectDemotedActor(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *testhelpers.TestServer, string) (gin.Params, any, func())
		call  func(*channels.Handler, *gin.Context)
	}{
		{
			name: "reorder channels",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				channelID := ts.CreateTestChannel(t, serverID, "stale-reorder")
				var position int
				require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, channelID).Scan(&position))
				return gin.Params{{Key: "id", Value: serverID}}, channels.ReorderChannelsRequest{Channels: []channels.ChannelPosition{{ChannelID: channelID, Position: position + 10}}}, func() {
					var actual int
					require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, channelID).Scan(&actual))
					assert.Equal(t, position, actual)
				}
			},
			call: func(h *channels.Handler, c *gin.Context) { h.ReorderChannels(c) },
		},
		{
			name: "update channel",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				channelID := ts.CreateTestChannel(t, serverID, "stale-update")
				var name string
				require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&name))
				return gin.Params{{Key: "id", Value: channelID}}, channels.UpdateChannelRequest{Name: "updated-channel", Type: "text"}, func() {
					var actual string
					require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channels WHERE id = $1`, channelID).Scan(&actual))
					assert.Equal(t, name, actual)
				}
			},
			call: func(h *channels.Handler, c *gin.Context) { h.UpdateChannel(c) },
		},
		{
			name: "delete channel group",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				groupID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'stale-delete-group', 0)`, groupID, serverID)
				require.NoError(t, err)
				return gin.Params{{Key: "group_id", Value: groupID}}, nil, func() {
					var count int
					require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_groups WHERE id = $1`, groupID).Scan(&count))
					assert.Equal(t, 1, count)
				}
			},
			call: func(h *channels.Handler, c *gin.Context) { h.DeleteChannelGroup(c) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts, actorID, serverID, roleID, h := newStaleChannelMutationActor(t)
			params, body, assertUnchanged := test.setup(t, ts, serverID)
			demotionTx := holdStaleChannelMutationDemotion(t, ts, serverID, actorID, roleID)
			completed := invokeStaleChannelMutation(t, actorID, params, body, test.call, h)
			require.NoError(t, demotionTx.Commit())
			w := <-completed
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assertUnchanged()
		})
	}
}

func newStaleChannelMutationActor(t *testing.T) (*testhelpers.TestServer, string, string, string, *channels.Handler) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale-channel-mutation-owner")
	actor := ts.CreateTestUser(t, "stale-channel-mutation-actor")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale channel mutation authority")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "channel-mutation-manager", 1, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	_, err := resolver.GetEffectivePermissions(context.Background(), serverID, actor.ID, "")
	require.NoError(t, err)
	log := logger.New("test")
	authority := rbac.NewHandler(
		ts.DB, log, ts.Redis, ts.Hub, resolver, cache, rbac.NewAuditWriter(ts.DB, log),
	)
	h := channels.NewHandler(ts.DB, log, ts.Hub, resolver, ts.Redis)
	h.SetAuthorityHandler(authority)
	return ts, actor.ID, serverID, roleID, h
}

func holdStaleChannelMutationDemotion(t *testing.T, ts *testhelpers.TestServer, serverID, actorID, roleID string) *sql.Tx {
	t.Helper()
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, actorID, roleID)
	require.NoError(t, err)
	return tx
}

func invokeStaleChannelMutation(t *testing.T, actorID string, params gin.Params, body any, handler func(*channels.Handler, *gin.Context), h *channels.Handler) <-chan *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = params
	c.Set("user_id", actorID)
	c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		handler(h, c)
		completed <- w
	}()
	probe, err := sql.Open("postgres", dbtest.DatabaseURL())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, probe.Close()) })
	var serverID string
	require.NoError(t, probe.QueryRow(`SELECT server_id FROM server_members WHERE user_id = $1 LIMIT 1`, actorID).Scan(&serverID))
	key, err := rbac.ServerVisibilityCaptureAdvisoryKey(serverID)
	require.NoError(t, err)
	dbtest.WaitForAdvisoryLockWaiter(t, probe, key)
	return completed
}
