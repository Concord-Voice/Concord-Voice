package members

import (
	"context"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/messages"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/middleware"
)

const (
	// purgeModerationRateLimit / Window bound the purge-on-ban/kick sub-action per actor,
	// fail-CLOSED (mirroring the standalone purge endpoint's RateLimitByUserFailClosed,
	// 5/hour). The moderation purge gets its OWN per-actor budget (separate Redis key), so
	// it is bounded AND fails closed on a Redis outage — closing the fail-open asymmetry the
	// #1353 review surfaced (Gitar + security-reviewer): the ban/kick route limiter is
	// 5/min and fail-OPEN, so without this a Redis outage left the destructive purge
	// unthrottled while the equivalent direct purge stayed blocked.
	purgeModerationRateLimit  = 5
	purgeModerationRateWindow = time.Hour
	// purgeOnModerationTimeout bounds the cancellation-detached best-effort purge so a
	// pathological hang cannot outlive the control plane's 15s HTTP write deadline;
	// a genuinely large purge fails cleanly instead of completing without a response.
	purgeOnModerationTimeout = 10 * time.Second
)

// serverMessagePurger is the narrow capability the moderation handlers need from the
// messages handler: purge one user's server-wide messages. Satisfied by *messages.Handler.
type serverMessagePurger interface {
	PurgeUserServerMessages(ctx context.Context, serverID, actorID, target, reason string) (int, messages.PurgeStatus, error)
}

// SetServerMessagePurger wires the purge capability post-construction. The router builds the
// members handler before the messages handler, so this cannot be a NewHandler parameter
// (mirrors SetVoiceEnforcer). Nil is a valid state — the purge fails closed (skipped).
// rateLimit/rateWindow are the resolvePurgeRateLimit(cfg) values (shared with the standalone
// purge endpoint, #1353 review Codex P2); non-positive falls back to the package defaults.
func (h *Handler) SetServerMessagePurger(p serverMessagePurger, rateLimit int, rateWindow time.Duration) {
	h.purger = p
	h.purgeRateLimit = rateLimit
	h.purgeRateWindow = rateWindow
}

// moderationPurgeRate returns the effective fail-closed budget, honoring the operator-configured
// values wired via SetServerMessagePurger and falling back to the package defaults.
func (h *Handler) moderationPurgeRate() (int, time.Duration) {
	limit, window := h.purgeRateLimit, h.purgeRateWindow
	if limit <= 0 {
		limit = purgeModerationRateLimit
	}
	if window <= 0 {
		window = purgeModerationRateWindow
	}
	return limit, window
}

// purgeOutcome is the response fragment describing the additive purge sub-action. It appears
// in the ban/kick response ONLY when purge was requested.
type purgeOutcome struct {
	Requested   bool                 `json:"requested"`
	Status      messages.PurgeStatus `json:"status"`
	PurgedCount int                  `json:"purged_count"`
}

// applyPurgeOnModeration runs the best-effort purge of target's server messages and returns the
// response fragment. It NEVER returns an error and never affects the caller — the ban/kick has
// already committed (the additive contract). A denied purge -> skipped_unauthorized; a rate-limit
// deny / Redis outage -> skipped_rate_limited; any failure (including a nil/unwired purger) ->
// failed. All non-completed paths are logged PII-safe (never message content or usernames,
// per observability.md #1/#2).
func (h *Handler) applyPurgeOnModeration(ctx context.Context, serverID, actorID, targetUserID, reason string) purgeOutcome {
	if h.purger == nil {
		h.log.Error("purge-on-moderation: no purger wired", "server_id", serverID, "reason", reason)
		return purgeOutcome{Requested: true, Status: messages.PurgeFailed}
	}

	// Detach from request cancellation FIRST so BOTH the fail-closed rate-limit check and the
	// purge complete server-side even if the client disconnects after the ban/kick committed
	// (the purge engine's own audit-count salvage uses the same WithoutCancel pattern). Running
	// the rate check on the raw request ctx would let a post-commit disconnect fail the INCR,
	// trip the fail-closed branch, and skip the purge as skipped_rate_limited — the exact
	// mid-flight abort this detachment exists to prevent (#1353 review, Gitar).
	// Preserve any caller deadline, which begins before the ban/kick's synchronous
	// prework, so the detached cleanup cannot overrun the HTTP write deadline.
	pctx := context.WithoutCancel(ctx)
	if deadline, ok := ctx.Deadline(); ok {
		var deadlineCancel context.CancelFunc
		pctx, deadlineCancel = context.WithDeadline(pctx, deadline)
		defer deadlineCancel()
	}
	pctx, cancel := context.WithTimeout(pctx, purgeOnModerationTimeout)
	defer cancel()
	if pctx.Err() != nil {
		return purgeOutcome{Requested: true, Status: messages.PurgeFailed}
	}

	// Fail-CLOSED per-actor rate limit: the moderation purge is destructive and must not
	// escape the velocity control the standalone purge endpoint enforces. An exhausted budget
	// OR a Redis backend error SKIPS the purge (the ban/kick already committed — additive),
	// never blocks it. A nil redis (unit tests / unwired) skips the check.
	if h.redis != nil {
		limit, window := h.moderationPurgeRate()
		key := fmt.Sprintf("ratelimit:user:%s:purge-on-moderation", actorID)
		allowed, rlErr := middleware.AllowUserAction(pctx, h.redis, key, limit, window)
		if rlErr != nil || !allowed {
			if pctx.Err() != nil {
				return purgeOutcome{Requested: true, Status: messages.PurgeFailed}
			}
			h.log.Warn("purge-on-moderation rate-limited",
				"server_id", serverID, "reason", reason, "exhausted", !allowed, "backend_error", rlErr != nil)
			return purgeOutcome{Requested: true, Status: messages.PurgeSkippedRateLimited}
		}
	}

	count, status, err := h.purger.PurgeUserServerMessages(pctx, serverID, actorID, targetUserID, reason)
	// Treat the returned status as authoritative alongside err (#1353 review, CodeRabbit):
	// a PurgeFailed status must resolve to failed even on a nil error, never logged as completed.
	if err != nil || status == messages.PurgeFailed {
		h.log.Error("purge-on-moderation failed", "server_id", serverID, "reason", reason, "error", err)
		return purgeOutcome{Requested: true, Status: messages.PurgeFailed, PurgedCount: count}
	}
	if status == messages.PurgeSkippedUnauthorized {
		h.log.Info("purge-on-moderation skipped (unauthorized)", "server_id", serverID, "reason", reason)
	} else {
		h.log.Info("purge-on-moderation completed", "server_id", serverID, "reason", reason, "deleted", count)
	}
	return purgeOutcome{Requested: true, Status: status, PurgedCount: count}
}
