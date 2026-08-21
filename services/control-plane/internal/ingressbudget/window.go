package ingressbudget

import (
	"sync"
	"time"
)

// Window is a bounded set of recently-seen keys, used to make a replayed message
// cheap rather than free.
//
// It is a WORK-AMPLIFICATION control, not a correctness control. The caller's
// action is already inert on a replayed message; the window only stops the
// replay from buying the attacker fan-out work. That is why degrading under
// pressure -- rotation-on-fill shortening the effective window -- is acceptable
// by construction, and it bounds what the tests may assert.
//
// The CAPACITY bound is the safety property. The TTL is only freshness.
type Window struct {
	mu       sync.Mutex
	capacity int
	ttl      time.Duration
	current  map[string]struct{}
	previous map[string]struct{}
	rotated  time.Time
	now      func() time.Time
}

// NewWindow returns a dedup window holding at most capacity keys per generation,
// so at most 2*capacity in total.
//
// ROTATION IS MARK-DRIVEN, NOT TIME-DRIVEN, and the ttl is therefore a floor on
// how long a key stays Seen rather than a ceiling. Nothing rotates the window on
// a timer: Seen is a pure read (that is the type-level guarantee the caller's
// mark-on-accept invariant rests on, so it must not mutate), so with no Mark
// traffic a key stays Seen indefinitely.
//
// That is the safe direction and is why it is accepted rather than fixed. A
// stale entry only sheds a REPLAY for a key already marked; it cannot admit
// anything, so it cannot reopen the fan-out this window exists to bound. Adding
// rotation to Seen would buy strict expiry at the cost of making Seen mutate,
// which is a worse trade.
//
// An earlier revision of this comment claimed the window rotates "at least every
// ttl". That was false for an idle window (CodeRabbit, PR #2871).
func NewWindow(capacity int, ttl time.Duration) *Window {
	return newWindowAt(capacity, ttl, time.Now)
}

func newWindowAt(capacity int, ttl time.Duration, now func() time.Time) *Window {
	// Clamp rather than trust -- see newBucketAt. A zero capacity is the worst of
	// the three: it does not panic, it silently rotates on every Mark, leaving a
	// two-key window that removes dedup entirely with no error anywhere.
	if capacity < 1 {
		capacity = 1
	}
	if ttl <= 0 {
		ttl = time.Minute
	}
	return &Window{
		capacity: capacity,
		ttl:      ttl,
		current:  make(map[string]struct{}, capacity),
		now:      now,
	}
}

// Seen reports whether the key was marked recently. It does NOT mark -- the
// split from Mark is what lets a caller check before steps that may still
// reject. A nil receiver reports false.
func (w *Window) Seen(key string) bool {
	if w == nil {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, ok := w.current[key]; ok {
		return true
	}
	_, ok := w.previous[key]
	return ok
}

// Mark records the key.
//
// Callers MUST call this only on an accepted message. Marking on a rejection
// path would let a forged message for a still-existing user occupy that user's
// slot and suppress their genuine clear after the real erasure.
func (w *Window) Mark(key string) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	now := w.now()
	if w.rotated.IsZero() {
		w.rotated = now
	}
	if now.Sub(w.rotated) >= w.ttl || len(w.current) >= w.capacity {
		w.previous = w.current
		w.current = make(map[string]struct{}, w.capacity)
		w.rotated = now
	}
	w.current[key] = struct{}{}
}

// residentLen reports total tracked keys across both generations. Test-only
// observability for the capacity bound, which is this type's safety property.
func (w *Window) residentLen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.current) + len(w.previous)
}
