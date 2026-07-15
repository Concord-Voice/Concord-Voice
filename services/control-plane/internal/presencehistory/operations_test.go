package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	testhelpers "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type operationTestDelivery struct{}

func (operationTestDelivery) DeliverCustomText(
	_ context.Context,
	plan DeliveryPlan,
) (DeliveryAck, error) {
	return DeliveryAck{OperationID: plan.OperationID}, nil
}

var _ Delivery = operationTestDelivery{}

func TestAudienceOperationContracts(t *testing.T) {
	assert.Equal(t, OperationMode(0), OrdinaryAudienceWrite)
	assert.Equal(t, OperationMode(1), ForcedSecurityClear)
	assert.Equal(t, DeliveryMode(0), DeliveryExactDelta)
	assert.Equal(t, DeliveryMode(1), DeliveryConservativeReset)

	operationID := uuid.New()
	ack, err := (operationTestDelivery{}).DeliverCustomText(context.Background(), DeliveryPlan{
		Mode:        DeliveryExactDelta,
		OperationID: operationID,
	})
	require.NoError(t, err)
	assert.Equal(t, operationID, ack.OperationID)
}

func TestAudienceOperationWithSenderGateSerializationCancellationAndBoundedStripes(t *testing.T) {
	service := NewService(nil, DisclosureState{}, false)
	assert.Len(t, service.senderGates, senderGateStripes)

	senderID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- service.WithSender(context.Background(), senderID, func() error {
			close(firstEntered)
			<-releaseFirst
			return nil
		})
	}()
	operationReceiveSignal(t, firstEntered, "first sender operation did not acquire its gate")

	canceledCtx, cancel := context.WithCancel(context.Background())
	secondStarted := make(chan struct{})
	secondDone := make(chan error, 1)
	var canceledWorkRan atomic.Bool
	go func() {
		close(secondStarted)
		secondDone <- service.WithSender(canceledCtx, senderID, func() error {
			canceledWorkRan.Store(true)
			return nil
		})
	}()
	operationReceiveSignal(t, secondStarted, "canceled waiter did not start")
	cancel()
	assert.ErrorIs(t, operationReceiveError(t, secondDone, "canceled waiter did not return"), context.Canceled)
	assert.False(t, canceledWorkRan.Load())

	thirdStarted := make(chan struct{})
	thirdEntered := make(chan struct{})
	thirdDone := make(chan error, 1)
	go func() {
		close(thirdStarted)
		thirdDone <- service.WithSender(context.Background(), senderID, func() error {
			close(thirdEntered)
			return nil
		})
	}()
	operationReceiveSignal(t, thirdStarted, "third sender operation did not start")
	select {
	case <-thirdEntered:
		t.Fatal("same-sender operation bypassed the held gate")
	default:
	}
	close(releaseFirst)
	require.NoError(t, operationReceiveError(t, firstDone, "first sender operation did not finish"))
	operationReceiveSignal(t, thirdEntered, "queued sender operation did not acquire the released gate")
	require.NoError(t, operationReceiveError(t, thirdDone, "queued sender operation did not finish"))

	otherID := operationDifferentStripeUUID(senderID)
	assert.NotEqual(t, senderGateIndex(senderID), senderGateIndex(otherID))
	holdAgain := make(chan struct{})
	heldAgain := make(chan struct{})
	holderDone := make(chan error, 1)
	go func() {
		holderDone <- service.WithSender(context.Background(), senderID, func() error {
			close(heldAgain)
			<-holdAgain
			return nil
		})
	}()
	operationReceiveSignal(t, heldAgain, "same stripe was not held")
	otherEntered := make(chan struct{})
	otherDone := make(chan error, 1)
	go func() {
		otherDone <- service.WithSender(context.Background(), otherID, func() error {
			close(otherEntered)
			return nil
		})
	}()
	operationReceiveSignal(t, otherEntered, "an unrelated stripe was blocked")
	require.NoError(t, operationReceiveError(t, otherDone, "unrelated stripe did not finish"))
	close(holdAgain)
	require.NoError(t, operationReceiveError(t, holderDone, "held stripe did not finish"))
}

func TestBeginAudienceOperationCapturesPriorStateAndCommitsMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)
	priorOperationID := uuid.New()
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_presence_settings (
			user_id, master_enabled,
			server_voice_tier, server_voice_show_details,
			private_call_tier, private_call_show_details,
			custom_text_tier, custom_text, custom_text_emoji,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, FALSE, 2, FALSE, 1, TRUE, 2, 'before', '🔒', 7, $2)
	`, senderID, priorOperationID)
	require.NoError(t, err)

	tx := operationBeginTx(ctx, t, db)
	operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
	require.NoError(t, err)
	assert.NotEqual(t, uuid.Nil, operation.ID)
	assert.Equal(t, senderID, operation.SenderID)
	assert.Equal(t, int64(7), operation.PriorVersion)
	assert.Equal(t, int64(8), operation.Version)
	require.NotNil(t, operation.PriorOperationID)
	assert.Equal(t, priorOperationID, *operation.PriorOperationID)
	assert.Equal(t, CustomTextState{Text: "before", Emoji: "🔒"}, operation.Before)
	assert.Equal(t, 2, operation.BeforeTier)
	assert.False(t, operation.BeforeMasterEnabled)
	assert.Equal(t, 2, operation.BeforeServerVoiceTier)
	assert.False(t, operation.BeforeServerVoiceShowDetails)
	assert.Equal(t, 1, operation.BeforePrivateCallTier)
	assert.True(t, operation.BeforePrivateCallShowDetails)

	var (
		version        int64
		settingsMarker uuid.UUID
		pendingMarker  uuid.UUID
		pendingPrior   int64
		createdAt      time.Time
		reconcileAfter time.Time
	)
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT presence_settings_version, presence_settings_operation_id
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&version, &settingsMarker))
	require.NoError(t, tx.QueryRowContext(ctx, `
		SELECT operation_id, prior_settings_version, created_at, reconcile_after
		FROM presence_settings_pending_operations
		WHERE user_id = $1
	`, senderID).Scan(&pendingMarker, &pendingPrior, &createdAt, &reconcileAfter))
	assert.Equal(t, operation.ID, settingsMarker)
	assert.Equal(t, operation.ID, pendingMarker)
	assert.Equal(t, operation.Version, version)
	assert.Equal(t, operation.PriorVersion, pendingPrior)
	assert.Equal(t, pendingOperationGrace, reconcileAfter.Sub(createdAt))
	assert.Equal(t, reconcileAfter, operation.ReconcileAfter)
	require.NoError(t, tx.Commit())

	assert.Equal(t, 1, operationPendingCount(t, db, senderID))
}

func TestBeginAudienceOperationRollbackIsAtomic(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)

	tx := operationBeginTx(ctx, t, db)
	operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
	require.NoError(t, err)
	assert.Equal(t, int64(1), operation.Version)
	assert.Equal(t, 1, operationPendingCountTx(t, tx, senderID))
	require.NoError(t, tx.Rollback())

	assert.Equal(t, 0, operationPendingCount(t, db, senderID))
	assert.Equal(t, 0, settingsRowCount(t, db, senderID),
		"first-write settings upsert and marker must roll back together")
}

func TestBeginAudienceOperationEnforcesOnePendingMarkerAndGrace(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)

	firstTx := operationBeginTx(ctx, t, db)
	first, err := service.BeginAudienceOperation(ctx, firstTx, senderID, OrdinaryAudienceWrite)
	require.NoError(t, err)
	require.NoError(t, firstTx.Commit())

	secondTx := operationBeginTx(ctx, t, db)
	_, err = service.BeginAudienceOperation(ctx, secondTx, senderID, OrdinaryAudienceWrite)
	require.Error(t, err)
	var serviceErr *ServiceError
	require.ErrorAs(t, err, &serviceErr)
	assert.Equal(t, http.StatusServiceUnavailable, serviceErr.Status)
	assert.Equal(t, "presence_operation_pending", serviceErr.Code)
	assert.Greater(t, serviceErr.RetryAfter, time.Duration(0))
	assert.LessOrEqual(t, serviceErr.RetryAfter, pendingOperationGrace)
	require.NoError(t, secondTx.Rollback())

	assert.Equal(t, 1, operationPendingCount(t, db, senderID))
	version, marker := operationSettingsMarker(t, db, senderID)
	assert.Equal(t, int64(1), version)
	assert.Equal(t, first.ID, marker)
}

func TestBeginAudienceOperationEligibleMarkerRequiresReconciliation(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)

	firstTx := operationBeginTx(ctx, t, db)
	first, err := service.BeginAudienceOperation(ctx, firstTx, senderID, OrdinaryAudienceWrite)
	require.NoError(t, err)
	require.NoError(t, firstTx.Commit())
	_, err = db.ExecContext(ctx, `
		UPDATE presence_settings_pending_operations
		SET created_at = clock_timestamp() - INTERVAL '1 minute',
		    reconcile_after = clock_timestamp() - INTERVAL '30 seconds'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)

	nextTx := operationBeginTx(ctx, t, db)
	_, err = service.BeginAudienceOperation(ctx, nextTx, senderID, OrdinaryAudienceWrite)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrPendingOperationEligible)
	require.NoError(t, nextTx.Rollback())

	version, marker := operationSettingsMarker(t, db, senderID)
	assert.Equal(t, int64(1), version)
	assert.Equal(t, first.ID, marker)
	assert.Equal(t, 1, operationPendingCount(t, db, senderID))
}

func TestBeginAudienceOperationForcedSecurityClearSupersedesMarker(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)
	_, err := db.ExecContext(ctx, `
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, custom_text_emoji
		) VALUES ($1, 2, 'sensitive', '🔐')
	`, senderID)
	require.NoError(t, err)

	ordinaryTx := operationBeginTx(ctx, t, db)
	ordinary, err := service.BeginAudienceOperation(ctx, ordinaryTx, senderID, OrdinaryAudienceWrite)
	require.NoError(t, err)
	require.NoError(t, ordinaryTx.Commit())

	forcedTx := operationBeginTx(ctx, t, db)
	forced, err := service.BeginAudienceOperation(ctx, forcedTx, senderID, ForcedSecurityClear)
	require.NoError(t, err)
	assert.Equal(t, int64(1), forced.PriorVersion)
	assert.Equal(t, int64(2), forced.Version)
	require.NotNil(t, forced.PriorOperationID)
	assert.Equal(t, ordinary.ID, *forced.PriorOperationID)
	assert.Equal(t, CustomTextState{Text: "sensitive", Emoji: "🔐"}, forced.Before)
	assert.Equal(t, 2, forced.BeforeTier)
	assert.NotEqual(t, ordinary.ID, forced.ID)
	require.NoError(t, forcedTx.Commit())

	version, marker := operationSettingsMarker(t, db, senderID)
	assert.Equal(t, forced.Version, version)
	assert.Equal(t, forced.ID, marker)
	assert.Equal(t, forced.ID, operationPendingMarker(t, db, senderID))
	assert.Equal(t, 1, operationPendingCount(t, db, senderID))
}

func TestBeginAudienceOperationRejectsUnknownMode(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)
	senderID := testhelpers.CreateUser(t, db)
	tx := operationBeginTx(ctx, t, db)

	_, err := service.BeginAudienceOperation(ctx, tx, senderID, OperationMode(99))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid audience operation mode")
	require.NoError(t, tx.Rollback())
	assert.Equal(t, 0, settingsRowCount(t, db, senderID))
}

func TestAudienceCommitClassificationRequiresExactEvidence(t *testing.T) {
	attemptedID := uuid.MustParse("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	priorID := uuid.MustParse("bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	laterID := uuid.MustParse("cccccccc-cccc-4ccc-8ccc-cccccccccccc")
	attempted := AudienceOperation{
		ID:                           attemptedID,
		SenderID:                     uuid.New(),
		PriorVersion:                 5,
		Version:                      6,
		PriorOperationID:             &priorID,
		Before:                       CustomTextState{Text: "before", Emoji: "🔒"},
		BeforeTier:                   2,
		BeforeMasterEnabled:          false,
		BeforeServerVoiceTier:        2,
		BeforeServerVoiceShowDetails: false,
		BeforePrivateCallTier:        1,
		BeforePrivateCallShowDetails: true,
	}

	for _, tc := range []struct {
		name  string
		state audienceCommitState
		want  CommitOutcome
	}{
		{
			name: "confirmed commit",
			state: audienceCommitState{
				UserExists:             true,
				SettingsExists:         true,
				Version:                6,
				OperationID:            &attemptedID,
				PendingOperationID:     &attemptedID,
				PendingPriorVersion:    5,
				PendingPriorVersionSet: true,
			},
			want: CommitConfirmed,
		},
		{
			name: "confirmed rollback",
			state: audienceCommitState{
				UserExists:             true,
				SettingsExists:         true,
				Version:                5,
				OperationID:            &priorID,
				Before:                 CustomTextState{Text: "before", Emoji: "🔒"},
				BeforeTier:             2,
				MasterEnabled:          false,
				ServerVoiceTier:        2,
				ServerVoiceShowDetails: false,
				PrivateCallTier:        1,
				PrivateCallShowDetails: true,
			},
			want: RollbackConfirmed,
		},
		{
			name: "confirmed forced rollback preserves prior pending marker",
			state: audienceCommitState{
				UserExists:             true,
				SettingsExists:         true,
				Version:                5,
				OperationID:            &priorID,
				Before:                 CustomTextState{Text: "before", Emoji: "🔒"},
				BeforeTier:             2,
				MasterEnabled:          false,
				ServerVoiceTier:        2,
				ServerVoiceShowDetails: false,
				PrivateCallTier:        1,
				PrivateCallShowDetails: true,
				PendingOperationID:     &priorID,
			},
			want: RollbackConfirmed,
		},
		{
			name: "equal-version different operation is superseded",
			state: audienceCommitState{
				UserExists:     true,
				SettingsExists: true,
				Version:        6,
				OperationID:    &laterID,
			},
			want: WriteSuperseded,
		},
		{
			name: "higher-version different operation is superseded",
			state: audienceCommitState{
				UserExists:     true,
				SettingsExists: true,
				Version:        7,
				OperationID:    &laterID,
			},
			want: WriteSuperseded,
		},
		{
			name: "attempted settings marker without pending marker is unresolved",
			state: audienceCommitState{
				UserExists:     true,
				SettingsExists: true,
				Version:        6,
				OperationID:    &attemptedID,
			},
			want: CommitUnresolved,
		},
		{
			name: "attempted pending marker with wrong prior version is unresolved",
			state: audienceCommitState{
				UserExists:             true,
				SettingsExists:         true,
				Version:                6,
				OperationID:            &attemptedID,
				PendingOperationID:     &attemptedID,
				PendingPriorVersion:    4,
				PendingPriorVersionSet: true,
			},
			want: CommitUnresolved,
		},
		{
			name: "changed prior state is unresolved",
			state: audienceCommitState{
				UserExists:     true,
				SettingsExists: true,
				Version:        5,
				OperationID:    &priorID,
				Before:         CustomTextState{Text: "different"},
				BeforeTier:     2,
			},
			want: CommitUnresolved,
		},
		{
			name:  "missing account is unresolved",
			state: audienceCommitState{},
			want:  CommitUnresolved,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyAudienceCommitState(attempted, tc.state))
		})
	}

	rollbackState := func() audienceCommitState {
		return audienceCommitState{
			UserExists:             true,
			SettingsExists:         true,
			Version:                attempted.PriorVersion,
			OperationID:            attempted.PriorOperationID,
			Before:                 attempted.Before,
			BeforeTier:             attempted.BeforeTier,
			MasterEnabled:          attempted.BeforeMasterEnabled,
			ServerVoiceTier:        attempted.BeforeServerVoiceTier,
			ServerVoiceShowDetails: attempted.BeforeServerVoiceShowDetails,
			PrivateCallTier:        attempted.BeforePrivateCallTier,
			PrivateCallShowDetails: attempted.BeforePrivateCallShowDetails,
		}
	}
	for _, tc := range []struct {
		name   string
		change func(*audienceCommitState)
	}{
		{name: "master enabled", change: func(state *audienceCommitState) { state.MasterEnabled = true }},
		{name: "server voice tier", change: func(state *audienceCommitState) { state.ServerVoiceTier = 1 }},
		{name: "server voice details", change: func(state *audienceCommitState) { state.ServerVoiceShowDetails = true }},
		{name: "private call tier", change: func(state *audienceCommitState) { state.PrivateCallTier = 2 }},
		{name: "private call details", change: func(state *audienceCommitState) { state.PrivateCallShowDetails = false }},
	} {
		t.Run("changed prior "+tc.name+" is unresolved", func(t *testing.T) {
			state := rollbackState()
			tc.change(&state)
			assert.Equal(t, CommitUnresolved, classifyAudienceCommitState(attempted, state))
		})
	}

	firstWrite := AudienceOperation{
		ID:                           attemptedID,
		SenderID:                     uuid.New(),
		Version:                      1,
		BeforeMasterEnabled:          true,
		BeforeServerVoiceTier:        1,
		BeforeServerVoiceShowDetails: true,
		BeforePrivateCallTier:        0,
		BeforePrivateCallShowDetails: false,
	}
	assert.Equal(t, RollbackConfirmed, classifyAudienceCommitState(firstWrite, audienceCommitState{
		UserExists:             true,
		MasterEnabled:          true,
		ServerVoiceTier:        1,
		ServerVoiceShowDetails: true,
		PrivateCallTier:        0,
		PrivateCallShowDetails: false,
	}))
}

func TestClassifyAudienceCommitUsesUncanceledBoundedReadback(t *testing.T) {
	attemptedID := uuid.New()
	operation := AudienceOperation{
		ID:           attemptedID,
		SenderID:     uuid.New(),
		PriorVersion: 4,
		Version:      5,
	}
	service := NewService(nil, DisclosureState{}, false)
	var calls atomic.Int32
	service.readCommitState = func(ctx context.Context, senderID uuid.UUID) (audienceCommitState, error) {
		calls.Add(1)
		assert.Equal(t, operation.SenderID, senderID)
		assert.NoError(t, ctx.Err(), "read-back must survive request cancellation")
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		remaining := time.Until(deadline)
		assert.Greater(t, remaining, 2*time.Second)
		assert.LessOrEqual(t, remaining, commitReadbackTimeout)
		return audienceCommitState{
			UserExists:             true,
			SettingsExists:         true,
			Version:                operation.Version,
			OperationID:            &attemptedID,
			PendingOperationID:     &attemptedID,
			PendingPriorVersion:    operation.PriorVersion,
			PendingPriorVersionSet: true,
		}, nil
	}
	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()

	assert.Equal(t, CommitConfirmed, service.ClassifyAudienceCommit(requestCtx, operation))
	assert.Equal(t, int32(1), calls.Load())

	service.readCommitState = func(context.Context, uuid.UUID) (audienceCommitState, error) {
		return audienceCommitState{}, errors.New("primary unavailable")
	}
	assert.Equal(t, CommitUnresolved, service.ClassifyAudienceCommit(requestCtx, operation))
}

func TestClassifyAudienceCommitReadsPrimaryDatabaseEvidence(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	ctx := context.Background()
	service := NewService(db, DisclosureState{}, false)

	t.Run("confirmed commit", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
		require.NoError(t, err)
		require.NoError(t, tx.Commit())
		assert.Equal(t, CommitConfirmed, service.ClassifyAudienceCommit(ctx, operation))
	})

	t.Run("confirmed rollback with prior state", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		priorID := uuid.New()
		_, err := db.ExecContext(ctx, `
			INSERT INTO user_presence_settings (
				user_id, master_enabled,
				server_voice_tier, server_voice_show_details,
				private_call_tier, private_call_show_details,
				custom_text_tier, custom_text, custom_text_emoji,
				presence_settings_version, presence_settings_operation_id
			) VALUES ($1, FALSE, 2, FALSE, 1, TRUE, 2, 'prior', '🛡️', 9, $2)
		`, senderID, priorID)
		require.NoError(t, err)
		tx := operationBeginTx(ctx, t, db)
		operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
		require.NoError(t, err)
		assert.False(t, operation.BeforeMasterEnabled)
		assert.Equal(t, 2, operation.BeforeServerVoiceTier)
		assert.False(t, operation.BeforeServerVoiceShowDetails)
		assert.Equal(t, 1, operation.BeforePrivateCallTier)
		assert.True(t, operation.BeforePrivateCallShowDetails)
		require.NoError(t, tx.Rollback())
		assert.Equal(t, RollbackConfirmed, service.ClassifyAudienceCommit(ctx, operation))
	})

	t.Run("confirmed rollback of first write", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		assert.Equal(t, RollbackConfirmed, service.ClassifyAudienceCommit(ctx, operation))
	})

	t.Run("superseded write", func(t *testing.T) {
		senderID := testhelpers.CreateUser(t, db)
		tx := operationBeginTx(ctx, t, db)
		operation, err := service.BeginAudienceOperation(ctx, tx, senderID, OrdinaryAudienceWrite)
		require.NoError(t, err)
		require.NoError(t, tx.Rollback())
		laterID := uuid.New()
		_, err = db.ExecContext(ctx, `
			INSERT INTO user_presence_settings (
				user_id, presence_settings_version, presence_settings_operation_id
			) VALUES ($1, $2, $3)
		`, senderID, operation.Version, laterID)
		require.NoError(t, err)
		assert.Equal(t, WriteSuperseded, service.ClassifyAudienceCommit(ctx, operation))
	})
}

func operationBeginTx(ctx context.Context, t *testing.T, db *sql.DB) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		err := tx.Rollback()
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback operation test transaction: %v", err)
		}
	})
	return tx
}

func operationPendingCount(t *testing.T, db *sql.DB, senderID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*)
		FROM presence_settings_pending_operations
		WHERE user_id = $1
	`, senderID).Scan(&count))
	return count
}

func operationPendingCountTx(t *testing.T, tx *sql.Tx, senderID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, tx.QueryRow(`
		SELECT COUNT(*)
		FROM presence_settings_pending_operations
		WHERE user_id = $1
	`, senderID).Scan(&count))
	return count
}

func operationPendingMarker(t *testing.T, db *sql.DB, senderID uuid.UUID) uuid.UUID {
	t.Helper()
	var marker uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id
		FROM presence_settings_pending_operations
		WHERE user_id = $1
	`, senderID).Scan(&marker))
	return marker
}

func operationSettingsMarker(t *testing.T, db *sql.DB, senderID uuid.UUID) (int64, uuid.UUID) {
	t.Helper()
	var (
		version int64
		marker  uuid.UUID
	)
	require.NoError(t, db.QueryRow(`
		SELECT presence_settings_version, presence_settings_operation_id
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&version, &marker))
	return version, marker
}

func operationDifferentStripeUUID(senderID uuid.UUID) uuid.UUID {
	for candidate := byte(2); candidate != 0; candidate++ {
		other := senderID
		other[0] = candidate
		if senderGateIndex(other) != senderGateIndex(senderID) {
			return other
		}
	}
	panic("no distinct sender stripe")
}

func operationReceiveSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(3 * time.Second):
		t.Fatal(message)
	}
}

func operationReceiveError(t *testing.T, result <-chan error, message string) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal(message)
		return nil
	}
}
