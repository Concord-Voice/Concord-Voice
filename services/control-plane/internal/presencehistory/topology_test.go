package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"testing"
	"time"

	testhelpers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCompleteTopologyBatchCommitsDeliversAndAcknowledges(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 4, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	require.NoError(t, service.CompleteTopologyBatch(ctx, tx, batch))
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, batch.operations[0].ID, plans[0].OperationID)
	assert.Equal(t, "secret", plans[0].Payload.Text)
}

func TestCompleteTopologyBatchDetachesFromCanceledRequest(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(context.Background(), t, db)
	batch, err := service.BeginTopologyBatch(context.Background(), tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	require.NoError(t, service.CompleteTopologyBatch(requestCtx, tx, batch))
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
}

func TestCompleteTopologyPlansShareOneAggregateDeadline(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderIDs := []uuid.UUID{
		testhelpers.CreateUser(t, db),
		testhelpers.CreateUser(t, db),
	}
	for _, senderID := range senderIDs {
		seedTopologyStatus(t, db, senderID, 1, "secret")
	}
	delivery := &task8Delivery{deliver: func(ctx context.Context, plan DeliveryPlan) error {
		if plan.Mode != DeliveryExactDelta {
			return nil
		}
		<-ctx.Done()
		return ctx.Err()
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, senderIDs)
	require.NoError(t, err)
	audiences := make([]TopologyAudience, 0, len(batch.operations))
	for _, operation := range batch.operations {
		audiences = append(audiences, TopologyAudience{SenderID: operation.SenderID})
	}
	batch, err = PrepareTopologyBatch(batch, audiences)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	completionCtx, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	err = service.completeTopologyPlans(completionCtx, batch.plans)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 2, task8PendingCount(t, db, batch.operations[0].SenderID)+
		task8PendingCount(t, db, batch.operations[1].SenderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, DeliveryConservativeReset, plans[1].Mode)
	assert.Equal(t, batch.operations[0].SenderID, plans[0].SenderID)
	assert.Equal(t, batch.operations[0].SenderID, plans[1].SenderID)
}

func TestCompleteTopologyBatchFreshAuthorizationNarrowsPreparedDelta(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	clearID := testhelpers.CreateUser(t, db)
	addedID := testhelpers.CreateUser(t, db)
	extraID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 2, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	for _, recipientID := range []uuid.UUID{clearID, addedID, extraID} {
		_, err = tx.ExecContext(ctx, `
			INSERT INTO friendships (requester_id, addressee_id, status)
			VALUES ($1, $2, 'accepted')
		`, senderID, recipientID)
		require.NoError(t, err)
	}
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{
		SenderID: senderID,
		Before:   map[uuid.UUID]bool{clearID: true},
		After:    map[uuid.UUID]bool{addedID: true},
	}})
	require.NoError(t, err)

	require.NoError(t, service.CompleteTopologyBatch(ctx, tx, batch))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Empty(t, plans[0].ClearRecipients)
	assert.Equal(t, map[uuid.UUID]bool{addedID: true}, plans[0].UpdateRecipients)
	assert.NotContains(t, plans[0].UpdateRecipients, extraID)
}

func TestCompleteTopologyBatchDoesNotHoldDatabaseLocksDuringDelivery(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	deliveryEntered := make(chan struct{})
	releaseDelivery := make(chan struct{})
	delivery := &task8Delivery{deliver: func(ctx context.Context, plan DeliveryPlan) error {
		if plan.Mode == DeliveryExactDelta {
			close(deliveryEntered)
			select {
			case <-releaseDelivery:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	done := make(chan error, 1)
	go func() { done <- service.CompleteTopologyBatch(ctx, tx, batch) }()
	task8Receive(t, deliveryEntered, "topology delivery did not start")
	lockCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	lockTx, err := db.BeginTx(lockCtx, nil)
	require.NoError(t, err)
	var version int64
	require.NoError(t, lockTx.QueryRowContext(lockCtx, `
		SELECT presence_settings_version
		FROM user_presence_settings
		WHERE user_id = $1
		FOR UPDATE
	`, senderID).Scan(&version))
	require.NoError(t, lockTx.Rollback())
	close(releaseDelivery)
	require.NoError(t, task8ReceiveError(t, done, "topology completion did not finish"))
}

func TestCompleteTopologyBatchFailureResetsAndRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	clearID := testhelpers.CreateUser(t, db)
	updateID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{deliver: func(_ context.Context, plan DeliveryPlan) error {
		if plan.Mode == DeliveryExactDelta {
			return errTestDelivery
		}
		return nil
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'accepted')
	`, senderID, updateID)
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{
		SenderID: senderID,
		Before:   map[uuid.UUID]bool{clearID: true},
		After:    map[uuid.UUID]bool{updateID: true},
	}})
	require.NoError(t, err)

	err = service.CompleteTopologyBatch(ctx, tx, batch)
	require.ErrorIs(t, err, errTestDelivery)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, map[uuid.UUID]bool{clearID: true}, plans[0].ClearRecipients)
	assert.Equal(t, map[uuid.UUID]bool{updateID: true}, plans[0].UpdateRecipients)
	assert.Equal(t, DeliveryConservativeReset, plans[1].Mode)
	assert.Equal(t, map[uuid.UUID]bool{clearID: true}, plans[1].ClearRecipients)
	assert.Nil(t, plans[1].UpdateRecipients)
	assert.Nil(t, plans[1].Payload)
}

func TestCompleteTopologyBatchBadAckResetsAllAndRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{ack: func(plan DeliveryPlan) DeliveryAck {
		if plan.Mode == DeliveryExactDelta {
			return DeliveryAck{OperationID: uuid.New()}
		}
		return DeliveryAck{OperationID: plan.OperationID}
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	err = service.CompleteTopologyBatch(ctx, tx, batch)
	require.ErrorContains(t, err, "acknowledgement mismatch")
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	assert.Nil(t, plans[1].ClearRecipients)
	assert.Nil(t, plans[1].UpdateRecipients)
}

func TestCompleteTopologyBatchRejectsZeroRowAcknowledgementDelete(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		DeleteClaim: func(context.Context, *sql.Tx, uuid.UUID, uuid.UUID) (sql.Result, error) {
			return task8Result(0), nil
		},
	})
	defer restore()
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	err = service.CompleteTopologyBatch(ctx, tx, batch)
	require.ErrorContains(t, err, "row count mismatch")
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	require.Len(t, delivery.snapshot(), 2)
}

func TestCompleteTopologyBatchCommitAmbiguityConfirmedStillAcknowledges(t *testing.T) {
	testCompleteTopologyBatchCommitAmbiguity(t, 1, "test ambiguous topology commit")
}

func TestCompleteTopologyBatchAcknowledgementAmbiguityDoesNotResetConfirmedDelete(t *testing.T) {
	testCompleteTopologyBatchCommitAmbiguity(t, 2, "test ambiguous topology acknowledgement")
}

func testCompleteTopologyBatchCommitAmbiguity(t *testing.T, failCommit int, message string) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	errAmbiguous := errors.New(message)
	var commits int
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			commits++
			require.NoError(t, tx.Commit())
			if commits == failCommit {
				return errAmbiguous
			}
			return nil
		},
	})
	defer restore()
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	err = service.CompleteTopologyBatch(ctx, tx, batch)
	require.ErrorIs(t, err, errAmbiguous)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	require.Len(t, delivery.snapshot(), 1)
}

func TestCompleteTopologyBatchRolledBackCommitSkipsDelivery(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	errCommit := errors.New("test rejected topology commit")
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errCommit },
	})
	defer restore()
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	err = service.CompleteTopologyBatch(ctx, tx, batch)
	require.ErrorIs(t, err, errCommit)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	assert.Empty(t, delivery.snapshot())
}

func TestCompleteTopologyBatchUnresolvedCommitResetsAll(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	errCommit := errors.New("test unresolved topology commit")
	service.readCommitState = func(context.Context, uuid.UUID) (audienceCommitState, error) {
		return audienceCommitState{}, errors.New("test commit readback unavailable")
	}
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errCommit },
	})
	defer restore()
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	expiredCtx, cancel := context.WithDeadline(ctx, time.Now().Add(-time.Second))
	defer cancel()
	err = service.CompleteTopologyBatch(expiredCtx, tx, batch)
	require.ErrorIs(t, err, errCommit)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Nil(t, plans[0].ClearRecipients)
	assert.Nil(t, plans[0].UpdateRecipients)
}

func TestCompleteTopologyBatchInvalidBatchRollsBack(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	tx := operationBeginTx(ctx, t, db)

	err := service.CompleteTopologyBatch(ctx, tx, TopologyBatch{})
	require.ErrorContains(t, err, "invalid topology batch completion")
	err = tx.QueryRowContext(ctx, `SELECT 1`).Scan(new(int))
	assert.ErrorIs(t, err, sql.ErrTxDone)
	assert.NoError(t, rollbackTopologyBatch(nil, nil))

	validationTx := operationBeginTx(ctx, t, db)
	invalidOperation := AudienceOperation{
		ID:           uuid.New(),
		SenderID:     uuid.New(),
		PriorVersion: 1,
		Version:      1,
	}
	err = validateTopologyBatchCompletion(ctx, service, validationTx, TopologyBatch{
		operations: []AudienceOperation{invalidOperation},
		plans: []DeliveryPlan{{
			Mode:        DeliveryExactDelta,
			OperationID: invalidOperation.ID,
			SenderID:    invalidOperation.SenderID,
		}},
	})
	require.ErrorContains(t, err, "invalid topology batch operation")
	require.NoError(t, validationTx.Rollback())

	errRollback := errors.New("test topology rollback failure")
	rollbackTx := operationBeginTx(ctx, t, db)
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Rollback: func(tx *sql.Tx) error {
			_ = tx.Rollback()
			return errRollback
		},
	})
	err = rollbackTopologyBatch(service, rollbackTx)
	restore()
	require.ErrorIs(t, err, errRollback)
}

func TestTopologyMarkerVerificationAndAcknowledgementEdgeCases(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	service := NewService(db, DisclosureState{}, false)
	plan := DeliveryPlan{
		Mode:        DeliveryExactDelta,
		OperationID: uuid.New(),
		SenderID:    senderID,
	}

	current, err := service.verifyTopologyMarker(ctx, plan)
	require.NoError(t, err)
	assert.False(t, current)
	acknowledged, err := service.acknowledgeTopologyMarker(ctx, plan)
	require.NoError(t, err)
	assert.False(t, acknowledged)
	missingPlan := plan
	missingPlan.SenderID = uuid.New()
	current, err = service.verifyTopologyMarker(ctx, missingPlan)
	require.NoError(t, err)
	assert.False(t, current)
	acknowledged, err = service.acknowledgeTopologyMarker(ctx, missingPlan)
	require.NoError(t, err)
	assert.False(t, acknowledged)

	seedID := uuid.New()
	_, err = db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = 2,
		    presence_settings_operation_id = $2
		WHERE user_id = $1
	`, senderID, seedID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version, created_at, reconcile_after
		) VALUES ($1, $2, 1, clock_timestamp(), clock_timestamp() + INTERVAL '1 second')
	`, senderID, seedID)
	require.NoError(t, err)
	plan.OperationID = seedID
	mismatchedPlan := plan
	mismatchedPlan.OperationID = uuid.New()
	current, err = service.verifyTopologyMarker(ctx, mismatchedPlan)
	require.NoError(t, err)
	assert.False(t, current)
	acknowledged, err = service.acknowledgeTopologyMarker(ctx, mismatchedPlan)
	require.NoError(t, err)
	assert.False(t, acknowledged)
	current, err = service.verifyTopologyMarker(ctx, plan)
	require.ErrorContains(t, err, "version relation is uncertain")
	assert.False(t, current)
	acknowledged, err = service.acknowledgeTopologyMarker(ctx, plan)
	require.ErrorContains(t, err, "version relation is uncertain")
	assert.False(t, acknowledged)
}

func TestCompleteTopologyBatchProcessesSendersInUUIDOrder(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderA := testhelpers.CreateUser(t, db)
	senderB := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderA, 1, "a")
	seedTopologyStatus(t, db, senderB, 1, "b")
	delivery := &topologyOrderedDelivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderB, senderA})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{
		{SenderID: senderB},
		{SenderID: senderA},
	})
	require.NoError(t, err)

	require.NoError(t, service.CompleteTopologyBatch(ctx, tx, batch))
	ordered := []uuid.UUID{senderA, senderB}
	if ordered[0].String() > ordered[1].String() {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	assert.Equal(t, ordered, delivery.snapshot())
}

func TestCompleteTopologyBatchRejectsCrossProcessSuccessorDuringDelivery(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	service := NewService(db, DisclosureState{}, false)
	successorService := NewService(db, DisclosureState{}, false)
	var successorErr error
	var forcedClearErr error
	delivery := &task8Delivery{deliver: func(deliveryCtx context.Context, plan DeliveryPlan) error {
		if plan.Mode != DeliveryExactDelta {
			return nil
		}
		tx, err := db.BeginTx(deliveryCtx, nil)
		if err != nil {
			return err
		}
		_, successorErr = successorService.BeginTopologyBatch(
			deliveryCtx, tx, []uuid.UUID{senderID},
		)
		if err := tx.Rollback(); err != nil {
			return err
		}
		forcedTx, err := successorService.BeginTx(deliveryCtx, nil)
		if err != nil {
			return err
		}
		_, forcedClearErr = successorService.BeginForcedSecurityClear(
			deliveryCtx, forcedTx, senderID,
		)
		return forcedTx.Rollback()
	}}
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)

	require.NoError(t, service.CompleteTopologyBatch(ctx, tx, batch))
	var pending *ServiceError
	require.ErrorAs(t, successorErr, &pending)
	assert.Equal(t, "presence_operation_pending", pending.Code)
	pending = nil
	require.ErrorAs(t, forcedClearErr, &pending)
	assert.Equal(t, "presence_operation_pending", pending.Code)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
}

func TestOpposingMultiSenderTopologyBatchesCompleteWithoutDeadlock(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	senderA := testhelpers.CreateUser(t, db)
	senderB := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderA, 1, "a")
	seedTopologyStatus(t, db, senderB, 1, "b")
	delivery := &topologyOrderedDelivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	start := make(chan struct{})
	done := make(chan error, 2)

	run := func(senderIDs []uuid.UUID) {
		<-start
		done <- service.WithSenders(ctx, senderIDs, func() error {
			tx, err := service.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback() //nolint:errcheck
			batch, err := service.BeginTopologyBatch(ctx, tx, senderIDs)
			if err != nil {
				return err
			}
			audiences := make([]TopologyAudience, 0, len(senderIDs))
			for _, senderID := range senderIDs {
				audiences = append(audiences, TopologyAudience{SenderID: senderID})
			}
			batch, err = PrepareTopologyBatch(batch, audiences)
			if err != nil {
				return err
			}
			return service.CompleteTopologyBatch(ctx, tx, batch)
		})
	}
	go run([]uuid.UUID{senderA, senderB})
	go run([]uuid.UUID{senderB, senderA})
	close(start)
	require.NoError(t, task8ReceiveError(t, done, "first topology batch deadlocked"))
	require.NoError(t, task8ReceiveError(t, done, "second topology batch deadlocked"))

	ordered := []uuid.UUID{senderA, senderB}
	if ordered[0].String() > ordered[1].String() {
		ordered[0], ordered[1] = ordered[1], ordered[0]
	}
	assert.Equal(t, append(append([]uuid.UUID{}, ordered...), ordered...), delivery.snapshot())
}

func TestReconcilePendingTopologyMarkerDisconnectsWithoutChangingStatus(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(
		t, db, senderID, operationID, operationID, 7, true,
		CustomTextState{Text: "secret", Emoji: "lock"},
	)
	_, err := db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = 7,
		    master_enabled = TRUE
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(ctx, 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Equal(t, 0, stats.CompensatedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	version, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, int64(7), version)
	assert.Equal(t, operationID, marker)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "secret", text.String)
	assert.Equal(t, "lock", emoji.String)
	var masterEnabled bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT master_enabled FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&masterEnabled))
	assert.True(t, masterEnabled)
	var historyCount int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM presence_history WHERE sender_id = $1
	`, senderID).Scan(&historyCount))
	assert.Zero(t, historyCount)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Nil(t, plans[0].ClearRecipients)
	assert.Nil(t, plans[0].UpdateRecipients)
}

func TestReconcilePendingTopologyFailureRetainsMarkerAndStatus(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(
		t, db, senderID, operationID, operationID, 3, true,
		CustomTextState{Text: "secret", Emoji: "lock"},
	)
	_, err := db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = 3
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{deliver: func(
		context.Context,
		DeliveryPlan,
	) error {
		return errTestDelivery
	}}))

	stats, err := service.ReconcilePending(ctx, 10)
	require.ErrorIs(t, err, errTestDelivery)
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 0, stats.CompensatedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	version, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, int64(3), version)
	assert.Equal(t, operationID, marker)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "secret", text.String)
	assert.Equal(t, "lock", emoji.String)
}

func TestReconcilePendingTopologyMalformedVersionRemainsQuarantined(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 7, true, CustomTextState{})
	_, err := db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = 6
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(ctx, 10)
	require.ErrorContains(t, err, "version is uncertain")
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	assert.Empty(t, delivery.snapshot())
}

func TestReconcilePendingTaggedTopologyMarkerMismatchRemainsQuarantined(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 7, "secret")
	service := NewService(db, DisclosureState{}, false)
	delivery := &task8Delivery{}
	require.NoError(t, service.BindDelivery(delivery))

	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	require.NoError(t, tx.Commit())
	require.Len(t, batch.operations, 1)
	operationID := batch.operations[0].ID
	assert.Equal(t, topologyOperationUUIDVersion, operationID.Version())

	_, err = db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_operation_id = $2
		WHERE user_id = $1
	`, senderID, uuid.New())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE presence_settings_pending_operations
		SET created_at = clock_timestamp() - INTERVAL '1 minute',
		    reconcile_after = clock_timestamp() - INTERVAL '1 second'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)

	stats, err := service.ReconcilePending(ctx, 10)
	require.ErrorContains(t, err, "topology operation proof is uncertain")
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	assert.Equal(t, operationID, operationPendingMarker(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Equal(t, operationID, plans[0].OperationID)
	assert.Nil(t, plans[0].ClearRecipients)
	assert.Nil(t, plans[0].UpdateRecipients)
	assert.Nil(t, plans[0].Payload)

	retryStats, err := service.ReconcilePending(ctx, 10)
	require.NoError(t, err)
	assert.Zero(t, retryStats.DiscoveredCount)
	assert.Len(t, delivery.snapshot(), 1)
}

func TestReconcilePendingTopologyBadAckRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 5, true, CustomTextState{})
	_, err := db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_version = 5
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{ack: func(DeliveryPlan) DeliveryAck {
		return DeliveryAck{OperationID: uuid.New()}
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(ctx, 10)
	require.ErrorContains(t, err, "acknowledgement mismatch")
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	require.Len(t, delivery.snapshot(), 1)
}

type topologyOrderedDelivery struct {
	mu    sync.Mutex
	order []uuid.UUID
}

func (d *topologyOrderedDelivery) DeliverCustomText(
	_ context.Context,
	plan DeliveryPlan,
) (DeliveryAck, error) {
	d.mu.Lock()
	d.order = append(d.order, plan.SenderID)
	d.mu.Unlock()
	return DeliveryAck{OperationID: plan.OperationID}, nil
}

func (d *topologyOrderedDelivery) snapshot() []uuid.UUID {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]uuid.UUID(nil), d.order...)
}

func seedTopologyStatus(t *testing.T, db *sql.DB, senderID uuid.UUID, version int64, text string) {
	t.Helper()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, custom_text_tier, custom_text,
			presence_settings_version
		) VALUES ($1, TRUE, 2, $2, $3)
		ON CONFLICT (user_id) DO UPDATE
		SET master_enabled = EXCLUDED.master_enabled,
		    custom_text_tier = EXCLUDED.custom_text_tier,
		    custom_text = EXCLUDED.custom_text,
		    presence_settings_version = EXCLUDED.presence_settings_version,
		    presence_settings_operation_id = NULL
	`, senderID, text, version)
	require.NoError(t, err)
}

func TestBeginTopologyBatchRollsBackMarkersWithGraphMutation(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderA := testhelpers.CreateUser(t, db)
	senderB := testhelpers.CreateUser(t, db)

	tx := operationBeginTx(ctx, t, db)
	_, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderB, senderA})
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `
		INSERT INTO friendships (requester_id, addressee_id, status)
		VALUES ($1, $2, 'accepted')
	`, senderA, senderB)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())

	assert.Equal(t, 0, operationPendingCount(t, db, senderA))
	assert.Equal(t, 0, operationPendingCount(t, db, senderB))
	var friendships int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT COUNT(*) FROM friendships`).Scan(&friendships))
	assert.Zero(t, friendships)
}

func TestBeginTopologyBatchYieldsToPendingMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)

	t.Run("topology marker", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		firstTx := operationBeginTx(ctx, t, db)
		first, err := service.BeginTopologyBatch(ctx, firstTx, []uuid.UUID{senderID})
		require.NoError(t, err)
		require.NoError(t, firstTx.Commit())

		secondTx := operationBeginTx(ctx, t, db)
		_, err = service.BeginTopologyBatch(ctx, secondTx, []uuid.UUID{senderID})
		var pending *ServiceError
		require.ErrorAs(t, err, &pending)
		assert.Equal(t, "presence_operation_pending", pending.Code)
		require.NoError(t, secondTx.Rollback())
		assert.Equal(t, first.operations[0].ID, operationPendingMarker(t, db, senderID))
	})

	t.Run("settings marker", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		settingsTx := operationBeginTx(ctx, t, db)
		settingsOperation, err := service.BeginAudienceOperation(
			ctx, settingsTx, senderID, OrdinaryAudienceWrite,
		)
		require.NoError(t, err)
		require.NoError(t, settingsTx.Commit())

		topologyTx := operationBeginTx(ctx, t, db)
		_, err = service.BeginTopologyBatch(ctx, topologyTx, []uuid.UUID{senderID})
		var pending *ServiceError
		require.ErrorAs(t, err, &pending)
		assert.Equal(t, "presence_operation_pending", pending.Code)
		require.NoError(t, topologyTx.Rollback())
		assert.Equal(t, settingsOperation.ID, operationPendingMarker(t, db, senderID))
	})
}

func TestForcedSecurityClearYieldsToEligibleTaggedTopologyMarkerMismatch(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	senderID := testhelpers.CreateUser(t, db)

	topologyTx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, topologyTx, []uuid.UUID{senderID})
	require.NoError(t, err)
	require.NoError(t, topologyTx.Commit())
	require.Len(t, batch.operations, 1)
	operationID := batch.operations[0].ID
	assert.Equal(t, topologyOperationUUIDVersion, operationID.Version())
	_, err = db.ExecContext(ctx, `
		UPDATE user_presence_settings
		SET presence_settings_operation_id = $2
		WHERE user_id = $1
	`, senderID, uuid.New())
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `
		UPDATE presence_settings_pending_operations
		SET created_at = clock_timestamp() - INTERVAL '1 minute',
		    reconcile_after = clock_timestamp() - INTERVAL '1 second'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)

	workCalled := false
	err = service.WithReadySenderMode(ctx, senderID, ForcedSecurityClear, func() error {
		workCalled = true
		return nil
	})
	var readinessPending *ServiceError
	require.ErrorAs(t, err, &readinessPending)
	assert.Equal(t, "presence_operation_pending", readinessPending.Code)
	assert.False(t, workCalled)

	forcedTx := operationBeginTx(ctx, t, db)
	_, err = service.BeginAudienceOperation(ctx, forcedTx, senderID, ForcedSecurityClear)
	var pending *ServiceError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, "presence_operation_pending", pending.Code)
	assert.NotErrorIs(t, err, ErrPendingOperationEligible)
	require.NoError(t, forcedTx.Rollback())
	assert.Equal(t, operationID, operationPendingMarker(t, db, senderID))
}

func TestClassifyAudienceCommitTopologyOutcomes(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)

	t.Run("confirmed", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		assert.Equal(t, CommitConfirmed, service.ClassifyAudienceCommit(ctx, batch.operations[0]))
	})

	t.Run("rolled back", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		assert.Equal(t, RollbackConfirmed, service.ClassifyAudienceCommit(ctx, batch.operations[0]))
	})

	t.Run("superseded at same version", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
		require.NoError(t, err)
		require.NoError(t, tx.Commit())

		laterID := uuid.New()
		laterTx := operationBeginTx(ctx, t, db)
		_, err = laterTx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET presence_settings_operation_id = $2
			WHERE user_id = $1
		`, senderID, laterID)
		require.NoError(t, err)
		_, err = laterTx.ExecContext(ctx, `
			UPDATE presence_settings_pending_operations
			SET operation_id = $2
			WHERE user_id = $1
		`, senderID, laterID)
		require.NoError(t, err)
		require.NoError(t, laterTx.Commit())
		later := batch.operations[0]
		later.ID = laterID
		assert.Equal(t, WriteSuperseded, service.ClassifyAudienceCommit(ctx, batch.operations[0]))
		assert.Equal(t, CommitConfirmed, service.ClassifyAudienceCommit(ctx, later))
	})

	t.Run("superseding state rolled back", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		firstTx := operationBeginTx(ctx, t, db)
		first, err := service.BeginTopologyBatch(ctx, firstTx, []uuid.UUID{senderID})
		require.NoError(t, err)
		require.NoError(t, firstTx.Commit())

		secondTx := operationBeginTx(ctx, t, db)
		_, err = secondTx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET presence_settings_operation_id = $2
			WHERE user_id = $1
		`, senderID, uuid.New())
		require.NoError(t, err)
		require.NoError(t, secondTx.Rollback())
		assert.Equal(t, CommitConfirmed, service.ClassifyAudienceCommit(ctx, first.operations[0]))
	})
}

func TestBeginTopologyBatchRetainsTransactionAfterValidationError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	tx := operationBeginTx(context.Background(), t, db)
	service := NewService(db, DisclosureState{}, false)

	_, err := service.BeginTopologyBatch(context.Background(), tx, nil)
	require.Error(t, err)
	assert.NoError(t, tx.QueryRow(`SELECT 1`).Scan(new(int)))
	require.NoError(t, tx.Rollback())
	err = tx.QueryRow(`SELECT 1`).Scan(new(int))
	assert.True(t, errors.Is(err, sql.ErrTxDone))
}

func TestBeginTopologyBatchValidatesAndDeduplicatesSenders(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderA := testhelpers.CreateUser(t, db)
	senderB := testhelpers.CreateUser(t, db)

	for _, tc := range []struct {
		name      string
		senderIDs []uuid.UUID
	}{
		{name: "empty"},
		{name: "nil sender", senderIDs: []uuid.UUID{senderA, uuid.Nil}},
		{name: "over bound", senderIDs: make([]uuid.UUID, maxReconcileBatch+1)},
		{name: "missing sender", senderIDs: []uuid.UUID{uuid.New()}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tx := operationBeginTx(ctx, t, db)
			_, err := service.BeginTopologyBatch(ctx, tx, tc.senderIDs)
			require.Error(t, err)
		})
	}

	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderB, senderA, senderB})
	require.NoError(t, err)
	require.Len(t, batch.operations, 2)
	assert.Less(t, batch.operations[0].SenderID.String(), batch.operations[1].SenderID.String())
	for _, operation := range batch.operations {
		assert.Equal(t, operation.PriorVersion, operation.Version)
		assert.Equal(t, audienceTopologyOperation, operation.kind)
		assert.NotEqual(t, uuid.Nil, operation.ID)
	}
	require.NoError(t, tx.Commit())

	for _, senderID := range []uuid.UUID{senderA, senderB} {
		version, settingsMarker := operationSettingsMarker(t, db, senderID)
		assert.Equal(t, int64(0), version)
		assert.Equal(t, operationPendingMarker(t, db, senderID), settingsMarker)
	}

	markerTx := operationBeginTx(ctx, t, db)
	err = writeTopologyOperationMarker(ctx, markerTx, uuid.New(), uuid.New(), time.Now())
	require.ErrorContains(t, err, "row count mismatch")
	require.NoError(t, markerTx.Rollback())
}

func TestPrepareTopologyBatchComputesExactDeltaAndClonesInput(t *testing.T) {
	senderID := uuid.New()
	removedID := uuid.New()
	retainedID := uuid.New()
	addedID := uuid.New()
	operationID := uuid.New()
	batch := TopologyBatch{operations: []AudienceOperation{{
		kind:                audienceTopologyOperation,
		ID:                  operationID,
		SenderID:            senderID,
		PriorVersion:        7,
		Version:             7,
		Before:              CustomTextState{Text: "secret", Emoji: "lock"},
		BeforeTier:          2,
		BeforeMasterEnabled: true,
	}}}
	before := map[uuid.UUID]bool{removedID: true, retainedID: true}
	after := map[uuid.UUID]bool{retainedID: true, addedID: true}

	prepared, err := PrepareTopologyBatch(batch, []TopologyAudience{{
		SenderID: senderID,
		Before:   before,
		After:    after,
	}})
	require.NoError(t, err)
	require.Len(t, prepared.plans, 1)
	plan := prepared.plans[0]
	assert.Equal(t, map[uuid.UUID]bool{removedID: true}, plan.ClearRecipients)
	assert.Equal(t, map[uuid.UUID]bool{addedID: true}, plan.UpdateRecipients)
	require.NotNil(t, plan.Payload)
	assert.Equal(t, "secret", plan.Payload.Text)
	assert.NotContains(t, plan.ClearRecipients, retainedID)
	assert.NotContains(t, plan.UpdateRecipients, retainedID)

	before[uuid.New()] = true
	after[uuid.New()] = true
	assert.Equal(t, map[uuid.UUID]bool{removedID: true}, plan.ClearRecipients)
	assert.Equal(t, map[uuid.UUID]bool{addedID: true}, plan.UpdateRecipients)
}

func TestPrepareTopologyBatchRejectsMalformedAudienceCoverage(t *testing.T) {
	senderID := uuid.New()
	operation := AudienceOperation{
		kind:                audienceTopologyOperation,
		ID:                  uuid.New(),
		SenderID:            senderID,
		PriorVersion:        1,
		Version:             1,
		Before:              CustomTextState{Text: "active"},
		BeforeTier:          1,
		BeforeMasterEnabled: true,
	}
	batch := TopologyBatch{operations: []AudienceOperation{operation}}

	for _, tc := range []struct {
		name      string
		audiences []TopologyAudience
	}{
		{name: "missing"},
		{name: "duplicate", audiences: []TopologyAudience{{SenderID: senderID}, {SenderID: senderID}}},
		{name: "unknown sender", audiences: []TopologyAudience{{SenderID: uuid.New()}}},
		{name: "nil recipient", audiences: []TopologyAudience{{SenderID: senderID, Before: map[uuid.UUID]bool{uuid.Nil: true}}}},
		{name: "self recipient", audiences: []TopologyAudience{{SenderID: senderID, After: map[uuid.UUID]bool{senderID: true}}}},
		{name: "false recipient", audiences: []TopologyAudience{{SenderID: senderID, After: map[uuid.UUID]bool{uuid.New(): false}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := PrepareTopologyBatch(batch, tc.audiences)
			require.Error(t, err)
		})
	}
}

func TestPrepareTopologyBatchInactiveStatusRejectsAudienceAndSkipsPayload(t *testing.T) {
	senderID := uuid.New()
	batch := TopologyBatch{operations: []AudienceOperation{{
		kind:         audienceTopologyOperation,
		ID:           uuid.New(),
		SenderID:     senderID,
		PriorVersion: 2,
		Version:      2,
	}}}

	prepared, err := PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)
	require.Len(t, prepared.plans, 1)
	assert.Empty(t, prepared.plans[0].ClearRecipients)
	assert.Empty(t, prepared.plans[0].UpdateRecipients)
	assert.Nil(t, prepared.plans[0].Payload)

	_, err = PrepareTopologyBatch(batch, []TopologyAudience{{
		SenderID: senderID,
		Before:   map[uuid.UUID]bool{uuid.New(): true},
	}})
	require.Error(t, err)
}

func TestEffectiveTopologyDeliveryTimeoutIgnoresConfiguredDeliveryTimeout(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)

	t.Run("default service uses the topology const", func(t *testing.T) {
		assert.Equal(t, 1500*time.Millisecond, service.effectiveTopologyDeliveryTimeout())
	})

	t.Run("a configured settings-rail timeout does not move it", func(t *testing.T) {
		service.deliveryTimeout = 30 * time.Second
		assert.Equal(t, 1500*time.Millisecond, service.effectiveTopologyDeliveryTimeout())
		assert.Equal(t, 30*time.Second, service.effectiveDeliveryTimeout())
	})
}

func TestCompleteTopologyBatchWithOutcomeRejectsAnInvalidBatch(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)

	outcome := service.CompleteTopologyBatchWithOutcome(context.Background(), nil, TopologyBatch{})

	assert.False(t, outcome.Committed)
	require.Error(t, outcome.Err)
	assert.Contains(t, outcome.Err.Error(), "invalid topology batch completion")
}

// topologyOutcomeFixture is one seeded topology sender with a prepared
// single-sender batch, so the commit-evidence cases below differ only by the
// commit hook they install.
type topologyOutcomeFixture struct {
	service  *Service
	delivery *task8Delivery
	senderID uuid.UUID
	tx       *sql.Tx
	batch    TopologyBatch
}

func newTopologyOutcomeFixture(
	ctx context.Context,
	t *testing.T,
	db *sql.DB,
	delivery *task8Delivery,
) topologyOutcomeFixture {
	t.Helper()
	senderID := testhelpers.CreateUser(t, db)
	seedTopologyStatus(t, db, senderID, 1, "secret")
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx := operationBeginTx(ctx, t, db)
	batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
	require.NoError(t, err)
	batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
	require.NoError(t, err)
	return topologyOutcomeFixture{
		service:  service,
		delivery: delivery,
		senderID: senderID,
		tx:       tx,
		batch:    batch,
	}
}

func TestCompleteTopologyBatchWithOutcomeReportsAPlainCommit(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	fixture := newTopologyOutcomeFixture(ctx, t, db, &task8Delivery{})

	outcome := fixture.service.CompleteTopologyBatchWithOutcome(ctx, fixture.tx, fixture.batch)

	assert.True(t, outcome.Committed)
	assert.NoError(t, outcome.Err)
	assert.Equal(t, 0, task8PendingCount(t, db, fixture.senderID))
	require.Len(t, fixture.delivery.snapshot(), 1)
}

// A driver error on a transaction that DID commit is the case the outcome type
// exists for: the mutation is durable, only the fan-out is in doubt, and the
// caller owes a 503 rather than a 500.
func TestCompleteTopologyBatchWithOutcomeReportsCommittedDespiteAnAmbiguousCommitError(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	fixture := newTopologyOutcomeFixture(ctx, t, db, &task8Delivery{})
	errAmbiguous := errors.New("test ambiguous topology commit outcome")
	restore := fixture.service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			require.NoError(t, tx.Commit())
			return errAmbiguous
		},
	})
	defer restore()

	outcome := fixture.service.CompleteTopologyBatchWithOutcome(ctx, fixture.tx, fixture.batch)

	assert.True(t, outcome.Committed)
	require.ErrorIs(t, outcome.Err, errAmbiguous)
	assert.Equal(t, 0, task8PendingCount(t, db, fixture.senderID))
	require.Len(t, fixture.delivery.snapshot(), 1)
}

func TestCompleteTopologyBatchWithOutcomeReportsNotCommittedOnAProvenRollback(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	fixture := newTopologyOutcomeFixture(ctx, t, db, &task8Delivery{})
	errCommit := errors.New("test rejected topology commit outcome")
	restore := fixture.service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errCommit },
	})
	defer restore()

	outcome := fixture.service.CompleteTopologyBatchWithOutcome(ctx, fixture.tx, fixture.batch)

	assert.False(t, outcome.Committed)
	require.ErrorIs(t, outcome.Err, errCommit)
	assert.Equal(t, 0, task8PendingCount(t, db, fixture.senderID))
	assert.Empty(t, fixture.delivery.snapshot())
}

// The bound deliverTopologyPlan actually applies is reachable by no other
// assertion in this file: reverting that one call back to the settings rail's
// effectiveDeliveryTimeout leaves every other topology test green. Read the
// deadline handed to the delivery adapter instead of waiting on it, so the
// assertion costs nothing and does not depend on elapsed time.
func TestDeliverTopologyPlanBoundsOneDeliveryByTheTopologyTimeout(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	var deadlineSet bool
	var remaining time.Duration
	delivery := &task8Delivery{deliver: func(deliveryCtx context.Context, _ DeliveryPlan) error {
		deadline, ok := deliveryCtx.Deadline()
		deadlineSet = ok
		if ok {
			remaining = time.Until(deadline)
		}
		return nil
	}}
	fixture := newTopologyOutcomeFixture(ctx, t, db, delivery)

	require.NoError(t, fixture.service.CompleteTopologyBatch(ctx, fixture.tx, fixture.batch))

	require.True(t, deadlineSet)
	// The settings rail's default is 5s, so the upper bound is what makes this
	// falsifiable; the lower bound only rejects an accidentally tiny budget.
	assert.LessOrEqual(t, remaining, topologyDeliveryTimeout)
	assert.Greater(t, remaining, topologyDeliveryTimeout/2)
}

// topologyResetDeadline records the deadline the delivery adapter is handed for
// a conservative reset. Both recovery paths deliver inline on the calling
// goroutine, so plain fields need no synchronisation.
type topologyResetDeadline struct {
	set       bool
	remaining time.Duration
}

func (d *topologyResetDeadline) record(ctx context.Context) {
	deadline, ok := ctx.Deadline()
	d.set = ok
	if ok {
		d.remaining = time.Until(deadline)
	}
}

func (d *topologyResetDeadline) assertBoundedByTopologyTimeout(t *testing.T) {
	t.Helper()
	require.True(t, d.set)
	// The settings rail's default is 5s, so the upper bound is what makes this
	// falsifiable; the lower bound only rejects an accidentally tiny budget.
	assert.LessOrEqual(t, d.remaining, topologyDeliveryTimeout)
	assert.Greater(t, d.remaining, topologyDeliveryTimeout/2)
}

// The bound the two topology RECOVERY paths apply is reachable by no other
// assertion in this package: reverting recoverTopologyPlan's or
// recoverUnresolvedTopologyCommit's call back to the settings rail's
// effectiveDeliveryTimeout leaves every other topology test green. Both sites
// are observable at the same delivery seam
// TestDeliverTopologyPlanBoundsOneDeliveryByTheTopologyTimeout uses:
// boundedDetachedContext clamps to min(9s completion ceiling, now+1500ms) and
// EmergencyReset then clamps min(that, 5s) at the adapter, so a revert moves
// the adapter's deadline from 1500ms to 5s. Read the deadline handed to the
// adapter instead of waiting on it, so the lock does not depend on elapsed
// time.
func TestTopologyRecoveryResetsAreBoundedByTheTopologyTimeout(t *testing.T) {
	t.Run("delivery failure recovery", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx := context.Background()
		senderID := testhelpers.CreateUser(t, db)
		seedTopologyStatus(t, db, senderID, 1, "secret")
		var reset topologyResetDeadline
		delivery := &task8Delivery{deliver: func(deliveryCtx context.Context, plan DeliveryPlan) error {
			if plan.Mode == DeliveryExactDelta {
				return errTestDelivery
			}
			reset.record(deliveryCtx)
			return nil
		}}
		service := NewService(db, DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		tx := operationBeginTx(ctx, t, db)
		batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
		require.NoError(t, err)
		batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
		require.NoError(t, err)

		require.ErrorIs(t, service.CompleteTopologyBatch(ctx, tx, batch), errTestDelivery)

		plans := delivery.snapshot()
		require.Len(t, plans, 2)
		require.Equal(t, DeliveryConservativeReset, plans[1].Mode)
		reset.assertBoundedByTopologyTimeout(t)
	})

	// TestCompleteTopologyBatchUnresolvedCommitResetsAll drives this same path
	// through an already-expired request context; this clone keeps a live one
	// because CompleteTopologyBatchWithOutcome detaches with
	// context.WithoutCancel before any of this runs, so the expired parent
	// never reaches the bound under assertion.
	t.Run("unresolved commit recovery", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx := context.Background()
		senderID := testhelpers.CreateUser(t, db)
		seedTopologyStatus(t, db, senderID, 1, "secret")
		var reset topologyResetDeadline
		delivery := &task8Delivery{deliver: func(deliveryCtx context.Context, _ DeliveryPlan) error {
			reset.record(deliveryCtx)
			return nil
		}}
		service := NewService(db, DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		errCommit := errors.New("test unresolved topology commit bound")
		service.readCommitState = func(context.Context, uuid.UUID) (audienceCommitState, error) {
			return audienceCommitState{}, errors.New("test commit readback unavailable")
		}
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(*sql.Tx) error { return errCommit },
		})
		defer restore()
		tx := operationBeginTx(ctx, t, db)
		batch, err := service.BeginTopologyBatch(ctx, tx, []uuid.UUID{senderID})
		require.NoError(t, err)
		batch, err = PrepareTopologyBatch(batch, []TopologyAudience{{SenderID: senderID}})
		require.NoError(t, err)

		require.ErrorIs(t, service.CompleteTopologyBatch(ctx, tx, batch), errCommit)

		plans := delivery.snapshot()
		require.Len(t, plans, 1)
		require.Equal(t, DeliveryConservativeReset, plans[0].Mode)
		reset.assertBoundedByTopologyTimeout(t)
	})
}

// A typed nil *Service satisfies graphpresence.TopologyRail, so #2446's boot
// guard (HasTopologyRail) answers TRUE on one and the process boots with a dead
// rail. Both other rail methods already fail closed on a nil receiver;
// WithSenders dereferenced s.senderGates and panicked, which surfaces on the
// first gated write instead of as an error the caller can classify.
func TestWithSendersFailsClosedOnNilService(t *testing.T) {
	var service *Service

	require.NotPanics(t, func() {
		err := service.WithSenders(context.Background(), []uuid.UUID{uuid.New()},
			func() error { return nil })
		require.Error(t, err, "a nil service must fail closed, not succeed")
	}, "a nil receiver must not panic -- the boot guard cannot see a typed nil")
}

// The work function must never run on a nil service: a caller that read the
// error but had already been handed a committed transaction would be worse off
// than one that got a panic.
func TestWithSendersOnNilServiceNeverRunsWork(t *testing.T) {
	var service *Service
	ran := false

	err := service.WithSenders(context.Background(), []uuid.UUID{uuid.New()},
		func() error { ran = true; return nil })

	require.Error(t, err)
	assert.False(t, ran, "work must not run when the gates were never acquired")
}
