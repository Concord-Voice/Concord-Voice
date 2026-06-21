package messages_test

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	sendMessagePath = "/api/v1/messages"
)

func TestSendMessageWithReply(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replyuser")
	serverID := ts.CreateTestServer(t, user.ID, "Reply Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	originalID := ts.CreateTestMessage(t, channelID, user, "Original message")

	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": originalID,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp struct {
		Message struct {
			ID        string  `json:"id"`
			ReplyToID *string `json:"reply_to_id"`
		} `json:"message"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.NotNil(t, resp.Message.ReplyToID)
	assert.Equal(t, originalID, *resp.Message.ReplyToID)
}

func TestSendMessageReplyInvalidUUID(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "badreplyid")
	serverID := ts.CreateTestServer(t, user.ID, "BadReply Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": "not-a-uuid",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageReplyNonexistent(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "missingreply")
	serverID := ts.CreateTestServer(t, user.ID, "Missing Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	fakeID := "00000000-0000-0000-0000-000000000099"
	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": fakeID,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageReplyCrossChannel(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "crosschannel")
	serverID := ts.CreateTestServer(t, user.ID, "Cross Server")
	channel1 := ts.CreateTestChannel(t, serverID, "channel-1")
	channel2 := ts.CreateTestChannel(t, serverID, "channel-2")
	msgInChannel1 := ts.CreateTestMessage(t, channel1, user, "In channel 1")

	// Try to reply from channel-2 to a message in channel-1
	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channel2,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": msgInChannel1,
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestSendMessageReplyEmptyString(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "emptyreply")
	serverID := ts.CreateTestServer(t, user.ID, "Empty Reply Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Empty string reply_to_id should be treated as no reply
	w := ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": "",
	}, testhelpers.AuthHeaders(user.AccessToken))

	assert.Equal(t, http.StatusCreated, w.Code)

	var resp struct {
		Message struct {
			ReplyToID *string `json:"reply_to_id"`
		} `json:"message"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	assert.Nil(t, resp.Message.ReplyToID)
}

func TestGetMessagesWithReplies(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "getreplymsg")
	serverID := ts.CreateTestServer(t, user.ID, "GetReply Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	originalID := ts.CreateTestMessage(t, channelID, user, "Original")

	// Send a reply
	ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     testhelpers.ValidCiphertext(),
		"reply_to_id": originalID,
	}, testhelpers.AuthHeaders(user.AccessToken))

	// Fetch messages
	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 2)

	// First message in DESC order is the reply (newest)
	var replyMsg struct {
		ID        string                   `json:"id"`
		ReplyToID *string                  `json:"reply_to_id"`
		RepliedTo *models.RepliedToSummary `json:"replied_to"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &replyMsg))
	require.NotNil(t, replyMsg.ReplyToID)
	assert.Equal(t, originalID, *replyMsg.ReplyToID)
	require.NotNil(t, replyMsg.RepliedTo)
	assert.Equal(t, originalID, replyMsg.RepliedTo.ID)
	assert.Equal(t, "Original", replyMsg.RepliedTo.Content)
}

func TestGetMessagesReplyToDeleted(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "deletedreply")
	serverID := ts.CreateTestServer(t, user.ID, "Deleted Server")
	channelID := ts.CreateTestChannel(t, serverID, "general")
	originalID := ts.CreateTestMessage(t, channelID, user, "Will be deleted")

	// Send a reply
	replyCiphertext := testhelpers.ValidCiphertext()
	ts.DoRequest("POST", sendMessagePath, map[string]interface{}{
		"channel_id":  channelID,
		"content":     replyCiphertext,
		"reply_to_id": originalID,
	}, testhelpers.AuthHeaders(user.AccessToken))

	// Delete the original message
	ts.DoRequest("DELETE", "/api/v1/messages/"+originalID, nil,
		testhelpers.AuthHeaders(user.AccessToken))

	// Fetch messages — reply should still exist but replied_to should be nil
	w := ts.DoRequest("GET", "/api/v1/channels/"+channelID+"/messages", nil,
		testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var resp struct {
		Messages []json.RawMessage `json:"messages"`
	}
	testhelpers.ParseJSON(t, w, &resp)
	require.GreaterOrEqual(t, len(resp.Messages), 1)

	// The reply message: reply_to_id is NULL (ON DELETE SET NULL), replied_to is omitted
	var replyMsg struct {
		Content   string                   `json:"content"`
		ReplyToID *string                  `json:"reply_to_id"`
		RepliedTo *models.RepliedToSummary `json:"replied_to"`
	}
	require.NoError(t, json.Unmarshal(resp.Messages[0], &replyMsg))
	assert.Equal(t, replyCiphertext, replyMsg.Content)
	// reply_to_id becomes NULL after original deletion (ON DELETE SET NULL)
	assert.Nil(t, replyMsg.ReplyToID)
	assert.Nil(t, replyMsg.RepliedTo)
}
