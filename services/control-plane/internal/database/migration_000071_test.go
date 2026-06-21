package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestMigration000071_RedemptionCodes(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("up creates redemption_codes table", func(t *testing.T) {
		var exists bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'redemption_codes')`,
		).Scan(&exists))
		require.True(t, exists, "redemption_codes should exist after migrations apply")
	})

	t.Run("code_hash UNIQUE rejects a duplicate hash", func(t *testing.T) {
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO redemption_codes (code_hash, grant_kind)
			 VALUES ('hash-dup-069', 'premium:subscription')`)
		require.NoError(t, err)
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO redemption_codes (code_hash, grant_kind)
			 VALUES ('hash-dup-069', 'premium:subscription')`)
		require.Error(t, err, "duplicate code_hash must fail the UNIQUE constraint")
	})

	t.Run("created_by ON DELETE SET NULL preserves the code row", func(t *testing.T) {
		issuer := ts.CreateTestUser(t, "code_issuer_069")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO redemption_codes (code_hash, grant_kind, created_by)
			 VALUES ('hash-issuer-069', 'premium:subscription', $1)`, issuer.ID)
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, issuer.ID)
		require.NoError(t, err)

		var createdBy *string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT created_by FROM redemption_codes WHERE code_hash = 'hash-issuer-069'`).Scan(&createdBy))
		require.Nil(t, createdBy, "created_by should be NULL after issuer deletion; the code row is preserved")
	})
}
