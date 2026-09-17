package websocket

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedMediaFileTyped inserts a tier-2 media_files row with an explicit
// file_type/mime_type pair. seedMediaFile in hub_attachments_test.go always
// pairs 'photo' with 'image/png', which cannot catch a FileType/MimeType
// swap in the code that reads it back — the two values there are equally
// plausible strings from a "does something get copied" test. Deliberately
// mismatched values here (e.g. 'photo' vs 'image/jpeg') make a swap fail the
// assertion instead of hiding behind it.
func seedMediaFileTyped(t *testing.T, setup *hubTestSetup, fileType, mimeType string) string {
	t.Helper()
	fileID := uuid.New().String()
	_, err := setup.db.Exec(
		`INSERT INTO media_files
		   (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, conversation_id)
		 VALUES ($1, $2, $3, 2, $4, 100, $5, 1, $6)`,
		fileID, setup.user1.String(), fileType, mimeType, "attachments/"+fileID, setup.convID,
	)
	require.NoError(t, err)
	return fileID
}

// dmMessageWithAttachments builds the IncomingMessage handleDMMessage expects
// for a "dm_message" carrying attachment_ids, matching parseAttachmentIDs'
// accepted shape (hub.go).
func dmMessageWithAttachments(setup *hubTestSetup, content string, attachmentIDs ...string) IncomingMessage {
	rawIDs := make([]interface{}, len(attachmentIDs))
	for i, id := range attachmentIDs {
		rawIDs[i] = id
	}
	return IncomingMessage{
		Type:     msgTypeDM,
		UserID:   setup.user1,
		ClientID: setup.client.ID,
		Data: map[string]interface{}{
			keyConversationID: setup.convID,
			keyContent:        content,
			"key_version":     float64(1),
			"attachment_ids":  rawIDs,
		},
	}
}

// registerUnsubscribedParticipant registers user2 as a connected but
// unsubscribed DM participant — the only shape that receives
// dm_unread_notify (see TestSendDMUnreadNotifySendsToUnsubscribedParticipants).
func registerUnsubscribedParticipant(setup *hubTestSetup) *Client {
	clientID := uuid.New()
	client := &Client{
		ID:       clientID,
		UserID:   setup.user2,
		Username: "hubuser2",
		Send:     make(chan []byte, 10),
		Hub:      setup.hub,
		Channels: make(map[uuid.UUID]bool),
	}
	setup.hub.clients[clientID] = client
	setup.hub.userClients[setup.user2] = map[uuid.UUID]bool{clientID: true}
	return client
}

// TestHandleDMMessage_UnreadNotifyCarriesRealAttachmentMetadata drives the
// real dm_message -> handleDMMessage -> sendDMUnreadNotify path end to end.
//
// The three tests added alongside this feature (TestSendDMUnreadNotify_
// CarriesAttachmentMetadata, _OmitsAttachmentKeysWhenAbsent, and
// TestSendDMMentionNotify_CarriesNoLastMessage) all call sendDMUnreadNotify
// directly with a hand-built dmUnreadLastMessage, so none of them reach
// handleDMMessage's mapping at hub.go:4318-4319:
//
//	lastMsg.attachmentType = attachments[0].FileType
//	lastMsg.attachmentMime = attachments[0].MimeType
//
// Swapping FileType and MimeType there left every existing test green. This
// test seeds a real media_files row, sends a real dm_message with
// attachment_ids, and asserts the value that actually reached the wire —
// closing that gap (#2364).
func TestHandleDMMessage_UnreadNotifyCarriesRealAttachmentMetadata(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	fileID := seedMediaFileTyped(t, setup, "photo", "image/jpeg")
	client2 := registerUnsubscribedParticipant(setup)

	setup.hub.handleDMMessage(dmMessageWithAttachments(setup, "check out this photo", fileID))

	notify := readClientMsg(t, client2)
	assert.Equal(t, "dm_unread_notify", notify["type"])
	data, ok := notify["data"].(map[string]interface{})
	require.True(t, ok)
	lm, ok := data["last_message"].(map[string]interface{})
	require.True(t, ok, "last_message must be present in dm_unread_notify")
	assert.Equal(t, "photo", lm["attachment_type"])
	assert.Equal(t, "image/jpeg", lm["attachment_mime"])
}

// TestHandleDMMessage_UnreadNotifyUsesFirstAttachmentWhenMultiple pins that
// the FIRST attachment (input order, not insertion order) wins the preview
// when a message carries more than one. linkAttachmentsToTable reorders
// attachments by file_id for locking and restores input position before
// returning, so this also guards that reordering.
func TestHandleDMMessage_UnreadNotifyUsesFirstAttachmentWhenMultiple(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	fileID0 := seedMediaFileTyped(t, setup, "photo", "image/jpeg")
	fileID1 := seedMediaFileTyped(t, setup, "video", "video/mp4")
	client2 := registerUnsubscribedParticipant(setup)

	setup.hub.handleDMMessage(dmMessageWithAttachments(setup, "two attachments", fileID0, fileID1))

	notify := readClientMsg(t, client2)
	data, ok := notify["data"].(map[string]interface{})
	require.True(t, ok)
	lm, ok := data["last_message"].(map[string]interface{})
	require.True(t, ok, "last_message must be present in dm_unread_notify")
	assert.Equal(t, "photo", lm["attachment_type"], "position 0 attachment must win the preview")
	assert.Equal(t, "image/jpeg", lm["attachment_mime"])
}
