package middleware

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

// AuthBanConfig defines the IP-based cumulative auth failure ban parameters.
type AuthBanConfig struct {
	Threshold int           // Number of failures before ban
	Window    time.Duration // Rolling window for failure counting
	Duration  time.Duration // Ban length once triggered
}

// DefaultAuthBanConfig returns sensible defaults: 10 failures in 15 minutes triggers a 15-minute ban.
func DefaultAuthBanConfig() AuthBanConfig {
	return AuthBanConfig{
		Threshold: 10,
		Window:    15 * time.Minute,
		Duration:  15 * time.Minute,
	}
}

const (
	authBanKeyPrefix     = "auth_ban:ip:"
	authFailureKeyPrefix = "auth_failures:ip:"
)

// AuthBanCheck returns middleware that blocks requests from IPs temporarily
// banned due to cumulative auth failures across all auth endpoints.
// Returns 429 with Retry-After header when banned. Fails open if Redis is
// unavailable or not configured (nil client).
func AuthBanCheck(redisClient *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		if redisClient == nil {
			c.Next()
			return
		}

		ip := c.ClientIP()
		key := authBanKeyPrefix + ip

		ctx, cancel := context.WithTimeout(c.Request.Context(), 2*time.Second)
		defer cancel()

		ttl, err := redisClient.TTL(ctx, key).Result()
		if err != nil {
			// Redis down — fail open to prevent total outage
			c.Next()
			return
		}

		// TTL > 0: key exists with expiry (IP is banned)
		// TTL == -1: key exists without expiry (shouldn't happen, treat as banned)
		// TTL == -2: key does not exist (not banned)
		if ttl > 0 || ttl == -1 {
			retryAfter := int(ttl.Seconds())
			if retryAfter <= 0 {
				retryAfter = 60 // fallback for TTL == -1
			}
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "Too many authentication failures",
				"message": fmt.Sprintf("Your IP has been temporarily blocked. Try again in %d seconds.", retryAfter),
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// RecordAuthFailure increments the cumulative failure counter for an IP.
// When the counter reaches the configured threshold, the IP is banned for
// the configured duration and the counter is reset atomically via a pipeline.
func RecordAuthFailure(ctx context.Context, redisClient *redis.Client, ip string, cfg AuthBanConfig) {
	if redisClient == nil {
		return
	}

	failureKey := authFailureKeyPrefix + ip
	banKey := authBanKeyPrefix + ip

	count, err := redisClient.Incr(ctx, failureKey).Result()
	if err != nil {
		return // Redis down — silently skip
	}

	// Set TTL on first failure; bail if Expire fails to prevent permanent counters
	if count == 1 {
		if err := redisClient.Expire(ctx, failureKey, cfg.Window).Err(); err != nil {
			return
		}
	}

	if count >= int64(cfg.Threshold) {
		pipe := redisClient.TxPipeline()
		pipe.Set(ctx, banKey, "1", cfg.Duration)
		pipe.Del(ctx, failureKey) // Reset counter after ban
		if _, err := pipe.Exec(ctx); err != nil {
			return
		}
	}
}

// ClearAuthFailures removes the failure counter for an IP on successful authentication.
func ClearAuthFailures(ctx context.Context, redisClient *redis.Client, ip string) {
	if redisClient == nil {
		return
	}
	redisClient.Del(ctx, authFailureKeyPrefix+ip)
}
