package dmblock

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestReconcileDue_NotifiesCommittedParticipantRemovals(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	notifier := &recordingReconciliationNotifier{db: db}
	reconciler := New(db, nil)
	reconciler.SetReconciliationNotifier(notifier)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.ElementsMatch(t, []uuid.UUID{a, b}, notifier.participantRemovals)
	require.Empty(t, notifier.groupDeletes)
	require.Equal(t, []recordedRoleChange{{conversationID: conversation.String(), userID: survivor}}, notifier.roleChanges)
	require.Empty(t, notifier.keyRevocations)
	require.False(t, notifier.participantCallbacksBeforeCommit)
	require.False(t, notifier.roleCallbacksBeforeCommit)
}

func TestReconcileDue_EmitsCommittedKeyRevocationCueWithoutPrematureLedger(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	_, err := db.Exec(`UPDATE dm_conversations SET created_by = $1 WHERE id = $2`, survivor, conversation)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_channel_keys (conversation_id, user_id, wrapped_key, key_version) VALUES ($1, $2, $3, 1)`, conversation, a, "key")
	require.NoError(t, err)
	notifier := &recordingReconciliationNotifier{db: db}
	reconciler := New(db, nil)
	reconciler.SetReconciliationNotifier(notifier)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Empty(t, notifier.roleChanges)
	require.Equal(t, []recordedKeyRevocation{{
		conversationID: conversation.String(), revokedEpoch: 1, reason: "user_blocked",
	}}, notifier.keyRevocations)
	require.False(t, notifier.keyRevocationCallbacksBeforeCommit)
	var revocationCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_key_revocations WHERE conversation_id = $1`, conversation).Scan(&revocationCount))
	require.Zero(t, revocationCount, "the cue must not revoke an epoch before a successor wrap exists")
}

func TestReconcileDue_NotifiesCommittedGroupDeletion(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, _ := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), uuid.Nil)
	conversation, operation := uuid.New(), uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversation, a, b)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b) VALUES ($1, $2, $3, true, true)`, a, b, operation)
	require.NoError(t, err)
	notifier := &recordingReconciliationNotifier{db: db}
	reconciler := New(db, nil)
	reconciler.SetPurgeEngine(emptyConversationAttachmentRetirer{})
	reconciler.SetReconciliationNotifier(notifier)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.Empty(t, notifier.participantRemovals)
	require.Len(t, notifier.groupDeletes, 1)
	require.Equal(t, conversation.String(), notifier.groupDeletes[0].conversationID)
	require.ElementsMatch(t, []uuid.UUID{a, b}, notifier.groupDeletes[0].userIDs)
	require.False(t, notifier.groupCallbacksBeforeCommit)
}

func TestReconcileDue_NotificationFailureDoesNotUndoCommittedRemoval(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	conversation, operation := uuid.New(), uuid.New()
	insertReconcileConversationFixture(t, db, conversation, a, b, survivor, operation)
	notifier := &recordingReconciliationNotifier{db: db, participantErr: errors.New("websocket unavailable")}
	reconciler := New(db, nil)
	reconciler.SetReconciliationNotifier(notifier)

	count, err := reconciler.ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	require.ElementsMatch(t, []uuid.UUID{a, b}, notifier.participantRemovals)
	var participants, markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1`, conversation).Scan(&participants))
	require.Equal(t, 1, participants)
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE operation_id = $1`, operation).Scan(&markers))
	require.Zero(t, markers, "delivery failure must not restore or retry the committed topology")
}

type recordedGroupDeletion struct {
	conversationID string
	userIDs        []uuid.UUID
}

type recordedRoleChange struct {
	conversationID string
	userID         uuid.UUID
}

type recordedKeyRevocation struct {
	conversationID string
	revokedEpoch   int
	reason         string
}

type recordingReconciliationNotifier struct {
	db                                 *sql.DB
	participantErr                     error
	groupErr                           error
	participantRemovals                []uuid.UUID
	groupDeletes                       []recordedGroupDeletion
	roleChanges                        []recordedRoleChange
	keyRevocations                     []recordedKeyRevocation
	participantCallbacksBeforeCommit   bool
	groupCallbacksBeforeCommit         bool
	roleCallbacksBeforeCommit          bool
	keyRevocationCallbacksBeforeCommit bool
}

func (n *recordingReconciliationNotifier) ParticipantRemoved(ctx context.Context, conversationID string, userID uuid.UUID) error {
	var count int
	if err := n.db.QueryRowContext(ctx, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID).Scan(&count); err != nil {
		return err
	}
	n.participantCallbacksBeforeCommit = n.participantCallbacksBeforeCommit || count != 0
	n.participantRemovals = append(n.participantRemovals, userID)
	return n.participantErr
}

func (n *recordingReconciliationNotifier) GroupDeleted(ctx context.Context, conversationID string, userIDs []uuid.UUID) error {
	var count int
	if err := n.db.QueryRowContext(ctx, `SELECT count(*) FROM dm_conversations WHERE id = $1`, conversationID).Scan(&count); err != nil {
		return err
	}
	n.groupCallbacksBeforeCommit = n.groupCallbacksBeforeCommit || count != 0
	n.groupDeletes = append(n.groupDeletes, recordedGroupDeletion{
		conversationID: conversationID,
		userIDs:        append([]uuid.UUID(nil), userIDs...),
	})
	return n.groupErr
}

func (n *recordingReconciliationNotifier) RoleChanged(ctx context.Context, conversationID string, userID uuid.UUID) error {
	var createdBy uuid.UUID
	if err := n.db.QueryRowContext(ctx, `SELECT created_by FROM dm_conversations WHERE id = $1`, conversationID).Scan(&createdBy); err != nil {
		return err
	}
	n.roleCallbacksBeforeCommit = n.roleCallbacksBeforeCommit || createdBy != userID
	n.roleChanges = append(n.roleChanges, recordedRoleChange{conversationID: conversationID, userID: userID})
	return nil
}

func (n *recordingReconciliationNotifier) KeyRevocation(ctx context.Context, conversationID string, revokedEpoch int, reason string) error {
	var participantCount int
	if err := n.db.QueryRowContext(ctx, `SELECT count(*) FROM dm_participants WHERE conversation_id = $1`, conversationID).Scan(&participantCount); err != nil {
		return err
	}
	n.keyRevocationCallbacksBeforeCommit = n.keyRevocationCallbacksBeforeCommit || participantCount != 1
	n.keyRevocations = append(n.keyRevocations, recordedKeyRevocation{
		conversationID: conversationID,
		revokedEpoch:   revokedEpoch,
		reason:         reason,
	})
	return nil
}
