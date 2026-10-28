package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000186 (epic #2033 D1): billing_credit_outbox, code credit the
// redemption committed and Stripe has not yet applied. Round trips and
// refusals run in rolled-back transactions, as in the 000182 tests.

const migration000186Refusal = "cannot drop billing_credit_outbox while code credits are pending"

func migration000186UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000186_billing_credit_outbox.up.sql")
}

func migration000186DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000186_billing_credit_outbox.down.sql")
}

type migration000186Querier interface {
	migrationExecer
	QueryRow(query string, args ...any) *sql.Row
}

// migration000186Redemption inserts a user, a code and its ledger row, and
// returns the ledger row's id.
func migration000186Redemption(t *testing.T, q migration000186Querier, db *sql.DB) uuid.UUID {
	t.Helper()
	user := testhelpers.CreateUser(t, db)
	var codeID, redemptionID uuid.UUID
	require.NoError(t, q.QueryRow(`INSERT INTO redemption_codes (code_hash, grant_kind) VALUES ($1, 'premium:subscription') RETURNING id`,
		uuid.NewString()).Scan(&codeID))
	require.NoError(t, q.QueryRow(`INSERT INTO code_redemptions (code_id, user_id) VALUES ($1, $2) RETURNING id`,
		codeID, user).Scan(&redemptionID))
	return redemptionID
}

func migration000186Enqueue(q migrationExecer, redemptionID uuid.UUID, amount int64) error {
	_, err := q.Exec(`
		INSERT INTO billing_credit_outbox (redemption_id, stripe_customer_id, amount, currency, description)
		VALUES ($1, 'cus_test', $2, 'usd', 'credit')`, redemptionID, amount)
	return err
}

func TestMigration000186_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "billing_credit_outbox")
	for _, line := range []string{
		"column redemption_id uuid - NO -",
		"column amount bigint - NO -",
		"column next_attempt_at timestamp with time zone - NO now()",
		"constraint billing_credit_outbox_redemption_id_key UNIQUE (redemption_id)",
	} {
		assert.Contains(t, applied, line)
	}
	assert.NotEmpty(t, migrationSnapshotLine(applied, "constraint billing_credit_outbox_amount_check "))

	_, err = tx.Exec(migration000186DownSQL(t))
	require.NoError(t, err, "the down must drop an empty outbox")
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "billing_credit_outbox"))
	_, err = tx.Exec(migration000186DownSQL(t))
	require.NoError(t, err, "a second down must find the outbox gone and return without error")

	_, err = tx.Exec(migration000186UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "billing_credit_outbox"))
}

func TestMigration000186_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000186DownSQL(t)

	t.Run("refuses while a credit is pending", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000186Enqueue(tx, migration000186Redemption(t, tx, db), 2697))

		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, "23001", "")
		assert.Contains(t, err.Error(), migration000186Refusal)
	})

	t.Run("checks for a credit only after it holds the lock", func(t *testing.T) {
		redemptionID := migration000186Redemption(t, db, db)
		migrationRequireGuardRunsUnderLock(t, db, down, "billing_credit_outbox", migration000186Refusal,
			func(tx *sql.Tx) error { return migration000186Enqueue(tx, redemptionID, 2697) })
	})
}

func TestMigration000186_Constraints(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("rejects a non-positive amount", func(t *testing.T) {
		err := migration000186Enqueue(db, migration000186Redemption(t, db, db), 0)
		migrationRequirePQError(t, err, "23514", "billing_credit_outbox_amount_check")
	})

	t.Run("one credit per redemption", func(t *testing.T) {
		redemptionID := migration000186Redemption(t, db, db)
		require.NoError(t, migration000186Enqueue(db, redemptionID, 2697))
		err := migration000186Enqueue(db, redemptionID, 2697)
		migrationRequirePQError(t, err, "23505", "billing_credit_outbox_redemption_id_key")
	})

	t.Run("deleting the ledger row drops its pending credit", func(t *testing.T) {
		redemptionID := migration000186Redemption(t, db, db)
		require.NoError(t, migration000186Enqueue(db, redemptionID, 2697))
		_, err := db.Exec(`DELETE FROM code_redemptions WHERE id = $1`, redemptionID)
		require.NoError(t, err)
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM billing_credit_outbox WHERE redemption_id = $1`, redemptionID).Scan(&n))
		assert.Zero(t, n)
	})
}
