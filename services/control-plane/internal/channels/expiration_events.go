package channels

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/expiration"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/google/uuid"
)

// expirationEventMessagesType is the messages.type discriminator value for a
// durable expiration policy-change system row. Matches the DM side's
// identical constant in internal/dm/expiration_events.go and migration
// 000137's comment.
const expirationEventMessagesType = "expiration_event"

// wsExpirationEvent is the WebSocket event type broadcast for a channel
// expiration-policy change. There is no pre-existing live delivery for any
// system-message row to mirror (dm_messages call_event rows are written by
// internal/dm/call_events.go with no WS broadcast at all — a call event
// reaches clients only on next fetch), so this is a new, purpose-built event
// rather than a reuse of the generic "message" broadcast: the REST handler
// that produces it has no *websocket.Client to build a message-shaped
// payload from, and a minimal envelope avoids dragging in message-create
// fields (attachments, gif_slug, reply_to_id, …) that do not apply here.
const wsExpirationEvent = "expiration_event"

// insertChannelExpirationEvent persists a durable system-message row for a
// completed channel expiration-policy mutation, on the SAME transaction as
// the policy write itself (see UpdateExpiration). That is the whole
// transactional-correctness story: there is no separate commit for this row,
// so a failure here rolls back the policy write too via the caller's
// deferred tx.Rollback, and a caller that reaches tx.Commit() has committed
// both atomically or neither. Message content is left empty, like a
// call_event row — the meaningful data lives entirely in
// expiration_event_payload, which the client never decrypts (E2EE wraps
// messages.content only). Returns the generated row id so the caller can
// stamp it on the live broadcast for scroll-anchoring (design spec §3.6).
func insertChannelExpirationEvent(
	ctx context.Context,
	tx *sql.Tx,
	channelID, actorUserID string,
	payload expiration.EventPayload,
) (uuid.UUID, error) {
	messageID := uuid.New()
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal channel expiration event payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO messages (id, channel_id, user_id, content, type, expiration_event_payload, created_at, expires_at)
		VALUES ($1, $2, $3, '', $4, $5, $6::timestamptz,
		        CASE WHEN $7::integer IS NULL THEN NULL ELSE $6::timestamptz + make_interval(secs => $7) END)
	`, messageID, channelID, actorUserID, expirationEventMessagesType, payloadJSON, payload.ChangedAt, payload.WindowSeconds); err != nil {
		return uuid.Nil, fmt.Errorf("insert channel expiration_event row: %w", err)
	}
	return messageID, nil
}

// broadcastChannelExpirationEvent sends the durable row to live channel
// subscribers so it renders without a re-fetch. Mirrors the REST-triggered
// message-mutation broadcasts in internal/messages (edit/delete/pin all use
// BroadcastToChannelAuthorized so a subscriber that has since lost channel
// view access is filtered out) rather than the sender-exclusion form used
// for a WS-originated chat send: the acting user's PATCH response carries
// only the Policy, never this row, so they need the broadcast exactly like
// every other viewer.
func (h *Handler) broadcastChannelExpirationEvent(channelID string, messageID uuid.UUID, actorUserID string, payload expiration.EventPayload, policy expiration.Policy) {
	// Both packages guard every other broadcast helper this way. Without it a nil hub
	// panics here — after the policy row has already committed, so the write survives
	// and the request 500s.
	if h.hub == nil {
		return
	}
	channelUUID, err := uuid.Parse(channelID)
	if err != nil {
		// Unreachable in production: channelID is uuid.Parse-validated at the
		// top of UpdateExpiration before this point is ever reached. Fail
		// closed by skipping the broadcast rather than sending a zero UUID.
		return
	}
	// Detached from the request on purpose — the policy write has already committed and
	// this lookup must never fail it — but BOUNDED, because an unbounded context on a
	// stuck pool holds this goroutine with no deadline. A failed lookup degrades to empty
	// names, which the renderer shows as "someone".
	nameCtx, cancelName := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelName()
	actorUsername, actorDisplayName := expiration.ActorName(nameCtx, h.db, actorUserID)
	h.hub.BroadcastToChannelAuthorized(channelUUID, websocket.OutgoingMessage{
		Type: wsExpirationEvent,
		Data: map[string]interface{}{
			"id":                 messageID.String(),
			"channel_id":         channelID,
			"actor_user_id":      actorUserID,
			"actor_username":     actorUsername,
			"actor_display_name": actorDisplayName,
			"kind":               payload.Kind,
			"window_seconds":     payload.WindowSeconds,
			"created_at":         payload.ChangedAt.UTC().Format(time.RFC3339),
			// The policy's own revision travels with the event so the receiving
			// client can advance its composer indicator in the same tick it renders
			// the system row. Without it the row would announce a new window beside
			// a bar still showing the old one until the next fetch. mergeExpirationPolicy
			// fences on revision, so an out-of-order or replayed event is ignored
			// rather than regressing the indicator.
			"revision":         policy.Revision,
			"updated_at":       expiration.UpdatedAtRFC3339(policy),
			"backfill_pending": policy.BackfillPending,
		},
	})
}
