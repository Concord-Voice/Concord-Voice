package database_test

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000182 (epic #2033 PR-1, Refs #1305): billing_erasure_outbox, the
// durable obligations to erase a Stripe Customer after its user is gone. Rows
// carry no user id and no foreign key. Round trips and refusals run in
// rolled-back transactions, as in the 000178 tests.

const migration000182Refusal = "cannot drop billing_erasure_outbox while Stripe Customer erasure obligations are pending"

func migration000182UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000182_billing_erasure_outbox.up.sql")
}

func migration000182DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000182_billing_erasure_outbox.down.sql")
}

// migration000182Enqueue is the writers' idiom: one obligation per Customer,
// a repeat enqueue is a no-op.
func migration000182Enqueue(q migrationExecer, customerID string) (int64, error) {
	res, err := q.Exec(`
		INSERT INTO billing_erasure_outbox (stripe_customer_id) VALUES ($1)
		ON CONFLICT (stripe_customer_id) DO NOTHING`, customerID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func TestMigration000182_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "billing_erasure_outbox")
	for _, line := range []string{
		"column id uuid - NO gen_random_uuid()",
		"column stripe_customer_id text - NO -",
		"column created_at timestamp with time zone - NO now()",
		"column attempts integer - NO 0",
		"column next_attempt_at timestamp with time zone - NO now()",
		"column last_error_class character varying 32 YES -",
		"constraint billing_erasure_outbox_pkey PRIMARY KEY (id)",
		"constraint billing_erasure_outbox_stripe_customer_id_key UNIQUE (stripe_customer_id)",
	} {
		assert.Contains(t, applied, line)
	}
	assert.NotEmpty(t, migrationSnapshotLine(applied, "constraint billing_erasure_outbox_attempts_check "))
	assert.Empty(t, migrationSnapshotLine(applied, "column user_id "), "an obligation must not keep a user id")
	for _, line := range applied {
		assert.NotContains(t, line, "FOREIGN KEY", "an obligation must outlive the user it came from")
	}

	_, err = tx.Exec(migration000182DownSQL(t))
	require.NoError(t, err, "the down must drop an empty outbox")
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "billing_erasure_outbox"))
	_, err = tx.Exec(migration000182DownSQL(t))
	require.NoError(t, err, "a second down must find the outbox gone and return without error")

	_, err = tx.Exec(migration000182UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "billing_erasure_outbox"))
}

func TestMigration000182_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000182DownSQL(t)

	t.Run("refuses while an obligation is pending", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		_, err = migration000182Enqueue(tx, "cus_test_"+uuid.NewString())
		require.NoError(t, err)

		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, "23001", "")
		assert.Contains(t, err.Error(), migration000182Refusal)
	})

	t.Run("checks for an obligation only after it holds the lock", func(t *testing.T) {
		migrationRequireGuardRunsUnderLock(t, db, down, "billing_erasure_outbox", migration000182Refusal,
			func(tx *sql.Tx) error {
				_, err := migration000182Enqueue(tx, "cus_test_"+uuid.NewString())
				return err
			})
	})

	var present bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('billing_erasure_outbox') IS NOT NULL`).Scan(&present))
	assert.True(t, present, "a refused or rolled-back down must leave the outbox in place")
}

func TestMigration000182_Constraints(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("one obligation per Customer; a repeat enqueue is a no-op", func(t *testing.T) {
		customerID := "cus_test_" + uuid.NewString()
		inserted, err := migration000182Enqueue(db, customerID)
		require.NoError(t, err)
		assert.EqualValues(t, 1, inserted)
		inserted, err = migration000182Enqueue(db, customerID)
		require.NoError(t, err)
		assert.EqualValues(t, 0, inserted, "ON CONFLICT DO NOTHING must absorb a second enqueue")

		_, err = db.Exec(`INSERT INTO billing_erasure_outbox (stripe_customer_id) VALUES ($1)`, customerID)
		migrationRequirePQError(t, err, "23505", "billing_erasure_outbox_stripe_customer_id_key")
	})

	t.Run("defaults make a new row due at once", func(t *testing.T) {
		customerID := "cus_test_" + uuid.NewString()
		_, err := migration000182Enqueue(db, customerID)
		require.NoError(t, err)
		var attempts int
		var due bool
		var lastErrorClass sql.NullString
		require.NoError(t, db.QueryRow(`
			SELECT attempts, next_attempt_at <= NOW(), last_error_class
			  FROM billing_erasure_outbox WHERE stripe_customer_id = $1`, customerID,
		).Scan(&attempts, &due, &lastErrorClass))
		assert.Zero(t, attempts)
		assert.True(t, due)
		assert.False(t, lastErrorClass.Valid)
	})

	t.Run("accepts a zero attempt count and a 32-character error class", func(t *testing.T) {
		_, err := db.Exec(`
			INSERT INTO billing_erasure_outbox (stripe_customer_id, attempts, last_error_class)
			VALUES ($1, 0, $2)`, "cus_test_"+uuid.NewString(), strings.Repeat("e", 32))
		require.NoError(t, err)
	})

	t.Run("rejects a negative attempt count", func(t *testing.T) {
		_, err := db.Exec(`
			INSERT INTO billing_erasure_outbox (stripe_customer_id, attempts)
			VALUES ($1, -1)`, "cus_test_"+uuid.NewString())
		migrationRequirePQError(t, err, "23514", "billing_erasure_outbox_attempts_check")
	})

	t.Run("rejects a missing Customer id", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO billing_erasure_outbox (attempts) VALUES (0)`)
		migrationRequirePQError(t, err, "23502", "")
	})
}
