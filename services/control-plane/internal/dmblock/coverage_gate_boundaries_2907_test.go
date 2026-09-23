package dmblock

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestCoverageGate_BoundariesFailClosed(t *testing.T) {
	ctx := context.Background()
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)

	t.Run("lock rejects missing subject row", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.ErrorIs(t, lockSubjectsModeTx(ctx, tx, []uuid.UUID{a, uuid.New()}, LockUpdate), ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})

	t.Run("prepare rejects missing conversation", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = PrepareConversationTx(ctx, tx, uuid.NewString(), []uuid.UUID{a}, LockShare, LockShare)
		require.ErrorIs(t, err, ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})

	t.Run("worker rejects malformed operation and self pair", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.ErrorIs(t, RecordBlockTx(ctx, tx, "bad", b.String(), uuid.NewString()), ErrUnavailable)
		require.ErrorIs(t, RecordBlockTx(ctx, tx, a.String(), a.String(), uuid.NewString()), ErrUnavailable)
		require.ErrorIs(t, RecordBlockTx(ctx, tx, a.String(), b.String(), "bad"), ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})
}

func TestCoverageGate_PrepareConversationAndWorkerValidSnapshots(t *testing.T) {
	ctx := context.Background()
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b, extra := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversation, operation := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
	require.NoError(t, err)

	t.Run("prepare returns stable subject snapshot", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		got, err := PrepareConversationTx(ctx, tx, conversation.String(), []uuid.UUID{extra}, LockNoKeyUpdate, LockShare)
		require.NoError(t, err)
		require.ElementsMatch(t, []uuid.UUID{a, b}, got)
		require.NoError(t, tx.Commit())
	})
	t.Run("split preparation preserves stable subject snapshot", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		subjects, err := LockConversationUsersTx(ctx, tx, conversation.String(), []uuid.UUID{extra}, LockShare)
		require.NoError(t, err)
		got, err := PrepareConversationAfterUserLocksTx(ctx, tx, conversation.String(), subjects, LockShare)
		require.NoError(t, err)
		require.ElementsMatch(t, []uuid.UUID{a, b}, got)
		require.NoError(t, tx.Commit())
	})

	removeA := a.String() < b.String()
	low, high := minUUIDValue(a, b), maxUUIDValue(a, b)
	_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b) VALUES ($1, $2, $3, $4, $5)`, low, high, operation, removeA, !removeA)
	require.NoError(t, err)
	guardConversation := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, guardConversation, extra)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, guardConversation, extra)
	require.NoError(t, err)
	t.Run("resolved guard accepts unblocked conversation", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.NoError(t, ValidateConversationResolvedTx(ctx, tx, guardConversation.String()))
		require.NoError(t, tx.Commit())
	})
	t.Run("worker validates obligation and returns snapshot", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() { _ = tx.Rollback() })
		got, err := prepareWorkerConversationTx(ctx, tx, conversation.String(), claimedObligation{
			a:           low,
			b:           high,
			removeA:     removeA,
			removeB:     !removeA,
			operationID: operation,
		})
		require.NoError(t, err)
		require.ElementsMatch(t, []uuid.UUID{a, b}, got)
		require.NoError(t, tx.Commit())
	})

}

func TestCoverageGate_ResolutionAndMembershipFences(t *testing.T) {
	ctx := context.Background()
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b, extra := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
	require.NoError(t, err)

	t.Run("resolution preparation locks and snapshots complete membership", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		subjects, err := PrepareResolutionConversationTx(ctx, tx, conversation.String(), []uuid.UUID{extra}, LockNoKeyUpdate, LockUpdate)
		require.NoError(t, err)
		require.ElementsMatch(t, []uuid.UUID{a, b}, subjects)
		require.NoError(t, tx.Rollback())
	})

	t.Run("invalid parent mode fails closed", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = PrepareResolutionConversationTx(ctx, tx, conversation.String(), nil, LockShare, LockMode(99))
		require.ErrorIs(t, err, ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})

	t.Run("invalid user mode fails closed before parent access", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = PrepareResolutionConversationTx(ctx, tx, conversation.String(), nil, LockMode(99), LockShare)
		require.ErrorIs(t, err, ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})

	t.Run("missing conversation fails closed after user locks", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		_, err = PrepareConversationAfterUserLocksTx(ctx, tx, uuid.NewString(), []uuid.UUID{a, b}, LockShare)
		require.ErrorIs(t, err, ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})

	t.Run("membership drift requires a complete transaction restart", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		expected, err := LockConversationUsersTx(ctx, tx, conversation.String(), nil, LockShare)
		require.NoError(t, err)

		newMember := dbtest.CreateUser(t, db)
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, newMember)
		require.NoError(t, err)
		_, err = PrepareConversationAfterUserLocksTx(ctx, tx, conversation.String(), expected, LockShare)
		require.ErrorIs(t, err, ErrMembershipChanged)
		require.NoError(t, tx.Rollback())
	})
}

func TestCoverageGate_PrepareRejectsOversizedConversation(t *testing.T) {
	ctx := context.Background()
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	conversation, owner := uuid.New(), dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, owner)
	require.NoError(t, err)
	for i := 0; i < maxDMSubjects+1; i++ {
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, dbtest.CreateUser(t, db))
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = PrepareConversationTx(ctx, tx, conversation.String(), nil, LockShare, LockShare)
	require.ErrorIs(t, err, ErrUnavailable)
	require.NoError(t, tx.Rollback())
}

func TestCoverageGate_PrepareAcceptsLegacyElevenMemberConversation(t *testing.T) {
	ctx := context.Background()
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	conversation, owner := uuid.New(), dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, owner)
	require.NoError(t, err)
	for i := 0; i < 10; i++ {
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, dbtest.CreateUser(t, db))
		require.NoError(t, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = PrepareConversationTx(ctx, tx, conversation.String(), nil, LockShare, LockShare)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
}
