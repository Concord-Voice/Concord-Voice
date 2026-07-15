package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestBeginForcedSecurityClearArchivesStatusAndCreatesConservativePlan(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedclear").ID)
	viewerID := uuid.MustParse(ts.CreateTestUser(t, "forcedclearviewer").ID)
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	enableHistory(t, ts.DB, senderID, disclosure, 30)
	_, err := ts.DB.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		senderID, viewerID,
	)
	require.NoError(t, err)

	_, err = ts.DB.Exec(`
		UPDATE user_presence_settings
		SET custom_text_tier = 1, custom_text = 'sensitive status', custom_text_emoji = 'shield'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	require.NoError(t, recordAndCommit(
		t, NewRepository(ts.DB, disclosure), senderID,
		CustomTextState{}, CustomTextState{Text: "sensitive status", Emoji: "shield"},
	))

	service := NewService(ts.DB, disclosure, true)
	tx, err := service.BeginTx(ctx, nil)
	require.NoError(t, err)
	result, err := service.BeginForcedSecurityClear(ctx, tx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(tx))

	assert.Equal(t, ForcedSecurityClear, result.Mode)
	assert.Equal(t, DeliveryConservativeReset, result.Plan.Mode)
	assert.Equal(t, result.Operation.ID, result.Plan.OperationID)
	assert.Nil(t, result.Plan.Payload)
	assert.Empty(t, result.Plan.UpdateRecipients)
	assert.True(t, result.Plan.ClearRecipients[viewerID])

	var tier int
	var text, emoji *string
	require.NoError(t, ts.DB.QueryRow(`
		SELECT custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&tier, &text, &emoji))
	assert.Zero(t, tier)
	assert.Nil(t, text)
	assert.Nil(t, emoji)
	assert.Zero(t, openHistoryRowCount(t, ts.DB, senderID))
}

func TestBeginForcedSecurityClearRecorderFailureRollsBackEverything(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedclearrollback").ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'preserved')
	`, senderID)
	require.NoError(t, err)
	service := NewService(ts.DB, DisclosureState{}, false)
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		RecordTransition: func(context.Context, *sql.Tx, uuid.UUID, CustomTextState, CustomTextState) error {
			return errors.New("forced recorder failure")
		},
	})
	t.Cleanup(restore)
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = service.BeginForcedSecurityClear(context.Background(), tx, senderID)
	require.Error(t, err)
	require.NoError(t, service.RollbackTx(tx))

	var tier int
	var text string
	var version, pending int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT custom_text_tier, custom_text, presence_settings_version
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&tier, &text, &version))
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	assert.Equal(t, 1, tier)
	assert.Equal(t, "preserved", text)
	assert.Zero(t, version)
	assert.Zero(t, pending)
}

func TestBeginForcedSecurityClearSupersededAudienceFallsBackAllLocal(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedclearsupersede").ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'pending')
	`, senderID)
	require.NoError(t, err)
	service := NewService(ts.DB, DisclosureState{}, false)
	ordinaryTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = service.BeginAudienceOperation(
		context.Background(), ordinaryTx, senderID, OrdinaryAudienceWrite,
	)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(ordinaryTx))

	forcedTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	result, err := service.BeginForcedSecurityClear(context.Background(), forcedTx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.RollbackTx(forcedTx))
	assert.Nil(t, result.Plan.ClearRecipients)
	assert.Nil(t, result.Plan.UpdateRecipients)
}

func TestBeginForcedSecurityClearAfterAcknowledgedOperationUsesMappedAudience(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedclearmapped").ID)
	viewerID := uuid.MustParse(ts.CreateTestUser(t, "forcedclearmappedviewer").ID)
	_, err := ts.DB.Exec(
		`INSERT INTO friendships (requester_id, addressee_id, status) VALUES ($1, $2, 'accepted')`,
		senderID, viewerID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'acknowledged')
	`, senderID)
	require.NoError(t, err)
	service := NewService(ts.DB, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))

	ordinaryTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	ordinary, err := service.BeginAudienceOperation(
		context.Background(), ordinaryTx, senderID, OrdinaryAudienceWrite,
	)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(ordinaryTx))
	require.NoError(t, service.ClaimAndDeliver(context.Background(), DeliveryPlan{
		Mode: DeliveryExactDelta, OperationID: ordinary.ID, SenderID: senderID,
	}))
	assert.Zero(t, pendingOperationCount(t, ts.DB, senderID))

	forcedTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	result, err := service.BeginForcedSecurityClear(context.Background(), forcedTx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.RollbackTx(forcedTx))
	require.NotNil(t, result.Operation.PriorOperationID, "acknowledged operation ID remains in settings")
	assert.NotNil(t, result.Plan.ClearRecipients)
	assert.NotNil(t, result.Plan.UpdateRecipients)
	assert.True(t, result.Plan.ClearRecipients[viewerID])
}

func TestPrepareForcedClearAudience_MasterAlreadyOffSkipsAudienceLookup(t *testing.T) {
	recipients, err := prepareForcedClearAudience(context.Background(), nil, AudienceOperation{
		SenderID:            uuid.New(),
		BeforeMasterEnabled: false,
		BeforeTier:          2,
		Before:              CustomTextState{Text: "saved but disabled"},
	})

	require.NoError(t, err)
	assert.NotNil(t, recipients)
	assert.Empty(t, recipients)
}

func TestBeginForcedSecurityClear_MasterAlreadyOffStillDestroysSavedStatus(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedmasteroff").ID)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	enableHistory(t, ts.DB, senderID, disclosure, 30)
	_, err := ts.DB.Exec(`
		UPDATE user_presence_settings
		SET master_enabled = FALSE,
		    custom_text_tier = 2,
		    custom_text = 'saved but compromised',
		    custom_text_emoji = 'lock'
		WHERE user_id = $1
	`, senderID)
	require.NoError(t, err)
	require.NoError(t, recordAndCommit(
		t, NewRepository(ts.DB, disclosure), senderID,
		CustomTextState{}, CustomTextState{Text: "saved but compromised", Emoji: "lock"},
	))
	assert.Equal(t, 1, openHistoryRowCount(t, ts.DB, senderID))
	service := NewService(ts.DB, disclosure, true)
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)

	result, err := service.BeginForcedSecurityClear(context.Background(), tx, senderID)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(tx))
	assert.False(t, result.Operation.BeforeMasterEnabled)
	assert.NotNil(t, result.Plan.ClearRecipients)
	assert.Empty(t, result.Plan.ClearRecipients)
	assert.NotNil(t, result.Plan.UpdateRecipients)
	assert.Empty(t, result.Plan.UpdateRecipients)
	var master bool
	var tier int
	var text, emoji sql.NullString
	require.NoError(t, ts.DB.QueryRow(`
		SELECT master_enabled, custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&master, &tier, &text, &emoji))
	assert.False(t, master)
	assert.Zero(t, tier)
	assert.False(t, text.Valid)
	assert.False(t, emoji.Valid)
	assert.Equal(t, 1, historyRowCount(t, ts.DB, senderID))
	assert.Zero(t, openHistoryRowCount(t, ts.DB, senderID), "destructive recovery must archive the open status")
}

func TestWithReadySenderModeAllowsForcedSupersessionOfUnexpiredMarker(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedready").ID)
	service := NewService(ts.DB, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = service.BeginAudienceOperation(
		context.Background(), tx, senderID, OrdinaryAudienceWrite,
	)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(tx))

	called := false
	err = service.WithReadySenderMode(
		context.Background(), senderID, ForcedSecurityClear, func() error {
			called = true
			return nil
		},
	)
	require.NoError(t, err)
	assert.True(t, called)
}

func TestWithReadySenderModeDoesNotBypassPendingRollbackFailure(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedreadyrollback").ID)
	service := NewService(ts.DB, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task8Delivery{}))
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = service.BeginAudienceOperation(
		context.Background(), tx, senderID, OrdinaryAudienceWrite,
	)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(tx))

	rollbackCause := errors.New("forced readiness rollback failure")
	restore := service.SetTransactionTestHooks(TransactionTestHooks{
		Rollback: func(tx *sql.Tx) error {
			_ = tx.Rollback()
			return rollbackCause
		},
	})
	t.Cleanup(restore)
	called := false
	err = service.WithReadySenderMode(
		context.Background(), senderID, ForcedSecurityClear, func() error {
			called = true
			return nil
		},
	)

	require.Error(t, err)
	assert.ErrorIs(t, err, rollbackCause)
	var pending *ServiceError
	require.ErrorAs(t, err, &pending)
	assert.Equal(t, "presence_operation_pending", pending.Code)
	assert.False(t, called)
}

func TestCompleteForcedSecurityClearClassifiesAndPreservesQuarantine(t *testing.T) {
	commitCause := errors.New("forced clear commit ambiguity")

	t.Run("confirmed commit and acknowledgement preserve cause", func(t *testing.T) {
		ts, service, senderID, result := forcedClearCompletionFixture(t, &task8Delivery{})
		commitCalls := 0
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				commitCalls++
				if commitCalls == 1 {
					require.NoError(t, tx.Commit())
					return commitCause
				}
				return tx.Commit()
			},
		})
		t.Cleanup(restore)

		completion := service.CompleteForcedSecurityClear(context.Background(), result.tx, result.clear)
		assert.Equal(t, ForcedClearAcknowledged, completion.Outcome)
		assert.ErrorIs(t, completion.Err, commitCause)
		assert.True(t, completion.RequiresDisconnect())
		assert.Zero(t, pendingOperationCount(t, ts.DB, senderID))
	})

	t.Run("request cancellation after durable commit still acknowledges", func(t *testing.T) {
		ts, service, senderID, result := forcedClearCompletionFixture(t, &task8Delivery{})
		requestCtx, cancel := context.WithCancel(context.Background())
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				err := tx.Commit()
				cancel()
				return err
			},
		})
		t.Cleanup(restore)

		completion := service.CompleteForcedSecurityClear(requestCtx, result.tx, result.clear)
		assert.Equal(t, ForcedClearAcknowledged, completion.Outcome)
		require.NoError(t, completion.Err)
		assert.Zero(t, pendingOperationCount(t, ts.DB, senderID))
	})

	t.Run("confirmed rollback sends nothing", func(t *testing.T) {
		delivery := &task8Delivery{}
		ts, service, senderID, result := forcedClearCompletionFixture(t, delivery)
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(*sql.Tx) error { return commitCause },
		})
		t.Cleanup(restore)

		completion := service.CompleteForcedSecurityClear(context.Background(), result.tx, result.clear)
		assert.Equal(t, ForcedClearRolledBack, completion.Outcome)
		assert.ErrorIs(t, completion.Err, commitCause)
		assert.False(t, completion.RequiresDisconnect())
		assert.Empty(t, delivery.snapshot())
		assert.Zero(t, pendingOperationCount(t, ts.DB, senderID))
	})

	t.Run("delivery failure retains exact marker", func(t *testing.T) {
		delivery := &task8Delivery{deliver: func(context.Context, DeliveryPlan) error {
			return errTestDelivery
		}}
		ts, service, senderID, result := forcedClearCompletionFixture(t, delivery)

		completion := service.CompleteForcedSecurityClear(context.Background(), result.tx, result.clear)
		assert.Equal(t, ForcedClearQuarantined, completion.Outcome)
		assert.ErrorIs(t, completion.Err, errTestDelivery)
		assert.True(t, completion.RequiresDisconnect())
		assert.Equal(t, 1, pendingOperationCount(t, ts.DB, senderID))
	})

	t.Run("later forced write classifies superseded without stale delivery", func(t *testing.T) {
		delivery := &task8Delivery{}
		ts, service, senderID, result := forcedClearCompletionFixture(t, delivery)
		serviceB := NewService(ts.DB, DisclosureState{}, false)
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				require.NoError(t, tx.Commit())
				laterTx, err := serviceB.BeginTx(context.Background(), nil)
				require.NoError(t, err)
				_, err = serviceB.BeginAudienceOperation(
					context.Background(), laterTx, senderID, ForcedSecurityClear,
				)
				require.NoError(t, err)
				require.NoError(t, serviceB.CommitTx(laterTx))
				return commitCause
			},
		})
		t.Cleanup(restore)

		completion := service.CompleteForcedSecurityClear(context.Background(), result.tx, result.clear)
		assert.Equal(t, ForcedClearSuperseded, completion.Outcome)
		assert.ErrorIs(t, completion.Err, commitCause)
		assert.True(t, completion.RequiresDisconnect())
		assert.Empty(t, delivery.snapshot())
	})

	t.Run("unresolved commit repairs missing marker before reset", func(t *testing.T) {
		delivery := &task8Delivery{}
		ts, service, senderID, result := forcedClearCompletionFixture(t, delivery)
		service.readCommitState = func(context.Context, uuid.UUID) (audienceCommitState, error) {
			return audienceCommitState{}, errors.New("forced readback unavailable")
		}
		commitCalls := 0
		restore := service.SetTransactionTestHooks(TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				commitCalls++
				if commitCalls == 1 {
					require.NoError(t, tx.Commit())
					_, err := ts.DB.Exec(
						`DELETE FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
					)
					require.NoError(t, err)
					return commitCause
				}
				return tx.Commit()
			},
		})
		t.Cleanup(restore)

		completion := service.CompleteForcedSecurityClear(context.Background(), result.tx, result.clear)
		assert.Equal(t, ForcedClearUnresolved, completion.Outcome)
		assert.ErrorIs(t, completion.Err, commitCause)
		assert.Equal(t, 1, pendingOperationCount(t, ts.DB, senderID))
		plans := delivery.snapshot()
		require.Len(t, plans, 1)
		assert.Equal(t, DeliveryConservativeReset, plans[0].Mode)
	})
}

func TestCompleteForcedSecurityClearRejectsInvalidContractWithoutPanic(t *testing.T) {
	var nilService *Service
	completion := nilService.CompleteForcedSecurityClear(
		context.Background(), nil, ForcedClearResult{},
	)
	assert.Equal(t, ForcedClearRolledBack, completion.Outcome)
	require.Error(t, completion.Err)

	service := NewService(nil, DisclosureState{}, false)
	completion = service.CompleteForcedSecurityClear(
		context.Background(), nil, ForcedClearResult{
			Mode: ForcedSecurityClear,
		},
	)
	assert.Equal(t, ForcedClearRolledBack, completion.Outcome)
	require.Error(t, completion.Err)
}

func TestForcedClearCompletionRequiresDisconnectForEveryPossiblyDurableOutcome(t *testing.T) {
	tests := []struct {
		name    string
		outcome ForcedClearOutcome
		want    bool
	}{
		{name: "acknowledged", outcome: ForcedClearAcknowledged, want: true},
		{name: "quarantined", outcome: ForcedClearQuarantined, want: true},
		{name: "rolled back", outcome: ForcedClearRolledBack, want: false},
		{name: "superseded", outcome: ForcedClearSuperseded, want: true},
		{name: "unresolved", outcome: ForcedClearUnresolved, want: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			completion := ForcedClearCompletion{Outcome: test.outcome}
			assert.Equal(t, test.want, completion.RequiresDisconnect())
		})
	}
}

type forcedClearFixtureResult struct {
	tx    *sql.Tx
	clear ForcedClearResult
}

func forcedClearCompletionFixture(
	t *testing.T,
	delivery Delivery,
) (*presenceHistoryTestServer, *Service, uuid.UUID, forcedClearFixtureResult) {
	t.Helper()
	ts := setupPresenceHistoryTestServer(t)
	senderID := uuid.MustParse(ts.CreateTestUser(t, "forcedcompletion").ID)
	_, err := ts.DB.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'visible')
	`, senderID)
	require.NoError(t, err)
	service := NewService(ts.DB, DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	tx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	forcedClear, err := service.BeginForcedSecurityClear(context.Background(), tx, senderID)
	require.NoError(t, err)
	return ts, service, senderID, forcedClearFixtureResult{tx: tx, clear: forcedClear}
}

func pendingOperationCount(t *testing.T, db *sql.DB, senderID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&count))
	return count
}
