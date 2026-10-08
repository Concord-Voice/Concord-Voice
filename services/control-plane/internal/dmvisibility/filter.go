// Package dmvisibility contains shared DM hidden-range SQL and participant
// locking invariants used by HTTP writers and WebSocket delivery.
package dmvisibility

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// HiddenRangeFilterForViewerExpr returns the anti-join that excludes messages
// hidden for a fixed, compile-time SQL viewer expression. Callers must never
// pass request data as either identifier or expression.
//
// A pinned message is never hidden (#3458 §18). The range still covers it, so
// unpinning hides it again from every viewer whose range does. alias must name
// a row that carries pinned_at; a synthetic row stands in for a deleted message
// with the pin state captured before the delete, never a guessed one.
func HiddenRangeFilterForViewerExpr(alias, viewerExpr string) string {
	return fmt.Sprintf(` AND NOT EXISTS (
  SELECT 1 FROM dm_message_hidden_ranges hr
  WHERE hr.user_id = %[2]s AND hr.conversation_id = %[1]s.conversation_id
    AND %[1]s.created_at >= hr.hidden_from AND %[1]s.created_at < hr.hidden_to
    AND %[1]s.pinned_at IS NULL
    AND (hr.includes_own OR %[1]s.user_id <> %[2]s))`, alias, viewerExpr)
}

// Respawn clears hidden_at for every participant who hid the conversation
// before createdAt and returns who was respawned. Callers publish a
// dm_conversation_hidden {hidden_at: null} event to exactly those users after
// the transaction commits: a hidden thread's client has discarded its view and
// unsubscribed, so no message-derived frame can bring the thread back (#2822).
func Respawn(ctx context.Context, tx *sql.Tx, conversationID uuid.UUID, createdAt time.Time) (respawned []uuid.UUID, err error) {
	rows, err := tx.QueryContext(ctx, `
		UPDATE dm_participants SET hidden_at = NULL
		WHERE conversation_id = $1 AND hidden_at < $2
		RETURNING user_id`, conversationID, createdAt)
	if err != nil {
		return nil, fmt.Errorf("respawn DM participant visibility: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			// Never return IDs beside an error: callers publish the list, and
			// an errored transaction rolls back the respawn it describes.
			respawned, err = nil, fmt.Errorf("close respawned DM participants: %w", closeErr)
		}
	}()
	for rows.Next() {
		var userID uuid.UUID
		if err := rows.Scan(&userID); err != nil {
			return nil, fmt.Errorf("scan respawned DM participant: %w", err)
		}
		respawned = append(respawned, userID)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate respawned DM participants: %w", err)
	}
	return respawned, nil
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
