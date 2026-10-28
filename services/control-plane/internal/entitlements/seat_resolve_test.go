package entitlements_test

import (
	"context"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/entitlements"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A Wing seat entitles its holder only through a live, premium, Stripe Wing
// parent (#2033 PR-5, LiveSeatParentPredicate).
func TestResolveTierQ_WingSeat(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()

	cases := []struct {
		name               string
		plan, tier, status string
		seatState          string
		periodOffset       time.Duration
		want               string
	}{
		{"active seat, live wing", "wing", "premium", "active", "active", time.Hour, entitlements.TierPremium},
		{"past_due keeps the seat", "wing", "premium", "past_due", "active", time.Hour, entitlements.TierPremium},
		{"pending invite", "wing", "premium", "active", "pending", time.Hour, entitlements.TierFree},
		{"paused parent", "wing", "premium", "paused", "active", time.Hour, entitlements.TierFree},
		{"lapsed past the buffer", "wing", "premium", "active", "active", -7 * time.Hour, entitlements.TierFree},
		{"supersonic parent", "supersonic", "premium", "active", "active", time.Hour, entitlements.TierFree},
		{"malformed parent", "wing", "free", "active", "active", time.Hour, entitlements.TierFree},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payer, holder := testhelpers.CreateUser(t, db), testhelpers.CreateUser(t, db)
			var sub uuid.UUID
			require.NoError(t, db.QueryRow(`INSERT INTO subscriptions
				(user_id, tier, status, source, current_period_end, stripe_customer_id, stripe_subscription_id, plan)
				VALUES ($1, $2, $3, 'stripe', NOW() + make_interval(secs => $4), $5, $6, $7) RETURNING id`,
				payer, tc.tier, tc.status, tc.periodOffset.Seconds(), "cus_"+uuid.NewString(), "sub_"+uuid.NewString(), tc.plan).Scan(&sub))
			_, err := db.Exec(`INSERT INTO subscription_seats
				(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at, activated_at)
				VALUES ($1, $2, $3, $4::text, NOW() + INTERVAL '7 days', CASE WHEN $4::text = 'active' THEN NOW() END)`,
				sub, payer, holder, tc.seatState)
			require.NoError(t, err)
			got, err := entitlements.ResolveTierChecked(ctx, db, holder.String())
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
