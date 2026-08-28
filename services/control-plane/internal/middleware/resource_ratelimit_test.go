package middleware_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestIsRateLimitedNilRedis(t *testing.T) {
	blocked, ttl := middleware.IsRateLimited(context.Background(), nil, "test-key", 5, time.Minute)
	assert.False(t, blocked, "nil Redis should fail open")
	assert.Equal(t, time.Duration(0), ttl)
}

func TestIsRateLimitedAllowsUnderLimit(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:under"
	rdb.Del(ctx, key)

	for i := 0; i < 5; i++ {
		blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 5, time.Minute)
		assert.False(t, blocked, "request %d should be allowed", i+1)
	}
}

func TestIsRateLimitedBlocksOverLimit(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:over"
	rdb.Del(ctx, key)

	// Use up the limit
	for i := 0; i < 3; i++ {
		blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
		assert.False(t, blocked)
	}

	// 4th should be blocked
	blocked, ttl := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	assert.True(t, blocked)
	assert.Greater(t, ttl, time.Duration(0))
}

func TestIsRateLimitedIndependentKeys(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	keyA := "test:ratelimit:a"
	keyB := "test:ratelimit:b"
	rdb.Del(ctx, keyA, keyB)

	// Exhaust key A
	for i := 0; i < 2; i++ {
		middleware.IsRateLimited(ctx, rdb, keyA, 2, time.Minute)
	}
	blocked, _ := middleware.IsRateLimited(ctx, rdb, keyA, 2, time.Minute)
	assert.True(t, blocked, "key A should be blocked")

	// Key B should still work
	blocked, _ = middleware.IsRateLimited(ctx, rdb, keyB, 2, time.Minute)
	assert.False(t, blocked, "key B should be independent")
}

func TestRespondRateLimited(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	middleware.RespondRateLimited(c, 14*time.Hour+23*time.Minute, 10)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "10", w.Header().Get("X-RateLimit-Limit"))
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
	assert.NotEmpty(t, w.Header().Get("X-RateLimit-Reset"))

	// Check body
	body := w.Body.String()
	assert.Contains(t, body, "Rate limit exceeded")
	assert.Contains(t, body, "Try again in 14h 23m")
	assert.Contains(t, body, "retry_after")
}

// RespondRateLimited is shared by three callers: DM key rotation, channel key
// rotation, and (since #1218) DM key distribution. It hardcoded "Key rotation
// limit reached", so a distribution 429 told the caller its rotation was
// limited. The message must name no single caller's action.
func TestRespondRateLimitedMessageIsActionAgnostic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/test", nil)

	middleware.RespondRateLimited(c, 45*time.Second, 40)

	body := w.Body.String()
	assert.NotContains(t, body, "rotation",
		"a shared responder must not name one caller's action")
	assert.Contains(t, body, "Rate limit reached")
	assert.Contains(t, body, "Try again in 45s")
}

// failTTLHook fails exactly one Redis command — TTL — and lets everything else
// through. That combination is the whole point: it is the only way to reach
// IsRateLimited's TTL-error arm, since anything that breaks the connection
// breaks INCR first and the function returns before ever reading the TTL.
type failTTLHook struct{}

var errInjectedTTL = errors.New("injected TTL failure")

func (failTTLHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failTTLHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "ttl" {
			cmd.SetErr(errInjectedTTL)
			return errInjectedTTL
		}
		return next(ctx, cmd)
	}
}

func (failTTLHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// failExpireHook fails only EXPIRE, leaving INCR and TTL working. That is the
// one combination that reaches the expiry-repair branch: the counter keeps
// climbing while nothing can ever give it a window to roll over.
type failExpireHook struct{}

var errInjectedExpire = errors.New("injected EXPIRE failure")

func (failExpireHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (failExpireHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if cmd.Name() == "expire" {
			cmd.SetErr(errInjectedExpire)
			return errInjectedExpire
		}
		return next(ctx, cmd)
	}
}

func (failExpireHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A failed expiry repair is the one Redis fault here that does NOT heal itself:
// the counter never gets a TTL, every later call re-enters the repair branch on
// the same -1, and once the count passes the limit the key blocks that resource
// permanently, because no window remains to roll over.
func TestIsRateLimitedFailsOpenWhenExpiryRepairFails(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:expirefail"
	rdb.Del(ctx, key)
	rdb.AddHook(failExpireHook{})

	// Drive well past the limit. Every call lands in the repair branch, since
	// EXPIRE never succeeds and the key therefore never acquires a TTL.
	for i := 0; i < 6; i++ {
		blocked, ttl := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
		assert.False(t, blocked, "call %d must fail open while the counter cannot be given a window", i+1)
		assert.Equal(t, time.Duration(0), ttl)
	}

	// The counter really did climb — the assertions above are not passing
	// because nothing was counted.
	got, err := rdb.Get(ctx, key).Int64()
	require.NoError(t, err)
	assert.Greater(t, got, int64(3), "counter should be over the limit; the fail-open is the point, not an absent count")
}

// missingTTLHook reports -2 (key not found) for TTL, reproducing the key
// expiring in the gap between the INCR and the TTL read. Those are two separate
// round trips, so a counter in its final moment really can vanish between them;
// forcing the value avoids racing an expiry to observe it.
type missingTTLHook struct{}

func (missingTTLHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (missingTTLHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if cmd.Name() == "ttl" {
			if d, ok := cmd.(*redis.DurationCmd); ok {
				d.SetVal(-2)
			}
		}
		return nil
	}
}

func (missingTTLHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A -2 means the counter no longer exists, so its budget has already reset.
// Blocking on the count INCR returned for a key that has since vanished issues
// a full-window 429 to a caller entitled to proceed immediately.
func TestIsRateLimitedAllowsWhenCounterExpiredBeforeTTLRead(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:missingttl"
	rdb.Del(ctx, key)

	for i := 0; i < 3; i++ {
		blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
		assert.False(t, blocked)
	}
	// Over budget while the key is genuinely present, so the assertion below
	// cannot pass merely because nothing was counted.
	blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	require.True(t, blocked, "over budget with the counter present")

	rdb.AddHook(missingTTLHook{})

	blocked, ttl := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	assert.False(t, blocked, "a vanished counter has no budget left to enforce")
	assert.Equal(t, time.Duration(0), ttl)
}

// zeroTTLHook lets the real TTL run and then reports 0 for it.
//
// Redis rounds TTL to the NEAREST second — `(pttl + 500) / 1000` — so 0 is a
// value it genuinely returns, for any key with under 500 ms left. Producing it
// by setting a short expiry and racing the clock is what the first version of
// this test did, and it sat exactly on the rounding boundary: locally a few ms
// had elapsed and it read 0, in CI it read 1s and the test failed. Overriding
// the value reproduces the same server response with no timing at all.
type zeroTTLHook struct{}

func (zeroTTLHook) DialHook(next redis.DialHook) redis.DialHook { return next }

func (zeroTTLHook) ProcessHook(next redis.ProcessHook) redis.ProcessHook {
	return func(ctx context.Context, cmd redis.Cmder) error {
		if err := next(ctx, cmd); err != nil {
			return err
		}
		if cmd.Name() == "ttl" {
			if d, ok := cmd.(*redis.DurationCmd); ok {
				d.SetVal(0)
			}
		}
		return nil
	}
}

func (zeroTTLHook) ProcessPipelineHook(next redis.ProcessPipelineHook) redis.ProcessPipelineHook {
	return next
}

// A window with under half a second left reports TTL 0, which is a real value
// meaning "rolls over immediately" — not a missing one. Treating it as missing
// left remaining at the full window, so the responder emitted Retry-After for a
// budget that had already reset.
func TestIsRateLimitedPreservesZeroTTL(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:zerottl"
	rdb.Del(ctx, key)

	for i := 0; i < 3; i++ {
		blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
		assert.False(t, blocked)
	}

	rdb.AddHook(zeroTTLHook{})

	blocked, ttl := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	assert.True(t, blocked, "still over budget")
	assert.Equal(t, time.Duration(0), ttl,
		"a genuine zero must survive; the full window here is a Retry-After the caller does not owe")
}

// IsRateLimited documents itself as failing open on Redis errors, and the INCR
// path does. The TTL path did not: a failed TTL read fell through to the
// count comparison and still reported blocked. For an availability-sensitive
// caller — DM key distribution (#1218) — that wedges key delivery on a
// bookkeeping fault that says nothing about the budget.
func TestIsRateLimitedFailsOpenWhenTTLReadFails(t *testing.T) {
	rdb, cleanup := testhelpers.SetupTestRedis(t)
	defer cleanup()

	ctx := context.Background()
	key := "test:ratelimit:ttlfail"
	rdb.Del(ctx, key)

	// Spend the budget while TTL still works, so the counter is genuinely over
	// the limit. Without this the assertion below would pass on an under-budget
	// request and prove nothing.
	for i := 0; i < 3; i++ {
		blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
		assert.False(t, blocked, "request %d should be within budget", i+1)
	}
	blocked, _ := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	assert.True(t, blocked, "the 4th request must be blocked while TTL reads succeed")

	rdb.AddHook(failTTLHook{})

	blocked, ttl := middleware.IsRateLimited(ctx, rdb, key, 3, time.Minute)
	assert.False(t, blocked, "a failed TTL read must fail open, matching the INCR path and this function's contract")
	assert.Equal(t, time.Duration(0), ttl)
}
