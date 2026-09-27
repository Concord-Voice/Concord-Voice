package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathGroupsSuffix  = "/channel-groups"
	pathReorderSuffix = "/channels/reorder"
	invalidUUID       = "not-a-uuid"
)

// setupWithServer creates a test server with an owner user and a server.
func setupWithServer(t *testing.T) (*testhelpers.TestServer, testhelpers.TestUser, string) {
	t.Helper()
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "groupowner")
	serverID := ts.CreateTestServer(t, user.ID, "Group Test Server")
	return ts, user, serverID
}

// groupsPath builds the channel-groups endpoint path for a server.
func groupsPath(serverID string) string {
	return pathServersPrefix + serverID + pathGroupsSuffix
}

// groupPath builds the endpoint path for a specific group.
func groupPath(serverID, groupID string) string {
	return pathServersPrefix + serverID + pathGroupsSuffix + "/" + groupID
}

// reorderPath builds the reorder endpoint path for a server.
func reorderPath(serverID string) string {
	return pathServersPrefix + serverID + pathReorderSuffix
}

// createGroup creates a channel group via the API and returns its ID.
func createGroup(t *testing.T, ts *testhelpers.TestServer, serverID, name, token string) string {
	t.Helper()
	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": name,
	}, testhelpers.AuthHeaders(token))
	require.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := testhelpers.JSONField[map[string]interface{}](t, body, "channel_group")
	return group["id"].(string)
}

// --- List Channel Groups ---

func TestListChannelGroupsEmpty(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	groups := body["channel_groups"].([]interface{})
	assert.Empty(t, groups)
}

func TestListChannelGroupsSuccess(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	createGroup(t, ts, serverID, "Voice Channels", user.AccessToken)
	createGroup(t, ts, serverID, "Text Channels", user.AccessToken)

	w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	groups := body["channel_groups"].([]interface{})
	assert.Len(t, groups, 2)

	// Should be ordered by position
	first := testhelpers.JSONElem[map[string]interface{}](t, groups, 0)
	second := testhelpers.JSONElem[map[string]interface{}](t, groups, 1)
	assert.Equal(t, "Voice Channels", first["name"])
	assert.Equal(t, "Text Channels", second["name"])
}

func TestListChannelGroupsNotMember(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	outsider := ts.CreateTestUser(t, "groupoutsider")

	w := ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(outsider.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestListChannelGroupsInvalidServerID(t *testing.T) {
	ts, user, _ := setupWithServer(t)

	w := ts.DoRequest("GET", groupsPath(invalidUUID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Create Channel Group ---

func TestCreateChannelGroupSuccess(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "New Category",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := testhelpers.JSONField[map[string]interface{}](t, body, "channel_group")
	assert.Equal(t, "New Category", group["name"])
	assert.NotEmpty(t, group["id"])
	assert.Equal(t, float64(0), group["position"])
}

func TestCreateChannelGroupAutoPosition(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Create two groups — second should get position 1
	createGroup(t, ts, serverID, "First", user.AccessToken)

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Second",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusCreated, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := testhelpers.JSONField[map[string]interface{}](t, body, "channel_group")
	assert.Equal(t, float64(1), group["position"])
}

func TestCreateChannelGroupNotAdmin(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	member := ts.CreateTestUser(t, "groupmember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{
		"name": "Unauthorized",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestCreateChannelGroupInvalidBody(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	// Missing name
	w := ts.DoRequest("POST", groupsPath(serverID), map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestCreateChannelGroupInvalidServerID(t *testing.T) {
	ts, user, _ := setupWithServer(t)

	w := ts.DoRequest("POST", groupsPath(invalidUUID), map[string]interface{}{
		"name": "Test",
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Update Channel Group ---

func TestUpdateChannelGroupName(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Old Name", user.AccessToken)

	newName := "New Name"
	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name": newName,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := testhelpers.JSONField[map[string]interface{}](t, body, "channel_group")
	assert.Equal(t, newName, group["name"])
}

func TestUpdateChannelGroupPosition(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Movable", user.AccessToken)

	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"position": 5,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	group := testhelpers.JSONField[map[string]interface{}](t, body, "channel_group")
	assert.Equal(t, float64(5), group["position"])
}

func TestUpdateChannelGroupNotAdmin(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Protected", user.AccessToken)

	member := ts.CreateTestUser(t, "updatemember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("PATCH", groupPath(serverID, groupID), map[string]interface{}{
		"name": "Hacked",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUpdateChannelGroupNotFound(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("PATCH", groupPath(serverID, "00000000-0000-0000-0000-000000000000"), map[string]interface{}{
		"name": "Ghost",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateChannelGroupInvalidID(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("PATCH", groupPath(serverID, invalidUUID), map[string]interface{}{
		"name": "Bad",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Delete Channel Group ---

func TestDeleteChannelGroupSuccess(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Doomed", user.AccessToken)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify it's gone
	w = ts.DoRequest("GET", groupsPath(serverID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	groups := body["channel_groups"].([]interface{})
	assert.Empty(t, groups)
}

func TestDeleteChannelGroupRejectsStaleCredentialEpoch(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Stale epoch group", user.AccessToken)
	staleToken := ts.SimulateStaleEpochWindow(t, user.ID)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(staleToken))
	assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())

	var count int
	require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_groups WHERE id = $1`, groupID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestDeleteChannelGroupNotAdmin(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Protected", user.AccessToken)

	member := ts.CreateTestUser(t, "deletemember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	w := ts.DoRequest("DELETE", groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestDeleteChannelGroupNotFound(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("DELETE", groupPath(serverID, "00000000-0000-0000-0000-000000000000"), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteChannelGroupInvalidID(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("DELETE", groupPath(serverID, invalidUUID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// --- Reorder Channels ---

func TestReorderChannelsSuccess(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "Reorder Group", user.AccessToken)
	channelID := ts.CreateTestChannel(t, serverID, "reorder-chan")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{
				"channel_id": channelID,
				"group_id":   groupID,
				"position":   0,
			},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
}

func TestReorderChannelsUnknownChannelRejected(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{{
			"channel_id": uuid.NewString(),
			"position":   0,
		}},
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// TestReorderChannelsForeignGroupRejected covers CV-CAN-012: a bulk reorder must
// not assign a channel to a category owned by another server.
func TestReorderChannelsForeignGroupRejected(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	channelID := ts.CreateTestChannel(t, serverID, "reorder-foreign")
	otherServerID := ts.CreateTestServer(t, user.ID, "Reorder Other Server")
	foreignGroupID := createGroup(t, ts, otherServerID, "Foreign Group", user.AccessToken)

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{
			{"channel_id": channelID, "group_id": foreignGroupID, "position": 0},
		},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderChannelsNotAdmin(t *testing.T) {
	ts, _, serverID := setupWithServer(t)
	member := ts.CreateTestUser(t, "reordermember")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "reorder-not-admin")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{{"channel_id": channelID, "position": 0}},
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestReorderChannelsInvalidBody(t *testing.T) {
	ts, user, serverID := setupWithServer(t)

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderChannelsMalformedGroupIDReturnsInvalidBody(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	channelID := ts.CreateTestChannel(t, serverID, "reorder-malformed-group")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{
		"channels": []map[string]interface{}{{
			"channel_id": channelID,
			"group_id":   invalidUUID,
			"position":   0,
		}},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.JSONEq(t, `{"error":"Invalid request body"}`, w.Body.String())
}

func TestReorderChannelsInvalidServerID(t *testing.T) {
	ts, user, _ := setupWithServer(t)

	w := ts.DoRequest("PUT", reorderPath(invalidUUID), map[string]interface{}{
		"channels": []map[string]interface{}{},
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderChannelsOversizedBodyRejectedBeforeMutation(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	channels := make([]map[string]interface{}, 501)
	for i := range channels {
		channels[i] = map[string]interface{}{"channel_id": uuid.NewString(), "position": i}
	}

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{"channels": channels}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestReorderChannelsDuplicateIDsRejectedBeforeMutation(t *testing.T) {
	ts, user, serverID := setupWithServer(t)
	channelID := ts.CreateTestChannel(t, serverID, "duplicate-reorder")

	w := ts.DoRequest("PUT", reorderPath(serverID), map[string]interface{}{"channels": []map[string]interface{}{
		{"channel_id": channelID, "position": 0},
		{"channel_id": channelID, "position": 1},
	}}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}
