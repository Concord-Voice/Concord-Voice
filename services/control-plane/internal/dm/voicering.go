// Package dm — DM voice call ring state machinery (#1209).
//
// PendingCall is the ephemeral state of a ringing DM voice call. Its
// lifetime is bounded by the ring timeout (default 45s per spec §6.1);
// the entry is cleared on accept (transition to in-call), all-declined,
// caller-cancel, or timeout.
//
// pendingDMCalls is a process-local sync.Map keyed by conversation_id.
// Per spec §5.4, this is intentionally local-to-replica state — NATS
// bridges the *broadcast* plane across control-plane replicas, but
// the pending-ring tracking itself only lives on the replica that
// handled the original POST /voice/ring. The graceful-degradation
// rationale for not using Redis-backed shared state lives in the spec.
package dm

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// PendingCall tracks the ephemeral state of a ringing DM voice call.
type PendingCall struct {
	RingID          uuid.UUID
	CallerUserID    uuid.UUID
	ConversationID  uuid.UUID
	RingingUserIDs  map[uuid.UUID]struct{}
	DeclinedUserIDs map[uuid.UUID]struct{}
	AcceptedUserIDs map[uuid.UUID]struct{}
	RingStartedAt   time.Time
	TimeoutTimer    *time.Timer
	mu              sync.Mutex
}

// pendingDMCalls is the process-local map of active rings, keyed by
// conversation_id (uuid.UUID). Stored values are *PendingCall.
//
// Consumed by RingDMCall (Task B3 — handlers.go), DeclineDMCall and
// CancelDMCall (B4/B5 — handlers.go), AuthorizeDMVoiceForMediaPlane (B6),
// and the WS disconnect cleanup (B7).
var pendingDMCalls sync.Map

// newPendingCall constructs a PendingCall with the given callee set.
// The TimeoutTimer is NOT armed here; callers invoke ring.StartTimer
// separately once the timeout callback is ready (typically right
// after the ring is published to NATS so that the timer-vs-broadcast
// ordering is well-defined).
func newPendingCall(convID uuid.UUID, caller uuid.UUID, callees []uuid.UUID, _ time.Duration) *PendingCall {
	ringingSet := make(map[uuid.UUID]struct{}, len(callees))
	for _, u := range callees {
		ringingSet[u] = struct{}{}
	}
	return &PendingCall{
		RingID:          uuid.New(),
		CallerUserID:    caller,
		ConversationID:  convID,
		RingingUserIDs:  ringingSet,
		DeclinedUserIDs: make(map[uuid.UUID]struct{}),
		AcceptedUserIDs: make(map[uuid.UUID]struct{}),
		RingStartedAt:   time.Now(),
	}
}

// MarkDeclined moves the callee from the ringing set to the declined set.
// Idempotent: if the user is not in RingingUserIDs (already declined or
// accepted), the declined-set membership is still ensured.
func (p *PendingCall) MarkDeclined(user uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.RingingUserIDs, user)
	p.DeclinedUserIDs[user] = struct{}{}
}

// MarkAccepted moves the callee from the ringing set to the accepted set.
// Idempotent: same semantics as MarkDeclined.
func (p *PendingCall) MarkAccepted(user uuid.UUID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.RingingUserIDs, user)
	p.AcceptedUserIDs[user] = struct{}{}
}

// IsFullyDeclined returns true iff no one is still ringing AND no one
// has accepted. Caller is responsible for calling this AFTER a Mark*
// operation so the snapshot reflects the latest mutation.
func (p *PendingCall) IsFullyDeclined() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.RingingUserIDs) == 0 && len(p.AcceptedUserIDs) == 0
}

// StartTimer arms the ring-timeout timer with the given duration and
// callback. If a timer is already armed, it is stopped first — this
// makes StartTimer safe to call multiple times (e.g., on ring extension).
// The callback is invoked exactly once when the timer fires; callers
// should make the callback itself idempotent if the call may also be
// canceled via accept / decline / explicit cancel.
func (p *PendingCall) StartTimer(duration time.Duration, onTimeout func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.TimeoutTimer != nil {
		p.TimeoutTimer.Stop()
	}
	p.TimeoutTimer = time.AfterFunc(duration, onTimeout)
}

// StopTimer cancels the ring-timeout timer if armed. Safe to call when
// no timer has been started.
func (p *PendingCall) StopTimer() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.TimeoutTimer != nil {
		p.TimeoutTimer.Stop()
	}
}
