package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/middleware"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
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
