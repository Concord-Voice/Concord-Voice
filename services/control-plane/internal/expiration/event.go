package expiration

import (
	"time"

	"github.com/google/uuid"
)

// EventKind classifies a completed expiration-policy mutation for the
// durable system-message row described in
// [internal]specs/2026-09-16-1351-expiration-purge-presentation-design.md §3.6.
// The three values line up 1:1 with the renderer's copy
// ("set messages to expire after…" / "changed messages to expire after…" /
// "turned off message expiration").
type EventKind string

const (
	// EventKindSet marks the first time a scope (channel or DM conversation)
	// gets a non-null expiration window.
	EventKindSet EventKind = "set"
	// EventKindChanged marks a window replacing a different, previously
	// non-null window.
	EventKindChanged EventKind = "changed"
	// EventKindCleared marks a previously non-null window being turned off.
	EventKindCleared EventKind = "cleared"
)

// EventPayload is the plaintext JSONB envelope stored in
// messages.expiration_event_payload / dm_messages.expiration_event_payload
// for a type='expiration_event' row. It is server-authored metadata the
// client never decrypts — mirrors dm_messages.call_event_payload (000064)
// in spirit, but is stored under its own dedicated column; see the rationale
// in migration 000137.
type EventPayload struct {
	Kind EventKind `json:"kind"`
	// ActorUserID is the id of the user whose request produced this
	// mutation. Kept as a plain string (rather than uuid.UUID) to match the
	// idiom of the two handlers that populate it — both already carry the
	// authenticated user id as a bare string from gin's context.
	ActorUserID string `json:"actor_user_id"`
	// WindowSeconds is the CURRENT window after this mutation; nil for
	// EventKindCleared.
	WindowSeconds *int `json:"window_seconds"`
	// PreviousWindowSeconds is the window immediately before this mutation,
	// captured under the same lock as the mutation itself (see Transition).
	// nil for EventKindSet (there was no prior window) and for a clear from
	// an already-null state (unreachable in practice: EventKindForTransition
	// never classifies that case as an event at all).
	PreviousWindowSeconds *int `json:"previous_window_seconds,omitempty"`
	// ChangedAt is the same timestamp the policy mutation itself recorded
	// (Transition.Current.UpdatedAt), so the system row's displayed time and
	// the policy's accepted time never diverge.
	ChangedAt time.Time `json:"changed_at"`
}

// EventKindForTransition classifies a completed policy mutation for the
// durable system-message row. ok is false for a request shape that produces
// no event: "resume", which continues a backfill and is not itself a policy
// change, and any mutation that did not actually move the window — a clear
// against an already-null policy, or a set re-affirming the window already in
// force. A durable row is a record that something CHANGED; a no-op has nothing
// to record, and the cleared arm's row is permanent by construction.
func EventKindForTransition(request Request, transition Transition) (EventKind, bool) {
	switch request.Mode {
	case "clear":
		if transition.Previous.WindowSeconds == nil {
			// Nothing was turned off, because nothing was on. Emitting here announced a
			// change that did not occur AND minted a permanent row: a cleared event
			// carries a nil window, so its row is written with expires_at = NULL and the
			// expiry sweeper can never reap it. Repeatable by any permitted actor — in a
			// 1:1 DM, either participant — so the growth was bounded only by the rate
			// limiter. Proven with a PoC during review of this PR.
			return "", false
		}
		return EventKindCleared, true
	case "set":
		if transition.Previous.WindowSeconds == nil {
			return EventKindSet, true
		}
		if transition.Current.WindowSeconds != nil &&
			*transition.Previous.WindowSeconds == *transition.Current.WindowSeconds {
			// Re-setting the window already in force is the same no-op one arm up: it
			// would record "changed" over a policy that did not move.
			return "", false
		}
		return EventKindChanged, true
	default:
		return "", false
	}
}

// EventFor builds the system-row payload for a completed mutation, or reports
// that this transition produced no event at all.
//
// It lives here rather than in each handler because the two call sites were
// byte-identical and the `UpdatedAt` fallback below is the kind of detail that
// drifts silently when it is duplicated: a handler that forgot it would stamp
// the row with its own wall clock instead of the time the policy mutation
// recorded, and the row's displayed time would disagree with the policy's for
// reasons no test would surface.
//
// The false return covers "resume" and any other request that did not change
// the window — EventKindForTransition owns that classification — so a
// backfill-continuation request never produces a spurious row.
func EventFor(request Request, transition Transition, actorUserID string) (EventPayload, bool) {
	kind, ok := EventKindForTransition(request, transition)
	if !ok {
		return EventPayload{}, false
	}
	changedAt := time.Now().UTC()
	if transition.Current.UpdatedAt != nil {
		changedAt = *transition.Current.UpdatedAt
	}
	return EventPayload{
		Kind:                  kind,
		ActorUserID:           actorUserID,
		WindowSeconds:         transition.Current.WindowSeconds,
		PreviousWindowSeconds: transition.Previous.WindowSeconds,
		ChangedAt:             changedAt,
	}, true
}

// StagedEvent is a durable system row that has been written on the caller's
// transaction but not yet broadcast. Present is false when the transition
// produced no event, which is why the broadcast is guarded on it rather than on
// a zero MessageID — a zero UUID is not a sentinel this package wants to own.
type StagedEvent struct {
	MessageID uuid.UUID
	Payload   EventPayload
	Present   bool
}
