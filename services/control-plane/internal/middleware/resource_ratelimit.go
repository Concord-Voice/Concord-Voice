package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// IsRateLimited enforces a per-resource rate limit using Redis INCR+EXPIRE.
// Returns (true, retryAfter) if blocked, (false, 0) if allowed.
// Fails open on nil Redis client or Redis errors.
func IsRateLimited(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, time.Duration) {
	if rdb == nil {
		return false, 0
	}
	count, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, 0
	}
	// Defensive TTL re-guard: set expiry on first request or if key lost its TTL.
	// Prevents permanent lockout if the initial Expire call fails.
	// Pattern matches rateLimit() in this package (lines 64-71).
	remaining := window
	redisTTL, ttlErr := rdb.TTL(ctx, key).Result()
	if ttlErr != nil || redisTTL == -1 || count == 1 {
		// TTL is -1 when key exists but has no expiry.
		// Always set expiry to ensure window resets properly.
		rdb.Expire(ctx, key, window) //nolint:errcheck // best-effort TTL repair
	} else if redisTTL > 0 {
		remaining = redisTTL
	}
	if count > int64(limit) {
		return true, remaining
	}
	return false, 0
}

// RespondRateLimited writes a 429 Too Many Requests response with standard headers
// and a human-readable retry message.
func RespondRateLimited(c *gin.Context, ttl time.Duration, limit int) {
	retryAfterSec := int(ttl.Seconds())
	c.Header("Retry-After", fmt.Sprintf("%d", retryAfterSec))
	c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", limit))
	c.Header("X-RateLimit-Remaining", "0")
	c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(ttl).Unix()))
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error":       "Rate limit exceeded",
		"message":     fmt.Sprintf("Key rotation limit reached. Try again in %s.", FormatRetryAfter(ttl)),
		"retry_after": retryAfterSec,
	})
}
