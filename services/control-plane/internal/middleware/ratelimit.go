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
	Requests            int                       // Number of requests allowed
	Window              time.Duration             // Time window for the limit
	KeyFunc             func(*gin.Context) string // Function to generate rate limit key
	ExceededHandler     gin.HandlerFunc           // Optional fixed response for exceeded requests
	BackendErrorHandler gin.HandlerFunc           // Optional fixed response for fail-closed backend errors
	// FailClosed, when true, rejects the request (503) if the Redis backend
	// errors, instead of failing open. Use for privileged routes where the
	// limiter is a required security or privacy control. Default false preserves
	// the fail-open posture for availability-first routes.
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

// RateLimitByIPWithExceededHandler preserves the standard IP key and headers
// while delegating only the exceeded response. The supplied handler must emit a
// fixed response and must not include request-derived data.
func RateLimitByIPWithExceededHandler(
	redis *redis.Client,
	requests int,
	window time.Duration,
	exceededHandler gin.HandlerFunc,
) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests:        requests,
		Window:          window,
		KeyFunc:         rateLimitIPKey,
		ExceededHandler: exceededHandler,
	}
	return rateLimit(redis, config)
}

// RateLimitByIPFailClosedWithHandlers preserves the standard IP key and
// headers while rejecting Redis backend failures with a caller-owned fixed
// response. Use for privileged routes whose limiter must not fail open.
func RateLimitByIPFailClosedWithHandlers(
	redis *redis.Client,
	requests int,
	window time.Duration,
	exceededHandler gin.HandlerFunc,
	backendErrorHandler gin.HandlerFunc,
) gin.HandlerFunc {
	config := RateLimitConfig{
		Requests:            requests,
		Window:              window,
		KeyFunc:             rateLimitIPKey,
		ExceededHandler:     exceededHandler,
		BackendErrorHandler: backendErrorHandler,
		FailClosed:          true,
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

// AllowUserAction performs a fail-CLOSED per-key rate-limit check OUTSIDE the gin
// middleware chain, for a privileged sub-action invoked programmatically rather than as a
// route (e.g. the purge-on-ban/kick sub-action, #1353). It mirrors rateLimit's INCR +
// first-hit Expire but returns (allowed, err) instead of touching gin: a Redis backend
// error returns (false, err) so the caller treats an outage as DENY — the same fail-closed
// posture as RateLimitByUserFailClosed. The caller owns key construction and the deny action.
func AllowUserAction(ctx context.Context, rdb *redis.Client, key string, limit int, window time.Duration) (bool, error) {
	count, err := rdb.Incr(ctx, key).Result()
	if err != nil {
		return false, err // fail-closed: a backend error denies
	}
	// Ensure the key carries a TTL. A fresh INCR creates the key with NO expiry, and if an
	// earlier Expire failed after the INCR persisted, the key survives with no TTL (-1) — its
	// counter would then never reset and, once over budget, lock the actor out forever. Re-apply
	// the window whenever the key lacks an expiry (both the first hit and the repair case),
	// mirroring resolveRateLimitTTL. Post-INCR the key exists, so TTL is -1 (no expiry) or >=0.
	ttl, ttlErr := rdb.TTL(ctx, key).Result()
	if ttlErr != nil {
		return false, ttlErr // fail-closed: a TTL read failure denies
	}
	if ttl == -1 {
		if err := rdb.Expire(ctx, key, window).Err(); err != nil {
			return false, err // fail-closed: a TTL-set failure denies
		}
	}
	return count <= int64(limit), nil
}

// rateLimit is the core rate limiting middleware
func rateLimit(redis *redis.Client, config RateLimitConfig) gin.HandlerFunc {
	return func(c *gin.Context) {
		key := config.KeyFunc(c)
		count, ok := incrementRateLimitCounter(c, redis, config, key)
		if !ok {
			return
		}
		ttl, ok := resolveRateLimitTTL(c, redis, config, key, count)
		if !ok {
			return
		}

		// Set rate limit headers
		c.Header("X-RateLimit-Limit", fmt.Sprintf("%d", config.Requests))
		c.Header("X-RateLimit-Remaining", fmt.Sprintf("%d", maxInt(0, config.Requests-int(count))))
		c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(ttl).Unix()))

		// Check if rate limit exceeded
		if count > int64(config.Requests) {
			abortRateLimitExceeded(c, config, ttl)
			return
		}

		c.Next()
	}
}

func incrementRateLimitCounter(c *gin.Context, redis *redis.Client, config RateLimitConfig, key string) (int64, bool) {
	count, err := redis.Incr(context.Background(), key).Result()
	if err == nil {
		return count, true
	}
	if abortOnRateLimitBackendError(c, config) {
		return 0, false
	}
	// Default: fail open (allow request) to prevent total outage.
	c.Next()
	return 0, false
}

func resolveRateLimitTTL(
	c *gin.Context,
	redis *redis.Client,
	config RateLimitConfig,
	key string,
	count int64,
) (time.Duration, bool) {
	ctx := context.Background()
	ttl, ttlErr := redis.TTL(ctx, key).Result()
	if ttlErr != nil && abortOnRateLimitBackendError(c, config) {
		return 0, false
	}

	// Set expiry on the first request or if the key lost its TTL. A fail-open
	// route still attempts repair after a TTL read error.
	if ttlErr == nil && ttl != -1 && count != 1 {
		return ttl, true
	}
	if expireErr := redis.Expire(ctx, key, config.Window).Err(); expireErr != nil && abortOnRateLimitBackendError(c, config) {
		return 0, false
	}
	return config.Window, true
}

func abortRateLimitExceeded(c *gin.Context, config RateLimitConfig, ttl time.Duration) {
	c.Header("Retry-After", fmt.Sprintf("%d", int(ttl.Seconds())))
	if config.ExceededHandler != nil {
		config.ExceededHandler(c)
	} else {
		c.JSON(http.StatusTooManyRequests, gin.H{
			"error":   "Rate limit exceeded",
			"message": fmt.Sprintf("Too many requests. Please try again in %d seconds.", int(ttl.Seconds())),
		})
	}
	c.Abort()
}

func abortOnRateLimitBackendError(c *gin.Context, config RateLimitConfig) bool {
	if !config.FailClosed {
		return false
	}
	if config.BackendErrorHandler != nil {
		config.BackendErrorHandler(c)
		c.Abort()
		return true
	}
	// Privileged route: the limiter is a required control, so a Redis outage
	// must not open the floodgate. Reject with the shared fixed response.
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error":   "service unavailable",
		"message": "rate-limit backend unavailable; please retry shortly",
	})
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
