package channels

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/credepoch"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/keyrotation"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/gin-gonic/gin"
	_ "github.com/lib/pq" // register the postgres driver for sql.Open
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
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

func TestChannelGroupAuthorityHelpers(t *testing.T) {
	groupA := "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	groupB := "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	states := []groupedChannelState{
		{ID: "voice", GroupID: &groupA, SyncPermissions: true, IsVoice: true},
		{ID: "text", GroupID: &groupA, SyncPermissions: true},
		{ID: "plain", GroupID: &groupB, SyncPermissions: false, IsVoice: true},
	}

	all, voice := syncedGroupChannelIDs(states)
	assert.Equal(t, []string{"voice", "text"}, all)
	assert.Equal(t, []string{"voice"}, voice)

	assert.True(t, sameChannelIDSet([]string{"a", "b"}, []string{"a", "b"}))
	assert.False(t, sameChannelIDSet([]string{"a"}, []string{"a", "b"}))

	rotations := []keyrotation.Rotation{{DeletedChannelIDs: []string{"gone", "also-gone"}}, {DeletedChannelIDs: []string{"gone"}}}
	deleted := authorityRotationDeletedChannelIDs(rotations)
	remaining := filterDeletedReorderedChannels([]ChannelPosition{{ChannelID: "gone"}, {ChannelID: "keep"}}, deleted)
	assert.Equal(t, []ChannelPosition{{ChannelID: "keep"}}, remaining)

	request := ReorderChannelsRequest{Channels: []ChannelPosition{
		{ChannelID: "voice", GroupID: &groupB},
		{ChannelID: "text"},
	}}
	assert.Equal(t, []string{groupA, groupB}, groupIDsForReorder(request, map[string]groupedChannelState{
		"voice": states[0], "text": states[1],
	}))
	all, voice = authorityAffectedChannelIDs(request, map[string]groupedChannelState{
		"voice": states[0], "text": states[1],
	})
	assert.Equal(t, []string{"text", "voice"}, all)
	assert.Equal(t, []string{"voice"}, voice)

	assert.True(t, sameChannelStates(map[string]groupedChannelState{"voice": states[0]}, map[string]groupedChannelState{"voice": states[0]}))
	assert.False(t, sameChannelStates(map[string]groupedChannelState{"voice": states[0]}, map[string]groupedChannelState{"text": states[1]}))
}

func TestRespondChannelGroupDeleteMutationError_ClassifiesAuthorityFailures(t *testing.T) {
	cases := []struct {
		name string
		err  error
		code int
	}{
		{"stale credential epoch", credepoch.ErrEpochMismatch, http.StatusUnauthorized},
		{"blocked credential epoch", credepoch.ErrBlocked, http.StatusUnauthorized},
		{"permission denied", errManageChannelsDenied, http.StatusForbidden},
		{"not a member", rbac.ErrNotMember, http.StatusForbidden},
		{"temporary override", rbac.ErrTemporaryChannelOverrideManaged, http.StatusConflict},
		{"authority changed", errChannelAuthoritySetChanged, http.StatusConflict},
		{"child limit", errChannelGroupChildLimit, http.StatusConflict},
		{"unexpected failure", errors.New("database unavailable"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := brokenDBHandler(t)
			c, w := brokenCtx()
			h.respondChannelGroupDeleteMutationError(c, "server", nil, tc.err)
			assert.Equal(t, tc.code, w.Code)
		})
	}
}

func TestRespondCreateChannelGuardError_ClassifiesCredentialFailures(t *testing.T) {
	h := brokenDBHandler(t)
	for _, tc := range []struct {
		name string
		err  error
		code int
	}{
		{"stale credential epoch", credepoch.ErrEpochMismatch, http.StatusUnauthorized},
		{"blocked credential epoch", credepoch.ErrBlocked, http.StatusUnauthorized},
		{"guard lookup failure", errors.New("database unavailable"), http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, w := brokenCtx()
			h.respondCreateChannelGuardError(c, tc.err)
			assert.Equal(t, tc.code, w.Code)
		})
	}
}

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

func TestAuthorizeCreateChannelTxRejectsMissingMembership(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	h := NewHandler(db, logger.New("test"), nil, nil, nil)
	serverID := uuid.NewString()
	userID := uuid.NewString()
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	c, w := brokenCtx()
	request := CreateChannelRequest{ServerID: serverID, Type: "text"}
	// No membership row exists, so the write fence must stop before any
	// permission or channel-group work and return a fail-closed 403.
	ok := h.authorizeCreateChannelTx(c, tx, request, userID)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestAuthorizeCreateChannelTxFailsClosedOnTransactionError(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	h := NewHandler(db, logger.New("test"), nil, nil, nil)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
	c, w := brokenCtx()
	ok := h.authorizeCreateChannelTx(c, tx, CreateChannelRequest{ServerID: uuid.NewString(), Type: "text"}, uuid.NewString())
	assert.False(t, ok)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}
