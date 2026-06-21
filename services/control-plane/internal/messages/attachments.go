package messages

import (
	"database/sql"

	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/models"
)

// loadAttachmentsForMessages batch-loads attachment summaries for a set of messages.
// Returns a map from message ID to its attachment summaries. Avoids N+1 queries.
func loadAttachmentsForMessages(db *sql.DB, messageIDs []string) (map[string][]models.AttachmentSummary, error) {
	if len(messageIDs) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT ma.message_id, mf.id, mf.file_type, mf.mime_type, mf.file_size
		FROM message_attachments ma
		INNER JOIN media_files mf ON ma.file_id = mf.id
		WHERE ma.message_id = ANY($1::uuid[])
		  AND mf.deleted_at IS NULL
		ORDER BY ma.message_id, ma.position
	`, pq.Array(messageIDs))
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	result := make(map[string][]models.AttachmentSummary)
	for rows.Next() {
		var msgID string
		var a models.AttachmentSummary
		if err := rows.Scan(&msgID, &a.ID, &a.FileType, &a.MimeType, &a.FileSize); err != nil {
			return nil, err
		}
		result[msgID] = append(result[msgID], a)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	return result, nil
}
