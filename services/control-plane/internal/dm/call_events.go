// Package dm — call event row persistence for DM voice calls (#1209).
//
// Per spec §6.4 (post-pivot 2026-05-28): call events persist in
// dm_messages with type='call_event' and a plaintext JSONB payload.
// E2EE on dm_messages.content is client-side; call event payload is
// entirely server-known metadata (caller/callees/timestamps/status),
// so it stores plaintext — defense-in-depth at the DB layer is
// delegated to Postgres data-at-rest encryption.
//
// All call_event_payload INSERTs MUST go through insertCallEvent —
// no raw INSERTs against the column from other call sites. This is
// enforced by convention (Go has no compile-time invariant for it).
package dm

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// CallEventStatus is the terminal-state classification persisted in
// call_event_payload.status. Values are matched 1:1 with the spec's
// status enum + the renderer's CallEventMessage rendering branch.
type CallEventStatus string

// Call event status constants — discrete terminal-state values that
// classify how a DM voice call ended. Renderer's CallEventMessage
// component branches on these to choose the display string ("Voice
// call — 5:23" for completed, "Missed voice call" for missed, etc.).
const (
	CallEventCompleted CallEventStatus = "completed"
	CallEventMissed    CallEventStatus = "missed"
	CallEventDeclined  CallEventStatus = "declined"
	CallEventCanceled  CallEventStatus = "canceled"
	CallEventFailed    CallEventStatus = "failed"

	// dmMessagesCallEventType is the dm_messages.type discriminator value
	// for call-event rows. Matches the partial index in migration 000065
	// (idx_dm_messages_conversation_type_callevent WHERE type='call_event').
	dmMessagesCallEventType = "call_event"
)

// CallEventPayload is the cleartext shape stored as plaintext JSONB in
// dm_messages.call_event_payload. RingID identifies the accepted ring for a
// ring-originated call and is uuid.Nil for a direct call; consumers should read
// Status to discriminate rather than relying on RingID presence.
type CallEventPayload struct {
	RingID             uuid.UUID       `json:"ring_id"`
	CallerUserID       uuid.UUID       `json:"caller_user_id"`
	ParticipantUserIDs []uuid.UUID     `json:"participant_user_ids"`
	StartedAt          time.Time       `json:"started_at"`
	EndedAt            time.Time       `json:"ended_at"`
	Status             CallEventStatus `json:"status"`
	DurationSeconds    int             `json:"duration_seconds"`
}

// CompletedCallSummary is the authoritative media-room snapshot emitted with
// voice.room_empty. Unlike dm_voice_participants (live presence), it retains
// every participant after they leave and is tied to one exact room lifecycle.
type CompletedCallSummary struct {
	CallID             uuid.UUID
	RingID             uuid.UUID
	CallerUserID       uuid.UUID
	ParticipantUserIDs []uuid.UUID
	StartedAt          time.Time
	EndedAt            time.Time
}

// insertCallEvent persists a call-event row to dm_messages. The caller_user_id
// is used as the row's user_id (the dm_messages.user_id column is NOT NULL
// per the existing schema; assigning the caller as the "author" of the
// system event matches the call-event semantic — they initiated the ring).
//
// Uses a background-safe context wrapper: callers may pass either a request
// context (handlers) or context.Background() (timer callbacks). Per spec
// §6.1 edge case "Call-event insert on hang-up failure": if the DB insert
// fails the caller logs and proceeds — the call has already ended; missing
// a single history row is best-effort persistence, not a critical failure.
func (h *Handler) insertCallEvent(ctx context.Context, convID uuid.UUID, payload CallEventPayload) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal call event payload: %w", err)
	}

	// dm_messages.content is NOT NULL — store an empty string for call events
	// (the meaningful data lives in call_event_payload). Renderer dispatches
	// on type='call_event' and ignores content for these rows.
	_, err = h.db.ExecContext(ctx, `
		INSERT INTO dm_messages
		  (id, conversation_id, user_id, content, type, call_event_payload, created_at)
		VALUES ($1, $2, $3, '', $4, $5, NOW())
	`, uuid.New(), convID, payload.CallerUserID, dmMessagesCallEventType, payloadJSON)
	if err != nil {
		return fmt.Errorf("insert dm_messages call_event row: %w", err)
	}
	return nil
}

// callEventMissed constructs a CallEventPayload for the ring-timeout
// terminal state. Participants = [caller] only (no callees responded).
func callEventMissed(ring *PendingCall) CallEventPayload {
	return CallEventPayload{
		RingID:             ring.RingID,
		CallerUserID:       ring.CallerUserID,
		ParticipantUserIDs: []uuid.UUID{ring.CallerUserID},
		StartedAt:          ring.RingStartedAt,
		EndedAt:            time.Now(),
		Status:             CallEventMissed,
		DurationSeconds:    0,
	}
}

// callEventDeclined constructs a CallEventPayload for the all-callees-declined
// terminal state. Participants = caller + declined callees (records who was
// invited and who said no).
func callEventDeclined(ring *PendingCall) CallEventPayload {
	ring.mu.Lock()
	defer ring.mu.Unlock()

	participants := make([]uuid.UUID, 0, 1+len(ring.DeclinedUserIDs))
	participants = append(participants, ring.CallerUserID)
	for u := range ring.DeclinedUserIDs {
		participants = append(participants, u)
	}
	return CallEventPayload{
		RingID:             ring.RingID,
		CallerUserID:       ring.CallerUserID,
		ParticipantUserIDs: participants,
		StartedAt:          ring.RingStartedAt,
		EndedAt:            time.Now(),
		Status:             CallEventDeclined,
		DurationSeconds:    0,
	}
}

func loadLiveDMCallParticipants(
	ctx context.Context,
	db *sql.DB,
	convID uuid.UUID,
) ([]uuid.UUID, time.Time, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, joined_at FROM dm_voice_participants
		WHERE conversation_id = $1
		ORDER BY joined_at ASC
	`, convID)
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("fetch dm_voice_participants for completed call event: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var participants []uuid.UUID
	var startedAt time.Time
	first := true
	for rows.Next() {
		var uid uuid.UUID
		var joinedAt time.Time
		if err := rows.Scan(&uid, &joinedAt); err != nil {
			return nil, time.Time{}, fmt.Errorf("scan dm_voice_participant row: %w", err)
		}
		participants = append(participants, uid)
		if first {
			startedAt = joinedAt
			first = false
		}
	}
	if err := rows.Err(); err != nil {
		return nil, time.Time{}, fmt.Errorf("dm_voice_participants iteration: %w", err)
	}
	return participants, startedAt, nil
}

func insertCompletedCallEvent(
	ctx context.Context,
	db *sql.DB,
	convID uuid.UUID,
	messageID uuid.UUID,
	payload CallEventPayload,
	replaceExisting bool,
) error {
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal completed call event payload: %w", err)
	}

	query := `
		INSERT INTO dm_messages
		  (id, conversation_id, user_id, content, type, call_event_payload, created_at)
		VALUES ($1, $2, $3, '', $4, $5, $6)
		ON CONFLICT (id) DO NOTHING
	`
	if replaceExisting {
		// An exact heartbeat fallback can arrive before the media plane's richer
		// room-empty snapshot. The authoritative snapshot upgrades that same call
		// row; a later fallback uses DO NOTHING and cannot downgrade it.
		query = `
			INSERT INTO dm_messages
			  (id, conversation_id, user_id, content, type, call_event_payload, created_at)
			VALUES ($1, $2, $3, '', $4, $5, $6)
			ON CONFLICT (id) DO UPDATE SET
			  user_id = EXCLUDED.user_id,
			  call_event_payload = EXCLUDED.call_event_payload,
			  created_at = EXCLUDED.created_at
			WHERE dm_messages.conversation_id = EXCLUDED.conversation_id
			  AND dm_messages.type = 'call_event'
		`
	}
	_, err = db.ExecContext(
		ctx, query, messageID, convID, payload.CallerUserID,
		dmMessagesCallEventType, payloadJSON, payload.EndedAt,
	)
	if err != nil {
		return fmt.Errorf("insert completed call event row: %w", err)
	}
	return nil
}

// InsertCompletedCallEvent persists one idempotent completed-call row from the
// media plane's terminal room snapshot. The server-authoritative call ID doubles
// as the message ID, so duplicate NATS delivery cannot create duplicate history.
func InsertCompletedCallEvent(
	ctx context.Context,
	db *sql.DB,
	convID uuid.UUID,
	summary CompletedCallSummary,
) error {
	// room_empty is authoritative even when best-effort history persistence
	// fails. Never let an insert error retain the short-lived local handoff and
	// block a later call until its timer expires.
	if summary.RingID != uuid.Nil {
		defer forgetAcceptedDMCall(convID, summary.RingID)
	}
	if summary.CallID == uuid.Nil || summary.CallerUserID == uuid.Nil ||
		len(summary.ParticipantUserIDs) == 0 || summary.StartedAt.IsZero() ||
		summary.EndedAt.IsZero() || summary.EndedAt.Before(summary.StartedAt) {
		return fmt.Errorf("invalid completed call summary")
	}

	durationSeconds := int(summary.EndedAt.Sub(summary.StartedAt).Seconds())
	status := CallEventCompleted
	if summary.RingID != uuid.Nil && len(summary.ParticipantUserIDs) < 2 {
		status = CallEventFailed
	}
	payload := CallEventPayload{
		RingID:             summary.RingID,
		CallerUserID:       summary.CallerUserID,
		ParticipantUserIDs: summary.ParticipantUserIDs,
		StartedAt:          summary.StartedAt,
		EndedAt:            summary.EndedAt,
		Status:             status,
		DurationSeconds:    durationSeconds,
	}
	if err := insertCompletedCallEvent(ctx, db, convID, summary.CallID, payload, true); err != nil {
		return err
	}
	return nil
}

// InsertCompletedCallEventForDMHeartbeat persists an exact, idempotent
// presence-derived fallback when an empty heartbeat is the first terminal
// signal. A later authoritative room-empty snapshot upgrades the same call-ID
// row through InsertCompletedCallEvent instead of creating duplicate history.
func InsertCompletedCallEventForDMHeartbeat(
	ctx context.Context,
	db *sql.DB,
	convID, callID, ringID, callerUserID uuid.UUID,
	endedAt time.Time,
) error {
	if callID == uuid.Nil || callerUserID == uuid.Nil || endedAt.IsZero() {
		return fmt.Errorf("invalid completed heartbeat call identity")
	}
	if ringID != uuid.Nil {
		defer forgetAcceptedDMCall(convID, ringID)
	}
	participants, startedAt, err := loadLiveDMCallParticipants(ctx, db, convID)
	if err != nil {
		return err
	}
	if len(participants) == 0 {
		return nil
	}
	if endedAt.Before(startedAt) {
		startedAt = endedAt
	}
	status := CallEventCompleted
	if ringID != uuid.Nil && len(participants) < 2 {
		status = CallEventFailed
	}
	payload := CallEventPayload{
		RingID:             ringID,
		CallerUserID:       callerUserID,
		ParticipantUserIDs: participants,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		Status:             status,
		DurationSeconds:    int(endedAt.Sub(startedAt).Seconds()),
	}
	return insertCompletedCallEvent(ctx, db, convID, callID, payload, false)
}

// InsertCompletedCallEventForDMRoom is a best-effort fallback for a legacy media
// plane that emits voice.room_empty without a terminal room snapshot. Control
// and media plane lifecycle versions are deployed together; this fallback can
// recover history only when live presence still exists and no exact shared call
// owns the conversation. New callers must prefer InsertCompletedCallEvent
// because voice.left may already have erased those rows.
func InsertCompletedCallEventForDMRoom(ctx context.Context, db *sql.DB, convID uuid.UUID) error {
	accepted, hasAccepted := lookupAcceptedDMCallForConversation(convID)
	if hasAccepted {
		// A terminal event must release short-lived caller correlation even when
		// live rows are already gone or best-effort persistence fails.
		defer forgetAcceptedDMCall(convID, accepted.RingID)
	}

	participants, startedAt, err := loadLiveDMCallParticipants(ctx, db, convID)
	if err != nil {
		return err
	}
	if len(participants) == 0 {
		return nil
	}

	callerUserID := participants[0]
	ringID := uuid.Nil
	if hasAccepted {
		callerUserID = accepted.CallerUserID
		ringID = accepted.RingID
	}
	endedAt := time.Now()
	if endedAt.Before(startedAt) {
		// Preserve the best-effort history row during clock skew without
		// persisting a negative duration or future-dating the message row.
		startedAt = endedAt
	}
	payload := CallEventPayload{
		RingID:             ringID,
		CallerUserID:       callerUserID,
		ParticipantUserIDs: participants,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		Status:             CallEventCompleted,
		DurationSeconds:    int(endedAt.Sub(startedAt).Seconds()),
	}
	if err := insertCompletedCallEvent(ctx, db, convID, uuid.New(), payload, false); err != nil {
		return err
	}
	return nil
}

// callEventCanceled constructs a CallEventPayload for the caller-canceled
// terminal state. Participants = [caller] only (call was canceled before
// any callee got a chance to respond — recording each callee's name would
// imply a response they didn't give).
func callEventCanceled(ring *PendingCall) CallEventPayload {
	return CallEventPayload{
		RingID:             ring.RingID,
		CallerUserID:       ring.CallerUserID,
		ParticipantUserIDs: []uuid.UUID{ring.CallerUserID},
		StartedAt:          ring.RingStartedAt,
		EndedAt:            time.Now(),
		Status:             CallEventCanceled,
		DurationSeconds:    0,
	}
}

// database/sql is already used in this file for sql.DB and ExecContext;
// no need for an unused-import sentinel.
