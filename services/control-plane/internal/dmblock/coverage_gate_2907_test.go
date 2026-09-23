package dmblock

import (
	"context"
	"database/sql"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestCoverageGate_ValidationLeavesDMBlockHelpersFailClosed(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, lockSubjectsModeTx(ctx, nil, []uuid.UUID{uuid.New()}, LockShare), ErrUnavailable)
	require.ErrorIs(t, PrepareNewConversationTx(ctx, nil, []uuid.UUID{uuid.New()}, LockShare), ErrUnavailable)
	require.ErrorIs(t, ValidateConversationResolvedTx(ctx, nil, uuid.NewString()), ErrUnavailable)
	_, err := prepareWorkerConversationTx(ctx, nil, uuid.NewString(), claimedObligation{
		a:           uuid.New(),
		b:           uuid.New(),
		removeA:     true,
		operationID: uuid.New(),
	})
	require.ErrorIs(t, err, ErrUnavailable)
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	for name, subjects := range map[string][]uuid.UUID{
		"empty subjects":    nil,
		"nil subject":       {uuid.Nil},
		"too many subjects": make([]uuid.UUID, maxDMSubjects+1),
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := db.BeginTx(ctx, nil)
			require.NoError(t, err)
			require.ErrorIs(t, lockSubjectsModeTx(ctx, tx, subjects, LockShare), ErrUnavailable)
			require.NoError(t, tx.Rollback())
		})
	}
	t.Run("unknown lock mode", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.ErrorIs(t, lockSubjectsModeTx(ctx, tx, []uuid.UUID{uuid.New()}, LockMode(99)), ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})
	t.Run("empty guard subjects", func(t *testing.T) {
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		require.ErrorIs(t, guardBlockedSubjectsTx(ctx, tx, nil), ErrUnavailable)
		require.NoError(t, tx.Rollback())
	})
}

func TestCoverageGate_GuardSubjectsRejectsBlockedPair(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'blocked')`, a, b)
	require.NoError(t, err)
	require.NoError(t, RecordBlockTx(context.Background(), tx, a.String(), b.String(), uuid.NewString()))
	require.NoError(t, tx.Commit())
	tx, err = db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.ErrorIs(t, guardBlockedSubjectsTx(context.Background(), tx, []uuid.UUID{a, b}), ErrUnavailable)
	require.NoError(t, tx.Rollback())
}

func TestCoverageGate_PrepareNewConversationAcceptsUnblockedSubjects(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, PrepareNewConversationTx(context.Background(), tx, []uuid.UUID{b, a, a}, LockShare))
	require.NoError(t, tx.Commit())
}

func TestCoverageGate_DeleteEmptyConversationRemovesChildren(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
	require.NoError(t, err)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = (&Reconciler{attachmentRetirer: emptyConversationAttachmentRetirer{}}).deleteEmptyConversationTx(context.Background(), tx, conversation.String())
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	var count int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_conversations WHERE id = $1`, conversation).Scan(&count))
	require.Zero(t, count)
}

type emptyConversationAttachmentRetirer struct{}

func (emptyConversationAttachmentRetirer) CaptureConversationBlobsTx(
	context.Context, *sql.Tx, string,
) ([]string, []AttachmentBlobRef, error) {
	return nil, nil, nil
}

func (emptyConversationAttachmentRetirer) EnqueueBlobDeletes([]AttachmentBlobRef) {}

func TestCoverageGate_PureHelpersHandleBothDirections(t *testing.T) {
	a, b := uuid.New(), uuid.New()
	require.False(t, sameSubjects([]uuid.UUID{a}, []uuid.UUID{b}))
	require.False(t, sameSubjects([]uuid.UUID{a}, nil))
	require.True(t, sameSubjects([]uuid.UUID{a, b}, []uuid.UUID{a, b}))
	require.True(t, containsString([]string{"a", "b"}, "b"))
	require.False(t, containsString([]string{"a"}, "b"))
}
