package dmblock

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

func TestPreparationLeavesFailClosedWithoutTransaction(t *testing.T) {
	ctx := context.Background()
	require.ErrorIs(t, PrepareNewConversationTx(ctx, nil, []uuid.UUID{uuid.New()}, LockShare), ErrUnavailable)
	require.ErrorIs(t, RecordBlockTx(ctx, nil, uuid.NewString(), uuid.NewString(), uuid.NewString()), ErrUnavailable)
}

func TestReconcileDueRequiresUsableWorker(t *testing.T) {
	var reconciler *Reconciler
	_, err := reconciler.ReconcileDue(context.Background(), 1)
	require.ErrorIs(t, err, ErrUnavailable)
}

// This hits PostgreSQL rather than asserting a query string: it would have
// caught the malformed "FOR FOR NO KEY UPDATE" clause that silently left the
// blocking transition rail unusable.
func TestSubjectLockModesUseValidPostgresClauses(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	subject := dbtest.CreateUser(t, db)
	for _, mode := range []LockMode{LockShare, LockNoKeyUpdate, LockUpdate} {
		t.Run(modeName(mode), func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			require.NoError(t, lockSubjectsModeTx(context.Background(), tx, []uuid.UUID{subject}, mode))
			require.NoError(t, tx.Commit())
		})
	}
}

func TestRecordBlockPreservesOneWayAndMutualRemovalEvidence(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	first := dbtest.CreateUser(t, db)
	second := dbtest.CreateUser(t, db)
	low, high := first, second
	if low.String() > high.String() {
		low, high = high, low
	}

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, RecordBlockTx(context.Background(), tx, low.String(), high.String(), uuid.NewString()))
	require.NoError(t, tx.Commit())
	var removeLow, removeHigh bool
	require.NoError(t, db.QueryRow(`SELECT remove_a, remove_b FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, low, high).Scan(&removeLow, &removeHigh))
	require.True(t, removeLow)
	require.False(t, removeHigh)

	tx, err = db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, RecordBlockTx(context.Background(), tx, high.String(), low.String(), uuid.NewString()))
	require.NoError(t, tx.Commit())
	require.NoError(t, db.QueryRow(`SELECT remove_a, remove_b FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, low, high).Scan(&removeLow, &removeHigh))
	require.True(t, removeLow)
	require.True(t, removeHigh)
}

func TestOutboxUpsertsUseWallClockAfterOlderTransaction(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b := dbtest.CreateUser(t, db), dbtest.CreateUser(t, db)
	if a.String() > b.String() {
		a, b = b, a
	}
	conversation, operation := uuid.New(), uuid.New()

	staleTx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = staleTx.Rollback() }()
	var transactionNow time.Time
	require.NoError(t, staleTx.QueryRow(`SELECT CURRENT_TIMESTAMP`).Scan(&transactionNow))
	var later time.Time
	require.Eventually(t, func() bool {
		err := db.QueryRow(`SELECT clock_timestamp() WHERE clock_timestamp() > $1`, transactionNow).Scan(&later)
		return err == nil && later.After(transactionNow)
	}, time.Second, time.Millisecond, "database wall clock should advance past the older transaction timestamp")

	seedTx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = seedTx.Exec(`
		INSERT INTO dm_block_reconciliations
			(user_a_id, user_b_id, operation_id, remove_a, created_at, updated_at)
		VALUES ($1, $2, $3, true, $4, $4)`, a, b, operation, later)
	require.NoError(t, err)
	_, err = seedTx.Exec(`
		INSERT INTO dm_block_voice_ejections
			(conversation_id, user_id, generation, created_at, updated_at)
		VALUES ($1, $2, gen_random_uuid(), $3, $3)`, conversation, a, later)
	require.NoError(t, err)
	require.NoError(t, seedTx.Commit())

	require.NoError(t, RecordBlockTx(context.Background(), staleTx, a.String(), b.String(), uuid.NewString()))
	require.NoError(t, EnqueueVoiceEjectionsTx(context.Background(), staleTx, conversation.String(), []uuid.UUID{a}))
	require.NoError(t, staleTx.Commit())

	var blockCreated, blockUpdated, ejectCreated, ejectUpdated time.Time
	require.NoError(t, db.QueryRow(`SELECT created_at, updated_at FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, a, b).Scan(&blockCreated, &blockUpdated))
	require.NoError(t, db.QueryRow(`SELECT created_at, updated_at FROM dm_block_voice_ejections WHERE conversation_id = $1 AND user_id = $2`, conversation, a).Scan(&ejectCreated, &ejectUpdated))
	assert.GreaterOrEqual(t, blockUpdated, blockCreated)
	assert.GreaterOrEqual(t, ejectUpdated, ejectCreated)
}

func modeName(mode LockMode) string {
	switch mode {
	case LockShare:
		return "share"
	case LockNoKeyUpdate:
		return "no_key_update"
	case LockUpdate:
		return "update"
	default:
		return "unknown"
	}
}

func TestReconcileDue_BoundedPairWorkConvergesAcrossPasses(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a := dbtest.CreateUser(t, db)
	b := dbtest.CreateUser(t, db)
	c := dbtest.CreateUser(t, db)
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	for i := 0; i < 17; i++ {
		conversation := uuid.New()
		_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
		require.NoError(t, err)
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3), ($1, $4)`, conversation, a, b, c)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, LockBlockSubjectsTx(context.Background(), tx, []uuid.UUID{a, b}))
	_, err = tx.Exec(`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	require.NoError(t, RecordBlockTx(context.Background(), tx, a.String(), b.String(), uuid.NewString()))
	require.NoError(t, tx.Commit())

	reconciler := New(db, nil)
	count, err := reconciler.ReconcileDue(context.Background(), 1000)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	var remaining int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE user_id = $1`, a).Scan(&remaining))
	require.Equal(t, 1, remaining, "one bounded pass must leave one of the 17 conversations")

	count, err = reconciler.ReconcileDue(context.Background(), 1000)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	var markers int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, minUUIDValue(a, b), maxUUIDValue(a, b)).Scan(&markers))
	require.Zero(t, markers)
}

func TestReconcileDue_LegacyBlockedPairRemainsQuarantinedWithoutDestructiveDirection(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a := dbtest.CreateUser(t, db)
	b := dbtest.CreateUser(t, db)
	c := dbtest.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, c)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3), ($1, $4)`, conversation, a, b, c)
	require.NoError(t, err)
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = tx.Exec(`ALTER TABLE friendships DISABLE TRIGGER require_blocked_friendship_reconciliation`)
	require.NoError(t, err)
	_, err = tx.Exec(`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	_, err = tx.Exec(`ALTER TABLE friendships ENABLE TRIGGER require_blocked_friendship_reconciliation`)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	count, err := New(db, nil).ReconcileDue(context.Background(), 100)
	require.NoError(t, err)
	require.Zero(t, count)
	var markers, participants int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, minUUIDValue(a, b), maxUUIDValue(a, b)).Scan(&markers))
	require.Zero(t, markers, "unknown legacy direction must not become destructive authority")
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1`, conversation).Scan(&participants))
	require.Equal(t, 3, participants)

	tx, err = db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = PrepareConversationTx(context.Background(), tx, conversation.String(), []uuid.UUID{c}, LockShare, LockShare)
	require.ErrorIs(t, err, ErrUnavailable)
	require.NoError(t, tx.Rollback())
}

func TestReconcileDue_FailedConversationRetainsMarkerAndSchedulesRetry(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a := dbtest.CreateUser(t, db)
	b := dbtest.CreateUser(t, db)
	members := []uuid.UUID{a, b}
	for i := 0; i < 15; i++ {
		members = append(members, dbtest.CreateUser(t, db))
	}
	_, err := db.Exec(`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`, a, b)
	require.NoError(t, err)
	conversation := uuid.New()
	_, err = db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2)`, conversation, a)
	require.NoError(t, err)
	for _, member := range members {
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2)`, conversation, member)
		require.NoError(t, err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, LockBlockSubjectsTx(context.Background(), tx, []uuid.UUID{a, b}))
	_, err = tx.Exec(`UPDATE friendships SET status = 'blocked' WHERE requester_id = $1 AND addressee_id = $2`, a, b)
	require.NoError(t, err)
	require.NoError(t, RecordBlockTx(context.Background(), tx, a.String(), b.String(), uuid.NewString()))
	require.NoError(t, tx.Commit())

	count, err := New(db, nil).ReconcileDue(context.Background(), 1)
	require.NoError(t, err)
	require.Zero(t, count)
	var attempts int
	var failureClass string
	require.NoError(t, db.QueryRow(`SELECT attempts, failure_class FROM dm_block_reconciliations WHERE user_a_id = $1 AND user_b_id = $2`, minUUIDValue(a, b), maxUUIDValue(a, b)).Scan(&attempts, &failureClass))
	require.Equal(t, 1, attempts)
	require.Equal(t, "database", failureClass)
}

func TestReconcilePair_ContinuesAfterConversationFailureWithoutAcknowledging(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	a, b, survivor := canonicalPair(dbtest.CreateUser(t, db), dbtest.CreateUser(t, db), dbtest.CreateUser(t, db))
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	later := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	op := uuid.New()

	_, err := db.Exec(`INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, true, false, $2), ($3, true, false, $2)`, first, a, later)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3), ($4, $2), ($4, $3), ($4, $5)`, first, a, b, later, survivor)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_block_reconciliations (user_a_id, user_b_id, operation_id, remove_a, remove_b, reconcile_after) VALUES ($1, $2, $3, true, true, clock_timestamp() + INTERVAL '1 hour')`, a, b, op)
	require.NoError(t, err)

	err = New(db, nil).reconcilePair(context.Background(), claimedObligation{a: a, b: b, removeA: true, removeB: true, operationID: op})
	require.Error(t, err)

	var laterParticipants int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM dm_participants WHERE conversation_id = $1`, later).Scan(&laterParticipants))
	require.Equal(t, 1, laterParticipants, "a later conversation must reconcile despite an earlier failure")
	var due bool
	require.NoError(t, db.QueryRow(`SELECT reconcile_after <= clock_timestamp() FROM dm_block_reconciliations WHERE operation_id = $1`, op).Scan(&due))
	require.False(t, due, "a failed pair must not acknowledge or reschedule its marker")
}

func minUUIDValue(a, b uuid.UUID) uuid.UUID {
	if a.String() < b.String() {
		return a
	}
	return b
}

func maxUUIDValue(a, b uuid.UUID) uuid.UUID {
	if a.String() > b.String() {
		return a
	}
	return b
}
