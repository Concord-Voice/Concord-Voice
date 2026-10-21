package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000180 (epic #2033 PR-1, Refs #1305): stripe_customers maps a user
// to the one Stripe Customer created for them. Round trips and refusals run in
// rolled-back transactions, as in the 000178 tests.

const migration000180Refusal = "cannot drop stripe_customers while it maps a user to a Stripe Customer"

func migration000180UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000180_stripe_customers.up.sql")
}

func migration000180DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000180_stripe_customers.down.sql")
}

func migration000180InsertCustomer(q migrationExecer, userID uuid.UUID, customerID string) error {
	_, err := q.Exec(
		`INSERT INTO stripe_customers (user_id, stripe_customer_id) VALUES ($1, $2)`,
		userID, customerID)
	return err
}

func TestMigration000180_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "stripe_customers")
	for _, line := range []string{
		"column user_id uuid - NO -",
		"column stripe_customer_id text - NO -",
		"column created_at timestamp with time zone - NO now()",
		"column last_checkout_at timestamp with time zone - YES -",
		"constraint stripe_customers_pkey PRIMARY KEY (user_id)",
		"constraint stripe_customers_stripe_customer_id_key UNIQUE (stripe_customer_id)",
	} {
		assert.Contains(t, applied, line)
	}
	assert.Contains(t, migrationSnapshotLine(applied, "constraint stripe_customers_user_id_fkey FOREIGN KEY (user_id) REFERENCES "),
		"ON DELETE CASCADE")

	_, err = tx.Exec(migration000180DownSQL(t))
	require.NoError(t, err, "the down must drop an empty table")
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "stripe_customers"), "the down must remove the table")
	_, err = tx.Exec(migration000180DownSQL(t))
	require.NoError(t, err, "a second down must find the table gone and return without error")

	_, err = tx.Exec(migration000180UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "stripe_customers"))
}

func TestMigration000180_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000180DownSQL(t)

	t.Run("refuses while a mapping exists", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000180InsertCustomer(tx, user, "cus_test_"+uuid.NewString()))

		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, "23001", "")
		assert.Contains(t, err.Error(), migration000180Refusal)
	})

	t.Run("checks for a mapping only after it holds the lock", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		migrationRequireGuardRunsUnderLock(t, db, down, "stripe_customers", migration000180Refusal,
			func(tx *sql.Tx) error { return migration000180InsertCustomer(tx, user, "cus_test_"+uuid.NewString()) })
	})

	var present bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('stripe_customers') IS NOT NULL`).Scan(&present))
	assert.True(t, present, "a refused or rolled-back down must leave the table in place")
}

func TestMigration000180_Constraints(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("one Customer per user and one user per Customer", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		customerID := "cus_test_" + uuid.NewString()
		require.NoError(t, migration000180InsertCustomer(db, user, customerID))
		migrationRequirePQError(t, migration000180InsertCustomer(db, user, "cus_test_"+uuid.NewString()),
			"23505", "stripe_customers_pkey")
		migrationRequirePQError(t, migration000180InsertCustomer(db, testhelpers.CreateUser(t, db), customerID),
			"23505", "stripe_customers_stripe_customer_id_key")
	})

	t.Run("created_at defaults and last_checkout_at starts NULL", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000180InsertCustomer(db, user, "cus_test_"+uuid.NewString()))
		var createdSet bool
		var lastCheckout sql.NullTime
		require.NoError(t, db.QueryRow(
			`SELECT created_at IS NOT NULL, last_checkout_at FROM stripe_customers WHERE user_id = $1`, user,
		).Scan(&createdSet, &lastCheckout))
		assert.True(t, createdSet)
		assert.False(t, lastCheckout.Valid)
	})

	t.Run("a mapping needs an existing user", func(t *testing.T) {
		migrationRequirePQError(t, migration000180InsertCustomer(db, uuid.New(), "cus_test_"+uuid.NewString()),
			"23503", "stripe_customers_user_id_fkey")
	})

	t.Run("deleting the user deletes the mapping", func(t *testing.T) {
		user := testhelpers.CreateUser(t, db)
		require.NoError(t, migration000180InsertCustomer(db, user, "cus_test_"+uuid.NewString()))
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, user)
		require.NoError(t, err)
		var rows int
		require.NoError(t, db.QueryRow(`SELECT count(*) FROM stripe_customers WHERE user_id = $1`, user).Scan(&rows))
		assert.Zero(t, rows)
	})
}
