package channels

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq" // register the postgres driver for sql.Open
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
)

// broadcastChannelsReordered is best-effort and no-ops on an unset hub or an
// unparseable serverID; cover both guard branches directly (they are unreachable
// from the full ReorderChannels flow, where the hub is wired and serverID is
// already validated).
func TestBroadcastChannelsReordered_NilHub(t *testing.T) {
	h := &Handler{} // hub is nil
	assert.NotPanics(t, func() {
		h.broadcastChannelsReordered("11111111-1111-1111-1111-111111111111", nil)
	})
}

func TestBroadcastChannelsReordered_InvalidServerID(t *testing.T) {
	// Non-nil hub, but the serverID fails to parse -> returns before any broadcast.
	h := &Handler{hub: &websocket.Hub{}}
	assert.NotPanics(t, func() {
		h.broadcastChannelsReordered("not-a-uuid", nil)
	})
}

// brokenDBHandler builds a channels.Handler whose DB is closed, so every query
// errors. It covers the group-ownership helpers' defensive error branches, which
// cannot be reached through the full HTTP flow (an earlier query on the same DB
// would fail first). package channels (internal) reaches the unexported helpers
// directly; it cannot import internal/testhelpers (that would cycle through
// internal/api -> channels). The DSN carries no credentials — sql.Open is lazy
// and the pool is closed immediately, so no connection is attempted.
func brokenDBHandler(t *testing.T) *Handler {
	t.Helper()
	db, err := sql.Open("postgres", "postgres://localhost:5432/unused?sslmode=disable")
	require.NoError(t, err)
	require.NoError(t, db.Close())
	return &Handler{db: db, log: logger.New("test")}
}

func brokenCtx() (*gin.Context, *httptest.ResponseRecorder) {
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/", nil)
	return c, w
}

const testGroupID = "11111111-1111-1111-1111-111111111111"

// CV-CAN-010/011/012: groupBelongsToServer surfaces a DB error (rather than
// swallowing it) so callers fail closed with a 500.
func TestGroupBelongsToServer_DBError(t *testing.T) {
	h := brokenDBHandler(t)
	gid := testGroupID
	ok, err := h.groupBelongsToServer(context.Background(), &gid, "srv-1")
	assert.Error(t, err)
	assert.False(t, ok)
}

// CV-CAN-010/011/012 edge case: a malformed (non-UUID) group_id is a client
// input error. groupBelongsToServer rejects it as a bad binding (false, nil) so
// callers return 400, rather than letting the Postgres uuid cast fail and
// surface a 500. The parse guard short-circuits before any DB query, so the
// closed pool is never touched (no error is returned).
func TestGroupBelongsToServer_MalformedGroupID(t *testing.T) {
	h := brokenDBHandler(t)
	gid := "not-a-uuid"
	ok, err := h.groupBelongsToServer(context.Background(), &gid, "srv-1")
	assert.NoError(t, err)
	assert.False(t, ok)
}

func TestValidateReorderGroupOwnership_DBError_500(t *testing.T) {
	h := brokenDBHandler(t)
	c, w := brokenCtx()
	gid := testGroupID
	ok := h.validateReorderGroupOwnership(c,
		ReorderChannelsRequest{Channels: []ChannelPosition{{GroupID: &gid}}}, "srv-1")
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// CV-CAN-012 edge case: a malformed (non-UUID) group_id in a reorder request is
// rejected as a bad binding (400) during collection, before the batched
// ownership query runs — so the closed pool is never touched.
func TestValidateReorderGroupOwnership_MalformedGroupID_400(t *testing.T) {
	h := brokenDBHandler(t)
	c, w := brokenCtx()
	gid := "not-a-uuid"
	ok := h.validateReorderGroupOwnership(c,
		ReorderChannelsRequest{Channels: []ChannelPosition{{GroupID: &gid}}}, "srv-1")
	assert.False(t, ok)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestValidateUpdateChannelGroupOwnership_DBError_500(t *testing.T) {
	h := brokenDBHandler(t)
	c, w := brokenCtx()
	gid := testGroupID
	ok := h.validateUpdateChannelGroupOwnership(c,
		UpdateChannelRequest{GroupID: &gid}, "srv-1")
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
