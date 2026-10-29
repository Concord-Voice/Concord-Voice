package database_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000191 (#3680 follow-up to #4129, red-team-critical F1 and F2):
// accountguard_is_rewrap() accepts only 'mfa-rekey:' || pg_current_xact_id(),
// and the down refuses while accountguard_state holds no row.
//
// The transaction tests run the down and up FILE TEXT inside a transaction that
// is always rolled back, so the function under test is the file's. As in the
// 000189 tests, SetupTestDB is called first so its TRUNCATE cleanup runs after
// the rollback (t.Cleanup is last-in first-out).

// m191SetBound sets the flag transaction-locally to value bound to the current
// transaction id, the form internal/mfa/rekey.go writes.
const m191SetBound = `SELECT set_config($1, $2::text || ':' || pg_current_xact_id()::text, true)`

func migration000191UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000191_accountguard_rewrap_xact_bind.up.sql")
}

func migration000191DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000191_accountguard_rewrap_xact_bind.down.sql")
}

// migration000191Tx opens a rolled-back-on-cleanup transaction on the migrated
// schema; the caller must already have called SetupTestDB.
func migration000191Tx(t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.Begin()
	require.NoError(t, err)
	t.Cleanup(func() { migrationRollback(t, tx) })
	return tx
}

// m191IsRewrap sets the flag with query (skipped when empty) inside a savepoint
// and returns accountguard_is_rewrap().
func m191IsRewrap(t *testing.T, tx *sql.Tx, query, value string) bool {
	t.Helper()
	var ok bool
	agSavepoint(t, tx, func() {
		if query != "" {
			_, err := tx.Exec(query, agRewrapGUC, value)
			require.NoError(t, err)
		}
		require.NoError(t, tx.QueryRow(`SELECT accountguard_is_rewrap()`).Scan(&ok))
	})
	return ok
}

const m191SetBare = `SELECT set_config($1, $2, true)`

func TestMigration000191_UpBindsRewrapToTheTransaction(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000191Tx(t, db)
	_, err := tx.Exec(migration000191DownSQL(t))
	require.NoError(t, err, "the down must run on the migrated schema")
	_, err = tx.Exec(migration000191UpSQL(t))
	require.NoError(t, err)

	assert.False(t, m191IsRewrap(t, tx, "", ""), "unset")
	assert.True(t, m191IsRewrap(t, tx, m191SetBound, agRewrapValue), "bound to this transaction")
	assert.False(t, m191IsRewrap(t, tx, m191SetBare, agRewrapValue), "the bare value no longer exempts")
	assert.False(t, m191IsRewrap(t, tx, m191SetBare, agRewrapValue+":"), "an empty id")
	assert.False(t, m191IsRewrap(t, tx, m191SetBound, "MFA-REKEY"), "the value is case-sensitive")
	assert.False(t, m191IsRewrap(t, tx, m191SetBound, ""), "an empty value")
	assert.False(t, m191IsRewrap(t, tx,
		`SELECT set_config($1, $2::text || ':' || (pg_current_xact_id()::text::bigint + 1)::text, true)`, agRewrapValue),
		"another transaction's id")
}

func TestMigration000191_DownRestoresTheBareValueAndUpReapplies(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000191Tx(t, db)

	_, err := tx.Exec(migration000191DownSQL(t))
	require.NoError(t, err, "the seeded accountguard_state row must not refuse the down")
	assert.True(t, m191IsRewrap(t, tx, m191SetBare, agRewrapValue), "after the down, 000189's bare value exempts again")
	assert.False(t, m191IsRewrap(t, tx, m191SetBound, agRewrapValue), "after the down, the bound form is not 000189's value")

	_, err = tx.Exec(migration000191UpSQL(t))
	require.NoError(t, err, "the up must run again after the down")
	assert.True(t, m191IsRewrap(t, tx, m191SetBound, agRewrapValue))
	assert.False(t, m191IsRewrap(t, tx, m191SetBare, agRewrapValue))
}

// F1: a missing accountguard_state row makes the activation state unknown, so
// the down refuses (000189's down alone would read it as "not activated").
func TestMigration000191_DownRefusesWithoutAnActivationRow(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	tx := migration000191Tx(t, db)

	_, err := tx.Exec(`DELETE FROM accountguard_state`)
	require.NoError(t, err)
	_, err = tx.Exec(migration000191DownSQL(t))
	migrationRequirePQError(t, err, "23001", "")
	assert.Contains(t, err.Error(), "cannot roll back 000191: accountguard_state holds no row")
}

// F2: a flag set at session level (is_local = false) on a pooled connection
// does not exempt a later autocommit swap on that connection, whether it holds
// the bare value or a value bound to the transaction that set it. The control
// arm shows the same swap is exempt when the flag is bound in its own
// transaction, so the material arms are not vacuous.
func TestMigration000191_SessionLevelFlagDoesNotExemptALaterTransaction(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ctx := context.Background()

	pool, err := sql.Open("postgres", testdb.DatabaseURL())
	require.NoError(t, err)
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { assert.NoError(t, pool.Close()) })

	seed := func(t *testing.T) (uuid.UUID, int64) {
		t.Helper()
		id := uuid.New()
		require.NoError(t, migration000189InsertUser(db, id))
		var v int64
		require.NoError(t, db.QueryRow(`
			INSERT INTO user_mfa_totp (user_id, totp_secret_enc, totp_secret_nonce, key_version, enabled, confirmed)
			VALUES ($1, 'enc', 'nonce', 1, TRUE, TRUE) RETURNING material_version`, id).Scan(&v))
		return id, v
	}
	// Rewrap-shaped: only the secret, nonce and key_version move, so the flag is
	// the only thing that could exempt it.
	const swap = `UPDATE user_mfa_totp
		   SET totp_secret_enc = 'swapped', totp_secret_nonce = 'nonce2', key_version = key_version + 1 -- pragma: allowlist secret
		 WHERE user_id = $1 RETURNING material_version`

	for _, tc := range []struct{ name, set string }{
		{"bare value", `SELECT set_config('concord.credential_rewrap', 'mfa-rekey', false)`},
		{"value bound to the setting transaction",
			`SELECT set_config('concord.credential_rewrap', 'mfa-rekey:' || pg_current_xact_id()::text, false)`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, before := seed(t)
			conn, err := pool.Conn(ctx)
			require.NoError(t, err)
			defer func() { assert.NoError(t, conn.Close()) }()
			_, err = conn.ExecContext(ctx, tc.set)
			require.NoError(t, err)
			t.Cleanup(func() { _, _ = pool.Exec(`RESET concord.credential_rewrap`) })

			var flag string
			require.NoError(t, conn.QueryRowContext(ctx, `SELECT current_setting($1, true)`, agRewrapGUC).Scan(&flag))
			require.NotEmpty(t, flag, "the session-level flag must still be set for the swap")

			var after int64
			require.NoError(t, conn.QueryRowContext(ctx, swap, id).Scan(&after))
			assert.NotEqual(t, before, after, "a session-level flag must not exempt a later autocommit swap")
		})
	}

	t.Run("control: bound in its own transaction", func(t *testing.T) {
		id, before := seed(t)
		tx, err := pool.BeginTx(ctx, nil)
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		_, err = tx.Exec(m191SetBound, agRewrapGUC, agRewrapValue)
		require.NoError(t, err)
		var after int64
		require.NoError(t, tx.QueryRow(swap, id).Scan(&after))
		require.NoError(t, tx.Commit())
		assert.Equal(t, before, after, "a flag bound in the writing transaction exempts the rewrap")
	})
}
