package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000193 (follow-up to #4133): subscription_seats.end_reason admits
// 'pair_ineligible'. Round trips and refusals run in rolled-back transactions,
// as in the 000188 tests.

const migration000193Refusal = "cannot remove end_reason pair_ineligible while a seat carries it"

func migration000193UpSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000193_seat_end_reason_pair_ineligible.up.sql")
}

func migration000193DownSQL(t *testing.T) string {
	t.Helper()
	return migrationReadFile(t, "../../migrations/000193_seat_end_reason_pair_ineligible.down.sql")
}

func migration000193EndedSeat(q migrationExecer, f migration000188Fixture, reason string) error {
	_, err := q.Exec(`INSERT INTO subscription_seats
		(subscription_id, payer_user_id, holder_user_id, state, invite_expires_at, ended_at, end_reason)
		VALUES ($1, $2, $3, 'revoked', NOW() + INTERVAL '7 days', NOW(), $4)`,
		f.sub, f.payer, f.holder, reason)
	return err
}

func TestMigration000193_UpDownUpRoundTrip(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.Begin()
	require.NoError(t, err)
	defer migrationRollback(t, tx)

	applied := migrationSchemaSnapshot(t, tx, "subscription_seats")
	assert.Contains(t, migrationSnapshotLine(applied, "constraint subscription_seats_end_reason_check "), "pair_ineligible")

	_, err = tx.Exec(migration000193DownSQL(t))
	require.NoError(t, err, "the down must run with no pair_ineligible seat")
	down := migrationSnapshotLine(migrationSchemaSnapshot(t, tx, "subscription_seats"), "constraint subscription_seats_end_reason_check ")
	assert.NotContains(t, down, "pair_ineligible")
	assert.Contains(t, down, "invite_expired", "the down restores the 000188 list, not an empty one")

	_, err = tx.Exec(migration000193UpSQL(t))
	require.NoError(t, err, "the up must re-apply after its down")
	assert.Equal(t, applied, migrationSchemaSnapshot(t, tx, "subscription_seats"))
}

func TestMigration000193_AdmitsOnlyTheNewReason(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	require.NoError(t, migration000193EndedSeat(db, migration000188Parties(t, db), "pair_ineligible"))
	migrationRequirePQError(t, migration000193EndedSeat(db, migration000188Parties(t, db), "unfriended"),
		"23514", "subscription_seats_end_reason_check")
}

func TestMigration000193_DownRefusesUnderLock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	down := migration000193DownSQL(t)

	t.Run("other reasons do not block", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000193EndedSeat(tx, migration000188Parties(t, db), "payer_revoked"))
		_, err = tx.Exec(down)
		require.NoError(t, err)
	})

	t.Run("refuses while a seat carries pair_ineligible", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer migrationRollback(t, tx)
		require.NoError(t, migration000193EndedSeat(tx, migration000188Parties(t, db), "pair_ineligible"))
		_, err = tx.Exec(down)
		migrationRequirePQError(t, err, "23001", "")
		assert.Contains(t, err.Error(), migration000193Refusal)
	})

	t.Run("checks only after it holds the lock", func(t *testing.T) {
		f := migration000188Parties(t, db)
		migrationRequireGuardRunsUnderLock(t, db, down, "subscription_seats", migration000193Refusal,
			func(tx *sql.Tx) error { return migration000193EndedSeat(tx, f, "pair_ineligible") })
	})
}
