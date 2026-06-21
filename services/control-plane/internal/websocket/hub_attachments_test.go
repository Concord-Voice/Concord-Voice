package websocket

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testFileID    = "file-1"
	testTimestamp = "2026-01-01T00:00:00Z"
)

// --- parseAttachmentIDs tests ---

func TestParseAttachmentIDsNone(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		UserID:   setup.user1,
		Data:     map[string]interface{}{},
	}

	ids, ok := setup.hub.parseAttachmentIDs(msg)
	assert.True(t, ok)
	assert.Nil(t, ids)
}

func TestParseAttachmentIDsValid(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	id1 := uuid.New().String()
	id2 := uuid.New().String()
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		UserID:   setup.user1,
		Data: map[string]interface{}{
			"attachment_ids": []interface{}{id1, id2},
		},
	}

	ids, ok := setup.hub.parseAttachmentIDs(msg)
	assert.True(t, ok)
	assert.Len(t, ids, 2)
	assert.Equal(t, id1, ids[0])
	assert.Equal(t, id2, ids[1])
}

func TestParseAttachmentIDsTooMany(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	sixIDs := make([]interface{}, 6)
	for i := range sixIDs {
		sixIDs[i] = uuid.New().String()
	}
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		UserID:   setup.user1,
		Data: map[string]interface{}{
			"attachment_ids": sixIDs,
		},
	}

	ids, ok := setup.hub.parseAttachmentIDs(msg)
	assert.False(t, ok)
	assert.Nil(t, ids)

	// Should have sent an error to the client
	errMsg := readClientMsg(t, setup.client)
	assert.Equal(t, "error", errMsg["type"])
}

func TestParseAttachmentIDsInvalidUUID(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		UserID:   setup.user1,
		Data: map[string]interface{}{
			"attachment_ids": []interface{}{"not-a-uuid"},
		},
	}

	ids, ok := setup.hub.parseAttachmentIDs(msg)
	assert.False(t, ok)
	assert.Nil(t, ids)
}

func TestParseAttachmentIDsInvalidType(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msg := IncomingMessage{
		ClientID: setup.client.ID,
		UserID:   setup.user1,
		Data: map[string]interface{}{
			"attachment_ids": []interface{}{123}, // not a string
		},
	}

	ids, ok := setup.hub.parseAttachmentIDs(msg)
	assert.False(t, ok)
	assert.Nil(t, ids)
}

// --- verifyAttachmentAccess tests ---

func TestVerifyAttachmentAccessOwnerMismatch(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	ok := setup.hub.verifyAttachmentAccess(testFileID, "other-user", attachmentLinkCtx{
		userID: setup.user1.String(),
	}, nil, nil)
	assert.False(t, ok)
}

func TestVerifyAttachmentAccessChannelMismatch(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	wrongChannel := "wrong-channel-id"
	ok := setup.hub.verifyAttachmentAccess(testFileID, setup.user1.String(), attachmentLinkCtx{
		userID:    setup.user1.String(),
		channelID: "expected-channel-id",
	}, &wrongChannel, nil)
	assert.False(t, ok)
}

func TestVerifyAttachmentAccessConversationMismatch(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	wrongConv := "wrong-conv-id"
	ok := setup.hub.verifyAttachmentAccess(testFileID, setup.user1.String(), attachmentLinkCtx{
		userID:         setup.user1.String(),
		conversationID: "expected-conv-id",
	}, nil, &wrongConv)
	assert.False(t, ok)
}

func TestVerifyAttachmentAccessValid(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	expectedChannel := "ch-1"
	ok := setup.hub.verifyAttachmentAccess(testFileID, setup.user1.String(), attachmentLinkCtx{
		userID:    setup.user1.String(),
		channelID: "ch-1",
	}, &expectedChannel, nil)
	assert.True(t, ok)
}

func TestVerifyAttachmentAccessNilChannelOnFile(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	ok := setup.hub.verifyAttachmentAccess(testFileID, setup.user1.String(), attachmentLinkCtx{
		userID:    setup.user1.String(),
		channelID: "expected-channel",
	}, nil, nil) // file has no channel_id
	assert.False(t, ok)
}

// --- buildMessageBroadcast tests ---

func TestBuildMessageBroadcastBasic(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msgID := uuid.New()
	chID := uuid.New()

	data := setup.hub.buildMessageBroadcast(messageBroadcastCtx{
		messageID:        msgID,
		channelUUID:      chID,
		userID:           setup.user1,
		client:           setup.client,
		input:            &messageInput{content: "hello", keyVersion: 1},
		embedsSuppressed: false,
		createdAt:        testTimestamp,
		updatedAt:        testTimestamp,
	})

	assert.Equal(t, msgID, data["id"])
	assert.Equal(t, chID, data[keyChannelID])
	assert.Equal(t, "hello", data[keyContent])
	assert.Nil(t, data["attachments"])
	assert.Nil(t, data["reply_to_id"])
}

func TestBuildMessageBroadcastWithAttachments(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msgID := uuid.New()
	chID := uuid.New()
	attachments := []models.AttachmentSummary{
		{ID: "att-1", FileType: "photo", MimeType: "image/png", FileSize: 100},
	}

	data := setup.hub.buildMessageBroadcast(messageBroadcastCtx{
		messageID:   msgID,
		channelUUID: chID,
		userID:      setup.user1,
		client:      setup.client,
		input:       &messageInput{content: "see attached", keyVersion: 1},
		createdAt:   testTimestamp,
		updatedAt:   testTimestamp,
		attachments: attachments,
	})

	assert.NotNil(t, data["attachments"])
	atts, ok := data["attachments"].([]models.AttachmentSummary)
	require.True(t, ok)
	assert.Len(t, atts, 1)
	assert.Equal(t, "att-1", atts[0].ID)
}

func TestBuildMessageBroadcastWithReply(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msgID := uuid.New()
	chID := uuid.New()
	replyID := uuid.New().String()

	data := setup.hub.buildMessageBroadcast(messageBroadcastCtx{
		messageID:   msgID,
		channelUUID: chID,
		userID:      setup.user1,
		client:      setup.client,
		input:       &messageInput{content: "replying", keyVersion: 1},
		createdAt:   testTimestamp,
		updatedAt:   testTimestamp,
		replyToID:   &replyID,
	})

	assert.Equal(t, replyID, data["reply_to_id"])
}

// --- sendMessageAck with attachments ---

func TestSendMessageAckWithAttachments(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msgID := uuid.New()
	chID := uuid.New()
	attachments := []models.AttachmentSummary{
		{ID: "att-1", FileType: "photo", MimeType: "image/png", FileSize: 500},
	}

	setup.hub.sendMessageAck(messageAck{
		Client:      setup.client,
		Nonce:       "test-nonce",
		MessageID:   msgID,
		ChannelUUID: chID,
		CreatedAt:   testTimestamp,
		UpdatedAt:   testTimestamp,
		Attachments: attachments,
	})

	msg := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", msg["type"])
	ackData := msg["data"].(map[string]interface{})
	assert.Equal(t, "test-nonce", ackData["nonce"])
	assert.NotNil(t, ackData["attachments"])

	// Verify attachment data was serialized
	attsJSON, _ := json.Marshal(ackData["attachments"])
	var atts []map[string]interface{}
	require.NoError(t, json.Unmarshal(attsJSON, &atts))
	assert.Len(t, atts, 1)
	assert.Equal(t, "att-1", atts[0]["id"])
}

func TestSendMessageAckWithoutAttachments(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	msgID := uuid.New()
	chID := uuid.New()

	setup.hub.sendMessageAck(messageAck{
		Client:      setup.client,
		Nonce:       "nonce-2",
		MessageID:   msgID,
		ChannelUUID: chID,
		CreatedAt:   testTimestamp,
		UpdatedAt:   testTimestamp,
	})

	msg := readClientMsg(t, setup.client)
	assert.Equal(t, "message_ack", msg["type"])
	ackData := msg["data"].(map[string]interface{})
	assert.Nil(t, ackData["attachments"])
}

// --- linkAttachmentsToTable with empty list ---

func TestLinkAttachmentsToTableEmpty(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	result := setup.hub.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: uuid.New(),
		userID:    setup.user1.String(),
		insertSQL: `INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, $3)`,
	}, nil)
	assert.Nil(t, result)
}

func TestLinkAttachmentsToTableEmptySlice(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	result := setup.hub.linkAttachmentsToTable(attachmentLinkCtx{
		messageID: uuid.New(),
		userID:    setup.user1.String(),
		insertSQL: `INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, $3)`,
	}, []string{})
	assert.Nil(t, result)
}
