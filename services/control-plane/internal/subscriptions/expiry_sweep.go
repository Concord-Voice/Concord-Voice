package subscriptions

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
)

// DefaultExpirySweepInterval is the cadence for the subscription-expiry sweep.
// It MUST stay ≤ the entitlement cache TTL (entitlements.cacheTTL, 5 min) so the
// server-side tier and the client converge within one cache window of a grant
// lapsing. The sweep query is cheap (one bounded scan of a small table), so a
// short interval costs little; the real convergence lever is the OnTierChange
// disconnect it fires, which forces an immediate re-auth to the free claim.
const DefaultExpirySweepInterval = 5 * time.Minute

// TierChanger fires the entitlements convergence point (cache invalidate +
// entitlements_changed push + downgrade disconnect) for one user. Production
// binds it to entitlements.OnTierChange over the shared Cache + WS notifier (see
// tierChangeAdapter); tests inject a recorder, so the sweep's select/flip logic
// is exercised without a live Redis (the OnTierChange path has its own suite).
type TierChanger interface {
	OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error
}

// tierChangeAdapter binds TierChanger to entitlements.OnTierChange over the
// shared Cache + WS notifier — the production implementation.
type tierChangeAdapter struct {
	cache    *entitlements.Cache
	notifier entitlements.SessionNotifier
}

func (a tierChangeAdapter) OnTierChange(ctx context.Context, userID uuid.UUID, oldTier, newTier string) error {
	return entitlements.OnTierChange(ctx, a.cache, a.notifier, userID, oldTier, newTier)
}

// ExpirySweeper flips lapsed subscriptions to a terminal 'expired' status and
// fires the tier-change convergence point so a downgrade actually reaches the
// client. Without it, passive time-based expiry (the only real downgrade path
// until Stripe #1306) never invalidates the cache or disconnects the user, so a
// client keeps premium UI affordances for the rest of an uninterrupted session
// while the server rejects them (#2158).
//
// Multi-replica note (accepted, tracked in #2171): the tier-change notification
// reaches the client through entitlements.OnTierChange's hub calls, which are
// REPLICA-LOCAL — a pre-existing property of the shared convergence point (the
// redemption engine reaches clients the identical way). This is correct for the
// single-instance control-plane Beta ships; SERVER-side enforcement is
// cross-replica-correct regardless (cache.Invalidate unlinks the shared Redis
// key, so any replica re-resolves within the cache TTL). Only the EAGER client
// disconnect/push is replica-local, and only once the control-plane runs
// multi-replica (an HA milestone, #1504). Cross-replica NATS fan-out at the
// OnTierChange layer is tracked in #2171; do NOT work around it here.
type ExpirySweeper struct {
	db      *sql.DB
	log     *logger.Logger
	changer TierChanger
	// selfHosted mirrors entitlements.Cache's deployment-mode seam. On a
	// self-hosted instance every user resolves to the maximal tier, so the
	// post-flip tier MUST short-circuit to premium exactly as Cache.GetTier does;
	// a DB resolve would read TierFree and fire a bogus premium->free downgrade
	// (forced disconnect) for a user who is still premium (#2158 self-hosted).
	selfHosted bool
}

// NewExpirySweeper binds the sweeper to the shared entitlement Cache (for
// invalidation) and the WS SessionNotifier (for the push + downgrade kick),
// exactly the two args entitlements.OnTierChange needs. The Cache also carries
// the deployment mode, so the sweeper's post-flip tier resolution matches
// GetTier's self-hosted short-circuit.
func NewExpirySweeper(db *sql.DB, log *logger.Logger, cache *entitlements.Cache, notifier entitlements.SessionNotifier) *ExpirySweeper {
	s := NewExpirySweeperWith(db, log, tierChangeAdapter{cache: cache, notifier: notifier})
	s.selfHosted = cache.IsSelfHosted()
	return s
}

// NewExpirySweeperWith builds the sweeper against an injected TierChanger in SaaS
// mode. The dependency-injection seam used by tests (a recorder) and by any
// future caller that already holds a convergence-point adapter.
func NewExpirySweeperWith(db *sql.DB, log *logger.Logger, changer TierChanger) *ExpirySweeper {
	return &ExpirySweeper{db: db, log: log, changer: changer}
}

// expiredSubscription is one subscription row that has passed its period end but
// is still marked live.
type expiredSubscription struct {
	id      uuid.UUID
	userID  uuid.UUID
	oldTier string
}

// SweepExpired flips every past-period live subscription to
// 'expired' and fires OnTierChange(oldTier -> resolved) so the client is
// invalidated / disconnected. Returns the count actually expired. One bad row is
// logged and skipped so it cannot strand the rest of the sweep.
func (s *ExpirySweeper) SweepExpired(ctx context.Context) (int, error) {
	expired, err := s.selectExpiredSubscriptions(ctx)
	if err != nil {
		return 0, err
	}
	swept := 0
	for _, e := range expired {
		if err := s.expireOne(ctx, e); err != nil {
			s.log.Error("subscription expiry sweep: expire row",
				"error", err, "subscription_id", sanitizeID(e.id.String()), "user_id", sanitizeID(e.userID.String()))
			continue
		}
		swept++
	}
	return swept, nil
}

// selectExpiredSubscriptions returns live subscriptions whose period has ended.
// The status predicate matches the live set (identical to ResolveTier and the
// grant UPSERT) so an already-'expired'/'canceled' row is never re-selected —
// this is what makes the sweep idempotent across restarts.
//
// The source predicate scopes the sweep to code-owned grants ('code') and
// deliberately excludes 'stripe'. A Stripe subscription's lifecycle (renewal,
// past_due grace, terminal cancel) is owned by the Stripe
// webhook path (#1306); its local current_period_end can lag a delayed renewal
// webhook, so expiring a stripe row here would fire a premium->free
// downgrade/disconnect before Stripe has declared the subscription terminal.
// This mirrors the redemption grant path (redemption/grants.go), which likewise
// refuses to rewrite a 'stripe' source. Today every live row is source='code'
// (no Stripe integration yet), so the predicate is a forward-safe no-op.
func (s *ExpirySweeper) selectExpiredSubscriptions(ctx context.Context) ([]expiredSubscription, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, user_id, tier
		  FROM subscriptions
		 WHERE status IN ('active', 'trialing', 'past_due')
		   AND source = 'code'
		   AND current_period_end IS NOT NULL
		   AND current_period_end <= NOW()`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var out []expiredSubscription
	for rows.Next() {
		var e expiredSubscription
		if scanErr := rows.Scan(&e.id, &e.userID, &e.oldTier); scanErr != nil {
			return nil, scanErr
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// expireOne flips a single row to 'expired' and, only if that flip actually took,
// fires the tier-change notification. The flip and notify run in ONE transaction
// under the redemption path's user lock: it is committed only after the notify
// succeeds (at-least-once), and the lock (serialize) stops a concurrent
// redemption from interleaving between them.
func (s *ExpirySweeper) expireOne(ctx context.Context, e expiredSubscription) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin expiry tx: %w", err)
	}
	// Rollback is a no-op after a successful Commit; on any early return it
	// discards the flip so the row stays live for the next sweep to retry.
	defer func() { _ = tx.Rollback() }()

	// Serialize with the redemption grant path (redemption/grants.go), which locks
	// the users row FOR UPDATE before it reads/writes the subscription. Taking the
	// SAME lock here means a concurrent code redemption that extends the period
	// cannot interleave between our flip and our notification (it would otherwise
	// let us expire — and notify a spurious premium→free downgrade for — a
	// subscription the user just re-upped). The lock order is identical (users row
	// only), so it cannot deadlock with the redemption path.
	if _, err := tx.ExecContext(ctx, `SELECT 1 FROM users WHERE id = $1 FOR UPDATE`, e.userID); err != nil {
		return fmt.Errorf("lock user for expiry: %w", err)
	}

	// Conditionally flip ONLY if still live AND still past period end. The status
	// predicate makes this idempotent (a concurrent sweep or restart that already
	// expired the row updates 0 rows). The current_period_end re-check, now under
	// the user lock, is authoritative: an extended (still-valid) subscription
	// updates 0 rows and is never expired.
	res, err := tx.ExecContext(ctx, `
		UPDATE subscriptions
		   SET status = 'expired', updated_at = NOW()
		 WHERE id = $1
		   AND status IN ('active', 'trialing', 'past_due')
		   AND source = 'code'
		   AND current_period_end IS NOT NULL
		   AND current_period_end <= NOW()`, e.id)
	if err != nil {
		return fmt.Errorf("flip subscription to expired: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}
	if n == 0 {
		// Concurrent extend/flip won under the lock — nothing to expire or notify.
		return nil
	}

	// Resolve the post-expiry tier (normally free; premium under the self-hosted
	// short-circuit). oldTier is the expiring row's own tier, so a premium grant
	// lapsing to free is a downgrade → OnTierChange invalidates + pushes +
	// disconnects; a non-premium/self-hosted expiry degrades to invalidate + push.
	//
	// Notify BEFORE commit (at-least-once): the flip is committed only if the
	// notification succeeds, so a failed or crashed notify rolls back the flip and
	// the row stays live for the next sweep to retry — a crash between "marked
	// expired" and "client told" can never permanently suppress the downgrade. The
	// notify runs while we still hold the user lock, so no concurrent redemption
	// can extend the period between the flip and the notify; OnTierChange's work is
	// an in-memory hub push + a Redis Unlink (fast), so the lock is held only
	// briefly. A re-run after a post-notify crash re-notifies — OnTierChange is
	// idempotent, so at-least-once (never at-most-once) is the intended trade.
	newTier := s.resolveNewTier(ctx, e.userID)
	if err := s.changer.OnTierChange(ctx, e.userID, e.oldTier, newTier); err != nil {
		return fmt.Errorf("notify tier change after expiry: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit expiry: %w", err)
	}
	return nil
}

// resolveNewTier returns the tier the user holds AFTER the expiring row was
// flipped. It mirrors entitlements.Cache.GetTier's deployment-mode branch: on a
// self-hosted instance every user is premium regardless of subscription rows, so
// short-circuit to TierPremium (an expired row there yields premium->premium, a
// no-op notification rather than a spurious downgrade/disconnect). On SaaS,
// DB-resolve the remaining live entitlement (normally none → free).
func (s *ExpirySweeper) resolveNewTier(ctx context.Context, userID uuid.UUID) string {
	if s.selfHosted {
		return entitlements.TierPremium
	}
	return entitlements.ResolveTier(ctx, s.db, userID.String())
}

// StartExpirySweepWorker launches a goroutine that runs the expiry sweep once at
// startup (catching expiries that occurred while the process was down — the
// across-restart idempotency requirement) and then on a fixed interval. It stops
// cleanly when ctx is cancelled. Mirrors voice.StartTempGrantSweepWorker (the
// established background-job pattern); failures are logged, never fatal.
func StartExpirySweepWorker(
	ctx context.Context,
	db *sql.DB,
	log *logger.Logger,
	cache *entitlements.Cache,
	notifier entitlements.SessionNotifier,
	interval time.Duration,
) {
	sweeper := NewExpirySweeper(db, log, cache, notifier)
	ticker := time.NewTicker(interval)
	go func() {
		defer ticker.Stop()
		if n, err := sweeper.SweepExpired(ctx); err != nil {
			log.Warn("subscription expiry sweep: startup sweep failed", "error", err)
		} else if n > 0 {
			log.Info("subscription expiry sweep: startup", "expired", n)
		}
		for {
			select {
			case <-ctx.Done():
				log.Info("subscription expiry sweep worker stopped")
				return
			case <-ticker.C:
				if n, err := sweeper.SweepExpired(ctx); err != nil {
					log.Warn("subscription expiry sweep: sweep failed", "error", err)
				} else if n > 0 {
					log.Info("subscription expiry sweep: expired subscriptions", "expired", n)
				}
			}
		}
	}()
}
