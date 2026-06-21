package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000070_Subscriptions(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("up creates subscriptions table", func(t *testing.T) {
		var exists bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'subscriptions')`,
		).Scan(&exists))
		require.True(t, exists, "subscriptions should exist after migrations apply")
	})

	t.Run("partial unique index forbids two active subscriptions per user", func(t *testing.T) {
		u := ts.CreateTestUser(t, "sub_active_unique")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'active', 'code')`, u.ID)
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'trialing', 'stripe')`, u.ID)
		require.Error(t, err, "second active/trialing subscription for same user must fail")
	})

	t.Run("canceled subscriptions are exempt from the partial unique index", func(t *testing.T) {
		u := ts.CreateTestUser(t, "sub_canceled_exempt")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'canceled', 'stripe')`, u.ID)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'canceled', 'stripe')`, u.ID)
		require.NoError(t, err, "two canceled subscriptions for same user are allowed")
	})

	t.Run("ON DELETE CASCADE removes subscriptions with the user", func(t *testing.T) {
		u := ts.CreateTestUser(t, "sub_cascade")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'free', 'active', 'code')`, u.ID)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, u.ID)
		require.NoError(t, err)
		var count int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM subscriptions WHERE user_id = $1`, u.ID).Scan(&count))
		assert.Equal(t, 0, count)
	})

	t.Run("CHECK constraints reject out-of-enum status and source; tier stays extensible", func(t *testing.T) {
		u := ts.CreateTestUser(t, "sub_check_enum")
		// Out-of-enum (typo'd) status must be rejected — this CHECK is what stops a
		// typo'd status from dodging the partial unique "one active subscription" index.
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'aktiv', 'code')`, u.ID)
		require.Error(t, err, "out-of-enum status must fail the CHECK constraint")

		// Out-of-enum source must be rejected.
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'premium', 'active', 'bogus')`, u.ID)
		require.Error(t, err, "out-of-enum source must fail the CHECK constraint")

		// tier is intentionally CHECK-free (spec §10 extensibility) — a future tier
		// value must still be accepted at the schema layer.
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO subscriptions (user_id, tier, status, source)
			 VALUES ($1, 'enterprise', 'active', 'code')`, u.ID)
		require.NoError(t, err, "tier has no CHECK; a future tier value must be accepted")
	})
}
