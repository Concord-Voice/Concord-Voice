package messages_test

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathChannels       = "/api/v1/channels/"
	pathAPIMessages    = "/api/v1/messages"
	pathAPIMsgSlash    = "/api/v1/messages/"
	pathSuppressEmbeds = "/suppress-embeds"
)

// =====================================================================
// GetMessages: Edge Cases
// =====================================================================

func TestGetMessagesInvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsgbadch")

	w := ts.DoRequest("GET", pathChannels+"not-a-uuid/messages", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessagesInvalidBeforeParam(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsgbadbefore")
	serverID := ts.CreateTestServer(t, user.ID, "BadBefore Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("GET", pathChannels+channelID+"/messages?before=not-a-uuid", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestGetMessagesCustomLimit(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsglimit")
	serverID := ts.CreateTestServer(t, user.ID, "Limit Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Send 3 messages
	for i := 0; i < 3; i++ {
		w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
			"channel_id": channelID,
			"content":    testhelpers.ValidCiphertext(),
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)
	}

	// Request with limit=2
	w := ts.DoRequest("GET", pathChannels+channelID+"/messages?limit=2", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.Len(t, messages, 2)
}

func TestGetMessagesLimitClampedTo100(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsgclamp")
	serverID := ts.CreateTestServer(t, user.ID, "Clamp Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Request with limit=200 (above max of 100) — should be clamped to default 50
	w := ts.DoRequest("GET", pathChannels+channelID+"/messages?limit=200", nil, testhelpers.AuthHeaders(user.AccessToken))

	// Should succeed (invalid limit value is ignored, uses default)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestGetMessagesWithBeforePagination(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getmsgpaginate")
	serverID := ts.CreateTestServer(t, user.ID, "Paginate Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Send 3 messages in sequence
	var msgIDs []string
	for i := 0; i < 3; i++ {
		w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
			"channel_id": channelID,
			"content":    testhelpers.ValidCiphertext(),
		}, testhelpers.AuthHeaders(user.AccessToken))
		require.Equal(t, http.StatusCreated, w.Code)

		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		msg := body["message"].(map[string]interface{})
		msgIDs = append(msgIDs, msg["id"].(string))
	}

	// Get messages before the last one — should get at least 2
	w := ts.DoRequest("GET", pathChannels+channelID+"/messages?before="+msgIDs[2], nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	assert.GreaterOrEqual(t, len(messages), 2)
}

// =====================================================================
// SendMessage: Edge Cases
// =====================================================================

func TestSendMessageInvalidChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsginvch")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": "not-a-uuid",
		"content":    "Hello",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageMissingContent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsgnocont")
	serverID := ts.CreateTestServer(t, user.ID, "NoCont Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageMissingChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsgnoch")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"content": "Hello world",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageInvalidBody(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsginvbody")

	// Send a non-JSON body
	headers := testhelpers.AuthHeaders(user.AccessToken)
	w := ts.DoRequest("POST", pathAPIMessages, "not json", headers)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageE2EEWithInvalidCiphertext(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendinvcipher")
	serverID := ts.CreateTestServer(t, user.ID, "InvCipher Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	// Content is not valid base64 ciphertext
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    "not-valid-base64-ciphertext",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "ciphertext")
}

func TestSendMessageE2EEWithKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendkeyver")
	serverID := ts.CreateTestServer(t, user.ID, "KeyVer Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 2,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, float64(2), msg["key_version"])
}

func TestSendMessageDefaultKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "senddefkv")
	serverID := ts.CreateTestServer(t, user.ID, "DefKV Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// key_version not set — should default to 1
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, float64(1), msg["key_version"])
}

func TestSendMessageEmbedsSuppressedByDefault(t *testing.T) {
	// Server's allow_embedded_content defaults to false, so embeds_suppressed should be true
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendembed")
	serverID := ts.CreateTestServer(t, user.ID, "Embed Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := body["message"].(map[string]interface{})
	assert.Equal(t, true, msg["embeds_suppressed"])
}

func TestSendMessageNonExistentChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsgnoexist")
	fakeChannelID := uuid.New().String()

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": fakeChannelID,
		"content":    "Hello ghost channel",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// UpdateMessage: Edge Cases
// =====================================================================

func TestUpdateMessageInvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgbadid")

	w := ts.DoRequest("PATCH", "/api/v1/messages/not-a-uuid", map[string]interface{}{
		"content": "Edited content",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgnf")
	fakeID := uuid.New().String()

	w := ts.DoRequest("PATCH", pathAPIMsgSlash+fakeID, map[string]interface{}{
		"content": "Edited content",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUpdateMessageEmptyContent(t *testing.T) {
	ts, user, _, _, msgID := setupWithMessage(t)

	w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content": "",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessageInvalidBody(t *testing.T) {
	ts, user, _, _, msgID := setupWithMessage(t)

	headers := testhelpers.AuthHeaders(user.AccessToken)
	w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, "not json", headers)

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUpdateMessageE2EEEnforcement(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "upde2ee")
	serverID := ts.CreateTestServer(t, user.ID, "UpdateE2EE Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	// Send encrypted message first
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Try to update with plaintext — should fail
	w = ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content": "plaintext update",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["error"], "ciphertext")
}

func TestUpdateMessageE2EESuccess(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "upde2eesuc")
	serverID := ts.CreateTestServer(t, user.ID, "UpdateE2EE2 Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	// Send encrypted message
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Update with valid ciphertext — should succeed
	newCiphertext := testhelpers.ValidCiphertext()
	w = ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content": newCiphertext,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	updatedMsg := body["message"].(map[string]interface{})
	assert.Equal(t, newCiphertext, updatedMsg["content"])
	assert.NotNil(t, updatedMsg["edited_at"])
}

// =====================================================================
// DeleteMessage: Edge Cases
// =====================================================================

func TestDeleteMessageInvalidID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delmsgbadid")

	w := ts.DoRequest("DELETE", "/api/v1/messages/not-a-uuid", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestDeleteMessageNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "delmsgnf")
	fakeID := uuid.New().String()

	w := ts.DoRequest("DELETE", pathAPIMsgSlash+fakeID, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteMessageAlreadyDeleted(t *testing.T) {
	ts, user, _, _, msgID := setupWithMessage(t)

	// Delete once
	w := ts.DoRequest("DELETE", pathAPIMsgSlash+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Delete again — should be not found
	w = ts.DoRequest("DELETE", pathAPIMsgSlash+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestDeleteMessageVerifyGone(t *testing.T) {
	ts, user, _, channelID, msgID := setupWithMessage(t)

	// Delete the message
	w := ts.DoRequest("DELETE", pathAPIMsgSlash+msgID, nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	// Verify the message no longer appears in the channel
	w = ts.DoRequest("GET", pathChannels+channelID+"/messages?limit=50", nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	messages := body["messages"].([]interface{})
	for _, m := range messages {
		msg := m.(map[string]interface{})
		assert.NotEqual(t, msgID, msg["id"], "deleted message should not appear")
	}
}

// =====================================================================
// SuppressEmbeds
// =====================================================================

func TestSuppressEmbedsSuccess(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "suppressown")
	serverID := ts.CreateTestServer(t, owner.ID, "Suppress Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Enable embeds on server so message starts with embeds_suppressed=false
	_, err := ts.DB.Exec(`UPDATE servers SET allow_embedded_content = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)

	// Send a message (embeds allowed since server permits)
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)
	assert.Equal(t, false, msg["embeds_suppressed"])

	// Owner (has admin perms via server ownership) suppresses embeds
	w = ts.DoRequest("POST", pathAPIMsgSlash+msgID+pathSuppressEmbeds, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "suppressed")
}

func TestSuppressEmbedsInvalidMessageID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "suppressbad")

	w := ts.DoRequest("POST", "/api/v1/messages/not-a-uuid/suppress-embeds", nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSuppressEmbedsNotFound(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "suppressnf")
	fakeID := uuid.New().String()

	w := ts.DoRequest("POST", pathAPIMsgSlash+fakeID+pathSuppressEmbeds, nil, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestSuppressEmbedsAlreadySuppressed(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "suppressdup")
	serverID := ts.CreateTestServer(t, owner.ID, "SuppressDup Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Server has allow_embedded_content=false by default, so embeds_suppressed=true already
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Suppress — should be a no-op success
	w = ts.DoRequest("POST", pathAPIMsgSlash+msgID+pathSuppressEmbeds, nil, testhelpers.AuthHeaders(owner.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Contains(t, body["message"], "already")
}

func TestSuppressEmbedsInsufficientPermissions(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "suppressperm1")
	member := ts.CreateTestUser(t, "suppressperm2")
	serverID := ts.CreateTestServer(t, owner.ID, "SuppressPerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Enable embeds so we get a non-suppressed message
	_, err := ts.DB.Exec(`UPDATE servers SET allow_embedded_content = TRUE WHERE id = $1`, serverID)
	require.NoError(t, err)

	// Owner sends a message
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Regular member tries to suppress — should fail (needs PermManageAllMessages)
	w = ts.DoRequest("POST", pathAPIMsgSlash+msgID+pathSuppressEmbeds, nil, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// E2EE Epoch Enforcement
// =====================================================================

func TestSendMessageRevokedEpochRejected(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "epochrevoked")
	serverID := ts.CreateTestServer(t, user.ID, "Epoch Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	// Insert a key revocation for epoch 1
	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by) VALUES ($1, 1, 2, 'member removed', $2)`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	// Try to send with revoked epoch — should be rejected with 409
	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusConflict, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "epoch_revoked", body["code"])
	assert.NotNil(t, body["current_epoch"])
}

// =====================================================================
// GetMessages: RBAC permission denied
// =====================================================================

func TestGetMessagesReadHistoryDenied(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "readhistown")
	member := ts.CreateTestUser(t, "readhistmem")
	serverID := ts.CreateTestServer(t, owner.ID, "ReadHist Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "restricted")

	// Deny ReadMessageHistory for @all role in this channel
	var allRoleID string
	err := ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		serverID,
	).Scan(&allRoleID)
	require.NoError(t, err)

	// PermReadMessageHistory = 1 << 12 = 4096
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, 4096)

	w := ts.DoRequest("GET", pathChannels+channelID+"/messages", nil, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}

// =====================================================================
// SendMessage: RBAC permission denied
// =====================================================================

func TestSendMessagePermissionDenied(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendpermown")
	member := ts.CreateTestUser(t, "sendpermmem")
	serverID := ts.CreateTestServer(t, owner.ID, "SendPerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "readonly")

	// Deny SendMessages for @all role in this channel
	var allRoleID string
	err := ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		serverID,
	).Scan(&allRoleID)
	require.NoError(t, err)

	// PermSendMessages = 1 << 11 = 2048
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, 2048)

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    "Should be denied",
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}
