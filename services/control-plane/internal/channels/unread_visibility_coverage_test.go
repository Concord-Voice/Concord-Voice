package channels_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/channels"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
)

// brokenResolverChannelsRouter builds a channels.Handler with a working DB but a
// resolver backed by a closed DB, plus a router that injects userID as the
// authenticated principal. It covers the visible-channel / view-permission
// resolution error branches (HTTP 500) in GetChannel / GetUnreadCounts /
// GetServerUnreadStatus that the full-router tests cannot reach (a healthy
// resolver never errors on a valid request).
func brokenResolverChannelsRouter(t *testing.T, ts *testhelpers.TestServer, userID string) *gin.Engine {
	t.Helper()
	h := channels.NewHandler(ts.DB, logger.New("test"), ts.Hub, testhelpers.BrokenResolver(t, ts.Redis), ts.Redis)
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", userID); c.Next() })
	r.GET("/api/v1/channels/:id", h.GetChannel)
	r.GET("/api/v1/servers/:id/unread", h.GetUnreadCounts)
	r.GET("/api/v1/servers/unread-status", h.GetServerUnreadStatus)
	return r
}

func doGet(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
	return w
}

// CV-CAN-001: GetChannel returns 500 when the view-permission check errors.
func TestGetChannel_ViewPermissionError_500(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gcerr_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "GC Err Server")
	channelID := ts.CreateTestChannel(t, serverID, "gc-err")

	w := doGet(brokenResolverChannelsRouter(t, ts, owner.ID), pathChannelsPrefix+channelID)
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-002: GetUnreadCounts returns 500 when visible-channel resolution errors.
func TestGetUnreadCounts_ResolverError_500(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ucerr_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "UC Err Server")
	ts.CreateTestChannel(t, serverID, "uc-err")

	w := doGet(brokenResolverChannelsRouter(t, ts, owner.ID), pathServersPrefix+serverID+"/unread")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-002: GetServerUnreadStatus returns 500 when visible-channel resolution errors.
func TestGetServerUnreadStatus_ResolverError_500(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "suserr_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "SUS Err Server")
	ts.CreateTestChannel(t, serverID, "sus-err")

	w := doGet(brokenResolverChannelsRouter(t, ts, owner.ID), "/api/v1/servers/unread-status")
	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
}

// CV-CAN-002: GetServerUnreadStatus reports a server that has an unread message in
// a channel the caller can view — exercises the main query + row-scan path.
func TestGetServerUnreadStatus_WithUnread(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sus_owner")
	member := ts.CreateTestUser(t, "sus_member")
	serverID := ts.CreateTestServer(t, owner.ID, "SUS Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "sus-chan")

	// An unread message from another user (owner) in a channel the member can view.
	_, err := ts.DB.Exec(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		 VALUES ($1, $2, $3, 'hello', 1, false, NOW(), NOW())`,
		uuid.New().String(), channelID, owner.ID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())

	var body struct {
		ServerIDs []string `json:"server_ids"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Contains(t, body.ServerIDs, serverID)
}
