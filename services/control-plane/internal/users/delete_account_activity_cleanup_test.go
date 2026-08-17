package users

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/redistest"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type accountActivityDelivery struct {
	disconnectAllCalls int
}

func (*accountActivityDelivery) DeliverRichPresence(
	context.Context,
	presence.DeliveryPlan,
) error {
	return nil
}

func (*accountActivityDelivery) DisconnectRichPresenceClients(
	context.Context,
	map[uuid.UUID]bool,
) error {
	return nil
}

func (d *accountActivityDelivery) DisconnectAllRichPresenceClients(context.Context) error {
	d.disconnectAllCalls++
	return nil
}

func TestDeleteAccountResumesPendingActivityCleanupBeforeErasure(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	_ = insertPendingAccountActivityCleanup(t, db, userID)
	suppressor := &replayActivitySettingsSuppressor{}
	service := configuredAccountServiceForCleanup(t, db, suppressor)

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Equal(t, 1, suppressor.callCount())
	assert.Equal(t, 1, suppressor.accountCallCount())
	assertAccountActivityCleanupRows(t, db, userID, 0, 0)
	var audits int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM account_deletions WHERE user_id IS NULL
	`).Scan(&audits))
	assert.Equal(t, 1, audits)
}

func TestDeleteAccountCleanupFailureRetainsUserAndMarker(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	_ = insertPendingAccountActivityCleanup(t, db, userID)
	suppressor := &replayActivitySettingsSuppressor{
		err: errors.New("activity cleanup unavailable"),
	}
	service := configuredAccountServiceForCleanup(t, db, suppressor)

	err := service.DeleteAccount(context.Background(), userID.String())

	require.Error(t, err)
	assert.Equal(t, 1, suppressor.callCount())
	assert.Zero(t, suppressor.accountCallCount())
	assertAccountActivityCleanupRows(t, db, userID, 1, 1)
}

func TestDeleteAccountSuppressesActiveActivityWithoutPendingMarker(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	suppressor := &replayActivitySettingsSuppressor{}
	service := configuredAccountServiceForCleanup(t, db, suppressor)

	require.NoError(t, service.DeleteAccount(context.Background(), userID.String()))

	assert.Zero(t, suppressor.callCount())
	assert.Equal(t, 1, suppressor.accountCallCount())
	assertAccountActivityCleanupRows(t, db, userID, 0, 0)
}

func TestDeleteAccountDeletesActiveActivityStateWithoutPendingMarker(t *testing.T) {
	db, dbCleanup := testdb.SetupTestDB(t)
	redisClient, redisCleanup := setupAccountActivityRedis(t)
	t.Cleanup(dbCleanup)
	t.Cleanup(redisCleanup)
	ctx := context.Background()
	userID := testdb.CreateUser(t, db)
	store := presence.NewActivityStore(redisClient)
	state := presence.ActivityState{
		SourceToken: uuid.New(), SourceVersion: time.Now().UTC().UnixMicro(),
		Payload: json.RawMessage(`{"active":true}`), UpdatedAt: time.Now().UTC().Unix(),
	}
	for _, category := range []presence.Category{
		presence.CategoryServerVoice,
		presence.CategoryPrivateCall,
	} {
		stored, err := store.CompareAndSet(ctx, userID, category, state)
		require.NoError(t, err)
		require.True(t, stored)
	}
	delivery := &accountActivityDelivery{}
	activityService := presence.NewActivityService(
		nil, nil, store, nil, nil, delivery, permitAllPresence{},
	)
	service := configuredAccountServiceForCleanup(t, db, activityService)

	require.NoError(t, service.DeleteAccount(ctx, userID.String()))

	for _, category := range []presence.Category{
		presence.CategoryServerVoice,
		presence.CategoryPrivateCall,
	} {
		_, found, err := store.Get(ctx, userID, category)
		require.NoError(t, err)
		assert.False(t, found)
	}
	assert.Equal(t, 1, delivery.disconnectAllCalls)
	assertAccountActivityCleanupRows(t, db, userID, 0, 0)
}

// setupAccountActivityRedis returns a client on this process's own Redis logical
// database, allocated by redistest (#2680). It no longer demands a hand-set
// REDIS_URL: the isolation the old require.NotEmpty was asking an operator to
// arrange is now structural, and unset is the normal local case.
func setupAccountActivityRedis(t *testing.T) (*redis.Client, func()) {
	t.Helper()
	client := redistest.Client(t)
	require.NoError(t, redistest.Reset(context.Background(), client))
	return client, func() { assert.NoError(t, client.Close()) }
}

func TestDeleteAccountActivitySuppressionFailureRetainsUser(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	suppressErr := errors.New("forced account activity suppression failure")
	suppressor := &replayActivitySettingsSuppressor{accountErr: suppressErr}
	service := configuredAccountServiceForCleanup(t, db, suppressor)

	err := service.DeleteAccount(context.Background(), userID.String())

	require.ErrorIs(t, err, suppressErr)
	assert.Zero(t, suppressor.callCount())
	assert.Equal(t, 1, suppressor.accountCallCount())
	assertAccountActivityCleanupRows(t, db, userID, 1, 0)
}

func TestDeleteAccountRestrictPreservesDirectDeleteRaceWithPendingMarker(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	_ = insertPendingAccountActivityCleanup(t, db, userID)
	service := NewAccountService(db, logger.New("test"))

	err := service.DeleteAccount(context.Background(), userID.String())

	require.Error(t, err)
	assertAccountActivityCleanupRows(t, db, userID, 1, 1)
}

func TestDeleteAccountRetainsUserWhenConcurrentCleanupMarkerWins(t *testing.T) {
	db, cleanup := testdb.SetupTestDB(t)
	t.Cleanup(cleanup)
	userID := testdb.CreateUser(t, db)
	_ = insertPendingAccountActivityCleanup(t, db, userID)
	suppressionEntered := make(chan struct{})
	releaseSuppression := make(chan struct{})
	suppressor := &replayActivitySettingsSuppressor{hook: func() {
		close(suppressionEntered)
		<-releaseSuppression
	}}
	service := configuredAccountServiceForCleanup(t, db, suppressor)
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- service.DeleteAccount(context.Background(), userID.String())
	}()
	select {
	case <-suppressionEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("account cleanup did not reach suppression")
	}

	conn, err := db.Conn(context.Background())
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, conn.Close()) })
	var backendPID int
	require.NoError(t, conn.QueryRowContext(
		context.Background(), `SELECT pg_backend_pid()`,
	).Scan(&backendPID))
	successorID := uuid.New()
	evidence, err := encodeActivitySettingsCleanupEvidence(
		presence.ActivityPolicySettings{
			MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
		},
		presence.ActivityPolicySettings{
			MasterEnabled: false, ServerVoiceTier: presence.TierOff,
		},
	)
	require.NoError(t, err)
	upsertDone := make(chan error, 1)
	go func() {
		_, upsertErr := conn.ExecContext(context.Background(), `
			INSERT INTO activity_settings_pending_cleanups (
				user_id, operation_id, evidence
			) VALUES ($1, $2, $3)
			ON CONFLICT (user_id) DO UPDATE SET
				operation_id = EXCLUDED.operation_id,
				evidence = EXCLUDED.evidence,
				updated_at = clock_timestamp()
		`, userID, successorID, string(evidence))
		upsertDone <- upsertErr
	}()
	waitForActivityCleanupLock(t, db, backendPID)
	close(releaseSuppression)

	require.NoError(t, <-upsertDone)
	require.Error(t, <-deleteDone)
	assertAccountActivityCleanupRows(t, db, userID, 1, 1)
	var retainedOperationID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT operation_id FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&retainedOperationID))
	assert.Equal(t, successorID, retainedOperationID)
}

func configuredAccountServiceForCleanup(
	t *testing.T,
	db *sql.DB,
	suppressor ActivitySettingsSuppressor,
) *AccountService {
	t.Helper()
	coordinator := presencehistory.NewService(
		db, presencehistory.DisclosureState{}, false,
	)
	handler := &Handler{
		db: db, presenceHistory: coordinator, activitySuppressor: suppressor,
	}
	service := NewAccountService(db, logger.New("test"))
	configurer, ok := any(service).(interface {
		SetActivitySettingsCleanupHandler(*Handler)
	})
	require.True(t, ok,
		"account deletion must accept the shared sender-gated cleanup handler")
	configurer.SetActivitySettingsCleanupHandler(handler)
	return service
}

func insertPendingAccountActivityCleanup(t *testing.T, db *sql.DB, userID uuid.UUID) uuid.UUID {
	t.Helper()
	evidence, err := encodeActivitySettingsCleanupEvidence(
		presence.ActivityPolicySettings{
			MasterEnabled: true, ServerVoiceTier: presence.TierFriends,
		},
		presence.ActivityPolicySettings{
			MasterEnabled: false, ServerVoiceTier: presence.TierOff,
		},
	)
	require.NoError(t, err)
	operationID := uuid.New()
	_, err = db.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, $3)
	`, userID, operationID, string(evidence))
	require.NoError(t, err)
	return operationID
}

func waitForActivityCleanupLock(t *testing.T, db *sql.DB, backendPID int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		var waiting bool
		require.NoError(t, db.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM pg_stat_activity
				WHERE pid = $1 AND wait_event_type = 'Lock'
			)
		`, backendPID).Scan(&waiting))
		if waiting {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("concurrent marker did not block behind cleanup receipt write")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func assertAccountActivityCleanupRows(
	t *testing.T,
	db *sql.DB,
	userID uuid.UUID,
	wantUsers, wantMarkers int,
) {
	t.Helper()
	var usersCount, markerCount int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, userID,
	).Scan(&usersCount))
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, userID).Scan(&markerCount))
	assert.Equal(t, wantUsers, usersCount)
	assert.Equal(t, wantMarkers, markerCount)
}
