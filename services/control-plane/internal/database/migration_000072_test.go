package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000072_CodeRedemptions(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("up creates code_redemptions table", func(t *testing.T) {
		var exists bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'code_redemptions')`,
		).Scan(&exists))
		require.True(t, exists, "code_redemptions should exist after migrations apply")
	})

	t.Run("UNIQUE(code_id,user_id) rejects a double redemption", func(t *testing.T) {
		u := ts.CreateTestUser(t, "redeemer_070")
		var codeID string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`INSERT INTO redemption_codes (code_hash, grant_kind)
			 VALUES ('hash-070-dup', 'premium:subscription') RETURNING id`).Scan(&codeID))

		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO code_redemptions (code_id, user_id) VALUES ($1, $2)`, codeID, u.ID)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO code_redemptions (code_id, user_id) VALUES ($1, $2)`, codeID, u.ID)
		require.Error(t, err, "same (code_id, user_id) must fail the UNIQUE constraint")
	})

	t.Run("ON DELETE CASCADE removes ledger rows when the code is deleted", func(t *testing.T) {
		u := ts.CreateTestUser(t, "redeemer_070_cascade")
		var codeID string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`INSERT INTO redemption_codes (code_hash, grant_kind)
			 VALUES ('hash-070-cascade', 'premium:subscription') RETURNING id`).Scan(&codeID))
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO code_redemptions (code_id, user_id) VALUES ($1, $2)`, codeID, u.ID)
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx, `DELETE FROM redemption_codes WHERE id = $1`, codeID)
		require.NoError(t, err)
		var count int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM code_redemptions WHERE code_id = $1`, codeID).Scan(&count))
		assert.Equal(t, 0, count)
	})
}
