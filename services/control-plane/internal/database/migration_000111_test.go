package database_test

import (
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Every CHECK must reject its negative case. A constraint nobody tests is a
// constraint a later migration can silently drop.
func TestMigration000111_RejectsInvalidRows(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)

	cases := []struct {
		name       string
		category   string
		resolution string
		lifecycle  interface{}
		failure    interface{}
	}{
		{"unknown category", "screen_share", "conservative", nil, nil},
		{"unknown resolution", "private_call", "approximate", nil, nil},
		{"exact without lifecycle evidence", "private_call", "exact", nil, nil},
		{"unknown failure class", "private_call", "conservative", nil, "kaboom"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := db.Exec(`
				INSERT INTO presence_active_pending_plans (
					user_id, category, operation_id, resolution,
					scope_lifecycle_id, scope_event_at, failure_class
				) VALUES ($1, $2, gen_random_uuid(), $3, $4, clock_timestamp(), $5)`,
				userID, tc.category, tc.resolution, tc.lifecycle, tc.failure)
			require.Error(t, err, "the CHECK constraint must reject this row")
		})
	}
}

// A user cannot be erased out from under an unresolved obligation. This is what
// makes the erasure drain in a later task mandatory rather than optional.
func TestMigration000111_ForeignKeyRestrictsUserDeletion(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)

	_, err := db.Exec(`
		INSERT INTO presence_active_pending_plans (
			user_id, category, operation_id, resolution, scope_event_at
		) VALUES ($1, 'private_call', gen_random_uuid(), 'conservative', clock_timestamp())`,
		userID)
	require.NoError(t, err)

	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.Error(t, err, "ON DELETE RESTRICT must block the erasure while a plan survives")
	require.Contains(t, err.Error(), "presence_active_pending_plans")
}

// The down migration must refuse to destroy unresolved evidence, and must run
// clean once the table is empty. Executed inside rolled-back transactions so
// the test does not leave the schema downgraded for the rest of the package.
func TestMigration000111_DownGuard(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)

	down := migration000111DownSQL(t)

	t.Run("refuses while a plan row survives", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(`
			INSERT INTO presence_active_pending_plans (
				user_id, category, operation_id, resolution, scope_event_at
			) VALUES ($1, 'server_voice', gen_random_uuid(), 'conservative', clock_timestamp())`,
			userID)
		require.NoError(t, err)

		_, err = tx.Exec(down)
		require.Error(t, err)
		require.Contains(t, err.Error(), "cannot drop presence_active_pending_plans")
	})

	t.Run("drops cleanly when empty", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(down)
		require.NoError(t, err)

		var exists bool
		require.NoError(t, tx.QueryRow(
			`SELECT to_regclass('public.presence_active_pending_plans') IS NOT NULL`,
		).Scan(&exists))
		require.False(t, exists)
	})
}

func migration000111DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000111_add_active_pending_plans.down.sql")
}

// The table above cannot reach these two CHECKs: its shared INSERT leaves
// attempts and reconcile_after at their defaults. Review finding -- the comment
// on TestMigration000111_RejectsInvalidRows claims EVERY CHECK has a negative
// case, and two did not. A constraint nobody tests is a constraint a later
// migration can silently drop.
func TestMigration000111_RejectsNegativeAttemptsAndBackdatedReconcile(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)

	t.Run("negative attempts", func(t *testing.T) {
		_, err := db.Exec(`
			INSERT INTO presence_active_pending_plans (
				user_id, category, operation_id, resolution, scope_event_at, attempts
			) VALUES ($1, 'private_call', gen_random_uuid(), 'conservative', clock_timestamp(), -1)`,
			userID)
		require.Error(t, err, "attempts >= 0 must reject a negative counter")
	})

	t.Run("reconcile_after before created_at", func(t *testing.T) {
		_, err := db.Exec(`
			INSERT INTO presence_active_pending_plans (
				user_id, category, operation_id, resolution, scope_event_at,
				created_at, reconcile_after
			) VALUES ($1, 'server_voice', gen_random_uuid(), 'conservative', clock_timestamp(),
			          clock_timestamp(), clock_timestamp() - INTERVAL '1 hour')`,
			userID)
		require.Error(t, err, "reconcile_after >= created_at must reject a backdated lease")
	})
}
