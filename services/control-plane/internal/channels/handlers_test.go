package channels_test

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
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
	keyChannel             = "channel"
	roleMember             = "member"
	fmtRateLimitChannelKey = "ratelimit:channel_rotate:%s"
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

// --- Rate Limiting: RotateKey ---

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
