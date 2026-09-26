package database_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

// Migration 000158 (add_user_mfa_totp_last_used_step): up -> down -> up
// round-trips, and the column comes back nullable BIGINT with no default.
//
// SetupTestDB already runs every migration up to head, so each check runs the
// .up.sql / .down.sql contents directly inside a transaction that is always
// rolled back -- the migration_000157_test.go shape -- and the shared schema
// used by the rest of this package's test binary is never downgraded.

func migration000158UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000158_add_user_mfa_totp_last_used_step.up.sql")
}

func migration000158DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000158_add_user_mfa_totp_last_used_step.down.sql")
}

// migration000158Column reads the column's shape through tx, so it observes
// that transaction's own uncommitted DDL. present is false when the column is
// absent, and the other results are then meaningless.
func migration000158Column(t *testing.T, tx *sql.Tx) (present bool, dataType, nullable string, hasDefault bool) {
	t.Helper()
	err := tx.QueryRow(
		`SELECT data_type, is_nullable, column_default IS NOT NULL
		   FROM information_schema.columns
		  WHERE table_schema = current_schema()
		    AND table_name = 'user_mfa_totp'
		    AND column_name = 'last_used_step'`,
	).Scan(&dataType, &nullable, &hasDefault)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", "", false
	}
	require.NoError(t, err)
	return true, dataType, nullable, hasDefault
}

func requireMigration000158Shape(t *testing.T, tx *sql.Tx, step string) {
	t.Helper()
	present, dataType, nullable, hasDefault := migration000158Column(t, tx)
	require.True(t, present, "column must exist %s", step)
	require.Equal(t, "bigint", dataType, "column must be BIGINT %s", step)
	require.Equal(t, "YES", nullable, "column must be nullable %s", step)
	require.False(t, hasDefault, "column must carry no default %s", step)
}

func TestMigration000158_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migration000158UpSQL(t)
	down := migration000158DownSQL(t)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// Up is idempotent (ADD COLUMN IF NOT EXISTS): the column already exists
	// from SetupTestDB.
	_, err = tx.Exec(up)
	require.NoError(t, err, "up must be safe to re-run")
	requireMigration000158Shape(t, tx, "after up")

	_, err = tx.Exec(down)
	require.NoError(t, err, "down must succeed")
	present, _, _, _ := migration000158Column(t, tx)
	require.False(t, present, "column must be gone after down")

	// Down is idempotent too (DROP COLUMN IF EXISTS).
	_, err = tx.Exec(down)
	require.NoError(t, err, "down must be safe to re-run")

	_, err = tx.Exec(up)
	require.NoError(t, err, "up must recreate the column")
	requireMigration000158Shape(t, tx, "after the second up")
}

// TestMigration000158_ExistingRowsReadNull pins "no backfill": a row that
// predates the column reads NULL, which the compare-and-advance guard treats
// as "no step accepted yet".
func TestMigration000158_ExistingRowsReadNull(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	userID := testhelpers.CreateUser(t, db)

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(migration000158DownSQL(t))
	require.NoError(t, err)
	_, err = tx.Exec(
		`INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce) VALUES ($1, '\x00', '\x00')`,
		userID,
	)
	require.NoError(t, err)
	_, err = tx.Exec(migration000158UpSQL(t))
	require.NoError(t, err)

	var step sql.NullInt64
	require.NoError(t, tx.QueryRow(
		`SELECT last_used_step FROM user_mfa_totp WHERE user_id = $1`, userID,
	).Scan(&step))
	require.False(t, step.Valid, "a row that predates the column must read NULL")
}
