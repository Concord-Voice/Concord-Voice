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
	if ttlErr != nil {
		// Fail open, matching the INCR path above and this function's own
		// contract. A failed TTL read tells us nothing about the window, and
		// the branch below would otherwise report `blocked` on a bookkeeping
		// error — for an availability-sensitive caller like DM key
		// distribution that wedges key delivery on a fault that says nothing
		// about the budget. Repair is still attempted so the counter does not
		// keep accumulating without an expiry.
		rdb.Expire(ctx, key, window) //nolint:errcheck // best-effort TTL repair
		return false, 0
	}
	if redisTTL == -2 {
		// The key vanished between the INCR and the TTL read — the two are
		// separate round trips, so a counter in its final moment can expire in
		// the gap. INCR then reported a count from a key that no longer exists.
		//
		// An earlier revision of this function excluded -2 and called leaving
		// `remaining` at the full window "conservative". It is not: there is no
		// counter left to enforce, so the budget has already reset, and
		// blocking here issues a full-window 429 against a caller who is
		// entitled to proceed immediately. Allow it.
		return false, 0
	}
	if redisTTL == -1 || count == 1 {
		// TTL is -1 when key exists but has no expiry.
		// Always set expiry to ensure window resets properly.
		//
		// The repair error is NOT discarded, and the reason is that this is the
		// one failure that does not heal itself. A counter that never gets an
		// expiry keeps climbing, every later call re-enters this branch on the
		// same -1, and once it passes the limit the key blocks that resource
		// permanently — there is no window left to roll over. Fail open instead:
		// an unbounded counter is not evidence the caller is over budget.
		if expErr := rdb.Expire(ctx, key, window).Err(); expErr != nil {
			return false, 0
		}
	} else if redisTTL >= 0 {
		// >= 0, not > 0. Redis reports the remaining TTL in whole seconds, so a
		// window with under a second left reports exactly 0 — a real value
		// meaning "this rolls over immediately", not a missing one. Treating it
		// as missing left `remaining` at the full window, so the responder
		// emitted Retry-After: 60 for a budget about to reset and the client
		// dutifully suppressed for a minute it did not owe. (-2 is handled
		// above; it cannot reach here.)
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
	c.Header(RateLimitLimitHeader, fmt.Sprintf("%d", limit))
	c.Header("X-RateLimit-Remaining", "0")
	c.Header("X-RateLimit-Reset", fmt.Sprintf("%d", time.Now().Add(ttl).Unix()))
	c.JSON(http.StatusTooManyRequests, gin.H{
		"error": "Rate limit exceeded",
		// Action-agnostic: this responder serves DM key rotation, channel key
		// rotation, and DM key distribution (#1218). Naming one caller's
		// action mislabels the others. The rotate UX is unaffected —
		// useRotateKey renders from retry_after and never reads this field.
		"message":     fmt.Sprintf("Rate limit reached. Try again in %s.", FormatRetryAfter(ttl)),
		"retry_after": retryAfterSec,
	})
}
