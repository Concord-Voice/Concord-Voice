package database_test

import (
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000188 (epic #2033 PR-5): subscription_seats, Wing seats. Round
// trips and refusals run in rolled-back transactions, as in the 000186 tests.

const migration000188Refusal = "cannot drop subscription_seats while a Wing seat is pending or active"

func migration000188UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000188_subscription_seats.up.sql")
}

func migration000188DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000188_subscription_seats.down.sql")
}

type migration000188Fixture struct {
	sub, payer, holder uuid.UUID
}

// migration000188Parties creates a payer with a Wing row and a holder.
func migration000188Parties(t *testing.T, db *sql.DB) migration000188Fixture {
	t.Helper()
	f := migration000188Fixture{payer: testhelpers.CreateUser(t, db), holder: testhelpers.CreateUser(t, db)}
	require.NoError(t, db.QueryRow(`INSERT INTO subscriptions
		(user_id, tier, status, source, current_period_end, stripe_customer_id, stripe_subscription_id, plan, billing_interval)
		VALUES ($1, 'premium', 'active', 'stripe', $2, $3, $4, 'wing', 'year') RETURNING id`,
		f.payer, time.Now().Add(time.Hour), "cus_"+uuid.NewString(), "sub_"+uuid.NewString()).Scan(&f.sub))
	return f
}

func migration000188Seat(q migrationExecer, f migration000188Fixture, state string) error {
	_, err := q.Exec(`INSERT INTO subscription_seats
		(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at, activated_at, ended_at, end_reason)
		VALUES ($1, $2, $3, $4::text, NOW() + INTERVAL '7 days',
		        CASE WHEN $4::text IN ('active', 'left', 'revoked') THEN NOW() END,
		        CASE WHEN $4::text NOT IN ('pending', 'active') THEN NOW() END,
		        CASE WHEN $4::text NOT IN ('pending', 'active') THEN 'payer_revoked' END)`,
		f.sub, f.payer, f.holder, state)
	return err
}

func TestMigration000188_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	// 000193 widened end_reason after this file; step back over it so the
	// round trip compares 000188's own shape.
	_, err = tx.Exec(migration000193DownSQL(t))
	require.NoError(t, err)

	applied := migrationSchemaSnapshot(t, tx, "subscription_seats")
	for _, line := range []string{
		"column holder_user_id uuid - NO -",
		"column invite_expires_at timestamp with time zone - NO -",
	} {
		assert.Contains(t, applied, line)
	}
	assert.NotEmpty(t, migrationSnapshotLine(applied, "constraint subscription_seats_distinct_parties_check "))

	_, err = tx.Exec(migration000188DownSQL(t))
	require.NoError(t, err, "the down must drop a table with no open seat")
	assert.Empty(t, migrationSchemaSnapshot(t, tx, "subscription_seats"))
	_, err = tx.Exec(migration000188DownSQL(t))
	require.NoError(t, err, "a second down must find the table gone and return without error")

	_, err = tx.Exec(migration000188UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "subscription_seats"))
}

func TestMigration000188_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000188DownSQL(t)

	t.Run("terminal seats do not block", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000188Seat(tx, migration000188Parties(t, db), "revoked"))
		_, err = tx.Exec(down)
		require.NoError(t, err)
	})

	for _, state := range []string{"pending", "active"} {
		t.Run("refuses while a seat is "+state, func(t *testing.T) {
			tx, err := db.Begin()
			require.NoError(t, err)
			defer migrationRollback(t, tx)
			require.NoError(t, migration000188Seat(tx, migration000188Parties(t, db), state))
			_, err = tx.Exec(down)
			migrationRequirePQError(t, err, "23001", "")
			assert.Contains(t, err.Error(), migration000188Refusal)
		})
	}

	t.Run("checks for a seat only after it holds the lock", func(t *testing.T) {
		f := migration000188Parties(t, db)
		migrationRequireGuardRunsUnderLock(t, db, down, "subscription_seats", migration000188Refusal,
			func(tx *sql.Tx) error { return migration000188Seat(tx, f, "pending") })
	})
}

func TestMigration000188_Constraints(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	t.Run("one open seat per subscription", func(t *testing.T) {
		f := migration000188Parties(t, db)
		require.NoError(t, migration000188Seat(db, f, "pending"))
		f.holder = testhelpers.CreateUser(t, db)
		migrationRequirePQError(t, migration000188Seat(db, f, "active"), "23505", "idx_subscription_seats_open_per_subscription")
	})

	t.Run("one active seat per holder", func(t *testing.T) {
		a, b := migration000188Parties(t, db), migration000188Parties(t, db)
		b.holder = a.holder
		require.NoError(t, migration000188Seat(db, a, "active"))
		migrationRequirePQError(t, migration000188Seat(db, b, "active"), "23505", "idx_subscription_seats_active_holder")
	})

	t.Run("payer and holder differ", func(t *testing.T) {
		f := migration000188Parties(t, db)
		f.holder = f.payer
		migrationRequirePQError(t, migration000188Seat(db, f, "pending"), "23514", "subscription_seats_distinct_parties_check")
	})

	t.Run("an active seat needs an activation", func(t *testing.T) {
		f := migration000188Parties(t, db)
		_, err := db.Exec(`INSERT INTO subscription_seats
			(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at)
			VALUES ($1, $2, $3, 'active', NOW())`, f.sub, f.payer, f.holder)
		migrationRequirePQError(t, err, "23514", "subscription_seats_active_check")
	})

	t.Run("erasing the holder deletes the seat", func(t *testing.T) {
		f := migration000188Parties(t, db)
		require.NoError(t, migration000188Seat(db, f, "active"))
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, f.holder)
		require.NoError(t, err)
		var n int
		require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM subscription_seats WHERE subscription_id = $1`, f.sub).Scan(&n))
		assert.Zero(t, n)
	})
}
