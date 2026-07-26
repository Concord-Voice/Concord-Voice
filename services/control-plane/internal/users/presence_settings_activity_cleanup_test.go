package users_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type activitySettingsSuppressionCall struct {
	SenderID           uuid.UUID
	Before             presence.ActivityPolicySettings
	After              presence.ActivityPolicySettings
	MasterEnabled      bool
	ServerVoiceTier    presence.Tier
	PrivateCallTier    presence.Tier
	ContextErr         error
	ContextHasDeadline bool
}

type recordingActivitySettingsSuppressor struct {
	mu    sync.Mutex
	calls []activitySettingsSuppressionCall
	err   error
	hook  func(context.Context)
}

func (s *recordingActivitySettingsSuppressor) ApplySettingsSuppressionAlreadyGated(
	ctx context.Context,
	senderID uuid.UUID,
	before presence.ActivityPolicySettings,
	after presence.ActivityPolicySettings,
) error {
	if s.hook != nil {
		s.hook(ctx)
	}
	_, hasDeadline := ctx.Deadline()
	s.mu.Lock()
	s.calls = append(s.calls, activitySettingsSuppressionCall{
		SenderID:           senderID,
		Before:             before,
		After:              after,
		MasterEnabled:      after.MasterEnabled,
		ServerVoiceTier:    after.ServerVoiceTier,
		PrivateCallTier:    after.PrivateCallTier,
		ContextErr:         ctx.Err(),
		ContextHasDeadline: hasDeadline,
	})
	s.mu.Unlock()
	return s.err
}

func (s *recordingActivitySettingsSuppressor) SuppressAllActivityAlreadyGated(
	context.Context,
	uuid.UUID,
) error {
	return nil
}

func (s *recordingActivitySettingsSuppressor) snapshot() []activitySettingsSuppressionCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]activitySettingsSuppressionCall(nil), s.calls...)
}

type noopActivitySettingsSuppressor struct{}

func (noopActivitySettingsSuppressor) ApplySettingsSuppressionAlreadyGated(
	context.Context,
	uuid.UUID,
	presence.ActivityPolicySettings,
	presence.ActivityPolicySettings,
) error {
	return nil
}

func (noopActivitySettingsSuppressor) SuppressAllActivityAlreadyGated(
	context.Context,
	uuid.UUID,
) error {
	return nil
}

func bindNoopActivitySettingsSuppressor(handler *users.Handler) {
	handler.SetActivitySettingsSuppressor(noopActivitySettingsSuppressor{})
}

func newActivitySettingsCleanupHandler(
	t *testing.T,
	db *sql.DB,
	delivery presencehistory.Delivery,
	suppressor *recordingActivitySettingsSuppressor,
) (*users.Handler, *presencehistory.Service) {
	t.Helper()
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	handler := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	handler.SetPresenceHistory(service)
	if suppressor != nil {
		handler.SetActivitySettingsSuppressor(suppressor)
	}
	return handler, service
}

func TestUpdatePresenceSettingsRunsActivityCleanupAfterCommitAndClaim(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{}
	var sawCommittedPostState, sawAcknowledgedClaim bool
	suppressor := &recordingActivitySettingsSuppressor{hook: func(ctx context.Context) {
		var masterEnabled bool
		var serverVoiceTier, privateCallTier int
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT master_enabled, server_voice_tier, private_call_tier
			FROM user_presence_settings
			WHERE user_id = $1
		`, senderID).Scan(&masterEnabled, &serverVoiceTier, &privateCallTier))
		sawCommittedPostState = !masterEnabled &&
			serverVoiceTier == int(presence.TierServers) &&
			privateCallTier == int(presence.TierFriends)
		sawAcknowledgedClaim = len(delivery.snapshot()) == 1
	}}
	handler, _ := newActivitySettingsCleanupHandler(t, db, delivery, suppressor)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"master_enabled":    false,
		"server_voice_tier": int(presence.TierServers),
		"private_call_tier": int(presence.TierFriends),
	})

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.True(t, sawCommittedPostState)
	assert.True(t, sawAcknowledgedClaim)
	require.Equal(t, []activitySettingsSuppressionCall{{
		SenderID: senderID,
		Before: presence.ActivityPolicySettings{
			MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
			ServerVoiceShowDetails: true, PrivateCallTier: presence.TierOff,
		},
		After: presence.ActivityPolicySettings{
			MasterEnabled: false, ServerVoiceTier: presence.TierServers,
			ServerVoiceShowDetails: true, PrivateCallTier: presence.TierFriends,
		},
		MasterEnabled:      false,
		ServerVoiceTier:    presence.TierServers,
		PrivateCallTier:    presence.TierFriends,
		ContextHasDeadline: true,
	}}, suppressor.snapshot())
}

func TestUpdatePresenceSettingsKeepsSenderGateThroughActivityCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	cleanupEntered := make(chan struct{})
	releaseCleanup := make(chan struct{})
	suppressor := &recordingActivitySettingsSuppressor{hook: func(context.Context) {
		close(cleanupEntered)
		<-releaseCleanup
	}}
	handler, service := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)
	responseDone := make(chan int, 1)
	go func() {
		response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
			"server_voice_tier": int(presence.TierServers),
		})
		responseDone <- response.Code
	}()

	select {
	case <-cleanupEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("activity cleanup did not run inside the ready-sender operation")
	}
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
		t.Fatal("activity cleanup ran after the sender gate was released")
	case <-time.After(30 * time.Millisecond):
	}
	close(releaseCleanup)
	select {
	case status := <-responseDone:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(2 * time.Second):
		t.Fatal("settings request did not finish; cleanup likely reacquired the sender gate")
	}
	select {
	case <-contenderEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("sender gate contender did not enter after cleanup")
	}
	require.NoError(t, <-contenderDone)
}

func TestUpdatePresenceSettingsSkipsActivityCleanupForRejectedOrFailedWrites(t *testing.T) {
	t.Run("no fields", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		suppressor := &recordingActivitySettingsSuppressor{}
		handler, _ := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)

		response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{})

		assert.Equal(t, http.StatusBadRequest, response.Code)
		assert.Empty(t, suppressor.snapshot())
	})

	t.Run("writer failure", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		suppressor := &recordingActivitySettingsSuppressor{}
		handler, service := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Begin: func(context.Context, *sql.TxOptions) (*sql.Tx, error) {
				return nil, errors.New("settings writer unavailable")
			},
		})
		defer restore()

		response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
			"server_voice_tier": int(presence.TierServers),
		})

		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Empty(t, suppressor.snapshot())
	})
}

func TestUpdatePresenceSettingsUsesDetachedBoundedContextForActivityCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{}
	handler, service := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			err := tx.Commit()
			cancelRequest()
			return err
		},
	})
	defer restore()

	response := invokePresenceSettingsPATCHWithContext(requestCtx, handler, senderID, map[string]interface{}{
		"server_voice_tier": int(presence.TierServers),
	})

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	require.ErrorIs(t, requestCtx.Err(), context.Canceled)
	require.Len(t, suppressor.snapshot(), 1)
	assert.NoError(t, suppressor.snapshot()[0].ContextErr)
	assert.True(t, suppressor.snapshot()[0].ContextHasDeadline)
}

func TestUpdatePresenceSettingsCleanupFailureIsRetryableAfterCommit(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{err: errors.New("activity cleanup unavailable")}
	var logs strings.Builder
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task9Delivery{}))
	handler := users.NewHandler(db, logger.NewWithWriter(&logs), nil, nil, nil, nil, nil)
	handler.SetPresenceHistory(service)
	handler.SetActivitySettingsSuppressor(suppressor)

	request := map[string]interface{}{
		"master_enabled":    false,
		"server_voice_tier": int(presence.TierOff),
	}
	response := invokePresenceSettingsPATCH(handler, senderID, request)

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, logs.String(), "error_class=activity_cleanup")
	var masterEnabled bool
	var serverVoiceTier int
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled, server_voice_tier
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&masterEnabled, &serverVoiceTier))
	assert.False(t, masterEnabled)
	assert.Equal(t, int(presence.TierOff), serverVoiceTier)
	var markerOperationID uuid.UUID
	var beforeMaster, afterMaster bool
	var beforeServerTier, afterServerTier int
	require.NoError(t, db.QueryRow(`
		SELECT operation_id,
		       (evidence #>> '{before,master_enabled}')::boolean,
		       (evidence #>> '{before,server_voice_tier}')::integer,
		       (evidence #>> '{after,master_enabled}')::boolean,
		       (evidence #>> '{after,server_voice_tier}')::integer
		FROM activity_settings_pending_cleanups
		WHERE user_id = $1
	`, senderID).Scan(
		&markerOperationID,
		&beforeMaster,
		&beforeServerTier,
		&afterMaster,
		&afterServerTier,
	))
	assert.NotEqual(t, uuid.Nil, markerOperationID)
	assert.True(t, beforeMaster)
	assert.Equal(t, int(presence.TierFriends), beforeServerTier)
	assert.False(t, afterMaster)
	assert.Equal(t, int(presence.TierOff), afterServerTier)

	suppressor.err = nil
	retryService := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, retryService.BindDelivery(&task9Delivery{}))
	retryHandler := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil)
	retryHandler.SetPresenceHistory(retryService)
	retryHandler.SetActivitySettingsSuppressor(suppressor)
	retry := invokePresenceSettingsPATCH(retryHandler, senderID, request)

	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	calls := suppressor.snapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, calls[0].Before, calls[1].Before,
		"retry must use the original pre-commit policy, not the current row")
	assert.Equal(t, calls[0].After, calls[1].After)
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	assert.Zero(t, pending)
}

func TestUpdatePresenceSettingsSuppressionReceiptSurvivesFinalizationRollback(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{}
	handler, _ := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, suppressor,
	)
	require.NoError(t, execActivityCleanupTestSQL(db, `
		CREATE OR REPLACE FUNCTION reject_activity_cleanup_delete_for_test()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'forced activity cleanup finalization rollback';
		END
		$$
	`))
	require.NoError(t, execActivityCleanupTestSQL(db, `
		CREATE TRIGGER reject_activity_cleanup_delete_for_test
		BEFORE DELETE ON activity_settings_pending_cleanups
		FOR EACH ROW EXECUTE FUNCTION reject_activity_cleanup_delete_for_test()
	`))
	t.Cleanup(func() {
		assert.NoError(t, execActivityCleanupTestSQL(db, `
			DROP TRIGGER IF EXISTS reject_activity_cleanup_delete_for_test
			ON activity_settings_pending_cleanups
		`))
		assert.NoError(t, execActivityCleanupTestSQL(db,
			`DROP FUNCTION IF EXISTS reject_activity_cleanup_delete_for_test()`))
	})

	request := map[string]interface{}{"master_enabled": false}
	first := invokePresenceSettingsPATCH(handler, senderID, request)
	require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())
	require.Len(t, suppressor.snapshot(), 1)

	var suppressionCompleted bool
	require.NoError(t, db.QueryRow(`
		SELECT COALESCE((evidence ->> 'suppressed')::boolean, false)
		FROM activity_settings_pending_cleanups
		WHERE user_id = $1
	`, senderID).Scan(&suppressionCompleted))
	assert.True(t, suppressionCompleted,
		"successful external suppression needs a durable receipt before marker finalization")

	require.NoError(t, execActivityCleanupTestSQL(db, `
		DROP TRIGGER reject_activity_cleanup_delete_for_test
		ON activity_settings_pending_cleanups
	`))
	suppressor.err = errors.New("settings evidence unavailable after prior suppression")
	retry := invokePresenceSettingsPATCH(handler, senderID, request)

	require.Equal(t, http.StatusOK, retry.Code, retry.Body.String())
	assert.Len(t, suppressor.snapshot(), 1,
		"a durable suppression receipt must make marker finalization replay-only")
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	assert.Zero(t, pending)
}

func execActivityCleanupTestSQL(db *sql.DB, statement string) error {
	_, err := db.Exec(statement)
	return err
}

type settingsCleanupActivityDelivery struct {
	disconnectAllCalls int
}

func (*settingsCleanupActivityDelivery) DeliverRichPresence(
	context.Context,
	presence.DeliveryPlan,
) error {
	return nil
}

func (*settingsCleanupActivityDelivery) DisconnectRichPresenceClients(
	context.Context,
	map[uuid.UUID]bool,
) error {
	return nil
}

func (d *settingsCleanupActivityDelivery) DisconnectAllRichPresenceClients(
	context.Context,
) error {
	d.disconnectAllCalls++
	return nil
}

func activityCleanupEvidenceJSON(
	t *testing.T,
	before, after presence.ActivityPolicySettings,
) string {
	t.Helper()
	settings := func(value presence.ActivityPolicySettings) map[string]any {
		return map[string]any{
			"master_enabled":            value.MasterEnabled,
			"server_voice_tier":         int(value.ServerVoiceTier),
			"server_voice_show_details": value.ServerVoiceShowDetails,
			"private_call_tier":         int(value.PrivateCallTier),
			"private_call_show_details": value.PrivateCallShowDetails,
		}
	}
	evidence, err := json.Marshal(map[string]any{
		"version": 1,
		"before":  settings(before),
		"after":   settings(after),
	})
	require.NoError(t, err)
	return string(evidence)
}

func TestUpdatePresenceSettingsPriorIneligibleMissingActivityStateClearsMarker(t *testing.T) {
	for _, test := range []struct {
		name                  string
		beforeServerTier      presence.Tier
		beforeDetails         bool
		afterDetails          bool
		request               map[string]interface{}
		wantStatus            int
		wantMarker            int
		wantDisconnectAll     int
		wantStoredShowDetails bool
	}{
		{
			name:             "prior server voice off permits truly absent state",
			beforeServerTier: presence.TierOff, beforeDetails: false,
			afterDetails: true,
			request: map[string]interface{}{
				"server_voice_tier":         int(presence.TierFriends),
				"server_voice_show_details": true,
			},
			wantStatus: http.StatusOK, wantStoredShowDetails: true,
		},
		{
			name:             "prior server voice eligible remains fail closed",
			beforeServerTier: presence.TierFriends, beforeDetails: true,
			afterDetails: false,
			request: map[string]interface{}{
				"server_voice_show_details": true,
			},
			wantStatus: http.StatusServiceUnavailable, wantMarker: 1,
			wantDisconnectAll: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, dbCleanup := testhelpers.SetupTestDB(t)
			t.Cleanup(dbCleanup)
			redisClient, redisCleanup := testhelpers.SetupTestRedis(t)
			t.Cleanup(redisCleanup)
			userID := testhelpers.CreateUser(t, db)
			serverID := testhelpers.CreateServer(t, db, userID)
			testhelpers.AddServerMember(t, db, serverID, userID)
			channelID := uuid.New()
			_, err := db.Exec(`
				INSERT INTO channels (id, server_id, name, type)
				VALUES ($1, $2, 'settings-cleanup', 'voice')
			`, channelID, serverID)
			require.NoError(t, err)
			lifecycleAt := time.Date(2026, 7, 22, 17, 0, 0, 0, time.UTC)
			_, err = db.Exec(`
				INSERT INTO voice_participants (
					channel_id, user_id, joined_at, lifecycle_event_at
				) VALUES ($1, $2, $3, $3)
			`, channelID, userID, lifecycleAt)
			require.NoError(t, err)
			_, err = db.Exec(`
				INSERT INTO user_presence_settings (
					user_id, master_enabled, server_voice_tier,
					server_voice_show_details
				) VALUES ($1, TRUE, $2, $3)
			`, userID, presence.TierFriends, test.afterDetails)
			require.NoError(t, err)
			before := presence.ActivityPolicySettings{
				MasterEnabled: true, ServerVoiceTier: test.beforeServerTier,
				ServerVoiceShowDetails: test.beforeDetails,
				PrivateCallTier:        presence.TierOff,
			}
			after := before
			after.ServerVoiceTier = presence.TierFriends
			after.ServerVoiceShowDetails = test.afterDetails
			operationID := uuid.New()
			_, err = db.Exec(`
				INSERT INTO activity_settings_pending_cleanups (
					user_id, operation_id, evidence
				) VALUES ($1, $2, $3)
			`, userID, operationID, activityCleanupEvidenceJSON(t, before, after))
			require.NoError(t, err)

			coordinator := presencehistory.NewService(
				db, presencehistory.DisclosureState{}, false,
			)
			require.NoError(t, coordinator.BindDelivery(&task9Delivery{}))
			activityStore := presence.NewActivityStore(redisClient)
			activityDelivery := &settingsCleanupActivityDelivery{}
			activityService := presence.NewActivityService(
				coordinator,
				presence.NewActivityBuilder(db, nil, activityStore),
				activityStore,
				db,
				nil,
				activityDelivery,
				permitAllPresence{},
			)
			handler := users.NewHandler(
				db, logger.NewWithWriter(io.Discard), nil, nil, nil, nil, nil,
			)
			handler.SetPresenceHistory(coordinator)
			handler.SetActivitySettingsSuppressor(activityService)

			response := invokePresenceSettingsPATCH(handler, userID, test.request)

			assert.Equal(t, test.wantStatus, response.Code, response.Body.String())
			var markerCount int
			require.NoError(t, db.QueryRow(`
				SELECT COUNT(*) FROM activity_settings_pending_cleanups
				WHERE user_id = $1 AND operation_id = $2
			`, userID, operationID).Scan(&markerCount))
			assert.Equal(t, test.wantMarker, markerCount)
			assert.Equal(t, test.wantDisconnectAll, activityDelivery.disconnectAllCalls)
			var storedShowDetails bool
			require.NoError(t, db.QueryRow(`
				SELECT server_voice_show_details
				FROM user_presence_settings WHERE user_id = $1
			`, userID).Scan(&storedShowDetails))
			assert.Equal(t, test.wantStoredShowDetails, storedShowDetails)
		})
	}
}

func TestUpdatePresenceSettingsCustomTextOnlyResumesEarlierActivityCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{
		err: errors.New("activity cleanup unavailable"),
	}
	handler, _ := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, suppressor,
	)

	first := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"master_enabled": false,
	})
	require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	require.Equal(t, 1, pending)

	suppressor.err = nil
	customOnly := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "cleanup before custom-only success",
	})

	require.Equal(t, http.StatusOK, customOnly.Code, customOnly.Body.String())
	calls := suppressor.snapshot()
	require.Len(t, calls, 2,
		"every later settings write must resume an earlier activity-policy cleanup")
	require.Equal(t, calls[0].Before, calls[1].Before)
	require.Equal(t, calls[0].After, calls[1].After)
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	require.Zero(t, pending)
	var customText string
	require.NoError(t, db.QueryRow(`
		SELECT custom_text FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&customText))
	require.Equal(t, "cleanup before custom-only success", customText)
}

func TestUpdatePresenceSettingsSerializesCleanupAcrossHandlerInstances(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	request := map[string]interface{}{"master_enabled": false}

	failedSuppressor := &recordingActivitySettingsSuppressor{
		err: errors.New("activity cleanup unavailable"),
	}
	failedHandler, _ := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, failedSuppressor,
	)
	first := invokePresenceSettingsPATCH(failedHandler, senderID, request)
	require.Equal(t, http.StatusServiceUnavailable, first.Code, first.Body.String())

	oldCleanupEntered := make(chan struct{})
	releaseOldCleanup := make(chan struct{})
	var enterOldOnce, releaseOldOnce sync.Once
	t.Cleanup(func() { releaseOldOnce.Do(func() { close(releaseOldCleanup) }) })
	oldSuppressor := &recordingActivitySettingsSuppressor{hook: func(context.Context) {
		enterOldOnce.Do(func() {
			close(oldCleanupEntered)
			<-releaseOldCleanup
		})
	}}
	oldHandler, _ := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, oldSuppressor,
	)
	oldDone := make(chan int, 1)
	go func() {
		oldDone <- invokePresenceSettingsPATCH(oldHandler, senderID, request).Code
	}()
	select {
	case <-oldCleanupEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("old cleanup did not enter")
	}

	newCleanupEntered := make(chan struct{})
	var enterNewOnce sync.Once
	newSuppressor := &recordingActivitySettingsSuppressor{hook: func(context.Context) {
		enterNewOnce.Do(func() { close(newCleanupEntered) })
	}}
	newHandler, _ := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, newSuppressor,
	)
	newDone := make(chan int, 1)
	go func() {
		newDone <- invokePresenceSettingsPATCH(newHandler, senderID, map[string]interface{}{
			"master_enabled": true,
		}).Code
	}()

	concurrentCleanup := false
	select {
	case <-newCleanupEntered:
		concurrentCleanup = true
	case <-time.After(100 * time.Millisecond):
	}
	assert.False(t, concurrentCleanup,
		"a second replica must not replay or supersede a cleanup while the old cleanup is in flight")
	var masterEnabled bool
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&masterEnabled))
	assert.False(t, masterEnabled,
		"a newer activity-policy write must wait for the old durable cleanup")

	releaseOldOnce.Do(func() { close(releaseOldCleanup) })
	select {
	case status := <-oldDone:
		assert.Equal(t, http.StatusOK, status)
	case <-time.After(2 * time.Second):
		t.Fatal("old retry did not finish")
	}
	var newStatus int
	select {
	case newStatus = <-newDone:
		assert.Contains(t, []int{http.StatusOK, http.StatusServiceUnavailable}, newStatus,
			"a cross-replica custom-status claim may require an ordinary retry")
	case <-time.After(2 * time.Second):
		t.Fatal("newer settings write did not finish")
	}
	final := invokePresenceSettingsPATCH(newHandler, senderID, map[string]interface{}{
		"master_enabled": true,
	})
	require.Equal(t, http.StatusOK, final.Code, final.Body.String())

	require.NoError(t, db.QueryRow(`
		SELECT master_enabled FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&masterEnabled))
	assert.True(t, masterEnabled)
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	assert.Zero(t, pending)
}

func TestUpdatePresenceSettingsOrdinarySameValueWriteCreatesNoCleanupWork(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{}
	handler, _ := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"master_enabled":            true,
		"server_voice_tier":         int(presence.TierFriends),
		"server_voice_show_details": true,
		"private_call_tier":         int(presence.TierOff),
		"private_call_show_details": false,
	})

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Empty(t, suppressor.snapshot())
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	assert.Zero(t, pending)
}

func TestUpdatePresenceSettingsMalformedCleanupEvidenceFailsClosedAndRemains(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, '{"version":999}'::jsonb)
	`, senderID, operationID)
	require.NoError(t, err)
	suppressor := &recordingActivitySettingsSuppressor{}
	handler, _ := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"server_voice_tier": int(presence.TierOff),
	})

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Empty(t, suppressor.snapshot())
	var retainedOperationID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&retainedOperationID))
	assert.Equal(t, operationID, retainedOperationID)
}

func TestUpdatePresenceSettingsExpiredRetryStillCompletesDurableCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{
		err: errors.New("activity cleanup unavailable"),
	}
	handler, _ := newActivitySettingsCleanupHandler(t, db, &task9Delivery{}, suppressor)
	request := map[string]interface{}{
		"master_enabled": false,
	}
	first := invokePresenceSettingsPATCH(handler, senderID, request)
	require.Equal(t, http.StatusServiceUnavailable, first.Code)

	requestCtx, cancelRequest := context.WithCancel(context.Background())
	suppressor.err = nil
	suppressor.hook = func(cleanupCtx context.Context) {
		cancelRequest()
		assert.NoError(t, cleanupCtx.Err(),
			"cleanup retry must be detached once it starts under the sender gate")
	}
	retry := invokePresenceSettingsPATCHWithContext(requestCtx, handler, senderID, request)

	assert.Equal(t, http.StatusInternalServerError, retry.Code)
	assert.ErrorIs(t, requestCtx.Err(), context.Canceled)
	calls := suppressor.snapshot()
	require.Len(t, calls, 2)
	assert.Equal(t, calls[0].Before, calls[1].Before)
	assert.Equal(t, calls[0].After, calls[1].After)
	assert.NoError(t, calls[1].ContextErr)
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, senderID).Scan(&pending))
	assert.Zero(t, pending,
		"request expiry after cleanup starts must not resurrect durable evidence")
}

func TestUpdatePresenceSettingsConfirmedCommitRunsCleanupAfterCustomDeliveryFailure(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	deliveryErr := errors.New("custom status delivery unavailable")
	suppressor := &recordingActivitySettingsSuppressor{}
	var logs strings.Builder
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(&task9Delivery{err: deliveryErr}))
	handler := users.NewHandler(db, logger.NewWithWriter(&logs), nil, nil, nil, nil, nil)
	handler.SetPresenceHistory(service)
	handler.SetActivitySettingsSuppressor(suppressor)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"server_voice_show_details": false,
		"custom_text_tier":          1,
		"custom_text":               "committed before delivery failure",
	})

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Contains(t, logs.String(), "error_class=delivery")
	require.Len(t, suppressor.snapshot(), 1,
		"confirmed activity policy must be cleaned even when Custom Status delivery fails")
	var showDetails bool
	require.NoError(t, db.QueryRow(`
		SELECT server_voice_show_details
		FROM user_presence_settings
		WHERE user_id = $1
	`, senderID).Scan(&showDetails))
	assert.False(t, showDetails)
}

func TestUpdatePresenceSettingsRollbackConfirmedSkipsActivityCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	suppressor := &recordingActivitySettingsSuppressor{}
	handler, service := newActivitySettingsCleanupHandler(
		t, db, &task9Delivery{}, suppressor,
	)
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(*sql.Tx) error { return errors.New("commit rejected") },
	})
	t.Cleanup(restore)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"server_voice_show_details": false,
	})

	assert.Equal(t, http.StatusInternalServerError, response.Code)
	assert.Empty(t, suppressor.snapshot())
}

func TestUpdatePresenceSettingsRequiresActivitySettingsSuppressorBeforeMutation(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{}
	handler, _ := newActivitySettingsCleanupHandler(t, db, delivery, nil)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"master_enabled": false,
	})

	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.Empty(t, delivery.snapshot())
	var settingsRows int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&settingsRows))
	assert.Zero(t, settingsRows)
}

func TestUpdatePresenceSettingsCustomTextOnlySkipsActivityCleanup(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{}
	handler, _ := newActivitySettingsCleanupHandler(t, db, delivery, nil)

	response := invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "custom-only update",
	})

	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	assert.Len(t, delivery.snapshot(), 1, "Custom Status delivery still completes under its sender gate")
}
