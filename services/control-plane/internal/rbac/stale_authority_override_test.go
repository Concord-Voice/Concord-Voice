package rbac_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestUpsertChannelOverride_DemotedActorRejectedByTransactionalAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale-channel-owner")
	actor := ts.CreateTestUser(t, "stale-channel-actor")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale channel authority")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "channel-manager", 1, int64(rbac.PermManageChannels|rbac.PermSendMessages))
	ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
	channelID := ts.CreateTestChannel(t, serverID, "stale-channel")

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	_, err := resolver.GetEffectivePermissions(context.Background(), serverID, actor.ID, "")
	require.NoError(t, err) // poison the cache with the pre-demotion authority
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, actor.ID, roleID)
	require.NoError(t, err)

	completed := invokeOverrideHandler(t, actor.ID, channelID, rbac.UpsertOverrideRequest{TargetType: "user", TargetID: actor.ID, Allow: int64(rbac.PermSendMessages)}, h.UpsertChannelOverride)
	require.NoError(t, tx.Commit())
	w := <-completed
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count)
}

func TestUpsertChannelOverrideRejectsStaleCredentialEpoch(t *testing.T) {
	ts, owner, member, serverID := setupOwnerAndMember(t)
	channelID := ts.CreateTestChannel(t, serverID, "stale-epoch-channel-override")

	staleToken := ts.SimulateStaleEpochWindow(t, owner.ID)
	w := ts.DoRequest(http.MethodPut, channelOverridesPath(channelID), rbac.UpsertOverrideRequest{
		TargetType: "user",
		TargetID:   member.ID,
		Allow:      int64(rbac.PermSendMessages),
	}, testhelpers.AuthHeaders(staleToken))
	require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE channel_id = $1`, channelID).Scan(&count))
	assert.Zero(t, count, "stale override write must not persist authority")
}

func TestUpsertCategoryOverride_DemotedActorRejectedByTransactionalAuthority(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale-category-owner")
	actor := ts.CreateTestUser(t, "stale-category-actor")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale category authority")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "category-manager", 1, int64(rbac.PermManageChannels|rbac.PermSendMessages))
	ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
	var categoryID string
	require.NoError(t, ts.DB.QueryRow(`INSERT INTO channel_groups (id, server_id, name, position) VALUES (gen_random_uuid(), $1, 'stale-category', 0) RETURNING id`, serverID).Scan(&categoryID))

	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	_, err := resolver.GetEffectivePermissions(context.Background(), serverID, actor.ID, "")
	require.NoError(t, err)
	h := rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	defer tx.Rollback() //nolint:errcheck
	_, err = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, actor.ID, roleID)
	require.NoError(t, err)

	completed := invokeOverrideHandler(t, actor.ID, categoryID, rbac.UpsertOverrideRequest{TargetType: "user", TargetID: actor.ID, Allow: int64(rbac.PermSendMessages)}, h.UpsertCategoryOverride)
	require.NoError(t, tx.Commit())
	w := <-completed
	assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM category_permission_overrides WHERE category_id = $1`, categoryID).Scan(&count))
	assert.Zero(t, count)
}

func invokeOverrideHandler(t *testing.T, actorID, id string, body rbac.UpsertOverrideRequest, handler func(*gin.Context)) <-chan *httptest.ResponseRecorder {
	t.Helper()
	payload, err := json.Marshal(body)
	require.NoError(t, err)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Params = gin.Params{{Key: "id", Value: id}}
	c.Set("user_id", actorID)
	c.Request = httptest.NewRequest(http.MethodPut, "/", bytes.NewReader(payload))
	c.Request.Header.Set("Content-Type", "application/json")
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		handler(c)
		completed <- w
	}()
	return completed
}

func TestStaleAuthority_RBACMutationHandlersRejectDemotedActor(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*testing.T, *testhelpers.TestServer, string) (gin.Params, any, func())
		call  func(*rbac.Handler, *gin.Context)
	}{
		{
			name: "delete channel override",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				channelID := ts.CreateTestChannel(t, serverID, "stale-delete-override")
				overrideID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'user', $3, 0, 0)`, overrideID, channelID, uuid.NewString())
				require.NoError(t, err)
				return gin.Params{{Key: "id", Value: channelID}, {Key: "override_id", Value: overrideID}}, nil, func() {
					var count int
					require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE id = $1`, overrideID).Scan(&count))
					assert.Equal(t, 1, count)
				}
			},
			call: func(h *rbac.Handler, c *gin.Context) { h.DeleteChannelOverride(c) },
		},
		{
			name: "delete category override",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				categoryID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'stale-category', 0)`, categoryID, serverID)
				require.NoError(t, err)
				overrideID := uuid.NewString()
				_, err = ts.DB.Exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'user', $3, 0, 0)`, overrideID, categoryID, uuid.NewString())
				require.NoError(t, err)
				return gin.Params{{Key: "id", Value: categoryID}, {Key: "override_id", Value: overrideID}}, nil, func() {
					var count int
					require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM category_permission_overrides WHERE id = $1`, overrideID).Scan(&count))
					assert.Equal(t, 1, count)
				}
			},
			call: func(h *rbac.Handler, c *gin.Context) { h.DeleteCategoryOverride(c) },
		},
		{
			name: "enable channel permission sync",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				categoryID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'stale-enable', 0)`, categoryID, serverID)
				require.NoError(t, err)
				channelID := ts.CreateTestChannel(t, serverID, "stale-enable-sync")
				_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1 WHERE id = $2`, categoryID, channelID)
				require.NoError(t, err)
				return gin.Params{{Key: "id", Value: channelID}}, map[string]bool{"sync_permissions": true}, func() {
					var synced bool
					require.NoError(t, ts.DB.QueryRow(`SELECT sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&synced))
					assert.False(t, synced)
				}
			},
			call: func(h *rbac.Handler, c *gin.Context) { h.SetChannelPermissionSync(c) },
		},
		{
			name: "disable channel permission sync",
			setup: func(t *testing.T, ts *testhelpers.TestServer, serverID string) (gin.Params, any, func()) {
				categoryID := uuid.NewString()
				_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'stale-disable', 0)`, categoryID, serverID)
				require.NoError(t, err)
				channelID := ts.CreateTestChannel(t, serverID, "stale-disable-sync")
				_, err = ts.DB.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, categoryID, channelID)
				require.NoError(t, err)
				return gin.Params{{Key: "id", Value: channelID}}, map[string]bool{"sync_permissions": false}, func() {
					var synced bool
					require.NoError(t, ts.DB.QueryRow(`SELECT sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&synced))
					assert.True(t, synced)
				}
			},
			call: func(h *rbac.Handler, c *gin.Context) { h.SetChannelPermissionSync(c) },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts, actorID, serverID, roleID, h := newStaleRBACMutationActor(t)
			params, body, assertUnchanged := test.setup(t, ts, serverID)
			demotionTx := holdStaleRBACMutationDemotion(t, ts, serverID, actorID, roleID)
			completed := invokeStaleRBACMutation(t, actorID, params, body, test.call, h)
			require.NoError(t, demotionTx.Commit())
			w := <-completed
			assert.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			assertUnchanged()
		})
	}
}

func newStaleRBACMutationActor(t *testing.T) (*testhelpers.TestServer, string, string, string, *rbac.Handler) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "stale-mutation-owner")
	actor := ts.CreateTestUser(t, "stale-mutation-actor")
	serverID := ts.CreateTestServer(t, owner.ID, "Stale mutation authority")
	ts.AddMemberToServer(t, serverID, actor.ID, "member")
	roleID := ts.CreateTestRole(t, serverID, "mutation-manager", 1, int64(rbac.PermManageChannels))
	ts.AssignRoleToUser(t, serverID, actor.ID, roleID)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
	_, err := resolver.GetEffectivePermissions(context.Background(), serverID, actor.ID, "")
	require.NoError(t, err)
	return ts, actor.ID, serverID, roleID, rbac.NewHandler(ts.DB, logger.New("test"), ts.Redis, websocket.NewHub(ts.DB, ts.Redis), resolver, cache, nil)
}

func holdStaleRBACMutationDemotion(t *testing.T, ts *testhelpers.TestServer, serverID, actorID, roleID string) *sql.Tx {
	t.Helper()
	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	require.NoError(t, rbac.LockServerVisibilityCapture(context.Background(), tx, serverID))
	_, err = tx.Exec(`DELETE FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, actorID, roleID)
	require.NoError(t, err)
	return tx
}

func invokeStaleRBACMutation(t *testing.T, actorID string, params gin.Params, body any, handler func(*rbac.Handler, *gin.Context), h *rbac.Handler) <-chan *httptest.ResponseRecorder {
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
	return completed
}

// TestRBACAuthorityWritersRejectStaleCredentialEpoch keeps the epoch fence on
// every RBAC writer that can change role or permission authority. The helper
// admits the request through the stale Redis middleware window, so a 401 must
// come from the writer's transaction guard rather than token middleware.
func TestRBACAuthorityWritersRejectStaleCredentialEpoch(t *testing.T) {
	tests := []struct {
		name string
		call func(*testing.T, *testhelpers.TestServer, string, string, string)
	}{
		{name: "create role", call: func(t *testing.T, ts *testhelpers.TestServer, _ string, serverID, token string) {
			w := ts.DoRequest(http.MethodPost, rolesPath(serverID), map[string]any{"name": "stale-role", "permissions": "2048"}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}},
		{name: "edit role", call: func(t *testing.T, ts *testhelpers.TestServer, _ string, serverID, token string) {
			roleID := ts.CreateTestRole(t, serverID, "stale-edit-role", 1, int64(rbac.PermSendMessages))
			w := ts.DoRequest(http.MethodPatch, rolePath(serverID, roleID), map[string]any{"name": "changed"}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			var name string
			require.NoError(t, ts.DB.QueryRow(`SELECT name FROM roles WHERE id = $1`, roleID).Scan(&name))
			require.Equal(t, "stale-edit-role", name)
		}},
		{name: "reorder roles", call: func(t *testing.T, ts *testhelpers.TestServer, _ string, serverID, token string) {
			roleID := ts.CreateTestRole(t, serverID, "stale-reorder-role", 1, int64(rbac.PermSendMessages))
			w := ts.DoRequest(http.MethodPatch, reorderRolesPath(serverID), map[string]any{"role_ids": []string{roleID}}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}},
		{name: "assign role", call: func(t *testing.T, ts *testhelpers.TestServer, _ string, serverID, token string) {
			member := ts.CreateTestUser(t, "stale-assignment-target")
			ts.AddMemberToServer(t, serverID, member.ID, "member")
			roleID := ts.CreateTestRole(t, serverID, "stale-assignment-role", 1, int64(rbac.PermSendMessages))
			w := ts.DoRequest(http.MethodPost, assignRolePath(serverID, member.ID), map[string]any{"role_id": roleID}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}},
		{name: "category override", call: func(t *testing.T, ts *testhelpers.TestServer, ownerID, serverID, token string) {
			categoryID := createStaleRBACCategory(t, ts, serverID)
			w := ts.DoRequest(http.MethodPut, "/api/v1/categories/"+categoryID+"/overrides", rbac.UpsertOverrideRequest{TargetType: "user", TargetID: ownerID, Allow: int64(rbac.PermSendMessages)}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}},
		{name: "channel override delete", call: func(t *testing.T, ts *testhelpers.TestServer, ownerID, serverID, token string) {
			channelID := ts.CreateTestChannel(t, serverID, "stale-delete-channel-override")
			overrideID := uuid.NewString()
			_, err := ts.DB.Exec(`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'user', $3, 0, 0)`, overrideID, channelID, ownerID)
			require.NoError(t, err)
			w := ts.DoRequest(http.MethodDelete, "/api/v1/channels/"+channelID+"/overrides/"+overrideID, nil, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			var count int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_permission_overrides WHERE id = $1`, overrideID).Scan(&count))
			require.Equal(t, 1, count)
		}},
		{name: "category override delete", call: func(t *testing.T, ts *testhelpers.TestServer, ownerID, serverID, token string) {
			categoryID := createStaleRBACCategory(t, ts, serverID)
			overrideID := uuid.NewString()
			_, err := ts.DB.Exec(`INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny) VALUES ($1, $2, 'user', $3, 0, 0)`, overrideID, categoryID, ownerID)
			require.NoError(t, err)
			w := ts.DoRequest(http.MethodDelete, "/api/v1/categories/"+categoryID+"/overrides/"+overrideID, nil, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
			var count int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM category_permission_overrides WHERE id = $1`, overrideID).Scan(&count))
			require.Equal(t, 1, count)
		}},
		{name: "channel permission sync", call: func(t *testing.T, ts *testhelpers.TestServer, _ string, serverID, token string) {
			categoryID := createStaleRBACCategory(t, ts, serverID)
			channelID := ts.CreateTestChannel(t, serverID, "stale-sync-channel")
			_, err := ts.DB.Exec(`UPDATE channels SET group_id = $1 WHERE id = $2`, categoryID, channelID)
			require.NoError(t, err)
			w := ts.DoRequest(http.MethodPut, "/api/v1/channels/"+channelID+"/permission-sync", map[string]bool{"sync_permissions": true}, testhelpers.AuthHeaders(token))
			require.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts, owner, _, serverID := setupOwnerAndMember(t)
			test.call(t, ts, owner.ID, serverID, ts.SimulateStaleEpochWindow(t, owner.ID))
		})
	}
}

// TestRoleWritersRecheckRoutePermissionAfterMiddlewareCacheStales proves that
// the writer guard does not trust a permission admitted by stale middleware.
// The actor keeps hierarchy over the target role, so a 403 can only come from
// the fresh route-permission check inside the writer transaction.
func TestRoleWritersRecheckRoutePermissionAfterMiddlewareCacheStales(t *testing.T) {
	tests := []struct {
		name string
		call func(*testing.T, *testhelpers.TestServer, string, string, string, string, string)
	}{
		{name: "update role", call: func(t *testing.T, ts *testhelpers.TestServer, serverID, _, actorToken, targetRoleID, _ string) {
			w := ts.DoRequest(http.MethodPatch, rolePath(serverID, targetRoleID), map[string]any{"name": "must-not-change"}, testhelpers.AuthHeaders(actorToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var name string
			require.NoError(t, ts.DB.QueryRow(`SELECT name FROM roles WHERE id = $1`, targetRoleID).Scan(&name))
			require.NotEqual(t, "must-not-change", name)
		}},
		{name: "delete role", call: func(t *testing.T, ts *testhelpers.TestServer, serverID, _, actorToken, targetRoleID, _ string) {
			w := ts.DoRequest(http.MethodDelete, rolePath(serverID, targetRoleID), nil, testhelpers.AuthHeaders(actorToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var exists bool
			require.NoError(t, ts.DB.QueryRow(`SELECT EXISTS(SELECT 1 FROM roles WHERE id = $1)`, targetRoleID).Scan(&exists))
			require.True(t, exists)
		}},
		{name: "reorder roles", call: func(t *testing.T, ts *testhelpers.TestServer, serverID, _, actorToken, targetRoleID, _ string) {
			w := ts.DoRequest(http.MethodPatch, reorderRolesPath(serverID), map[string]any{"role_ids": []string{targetRoleID}}, testhelpers.AuthHeaders(actorToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
		}},
		{name: "assign role", call: func(t *testing.T, ts *testhelpers.TestServer, serverID, _, actorToken, targetRoleID, targetUserID string) {
			w := ts.DoRequest(http.MethodPost, assignRolePath(serverID, targetUserID), map[string]any{"role_id": targetRoleID}, testhelpers.AuthHeaders(actorToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var count int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, targetUserID, targetRoleID).Scan(&count))
			require.Zero(t, count)
		}},
		{name: "unassign role", call: func(t *testing.T, ts *testhelpers.TestServer, serverID, _, actorToken, targetRoleID, targetUserID string) {
			w := ts.DoRequest(http.MethodDelete, unassignRolePath(serverID, targetUserID, targetRoleID), nil, testhelpers.AuthHeaders(actorToken))
			require.Equal(t, http.StatusForbidden, w.Code, w.Body.String())
			var count int
			require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM member_roles WHERE server_id = $1 AND user_id = $2 AND role_id = $3`, serverID, targetUserID, targetRoleID).Scan(&count))
			require.Equal(t, 1, count)
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ts := testhelpers.SetupTestServer(t)
			owner := ts.CreateTestUser(t, "fresh-route-owner")
			actor := ts.CreateTestUser(t, "fresh-route-actor")
			target := ts.CreateTestUser(t, "fresh-route-target")
			serverID := ts.CreateTestServer(t, owner.ID, "Fresh route permission")
			ts.AddMemberToServer(t, serverID, actor.ID, "member")
			ts.AddMemberToServer(t, serverID, target.ID, "member")
			actorRoleID := ts.CreateTestRole(t, serverID, "route-manager", 5, int64(rbac.PermManageRoles|rbac.PermManageRolesAssign))
			ts.AssignRoleToUser(t, serverID, actor.ID, actorRoleID)
			targetRoleID := ts.CreateTestRole(t, serverID, "lower-role", 1, int64(rbac.PermSendMessages))
			if test.name == "unassign role" {
				ts.AssignRoleToUser(t, serverID, target.ID, targetRoleID)
			}

			cache := rbac.NewPermissionCache(ts.Redis)
			resolver := rbac.NewResolver(ts.DB, cache, logger.New("test"))
			_, err := resolver.GetEffectivePermissions(context.Background(), serverID, actor.ID, "")
			require.NoError(t, err)
			_, err = ts.DB.Exec(`UPDATE roles SET permissions = 0 WHERE id = $1`, actorRoleID)
			require.NoError(t, err)

			test.call(t, ts, serverID, actor.ID, actor.AccessToken, targetRoleID, target.ID)
		})
	}
}

func createStaleRBACCategory(t *testing.T, ts *testhelpers.TestServer, serverID string) string {
	t.Helper()
	categoryID := uuid.NewString()
	_, err := ts.DB.Exec(`INSERT INTO channel_groups (id, server_id, name, position) VALUES ($1, $2, 'stale-category', 0)`, categoryID, serverID)
	require.NoError(t, err)
	return categoryID
}
