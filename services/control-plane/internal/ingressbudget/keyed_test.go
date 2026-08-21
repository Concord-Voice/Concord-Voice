package ingressbudget

import (
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestKeyedBucketsMeterEachKeyIndependently(t *testing.T) {
	clock := newTestClock()
	k := newKeyedBucketsAt(time.Second/4, 2, 16, clock.now)

	require.True(t, k.Allow("room-a"))
	require.True(t, k.Allow("room-a"))
	require.False(t, k.Allow("room-a"), "room-a exhausted its own burst")
	require.True(t, k.Allow("room-b"), "room-b must be unaffected by room-a")
}

func TestResidencyIsNotAdmission(t *testing.T) {
	clock := newTestClock()
	k := newKeyedBucketsAt(time.Second/4, 1, 16, clock.now)

	require.False(t, k.resident("room-a"), "never seen")
	require.True(t, k.Allow("room-a"))
	require.True(t, k.resident("room-a"), "resident after first admission")

	require.False(t, k.Allow("room-a"), "burst exhausted")
	require.True(t, k.resident("room-a"), "a denial does not evict the key")
}

// The capacity bound is the safety property, and PROMOTION must respect it too.
//
// This is a regression lock on a real defect: the capacity check originally
// guarded only the mint path, so promoting a key out of the previous generation
// grew current with no ceiling. Touch every resident key to promote it, add
// capacity fresh keys to force a rotation that carries the promoted set forward
// as the next previous, repeat -- measured at +capacity per cycle, indefinitely
// (64, 128, 192, 256, 320, 384 ... against a claimed bound of 128).
//
// It was reachable by an unauthenticated actor because a resident key skipped
// the caller's then-existing new-key gate, so promotion cost nothing. BOTH
// halves of that sentence are now historical: the residency probe was
// unexported to resident(), and the new-key gate was removed as a denial
// primitive. The growth defect this test locks was real regardless of either.
//
// TestKeyedBucketsRotateAtCapacity below asserts the same property but drives
// only NEW keys, so it never executed the promote branch and stayed green the
// whole time. Both tests are kept: the bound needs exercising by every path
// that can grow the thing being bounded.
func TestPromotionCannotGrowCurrentPastCapacity(t *testing.T) {
	clock := newTestClock()
	const capacity = 64
	k := newKeyedBucketsAt(time.Second/4, 1, capacity, clock.now)

	live := make([]string, 0, capacity*6)
	for cycle := range 6 {
		for _, key := range live { // every one takes the promote branch
			k.Allow(key)
		}
		for i := range capacity { // force a rotation
			key := "c" + strconv.Itoa(cycle) + "-" + strconv.Itoa(i)
			k.Allow(key)
			live = append(live, key)
		}
		require.LessOrEqualf(t, k.residentLen(), capacity*2,
			"cycle %d: promotion must not grow the map past two generations", cycle)
	}
}

// The capacity bound is the safety property: an attacker minting distinct keys
// must not grow this map without limit.
func TestKeyedBucketsRotateAtCapacity(t *testing.T) {
	clock := newTestClock()
	const capacity = 8
	k := newKeyedBucketsAt(time.Second/4, 1, capacity, clock.now)

	for i := range capacity * 4 {
		require.True(t, k.Allow("room-"+strconv.Itoa(i)))
	}
	require.LessOrEqual(t, k.residentLen(), capacity*2,
		"two generations bound the map at 2x capacity; it must not grow unbounded")
}

// Deliberate assertion of the fail-OPEN arm, so it cannot regress silently into
// a fail-closed surprise.
//
// What makes it acceptable is the PRICE, not a caller-side gate: eviction costs
// 2*capacity distinct foreign keys to buy one victim key a fresh burst. An
// earlier revision of this comment said it was safe "because the caller's
// new-key gate still bounds admission" -- that gate was removed as a denial
// primitive in the same change, so the justification named a control that does
// not exist.
func TestRotatedOutKeyIsAdmittedAgainWithAFreshBucket(t *testing.T) {
	clock := newTestClock()
	const capacity = 4
	k := newKeyedBucketsAt(time.Second/4, 1, capacity, clock.now)

	require.True(t, k.Allow("victim"))
	require.False(t, k.Allow("victim"), "burst of 1 exhausted")

	// Push "victim" out of both generations.
	for i := range capacity * 3 {
		k.Allow("filler-" + strconv.Itoa(i))
	}

	require.False(t, k.resident("victim"), "rotated out of both generations")
	require.True(t, k.Allow("victim"),
		"eviction-then-miss ADMITS with a fresh bucket -- deliberate fail-open")
}

func TestNilKeyedBucketsAllowAndAreNeverResident(t *testing.T) {
	var k *KeyedBuckets
	require.True(t, k.Allow("x"))
	require.False(t, k.resident("x"))
}

// Promotion must preserve the key's EXHAUSTED state, not re-mint it.
//
// Regression lock: an earlier revision rotated before reading the previous
// generation, so rotation overwrote previous and a key resident only there --
// arriving while current was full -- silently got a fresh full burst instead of
// its spent bucket. Worth roughly one extra burst per capacity misses, and a
// direct divergence from bucketForLocked's own documented order.
func TestPromotionPreservesAnExhaustedBucketAcrossARotation(t *testing.T) {
	clock := newTestClock()
	const capacity = 4
	k := newKeyedBucketsAt(time.Second/4, 1, capacity, clock.now)

	// Exhaust the victim, then push it into the PREVIOUS generation exactly once
	// by filling current to capacity with foreign keys.
	require.True(t, k.Allow("victim"))
	require.False(t, k.Allow("victim"), "burst of 1 spent")
	for i := range capacity {
		k.Allow("fill-" + strconv.Itoa(i))
	}
	require.True(t, k.resident("victim"), "still tracked, now in the previous generation")

	// Re-touch it while current is full again, forcing the rotate-and-promote path.
	for i := range capacity - 1 {
		k.Allow("more-" + strconv.Itoa(i))
	}
	require.False(t, k.Allow("victim"),
		"a promoted key must carry its spent bucket across the rotation; "+
			"re-minting it here would hand an evictor a free burst")
}
