// Package dm — expiration-policy-change system row persistence, mirroring
// call_events.go's shape for a different message kind (#1351 purge/expiration
// presentation). See [internal]specs/2026-09-16-1351-expiration-purge-presentation-design.md §3.6.
package dm

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

// expirationEventDMMessagesType is the dm_messages.type discriminator value
// for a durable expiration policy-change system row. dm_messages.type has
// carried 'user' / 'call_event' since 000026/000064-065; this adds a third
// value without touching either existing one. Matches the channel side's
// identical constant in internal/channels/expiration_events.go.
const expirationEventDMMessagesType = "expiration_event"

// wsDMExpirationEvent is the WebSocket event type broadcast for a DM
// expiration-policy change. Named with the "dm_" prefix the rest of this
// package uses for DM-scoped broadcasts (dm_message, dm_voice_call_*, …);
// see internal/channels/expiration_events.go for why this is a new event
// rather than a reuse of an existing one.
const wsDMExpirationEvent = "dm_expiration_event"

// insertDMExpirationEvent persists a durable system-message row for a
// completed DM expiration-policy mutation, on the SAME transaction as the
// policy write itself (see UpdateExpiration). A failure here rolls back the
// policy write too via the caller's deferred tx.Rollback, so the two writes
// can never diverge — there is no separate commit for this row. Message
// content is left empty, like a call_event row (insertCallEvent above):
// the meaningful data lives entirely in expiration_event_payload, which the
// client never decrypts (E2EE wraps dm_messages.content only). Returns the
// generated row id so the caller can stamp it on the live broadcast for
// scroll-anchoring.
func insertDMExpirationEvent(
	ctx context.Context,
	tx *sql.Tx,
	conversationID, actorUserID string,
	payload expiration.EventPayload,
) (uuid.UUID, error) {
	messageID := uuid.New()
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return uuid.Nil, fmt.Errorf("marshal DM expiration event payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO dm_messages (id, conversation_id, user_id, content, type, expiration_event_payload, created_at, expires_at)
		VALUES ($1, $2, $3, '', $4, $5, $6::timestamptz,
		        CASE WHEN $7::integer IS NULL THEN NULL ELSE $6::timestamptz + make_interval(secs => $7) END)
	`, messageID, conversationID, actorUserID, expirationEventDMMessagesType, payloadJSON, payload.ChangedAt, payload.WindowSeconds); err != nil {
		return uuid.Nil, fmt.Errorf("insert dm_messages expiration_event row: %w", err)
	}
	return messageID, nil
}

// broadcastDMExpirationEvent sends the durable row to every participant,
// including the acting user — their PATCH response carries only the Policy,
// never this row, so they need the broadcast exactly like every other
// participant. Uses the Hub's own BroadcastToDMParticipants (no exclusion),
// matching the terminal ring-event broadcasts in handlers.go
// (dm_voice_call_timed_out et al.) rather than the sender-excluding
// h.broadcastToDMParticipants wrapper that dm_message_update/delete use for
// a message the sender already has their own copy of.
func (h *Handler) broadcastDMExpirationEvent(conversationID string, messageID uuid.UUID, actorUserID string, payload expiration.EventPayload, policy expiration.Policy) {
	// Both packages guard every other broadcast helper this way. Without it a nil hub
	// panics here — after the policy row has already committed, so the write survives
	// and the request 500s.
	if h.hub == nil {
		return
	}
	convUUID, err := uuid.Parse(conversationID)
	if err != nil {
		// Unreachable in production: conversationID is uuid.Parse-validated at
		// the top of UpdateExpiration before this point is ever reached. Fail
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
	h.hub.BroadcastToDMParticipants(convUUID, websocket.OutgoingMessage{
		Type: wsDMExpirationEvent,
		Data: map[string]interface{}{
			"id":                 messageID.String(),
			"conversation_id":    conversationID,
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
