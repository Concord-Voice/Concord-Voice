package database_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000146_LocksUsersAndFriendshipsBeforeChangingSchema(t *testing.T) {
	const lock = "LOCK TABLE public.users, public.friendships, public.dm_participants IN SHARE ROW EXCLUSIVE MODE;"

	for _, direction := range []string{"up", "down"} {
		t.Run(direction, func(t *testing.T) {
			filename := "../../migrations/000146_add_dm_block_reconciliation." + direction + ".sql"
			sql := strings.TrimSpace(migrationReadFile(t, filename))
			require.True(t, strings.HasPrefix(sql, lock), "migration must acquire the canonical cutoff lock first")
		})
	}
}

func TestMigration000146_BlockedPairAndCanonicalChecks(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	c := testhelpers.CreateUser(t, db)
	conversation := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO dm_conversations (id, created_by)
		VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, b, a)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, b)
	require.Error(t, err, "a blocked friendship pair must not enter a DM")
	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, c)
	require.NoError(t, err)
	_, err = db.Exec(`
		UPDATE dm_participants SET user_id = $1
		WHERE conversation_id = $2 AND user_id = $3`, b, conversation, c)
	require.Error(t, err, "an update must not move a participant into a blocked pair")

	// The pair rail is canonical and rejects both reversed and self pairs.
	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id)
		VALUES (GREATEST($1::uuid, $2::uuid), LEAST($1::uuid, $2::uuid), gen_random_uuid())`, a, b)
	requireConstraint(t, err, "dm_block_reconciliations_pair_order_check")
	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id)
		VALUES ($1, $1, gen_random_uuid())`, a)
	requireConstraint(t, err, "dm_block_reconciliations_pair_order_check")
}

func TestMigration000146_ConcurrentBlockedParticipantsSerialize(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	conversation := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO dm_conversations (id, created_by)
		VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, a, b)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	first, err := db.Begin()
	require.NoError(t, err)
	// Keep the connection's locks bounded even when a later assertion fails.
	// require.NoError calls FailNow, so a defer is the only reliable cleanup
	// path for this deliberately open transaction.
	defer func() { _ = first.Rollback() }()
	_, err = first.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)

	second, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = second.Rollback() }()
	_, err = second.Exec(`SET LOCAL lock_timeout = '100ms'`)
	require.NoError(t, err)
	_, err = second.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, b)
	require.Error(t, err, "the parent lock must serialize the opposing insert")
	require.NoError(t, second.Rollback())
	require.NoError(t, first.Commit())

	_, err = db.Exec(`
		INSERT INTO dm_participants (conversation_id, user_id)
		VALUES ($1, $2)`, conversation, b)
	require.Error(t, err, "the post-serialization fresh check must reject the pair")
}

func TestMigration000146_UncommittedFriendshipBlockRejectsParticipant(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	conversation := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, a)
	require.NoError(t, err)
	blockTx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = blockTx.Rollback() }()
	_, err = blockTx.Exec(`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = blockTx.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, a, b, uuid.New())
	require.NoError(t, err)
	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, conn.Close()) })
	var pid int
	require.NoError(t, conn.QueryRowContext(context.Background(), `SELECT pg_backend_pid()`).Scan(&pid))
	result := make(chan error, 1)
	go func() {
		_, insertErr := conn.ExecContext(context.Background(), `INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, b)
		result <- insertErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_locks WHERE pid = $1 AND NOT granted)`, pid).Scan(&waiting)
		return err == nil && waiting
	}, time.Second, 10*time.Millisecond, "participant insert must wait for the uncommitted friendship block")
	require.NoError(t, blockTx.Commit())
	insertErr := <-result
	var pqErr *pq.Error
	require.ErrorAs(t, insertErr, &pqErr)
	assert.Equal(t, "23514", string(pqErr.Code))
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversation, b).Scan(&count))
	assert.Zero(t, count)
}

func TestMigration000146_FreshInstallOwnsStrongFriendshipFence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	historicalA := testhelpers.CreateUser(t, db)
	historicalB := testhelpers.CreateUser(t, db)
	currentA := testhelpers.CreateUser(t, db)
	currentB := testhelpers.CreateUser(t, db)
	staleA := testhelpers.CreateUser(t, db)
	staleB := testhelpers.CreateUser(t, db)
	down := migrationReadFile(t, "../../migrations/000146_add_dm_block_reconciliation.down.sql")
	up := migrationReadFile(t, "../../migrations/000146_add_dm_block_reconciliation.up.sql")

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), down)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, historicalA, historicalB)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), up)
	require.NoError(t, err)
	require.NoError(t, tx.Commit(), "historical pre-000146 blocks are not directional evidence and remain valid")

	var historicalStatus string
	require.NoError(t, db.QueryRow(`SELECT status FROM friendships WHERE requester_id = $1 AND addressee_id = $2`, historicalA, historicalB).Scan(&historicalStatus))
	assert.Equal(t, "blocked", historicalStatus)

	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, currentA, currentB)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, currentA, currentB, uuid.New())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE, FALSE)`, staleA, staleB, uuid.New())
	require.NoError(t, err)
	tx, err = db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(t.Context(), `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'blocked')`, staleA, staleB)
	require.NoError(t, err)
	requireCheckViolation(t, tx.Commit())
}

func TestMigration000146_DownGuardRetainsEvidence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	a := testhelpers.CreateUser(t, db)
	b := testhelpers.CreateUser(t, db)
	d := testhelpers.CreateUser(t, db)
	e := testhelpers.CreateUser(t, db)
	f := testhelpers.CreateUser(t, db)
	operationID := uuid.NewString()
	_, err := db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE)`, a, b, operationID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), $3, TRUE)`, a, d, operationID)
	requireConstraint(t, err, "dm_block_reconciliations_operation_id_key")
	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, failure_class)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), gen_random_uuid(), TRUE, 'not_a_class')`, a, e)
	requireConstraint(t, err, "dm_block_reconciliations_failure_class_check")
	_, err = db.Exec(`
		INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id)
		VALUES (LEAST($1::uuid, $2::uuid), GREATEST($1::uuid, $2::uuid), gen_random_uuid())`, a, f)
	requireConstraint(t, err, "dm_block_reconciliations_removal_side_check")
	_, err = db.Exec(`UPDATE dm_block_reconciliations SET attempts = -1 WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, a, b)
	require.Error(t, err, "attempts cannot be negative")
	_, err = db.Exec(`UPDATE dm_block_reconciliations SET updated_at = created_at - interval '1 second' WHERE user_a_id = LEAST($1::uuid, $2::uuid) AND user_b_id = GREATEST($1::uuid, $2::uuid)`, a, b)
	require.Error(t, err, "updated_at cannot precede created_at")
	_, err = db.Exec(`
		UPDATE dm_block_reconciliations
		SET remove_a = remove_a OR FALSE, remove_b = remove_b OR TRUE
		WHERE user_a_id = LEAST($1::uuid, $2::uuid)
		  AND user_b_id = GREATEST($1::uuid, $2::uuid)`, a, b)
	require.NoError(t, err, "removal sides must support monotone OR updates")
	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, a)
	require.Error(t, err, "unresolved evidence must restrict user erasure")

	down := migrationReadFile(t, "../../migrations/000146_add_dm_block_reconciliation.down.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(down)
	require.Error(t, err, "rollback must not discard unresolved block evidence")
	_ = tx.Rollback()

	t.Run("drops cleanly after evidence drains", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		down153 := migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.down.sql")
		up153 := migrationReadFile(t, "../../migrations/000153_guard_blocked_friendship_reconciliation.up.sql")
		_, err = tx.Exec(`DELETE FROM dm_block_reconciliations`)
		require.NoError(t, err)
		_, err = tx.Exec(down153)
		require.NoError(t, err, "roll back dependent 000153 objects before replaying 000146")
		_, err = tx.Exec(down)
		require.NoError(t, err)
		var exists bool
		require.NoError(t, tx.QueryRow(`
			SELECT to_regclass('public.dm_block_reconciliations') IS NOT NULL`).Scan(&exists))
		require.False(t, exists)
		_, err = tx.Exec(down)
		require.NoError(t, err, "down migration must replay when its table is already absent")
		var triggerExists bool
		require.NoError(t, tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'reject_blocked_dm_participant')`).Scan(&triggerExists))
		require.False(t, triggerExists)
		require.NoError(t, tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'require_blocked_friendship_reconciliation')`).Scan(&triggerExists))
		require.False(t, triggerExists)
		var functionExists bool
		require.NoError(t, tx.QueryRow(`SELECT to_regprocedure('public.require_blocked_friendship_reconciliation()') IS NOT NULL`).Scan(&functionExists))
		require.False(t, functionExists)
		_, err = tx.Exec(migrationReadFile(t, "../../migrations/000146_add_dm_block_reconciliation.up.sql"))
		require.NoError(t, err, "migration must replay after clean rollback")
		require.NoError(t, tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'reject_blocked_dm_participant')`).Scan(&triggerExists))
		require.True(t, triggerExists)
		require.NoError(t, tx.QueryRow(`SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'require_blocked_friendship_reconciliation')`).Scan(&triggerExists))
		require.True(t, triggerExists)
		_, err = tx.Exec(up153)
		require.NoError(t, err, "latest dependent migration must replay after 000146")
	})
}

func requireConstraint(t *testing.T, err error, name string) {
	t.Helper()
	var pqErr *pq.Error
	require.ErrorAs(t, err, &pqErr)
	assert.Equal(t, name, pqErr.Constraint)
}
