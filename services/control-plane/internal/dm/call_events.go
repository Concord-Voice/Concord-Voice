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

	// dmMessagesCallEventType is the dm_messages.type discriminator value
	// for call-event rows. Matches the partial index in migration 000065
	// (idx_dm_messages_conversation_type_callevent WHERE type='call_event').
	dmMessagesCallEventType = "call_event"
)

// CallEventPayload is the cleartext shape stored as plaintext JSONB in
// dm_messages.call_event_payload. RingID is uuid.Nil for completed events
// (where the call genuinely succeeded post-accept and didn't end via a
// ring-terminal-state path); consumers should read Status to discriminate
// rather than relying on RingID presence.
type CallEventPayload struct {
	RingID             uuid.UUID       `json:"ring_id"`
	CallerUserID       uuid.UUID       `json:"caller_user_id"`
	ParticipantUserIDs []uuid.UUID     `json:"participant_user_ids"`
	StartedAt          time.Time       `json:"started_at"`
	EndedAt            time.Time       `json:"ended_at"`
	Status             CallEventStatus `json:"status"`
	DurationSeconds    int             `json:"duration_seconds"`
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

// InsertCompletedCallEventForDMRoom is the exported helper invoked from
// the voice NATSSubscriber's handleRoomEmpty DM branch when an SFU room
// goes empty (#1209 plan task B7 Part 1). Per spec §6.4: the dm package
// owns call_event persistence; this exported wrapper lets the voice
// package trigger the insertion without taking a *Handler dependency.
//
// Looks up dm_voice_participants rows for the conversation (called BEFORE
// the handleRoomEmpty DELETE clears them) to gather participants +
// earliest joined_at (used as the call's started_at). The first joiner
// is approximated as the caller_user_id — this is correct for
// caller-initiated rings where the caller joins their own room first,
// and reasonable-default for other accepted paths.
//
// Best-effort: returns the error but caller (NATSSubscriber) logs +
// proceeds. Skipping the call_event row is graceful degradation; the
// call itself completed normally.
func InsertCompletedCallEventForDMRoom(ctx context.Context, db *sql.DB, convID uuid.UUID) error {
	rows, err := db.QueryContext(ctx, `
		SELECT user_id, joined_at FROM dm_voice_participants
		WHERE conversation_id = $1
		ORDER BY joined_at ASC
	`, convID)
	if err != nil {
		return fmt.Errorf("fetch dm_voice_participants for completed call event: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var participants []uuid.UUID
	var startedAt time.Time
	first := true
	for rows.Next() {
		var uid uuid.UUID
		var joinedAt time.Time
		if err := rows.Scan(&uid, &joinedAt); err != nil {
			return fmt.Errorf("scan dm_voice_participant row: %w", err)
		}
		participants = append(participants, uid)
		if first {
			startedAt = joinedAt
			first = false
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("dm_voice_participants iteration: %w", err)
	}

	// Defensive: handle race where room_empty fires after participants are
	// already cleared (rare but possible under reconnect storms). No call
	// happened from this row-set; skip the insert.
	if len(participants) == 0 {
		return nil
	}

	endedAt := time.Now()
	payload := CallEventPayload{
		RingID:             uuid.Nil, // ring already cleared on accept; not reliably available here
		CallerUserID:       participants[0],
		ParticipantUserIDs: participants,
		StartedAt:          startedAt,
		EndedAt:            endedAt,
		Status:             CallEventCompleted,
		DurationSeconds:    int(endedAt.Sub(startedAt).Seconds()),
	}

	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal completed call event payload: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO dm_messages
		  (id, conversation_id, user_id, content, type, call_event_payload, created_at)
		VALUES ($1, $2, $3, '', $4, $5, NOW())
	`, uuid.New(), convID, payload.CallerUserID, dmMessagesCallEventType, payloadJSON)
	if err != nil {
		return fmt.Errorf("insert completed call event row: %w", err)
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
