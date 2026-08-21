package ingressbudget

import (
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// A fake clock keeps refill assertions deterministic; wall-clock sleeps in a
// rate-limit test are the classic source of CI flake.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time { return c.t }

func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestClock() *fakeClock {
	return &fakeClock{t: time.Date(2026, 8, 21, 0, 0, 0, 0, time.UTC)}
}

func TestBucketAdmitsExactlyItsBurstThenDenies(t *testing.T) {
	clock := newTestClock()
	b := newBucketAt(time.Second/10, 3, clock.now)

	for i := range 3 {
		require.Truef(t, b.Allow(), "token %d of the burst must be admitted", i+1)
	}
	require.False(t, b.Allow(), "the burst is exhausted; the next call must be denied")
}

func TestBucketRefillsOneTokenPerInterval(t *testing.T) {
	clock := newTestClock()
	b := newBucketAt(time.Second/10, 1, clock.now)

	require.True(t, b.Allow())
	require.False(t, b.Allow())

	clock.advance(100 * time.Millisecond)
	require.True(t, b.Allow(), "one interval elapsed, so exactly one token refilled")
	require.False(t, b.Allow(), "and only one")
}

// The precedent at websocket/client.go:rateLimitAllow resets its fill marker to
// `now`, discarding the sub-interval remainder and biasing the effective rate
// DOWN over time. This bucket advances the marker by whole intervals instead.
// Asserting it here is what stops a later "simplification" back to the
// precedent's shape.
func TestBucketDoesNotDiscardTheSubIntervalRemainder(t *testing.T) {
	clock := newTestClock()
	b := newBucketAt(100*time.Millisecond, 1, clock.now)

	require.True(t, b.Allow())
	clock.advance(150 * time.Millisecond) // one token, 50ms carried
	require.True(t, b.Allow())
	clock.advance(50 * time.Millisecond) // carried 50ms + 50ms = one more token
	require.True(t, b.Allow(), "the carried remainder must count toward the next token")
}

func TestBucketRefillIsCappedAtBurst(t *testing.T) {
	clock := newTestClock()
	b := newBucketAt(time.Second/10, 2, clock.now)

	clock.advance(time.Hour) // would be 36000 tokens uncapped
	require.True(t, b.Allow())
	require.True(t, b.Allow())
	require.False(t, b.Allow(), "an idle bucket must not bank more than its burst")
}

func TestNilBucketAllows(t *testing.T) {
	var b *Bucket
	require.True(t, b.Allow(), "an unwired bucket is a no-op, never a deny")
}

// The clamps, on ALL THREE constructors.
//
// They had no test at all when first added, and were applied to only one of the
// three constructors despite a justification that covered all of them. The two
// unclamped shapes were not theoretical: NewBucket(0, n).Allow() panicked with
// the integer divide-by-zero the clamp comment cites as its reason for
// existing, and NewWindow(0, ttl) silently degraded to a two-key dedup window
// -- a fail-open that removes dedup with no error anywhere.
func TestDegenerateConstructorArgumentsAreClamped(t *testing.T) {
	t.Run("bucket with a zero refill does not panic", func(t *testing.T) {
		b := NewBucket(0, 8)
		require.NotPanics(t, func() { b.Allow() },
			"a zero refill divided by zero inside Allow, far from its cause")
	})

	t.Run("bucket with a zero burst still admits", func(t *testing.T) {
		b := NewBucket(time.Second, 0)
		require.True(t, b.Allow(), "clamped to a burst of one, not to a permanent deny")
	})

	t.Run("window with a zero capacity keeps a usable dedup set", func(t *testing.T) {
		w := NewWindow(0, time.Minute)
		w.Mark("a")
		require.True(t, w.Seen("a"),
			"a zero capacity rotated on every Mark, silently removing dedup")
	})

	t.Run("keyed buckets with degenerate arguments still meter", func(t *testing.T) {
		k := NewKeyedBuckets(0, 0, 0)
		require.True(t, k.Allow("room"), "clamped, not dead")
	})
}

// All three types document themselves "safe for concurrent use" and sit in front
// of an unauthenticated ingress path whose real caller is the NATS dispatcher.
// Nothing exercised that. KeyedBuckets.Allow in particular RELEASES the map lock
// before calling bucket.Allow, and the reasoning for why that is safe lived only
// in a comment.
//
// Uses the real clock deliberately: the fake clock is not itself synchronised,
// so sharing one across goroutines would make the TEST racy rather than the code
// under test. Assertions are therefore on invariants that hold at any refill --
// never on an exact count.
func TestPrimitivesAreSafeForConcurrentUse(t *testing.T) {
	const goroutines, perGoroutine = 8, 2000

	t.Run("bucket never admits more than burst plus refills", func(t *testing.T) {
		b := NewBucket(time.Hour, 16) // refill so slow it cannot fire during the test
		var admitted atomic.Int64
		var wg sync.WaitGroup
		for range goroutines {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for range perGoroutine {
					if b.Allow() {
						admitted.Add(1)
					}
				}
			}()
		}
		wg.Wait()
		require.EqualValues(t, 16, admitted.Load(),
			"exactly the burst, no double-spend under contention")
	})

	t.Run("keyed buckets stay bounded under concurrent distinct keys", func(t *testing.T) {
		k := NewKeyedBuckets(time.Hour, 4, 64)
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := range perGoroutine {
					k.Allow("k-" + strconv.Itoa(g) + "-" + strconv.Itoa(i))
				}
			}(g)
		}
		wg.Wait()
		require.LessOrEqual(t, k.residentLen(), 64*2,
			"the capacity bound must hold under concurrent rotation")
	})

	t.Run("window stays bounded under concurrent marks and reads", func(t *testing.T) {
		w := NewWindow(64, time.Hour)
		var wg sync.WaitGroup
		for g := range goroutines {
			wg.Add(1)
			go func(g int) {
				defer wg.Done()
				for i := range perGoroutine {
					key := "w-" + strconv.Itoa(g) + "-" + strconv.Itoa(i)
					w.Mark(key)
					w.Seen(key)
				}
			}(g)
		}
		wg.Wait()
		require.LessOrEqual(t, w.residentLen(), 64*2,
			"the capacity bound must hold under concurrent rotation")
	})
}
