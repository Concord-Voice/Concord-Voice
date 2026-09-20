// Package dmvisibility contains shared DM hidden-range SQL and participant
// locking invariants used by HTTP writers and WebSocket delivery.
package dmvisibility

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/google/uuid"
)

// HiddenRangeFilterForViewerExpr returns the anti-join that excludes messages
// hidden for a fixed, compile-time SQL viewer expression. Callers must never
// pass request data as either identifier or expression.
func HiddenRangeFilterForViewerExpr(alias, viewerExpr string) string {
	return fmt.Sprintf(` AND NOT EXISTS (
  SELECT 1 FROM dm_message_hidden_ranges hr
  WHERE hr.user_id = %[2]s AND hr.conversation_id = %[1]s.conversation_id
    AND %[1]s.created_at >= hr.hidden_from AND %[1]s.created_at < hr.hidden_to
    AND (hr.includes_own OR %[1]s.user_id <> %[2]s))`, alias, viewerExpr)
}

// LockParticipantsForWrite preserves the users -> conversation -> participants
// -> message lock order before a DM message-derived write.
func LockParticipantsForWrite(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID) (err error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT user_id FROM dm_participants
		WHERE conversation_id = $1 ORDER BY user_id FOR UPDATE`, conversationID)
	if err != nil {
		return fmt.Errorf("lock DM participant visibility: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			err = fmt.Errorf("close locked DM participant visibility: %w", closeErr)
		}
	}()
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return fmt.Errorf("scan locked DM participant visibility: %w", err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterate locked DM participant visibility: %w", err)
	}
	return nil
}
