package websocket

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLinkDMAttachmentsTxSkipsInvalidAttachmentsAndPreservesPosition(t *testing.T) {
	setup := setupEpochTest(t, false, false)
	messageID := seedDMMessage(t, setup)
	validID := seedMediaFile(t, setup, 2)

	unauthorizedID := uuid.New().String()
	_, err := setup.db.Exec(`
		INSERT INTO media_files
			(id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key,
			 key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'text/plain', 12, $3, 1, $4)`,
		unauthorizedID, setup.user2, "attachments/"+unauthorizedID, setup.convID)
	require.NoError(t, err)

	wrongConversationID := uuid.New().String()
	otherConversationID := uuid.New()
	_, err = setup.db.Exec(`
		INSERT INTO dm_conversations (id, is_group, is_personal, created_by)
		VALUES ($1, false, false, $2)`, otherConversationID, setup.user1)
	require.NoError(t, err)
	_, err = setup.db.Exec(`
		INSERT INTO media_files
			(id, uploader_id, file_type, media_tier, mime_type, file_size, storage_key,
			 key_version, conversation_id)
		VALUES ($1, $2, 'file', 2, 'text/plain', 12, $3, 1, $4)`,
		wrongConversationID, setup.user1, "attachments/"+wrongConversationID, otherConversationID)
	require.NoError(t, err)

	unknownID := uuid.New().String()
	attachmentIDs := []string{unknownID, unauthorizedID, wrongConversationID, validID}
	tx, err := setup.db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	summaries, err := setup.hub.linkDMAttachmentsTx(
		context.Background(), tx, messageID, setup.user1, attachmentIDs, setup.convID,
	)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	require.Len(t, summaries, 1)
	assert.Equal(t, validID, summaries[0].ID)

	rows, err := setup.db.Query(`
		SELECT file_id, position
		FROM dm_message_attachments
		WHERE message_id = $1
		ORDER BY position`, messageID)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, rows.Close()) })

	var linkedID string
	var position int
	require.True(t, rows.Next())
	require.NoError(t, rows.Scan(&linkedID, &position))
	assert.Equal(t, validID, linkedID)
	assert.Equal(t, 3, position, "valid attachment keeps its input position")
	assert.False(t, rows.Next())
	require.NoError(t, rows.Err())

	var invalidLinks int
	require.NoError(t, setup.db.QueryRow(`
		SELECT COUNT(*) FROM dm_message_attachments
		WHERE message_id = $1 AND file_id IN ($2, $3, $4)`,
		messageID, unknownID, unauthorizedID, wrongConversationID).Scan(&invalidLinks))
	assert.Zero(t, invalidLinks)
}
