package messages_test

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
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
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
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
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
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
		"channel_id":  "not-a-uuid",
		"content":     "Hello",
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageMissingContent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsgnocont")
	serverID := ts.CreateTestServer(t, user.ID, "NoCont Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageMissingChannelID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendmsgnoch")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"content":     "Hello world",
		"key_version": 1,
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
		"channel_id":  channelID,
		"content":     "not-valid-base64-ciphertext",
		"key_version": 1,
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

// #2832: this test previously asserted the opposite — that omitting key_version
// yielded 201 with a server-invented epoch 1. Do NOT restore that. The epoch is
// CLIENT-ATTESTED: if the server substitutes one, the revocation check in
// enforceChannelEpoch runs against a value the sender never claimed, so a client
// that simply omits the field sails past a revoked epoch. Missing key_version is
// rejected, never defaulted.
//
// The 400 body is the generic bind error, not errMsgInvalidKeyVersion: the
// `binding:"required,min=1"` tag on SendMessageRequest.KeyVersion rejects at
// ShouldBindJSON, before enforceE2EE's fail-closed guard can be reached over
// HTTP. That guard is defence-in-depth for non-HTTP callers.
func TestSendMessageRejectsMissingKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "senddefkv")
	serverID := ts.CreateTestServer(t, user.ID, "DefKV Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id": channelID,
		"content":    testhelpers.ValidCiphertext(),
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "Invalid request body", body["error"])
	assert.Zero(t, countChannelMessages(t, ts, channelID), "no message may persist under a server-invented epoch")
}

func TestSendMessageEmbedsSuppressedByDefault(t *testing.T) {
	// Server's allow_embedded_content defaults to false, so embeds_suppressed should be true
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendembed")
	serverID := ts.CreateTestServer(t, user.ID, "Embed Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
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
		"channel_id":  fakeChannelID,
		"content":     "Hello ghost channel",
		"key_version": 1,
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
		"content":     "Edited content",
		"key_version": 1,
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
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Try to update with plaintext — should fail
	w = ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content":     "plaintext update",
		"key_version": 1,
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
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusCreated, w.Code)

	var sendBody map[string]interface{}
	testhelpers.ParseJSON(t, w, &sendBody)
	msg := sendBody["message"].(map[string]interface{})
	msgID := msg["id"].(string)

	// Update with valid ciphertext — should succeed
	newCiphertext := testhelpers.ValidCiphertext()
	w = ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content":     newCiphertext,
		"key_version": 1,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	updatedMsg := body["message"].(map[string]interface{})
	assert.Equal(t, newCiphertext, updatedMsg["content"])
	assert.NotNil(t, updatedMsg["edited_at"])
}

func TestUpdateMessage_StoresKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgkeyversion")
	serverID := ts.CreateTestServer(t, user.ID, "Update Key Version Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	originalCiphertext := testhelpers.ValidCiphertext()
	msgID := ts.CreateTestMessage(t, channelID, user, originalCiphertext)
	editedCiphertext := testhelpers.ValidCiphertext()
	require.NotEqual(t, originalCiphertext, editedCiphertext)

	w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content":     editedCiphertext,
		"key_version": 2,
	}, testhelpers.AuthHeaders(user.AccessToken))
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg, ok := body["message"].(map[string]interface{})
	require.True(t, ok, "response should contain a message object")
	assert.Equal(t, editedCiphertext, msg["content"])
	assert.Equal(t, float64(2), msg["key_version"])

	var storedContent string
	var storedKeyVersion int
	err := ts.DB.QueryRow(
		`SELECT content, key_version FROM messages WHERE id = $1`,
		msgID,
	).Scan(&storedContent, &storedKeyVersion)
	require.NoError(t, err)
	assert.Equal(t, editedCiphertext, storedContent)
	assert.Equal(t, 2, storedKeyVersion)
}

func TestUpdateMessage_RequiresPositiveKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgrequiredkeyversion")
	serverID := ts.CreateTestServer(t, user.ID, "Update Required Key Version Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	originalCiphertext := testhelpers.ValidCiphertext()
	msgID := ts.CreateTestMessage(t, channelID, user, originalCiphertext)

	tests := []struct {
		name string
		body map[string]interface{}
	}{
		{
			name: "missing",
			body: map[string]interface{}{"content": testhelpers.ValidCiphertext()},
		},
		{
			name: "zero",
			body: map[string]interface{}{
				"content":     testhelpers.ValidCiphertext(),
				"key_version": 0,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.DoRequest(
				"PATCH",
				pathAPIMsgSlash+msgID,
				tt.body,
				testhelpers.AuthHeaders(user.AccessToken),
			)
			assert.Equal(t, http.StatusBadRequest, w.Code)

			var storedContent string
			var storedKeyVersion int
			err := ts.DB.QueryRow(
				`SELECT content, key_version FROM messages WHERE id = $1`,
				msgID,
			).Scan(&storedContent, &storedKeyVersion)
			require.NoError(t, err)
			assert.Equal(t, originalCiphertext, storedContent)
			assert.Equal(t, 1, storedKeyVersion)
		})
	}
}

func TestUpdateMessage_RequiresManageOwnMessages(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "updmsgpermowner")
	author := ts.CreateTestUser(t, "updmsgpermauthor")
	serverID := ts.CreateTestServer(t, owner.ID, "Update Permission Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	ts.AddMemberToServer(t, serverID, author.ID, "member")

	originalCiphertext := testhelpers.ValidCiphertext()
	msgID := ts.CreateTestMessage(t, channelID, author, originalCiphertext)
	ts.CreateChannelOverride(
		t,
		channelID,
		"user",
		author.ID,
		0,
		int64(rbac.PermManageOwnMessages),
	)

	editedCiphertext := testhelpers.ValidCiphertext()
	require.NotEqual(t, originalCiphertext, editedCiphertext)
	w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content":     editedCiphertext,
		"key_version": 2,
	}, testhelpers.AuthHeaders(author.AccessToken))
	assert.Equal(t, http.StatusForbidden, w.Code)

	var storedContent string
	var storedKeyVersion int
	err := ts.DB.QueryRow(
		`SELECT content, key_version FROM messages WHERE id = $1`,
		msgID,
	).Scan(&storedContent, &storedKeyVersion)
	require.NoError(t, err)
	assert.Equal(t, originalCiphertext, storedContent)
	assert.Equal(t, 1, storedKeyVersion)
}

func TestUpdateMessage_RejectsRevokedEpoch(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgrevoked")
	serverID := ts.CreateTestServer(t, user.ID, "Update Revoked Epoch Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	originalCiphertext := testhelpers.ValidCiphertext()
	msgID := ts.CreateTestMessage(t, channelID, user, originalCiphertext)

	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 2, 3, 'test', $2)`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	editedCiphertext := testhelpers.ValidCiphertext()
	require.NotEqual(t, originalCiphertext, editedCiphertext)
	w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
		"content":     editedCiphertext,
		"key_version": 2,
	}, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code)

	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	assert.Equal(t, "epoch_revoked", body["code"])
	assert.Equal(t, float64(3), body["current_epoch"])

	var storedContent string
	var storedKeyVersion int
	err = ts.DB.QueryRow(
		`SELECT content, key_version FROM messages WHERE id = $1`,
		msgID,
	).Scan(&storedContent, &storedKeyVersion)
	require.NoError(t, err)
	assert.Equal(t, originalCiphertext, storedContent)
	assert.Equal(t, 1, storedKeyVersion)
}

func TestUpdateMessage_SerializesWithConcurrentRevocation(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "updmsgconcurrent")
	serverID := ts.CreateTestServer(t, user.ID, "Concurrent Revocation Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")
	originalCiphertext := testhelpers.ValidCiphertext()
	msgID := ts.CreateTestMessage(t, channelID, user, originalCiphertext)

	revocationTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = revocationTx.Rollback() }()
	_, err = revocationTx.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 2, 3, 'concurrent test', $2)`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	result := make(chan int, 1)
	go func() {
		w := ts.DoRequest("PATCH", pathAPIMsgSlash+msgID, map[string]interface{}{
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 2,
		}, testhelpers.AuthHeaders(user.AccessToken))
		result <- w.Code
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%SELECT id FROM channels WHERE id = $1 FOR NO KEY UPDATE%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "edit should wait on the channel epoch lock")

	require.NoError(t, revocationTx.Commit())
	select {
	case status := <-result:
		assert.Equal(t, http.StatusConflict, status)
	case <-time.After(time.Second):
		t.Fatal("edit did not resume after revocation commit")
	}

	var storedContent string
	var storedKeyVersion int
	err = ts.DB.QueryRow(`SELECT content, key_version FROM messages WHERE id = $1`, msgID).Scan(&storedContent, &storedKeyVersion)
	require.NoError(t, err)
	assert.Equal(t, originalCiphertext, storedContent)
	assert.Equal(t, 1, storedKeyVersion)
}

// regression for #2344: a send admitted before a moderation removal must lock the
// membership row before it writes. The removal then either waits and purges the
// committed send, or commits first and makes the send fail without inserting.
func TestSendMessage_RacingMembershipRemovalCannotOutrunPurge(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendrace_owner")
	victim := ts.CreateTestUser(t, "sendrace_victim")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Race Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")

	moderationTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = moderationTx.Rollback() }()
	var locked int
	require.NoError(t, moderationTx.QueryRow(
		`SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2 FOR UPDATE`,
		serverID, victim.ID,
	).Scan(&locked))

	sendResult := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(victim.AccessToken))
		sendResult <- w.Code
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%timed_out_until%FOR SHARE%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "send should wait on the membership lock")

	_, err = moderationTx.Exec(
		`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, victim.ID,
	)
	require.NoError(t, err)
	require.NoError(t, moderationTx.Commit())

	w := ts.DoRequest(http.MethodDelete, purgeServerPath(serverID), map[string]any{
		"range":          "all",
		"target_user_id": victim.ID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	require.Equal(t, http.StatusOK, w.Code, w.Body.String())

	select {
	case status := <-sendResult:
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(time.Second):
		t.Fatal("send did not resume after membership removal")
	}
	assert.Zero(t, countChannelMessages(t, ts, channelID))
	assert.Equal(t, 1, countPurgeAudits(t, ts, serverID))
}

func requireSendWaitsForCredentialGuard(t *testing.T, ts *testhelpers.TestServer, before string) {
	t.Helper()
	require.Eventually(t, func() bool {
		var waiting bool
		err := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%credential_epoch%users%FOR SHARE%'
		)`).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "send should finish preflight before "+before)
}

// regression for #2344: the membership lock must not treat a rejoin as the
// membership row that passed send preflight before the removal.
func TestSendMessage_RejoinedMemberCannotRestoreStaleSend(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendrejoin_owner")
	victim := ts.CreateTestUser(t, "sendrejoin_victim")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Rejoin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")

	userLockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := userLockTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback user lock transaction: %v", rollbackErr)
		}
	}()
	var epoch sql.NullString
	require.NoError(t, userLockTx.QueryRow(
		`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, victim.ID,
	).Scan(&epoch))

	sendResult := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(victim.AccessToken))
		sendResult <- w.Code
	}()

	requireSendWaitsForCredentialGuard(t, ts, "the membership changes")

	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, victim.ID)
	require.NoError(t, err)
	ts.AddMemberToServer(t, serverID, victim.ID, "member")
	require.NoError(t, userLockTx.Commit())

	select {
	case status := <-sendResult:
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(time.Second):
		t.Fatal("stale send did not resume after rejoin")
	}
	assert.Zero(t, countChannelMessages(t, ts, channelID))
}

// regression for #2507: a moderation timeout committed after send preflight
// must be observed while the write transaction locks the membership row.
func TestSendMessage_TimedOutDuringPreflightCannotPersist(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendtimeoutpreflight_owner")
	member := ts.CreateTestUser(t, "sendtimeoutpreflight_member")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Timeout Preflight Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	userLockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := userLockTx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback user lock transaction: %v", rollbackErr)
		}
	}()
	var epoch sql.NullString
	require.NoError(t, userLockTx.QueryRow(
		`SELECT credential_epoch FROM users WHERE id = $1 FOR NO KEY UPDATE`, member.ID,
	).Scan(&epoch))

	sendResult := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(member.AccessToken))
		sendResult <- w.Code
	}()

	requireSendWaitsForCredentialGuard(t, ts, "the timeout")

	_, err = ts.DB.Exec(
		`UPDATE server_members SET timed_out_until = NOW() + INTERVAL '1 hour' WHERE server_id = $1 AND user_id = $2`,
		serverID, member.ID,
	)
	require.NoError(t, err)
	require.NoError(t, userLockTx.Commit())

	select {
	case status := <-sendResult:
		assert.Equal(t, http.StatusForbidden, status)
	case <-time.After(time.Second):
		t.Fatal("timed-out send did not resume")
	}
	assert.Zero(t, countChannelMessages(t, ts, channelID))
}

// regression for #2507: a new membership must not reuse permissions cached
// for the preceding membership incarnation.
func TestSendMessage_RejoinedMemberCannotUseStalePermissionCache(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendcache_owner")
	member := ts.CreateTestUser(t, "sendcache_member")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Cache Rejoin Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	cache := rbac.NewPermissionCache(ts.Redis)
	require.NoError(t, cache.Set(context.Background(), serverID, member.ID, channelID, rbac.PermSendMessages))
	_, err := ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, member.ID)
	require.NoError(t, err)
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermSendMessages))

	w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
	assert.JSONEq(t, `{"error":"Insufficient permissions"}`, w.Body.String())
	assert.Zero(t, countChannelMessages(t, ts, channelID))
}

func TestSendMessage_MembershipLockHonorsTimeout(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendtimeout_owner")
	member := ts.CreateTestUser(t, "sendtimeout_member")
	serverID := ts.CreateTestServer(t, owner.ID, "Send Timeout Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	lockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = lockTx.Rollback() }()
	var locked int
	require.NoError(t, lockTx.QueryRow(
		`SELECT 1 FROM server_members WHERE server_id = $1 AND user_id = $2 FOR UPDATE`,
		serverID, member.ID,
	).Scan(&locked))

	result := make(chan int, 1)
	started := time.Now()
	go func() {
		w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(member.AccessToken))
		result <- w.Code
	}()

	select {
	case status := <-result:
		assert.Equal(t, http.StatusInternalServerError, status)
		assert.GreaterOrEqual(t, time.Since(started), 2*time.Second,
			"membership lock should hold the message write until its context deadline")
	case <-time.After(4 * time.Second):
		t.Fatal("membership lock did not honor the message write timeout")
	}
}

// regression for #2344: once the writer has acquired the membership share lock,
// removal waits for the write to commit and then its purge deletes that message.
func TestSendMessage_MembershipLockWriterFirstIsPurgedAfterRemoval(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "sendfirst_owner")
	victim := ts.CreateTestUser(t, "sendfirst_victim")
	serverID := ts.CreateTestServer(t, owner.ID, "Send First Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	ts.AddMemberToServer(t, serverID, victim.ID, "member")

	messageLockTx, err := ts.DB.Begin()
	require.NoError(t, err)
	defer func() { _ = messageLockTx.Rollback() }()
	_, err = messageLockTx.Exec(`LOCK TABLE messages IN ACCESS EXCLUSIVE MODE`)
	require.NoError(t, err)

	sendResult := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPost, pathAPIMessages, map[string]any{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(victim.AccessToken))
		sendResult <- w.Code
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%INSERT INTO messages%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "send should hold membership while waiting to insert")

	removalResult := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodDelete, "/api/v1/servers/"+serverID+"/members/"+victim.ID,
			map[string]any{"purge_messages": true}, testhelpers.AuthHeaders(owner.AccessToken))
		removalResult <- w.Code
	}()

	require.Eventually(t, func() bool {
		var waiting bool
		queryErr := ts.DB.QueryRow(`SELECT EXISTS (
			SELECT 1 FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND query LIKE '%DELETE FROM server_members%'
		)`).Scan(&waiting)
		return queryErr == nil && waiting
	}, time.Second, 10*time.Millisecond, "removal should wait on the writer's membership lock")

	require.NoError(t, messageLockTx.Commit())
	select {
	case status := <-sendResult:
		assert.Equal(t, http.StatusCreated, status)
	case <-time.After(time.Second):
		t.Fatal("send did not commit after releasing the message lock")
	}
	select {
	case status := <-removalResult:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(time.Second):
		t.Fatal("removal did not finish after the writer committed")
	}
	assert.Zero(t, countChannelMessages(t, ts, channelID))
	assert.Equal(t, 1, countPurgeAudits(t, ts, serverID))
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
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
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
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
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
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 1,
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
	assert.Equal(t, float64(2), body["current_epoch"], "the ledger remains authoritative without an epoch-2 key row")
}

// #2832: every non-positive or non-integer key_version is rejected outright. The
// pre-#2832 handler quietly rewrote such a value to epoch 1, which made the
// revocation lookup run against an epoch the sender never attested to.
func TestSendMessageRejectsInvalidKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendbadkv")
	serverID := ts.CreateTestServer(t, user.ID, "BadKV Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	tests := []struct {
		name       string
		keyVersion interface{}
	}{
		{name: "zero", keyVersion: 0},
		{name: "negative", keyVersion: -1},
		{name: "non-integer string", keyVersion: "1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
				"channel_id":  channelID,
				"content":     testhelpers.ValidCiphertext(),
				"key_version": tt.keyVersion,
			}, testhelpers.AuthHeaders(user.AccessToken))

			assert.Equal(t, http.StatusBadRequest, w.Code)
			assert.Zero(t, countChannelMessages(t, ts, channelID), "no message may persist for a rejected epoch")
		})
	}
}

// #2832: the persisted epoch is the one the client submitted, byte for byte. The
// value is deliberately not 1 — a regression to the old hardcoded default would
// otherwise still satisfy this assertion.
func TestSendMessagePersistsSubmittedKeyVersion(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendkv3")
	serverID := ts.CreateTestServer(t, user.ID, "KV3 Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"key_version": 3,
	}, testhelpers.AuthHeaders(user.AccessToken))

	require.Equal(t, http.StatusCreated, w.Code)
	var body map[string]interface{}
	testhelpers.ParseJSON(t, w, &body)
	msg := testhelpers.JSONField[map[string]interface{}](t, body, "message")
	assert.Equal(t, float64(3), msg["key_version"])

	var storedKeyVersion int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT key_version FROM messages WHERE id = $1`,
		testhelpers.JSONField[string](t, msg, "id"),
	).Scan(&storedKeyVersion))
	assert.Equal(t, 3, storedKeyVersion, "the stored epoch must be the submitted one, not a default")
}

// #2832: revocation is evaluated against the SUBMITTED epoch. Epoch 1 is revoked
// here precisely because it is the value the old code substituted — a send under
// epoch 5 must be judged on epoch 5, and a send under a revoked epoch must be
// refused even though a live successor exists.
func TestSendMessageRevocationUsesSubmittedEpoch(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "sendkvrevoked")
	serverID := ts.CreateTestServer(t, user.ID, "Submitted Epoch Server")
	channelID := ts.CreateTestChannel(t, serverID, "encrypted")

	_, err := ts.DB.Exec(
		`INSERT INTO key_revocations (channel_id, revoked_epoch, successor_epoch, reason, revoked_by)
		 VALUES ($1, 1, 5, 'test', $2)`,
		channelID, user.ID,
	)
	require.NoError(t, err)

	t.Run("unrevoked submitted epoch is accepted", func(t *testing.T) {
		w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 5,
		}, testhelpers.AuthHeaders(user.AccessToken))

		assert.Equal(t, http.StatusCreated, w.Code, "epoch 5 is live; only the defaulted epoch 1 is revoked")
	})

	t.Run("revoked submitted epoch is refused", func(t *testing.T) {
		w := ts.DoRequest("POST", pathAPIMessages, map[string]interface{}{
			"channel_id":  channelID,
			"content":     testhelpers.ValidCiphertext(),
			"key_version": 1,
		}, testhelpers.AuthHeaders(user.AccessToken))

		assert.Equal(t, http.StatusConflict, w.Code)
		var body map[string]interface{}
		testhelpers.ParseJSON(t, w, &body)
		assert.Equal(t, "epoch_revoked", body["code"])
		assert.Equal(t, float64(5), body["current_epoch"])
	})
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
		"channel_id":  channelID,
		"content":     "Should be denied",
		"key_version": 1,
	}, testhelpers.AuthHeaders(member.AccessToken))

	assert.Equal(t, http.StatusForbidden, w.Code)
}
