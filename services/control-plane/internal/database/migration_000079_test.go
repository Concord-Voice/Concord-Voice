package database_test

import (
	"context"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000079 folds existing usernames to lowercase and adds a
// case-insensitive unique index users_username_lower_key ON users (LOWER(username)),
// closing the username-case-consistency class (#1931).
//
// Scope note (mirrors the migration_000078 precedent): SetupTestServer applies
// ALL migrations at setup against an empty DB, so the test cannot seed a
// duplicate-by-case pair BEFORE 000079 runs — the migration-time collision-abort
// (the DO $$ RAISE EXCEPTION block) is therefore NOT runtime-exercised here. It
// holds structurally: a clean migrate has no collisions to hit, and the DO block
// is read-verifiable in 000079_username_case_normalization.up.sql. What IS tested
// is the durable runtime guarantee the index provides post-migration: a
// duplicate-by-case INSERT is rejected (the same protection, enforced continuously
// rather than only at migrate time).
func TestMigration000079_UsernameLowerUniqueIndex(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("the LOWER(username) unique index exists", func(t *testing.T) {
		var n int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM pg_indexes WHERE indexname = 'users_username_lower_key'`).Scan(&n))
		assert.Equal(t, 1, n, "migration 000079 must create users_username_lower_key")
	})

	t.Run("a duplicate-by-case username is rejected by the index", func(t *testing.T) {
		// CreateTestUser stores 'casedupe' (lowercase). A direct INSERT of a
		// case-variant 'CaseDupe' does NOT collide on the raw users_username_key
		// (different bytes) — it must be rejected by users_username_lower_key.
		_ = ts.CreateTestUser(t, "casedupe")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO users (id, email, username, password_hash, email_verified, age_verified)
			 VALUES (gen_random_uuid(), 'casedupe-variant@example.test', 'CaseDupe', 'x', TRUE, TRUE)`)
		require.Error(t, err,
			"a case-variant of an existing username must violate users_username_lower_key")
	})
}
