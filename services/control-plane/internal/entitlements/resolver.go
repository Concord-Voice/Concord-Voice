package entitlements

import (
	"context"
	"database/sql"
	"errors"
	"log"
)

// TierResolver resolves a user's current subscription tier. *Cache satisfies it
// (Redis read-through, fail-closed to free). Consumers (users/media handlers)
// depend on this interface so enforcement logic can be tested with a stub,
// without a live Redis.
type TierResolver interface {
	GetTier(ctx context.Context, userID string) string
}

// ResolveTier returns the user's current tier from the subscriptions source of
// truth. It fails closed to TierFree on no live row OR any error (least
// privilege) — a DB hiccup can never escalate a user to premium. The returned
// string is not validated here; entitlements.For() is the fail-closed
// validation chokepoint downstream (unknown tier -> free set).
//
// The partial unique index idx_subscriptions_user_active guarantees at most one
// row matching the live-status set, so LIMIT 1 is exact, not arbitrary.
func ResolveTier(ctx context.Context, db *sql.DB, userID string) string {
	const q = `SELECT tier FROM subscriptions
	           WHERE user_id = $1
	             AND status IN ('active', 'trialing', 'past_due')
	             AND (current_period_end IS NULL OR current_period_end > NOW())
	           LIMIT 1`
	var tier string
	err := db.QueryRowContext(ctx, q, userID).Scan(&tier)
	if errors.Is(err, sql.ErrNoRows) {
		return TierFree
	}
	if err != nil {
		// Degrade, don't fail open. %q quoting is injection-safe for the userID
		// (observability.md: %q-escaped values need no further sanitization).
		log.Printf("entitlements: ResolveTier failed for user %q: %v", userID, err)
		return TierFree
	}
	return tier
}
