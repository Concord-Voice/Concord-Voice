package channels_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	gorillaWS "github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathChannelsPrefix     = "/api/v1/channels/"
	pathChannels           = "/api/v1/channels"
	pathServersPrefix      = "/api/v1/servers/"
	pathKeys               = "/keys"
	keyServerID            = "server_id"
	keyWrappedKeys         = "wrapped_keys"
	keyFingerprint         = "key_fingerprint"
	keyChannel             = "channel"
	roleMember             = "member"
	fmtRateLimitChannelKey = "ratelimit:channel_rotate:%s"
	rotationFingerprint    = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	otherFingerprint       = "//////////////////////////////////////////8="
)

func setupTS(t *testing.T) *testhelpers.TestServer {
	t.Helper()
	return testhelpers.SetupTestServer(t)
}

// Helper: create a user + server + channel combo
func setupWithChannel(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "chanuser")
	serverID := ts.CreateTestServer(t, user.ID, "Channel Test Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	return ts, user, serverID, channelID
}

// --- List Channels ---

func TestListChannelsSuccess(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/channels", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channels := body["channels"].([]interface{})
	assert.GreaterOrEqual(t, len(channels), 1)
}

func TestListChannelsNotMember(t *testing.T) {
	ts, _, serverID, _ := setupWithChannel(t)
	outsider := ts.CreateTestUser(t, "chanoutsider")

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/channels", nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Create Channel ---

func TestCreateChannelSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "createchan")
	serverID := ts.CreateTestServer(t, user.ID, "Chan Create Server")

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "new-channel",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body[keyChannel].(map[string]interface{})
	assert.Equal(t, "new-channel", channel["name"])
}

func TestCreateChannel_OverflowBindsInitialDistributionToCreator(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "overflowcreator")
	serverID := ts.CreateTestServer(t, creator.ID, "Overflow Channel Server")
	member := ts.CreateTestUser(t, "overflowmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "overflow-channel",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			creator.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channelID := body[keyChannel].(map[string]interface{})["id"].(string)
	var recordedCreator string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT creator_id FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID,
	).Scan(&recordedCreator))
	assert.Equal(t, creator.ID, recordedCreator)
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, member.ID),
		"an omitted initial recipient must be enqueued for creator re-wrap")
}

func TestCreateChannel_RequiresCreatorWrappedKey(t *testing.T) {
	ts := setupTS(t)
	creator := ts.CreateTestUser(t, "missingcreatorkey")
	serverID := ts.CreateTestServer(t, creator.ID, "Creator Key Server")
	member := ts.CreateTestUser(t, "creator-key-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "missing-creator-key",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			member.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(creator.AccessToken))

	assert.Equal(t, http.StatusConflict, w.Code)
	assert.JSONEq(t, `{"error":"Creator must provide a current encryption key"}`, w.Body.String())
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channels WHERE server_id = $1 AND name = $2`, serverID, "missing-creator-key"),
		"failed creation must roll back its channel row")
}

// TestCreateChannel_ForeignGroupRejected covers CV-CAN-010: a new channel must
// not be bound to a category (channel_groups) owned by another server, which
// the permission-sync cascade would treat as the source of overrides.
func TestCreateChannel_ForeignGroupRejected(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "cfgroupuser")
	serverID := ts.CreateTestServer(t, user.ID, "CFG Server A")
	otherServerID := ts.CreateTestServer(t, user.ID, "CFG Server B")
	foreignGroupID := createGroup(t, ts, otherServerID, "Foreign Group", user.AccessToken)

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "foreign-bound",
		"type":      "text",
		"group_id":  foreignGroupID,
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestUpdateChannel_ForeignGroupRejected covers CV-CAN-011: an existing channel
// must not be moved under a category owned by another server.
func TestUpdateChannel_ForeignGroupRejected(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "ufgroupuser")
	serverID := ts.CreateTestServer(t, user.ID, "UFG Server A")
	channelID := ts.CreateTestChannel(t, serverID, "ufg-chan")
	otherServerID := ts.CreateTestServer(t, user.ID, "UFG Server B")
	foreignGroupID := createGroup(t, ts, otherServerID, "Foreign Group", user.AccessToken)

	// name/type are required by UpdateChannelRequest binding; include valid
	// values so the request binds and the foreign-group guard (not the binding
	// validator) is what rejects it.
	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name":     "ufg-chan",
		"type":     "text",
		"group_id": foreignGroupID,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "group_id does not belong to this server", body["error"])
}

func TestCreateChannelNotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "chanowner2")
	member := ts.CreateTestUser(t, "chanmember2")
	serverID := ts.CreateTestServer(t, owner.ID, "Not Admin Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "blocked-channel",
		"type":      "text",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCreateChannelRequiresCreatorView(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "chanviewowner")
	creator := ts.CreateTestUser(t, "chanviewcreator")
	serverID := ts.CreateTestServer(t, owner.ID, "Channel View Server")
	ts.AddMemberToServer(t, serverID, creator.ID, roleMember)
	_, err := ts.DB.Exec(`UPDATE roles SET permissions = $2 WHERE server_id = $1 AND is_default = TRUE`, serverID, int64(rbac.PermManageChannels))
	require.NoError(t, err)
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "unviewable-channel",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			creator.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCreateChannelEncrypted(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "encrchan")
	serverID := ts.CreateTestServer(t, user.ID, "Encrypted Server")

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "encrypted-channel",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body[keyChannel].(map[string]interface{})
	assert.Equal(t, "encrypted-channel", channel["name"])
}

// --- Get Channel ---

func TestGetChannelSuccess(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestGetChannel_HiddenChannelDeniedToMember covers CV-CAN-001: a server member
// denied channel view must not read hidden channel metadata by UUID; the owner
// (view-bypass) still sees it.
func TestGetChannel_HiddenChannelDeniedToMember(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "gcowner")
	member := ts.CreateTestUser(t, "gcmember")
	serverID := ts.CreateTestServer(t, owner.ID, "GetChannel Hidden")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "hidden-general")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code, "view-denied member gets not-found (no existence oracle)")

	w = ts.DoRequest("GET", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "owner still sees the channel")
}

// TestGetUnreadCounts_ExcludesHiddenChannel covers CV-CAN-002: unread counts must
// not enumerate channel IDs the caller cannot view.
func TestGetUnreadCounts_ExcludesHiddenChannel(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ucowner")
	member := ts.CreateTestUser(t, "ucmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Unread Hidden")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	visibleChan := ts.CreateTestChannel(t, serverID, "visible-chan")
	hiddenChan := ts.CreateTestChannel(t, serverID, "hidden-chan")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, hiddenChan, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Unreads []struct {
			ChannelID string `json:"channel_id"`
		} `json:"unreads"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	ids := make(map[string]bool)
	for _, u := range body.Unreads {
		ids[u.ChannelID] = true
	}
	assert.True(t, ids[visibleChan], "visible channel must be present in unread counts")
	assert.False(t, ids[hiddenChan], "hidden channel must be excluded from unread counts")
}

// TestGetUnreadCounts_AllChannelsHiddenReturnsEmpty covers CV-CAN-002: a member
// denied view on every channel gets an empty unread list (200), and the count
// aggregation is never run over hidden channels (the SQL is scoped to visible
// channel IDs, so hidden-channel message history cannot be forced to be scanned).
func TestGetUnreadCounts_AllChannelsHiddenReturnsEmpty(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "uchidallowner")
	member := ts.CreateTestUser(t, "uchidallmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Unread All Hidden")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	hidden1 := ts.CreateTestChannel(t, serverID, "hidden-1")
	hidden2 := ts.CreateTestChannel(t, serverID, "hidden-2")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, hidden1, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, hidden2, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Unreads []struct {
			ChannelID string `json:"channel_id"`
		} `json:"unreads"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Empty(t, body.Unreads, "member who can view no channels must get an empty unread list")
}

// --- Update Channel ---

func TestUpdateChannelSuccess(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "renamed-channel",
		"type": "text",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body[keyChannel].(map[string]interface{})
	assert.Equal(t, "renamed-channel", channel["name"])
}

// --- Delete Channel ---

func TestDeleteChannelAsOwner(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestDeleteChannelNotAdmin(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "dchowner")
	member := ts.CreateTestUser(t, "dchmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Del Chan Server")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "protected")

	w := ts.DoRequest("DELETE", pathChannelsPrefix+channelID, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// --- Mark Read ---

func TestMarkChannelReadSuccess(t *testing.T) {
	ts, user, _, channelID := setupWithChannel(t)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// TestMarkChannelRead_ViewDeniedMemberBlocked covers CV-CAN-002: a member denied
// view on a channel must not be able to write read state for it via the
// per-channel endpoint. The response must match the non-member response so a
// hidden channel cannot be distinguished, and no channel_read_states row is written.
func TestMarkChannelRead_ViewDeniedMemberBlocked(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "mcrhidowner")
	member := ts.CreateTestUser(t, "mcrhidmember")
	serverID := ts.CreateTestServer(t, owner.ID, "MarkRead Hidden")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	hiddenChan := ts.CreateTestChannel(t, serverID, "hidden-mcr")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, hiddenChan, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	// View-denied member is blocked with the same response as a non-member.
	w := ts.DoRequest("POST", pathChannelsPrefix+hiddenChan+"/read", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	// No read state was written for the hidden channel.
	var exists bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT EXISTS(SELECT 1 FROM channel_read_states WHERE user_id = $1 AND channel_id = $2)`,
		member.ID, hiddenChan,
	).Scan(&exists))
	assert.False(t, exists, "no read state must be written for a hidden channel")

	// The owner (bypasses view) can still mark the channel read.
	w = ts.DoRequest("POST", pathChannelsPrefix+hiddenChan+"/read", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMarkServerReadSuccess(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("POST", pathServersPrefix+serverID+"/read", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Unread Counts ---

func TestGetUnreadCountsSuccess(t *testing.T) {
	ts, user, serverID, _ := setupWithChannel(t)

	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/unread", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetServerUnreadStatusSuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "unreaduser")

	w := ts.DoRequest("GET", "/api/v1/servers/unread-status", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

// --- Channel Key Management (E2EE) ---

// Helper: create encrypted channel via API (stores initial channel key)
func setupEncryptedChannel(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string, string) {
	t.Helper()
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "e2eeuser")
	serverID := ts.CreateTestServer(t, user.ID, "E2EE Server")

	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverID,
		"name":      "secret-channel",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			user.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	channel := body[keyChannel].(map[string]interface{})
	channelID := channel["id"].(string)

	return ts, user, serverID, channelID
}

func TestGetChannelKeysSuccess(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.NotNil(t, body["key"])
}

func TestGetChannelKeysNotMember(t *testing.T) {
	ts, _, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "keyoutsider")

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

// TestGetChannelKeys_HiddenChannelDeniedToMember covers CV-CAN-005: a member
// denied channel view must not retrieve wrapped channel-key material, even if a
// key row was previously stored for them.
func TestGetChannelKeys_HiddenChannelDeniedToMember(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "keynoview")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Owner distributes a key to the member while they still have view.
	distRec := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, distRec.Code)

	// Now deny view on the channel for the default role.
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	// The key distribution above cached the member's (then-allowed) view
	// permission. The direct-DB CreateChannelOverride helper does not invalidate
	// the RBAC permission cache the way the production override handler does
	// (internal/rbac/handlers.go), so invalidate it here to reflect the deny —
	// otherwise the stale cache entry masks the CV-CAN-005 gate.
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "view-denied member must not fetch channel keys")
}

// TestDistributeChannelKeys_SkipsNoViewTarget covers CV-CAN-005: key distribution
// must skip a target lacking channel view (they must not be enrolled for a hidden
// channel's key distribution).
func TestDistributeChannelKeys_SkipsNoViewTarget(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "distnoview")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"], "no-view target must be skipped by distribution")
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID),
		"a no-view member must not keep the creator fence active")
}

// TestDistributeChannelKeys_CallerDeniedViewForbidden covers CV-CAN-005: the
// distributor (caller) is gated on channel VIEW, not just membership + a stored
// key. A member who received a key and was later denied VIEW must not be able to
// push wraps/rotations for a hidden channel.
func TestDistributeChannelKeys_CallerDeniedViewForbidden(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "distcallernoview")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Owner gives the member a key while they still have view, so callerHasChannelKey
	// would pass — isolating the new VIEW gate as the reason for the 403.
	distRec := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, distRec.Code)

	// Deny view for the default role and invalidate the RBAC cache.
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "view-denied distributor must be blocked")
}

// TestRequestRewrap_HiddenChannelDeniedMemberNoOracle covers CV-CAN-005: a
// member denied channel view who POSTs /rewrap must get the SAME not-found
// response an unknown context yields (404), not the 403 that would leak the
// hidden channel's existence, and must not be enrolled for key distribution.
func TestRequestRewrap_HiddenChannelDeniedMemberNoOracle(t *testing.T) {
	ts, _, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "rewrapnoview")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	hidden := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, hidden.Code, "hidden channel must not be distinguishable via 403")

	unknown := ts.DoRequest("POST", "/api/v1/e2ee/keys/00000000-0000-0000-0000-000000000000/rewrap", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusNotFound, unknown.Code, "unknown context yields the same 404")

	var count int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, member.ID,
	).Scan(&count))
	assert.Equal(t, 0, count, "no-view member must not be enrolled")
}

// TestDistributeUnifiedKeys_HiddenChannelNoViewNoOracle covers CV-CAN-005: a
// server member denied channel VIEW who POSTs to the unified distribute route
// (POST /api/v1/e2ee/keys/:context_id) must get the SAME not-found (404) an
// unknown context yields — not the 403 that delegating to DistributeChannelKeys
// would leak — otherwise the endpoint is a hidden-channel existence oracle.
func TestDistributeUnifiedKeys_HiddenChannelNoViewNoOracle(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "duknoview")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Give the member a key while they still have view so callerHasChannelKey
	// would pass — isolating the VIEW gate (not the has-key gate) as the reason
	// the hidden channel stays indistinguishable from an unknown context.
	distRec := ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, distRec.Code)

	// Deny view for the member via a user override, then invalidate RBAC cache.
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermViewTextChannels))
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	hidden := ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusNotFound, hidden.Code, "hidden channel must not be distinguishable via 403; body: %s", hidden.Body.String())

	unknown := ts.DoRequest("POST", pathE2EEKeys+"00000000-0000-0000-0000-000000000000", map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(member.AccessToken))
	require.Equal(t, http.StatusNotFound, unknown.Code, "unknown context must yield the same 404")

	assert.JSONEq(t, hidden.Body.String(), unknown.Body.String(), "no-view and unknown-context responses must be byte-identical (no oracle)")
}

// TestDistributeUnifiedKeys_ChannelCheckDBErrorFailsClosed covers CV-CAN-005:
// when the channel VIEW/membership check errors on the unified distribute route,
// the endpoint must fail CLOSED with a 500 rather than fall through to the DM
// branch or delegate. Renaming channels makes channelKeyAccess error.
func TestDistributeUnifiedKeys_ChannelCheckDBErrorFailsClosed(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dukdberr")
	channelID := uuid.NewString()

	withRenamedTable(t, ts, "channels", func() {
		w := ts.DoRequest("POST", pathE2EEKeys+channelID, map[string]interface{}{
			keyWrappedKeys: map[string]string{user.ID: testhelpers.ValidCiphertext()},
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "Failed to distribute keys", body["error"])
	})
}

// TestGetPendingKeyRequests_CallerDeniedViewHidesQueue covers CV-CAN-005: the
// pending-keys endpoint must not surface a channel's queue to a caller who holds
// a (stale) key but has since been denied channel view — otherwise a hidden
// channel's queue is exposed and the no-view caller is prompted to fulfill it.
func TestGetPendingKeyRequests_CallerDeniedViewHidesQueue(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	caller := ts.CreateTestUser(t, "pendcallernoview")
	ts.AddMemberToServer(t, serverID, caller.ID, roleMember)
	requester := ts.CreateTestUser(t, "pendrequester")
	ts.AddMemberToServer(t, serverID, requester.ID, roleMember)

	// Caller receives a key, so they appear as a servicer in the outer query.
	distRec := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{caller.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, distRec.Code)

	// Requester (still has view) enrolls a pending request for the channel.
	enroll := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil, testhelpers.AuthHeaders(requester.AccessToken))
	require.Equal(t, http.StatusAccepted, enroll.Code)

	// Deny view for the caller only (user override → priority over the default role),
	// leaving the requester's view intact so this isolates the caller-side gate.
	ts.CreateChannelOverride(t, channelID, "user", caller.ID, 0, int64(rbac.PermViewTextChannels))
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(caller.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["pending_requests"].([]interface{})
	assert.Empty(t, requests, "view-denied caller must not be shown a hidden channel's pending queue")
}

// TestGetChannelKeys_ChannelAccessDBError covers CV-CAN-005: when the channel
// VIEW/membership check itself errors (DB failure inside channelKeyAccess),
// GetChannelKeys must fail CLOSED with a 500 rather than leak key material or
// degrade to allow. Renaming channels makes the access query error.
func TestGetChannelKeys_ChannelAccessDBError(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "gckdberr1")
	channelID := uuid.NewString()

	withRenamedTable(t, ts, "channels", func() {
		w := ts.DoRequest("GET", pathChannelsPrefix+channelID+pathKeys, nil, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "Failed to fetch channel keys", body["error"])
	})
}

// TestDistributeChannelKeys_VerifyDBError covers CV-CAN-005: verifyChannelEncrypted
// must fail CLOSED with a 500 when the distributor's VIEW check errors, rather
// than falling back to membership-only and allowing a write. Renaming channels
// makes channelKeyAccess error before the caller-has-key check runs.
func TestDistributeChannelKeys_VerifyDBError(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "dckdberr1")
	channelID := uuid.NewString()

	withRenamedTable(t, ts, "channels", func() {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{user.ID: testhelpers.ValidCiphertext()},
		}, testhelpers.AuthHeaders(user.AccessToken))
		assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "Failed to distribute keys", body["error"])
	})
}

// TestDistributeChannelKeys_TransactionRecheckFailsClosedOnViewCheckError
// verifies that a fresh post-lock VIEW check cannot fail open. Renaming roles
// makes the recheck fail before any target can receive a wrapped key.
func TestDistributeChannelKeys_TransactionRecheckFailsClosedOnViewCheckError(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	target := ts.CreateTestUser(t, "distskiperr")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	withRenamedTable(t, ts, "roles", func() {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		}, testhelpers.AuthHeaders(owner.AccessToken))

		require.Equal(t, http.StatusInternalServerError, w.Code, "the post-lock recheck must fail closed; body: %s", w.Body.String())
	})
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, target.ID))
}

// TestGetPendingKeyRequests_CallerViewDBErrorFailsClosed covers CV-CAN-005: when
// the caller's per-channel VIEW check errors, the pending-keys endpoint must fail
// CLOSED (drop the request) rather than surface a hidden channel's queue. The
// outer pkr⋈channel_keys query does not touch channels, so renaming channels
// makes only channelKeyAccess error — isolating the fail-closed error branch.
func TestGetPendingKeyRequests_CallerViewDBErrorFailsClosed(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	caller := ts.CreateTestUser(t, "pendcallerdberr")
	ts.AddMemberToServer(t, serverID, caller.ID, roleMember)
	requester := ts.CreateTestUser(t, "pendrequesterdberr")
	ts.AddMemberToServer(t, serverID, requester.ID, roleMember)

	distRec := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{caller.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, distRec.Code)

	enroll := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil, testhelpers.AuthHeaders(requester.AccessToken))
	require.Equal(t, http.StatusAccepted, enroll.Code)

	// The outer pkr⋈channel_keys query does not touch channels, so renaming
	// channels makes only the per-candidate channelKeyAccess error.
	withRenamedTable(t, ts, "channels", func() {
		w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(caller.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, "body: %s", w.Body.String())
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		requests := body["pending_requests"].([]interface{})
		assert.Empty(t, requests, "caller VIEW-check DB error must fail closed (hide the queue)")
	})
}

func TestGetChannelKeysInvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badchid")

	w := ts.DoRequest("GET", "/api/v1/channels/not-a-uuid/keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDistributeChannelKeysSuccess(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	// Add a new member to the server
	newMember := ts.CreateTestUser(t, "newmember")
	ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

	// Owner distributes key to the new member
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(1), body["distributed"])
	assert.Equal(t, float64(0), body["duplicates"])
}

func TestDistributeChannelKeysDuplicate(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)

	// Add a new member
	newMember := ts.CreateTestUser(t, "dupmember")
	ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

	// Distribute once
	ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	// Distribute again — should be duplicate
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			newMember.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, float64(0), body["distributed"])
	assert.Equal(t, float64(1), body["duplicates"])
}

func TestDistributeChannelKeysNotMember(t *testing.T) {
	ts, _, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "distoutsider")

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			outsider.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDistributeChannelKeys_InitialOverflowAllowsOnlyCreator(t *testing.T) {
	ts, creator, serverID, channelID := setupEncryptedChannel(t)
	attacker := ts.CreateTestUser(t, "initialkeyattacker")
	target := ts.CreateTestUser(t, "initialkeytarget")
	ts.AddMemberToServer(t, serverID, attacker.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Give the attacker a valid existing channel key so the test isolates the
	// initial-distribution creator fence from the normal caller-has-key gate.
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{attacker.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`,
		channelID, creator.ID,
	)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(attacker.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, target.ID))

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
	}, testhelpers.AuthHeaders(attacker.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, target.ID))

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, target.ID))

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID))
}

func TestDistributeChannelKeys_RequiresIssuedRotationEpoch(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "rotationresponder")
	target := ts.CreateTestUser(t, "rotationtarget")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2147483647,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND key_version = 2147483647`, channelID))

	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "an ordinary member may answer an issued rotation")
}

func TestDistributeChannelKeys_BindsIssuedRotationToFirstDistributor(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	distributor := ts.CreateTestUser(t, "rotationdistributor")
	target := ts.CreateTestUser(t, "rotationbatchtarget")
	ts.AddMemberToServer(t, serverID, distributor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{distributor.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{},
		"key_version":  2,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code, "an empty batch must not claim a rotation")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, "an issued rotation requires a CSK fingerprint")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	// Two clients on one account can generate different CSKs. The second must
	// fail just like a different account, rather than interleave later batches.
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: otherFingerprint,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: otherFingerprint,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "a later holder of the established CSK may rewrap it")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	_, err = ts.DB.Exec(`DELETE FROM users WHERE id = $1`, distributor.ID)
	require.NoError(t, err)
	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code, "erasing the first distributor must not block a holder of the established CSK")
}

func TestDistributeChannelKeys_RechecksDistributorAfterChannelLock(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	distributor := ts.CreateTestUser(t, "staledistributor")
	target := ts.CreateTestUser(t, "staledistributiontarget")
	ts.AddMemberToServer(t, serverID, distributor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{distributor.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	_, err = lockTx.Exec(`SELECT 1 FROM channels WHERE id = $1 FOR UPDATE`, channelID)
	require.NoError(t, err)

	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
			keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
			"key_version":  2,
			keyFingerprint: rotationFingerprint,
		}, testhelpers.AuthHeaders(distributor.AccessToken))
	}()

	require.Eventually(t, func() bool {
		var waiting int
		err := ts.DB.QueryRow(`SELECT COUNT(*) FROM pg_locks WHERE NOT granted`).Scan(&waiting)
		return err == nil && waiting > 0
	}, time.Second, 10*time.Millisecond, "distribution must wait on the channel lock after its preflight")
	_, err = ts.DB.Exec(`DELETE FROM channel_keys WHERE channel_id = $1 AND user_id = $2`, channelID, distributor.ID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, distributor.ID)
	require.NoError(t, err)
	require.NoError(t, lockTx.Commit())

	w = <-response
	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, target.ID))
	var claimed bool
	require.NoError(t, ts.DB.QueryRow(
		`SELECT rotation_distributor_claimed FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 1`, channelID,
	).Scan(&claimed))
	assert.False(t, claimed)
}

func TestDistributeChannelKeys_QueuesRemainingRotationRecipients(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	distributor := ts.CreateTestUser(t, "partialrotationdistributor")
	remaining := ts.CreateTestUser(t, "partialrotationremaining")
	ts.AddMemberToServer(t, serverID, distributor.ID, roleMember)
	ts.AddMemberToServer(t, serverID, remaining.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{distributor.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(distributor.AccessToken), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(distributor.ID)) > 0
	}, time.Second, 10*time.Millisecond)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = conn.ReadMessage() // connection bootstrap
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{distributor.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, distributor.ID))
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, remaining.ID),
		"an unsubmitted later batch must stay recoverable after the first batch claims the CSK")
	messageTypes := make([]string, 0, 2)
	deadline := time.Now().Add(2 * time.Second)
	for len(messageTypes) < 2 && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, frame, err := conn.ReadMessage()
		require.NoError(t, err)
		var message struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &message))
		if message.Type == "key_delivered" || message.Type == "key_needed" {
			messageTypes = append(messageTypes, message.Type)
		}
	}
	assert.Equal(t, []string{"key_delivered", "key_needed"}, messageTypes)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{remaining.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(distributor.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, remaining.ID))

	_ = conn.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
	for {
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				break
			}
			require.NoError(t, readErr)
		}
		var message struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &message))
		assert.NotEqual(t, "key_needed", message.Type, "fulfilling an existing queue row must not start another queue drain")
	}
}

func TestDistributeChannelKeys_DoesNotClaimRotationWithoutRecipient(t *testing.T) {
	ts, owner, _, channelID := setupEncryptedChannel(t)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed
		 ) VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, owner.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{"not-a-uuid": testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var claimed bool
	var fingerprint sql.NullString
	require.NoError(t, ts.DB.QueryRow(
		`SELECT rotation_distributor_claimed, rotation_key_fingerprint
		 FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 1`, channelID,
	).Scan(&claimed, &fingerprint))
	assert.False(t, claimed)
	assert.False(t, fingerprint.Valid)
}

func TestDistributeChannelKeys_WakesCreatorForIncompleteMarker(t *testing.T) {
	ts, creator, serverID, channelID := setupEncryptedChannel(t)
	delivered := ts.CreateTestUser(t, "markerbatchdelivered")
	remaining := ts.CreateTestUser(t, "markerbatchremaining")
	ts.AddMemberToServer(t, serverID, delivered.ID, roleMember)
	ts.AddMemberToServer(t, serverID, remaining.ID, roleMember)

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
		 ) VALUES ($1, 1, 2, 'member_removal', $2, $2, TRUE, $3)`,
		channelID, creator.ID, rotationFingerprint,
	)
	require.NoError(t, err)
	_, err = tx.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id, key_version)
		 VALUES ($1, $2, 2)`,
		channelID, creator.ID,
	)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, creator.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, rotationFingerprint)
	require.NoError(t, err)
	_, err = tx.Exec(
		`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version)
		 VALUES ($1, $2, 'creator-epoch-two', 2)`,
		channelID, creator.ID,
	)
	require.NoError(t, err)
	_, err = tx.Exec(
		`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`,
		channelID, remaining.ID,
	)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	wsServer := httptest.NewServer(ts.Router)
	t.Cleanup(wsServer.Close)
	conn, _, err := gorillaWS.DefaultDialer.Dial(
		"ws"+strings.TrimPrefix(wsServer.URL, "http")+"/api/v1/ws?token="+url.QueryEscape(creator.AccessToken), nil,
	)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	require.Eventually(t, func() bool {
		return ts.Hub.GetUserClientCount(uuid.MustParse(creator.ID)) > 0
	}, time.Second, 10*time.Millisecond)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	_, _, err = conn.ReadMessage() // connection bootstrap
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{delivered.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`, channelID, remaining.ID))

	foundKeyNeeded := false
	deadline := time.Now().Add(2 * time.Second)
	for !foundKeyNeeded && time.Now().Before(deadline) {
		_ = conn.SetReadDeadline(deadline)
		_, frame, readErr := conn.ReadMessage()
		if readErr != nil {
			if netErr, ok := readErr.(net.Error); ok && netErr.Timeout() {
				break
			}
			require.NoError(t, readErr)
		}
		var message struct {
			Type string `json:"type"`
		}
		require.NoError(t, json.Unmarshal(frame, &message))
		foundKeyNeeded = message.Type == "key_needed"
	}
	assert.True(t, foundKeyNeeded)
}

func TestDistributeChannelKeys_RejectsLegacyIssuedRotation(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "legacyrotationmember")
	target := ts.CreateTestUser(t, "legacyrotationtarget")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 1, 2, 'member_removal', $2)`, channelID, owner.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`UPDATE key_revocations
		 SET rotation_distributor_claimed = NULL
		 WHERE channel_id = $1 AND revoked_epoch = 1`, channelID,
	)
	require.NoError(t, err)

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
	}, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, "legacy rotations must remain sealed")
}

func TestDistributeChannelKeys_ExplicitVersionDoesNotRevealHiddenChannel(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	member := ts.CreateTestUser(t, "hiddenrotationmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{member.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	require.NoError(t, rbac.NewPermissionCache(ts.Redis).InvalidateServer(context.Background(), serverID))

	body := map[string]interface{}{
		keyWrappedKeys: map[string]string{owner.ID: testhelpers.ValidCiphertext()},
		"key_version":  2,
	}
	hidden := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, body, testhelpers.AuthHeaders(member.AccessToken))
	unknown := ts.DoRequest("POST", pathChannelsPrefix+uuid.New().String()+pathKeys, body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, hidden.Code)
	assert.Equal(t, hidden.Code, unknown.Code)
}

func TestDistributeChannelKeys_DeletedInitialCreatorDeletesIncompleteChannel(t *testing.T) {
	ts, _, serverID, channelID := setupEncryptedChannel(t)
	creator := ts.CreateTestUser(t, "deletedinitialcreator")
	ts.AddMemberToServer(t, serverID, creator.ID, roleMember)

	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`,
		channelID, creator.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM users WHERE id = $1`, creator.ID)
	require.NoError(t, err)

	assert.Zero(t, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE id = $1`, channelID))
}

func TestDistributeChannelKeys_CompletesAdvancedInitialDistribution(t *testing.T) {
	ts, creator, serverID, channelID := setupEncryptedChannel(t)
	target := ts.CreateTestUser(t, "deferredrotationtarget")
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed)
		 VALUES ($1, 1, 2, 'member_removal', $2, FALSE)`, channelID, creator.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by, rotation_distributor_claimed)
		 VALUES ($1, 2, 3, 'member_removal', $2, FALSE)`, channelID, creator.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id, key_version)
		 VALUES ($1, $2, 3)`, channelID, creator.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code, "an advanced marker cannot accept an unversioned stale wrap")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{target.ID: testhelpers.ValidCiphertext()},
		"key_version":  3,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, "a twice-advanced marker uses its recorded successor epoch")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{creator.ID: testhelpers.ValidCiphertext()},
		"key_version":  3,
		keyFingerprint: otherFingerprint,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, "the creator cannot interleave a different CSK at an advanced marker")

	w = ts.DoRequest("POST", pathChannelsPrefix+channelID+pathKeys, map[string]interface{}{
		keyWrappedKeys: map[string]string{
			creator.ID: testhelpers.ValidCiphertext(),
		},
		"key_version":  3,
		keyFingerprint: rotationFingerprint,
	}, testhelpers.AuthHeaders(creator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_initial_key_distributions WHERE channel_id = $1`, channelID))
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM key_revocations WHERE channel_id = $1 AND revoked_epoch = 2 AND successor_epoch = 3`, channelID))
	assert.Equal(t, 2, rowCount(t, ts,
		`SELECT COUNT(*) FROM channel_keys WHERE channel_id = $1 AND key_version = 3`, channelID))
}

// --- Pending Key Requests ---

func TestGetPendingKeyRequestsEmpty(t *testing.T) {
	ts, user, _, _ := setupEncryptedChannel(t)

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["pending_requests"].([]interface{})
	assert.Equal(t, 0, len(requests))
}

func TestGetPendingKeyRequests_UsesActiveMarkerEpoch(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	requester := ts.CreateTestUser(t, "pendingmarkerepoch")
	nonCreator := ts.CreateTestUser(t, "pendingmarkernoncreator")
	ts.AddMemberToServer(t, serverID, requester.ID, roleMember)
	ts.AddMemberToServer(t, serverID, nonCreator.ID, roleMember)

	tx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(
		`INSERT INTO key_revocations (
			channel_id, revoked_epoch, successor_epoch, reason, revoked_by,
			rotation_distributor_id, rotation_distributor_claimed, rotation_key_fingerprint
		 ) VALUES ($1, 1, 2, 'member_removal', $2, $2, TRUE, $3)`,
		channelID, owner.ID, rotationFingerprint,
	)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO channel_initial_key_distributions (channel_id, creator_id, key_version) VALUES ($1, $2, 2)`, channelID, owner.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_distributor_id', $1, TRUE)`, owner.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`SELECT set_config('concord.rotation_key_fingerprint', $1, TRUE)`, rotationFingerprint)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'epoch-two', 2)`, channelID, owner.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO channel_keys (channel_id, user_id, wrapped_key, key_version) VALUES ($1, $2, 'epoch-two-other', 2)`, channelID, nonCreator.ID)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO pending_key_requests (channel_id, user_id) VALUES ($1, $2)`, channelID, requester.ID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	w := ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	requests := body["pending_requests"].([]interface{})
	require.Len(t, requests, 1)
	assert.Equal(t, float64(2), requests[0].(map[string]interface{})["key_version"])

	// A recipient that holds v2 cannot complete the creator-only marker, so it
	// must not be prompted with recovery work it would receive a 403 submitting.
	w = ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(nonCreator.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Empty(t, body["pending_requests"].([]interface{}))

	// Conversely, the creator must hold the active marker epoch; an older key
	// cannot be used to recover a successor CSK.
	_, err = ts.DB.Exec(`DELETE FROM channel_keys WHERE channel_id = $1 AND user_id = $2 AND key_version = 2`, channelID, owner.ID)
	require.NoError(t, err)
	w = ts.DoRequest("GET", "/api/v1/e2ee/pending-keys", nil, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)
	testhelpers.ParseJSON(t, w, &body)
	assert.Empty(t, body["pending_requests"].([]interface{}))
}

// --- Rate Limiting: RotateKey ---

func TestRotateKey_InitialDistributionBlocked(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)
	_, err := ts.DB.Exec(
		`INSERT INTO channel_initial_key_distributions (channel_id, creator_id) VALUES ($1, $2)`, channelID, user.ID,
	)
	require.NoError(t, err)

	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestRotateKeyRateLimitBlocks11th(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	// Clear any pre-existing rate limit counter for this channel.
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitChannelKey, channelID))

	// First 10 calls should succeed.
	for i := 1; i <= 10; i++ {
		w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusOK, w.Code, fmt.Sprintf("call %d should succeed", i))
	}

	// Clear the per-user middleware rate limit key so the 11th request
	// reaches the handler's per-channel rate limit (not the route middleware).
	userRLKey := fmt.Sprintf("ratelimit:user:%s:POST:/api/v1/channels/:id/rotate-key", user.ID)
	ts.Redis.Del(context.Background(), userRLKey)

	// 11th call should be rate-limited by the per-channel limit.
	w := ts.DoRequest("POST", pathChannelsPrefix+channelID+pathRotateKey, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body, "error")
	assert.Contains(t, body, "message")
	assert.Contains(t, body, "retry_after")

	msg, ok := body["message"].(string)
	require.True(t, ok)
	assert.Contains(t, msg, "Try again in")
}

func TestRotateKeyRateLimitIndependentChannels(t *testing.T) {
	// Use two separate users/servers so the per-user middleware rate limit
	// (10/min on the route) doesn't interfere with the per-channel test.
	ts, userA, _, channelA := setupEncryptedChannel(t)

	// Create a second user + server + encrypted channel for independence test.
	userB := ts.CreateTestUser(t, "ratelimitb")
	serverB := ts.CreateTestServer(t, userB.ID, "RateLimit B Server")
	w := ts.DoRequest("POST", pathChannels, map[string]interface{}{
		keyServerID: serverB,
		"name":      "encrypted-b",
		"type":      "text",
		keyWrappedKeys: map[string]string{
			userB.ID: testhelpers.ValidCiphertext(),
		},
	}, testhelpers.AuthHeaders(userB.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var createBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &createBody)
	channelB := createBody[keyChannel].(map[string]interface{})["id"].(string)

	// Clear any pre-existing rate limit counters for these channels.
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitChannelKey, channelA))
	ts.Redis.Del(context.Background(), fmt.Sprintf(fmtRateLimitChannelKey, channelB))

	// Exhaust channel A's per-channel limit.
	for i := 1; i <= 10; i++ {
		resp := ts.DoRequest("POST", pathChannelsPrefix+channelA+pathRotateKey, nil, testhelpers.AuthHeaders(userA.AccessToken))
		require.Equal(t, http.StatusOK, resp.Code, fmt.Sprintf("channel A call %d should succeed", i))
	}

	// Clear the per-user middleware rate limit key so the 11th request
	// reaches the handler's per-channel rate limit (not the route middleware).
	userARLKey := fmt.Sprintf("ratelimit:user:%s:POST:/api/v1/channels/:id/rotate-key", userA.ID)
	ts.Redis.Del(context.Background(), userARLKey)

	// Channel A should be blocked with proper response body.
	resp := ts.DoRequest("POST", pathChannelsPrefix+channelA+pathRotateKey, nil, testhelpers.AuthHeaders(userA.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, resp.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, resp, &body)
	assert.Contains(t, body, "error")
	assert.Contains(t, body, "message")
	assert.Contains(t, body, "retry_after")

	// Channel B (different channel, different user) should still work.
	resp = ts.DoRequest("POST", pathChannelsPrefix+channelB+pathRotateKey, nil, testhelpers.AuthHeaders(userB.AccessToken))
	assert.Equal(t, http.StatusOK, resp.Code)
}

// --- RequestRewrap (#1023) ---

func TestRequestRewrapChannelSuccess(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusAccepted, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["enrolled"])
	assert.Equal(t, "channel", body["kind"])

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, user.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestRequestRewrapDMSuccess(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "rewrapdmA")
	userB := ts.CreateTestUser(t, "rewrapdmB")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
		testhelpers.AuthHeaders(userA.AccessToken))

	assert.Equal(t, http.StatusAccepted, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, true, body["enrolled"])
	assert.Equal(t, "dm", body["kind"])

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestRequestRewrapIdempotent(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	w1 := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusAccepted, w1.Code)

	w2 := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusAccepted, w2.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, user.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestRequestRewrapChannelNonMember(t *testing.T) {
	ts, _, _, channelID := setupEncryptedChannel(t)
	outsider := ts.CreateTestUser(t, "rewrapout")

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, outsider.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRequestRewrapDMNonParticipant(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "rwdmA")
	userB := ts.CreateTestUser(t, "rwdmB")
	outsider := ts.CreateTestUser(t, "rwdmOut")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)
	_ = userB

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
		testhelpers.AuthHeaders(outsider.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)

	// No row inserted
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, outsider.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 0, count)
}

func TestRequestRewrapDMIdempotent(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "rwdmidemA")
	userB := ts.CreateTestUser(t, "rwdmidemB")
	_ = userB
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)

	w1 := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
		testhelpers.AuthHeaders(userA.AccessToken))
	assert.Equal(t, http.StatusAccepted, w1.Code)

	w2 := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
		testhelpers.AuthHeaders(userA.AccessToken))
	assert.Equal(t, http.StatusAccepted, w2.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestRequestRewrapInvalidUUID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rwbadid")

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/not-a-uuid/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRequestRewrapUnknownContext(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rwunknown")

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/00000000-0000-0000-0000-000000000000/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

// --- Auto-enroll on 404 (#1023) ---

func TestGetUnifiedKeysChannelMissingKeyAutoEnrolls(t *testing.T) {
	ts, owner, serverID, channelID := setupEncryptedChannel(t)
	_ = owner

	// A newly-joined member with NO channel_keys row.
	newMember := ts.CreateTestUser(t, "autoenrollchan")
	ts.AddMemberToServer(t, serverID, newMember.ID, roleMember)

	w := ts.DoRequest("GET", "/api/v1/e2ee/keys/"+channelID, nil,
		testhelpers.AuthHeaders(newMember.AccessToken))

	// Response is 404 + pending:true (existing contract preserved)
	assert.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "NO_KEY_YET", body["code"])
	assert.Equal(t, true, body["pending"])

	// AND a pending row was inserted as a side effect
	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM pending_key_requests WHERE channel_id = $1 AND user_id = $2`,
		channelID, newMember.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestGetUnifiedKeysDMMissingKeyAutoEnrolls(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "autoenrolldmA")
	userB := ts.CreateTestUser(t, "autoenrolldmB")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)

	// dm_channel_keys is NOT populated for userA yet.

	w := ts.DoRequest("GET", "/api/v1/e2ee/keys/"+conversationID, nil,
		testhelpers.AuthHeaders(userA.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "NO_KEY_YET", body["code"])
	assert.Equal(t, true, body["pending"])
	assert.Equal(t, "dm", body["kind"])

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

func TestGetUnifiedKeysAutoEnrollIdempotent(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "autoidem1")
	userB := ts.CreateTestUser(t, "autoidem2")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)
	_ = userB

	// First GET — auto-enroll fires
	w1 := ts.DoRequest("GET", "/api/v1/e2ee/keys/"+conversationID, nil,
		testhelpers.AuthHeaders(userA.AccessToken))
	assert.Equal(t, http.StatusNotFound, w1.Code)

	// Second GET — should NOT create a duplicate row
	w2 := ts.DoRequest("GET", "/api/v1/e2ee/keys/"+conversationID, nil,
		testhelpers.AuthHeaders(userA.AccessToken))
	assert.Equal(t, http.StatusNotFound, w2.Code)

	var count int
	err := ts.DB.QueryRow(
		`SELECT COUNT(*) FROM dm_pending_key_requests WHERE conversation_id = $1 AND user_id = $2`,
		conversationID, userA.ID,
	).Scan(&count)
	require.NoError(t, err)
	assert.Equal(t, 1, count)
}

// --- RequestRewrap DB-error branches (#1023) ---
//
// The integration test pattern (mirroring TestGetUnifiedKeys_DM_DBError_*)
// induces real PostgreSQL failures by transiently renaming the tables the
// handler queries. Each test gets a fresh DB via SetupTestServer, so the
// deferred RENAME-back restores schema for cleanup without affecting peers.

// TestRequestRewrapChannelCheckDBError exercises the re_wrap_check_db_error
// branch (channel-existence query failure) at the top of RequestRewrap.
func TestRequestRewrapChannelCheckDBError(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rwchkerr1")
	ctxID := uuid.NewString()

	_, err := ts.DB.Exec(`ALTER TABLE channels RENAME TO channels_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE channels_hidden_for_test RENAME TO channels`); revertErr != nil {
			t.Logf("testhelpers: failed to revert channels rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+ctxID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to process rewrap request", body["error"],
		"channel_check_db_error must envelope to the generic rewrap error message")
}

// TestRequestRewrapDMCheckDBError exercises the re_wrap_check_db_error branch
// reached when the channel check returns false (no channel matches) and the
// DM-participation query then fails. Renaming only dm_conversations leaves
// the initial channels-membership check intact (it returns false because no
// row matches), so execution falls through to the DM check which fails.
func TestRequestRewrapDMCheckDBError(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "rwdmchkerr1")
	ctxID := uuid.NewString()

	_, err := ts.DB.Exec(`ALTER TABLE dm_conversations RENAME TO dm_conversations_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_conversations_hidden_for_test RENAME TO dm_conversations`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_conversations rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+ctxID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to process rewrap request", body["error"])
}

// TestRequestRewrapChannelInsertFailure exercises the re_wrap_insert_db_error
// branch on the channel side: the membership check passes but the INSERT into
// pending_key_requests fails. Simulated by renaming the pending_key_requests
// table after the channel + membership are set up.
func TestRequestRewrapChannelInsertFailure(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	_, err := ts.DB.Exec(`ALTER TABLE pending_key_requests RENAME TO pending_key_requests_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE pending_key_requests_hidden_for_test RENAME TO pending_key_requests`); revertErr != nil {
			t.Logf("testhelpers: failed to revert pending_key_requests rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to enroll rewrap request", body["error"])
}

// TestRequestRewrapDMInsertFailure exercises the re_wrap_insert_db_error
// branch on the DM side: DM participation check passes but INSERT into
// dm_pending_key_requests fails.
func TestRequestRewrapDMInsertFailure(t *testing.T) {
	ts := setupTS(t)
	userA := ts.CreateTestUser(t, "rwdmierrA")
	userB := ts.CreateTestUser(t, "rwdmierrB")
	conversationID := ts.CreateDMConversation(t, userA.ID, userB.ID)

	_, err := ts.DB.Exec(`ALTER TABLE dm_pending_key_requests RENAME TO dm_pending_key_requests_hidden_for_test`)
	require.NoError(t, err)
	defer func() {
		if _, revertErr := ts.DB.Exec(`ALTER TABLE dm_pending_key_requests_hidden_for_test RENAME TO dm_pending_key_requests`); revertErr != nil {
			t.Logf("testhelpers: failed to revert dm_pending_key_requests rename: %v", revertErr)
		}
	}()

	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+conversationID+"/rewrap", nil,
		testhelpers.AuthHeaders(userA.AccessToken))

	assert.Equal(t, http.StatusInternalServerError, w.Code, "body: %s", w.Body.String())
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Failed to enroll rewrap request", body["error"])
}

// TestRequestRewrapRateLimited verifies the per-user 10/min rate limit on the
// new POST /e2ee/keys/:context_id/rewrap route. Mirrors TestRotateKeyRateLimitBlocks11th.
// Without this test a future router-config drift that drops the
// `middleware.RateLimitByUser(...)` wrapper would go undetected.
func TestRequestRewrapRateLimited(t *testing.T) {
	ts, user, _, channelID := setupEncryptedChannel(t)

	// Clear any pre-existing per-user rate-limit counter for this route.
	userRLKey := fmt.Sprintf("ratelimit:user:%s:POST:/api/v1/e2ee/keys/:context_id/rewrap", user.ID)
	ts.Redis.Del(context.Background(), userRLKey)

	// First 10 calls succeed (202 — idempotent enrollment).
	for i := 1; i <= 10; i++ {
		w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
			testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusAccepted, w.Code, fmt.Sprintf("call %d should succeed", i))
	}

	// 11th call is rate-limited by the per-user route middleware.
	w := ts.DoRequest("POST", "/api/v1/e2ee/keys/"+channelID+"/rewrap", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// --- Audio tier server-ceiling validation ---

func TestUpdateChannel_RejectsAudioTierAboveServerCeiling(t *testing.T) {
	// A Groundspeed server's audio ceiling is "standard". Setting "studio" must
	// be rejected with 400 (the server-ceiling guard introduced by #179).
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ceil_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Ceiling Test Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ceiling")

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name":               "voice-ceiling",
		"type":               "voice",
		"audio_quality_tier": "studio",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateChannel_AcceptsAudioTierAtServerCeiling(t *testing.T) {
	// "standard" is at the Groundspeed ceiling → must be accepted (200).
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ceil_ok_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Ceiling OK Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ceiling-ok")

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name":               "voice-ceiling-ok",
		"type":               "voice",
		"audio_quality_tier": "standard",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateChannel_AcceptsStudioAudioTierWhenSelfHosted(t *testing.T) {
	t.Setenv("INSTANCE_TYPE", "self-hosted")
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ceil_selfhost_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Self-hosted Ceiling Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ceiling-selfhost")

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name":               "voice-ceiling-selfhost",
		"type":               "voice",
		"audio_quality_tier": "studio",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUpdateChannel_AcceptsMachAudioTierFromServerTierCache(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "ceil_mach_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Mach Ceiling Server")
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-ceiling-mach")
	require.NoError(t, entitlements.NewServerCache(ts.Redis, ts.DB).
		SetServerTier(context.Background(), serverID, entitlements.TierMach1))

	w := ts.DoRequest("PATCH", pathChannelsPrefix+channelID, map[string]interface{}{
		"name":               "voice-ceiling-mach",
		"type":               "voice",
		"audio_quality_tier": "studio",
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
}
