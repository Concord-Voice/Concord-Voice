package stepup

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

// Budget bounds are const, never configuration: a configurable security bound
// is one more place it can be wrong ([internal]rules/backend.md).
const (
	BudgetLimit  = 5
	BudgetWindow = 15 * time.Minute
)

// MFASettingsBudgetPrefix keys the one budget shared by every MFA-settings
// route and the MFA-enforcement toggle's OFF confirmation. It is one budget
// because each of those surfaces verifies the same factor: a budget per route
// hands a stolen session five more guesses for every route added (PR #3464
// review finding 8, CWE-307). The spelling predates the toggle and is kept, so
// a deploy does not reset counters in flight.
const MFASettingsBudgetPrefix = "stepup:mfa_settings:"

// ErrMsgTooManyAttempts is the 429 body when the budget is exhausted.
const ErrMsgTooManyAttempts = "Too many verification attempts"

// ErrMsgBudgetUnavailable is the 503 body when the budget cannot be
// evaluated. It is deliberately NOT the 429: telling a user "too many
// attempts" during a Redis outage misreports a server fault as their own
// doing and locks the form for a window that is not running.
const ErrMsgBudgetUnavailable = "Verification is temporarily unavailable. Try again in a few minutes."

var errBudgetUnwired = errors.New("step-up attempt budget has no Redis client")

// Budget is the fail-closed attempt budget every credential-bearing step-up
// charges before it opens a transaction. It is shared so the purge fence and
// the MFA-settings routes cannot drift apart again: they did once, and one of
// them answered a Redis outage with a 429.
//
// Charge it BEFORE BeginTx — a Redis round trip inside an open transaction
// pins a pooled connection for the length of a network call — and only when
// the request actually carries a credential: a credential-less request learns
// nothing beyond the fixed "step-up required" refusal every account gets, so
// charging it would let any bearer burn the owner's budget without guessing.
//
// Clear it only after a VERIFIED success has committed. AllowUserAction
// increments before the outcome is knowable and never decrements, so without
// the clear a legitimate user is eventually locked out by their own correct
// credentials; a verified credential whose transaction then rolled back
// changed nothing and still costs an attempt.
type Budget struct {
	rdb    *redis.Client
	prefix string
}

// NewBudget binds a budget to its Redis client and key prefix. A nil client
// is allowed and DENIES (503): a limiter that silently no-ops when unwired is
// the failure this type exists to prevent.
func NewBudget(rdb *redis.Client, prefix string) Budget {
	return Budget{rdb: rdb, prefix: prefix}
}

// Key is the one place a budget's Redis key is spelled, so the consume and
// the clear cannot disagree about it.
func (b Budget) Key(userID string) string { return b.prefix + userID }

// Consume charges one attempt. It returns nil when the attempt is admitted,
// a 429 when the budget is exhausted, and a 503 — with Cause set, for the
// caller to log — when the budget cannot be evaluated at all.
func (b Budget) Consume(ctx context.Context, userID string) *Error {
	if b.rdb == nil {
		return budgetUnavailable(errBudgetUnwired)
	}
	allowed, err := middleware.AllowUserAction(ctx, b.rdb, b.Key(userID), BudgetLimit, BudgetWindow)
	if err != nil {
		return budgetUnavailable(fmt.Errorf("consume step-up attempt: %w", err))
	}
	if !allowed {
		return &Error{Status: http.StatusTooManyRequests, Body: gin.H{"error": ErrMsgTooManyAttempts}}
	}
	return nil
}

// Clear resets the budget after a verified, committed success. It is
// best-effort by design: a failure leaves the counter high, which fails
// toward MORE limiting, never less. The error is returned for the caller to
// log; an unwired client has nothing to clear.
func (b Budget) Clear(ctx context.Context, userID string) error {
	if b.rdb == nil {
		return nil
	}
	if err := b.rdb.Del(ctx, b.Key(userID)).Err(); err != nil {
		return fmt.Errorf("clear step-up attempts: %w", err)
	}
	return nil
}

func budgetUnavailable(cause error) *Error {
	return &Error{
		Status: http.StatusServiceUnavailable,
		Body:   gin.H{"error": ErrMsgBudgetUnavailable},
		Cause:  cause,
	}
}
