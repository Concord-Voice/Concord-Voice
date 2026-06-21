package feedback

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const (
	// globalFeedbackCap is the GLOBAL aggregate cap on feedback submissions
	// per globalFeedbackWindow, across ALL users. It sits in front of the
	// per-user 10/hour limit (router.go) to bound mass abuse: the per-user cap
	// alone is multiplied by N Sybil accounts, so N accounts could flood the
	// PUBLIC feedback tracker with N*10 issues/hour. This aggregate guard caps
	// that at globalFeedbackCap regardless of how many accounts participate.
	//
	// Value rationale: at 10 submissions/user/hour, 500/hour tolerates ~50
	// maximally-active distinct users (or hundreds of normal users filing 1–2
	// reports each) within the window without ever tripping in honest
	// multi-user operation — comfortably above any realistic Beta/GA feedback
	// volume for a small-team launch — while bounding a coordinated Sybil
	// flood to 500 public-repo issues/hour instead of an unbounded N*10. If
	// legitimate volume ever approaches this ceiling it is itself a signal
	// worth alerting on, not silently absorbing.
	globalFeedbackCap    = 500
	globalFeedbackWindow = 1 * time.Hour

	// globalFeedbackKey is the fixed Redis key for the global token bucket.
	// Not keyed by user / IP / path — it is the single aggregate counter.
	globalFeedbackKey = "ratelimit:global:feedback"
)

// GlobalRateLimit returns a gin middleware enforcing the GLOBAL aggregate cap
// (globalFeedbackCap per globalFeedbackWindow) on the feedback route via a
// Redis INCR+EXPIRE token bucket.
//
// Failure posture is FAIL CLOSED — consistent with the fail-closed per-user
// limiter wired on this route: the feedback endpoint is a privileged-PAT
// public-repo write where a rate limiter is the SOLE velocity control, so a
// Redis blip must NOT remove the flood cap. A nil client (feature not wired)
// or a Redis error responds 503 rather than allowing the request through.
//
//   - over cap            → 429 Too Many Requests
//   - Redis error / nil   → 503 Service Unavailable (fail closed)
//   - under cap           → c.Next()
func GlobalRateLimit(rdb *redis.Client) gin.HandlerFunc {
	return func(c *gin.Context) {
		if rdb == nil {
			// No Redis configured — fail closed. The global cap is a
			// security guardrail on a public-repo write; we do not silently
			// drop it.
			abortGlobalUnavailable(c)
			return
		}

		ctx := context.Background()
		count, err := rdb.Incr(ctx, globalFeedbackKey).Result()
		if err != nil {
			abortGlobalUnavailable(c)
			return
		}

		// Set / re-arm the window TTL on the first increment or if the key
		// somehow lost its expiry (mirrors rateLimit() in the middleware
		// package). Best-effort: a failed Expire is repaired on a later hit.
		ttl, ttlErr := rdb.TTL(ctx, globalFeedbackKey).Result()
		if ttlErr != nil || ttl == -1 || count == 1 {
			_ = rdb.Expire(ctx, globalFeedbackKey, globalFeedbackWindow).Err()
			ttl = globalFeedbackWindow
		}

		if count > int64(globalFeedbackCap) {
			retryAfter := int(ttl.Seconds())
			c.Header("Retry-After", fmt.Sprintf("%d", retryAfter))
			c.JSON(http.StatusTooManyRequests, gin.H{
				"error":   "Rate limit exceeded",
				"message": "The feedback service is temporarily at capacity. Please try again later.",
			})
			c.Abort()
			return
		}

		c.Next()
	}
}

// abortGlobalUnavailable writes the fail-closed 503 used when the global cap
// cannot be evaluated (nil client or Redis error).
func abortGlobalUnavailable(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{
		"error": "feedback service temporarily unavailable; please try again later",
	})
}
