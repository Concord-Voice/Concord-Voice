package database_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000153_DirectBlockedFriendshipInsertIsRejected(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, a, b)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())

	var friendships, markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&friendships))
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, a, b).Scan(&markers))
	assert.Zero(t, friendships)
	assert.Zero(t, markers)
}

func TestMigration000153_DirectAcceptedToBlockedUpdateIsRejected(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&status))
	assert.Equal(t, "accepted", status)
}

func TestMigration000153_BlockedFriendshipIDChangeWithoutMarkerIsRejected(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	var originalID uuid.UUID
	require.NoError(t, db.QueryRow(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'accepted')
		RETURNING id`, a, b).Scan(&originalID))
	changedID := uuid.New()

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		UPDATE friendships
		SET status = 'blocked'
		WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		UPDATE friendships
		SET id = $3
		WHERE requester_id = $1 AND addressee_id = $2`, a, b, changedID)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())

	var finalID uuid.UUID
	var status string
	require.NoError(t, db.QueryRow(`SELECT id, status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&finalID, &status))
	assert.Equal(t, originalID, finalID)
	assert.Equal(t, "accepted", status)
}

func TestMigration000153_BlockedFriendshipIDChangeWithCurrentMarkerIsAccepted(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	changedID := uuid.New()

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET id = $3 WHERE requester_id = $1 AND addressee_id = $2`, a, b, changedID)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var finalID uuid.UUID
	var status string
	require.NoError(t, db.QueryRow(`SELECT id, status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&finalID, &status))
	assert.Equal(t, changedID, finalID)
	assert.Equal(t, "blocked", status)
}

func TestMigration000153_CurrentWriterMarkerPreservesDirectionalAuthority(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	operationID := uuid.New()
	removeA := a.String() < b.String()

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, $4, $5)
		ON CONFLICT (user_a_id, user_b_id) DO UPDATE
		SET remove_a = EXCLUDED.remove_a, remove_b = EXCLUDED.remove_b, operation_id = EXCLUDED.operation_id`, a, b, operationID, removeA, !removeA)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	var gotA, gotB bool
	var gotOperation string
	require.NoError(t, db.QueryRow(`SELECT remove_a, remove_b, operation_id FROM dm_block_reconciliations WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, a, b).Scan(&gotA, &gotB, &gotOperation))
	assert.Equal(t, removeA, gotA)
	assert.Equal(t, !removeA, gotB)
	assert.Equal(t, operationID.String(), gotOperation)
}

func TestMigration000153_CurrentWriterMarkerInsideSavepointIsAccepted(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SAVEPOINT reconciliation_marker`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `RELEASE SAVEPOINT reconciliation_marker`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
}

func TestMigration000153_RolledBackMarkerDoesNotSatisfyBlockedFriendship(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SAVEPOINT reconciliation_marker`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `ROLLBACK TO SAVEPOINT reconciliation_marker`)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())
}

func TestMigration000153_BlockedToBlockedCannotReuseStaleMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	operationID := uuid.New()
	removeA := a.String() < b.String()

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b) VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, $4, $5)`, a, b, operationID, removeA, !removeA)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&status))
	assert.Equal(t, "blocked", status)
}

func TestMigration000153_BlockThenUnblockInOneTransactionIsSafe(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'accepted' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(), "a transaction whose final friendship state is safe needs no marker")
}

func TestMigration000153_BlockThenDeleteInOneTransactionIsSafe(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'blocked')`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(), "a transaction whose final friendship row is absent needs no marker")
}

func TestMigration000153_DifferentPairMarkerDoesNotSatisfyBlockedPair(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	c := testhelpers.CreateUser(t, db)
	d := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'accepted'), ($3, $4, 'accepted')`, a, b, c, d)
	require.NoError(t, err)

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), gen_random_uuid(), TRUE, FALSE)`, c, d)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, a, b).Scan(&status))
	assert.Equal(t, "accepted", status, "the blocked friendship transaction must roll back")
}

func TestMigration000153_CleanOld130UpgradeRestoresStrongFence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	up := migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.up.sql")
	_, err = tx.ExecContext(t.Context(), `DROP TRIGGER require_blocked_friendship_reconciliation ON public.friendships`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.require_blocked_friendship_reconciliation()`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP TRIGGER stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.stamp_dm_block_reconciliation_transaction()`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `ALTER TABLE public.dm_block_reconciliations DROP COLUMN reconciliation_transaction_id`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), up)
	require.NoError(t, err)
	var hasXID8 bool
	require.NoError(t, tx.QueryRowContext(t.Context(), `
		SELECT a.atttypid = 'xid8'::regtype
		FROM pg_catalog.pg_attribute a
		JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
		JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = 'public'
		  AND c.relname = 'dm_block_reconciliations'
		  AND a.attname = 'reconciliation_transaction_id'
		  AND NOT a.attisdropped`).Scan(&hasXID8))
	assert.True(t, hasXID8)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, testhelpers.CreateUser(t, db), testhelpers.CreateUser(t, db))
	require.NoError(t, err)
	// The clean upgrade has no blocked rows before the retrofit. The inserted row
	// must still prove that the restored trigger is active.
	requireCheckViolation(t, tx.Commit())
}

func TestMigration000153_Old130BlockedStateFailsClosed(t *testing.T) {
	for _, markerPresent := range []bool{false, true} {
		name := "without marker"
		if markerPresent {
			name = "with marker"
		}
		t.Run(name, func(t *testing.T) {
			db, cleanup := testhelpers.SetupTestDB(t)
			defer cleanup()
			a := testhelpers.CreateUser(t, db)
			b := testhelpers.CreateUser(t, db)
			tx, err := db.BeginTx(t.Context(), nil)
			require.NoError(t, err)
			defer func() { _ = tx.Rollback() }()
			_, err = tx.ExecContext(t.Context(), `DROP TRIGGER require_blocked_friendship_reconciliation ON public.friendships`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.require_blocked_friendship_reconciliation()`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `DROP TRIGGER stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.stamp_dm_block_reconciliation_transaction()`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `ALTER TABLE public.dm_block_reconciliations DROP COLUMN reconciliation_transaction_id`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), `
				INSERT INTO friendships (requester_id, addressee_id, status)
				VALUES ($1, $2, 'blocked')`, a, b)
			require.NoError(t, err)
			if markerPresent {
				_, err = tx.ExecContext(t.Context(), `
					INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
					VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
				require.NoError(t, err)
			}
			_, err = tx.ExecContext(t.Context(), `SAVEPOINT before_137`)
			require.NoError(t, err)
			_, err = tx.ExecContext(t.Context(), migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.up.sql"))
			requireCheckViolation(t, err)
			_, err = tx.ExecContext(t.Context(), `ROLLBACK TO SAVEPOINT before_137`)
			require.NoError(t, err)

			var blockedCount int
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT count(*) FROM friendships WHERE requester_id = $1 AND addressee_id = $2 AND status = 'blocked'`, a, b).Scan(&blockedCount))
			assert.Equal(t, 1, blockedCount)
			var stampColumnExists, guardTriggerExists, stampTriggerExists bool
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_catalog.pg_attribute a JOIN pg_catalog.pg_class c ON c.oid = a.attrelid JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE n.nspname = 'public' AND c.relname = 'dm_block_reconciliations' AND a.attname = 'reconciliation_transaction_id' AND NOT a.attisdropped)`).Scan(&stampColumnExists))
			assert.False(t, stampColumnExists)
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'require_blocked_friendship_reconciliation')`).Scan(&guardTriggerExists))
			assert.False(t, guardTriggerExists)
			require.NoError(t, tx.QueryRowContext(t.Context(), `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'stamp_dm_block_reconciliation_transaction')`).Scan(&stampTriggerExists))
			assert.False(t, stampTriggerExists)
		})
	}
}

func TestMigration000153_IncompatiblePreexistingStampColumnIsRejected(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP TRIGGER require_blocked_friendship_reconciliation ON public.friendships`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.require_blocked_friendship_reconciliation()`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP TRIGGER stamp_dm_block_reconciliation_transaction ON public.dm_block_reconciliations`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `DROP FUNCTION public.stamp_dm_block_reconciliation_transaction()`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `ALTER TABLE public.dm_block_reconciliations DROP COLUMN reconciliation_transaction_id`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `ALTER TABLE public.dm_block_reconciliations ADD COLUMN reconciliation_transaction_id text`)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.up.sql"))
	requireCheckViolation(t, err)
	require.NoError(t, tx.Rollback())
}

func TestMigration000153_DownRetainsStrongFence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.down.sql"))
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `SET CONSTRAINTS require_blocked_friendship_reconciliation IMMEDIATE`)
	requireCheckViolation(t, err)
	_ = tx.Rollback()
}

func requireCheckViolation(t *testing.T, err error) {
	t.Helper()
	var pqErr *pq.Error
	require.ErrorAs(t, err, &pqErr)
	assert.Equal(t, "23514", string(pqErr.Code))
}
