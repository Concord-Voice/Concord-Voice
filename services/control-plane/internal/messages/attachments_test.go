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

const mimeImagePNG = "image/png"

func channelMessagesURL(channelID string) string {
	return "/api/v1/channels/" + channelID + "/messages"
}

// insertMediaFile inserts a media_files row directly and returns its ID.
func insertMediaFile(t *testing.T, ts *testhelpers.TestServer, uploaderID, channelID, fileType, mimeType string, fileSize int64) string {
	t.Helper()
	var fileID string
	err := ts.DB.QueryRow(
		`INSERT INTO media_files (id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key, key_version, channel_id, created_at)
		 VALUES (gen_random_uuid(), $1, $2, 2, $3, $4, 'attachments/' || gen_random_uuid(), 1, $5, NOW())
		 RETURNING id`,
		uploaderID, fileType, mimeType, fileSize, channelID,
	).Scan(&fileID)
	require.NoError(t, err)
	return fileID
}

// insertMessageAttachment links a media file to a message.
func insertMessageAttachment(t *testing.T, ts *testhelpers.TestServer, messageID, fileID string, position int) {
	t.Helper()
	_, err := ts.DB.Exec(
		`INSERT INTO message_attachments (message_id, file_id, position) VALUES ($1, $2, $3)`,
		messageID, fileID, position,
	)
	require.NoError(t, err)
}

// insertChannelMessage inserts a message row and returns its ID.
func insertChannelMessage(t *testing.T, ts *testhelpers.TestServer, channelID, userID, content string) string {
	t.Helper()
	var msgID string
	err := ts.DB.QueryRow(
		`INSERT INTO messages (id, channel_id, user_id, content, key_version, embeds_suppressed, created_at, updated_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, 1, false, NOW(), NOW())
		 RETURNING id`,
		channelID, userID, content,
	).Scan(&msgID)
	require.NoError(t, err)
	return msgID
}

func TestGetMessagesReturnsAttachments(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "attachuser1")
	serverID := ts.CreateTestServer(t, user.ID, "AttachServer")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	// Create message and attach a file
	msgID := insertChannelMessage(t, ts, channelID, user.ID, "check this out")
	fileID := insertMediaFile(t, ts, user.ID, channelID, "photo", mimeImagePNG, 12345)
	insertMessageAttachment(t, ts, msgID, fileID, 0)

	// GET messages
	w := ts.DoRequest("GET", channelMessagesURL(channelID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string                     `json:"id"`
			Content     string                     `json:"content"`
			Attachments []models.AttachmentSummary `json:"attachments"`
		} `json:"messages"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &body)
	require.NoError(t, err)

	// Find our message
	var found bool
	for _, msg := range body.Messages {
		if msg.ID == msgID {
			found = true
			require.Len(t, msg.Attachments, 1)
			assert.Equal(t, fileID, msg.Attachments[0].ID)
			assert.Equal(t, "photo", msg.Attachments[0].FileType)
			assert.Equal(t, mimeImagePNG, msg.Attachments[0].MimeType)
			assert.Equal(t, int64(12345), msg.Attachments[0].FileSize)
			break
		}
	}
	assert.True(t, found, "Message with attachment not found")
}

func TestGetMessagesNoAttachments(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "attachuser2")
	serverID := ts.CreateTestServer(t, user.ID, "NoAttachServer")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	insertChannelMessage(t, ts, channelID, user.ID, "no files here")

	w := ts.DoRequest("GET", channelMessagesURL(channelID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string                     `json:"id"`
			Attachments []models.AttachmentSummary `json:"attachments"`
		} `json:"messages"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &body)
	require.NoError(t, err)
	require.NotEmpty(t, body.Messages)
	// Attachments should be nil/empty (omitempty)
	assert.Empty(t, body.Messages[0].Attachments)
}

func TestGetMessagesMultipleAttachmentsOrdered(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "attachuser3")
	serverID := ts.CreateTestServer(t, user.ID, "MultiAttachServer")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	msgID := insertChannelMessage(t, ts, channelID, user.ID, "multi attach")
	file1 := insertMediaFile(t, ts, user.ID, channelID, "photo", mimeImagePNG, 100)
	file2 := insertMediaFile(t, ts, user.ID, channelID, "file", "application/pdf", 200)
	file3 := insertMediaFile(t, ts, user.ID, channelID, "video", "video/mp4", 300)

	// Insert in reverse order to verify position sorting
	insertMessageAttachment(t, ts, msgID, file3, 2)
	insertMessageAttachment(t, ts, msgID, file1, 0)
	insertMessageAttachment(t, ts, msgID, file2, 1)

	w := ts.DoRequest("GET", channelMessagesURL(channelID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string                     `json:"id"`
			Attachments []models.AttachmentSummary `json:"attachments"`
		} `json:"messages"`
	}
	err := json.Unmarshal(w.Body.Bytes(), &body)
	require.NoError(t, err)

	for _, msg := range body.Messages {
		if msg.ID == msgID {
			require.Len(t, msg.Attachments, 3)
			assert.Equal(t, file1, msg.Attachments[0].ID)
			assert.Equal(t, file2, msg.Attachments[1].ID)
			assert.Equal(t, file3, msg.Attachments[2].ID)
			return
		}
	}
	t.Fatal("Message not found")
}

func TestGetMessagesSoftDeletedAttachmentsExcluded(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	user := ts.CreateTestUser(t, "attachuser4")
	serverID := ts.CreateTestServer(t, user.ID, "SoftDelServer")
	channelID := ts.CreateTestChannel(t, serverID, "general")

	msgID := insertChannelMessage(t, ts, channelID, user.ID, "deleted attach")
	fileID := insertMediaFile(t, ts, user.ID, channelID, "photo", mimeImagePNG, 100)
	insertMessageAttachment(t, ts, msgID, fileID, 0)

	// Soft-delete the file
	_, err := ts.DB.Exec(`UPDATE media_files SET deleted_at = NOW() WHERE id = $1`, fileID)
	require.NoError(t, err)

	w := ts.DoRequest("GET", channelMessagesURL(channelID), nil, testhelpers.AuthHeaders(user.AccessToken))
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Messages []struct {
			ID          string                     `json:"id"`
			Attachments []models.AttachmentSummary `json:"attachments"`
		} `json:"messages"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))

	for _, msg := range body.Messages {
		if msg.ID == msgID {
			assert.Empty(t, msg.Attachments)
			return
		}
	}
	t.Fatal("Message not found")
}
