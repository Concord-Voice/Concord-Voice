package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000061_UpDownSymmetry(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("up creates user_sso_identities and adds users columns", func(t *testing.T) {
		var exists bool
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_name = 'user_sso_identities')`,
		).Scan(&exists))
		require.True(t, exists, "user_sso_identities should exist after migrations apply")

		var col string
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_name = 'users' AND column_name = 'password_login_disabled'`,
		).Scan(&col))
		require.Equal(t, "password_login_disabled", col)

		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT column_name FROM information_schema.columns
			 WHERE table_name = 'users' AND column_name = 'trust_sso_security'`,
		).Scan(&col))
		require.Equal(t, "trust_sso_security", col)
	})

	t.Run("unique (provider, provider_user_id) enforced", func(t *testing.T) {
		alice := ts.CreateTestUser(t, "alice_sso_unique")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
			 VALUES ($1, 'google', 'sub-123', 'alice@example.test')`,
			alice.ID,
		)
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
			 VALUES ($1, 'google', 'sub-123', 'alice@example.test')`,
			alice.ID,
		)
		require.Error(t, err, "duplicate (google, sub-123) must fail")
	})

	t.Run("unique (user_id, provider) enforced", func(t *testing.T) {
		// A single Concord user must not accumulate two identities for the
		// same provider — the unlink path operates on (user_id, provider) and
		// would silently leave the duplicate row behind.
		carol := ts.CreateTestUser(t, "carol_sso_user_provider_unique")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
			 VALUES ($1, 'google', 'sub-789', 'carol@example.test')`,
			carol.ID,
		)
		require.NoError(t, err)

		// Different provider_user_id but same (user, provider) — must fail
		// the new UNIQUE (user_id, provider) constraint.
		_, err = ts.DB.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
			 VALUES ($1, 'google', 'sub-790', 'carol-alt@example.test')`,
			carol.ID,
		)
		require.Error(t, err, "second 'google' identity for same user must fail")
	})

	t.Run("ON DELETE CASCADE removes sso identities with user", func(t *testing.T) {
		bob := ts.CreateTestUser(t, "bob_sso_cascade")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_sso_identities (user_id, provider, provider_user_id, provider_email)
			 VALUES ($1, 'google', 'sub-456', 'bob@example.test')`,
			bob.ID,
		)
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, bob.ID)
		require.NoError(t, err)

		var count int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM user_sso_identities WHERE user_id = $1`, bob.ID,
		).Scan(&count))
		assert.Equal(t, 0, count)
	})
}
