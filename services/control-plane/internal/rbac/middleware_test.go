package rbac_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
)

const pathServersSlash = "/servers/"
const testUserID = "some-user-id"

func init() {
	gin.SetMode(gin.TestMode)
}

// setupMiddlewareTest creates a resolver backed by real DB + Redis and returns the test server.
func setupMiddlewareTest(t *testing.T) (*rbac.Resolver, *testhelpers.TestServer) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	log := logger.New("test")
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, log)
	return resolver, ts
}

// doMiddlewareRequest builds and executes a gin request through a middleware-guarded route.
func doMiddlewareRequest(handler gin.HandlerFunc, userID, serverID string) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/servers/:id/test", func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
	}, handler, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathServersSlash+serverID+"/test", nil)
	router.ServeHTTP(w, req)
	return w
}

// --- RequireMembership Tests ---

func TestRequireMembershipSuccess(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwowner1")
	serverID := ts.CreateTestServer(t, owner.ID, "Middleware Server")

	w := doMiddlewareRequest(rbac.RequireMembership(resolver), owner.ID, serverID)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireMembershipNotMember(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwowner2")
	outsider := ts.CreateTestUser(t, "mwoutsider2")
	serverID := ts.CreateTestServer(t, owner.ID, "Middleware Server 2")

	w := doMiddlewareRequest(rbac.RequireMembership(resolver), outsider.ID, serverID)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequireMembershipEmptyUserID(t *testing.T) {
	resolver, _ := setupMiddlewareTest(t)

	w := doMiddlewareRequest(rbac.RequireMembership(resolver), "", "some-server-id")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireMembershipEmptyServerID(t *testing.T) {
	resolver, _ := setupMiddlewareTest(t)

	// Use a route with empty :id param
	router := gin.New()
	router.GET("/servers/:id/test", func(c *gin.Context) {
		c.Set("user_id", testUserID)
		c.Params = gin.Params{{Key: "id", Value: ""}}
	}, rbac.RequireMembership(resolver), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/servers//test", nil)
	router.ServeHTTP(w, req)
	// Empty server ID should fail (either 401 or 404 depending on routing)
	assert.NotEqual(t, http.StatusOK, w.Code)
}

func TestRequireMembershipInvalidUUID(t *testing.T) {
	resolver, _ := setupMiddlewareTest(t)

	w := doMiddlewareRequest(rbac.RequireMembership(resolver), testUserID, "not-a-uuid")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- RequirePermission Tests ---

func doPermissionRequest(handler gin.HandlerFunc, userID, serverID string) *httptest.ResponseRecorder {
	router := gin.New()
	router.GET("/servers/:id/action", func(c *gin.Context) {
		if userID != "" {
			c.Set("user_id", userID)
		}
	}, handler, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathServersSlash+serverID+"/action", nil)
	router.ServeHTTP(w, req)
	return w
}

func TestRequirePermissionSuccess(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwpowner1")
	member := ts.CreateTestUser(t, "mwpmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "Perm Middleware Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Member has base permissions, including PermViewTextChannels
	handler := rbac.RequirePermission(resolver, rbac.PermViewTextChannels, "")
	w := doPermissionRequest(handler, member.ID, serverID)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequirePermissionDenied(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwpowner2")
	member := ts.CreateTestUser(t, "mwpmember2")
	serverID := ts.CreateTestServer(t, owner.ID, "Perm Middleware Server 2")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Member does NOT have PermManageChannels
	handler := rbac.RequirePermission(resolver, rbac.PermManageChannels, "")
	w := doPermissionRequest(handler, member.ID, serverID)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestRequirePermissionEmptyUserID(t *testing.T) {
	resolver, _ := setupMiddlewareTest(t)

	handler := rbac.RequirePermission(resolver, rbac.PermViewTextChannels, "")
	w := doPermissionRequest(handler, "", "some-server-id")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequirePermissionInvalidServerUUID(t *testing.T) {
	resolver, _ := setupMiddlewareTest(t)

	handler := rbac.RequirePermission(resolver, rbac.PermViewTextChannels, "")
	w := doPermissionRequest(handler, testUserID, "not-a-uuid")
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRequirePermissionOwnerHasAll(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwpowner3")
	serverID := ts.CreateTestServer(t, owner.ID, "Perm Owner Server")

	// Owner should pass even for ManageServer permission
	handler := rbac.RequirePermission(resolver, rbac.PermManageServer, "")
	w := doPermissionRequest(handler, owner.ID, serverID)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequirePermissionWithChannelIDParam(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)

	owner := ts.CreateTestUser(t, "mwpowner4")
	member := ts.CreateTestUser(t, "mwpmember4")
	serverID := ts.CreateTestServer(t, owner.ID, "Channel Perm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "test-chan")

	handler := rbac.RequirePermission(resolver, rbac.PermSendMessages, "channel_id")

	router := gin.New()
	router.GET("/servers/:id/channels/:channel_id/send", func(c *gin.Context) {
		c.Set("user_id", member.ID)
	}, handler, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathServersSlash+serverID+"/channels/"+channelID+"/send", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequirePermissionChannelDeny(t *testing.T) {
	resolver, ts := setupMiddlewareTest(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mwpowner5")
	member := ts.CreateTestUser(t, "mwpmember5")
	serverID := ts.CreateTestServer(t, owner.ID, "Channel Deny Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "restricted-chan")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	if err != nil {
		t.Fatalf("failed to get @all role: %v", err)
	}

	// Deny send messages in channel for @all role
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermSendMessages))

	// Invalidate cache
	cache := rbac.NewPermissionCache(ts.Redis)
	_ = cache.Invalidate(ctx, serverID, member.ID)

	handler := rbac.RequirePermission(resolver, rbac.PermSendMessages, "channel_id")

	router := gin.New()
	router.GET("/servers/:id/channels/:channel_id/send", func(c *gin.Context) {
		c.Set("user_id", member.ID)
	}, handler, func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})

	w := httptest.NewRecorder()
	req := httptest.NewRequest("GET", pathServersSlash+serverID+"/channels/"+channelID+"/send", nil)
	router.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
