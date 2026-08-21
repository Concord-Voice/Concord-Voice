// Package ingressbudget provides small in-process admission-control primitives
// for bounding unauthenticated ingress work.
//
// The primitives are deliberately IN-PROCESS and REPLICA-SCOPED, and for the
// dedup window that is a CORRECTNESS property rather than an economy: core NATS
// fans every publish to every replica, and each replica holds a different set of
// connected viewers, so a fleet-shared dedup key would let one replica's
// consumption suppress the message on another replica where it was never
// delivered. See [internal]specs/2026-08-21-2854-b1-nats-ingress-budget-design.md
// section 2, row R2.
//
// Zero internal dependencies, following internal/presencecapture, so a future
// consumer (#2270) can import it without inheriting a dependency graph.
package ingressbudget

import (
	"sync"
	"time"
)

// Bucket is a token bucket with lazy, read-time refill: no timer, no goroutine,
// and nothing to prune. Safe for concurrent use.
type Bucket struct {
	mu     sync.Mutex
	refill time.Duration
	burst  int
	tokens int
	last   time.Time
	now    func() time.Time
}

// NewBucket returns a bucket that regains one token every refill interval and
// never banks more than burst.
func NewBucket(refill time.Duration, burst int) *Bucket {
	return newBucketAt(refill, burst, time.Now)
}

// NewBucketWithClock is the injected-clock constructor, exported ONLY so a
// consuming package can make refill-bound assertions deterministic. Production
// code must use NewBucket. A test that meters against the wall clock while each
// admitted message does a database round-trip can cross a refill boundary and
// admit one more than its bound, which is a flake, not a finding.
func NewBucketWithClock(refill time.Duration, burst int, now func() time.Time) *Bucket {
	return newBucketAt(refill, burst, now)
}

// newBucketAt is the injected-clock constructor. Unexported: the fake clock is a
// testability concession, not part of the package's contract.
func newBucketAt(refill time.Duration, burst int, now func() time.Time) *Bucket {
	// Clamp rather than trust. A zero refill panics with an integer divide by
	// zero inside Allow -- a long way from the constructor that caused it. Every
	// current caller passes a package constant, but this type is exported for a
	// future consumer (#2270), which is the whole reason the package is a leaf.
	//
	// All THREE constructors clamp. An earlier revision clamped only
	// KeyedBuckets, with a comment whose reasoning applied verbatim to the other
	// two -- NewBucket(0, n).Allow() still panicked and NewWindow(0, ttl)
	// silently degraded to a two-key dedup window.
	if refill <= 0 {
		refill = time.Second
	}
	if burst < 1 {
		burst = 1
	}
	return &Bucket{refill: refill, burst: burst, tokens: burst, now: now}
}

// Allow consumes one token, reporting whether one was available.
//
// A nil receiver ALLOWS. An unwired gate must degrade to the pre-gate behaviour
// rather than refusing traffic — this sits in front of a right-to-erasure path,
// where a wiring bug that silently denied would be worse than one that silently
// permitted. TestNewNATSSubscriberWiresTheIngressGates locks the production
// construction path so the no-op cannot go unnoticed.
func (b *Bucket) Allow() bool {
	if b == nil {
		return true
	}
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	if b.last.IsZero() {
		b.last = now
	}
	// Advance by WHOLE intervals and carry the remainder. Resetting last to now
	// (as websocket/client.go:rateLimitAllow does) discards the remainder and
	// biases the effective rate below the configured one.
	if elapsed := now.Sub(b.last); elapsed >= b.refill {
		gained := int(elapsed / b.refill)
		b.tokens = min(b.tokens+gained, b.burst)
		b.last = b.last.Add(time.Duration(gained) * b.refill)
	}

	if b.tokens <= 0 {
		return false
	}
	b.tokens--
	return true
}
