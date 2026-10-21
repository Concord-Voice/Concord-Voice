package entitlements

import (
	"context"
	"database/sql"
)

// TierResolver resolves a user's current subscription tier. *Cache satisfies it
// (Redis read-through, fail-closed to free). Consumers (users/media handlers)
// depend on this interface so enforcement logic can be tested with a stub,
// without a live Redis.
type TierResolver interface {
	GetTier(ctx context.Context, userID string) string
}

// ResolveTier returns the user's current tier from the subscriptions source of
// truth, read on the pool. It is ResolveTierQ over a *sql.DB: the highest Rank
// across the user's live rows (LiveRowPredicate), failing closed to TierFree on
// no live row OR any error (least privilege), so a DB hiccup can never escalate
// a user to premium. entitlements.For() remains the fail-closed validation
// chokepoint downstream (unknown tier -> free set). The query with its error
// kept is ResolveTierChecked (live.go).
func ResolveTier(ctx context.Context, db *sql.DB, userID string) string {
	return ResolveTierQ(ctx, db, userID)
}
