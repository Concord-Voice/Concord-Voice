package ingressbudget

import (
	"sync"
	"time"
)

// KeyedBuckets meters each key independently under a hard bound on how many keys
// it will track.
//
// Bounding is by TWO-GENERATION ROTATION rather than an LRU: when the live
// generation fills, it becomes the previous generation and a fresh one starts.
// That is O(1), needs no ordering structure, and gives approximate recency for
// free.
//
// THE COST, AND ITS ACTUAL JUSTIFICATION. A key rotated out of both generations
// is readmitted with a fresh full-burst bucket, so an attacker who evicts a
// victim key resets that key's budget. What makes this acceptable is the PRICE:
// eviction costs 2*capacity distinct foreign keys to buy one victim key `burst`
// extra tokens -- at the production sizing that is 4096 forged messages for 64,
// a 64:1 overhead against an attacker who could simply have spent those 4096
// messages directly.
//
// An earlier revision justified it instead as "safe because the caller's new-key
// gate still bounds admission". That gate was removed as a denial primitive in
// the same change that introduced this package, so the sentence described a
// control that does not exist. Do not restore that reasoning; the price argument
// above is the one that holds, and it holds without any caller-side gate.
type KeyedBuckets struct {
	mu       sync.Mutex
	refill   time.Duration
	burst    int
	capacity int
	current  map[string]*Bucket
	previous map[string]*Bucket
	now      func() time.Time
}

// NewKeyedBuckets returns a keyed meter tracking at most capacity keys per
// generation, so at most 2*capacity keys in total.
func NewKeyedBuckets(refill time.Duration, burst, capacity int) *KeyedBuckets {
	return newKeyedBucketsAt(refill, burst, capacity, time.Now)
}

func newKeyedBucketsAt(
	refill time.Duration, burst, capacity int, now func() time.Time,
) *KeyedBuckets {
	// Clamp rather than trust. Every current caller passes a package constant,
	// but this type is exported for a future consumer (#2270) and a zero refill
	// panics with an integer divide by zero inside Bucket.Allow -- a long way
	// from the constructor that caused it.
	if refill <= 0 {
		refill = time.Second
	}
	if burst < 1 {
		burst = 1
	}
	if capacity < 1 {
		capacity = 1
	}
	return &KeyedBuckets{
		refill:   refill,
		burst:    burst,
		capacity: capacity,
		current:  make(map[string]*Bucket, capacity),
		now:      now,
	}
}

// Allow consumes one token from the key's bucket. A nil receiver allows; see
// Bucket.Allow for why an unwired gate is a no-op rather than a deny.
func (k *KeyedBuckets) Allow(key string) bool {
	if k == nil {
		return true
	}
	k.mu.Lock()
	bucket := k.bucketForLocked(key)
	k.mu.Unlock()
	// Released before Allow so one key's bucket never contends the map lock.
	// Safe because the bucket is reachable through the returned pointer
	// regardless of a concurrent rotation: rotation can only stop FUTURE lookups
	// finding it, never invalidate a pointer already handed out, and the bucket
	// carries its own mutex.
	return bucket.Allow()
}

// bucketForLocked returns the key's bucket, promoting it out of the previous
// generation or minting a fresh one, rotating first if the live generation is
// full. The caller must hold k.mu.
func (k *KeyedBuckets) bucketForLocked(key string) *Bucket {
	if bucket, ok := k.current[key]; ok {
		return bucket
	}

	// READ THE PREVIOUS GENERATION BEFORE ROTATING, because rotation overwrites
	// it. An earlier revision rotated first and then looked, so a key resident
	// ONLY in previous, arriving while current was full, was silently re-minted
	// with a fresh full burst instead of promoted with its exhausted state --
	// a divergence from this function's own documented order, worth roughly one
	// extra burst per capacity misses.
	promoted, wasResident := k.previous[key]

	// EVERY path below inserts into current, so the capacity check must precede
	// ALL of them -- not just the mint.
	//
	// It originally guarded only the mint, and promotion from the previous
	// generation grew current with no ceiling at all: touch every resident key
	// to promote it, then add capacity fresh keys to force a rotation that
	// carries the whole promoted set forward as the next previous. Measured
	// growth was +capacity per cycle, indefinitely, and reachable by an
	// unauthenticated actor.
	//
	// Locked by TestPromotionCannotGrowCurrentPastCapacity. Note the older
	// TestKeyedBucketsRotateAtCapacity asserts the right property but drives
	// only NEW keys, so it never executed this branch and passed throughout.
	if len(k.current) >= k.capacity {
		k.previous = k.current
		k.current = make(map[string]*Bucket, k.capacity)
	}

	if wasResident {
		k.current[key] = promoted // promote; it stays in previous until rotation
		return promoted
	}
	bucket := newBucketAt(k.refill, k.burst, k.now)
	k.current[key] = bucket
	return bucket
}

// resident reports whether the key is tracked in either generation.
//
// Unexported deliberately. It was exported as Known for the new-key gate that
// was removed as a denial primitive; with that gate gone it had zero production
// callers, and an exported method whose doc describes a caller that does not
// exist is an invitation to rebuild the caller. If #2270 needs residency
// introspection it can export it then, with a live consumer to shape it.
func (k *KeyedBuckets) resident(key string) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if _, ok := k.current[key]; ok {
		return true
	}
	_, ok := k.previous[key]
	return ok
}

// residentLen reports total tracked keys across both generations. Test-only
// observability for the capacity bound, which is this type's safety property.
func (k *KeyedBuckets) residentLen() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.current) + len(k.previous)
}
