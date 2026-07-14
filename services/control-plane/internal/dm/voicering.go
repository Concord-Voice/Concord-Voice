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
	terminalOwned   bool
}

type declineTransition uint8

const (
	declineTransitionInactive declineTransition = iota
	declineTransitionNotRinging
	declineTransitionPending
	declineTransitionTerminal
)

// pendingDMCalls is the process-local map of active rings, keyed by
// conversation_id (uuid.UUID). Stored values are *PendingCall.
//
// Consumed by RingDMCall (Task B3 — handlers.go), DeclineDMCall and
// CancelDMCall (B4/B5 — handlers.go), AuthorizeDMVoiceForMediaPlane (B6),
// and the WS disconnect cleanup (B7).
var pendingDMCalls sync.Map

// Ring creation and pending->accepted promotion must be one conversation-level
// critical section. A small striped lock table avoids an unbounded UUID->mutex
// map while keeping unrelated conversations mostly independent.
var dmCallLifecycleLocks [64]sync.Mutex

func dmCallLifecycleLock(convID uuid.UUID) *sync.Mutex {
	return &dmCallLifecycleLocks[int(convID[0])%len(dmCallLifecycleLocks)]
}

// LockDMCallLifecycle serializes ring, authorization, and NATS lifecycle
// transitions for one conversation on this replica. A shared Redis lease fences
// active calls across replicas only after that lease exists; pending rings are
// intentionally local (see the package comment). The returned function must be
// deferred by the caller.
func LockDMCallLifecycle(convID uuid.UUID) func() {
	lock := dmCallLifecycleLock(convID)
	lock.Lock()
	return lock.Unlock
}

// HasLocalPendingDMCall reports whether this replica still owns a pre-accept
// ring for the conversation. Callers that use this as a lifecycle fence must
// hold LockDMCallLifecycle so /ring cannot appear between the check and a later
// shared-lease claim. Cancel/decline/timeout may remove the ring concurrently;
// a conservative true only defers one media event until the next heartbeat.
func HasLocalPendingDMCall(convID uuid.UUID) bool {
	_, ok := pendingDMCalls.Load(convID)
	return ok
}

const acceptedDMCallCorrelationTTL = 60 * time.Second

// acceptedDMCall is the short-lived, server-authoritative bridge between an
// accepted ring and the media room it creates. The renderer may present a ring
// ID, but caller attribution is copied only from this exact live record.
type acceptedDMCall struct {
	ConversationID uuid.UUID
	RingID         uuid.UUID
	CallerUserID   uuid.UUID
	ExpiresAt      time.Time
	timer          *time.Timer
}

// acceptedDMCalls is keyed by conversation ID. A replacement ring atomically
// supersedes the previous record; the old timer uses CompareAndDelete so it
// cannot erase the replacement. Entries expire even when no SFU room is ever
// created.
var acceptedDMCalls sync.Map

func rememberAcceptedDMCall(ring *PendingCall, ttl time.Duration) {
	record := &acceptedDMCall{
		ConversationID: ring.ConversationID,
		RingID:         ring.RingID,
		CallerUserID:   ring.CallerUserID,
		ExpiresAt:      time.Now().Add(ttl),
	}
	record.timer = time.AfterFunc(ttl, func() {
		acceptedDMCalls.CompareAndDelete(ring.ConversationID, record)
	})

	if previous, loaded := acceptedDMCalls.Swap(ring.ConversationID, record); loaded {
		if previousRecord, ok := previous.(*acceptedDMCall); ok && previousRecord.timer != nil {
			previousRecord.timer.Stop()
		}
	}
}

func lookupAcceptedDMCall(convID, ringID uuid.UUID) (*acceptedDMCall, bool) {
	stored, loaded := acceptedDMCalls.Load(convID)
	if !loaded {
		return nil, false
	}
	record, ok := stored.(*acceptedDMCall)
	if !ok || record.RingID != ringID || time.Now().After(record.ExpiresAt) {
		if ok && time.Now().After(record.ExpiresAt) {
			forgetAcceptedDMCall(convID, record.RingID)
		}
		return nil, false
	}
	return record, true
}

func lookupAcceptedDMCallForConversation(convID uuid.UUID) (*acceptedDMCall, bool) {
	stored, loaded := acceptedDMCalls.Load(convID)
	if !loaded {
		return nil, false
	}
	record, ok := stored.(*acceptedDMCall)
	if !ok || time.Now().After(record.ExpiresAt) {
		if ok {
			forgetAcceptedDMCall(convID, record.RingID)
		}
		return nil, false
	}
	return record, true
}

func forgetAcceptedDMCall(convID, ringID uuid.UUID) {
	stored, loaded := acceptedDMCalls.Load(convID)
	if !loaded {
		return
	}
	record, ok := stored.(*acceptedDMCall)
	if !ok || record.RingID != ringID || !acceptedDMCalls.CompareAndDelete(convID, record) {
		return
	}
	if record.timer != nil {
		record.timer.Stop()
	}
}

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

func (p *PendingCall) isCurrentLocked() bool {
	stored, loaded := pendingDMCalls.Load(p.ConversationID)
	return loaded && stored == p
}

// tryAccept claims the terminal transition for one exact live ring. The
// callback runs while the ring lock is held so accepted-call correlation and
// its WebSocket signal become visible before a concurrent accepter can inspect
// the terminal state.
func (p *PendingCall) tryAccept(user uuid.UUID, onAccepted func() error) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminalOwned || !p.isCurrentLocked() {
		return false, nil
	}
	p.terminalOwned = true
	delete(p.RingingUserIDs, user)
	p.AcceptedUserIDs[user] = struct{}{}
	if onAccepted != nil {
		if err := onAccepted(); err != nil {
			p.terminalOwned = false
			delete(p.AcceptedUserIDs, user)
			p.RingingUserIDs[user] = struct{}{}
			return false, err
		}
	}
	return true, nil
}

// tryDecline applies a decline only while this exact ring still owns the map
// entry. onDeclined is enqueued under the same lock so accept/cancel/timeout
// cannot overtake the decline notification. A fully-declined ring reserves the
// terminal transition until finalizeTerminal releases the map entry.
func (p *PendingCall) tryDecline(user uuid.UUID, onDeclined func()) declineTransition {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminalOwned || !p.isCurrentLocked() {
		return declineTransitionInactive
	}
	if _, ringing := p.RingingUserIDs[user]; !ringing {
		return declineTransitionNotRinging
	}
	delete(p.RingingUserIDs, user)
	p.DeclinedUserIDs[user] = struct{}{}
	fullyDeclined := len(p.RingingUserIDs) == 0 && len(p.AcceptedUserIDs) == 0
	if fullyDeclined {
		p.terminalOwned = true
	}
	if onDeclined != nil {
		onDeclined()
	}
	if fullyDeclined {
		return declineTransitionTerminal
	}
	return declineTransitionPending
}

// tryTerminate reserves a cancel/timeout/disconnect terminal transition. The
// map entry intentionally remains until side effects are emitted, preventing a
// replacement ring from being created and then canceled by the older event.
func (p *PendingCall) tryTerminate() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminalOwned || !p.isCurrentLocked() {
		return false
	}
	p.terminalOwned = true
	return true
}

// finalizeTerminal releases a transition claimed by tryAccept, tryDecline, or
// tryTerminate after persistence and broadcasts have completed.
func (p *PendingCall) finalizeTerminal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.terminalOwned {
		return
	}
	pendingDMCalls.CompareAndDelete(p.ConversationID, p)
	if p.TimeoutTimer != nil {
		p.TimeoutTimer.Stop()
	}
}

// loadOrStoreInitializedPendingCall publishes a new ring to the global map
// while holding its transition lock through timer setup and invite enqueue.
// Once another goroutine can load the pointer, terminal transitions block until
// initialize returns, so cancel/accept cannot overtake the invitation.
func loadOrStoreInitializedPendingCall(
	p *PendingCall,
	initialize func(),
) (existing interface{}, loaded bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	existing, loaded = pendingDMCalls.LoadOrStore(p.ConversationID, p)
	if loaded {
		return existing, true
	}
	if initialize != nil {
		initialize()
	}
	return nil, false
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
	p.startTimerLocked(duration, onTimeout)
}

func (p *PendingCall) startTimerLocked(duration time.Duration, onTimeout func()) {
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
