package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// M-1 of #3453 (T8): migration 000157 (add_server_mfa_enforcement). up ->
// down -> up round-trips, the guarded down refuses while any server enforces
// (naming the count) and leaves the column intact, and the down succeeds once
// every row is reset to FALSE.
//
// SetupTestDB already runs every migration up to head, so the column exists
// before this test ever executes a statement of its own. Every check below
// therefore runs the .up.sql / .down.sql file contents directly, inside a
// transaction that is always rolled back at the end of the subtest -- the
// TestMigration000111_DownGuard shape -- so the shared schema used by the
// rest of this package's test binary is never actually downgraded.

func migration000157UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000157_add_server_mfa_enforcement.up.sql")
}

func migration000157DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000157_add_server_mfa_enforcement.down.sql")
}

// migration000157ColumnPresent queries information_schema through the given
// transaction so it observes that transaction's own uncommitted DDL, rather
// than a separate connection's committed-only view.
func migration000157ColumnPresent(t *testing.T, tx *sql.Tx) bool {
	t.Helper()
	var present bool
	require.NoError(t, tx.QueryRow(
		`SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_schema = current_schema()
			  AND table_name = 'servers'
			  AND column_name = 'enforce_mfa_dangerous_actions'
		)`,
	).Scan(&present))
	return present
}

// TestMigration000157_UpDownUpRoundTrip runs up (idempotent -- the column
// already exists via SetupTestDB) then down then up again inside one
// transaction, and confirms the column is present at every step it should be.
func TestMigration000157_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migration000157UpSQL(t)
	down := migration000157DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// Up is idempotent (ADD COLUMN IF NOT EXISTS): running it again against a
	// schema that already has the column from SetupTestDB must not error.
	_, err = tx.Exec(up)
	require.NoError(t, err, "up must be safe to re-run")
	require.True(t, migration000157ColumnPresent(t, tx), "column must exist after up")

	// No server enforces in this fresh transaction, so down must succeed.
	_, err = tx.Exec(down)
	require.NoError(t, err, "down must succeed when no server enforces")
	require.False(t, migration000157ColumnPresent(t, tx), "column must be gone after down")

	// Up again, completing the round trip.
	_, err = tx.Exec(up)
	require.NoError(t, err, "up must recreate the column")
	require.True(t, migration000157ColumnPresent(t, tx), "column must exist after the second up")
}

// TestMigration000157_DownRefusesWhileAnyServerEnforces is the guarded-down
// half of M-1: a TRUE row must refuse the down, name the count in the error,
// and leave the column (and the row) intact. Resetting every row to FALSE
// must then let the identical down succeed.
func TestMigration000157_DownRefusesWhileAnyServerEnforces(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000157DownSQL(t)

	owner := testhelpers.CreateUser(t, db)
	serverID := testhelpers.CreateServer(t, db, owner)

	t.Run("refuses and leaves the column intact", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = TRUE WHERE id = $1`, serverID)
		require.NoError(t, err)

		_, err = tx.Exec(down)
		require.Error(t, err, "the down must refuse while a server enforces")
		require.Contains(t, err.Error(), "cannot remove servers.enforce_mfa_dangerous_actions")
		require.Contains(t, err.Error(), "1 server(s) enforce it", "the refusal must name the count")

		// The failed statement poisons this transaction (25P02); the column
		// itself is unaffected because the whole down never committed.
	})

	t.Run("succeeds once every row is reset to FALSE", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(`UPDATE servers SET enforce_mfa_dangerous_actions = FALSE WHERE id = $1`, serverID)
		require.NoError(t, err)

		_, err = tx.Exec(down)
		require.NoError(t, err, "the down must succeed once no server enforces")
		require.False(t, migration000157ColumnPresent(t, tx), "column must be gone after a clean down")
	})

	// The real schema (outside any test transaction) was never touched: both
	// subtests ran inside transactions that were rolled back, and this direct
	// check confirms the column -- and the row this test wrote -- survive.
	var enforcing bool
	require.NoError(t, db.QueryRow(
		`SELECT enforce_mfa_dangerous_actions FROM servers WHERE id = $1`, serverID,
	).Scan(&enforcing))
	require.False(t, enforcing, "the row this test wrote inside rolled-back transactions must not have leaked out")
}
