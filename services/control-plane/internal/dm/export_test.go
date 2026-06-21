// Package dm — test-only exports for the dm_test external test package
// to inspect or reset internal state without leaking into the public API.
package dm

import (
	"github.com/google/uuid"
)

// ResetPendingDMCallsForTest clears the global pendingDMCalls sync.Map.
// Tests that mutate the map (e.g., RingDMCall tests) MUST call this in a
// t.Cleanup to prevent test-cross-contamination — go test runs tests in a
// shared process so package-level state persists across tests.
func ResetPendingDMCallsForTest() {
	pendingDMCalls.Range(func(key, _ interface{}) bool {
		if v, ok := pendingDMCalls.LoadAndDelete(key); ok {
			if pc, ok := v.(*PendingCall); ok {
				pc.StopTimer()
			}
		}
		return true
	})
}

// PendingDMCallExistsForTest returns true iff pendingDMCalls has an entry
// for the given conversation. Used by tests that verify ring lifecycle.
func PendingDMCallExistsForTest(convID uuid.UUID) bool {
	_, ok := pendingDMCalls.Load(convID)
	return ok
}

// StoreRingForTest seeds a *PendingCall directly into pendingDMCalls
// without going through RingDMCall. Used by onRingTimeout tests so they
// can avoid waiting on the real timer fire.
func StoreRingForTest(convID uuid.UUID, ring *PendingCall) {
	pendingDMCalls.Store(convID, ring)
}

// NewPendingCallForTest is the test-side constructor for *PendingCall
// that exposes the unexported newPendingCall. Used by onRingTimeout
// tests that need a real ring instance.
func NewPendingCallForTest(convID, caller uuid.UUID, callees []uuid.UUID) *PendingCall {
	return newPendingCall(convID, caller, callees, 0)
}

// HandlerOnRingTimeoutForTest exposes the unexported onRingTimeout
// callback so tests can invoke it directly without waiting for the
// real timer. h must be a *Handler.
func HandlerOnRingTimeoutForTest(h *Handler, convID uuid.UUID, ring *PendingCall) {
	h.onRingTimeout(convID, ring)
}
