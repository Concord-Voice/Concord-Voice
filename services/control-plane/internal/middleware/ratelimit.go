package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// RateLimitConfig defines rate limiting parameters
type RateLimitConfig struct {
	Requests int                       // Number of requests allowed
	Window   time.Duration             // Time window for the limit
	KeyFunc  func(*gin.Context) string // Function to generate rate limit key
	// FailClosed, when true, rejects the request (503) if the Redis backend
	// errors, instead of failing open. Use for privileged-write routes where
	// the limiter is the SOLE velocity control (e.g. feedback issue creation
	// against a PUBLIC repo via a privileged PAT — #158). Default false
	// preserves the fail-open posture for availability-first routes.
	FailClosed bool
}

// rateLimitIPKey builds the per-IP rate-limit Redis key.
func rateLimitIPKey(c *gin.Context) string {
	return fmt.Sprintf("ratelimit:ip:%s:%s:%s", c.ClientIP(), c.Request.Method, c.FullPath())
}

// rateLimitUserKey builds the per-user rate-limit Redis key, falling back to
// the per-IP key when the request is unauthenticated.
func rateLimitUserKey(c *gin.Context) string {
	userID, exists := c.Get("user_id")
	if !exists {
		return rateLimitIPKey(c)
	}
	return fmt.Sprintf("ratelimit:user:%v:%s:%s", userID, c.Request.Method, c.FullPath())
}

// RateLimitByIP creates a rate limiter based on IP address
func RateLimitByIP(redis *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests: requests,
		Window:   window,
		KeyFunc:  rateLimitIPKey,
	}
	return rateLimit(redis, config)
}

// RateLimitByUser creates a rate limiter based on user ID (for authenticated routes)
func RateLimitByUser(redis *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests: requests,
		Window:   window,
		KeyFunc:  rateLimitUserKey,
	}
	return rateLimit(redis, config)
}

// RateLimitGlobal creates a fail-open aggregate limiter shared by all callers.
func RateLimitGlobal(redis *redis.Client, key string, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests: requests,
		Window:   window,
		KeyFunc: func(*gin.Context) string {
			return key
		},
	}
	return rateLimit(redis, config)
}

// RateLimitByUserFailClosed is RateLimitByUser with fail-CLOSED semantics: a
// Redis backend error rejects the request (503) rather than allowing it. Use
// ONLY for privileged-write routes where the limiter is the single velocity
// control and an open floodgate is worse than a brief outage — e.g.
// POST /api/v1/feedback, which creates issues in a PUBLIC repo via a privileged
// PAT (#158). All other routes keep the fail-open RateLimitByUser to favor
// availability.
func RateLimitByUserFailClosed(redis *redis.Client, requests int, window time.Duration) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests:   requests,
		Window:     window,
		FailClosed: true,
		KeyFunc:    rateLimitUserKey,
	}
	return rateLimit(redis, config)
}

// rateLimit is the core rate limiting middleware
func rateLimit(redis *redis.Client, config RateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx := context.Background()
		key := config.KeyFunc(c)

		// Increment counter
		count, err := redis.Incr(ctx, key).Result()
		if err != nil {
			if config.FailClosed {
				// Privileged-write route: the limiter is the only velocity cap,
				// so a Redis outage must NOT open the floodgate (#158). Reject.
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
					"error":   "service unavailable",
					"message": "rate-limit backend unavailable; please retry shortly",
				})
				return
			}
			// Default: fail open (allow request) to prevent total outage
			c.Next()
			return
		}

		// Set expiry on first request OR if key somehow lost its TTL
		// This prevents keys from persisting indefinitely if Expire fails
		ttl, err := redis.TTL(ctx, key).Result()
		if err != nil || ttl == -1 || count == 1 {
			// TTL is -1 when key exists but has no expiry
			// Always set expiry to ensure window resets properly
			redis.Expire(ctx, key, config.Window)
			ttl = config.Window
		}

		// Set rate limit headers
		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", config.Requests))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", maxInt(0, config.Requests-int(count))))
		c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(ttl).Unix()))

		// Check if rate limit exceeded
		if count > int64(config.Requests) {
			c.Header("Retry-After", fmt.Sprintf("%d", int(ttl.Seconds())))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "Rate limit exceeded",
				"message": fmt.Sprintf("Too many requests. Please try again in %d seconds.", int(ttl.Seconds())),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
