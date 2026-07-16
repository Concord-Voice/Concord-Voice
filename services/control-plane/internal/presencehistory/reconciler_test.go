package presencehistory

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	testhelpers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errTestDelivery = errors.New("test delivery failure")

type task8Result int64

func (r task8Result) LastInsertId() (int64, error) { return 0, errors.New("unsupported") }
func (r task8Result) RowsAffected() (int64, error) { return int64(r), nil }

type task8Delivery struct {
	mu      sync.Mutex
	plans   []DeliveryPlan
	ack     func(DeliveryPlan) DeliveryAck
	deliver func(context.Context, DeliveryPlan) error
}

type orderedTask8Delivery struct {
	initialID uuid.UUID
	entered   chan struct{}
	release   chan struct{}
	mu        sync.Mutex
	order     []string
}

func (d *orderedTask8Delivery) DeliverCustomText(ctx context.Context, plan DeliveryPlan) (DeliveryAck, error) {
	if plan.OperationID == d.initialID && plan.Mode == DeliveryExactDelta {
		close(d.entered)
		select {
		case <-d.release:
		case <-ctx.Done():
			return DeliveryAck{}, ctx.Err()
		}
		d.mu.Lock()
		d.order = append(d.order, "old-update")
		d.mu.Unlock()
	} else {
		d.mu.Lock()
		d.order = append(d.order, "new-clear")
		d.mu.Unlock()
	}
	return DeliveryAck{OperationID: plan.OperationID}, nil
}

func (d *orderedTask8Delivery) snapshot() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.order...)
}

func (d *task8Delivery) DeliverCustomText(ctx context.Context, plan DeliveryPlan) (DeliveryAck, error) {
	d.mu.Lock()
	d.plans = append(d.plans, plan)
	d.mu.Unlock()
	if d.deliver != nil {
		if err := d.deliver(ctx, plan); err != nil {
			return DeliveryAck{}, err
		}
	}
	if d.ack != nil {
		return d.ack(plan), nil
	}
	return DeliveryAck{OperationID: plan.OperationID}, nil
}

func (d *task8Delivery) snapshot() []DeliveryPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]DeliveryPlan(nil), d.plans...)
}

func TestBindDeliveryFailsClosedForNilAndDoubleBinding(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)
	require.Error(t, service.BindDelivery(nil))
	var typedNil *task8Delivery
	require.Error(t, service.BindDelivery(typedNil))
	delivery := &task8Delivery{}
	require.NoError(t, service.BindDelivery(delivery))
	require.Error(t, service.BindDelivery(delivery))
	require.Error(t, service.BindDelivery(&task8Delivery{}))
}

func TestClaimAndDeliverRejectsZeroRowMarkerDelete(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		DeleteClaim: func(context.Context, *sql.Tx, uuid.UUID, uuid.UUID) (sql.Result, error) {
			return task8Result(0), nil
		},
	})
	defer restore()

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorContains(t, err, "row count mismatch")
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
}

func TestClaimAndDeliverRequiresExactSettingsAndPendingMarkers(t *testing.T) {
	for _, tc := range []struct {
		name             string
		settingsMarker   bool
		pendingMarker    bool
		ackMatches       bool
		wantDeliveryCall bool
	}{
		{name: "exact", settingsMarker: true, pendingMarker: true, ackMatches: true, wantDeliveryCall: true},
		{name: "settings mismatch", pendingMarker: true, ackMatches: true},
		{name: "pending mismatch", settingsMarker: true, ackMatches: true},
		{name: "ack mismatch", settingsMarker: true, pendingMarker: true, wantDeliveryCall: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := testhelpers.SetupTestDB(t)
			defer cleanup()
			senderID := testhelpers.CreateUser(t, db)
			operationID := uuid.New()
			settingsID := uuid.New()
			pendingID := uuid.New()
			if tc.settingsMarker {
				settingsID = operationID
			}
			if tc.pendingMarker {
				pendingID = operationID
			}
			seedTask8Pending(t, db, senderID, settingsID, pendingID, 0, true, CustomTextState{Text: "live", Emoji: "x"})
			delivery := &task8Delivery{ack: func(plan DeliveryPlan) DeliveryAck {
				if tc.ackMatches {
					return DeliveryAck{OperationID: plan.OperationID}
				}
				return DeliveryAck{OperationID: uuid.New()}
			}}
			service := NewService(db, DisclosureState{}, false)
			require.NoError(t, service.BindDelivery(delivery))

			err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
				Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
			})
			if tc.name == "exact" {
				require.NoError(t, err)
				assert.Equal(t, 0, task8PendingCount(t, db, senderID))
			} else {
				require.Error(t, err)
				assert.Equal(t, 1, task8PendingCount(t, db, senderID))
			}
			assert.Equal(t, tc.wantDeliveryCall, len(delivery.snapshot()) > 0)
		})
	}
}

func TestClaimAndDeliverFailureRunsEmergencyResetAndRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "secret"})
	delivery := &task8Delivery{deliver: func(_ context.Context, plan DeliveryPlan) error {
		if plan.Mode == DeliveryExactDelta {
			return errTestDelivery
		}
		return nil
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
		ClearRecipients: map[uuid.UUID]bool{uuid.New(): true},
	})
	require.ErrorIs(t, err, errTestDelivery)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, DeliveryConservativeReset, plans[1].Mode)
	assert.Equal(t, plans[0].ClearRecipients, plans[1].ClearRecipients)
	assert.Nil(t, plans[1].UpdateRecipients)
}

func TestClaimAndDeliverJoinsEmergencyResetFailureAndRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "secret"})
	errEmergency := errors.New("emergency delivery failure")
	delivery := &task8Delivery{deliver: func(_ context.Context, plan DeliveryPlan) error {
		if plan.Mode == DeliveryExactDelta {
			return errTestDelivery
		}
		return errEmergency
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorIs(t, err, errTestDelivery)
	require.ErrorIs(t, err, errEmergency)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	_, marker, tier, text, _ := task8SettingsState(t, db, senderID)
	assert.Equal(t, operationID, marker)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "secret", text.String)
	require.Len(t, delivery.snapshot(), 2)
}

func TestEmergencyResetRequiresMatchingAckAndNeverChangesMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "secret"})
	delivery := &task8Delivery{ack: func(DeliveryPlan) DeliveryAck { return DeliveryAck{OperationID: uuid.New()} }}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	err := service.EmergencyReset(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.Error(t, err)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	assert.Equal(t, operationID, task8PendingMarker(t, db, senderID))
}

func TestEmergencyResetBoundsPublicTask9Calls(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)
	service.deliveryTimeout = 20 * time.Millisecond
	require.NoError(t, service.BindDelivery(&task8Delivery{deliver: func(ctx context.Context, _ DeliveryPlan) error {
		<-ctx.Done()
		return ctx.Err()
	}}))

	err := service.EmergencyReset(context.Background(), DeliveryPlan{
		OperationID: uuid.New(), SenderID: uuid.New(),
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestClaimAndDeliverTimeoutCannotAuthorizeMarkerRemoval(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "live"})
	late := make(chan struct{})
	delivery := &task8Delivery{deliver: func(ctx context.Context, plan DeliveryPlan) error {
		if plan.Mode == DeliveryConservativeReset {
			return nil
		}
		<-ctx.Done()
		close(late)
		return ctx.Err()
	}}
	service := NewService(db, DisclosureState{}, false)
	service.deliveryTimeout = 20 * time.Millisecond
	require.NoError(t, service.BindDelivery(delivery))

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	select {
	case <-late:
	case <-time.After(time.Second):
		t.Fatal("timed-out delivery did not return before ClaimAndDeliver")
	}
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
}

func TestClaimAndDeliverCommitFailureRetainsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "live"})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errors.New("forced commit failure") },
	})
	defer restore()

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryConservativeReset, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorContains(t, err, "commit")
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
}

func TestClaimAndDeliverCommittedThenErrorUsesReadbackWithoutUnsafeReset(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "live"})
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	commitCount := 0
	restore := service.SetTransactionTestHooks(TransactionTestHooks{Commit: func(tx *sql.Tx) error {
		commitCount++
		if err := tx.Commit(); err != nil {
			return err
		}
		if commitCount == 1 {
			return errors.New("ambiguous committed claim")
		}
		return nil
	}})
	defer restore()

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorContains(t, err, "ambiguous committed claim")
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "confirmed exact delivery and delete must not be followed by a privacy-widening clear")
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
}

func TestClaimAndDeliverUnresolvedCommitRestoresQuarantineBeforeReset(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "live"})
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	readCount := 0
	service.readClaimState = func(ctx context.Context, senderID uuid.UUID) (audienceCommitState, error) {
		readCount++
		if readCount == 1 {
			return audienceCommitState{}, errors.New("forced readback failure")
		}
		return service.readAudienceCommitState(ctx, senderID)
	}
	commitCount := 0
	restore := service.SetTransactionTestHooks(TransactionTestHooks{Commit: func(tx *sql.Tx) error {
		commitCount++
		if err := tx.Commit(); err != nil {
			return err
		}
		if commitCount == 1 {
			return errors.New("ambiguous committed claim")
		}
		return nil
	}})
	defer restore()

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorContains(t, err, "ambiguous committed claim")
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	assert.Equal(t, operationID, task8PendingMarker(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, DeliveryConservativeReset, plans[1].Mode)
}

func TestClaimAndDeliverCompletedSupersessionSkipsStaleEmergencyReset(t *testing.T) {
	t.Run("primary readback", func(t *testing.T) {
		testClaimAndDeliverCompletedSupersession(t, false)
	})
	t.Run("repair inspection after readback failure", func(t *testing.T) {
		testClaimAndDeliverCompletedSupersession(t, true)
	})
}

func TestCompleteClaimPreDeliveryRecoverySkipsResetAfterCompletedSupersession(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationA := uuid.New()
	seedTask8Pending(t, db, senderID, operationA, operationA, 0, false, CustomTextState{Text: "old"})
	delivery := &task8Delivery{}
	serviceA := NewService(db, DisclosureState{}, false)
	serviceB := NewService(db, DisclosureState{}, false)
	require.NoError(t, serviceA.BindDelivery(delivery))
	require.NoError(t, serviceB.BindDelivery(delivery))

	claimCause := errors.New("old claim transaction unavailable")
	recoveryEntered := make(chan struct{})
	releaseRecovery := make(chan struct{})
	beginCalls := 0
	restore := serviceA.SetTransactionTestHooks(TransactionTestHooks{
		Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
			beginCalls++
			if beginCalls == 1 {
				return nil, claimCause
			}
			if beginCalls == 2 {
				close(recoveryEntered)
				<-releaseRecovery
			}
			return db.BeginTx(ctx, options)
		},
	})
	defer restore()

	completionDone := make(chan ClaimCompletion, 1)
	go func() {
		completionDone <- serviceA.CompleteClaim(ctx, DeliveryPlan{
			Mode: DeliveryExactDelta, OperationID: operationA, SenderID: senderID,
			Payload: &CustomTextState{Text: "old"},
		})
	}()
	task8Receive(t, recoveryEntered, "old claim did not reach pre-delivery recovery")
	operationB, err := task8CompleteSupersedingOperation(ctx, serviceB, senderID, ForcedSecurityClear)
	require.NoError(t, err)
	close(releaseRecovery)
	var completion ClaimCompletion
	select {
	case completion = <-completionDone:
	case <-time.After(10 * time.Second):
		t.Fatal("old claim recovery did not finish")
	}
	require.Error(t, completion.Err)
	assert.ErrorIs(t, completion.Err, claimCause)
	assert.Equal(t, ClaimSuperseded, completion.Outcome)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "old pre-delivery recovery must not reset after newer acknowledged update")
	assert.Equal(t, operationB.ID, plans[0].OperationID)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
}

func TestCompleteClaimPreDeliveryRecoveryResetsBeforeLiveNewerPending(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationA := uuid.New()
	seedTask8Pending(t, db, senderID, operationA, operationA, 0, false, CustomTextState{Text: "old"})
	delivery := &task8Delivery{}
	serviceA := NewService(db, DisclosureState{}, false)
	serviceB := NewService(db, DisclosureState{}, false)
	require.NoError(t, serviceA.BindDelivery(delivery))
	require.NoError(t, serviceB.BindDelivery(delivery))

	claimCause := errors.New("old claim transaction unavailable")
	recoveryEntered := make(chan struct{})
	releaseRecovery := make(chan struct{})
	beginCalls := 0
	restore := serviceA.SetTransactionTestHooks(TransactionTestHooks{
		Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
			beginCalls++
			if beginCalls == 1 {
				return nil, claimCause
			}
			if beginCalls == 2 {
				close(recoveryEntered)
				<-releaseRecovery
			}
			return db.BeginTx(ctx, options)
		},
	})
	defer restore()

	completionDone := make(chan ClaimCompletion, 1)
	go func() {
		completionDone <- serviceA.CompleteClaim(ctx, DeliveryPlan{
			Mode: DeliveryExactDelta, OperationID: operationA, SenderID: senderID,
			ClearRecipients: map[uuid.UUID]bool{uuid.New(): true},
		})
	}()
	task8Receive(t, recoveryEntered, "old claim did not reach pre-delivery recovery")
	var operationB AudienceOperation
	require.NoError(t, serviceB.WithSender(ctx, senderID, func() error {
		tx, err := serviceB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		operationB, err = serviceB.BeginAudienceOperation(ctx, tx, senderID, ForcedSecurityClear)
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET custom_text_tier = 2, custom_text = 'new pending', custom_text_emoji = NULL
			WHERE user_id = $1
		`, senderID)
		if err != nil {
			return err
		}
		return serviceB.CommitTx(tx)
	}))
	close(releaseRecovery)
	var completion ClaimCompletion
	select {
	case completion = <-completionDone:
	case <-time.After(10 * time.Second):
		t.Fatal("old claim recovery did not finish")
	}
	require.Error(t, completion.Err)
	assert.ErrorIs(t, completion.Err, claimCause)
	assert.Equal(t, ClaimRecovered, completion.Outcome)
	assert.Equal(t, operationB.ID, task8PendingMarker(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "old reset must precede any later claim of the live newer marker")
	assert.Equal(t, operationA, plans[0].OperationID)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
}

func TestCompleteClaimPreDeliveryRecoveryResetsMatchingForcedClear(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	tx, err := service.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	forced, err := service.BeginForcedSecurityClear(ctx, tx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(tx))

	claimCause := errors.New("forced claim transaction unavailable")
	beginCalls := 0
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
			beginCalls++
			if beginCalls == 1 {
				return nil, claimCause
			}
			return db.BeginTx(ctx, options)
		},
	})
	defer restore()

	completion := service.CompleteClaim(ctx, forced.Plan)
	require.Error(t, completion.Err)
	assert.ErrorIs(t, completion.Err, claimCause)
	assert.Equal(t, ClaimRecovered, completion.Outcome)
	assert.Equal(t, forced.Operation.ID, task8PendingMarker(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "the matching forced plan must retain conservative recovery")
	assert.Equal(t, forced.Operation.ID, plans[0].OperationID)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
}

func TestCompleteClaimPreDeliveryRecoveryResetsVersionIncoherentClear(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	oldOperationID := uuid.New()
	newOperationID := uuid.New()
	seedTask8Pending(t, db, senderID, newOperationID, newOperationID, 4, false, CustomTextState{})
	_, err := db.Exec(`
		UPDATE user_presence_settings
		SET presence_settings_version = presence_settings_version + 1
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	completion := service.CompleteClaim(ctx, DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: oldOperationID, SenderID: senderID,
		Payload: &CustomTextState{Text: "old"},
	})
	require.Error(t, completion.Err)
	assert.ErrorContains(t, completion.Err, "marker mismatch")
	assert.Equal(t, ClaimRecovered, completion.Outcome)
	assert.Equal(t, newOperationID, task8PendingMarker(t, db, senderID))
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "version-incoherent clear must retain fail-closed recovery")
	assert.Equal(t, oldOperationID, plans[0].OperationID)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
}

func TestCompleteClaimMissingPendingDisconnectsAllAndReturnsFailure(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, 1, 'uncertain live state', 1, $2)
	`, senderID, operationID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	completion := service.CompleteClaim(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
		ClearRecipients:  map[uuid.UUID]bool{uuid.New(): true},
		UpdateRecipients: map[uuid.UUID]bool{uuid.New(): true},
		Payload:          &CustomTextState{Text: "uncertain live state"},
	})
	require.Error(t, completion.Err, "missing quarantine cannot authorize HTTP success")
	assert.Equal(t, ClaimRecovered, completion.Outcome)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Nil(t, plans[0].ClearRecipients, "unreadable state must not replay stale mapped recipients")
	assert.Nil(t, plans[0].UpdateRecipients, "nil maps select sender-safe all-local disconnect")
}

func TestBoundedDetachedContextPreservesOperationDeadlineAfterCancellation(t *testing.T) {
	operationCtx, cancelOperation := context.WithTimeout(context.Background(), 75*time.Millisecond)
	cancelOperation()
	startedAt := time.Now()
	nestedCtx, cancelNested := boundedDetachedContext(operationCtx, 500*time.Millisecond)
	defer cancelNested()
	<-nestedCtx.Done()
	elapsed := time.Since(startedAt)
	assert.GreaterOrEqual(t, elapsed, 40*time.Millisecond,
		"operation cancellation must be detached until the inherited hard deadline")
	assert.Less(t, elapsed, 250*time.Millisecond,
		"nested recovery timeouts must not extend the inherited total deadline")
}

func TestCompleteClaimIgnoresExpiredRequestDeadlineForRecovery(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)
	delivery := &task8Delivery{deliver: func(ctx context.Context, _ DeliveryPlan) error {
		return ctx.Err()
	}}
	require.NoError(t, service.BindDelivery(delivery))
	expiredRequest, cancelRequest := context.WithDeadline(
		context.Background(), time.Now().Add(-time.Second),
	)
	defer cancelRequest()

	completion := service.CompleteClaim(expiredRequest, DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: uuid.New(), SenderID: uuid.New(),
	})
	require.Error(t, completion.Err, "pre-delivery failure must still block HTTP success")
	assert.Equal(t, ClaimRecovered, completion.Outcome,
		"expired HTTP deadline must not suppress bounded conservative recovery")
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
}

func testClaimAndDeliverCompletedSupersession(t *testing.T, failPrimaryReadback bool) {
	t.Helper()
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationA := uuid.New()
	seedTask8Pending(t, db, senderID, operationA, operationA, 0, true, CustomTextState{Text: "old"})
	delivery := &task8Delivery{}
	serviceA := NewService(db, DisclosureState{}, false)
	serviceB := NewService(db, DisclosureState{}, false)
	require.NoError(t, serviceA.BindDelivery(delivery))
	require.NoError(t, serviceB.BindDelivery(delivery))

	readbackEntered := make(chan struct{})
	supersessionFinished := make(chan struct{})
	serviceA.readClaimState = func(ctx context.Context, senderID uuid.UUID) (audienceCommitState, error) {
		close(readbackEntered)
		<-supersessionFinished
		if failPrimaryReadback {
			return audienceCommitState{}, errors.New("forced primary readback failure")
		}
		return serviceA.readAudienceCommitState(ctx, senderID)
	}
	restore := serviceA.SetTransactionTestHooks(TransactionTestHooks{Commit: func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("ambiguous committed claim A")
	}})
	defer restore()

	claimADone := make(chan error, 1)
	go func() {
		claimADone <- serviceA.WithSender(ctx, senderID, func() error {
			return serviceA.ClaimAndDeliver(ctx, DeliveryPlan{
				Mode: DeliveryExactDelta, OperationID: operationA, SenderID: senderID,
				Payload: &CustomTextState{Text: "old"},
			})
		})
	}()
	task8Receive(t, readbackEntered, "claim A did not reach ambiguous commit readback")

	operationB, errB := task8CompleteSupersedingOperation(ctx, serviceB, senderID, OrdinaryAudienceWrite)
	close(supersessionFinished)
	require.NoError(t, errB)
	errA := task8ReceiveError(t, claimADone, "claim A did not finish after supersession")
	require.ErrorContains(t, errA, "ambiguous committed claim A")
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))

	plans := delivery.snapshot()
	require.Len(t, plans, 2, "completed supersession B must not be followed by stale conservative reset A")
	assert.Equal(t, operationA, plans[0].OperationID)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, operationB.ID, plans[1].OperationID)
	assert.Equal(t, DeliveryExactDelta, plans[1].Mode)
}

func TestClaimAndDeliverRepairCommitSupersessionSkipsStaleEmergencyReset(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationA := uuid.New()
	seedTask8Pending(t, db, senderID, operationA, operationA, 0, true, CustomTextState{Text: "old"})
	delivery := &task8Delivery{}
	serviceA := NewService(db, DisclosureState{}, false)
	serviceB := NewService(db, DisclosureState{}, false)
	require.NoError(t, serviceA.BindDelivery(delivery))
	require.NoError(t, serviceB.BindDelivery(delivery))

	repairReadbackEntered := make(chan struct{})
	supersessionFinished := make(chan struct{})
	readCount := 0
	serviceA.readClaimState = func(ctx context.Context, senderID uuid.UUID) (audienceCommitState, error) {
		readCount++
		if readCount == 1 {
			return audienceCommitState{}, errors.New("forced primary readback failure")
		}
		close(repairReadbackEntered)
		<-supersessionFinished
		return serviceA.readAudienceCommitState(ctx, senderID)
	}
	restore := serviceA.SetTransactionTestHooks(TransactionTestHooks{Commit: func(tx *sql.Tx) error {
		if err := tx.Commit(); err != nil {
			return err
		}
		return errors.New("ambiguous committed transaction A")
	}})
	defer restore()

	claimADone := make(chan error, 1)
	go func() {
		claimADone <- serviceA.WithSender(ctx, senderID, func() error {
			return serviceA.ClaimAndDeliver(ctx, DeliveryPlan{
				Mode: DeliveryExactDelta, OperationID: operationA, SenderID: senderID,
				Payload: &CustomTextState{Text: "old"},
			})
		})
	}()
	task8Receive(t, repairReadbackEntered, "claim A did not reach repair-commit readback")

	operationB, errB := task8CompleteSupersedingOperation(ctx, serviceB, senderID, ForcedSecurityClear)
	close(supersessionFinished)
	require.NoError(t, errB)
	errA := task8ReceiveError(t, claimADone, "claim A did not finish after repair-commit supersession")
	require.ErrorContains(t, errA, "ambiguous committed transaction A")
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))

	plans := delivery.snapshot()
	require.Len(t, plans, 2, "repair-commit supersession B must not be followed by stale conservative reset A")
	assert.Equal(t, operationA, plans[0].OperationID)
	assert.Equal(t, DeliveryExactDelta, plans[0].Mode)
	assert.Equal(t, operationB.ID, plans[1].OperationID)
	assert.Equal(t, DeliveryExactDelta, plans[1].Mode)
}

func task8CompleteSupersedingOperation(
	ctx context.Context,
	service *Service,
	senderID uuid.UUID,
	mode OperationMode,
) (operation AudienceOperation, returnErr error) {
	err := service.WithSender(ctx, senderID, func() error {
		tx, err := service.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		operation, err = service.BeginAudienceOperation(ctx, tx, senderID, mode)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `
			UPDATE user_presence_settings
			SET custom_text_tier = 2, custom_text = 'new', custom_text_emoji = NULL
			WHERE user_id = $1
		`, senderID); err != nil {
			return err
		}
		if err := service.CommitTx(tx); err != nil {
			return err
		}
		return service.ClaimAndDeliver(ctx, DeliveryPlan{
			Mode: DeliveryExactDelta, OperationID: operation.ID, SenderID: senderID,
			Payload: &CustomTextState{Text: "new"},
		})
	})
	return operation, err
}

func TestClaimAndDeliverFailsClosedForMissingTransactionAndDeletedAccount(t *testing.T) {
	t.Run("missing transaction", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		senderID := testhelpers.CreateUser(t, db)
		operationID := uuid.New()
		seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
		delivery := &task8Delivery{}
		service := NewService(db, DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Begin: func(context.Context, *sql.TxOptions) (*sql.Tx, error) { return nil, nil },
		})
		defer restore()

		err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
			Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
		})
		require.ErrorContains(t, err, "missing transaction")
		assert.Empty(t, delivery.snapshot())
		assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	})

	t.Run("account deleted", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		senderID := testhelpers.CreateUser(t, db)
		operationID := uuid.New()
		seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
		_, err := db.Exec(`DELETE FROM users WHERE id = $1`, senderID)
		require.NoError(t, err)
		delivery := &task8Delivery{}
		service := NewService(db, DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))

		err = service.ClaimAndDeliver(context.Background(), DeliveryPlan{
			Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
		})
		require.Error(t, err)
		assert.Empty(t, delivery.snapshot())
	})
}

func TestClaimForcedClearStaleEnqueueOrdering(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "old"})
	delivery := &orderedTask8Delivery{
		initialID: operationID,
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
	}
	claimService := NewService(db, DisclosureState{}, false)
	forcedService := NewService(db, DisclosureState{}, false)
	require.NoError(t, claimService.BindDelivery(delivery))
	require.NoError(t, forcedService.BindDelivery(delivery))
	claimDone := make(chan error, 1)
	go func() {
		claimDone <- claimService.WithSender(ctx, senderID, func() error {
			return claimService.ClaimAndDeliver(ctx, DeliveryPlan{
				Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
				Payload: &CustomTextState{Text: "old"},
			})
		})
	}()
	task8Receive(t, delivery.entered, "claim delivery did not enter")

	forcedPID := make(chan int, 1)
	forcedDone := make(chan error, 1)
	go func() {
		forcedDone <- func() (returnErr error) {
			tx, err := forcedService.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer func() { _ = tx.Rollback() }()
			var pid int
			if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				return err
			}
			forcedPID <- pid
			operation, err := forcedService.BeginAudienceOperation(ctx, tx, senderID, ForcedSecurityClear)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `
				UPDATE user_presence_settings
				SET custom_text_tier = 0, custom_text = NULL, custom_text_emoji = NULL
				WHERE user_id = $1
			`, senderID); err != nil {
				return err
			}
			if err := forcedService.CommitTx(tx); err != nil {
				return err
			}
			return forcedService.ClaimAndDeliver(ctx, DeliveryPlan{
				Mode: DeliveryConservativeReset, OperationID: operation.ID, SenderID: senderID,
			})
		}()
	}()
	pid := <-forcedPID
	require.Eventually(t, func() bool {
		var waiting bool
		err := db.QueryRow(`
			SELECT COALESCE(wait_event_type = 'Lock', FALSE)
			FROM pg_stat_activity WHERE pid = $1
		`, pid).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond, "forced clear did not wait on claim-held canonical locks")
	close(delivery.release)
	require.NoError(t, task8ReceiveError(t, claimDone, "claim did not finish"))
	require.NoError(t, task8ReceiveError(t, forcedDone, "forced clear did not finish"))
	assert.Equal(t, []string{"old-update", "new-clear"}, delivery.snapshot())
}

func TestForcedClearWinsBeforeStaleClaimEnqueueOrdering(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	senderID := testhelpers.CreateUser(t, db)
	oldOperationID := uuid.New()
	seedTask8Pending(
		t, db, senderID, oldOperationID, oldOperationID, 0, true,
		CustomTextState{Text: "old"},
	)
	delivery := &task8Delivery{}
	claimService := NewService(db, DisclosureState{}, false)
	forcedService := NewService(db, DisclosureState{}, false)
	require.NoError(t, claimService.BindDelivery(delivery))
	require.NoError(t, forcedService.BindDelivery(delivery))

	forcedTx, err := forcedService.BeginTx(ctx, nil)
	require.NoError(t, err)
	defer func() { _ = forcedTx.Rollback() }()
	forced, err := forcedService.BeginForcedSecurityClear(ctx, forcedTx, senderID)
	require.NoError(t, err)
	require.NoError(t, forcedService.CommitTx(forcedTx))
	assert.Equal(t, forced.Operation.ID, task8PendingMarker(t, db, senderID))

	completion := claimService.CompleteClaim(ctx, DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: oldOperationID, SenderID: senderID,
		Payload: &CustomTextState{Text: "old"},
	})
	require.Error(t, completion.Err)
	assert.ErrorContains(t, completion.Err, "marker mismatch")
	assert.Equal(t, ClaimSuperseded, completion.Outcome)
	assert.Empty(t, delivery.snapshot(), "stale ordinary claim must not enqueue an old update")
	assert.Equal(t, forced.Operation.ID, task8PendingMarker(t, db, senderID))

	require.NoError(t, forcedService.ClaimAndDeliver(ctx, forced.Plan))
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Equal(t, forced.Operation.ID, plans[0].OperationID)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	_, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, forced.Operation.ID, marker)
	assert.Zero(t, tier)
	assert.False(t, text.Valid)
	assert.False(t, emoji.Valid)
}

func TestReconcilePendingCompensatesLiveMarkerThenClaimsDelivery(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 4, true, CustomTextState{Text: "live", Emoji: "x"})
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.DiscoveredCount)
	assert.Equal(t, 1, stats.CompensatedCount)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	version, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, int64(6), version)
	assert.NotEqual(t, operationID, marker)
	assert.Equal(t, 0, tier)
	assert.False(t, text.Valid)
	assert.False(t, emoji.Valid)
	var master bool
	require.NoError(t, db.QueryRow(
		`SELECT master_enabled FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&master))
	assert.False(t, master, "true compensation must disable the category master")
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Equal(t, marker, plans[0].OperationID)
	assert.Nil(t, plans[0].ClearRecipients, "crash recovery must force all-local disconnect")
}

func TestReconcilePending_MasterOffClaimsConservativeResetWithoutErasingSavedStatus(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 3, true, CustomTextState{
		Text: "saved while master is off", Emoji: "lock",
	})
	_, err := db.Exec(`
		UPDATE user_presence_settings
		SET master_enabled = FALSE,
		    server_voice_tier = 2,
		    server_voice_show_details = FALSE,
		    private_call_tier = 1,
		    private_call_show_details = TRUE
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(context.Background(), 10)

	require.NoError(t, err)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Zero(t, stats.CompensatedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	version, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, int64(4), version)
	assert.Equal(t, operationID, marker)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "saved while master is off", text.String)
	assert.Equal(t, "lock", emoji.String)
	var serverTier, privateTier int
	var serverDetails, privateDetails bool
	require.NoError(t, db.QueryRow(`
		SELECT server_voice_tier, server_voice_show_details,
		       private_call_tier, private_call_show_details
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&serverTier, &serverDetails, &privateTier, &privateDetails))
	assert.Equal(t, 2, serverTier)
	assert.False(t, serverDetails)
	assert.Equal(t, 1, privateTier)
	assert.True(t, privateDetails)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Equal(t, operationID, plans[0].OperationID)
	assert.Nil(t, plans[0].ClearRecipients)
	assert.Nil(t, plans[0].UpdateRecipients)
}

func TestReconcilePending_MasterOffDeliveryFailureRetainsMarkerAndSavedStatus(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 2, true, CustomTextState{
		Text: "still quarantined", Emoji: "lock",
	})
	_, err := db.Exec(
		`UPDATE user_presence_settings SET master_enabled = FALSE WHERE user_id = $1`,
		senderID,
	)
	require.NoError(t, err)
	delivery := &task8Delivery{deliver: func(context.Context, DeliveryPlan) error {
		return errTestDelivery
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(context.Background(), 10)

	require.ErrorIs(t, err, errTestDelivery)
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	assert.Equal(t, operationID, task8PendingMarker(t, db, senderID))
	_, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, operationID, marker)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "still quarantined", text.String)
	assert.Equal(t, "lock", emoji.String)
}

func TestAuthoritativeSupersedingClear_MasterOffOrLegacyClearShape(t *testing.T) {
	oldPlan := uuid.New()
	pending := pendingOperationRow{ID: uuid.New(), PriorVersion: 8}

	assert.True(t, isAuthoritativeSupersedingClear(lockedPresenceSettings{
		Version:       9,
		OperationID:   uuid.NullUUID{UUID: pending.ID, Valid: true},
		MasterEnabled: false,
		Tier:          2,
		Text:          sql.NullString{String: "saved", Valid: true},
	}, pending, oldPlan))
	assert.True(t, isAuthoritativeSupersedingClear(lockedPresenceSettings{
		Version:       9,
		OperationID:   uuid.NullUUID{UUID: pending.ID, Valid: true},
		MasterEnabled: true,
		Tier:          0,
	}, pending, oldPlan))
}

func TestReconcilePendingTierOffPreservesHiddenStatus(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 1, true, CustomTextState{
		Text: "hidden", Emoji: "shield",
	})
	_, err := db.Exec(`UPDATE user_presence_settings SET custom_text_tier = 0 WHERE user_id = $1`, senderID)
	require.NoError(t, err)
	delivery := &task8Delivery{}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Zero(t, stats.CompensatedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	version, marker, tier, text, emoji := task8SettingsState(t, db, senderID)
	assert.Equal(t, int64(2), version)
	assert.Equal(t, operationID, marker)
	assert.Zero(t, tier)
	assert.Equal(t, "hidden", text.String)
	assert.Equal(t, "shield", emoji.String)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	assert.Equal(t, operationID, plans[0].OperationID)
}

func TestReconcilePendingDeliveryRetryKeepsCompensationMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 1, true, CustomTextState{Text: "live"})
	var fail = true
	delivery := &task8Delivery{deliver: func(context.Context, DeliveryPlan) error {
		if fail {
			return errTestDelivery
		}
		return nil
	}}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))

	stats, err := service.ReconcilePending(context.Background(), 10)
	require.Error(t, err)
	assert.Equal(t, 1, stats.FailedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	_, compensationID, tier, _, _ := task8SettingsState(t, db, senderID)
	assert.NotEqual(t, operationID, compensationID)
	assert.Equal(t, 0, tier)
	task8MakePendingEligible(t, db, senderID)
	fail = false

	stats, err = service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	assert.Equal(t, compensationID, delivery.snapshot()[1].OperationID)
}

func TestReconcilePendingRecorderAndCommitFailuresRetainQuarantine(t *testing.T) {
	for _, tc := range []struct {
		name  string
		hooks TransactionTestHooks
	}{
		{
			name: "recorder failure",
			hooks: TransactionTestHooks{RecordTransition: func(
				context.Context, *sql.Tx, uuid.UUID, CustomTextState, CustomTextState,
			) error {
				return errors.New("forced recorder failure")
			}},
		},
		{
			name:  "commit rollback",
			hooks: TransactionTestHooks{Commit: func(*sql.Tx) error { return errors.New("forced commit failure") }},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := testhelpers.SetupTestDB(t)
			defer cleanup()
			senderID := testhelpers.CreateUser(t, db)
			operationID := uuid.New()
			seedTask8Pending(t, db, senderID, operationID, operationID, 2, true, CustomTextState{Text: "live"})
			service := NewService(db, DisclosureState{}, false)
			require.NoError(t, service.BindDelivery(&task8Delivery{}))
			restore := service.SetTransactionTestHooks(tc.hooks)
			defer restore()

			stats, err := service.ReconcilePending(context.Background(), 10)
			require.Error(t, err)
			assert.Equal(t, 1, stats.FailedCount)
			assert.Equal(t, 1, stats.RetainedCount)
			assert.Equal(t, operationID, task8PendingMarker(t, db, senderID))
			version, marker, tier, text, _ := task8SettingsState(t, db, senderID)
			assert.Equal(t, int64(3), version)
			assert.Equal(t, operationID, marker)
			assert.Equal(t, 2, tier)
			assert.Equal(t, "live", text.String)
		})
	}
}

func TestReconcilePendingCompensationCommitRollbackPreservesAllPriorSettingsAndProvesRollback(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 4, true, CustomTextState{
		Text: "prior status", Emoji: "shield",
	})
	_, err := db.Exec(`
		UPDATE user_presence_settings
		SET master_enabled = TRUE,
		    server_voice_tier = 1,
		    server_voice_show_details = TRUE,
		    private_call_tier = 2,
		    private_call_show_details = TRUE
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	service := NewService(db, DisclosureState{}, false)
	delivery := &task8Delivery{}
	require.NoError(t, service.BindDelivery(delivery))
	commitCause := errors.New("compensation commit acknowledgement failed after rollback")
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			require.NoError(t, tx.Rollback(), "test must make the failed compensation transaction truly roll back")
			return commitCause
		},
	})
	defer restore()

	stats, err := service.ReconcilePending(context.Background(), 10)

	require.ErrorIs(t, err, commitCause)
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Empty(t, delivery.snapshot())
	proofTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	settings, err := lockPresenceSettings(context.Background(), proofTx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.RollbackTx(proofTx))
	assert.Equal(t, int64(5), settings.Version)
	require.True(t, settings.OperationID.Valid)
	assert.Equal(t, operationID, settings.OperationID.UUID)
	assert.True(t, settings.MasterEnabled)
	assert.Equal(t, 1, settings.ServerVoiceTier, "default Server Voice tier is rollback evidence")
	assert.True(t, settings.ServerVoiceShowDetails, "default Server Voice detail flag is rollback evidence")
	assert.Equal(t, 2, settings.PrivateCallTier)
	assert.True(t, settings.PrivateCallShowDetails)
	assert.Equal(t, 2, settings.Tier)
	assert.Equal(t, "prior status", settings.Text.String)
	assert.Equal(t, "shield", settings.Emoji.String)

	compensationID := uuid.New()
	operation := compensationAudienceOperation(
		senderID, compensationID, settings, pendingOperationRow{ID: operationID},
	)
	assert.Equal(t, RollbackConfirmed, service.ClassifyAudienceCommit(context.Background(), operation))
}

func TestReconcilePendingProvesAmbiguousCompensationCommitBeforeClaim(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 3, true, CustomTextState{Text: "live"})
	service := NewService(db, DisclosureState{}, false)
	delivery := &task8Delivery{}
	require.NoError(t, service.BindDelivery(delivery))
	var commitCount int
	var commitMu sync.Mutex
	restore := service.SetTransactionTestHooks(TransactionTestHooks{Commit: func(tx *sql.Tx) error {
		commitMu.Lock()
		defer commitMu.Unlock()
		commitCount++
		if err := tx.Commit(); err != nil {
			return err
		}
		if commitCount == 1 {
			return errors.New("ambiguous committed result")
		}
		return nil
	}})
	defer restore()

	stats, err := service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.CompensatedCount)
	assert.Equal(t, 1, stats.ResolvedCount)
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
	require.Len(t, delivery.snapshot(), 1)
}

func TestClaimRollbackFailureIsJoinedAndLoggedWithoutIdentifiers(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, uuid.New(), operationID, 0, true, CustomTextState{})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	var output bytes.Buffer
	service.repository.log = logger.NewWithWriter(&output)
	restore := service.SetTransactionTestHooks(TransactionTestHooks{Rollback: func(tx *sql.Tx) error {
		_ = tx.Rollback()
		return errors.New("forced rollback failure")
	}})
	defer restore()

	err := service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: operationID, SenderID: senderID,
	})
	require.ErrorContains(t, err, "rollback")
	logs := output.String()
	assert.Contains(t, logs, "rollback_failure")
	assert.NotContains(t, logs, senderID.String())
	assert.NotContains(t, logs, operationID.String())
}

func TestReconcilePendingOnlyDeletesWithRollbackOrSupersessionProof(t *testing.T) {
	for _, tc := range []struct {
		name            string
		priorVersion    int64
		settingsVersion int64
		wantResolved    bool
	}{
		{name: "rollback proof", priorVersion: 7, settingsVersion: 7, wantResolved: true},
		{name: "supersession proof", priorVersion: 7, settingsVersion: 9, wantResolved: true},
		{name: "uncertain older version", priorVersion: 7, settingsVersion: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db, cleanup := testhelpers.SetupTestDB(t)
			defer cleanup()
			senderID := testhelpers.CreateUser(t, db)
			pendingID := uuid.New()
			settingsID := uuid.New()
			seedTask8Pending(t, db, senderID, settingsID, pendingID, tc.priorVersion, true, CustomTextState{})
			_, err := db.Exec(`UPDATE user_presence_settings SET presence_settings_version = $2 WHERE user_id = $1`, senderID, tc.settingsVersion)
			require.NoError(t, err)
			service := NewService(db, DisclosureState{}, false)
			require.NoError(t, service.BindDelivery(&task8Delivery{}))

			stats, reconcileErr := service.ReconcilePending(context.Background(), 10)
			if tc.wantResolved {
				require.NoError(t, reconcileErr)
				assert.Equal(t, 1, stats.ProvenCount)
				assert.Equal(t, 0, task8PendingCount(t, db, senderID))
			} else {
				require.Error(t, reconcileErr)
				assert.Equal(t, 1, stats.RetainedCount)
				assert.Equal(t, 1, task8PendingCount(t, db, senderID))
			}
		})
	}
}

func TestReconcilePendingHonorsEligibilityAndVersionExhaustion(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, math.MaxInt64-1, false, CustomTextState{Text: "live"})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))

	stats, err := service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	assert.Equal(t, 0, stats.DiscoveredCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
	task8MakePendingEligible(t, db, senderID)
	_, err = db.Exec(`UPDATE user_presence_settings SET presence_settings_version = $2 WHERE user_id = $1`, senderID, int64(math.MaxInt64))
	require.NoError(t, err)

	stats, err = service.ReconcilePending(context.Background(), 10)
	require.ErrorContains(t, err, "exhausted")
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
}

func TestReconcilePendingStartupPassIsBounded(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	for range 2 {
		senderID := testhelpers.CreateUser(t, db)
		operationID := uuid.New()
		seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
	}
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))

	stats, err := service.ReconcilePending(context.Background(), 1)
	require.NoError(t, err)
	assert.Equal(t, 1, stats.DiscoveredCount)
	var pendingCount int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_settings_pending_operations`).Scan(&pendingCount))
	assert.Equal(t, 1, pendingCount)
}

func TestWithReadySenderRejectsUnexpiredAndKeepsOneContinuousGate(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, false, CustomTextState{Text: "live"})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	workCalled := false
	err := service.WithReadySender(context.Background(), senderID, func() error {
		workCalled = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, workCalled)

	task8MakePendingEligible(t, db, senderID)
	workEntered := make(chan struct{})
	releaseWork := make(chan struct{})
	readyDone := make(chan error, 1)
	go func() {
		readyDone <- service.WithReadySender(context.Background(), senderID, func() error {
			close(workEntered)
			<-releaseWork
			return nil
		})
	}()
	task8Receive(t, workEntered, "eligible reconciliation never reached work")
	contenderEntered := make(chan struct{})
	contenderDone := make(chan error, 1)
	go func() {
		contenderDone <- service.WithSender(context.Background(), senderID, func() error {
			close(contenderEntered)
			return nil
		})
	}()
	select {
	case <-contenderEntered:
		t.Fatal("sender gate was released between reconciliation and work")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseWork)
	require.NoError(t, task8ReceiveError(t, readyDone, "ready sender did not finish"))
	task8Receive(t, contenderEntered, "contender did not enter after work")
	require.NoError(t, task8ReceiveError(t, contenderDone, "contender did not finish"))
}

func TestWithReadySenderDoesNotRunWorkWhenEligibleReconciliationFails(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "live"})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{deliver: func(context.Context, DeliveryPlan) error {
		return errTestDelivery
	}}))
	workCalled := false

	err := service.WithReadySender(context.Background(), senderID, func() error {
		workCalled = true
		return nil
	})
	require.ErrorIs(t, err, errTestDelivery)
	assert.False(t, workCalled)
	assert.Equal(t, 1, task8PendingCount(t, db, senderID))
}

func TestRunPendingReconcilerRetriesOnCadenceAndStopsOnCancel(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
	delivered := make(chan struct{}, 1)
	var attemptMu sync.Mutex
	attemptCount := 0
	delivery := &task8Delivery{deliver: func(context.Context, DeliveryPlan) error {
		attemptMu.Lock()
		attemptCount++
		attempt := attemptCount
		attemptMu.Unlock()
		select {
		case delivered <- struct{}{}:
		default:
		}
		if attempt == 1 {
			return errTestDelivery
		}
		return nil
	}}
	service := NewService(db, DisclosureState{}, false)
	service.reconcileInterval = 10 * time.Millisecond
	require.NoError(t, service.BindDelivery(delivery))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { service.RunPendingReconciler(ctx); close(done) }()
	task8Receive(t, delivered, "periodic reconciler did not run")
	require.Eventually(t, func() bool {
		return task8PendingCount(t, db, senderID) == 0
	}, 2*time.Second, 10*time.Millisecond, "periodic reconciler did not finish its acknowledged claim")
	attemptMu.Lock()
	assert.GreaterOrEqual(t, attemptCount, 2)
	attemptMu.Unlock()
	cancel()
	task8Receive(t, done, "periodic reconciler did not stop")
	assert.Equal(t, 0, task8PendingCount(t, db, senderID))
}

func TestReconcilePendingCountsGateCancellationAsRetained(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	held := make(chan struct{})
	release := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- service.WithSender(context.Background(), senderID, func() error {
			close(held)
			<-release
			return nil
		})
	}()
	task8Receive(t, held, "sender gate was not held")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	stats, err := service.ReconcilePending(ctx, 10)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, stats.DiscoveredCount)
	assert.Equal(t, 1, stats.RetainedCount)
	assert.Equal(t, 1, stats.FailedCount)
	close(release)
	require.NoError(t, task8ReceiveError(t, holderDone, "sender gate holder did not finish"))
}

func TestClaimAndReconcileFailClosedWithoutDependencies(t *testing.T) {
	plan := DeliveryPlan{Mode: DeliveryExactDelta, OperationID: uuid.New(), SenderID: uuid.New()}
	var nilService *Service
	require.Error(t, nilService.ClaimAndDeliver(context.Background(), plan))
	service := NewService(nil, DisclosureState{}, false)
	require.Error(t, service.ClaimAndDeliver(context.Background(), plan))
	_, err := service.ReconcilePending(context.Background(), 10)
	require.Error(t, err)
	require.Error(t, service.WithReadySender(context.Background(), plan.SenderID, func() error { return nil }))
}

func TestWithReadySenderRequiresBoundDeliveryBeforeWork(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	service := NewService(db, DisclosureState{}, false)
	workCalled := false
	err := service.WithReadySender(context.Background(), senderID, func() error {
		workCalled = true
		return nil
	})
	require.Error(t, err)
	assert.False(t, workCalled)
}

func TestReconcilePendingLogsOnlyAggregateOutcomes(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	seedTask8Pending(t, db, senderID, operationID, operationID, 0, true, CustomTextState{Text: "private-value"})
	service := NewService(db, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	var output bytes.Buffer
	service.repository.log = logger.NewWithWriter(&output)

	_, err := service.ReconcilePending(context.Background(), 10)
	require.NoError(t, err)
	logs := output.String()
	assert.Contains(t, logs, "discovered_count=1")
	assert.Contains(t, logs, "resolved_count=1")
	assert.NotContains(t, logs, senderID.String())
	assert.NotContains(t, logs, operationID.String())
	assert.NotContains(t, logs, "private-value")
}

func seedTask8Pending(
	t *testing.T,
	db *sql.DB,
	senderID uuid.UUID,
	settingsID uuid.UUID,
	pendingID uuid.UUID,
	priorVersion int64,
	eligible bool,
	state CustomTextState,
) {
	t.Helper()
	reconcileOffset := "1 minute"
	if eligible {
		reconcileOffset = "-1 second"
	}
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, custom_text_emoji,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), $5, $6)
	`, senderID, task8Tier(state), state.Text, state.Emoji, priorVersion+1, settingsID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version, created_at, reconcile_after
		) VALUES ($1, $2, $3, clock_timestamp() - INTERVAL '1 minute', clock_timestamp() + $4::INTERVAL)
	`, senderID, pendingID, priorVersion, reconcileOffset)
	require.NoError(t, err)
}

func task8Tier(state CustomTextState) int {
	if state.Text == "" {
		return 0
	}
	return 2
}

func task8MakePendingEligible(t *testing.T, db *sql.DB, senderID uuid.UUID) {
	t.Helper()
	_, err := db.Exec(`
		UPDATE presence_settings_pending_operations
		SET created_at = clock_timestamp() - INTERVAL '1 minute',
		    reconcile_after = clock_timestamp() - INTERVAL '1 second'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
}

func task8PendingCount(t *testing.T, db *sql.DB, senderID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID).Scan(&count))
	return count
}

func task8PendingMarker(t *testing.T, db *sql.DB, senderID uuid.UUID) uuid.UUID {
	t.Helper()
	var marker uuid.UUID
	require.NoError(t, db.QueryRow(`SELECT operation_id FROM presence_settings_pending_operations WHERE user_id = $1`, senderID).Scan(&marker))
	return marker
}

func task8SettingsState(t *testing.T, db *sql.DB, senderID uuid.UUID) (int64, uuid.UUID, int, sql.NullString, sql.NullString) {
	t.Helper()
	var version int64
	var marker uuid.UUID
	var tier int
	var text sql.NullString
	var emoji sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT presence_settings_version, presence_settings_operation_id,
		       custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&version, &marker, &tier, &text, &emoji))
	return version, marker, tier, text, emoji
}

func task8Receive(t *testing.T, channel <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-channel:
	case <-time.After(2 * time.Second):
		t.Fatal(message)
	}
}

func task8ReceiveError(t *testing.T, channel <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-channel:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal(message)
		return nil
	}
}
