package feedback

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupFeedbackTestRedis returns a Redis client isolated from dev data by
// default, skipping the test if Redis is unreachable. Deliberately does NOT
// use internal/testhelpers — that package imports internal/api, which imports
// this (feedback) package via feedback_wiring.go, so importing it from a
// package-internal test would create an import cycle. The client/cleanup shape
// mirrors testhelpers.SetupTestRedis.
//
// NOTE: tests using this share the FIXED globalFeedbackKey on one Redis DB.
// The key is deleted at setup AND cleanup so SERIAL runs stay isolated, but for
// that reason these tests MUST NOT call t.Parallel().
func setupFeedbackTestRedis(t *testing.T) *redis.Client {
	t.Helper()

	redisURL := os.Getenv("REDIS_URL")
	useDefaultDB := redisURL == ""
	if useDefaultDB {
		redisURL = "redis://:concord_dev_redis@localhost:6379" //nolint:gosec // dev-only default, matches docker-compose
	}
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		t.Fatalf("feedback test: failed to parse redis URL: %v", err)
	}
	if useDefaultDB {
		opts.DB = 1 // isolate from dev data on DB 0
	}

	client := redis.NewClient(opts)
	ctx := context.Background()
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		t.Skipf("Redis unavailable (%v) — skipping global-cap Redis-dependent test", err)
	}
	t.Cleanup(func() {
		_ = client.Del(context.Background(), globalFeedbackKey).Err()
		_ = client.Close()
	})
	// Start clean for the global counter key.
	client.Del(ctx, globalFeedbackKey)
	return client
}

// newGlobalLimitEngine wires GlobalRateLimit in front of a 200-OK terminal
// handler so tests can observe pass / 429 / 503 outcomes.
func newGlobalLimitEngine(t *testing.T, rdb *redis.Client) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/v1/feedback", GlobalRateLimit(rdb), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	})
	return r
}

func doGlobalPost(r *gin.Engine) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/feedback", nil)
	r.ServeHTTP(w, req)
	return w
}

// Under the global cap, requests pass through to the handler.
func TestGlobalRateLimit_AllowsUnderCap(t *testing.T) {
	rdb := setupFeedbackTestRedis(t)

	r := newGlobalLimitEngine(t, rdb)

	// A handful of requests well under globalFeedbackCap all succeed.
	for i := 0; i < 5; i++ {
		w := doGlobalPost(r)
		assert.Equal(t, http.StatusOK, w.Code, "request %d should pass under cap", i+1)
	}
}

// Over the global cap, requests are rejected with 429. Verified by pre-seeding
// the counter to the cap so the next request is the (cap+1)th.
func TestGlobalRateLimit_BlocksOverCap(t *testing.T) {
	rdb := setupFeedbackTestRedis(t)
	ctx := context.Background()

	// Seed the counter AT the cap. The next INCR makes it cap+1 → 429.
	require.NoError(t, rdb.Set(ctx, globalFeedbackKey, globalFeedbackCap, globalFeedbackWindow).Err())

	r := newGlobalLimitEngine(t, rdb)
	w := doGlobalPost(r)
	assert.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Contains(t, w.Body.String(), "Rate limit exceeded")
	assert.NotEmpty(t, w.Header().Get("Retry-After"))
}

// On a Redis error the middleware fails CLOSED with 503 — never allowing the
// request through, consistent with the privileged public-repo write posture.
func TestGlobalRateLimit_FailsClosedOnRedisError(t *testing.T) {
	rdb := setupFeedbackTestRedis(t)

	// Close the client to force every command to error.
	_ = rdb.Close()

	r := newGlobalLimitEngine(t, rdb)
	w := doGlobalPost(r)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "Redis error must fail CLOSED (503), not open")
	assert.Contains(t, w.Body.String(), "temporarily unavailable")
}

// A nil Redis client (feature not wired) also fails CLOSED — the global cap is
// a security guardrail and must not silently no-op. No Redis needed.
func TestGlobalRateLimit_FailsClosedOnNilClient(t *testing.T) {
	r := newGlobalLimitEngine(t, nil)
	w := doGlobalPost(r)
	assert.Equal(t, http.StatusServiceUnavailable, w.Code, "nil Redis must fail CLOSED (503)")
}

// The window TTL is armed on the first increment so the counter resets.
func TestGlobalRateLimit_ArmsWindowTTL(t *testing.T) {
	rdb := setupFeedbackTestRedis(t)
	ctx := context.Background()

	r := newGlobalLimitEngine(t, rdb)
	w := doGlobalPost(r)
	require.Equal(t, http.StatusOK, w.Code)

	ttl, err := rdb.TTL(ctx, globalFeedbackKey).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, time.Duration(0), "window TTL must be set after first increment")
	assert.LessOrEqual(t, ttl, globalFeedbackWindow)
}
