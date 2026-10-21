package entitlements

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// LiveRowPredicate is the one definition of "this subscriptions row entitles
// its user right now". It is an SQL fragment over unqualified subscriptions
// columns. It begins with the status test and has no leading AND, so a caller
// appends it to its own WHERE inside a constant expression:
//
//	const q = `SELECT tier FROM subscriptions WHERE user_id = $1 AND ` + LiveRowPredicate
//
// Combine it only with AND: its top level is `status IN (...) AND (...)`, and a
// trailing OR would bind to the second term alone.
//
// The status must be in the live set (active, trialing, past_due). Past that, a
// row is live per source (#2033 spec §5):
//   - source='code': current_period_end is NULL (a grant that never lapses) or
//     still in the future. There is no buffer: a code period end is final.
//   - source='stripe': current_period_end is later than 6 hours ago. Stripe
//     renews asynchronously, so a passed period end with no status change is
//     the renewal gap, not an end; a real end arrives as a status change
//     through the webhook or the drift worker. A Stripe row with a NULL period
//     end matches nothing, so the predicate fails closed
//     (subscriptions_stripe_period_check forbids that row anyway). The
//     arithmetic stays on NOW(), not on the column, so
//     idx_subscriptions_stripe_live_period_end can serve a range scan.
//
// Any other source matches nothing.
const LiveRowPredicate = `status IN ('active', 'trialing', 'past_due')
	AND (
		(source = 'code' AND (current_period_end IS NULL OR current_period_end > NOW()))
		OR (source = 'stripe' AND current_period_end > NOW() - INTERVAL '6 hours')
	)`

// Querier is the read method shared by *sql.DB and *sql.Tx, so a tier can be
// resolved on the pool or inside the transaction that is changing it, where it
// sees that transaction's uncommitted writes.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// resolveTierQuery reads the tier of every live row the user holds. Both
// operands are constants, so no SQL text is built at run time.
const resolveTierQuery = `SELECT tier FROM subscriptions WHERE user_id = $1 AND ` + LiveRowPredicate

// ResolveTierQ returns the highest-ranked tier (by Rank) across the user's live
// subscription rows, read through q. Since migration 000179 a user may hold one
// live code row and live Stripe rows at once, so no single row is "the"
// subscription. It fails closed to TierFree on no live row and on any error
// (the query, a row scan or the iteration), even after a premium row was read:
// an incomplete read never escalates a user. Only a tier that outranks TierFree
// can be returned, so an unknown tier string resolves to TierFree.
//
// Errors are logged with the userID %q-quoted (observability.md: %q-escaped
// values need no further sanitization) and no row values.
func ResolveTierQ(ctx context.Context, q Querier, userID string) string {
	tier, err := ResolveTierChecked(ctx, q, userID)
	if err != nil {
		log.Printf("entitlements: ResolveTierQ failed for user %q: %v", userID, err)
		return TierFree
	}
	return tier
}

// ResolveTierChecked is the one tier query, with the error kept. No live row is
// an ANSWER (TierFree, nil); a failed read is not ("", err), even after a
// premium row was read. Callers that hand the tier on rather than enforce with
// it (Cache.GetTierChecked, #1308; the expiry sweep's tier-change push) need
// that difference, which ResolveTierQ's fail-closed free erases. GetTier and
// GetTierChecked both read through here, so a pushed tier and an enforced tier
// come from the same rows.
func ResolveTierChecked(ctx context.Context, q Querier, userID string) (tier string, err error) {
	rows, err := q.QueryContext(ctx, resolveTierQuery, userID)
	if err != nil {
		return "", fmt.Errorf("query: %w", err)
	}
	defer func() {
		if closeErr := rows.Close(); closeErr != nil && err == nil {
			tier, err = "", fmt.Errorf("close: %w", closeErr)
		}
	}()

	best := TierFree
	for rows.Next() {
		var tier string
		if scanErr := rows.Scan(&tier); scanErr != nil {
			return "", fmt.Errorf("scan: %w", scanErr)
		}
		if Rank(tier) > Rank(best) {
			best = tier
		}
	}
	if iterErr := rows.Err(); iterErr != nil {
		return "", fmt.Errorf("rows: %w", iterErr)
	}
	return best, nil
}
