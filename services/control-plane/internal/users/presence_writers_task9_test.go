package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const task9ConsentHash = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // pragma: allowlist secret

func invokePresenceSettingsPATCH(
	h *users.Handler,
	senderID uuid.UUID,
	body map[string]interface{},
) *httptest.ResponseRecorder {
	return invokePresenceSettingsPATCHWithContext(context.Background(), h, senderID, body)
}

func invokePresenceSettingsPATCHWithContext(
	ctx context.Context,
	h *users.Handler,
	senderID uuid.UUID,
	body map[string]interface{},
) *httptest.ResponseRecorder {
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Set("user_id", senderID.String())
	c.Request = httptest.NewRequest(
		http.MethodPatch,
		"/api/v1/users/me/presence-settings",
		bytes.NewReader(encoded),
	).WithContext(ctx)
	c.Request.Header.Set("Content-Type", "application/json")
	h.UpdatePresenceSettings(c)
	return w
}

func TestUpdatePresenceSettingsCompletesPostCommitClaimAfterRequestCancellation(t *testing.T) {
	for _, test := range []struct {
		name        string
		initialTier int
		nextTier    int
		relation    func(*testing.T, *sql.DB, uuid.UUID, uuid.UUID)
	}{
		{
			name:        "tier narrowing clears revoked server peer",
			initialTier: 2,
			nextTier:    1,
			relation: func(t *testing.T, db *sql.DB, senderID, viewerID uuid.UUID) {
				t.Helper()
				serverID := testhelpers.CreateServer(t, db, senderID)
				testhelpers.AddServerMember(t, db, serverID, senderID)
				testhelpers.AddServerMember(t, db, serverID, viewerID)
			},
		},
		{
			name:        "turning off clears revoked friend",
			initialTier: 1,
			nextTier:    0,
			relation: func(t *testing.T, db *sql.DB, senderID, viewerID uuid.UUID) {
				t.Helper()
				testhelpers.AddFriendship(t, db, senderID, viewerID)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _ := testhelpers.SetupTestDB(t)
			senderID := testhelpers.CreateUser(t, db)
			viewerID := testhelpers.CreateUser(t, db)
			test.relation(t, db, senderID, viewerID)
			_, err := db.Exec(`
				INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
				VALUES ($1, $2, 'visible before cancellation')
			`, senderID, test.initialTier)
			require.NoError(t, err)
			delivery := &task9Delivery{}
			service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
			require.NoError(t, service.BindDelivery(delivery))
			h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
			h.SetPresenceHistory(service)

			requestCtx, cancelRequest := context.WithCancel(context.Background())
			restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
				Commit: func(tx *sql.Tx) error {
					err := tx.Commit()
					cancelRequest()
					return err
				},
			})
			defer restore()

			response := invokePresenceSettingsPATCHWithContext(
				requestCtx, h, senderID, map[string]interface{}{"custom_text_tier": test.nextTier},
			)
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			plans := delivery.snapshot()
			require.Len(t, plans, 1)
			assert.Equal(t, presencehistory.DeliveryExactDelta, plans[0].Mode)
			assert.True(t, plans[0].ClearRecipients[viewerID], "revoked connected viewer must be cleared")
			var pending int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			).Scan(&pending))
			assert.Zero(t, pending)
		})
	}
}

func TestUpdatePresenceSettingsClaimBeginFailureRunsConservativeResetAndRetainsQuarantine(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'visible before failed claim')
	`, senderID)
	require.NoError(t, err)
	delivery := &task9Delivery{}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)
	claimBeginCause := errors.New("claim transaction unavailable")
	beginCalls := 0
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Begin: func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
			beginCalls++
			if beginCalls == 3 {
				return nil, claimBeginCause
			}
			return db.BeginTx(ctx, options)
		},
	})
	defer restore()

	response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"custom_text_tier": 0})
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	plans := delivery.snapshot()
	require.Len(t, plans, 1, "pre-delivery claim failure must perform one conservative reset")
	assert.Equal(t, presencehistory.DeliveryConservativeReset, plans[0].Mode)
	var settingsMarker, pendingMarker uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT settings.presence_settings_operation_id, pending.operation_id
		FROM user_presence_settings AS settings
		JOIN presence_settings_pending_operations AS pending ON pending.user_id = settings.user_id
		WHERE settings.user_id = $1
	`, senderID).Scan(&settingsMarker, &pendingMarker))
	assert.Equal(t, settingsMarker, pendingMarker, "exact committed quarantine must remain retryable")
}

type task9Delivery struct {
	mu    sync.Mutex
	plans []presencehistory.DeliveryPlan
	err   error
}

func (d *task9Delivery) DeliverCustomText(
	_ context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	d.mu.Lock()
	d.plans = append(d.plans, plan)
	d.mu.Unlock()
	if d.err != nil {
		return presencehistory.DeliveryAck{}, d.err
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func (d *task9Delivery) snapshot() []presencehistory.DeliveryPlan {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]presencehistory.DeliveryPlan(nil), d.plans...)
}

func newTask9Handler(
	t *testing.T,
	ts *testhelpers.TestServer,
	delivery *task9Delivery,
) *users.Handler {
	t.Helper()
	h := users.NewHandler(ts.DB, logger.NewWithWriter(io.Discard), nil, nil, nil)
	service := presencehistory.NewService(ts.DB, presencehistory.DisclosureState{
		Available: true,
		RequiredConsent: &presencehistory.RequiredConsent{
			Version:  1,
			CopyHash: task9ConsentHash,
		},
	}, true)
	require.NoError(t, service.BindDelivery(delivery))
	h.SetPresenceHistory(service)
	return h
}

func TestUpdatePresenceSettingsRequiresBoundPresenceHistory(t *testing.T) {
	for _, test := range []struct {
		name       string
		bind       func(*users.Handler, *sql.DB)
		wantStatus int
	}{
		{name: "nil service", wantStatus: http.StatusServiceUnavailable},
		{
			name: "unbound delivery",
			bind: func(handler *users.Handler, db *sql.DB) {
				handler.SetPresenceHistory(
					presencehistory.NewService(db, presencehistory.DisclosureState{}, false),
				)
			},
			wantStatus: http.StatusServiceUnavailable,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, _ := testhelpers.SetupTestDB(t)
			senderID := testhelpers.CreateUser(t, db)
			h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
			if test.bind != nil {
				test.bind(h, db)
			}
			w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
				"custom_text_tier": 1,
				"custom_text":      "must not commit",
			})
			assert.Equal(t, test.wantStatus, w.Code)
			var count int
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, senderID,
			).Scan(&count))
			assert.Zero(t, count)
		})
	}
}

func TestUpdatePresenceSettingsCommitsHistoryAndClaimsBeforeSuccess(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, activity_history_enabled, activity_history_retention_days,
			activity_history_consent_version, activity_history_consent_copy_hash,
			activity_history_consented_at
		) VALUES ($1, TRUE, 30, 1, $2, clock_timestamp())
	`, senderID, task9ConsentHash)
	require.NoError(t, err)
	delivery := &task9Delivery{}
	h := newTask9Handler(t, ts, delivery)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier":  1,
		"custom_text":       "focused",
		"custom_text_emoji": "x",
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, presencehistory.DeliveryExactDelta, plans[0].Mode)
	assert.True(t, plans[0].UpdateRecipients[senderID])
	assert.True(t, plans[0].UpdateRecipients[viewerID])
	require.NotNil(t, plans[0].Payload)
	assert.Equal(t, "focused", plans[0].Payload.Text)
	var pending, history int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&history))
	assert.Zero(t, pending)
	assert.Equal(t, 1, history)
}

func TestUpdatePresenceSettingsDeliveryFailureReturnsRetryableAndQuarantines(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{err: errors.New("delivery unavailable")}
	h := newTask9Handler(t, ts, delivery)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "quarantined",
	})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	var pending int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	assert.Equal(t, 1, pending)
}

func TestReplacePresenceOverridesUsesExactDeltaAndOverrideMetadata(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	removedID := testhelpers.CreateUser(t, db)
	addedID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, removedID)
	testhelpers.AddFriendship(t, db, senderID, addedID)
	_, err := db.Exec(
		`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		 VALUES ($1, 1, 'focused')`, senderID,
	)
	require.NoError(t, err)
	seedOverridePreference(t, db, senderID, "b2xk", 1, addedID)
	delivery := &task9Delivery{}
	h := newTask9Handler(t, ts, delivery)

	w := invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
		EncryptedData:   "bmV3",
		ExpectedVersion: 1,
		ExcludedUserIDs: []string{removedID.String()},
	})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	require.NotNil(t, plans[0].OverrideVersion)
	assert.Equal(t, 2, *plans[0].OverrideVersion)
	assert.True(t, plans[0].ClearRecipients[removedID])
	assert.True(t, plans[0].UpdateRecipients[addedID])
	assert.False(t, plans[0].ClearRecipients[senderID])
	assert.False(t, plans[0].UpdateRecipients[senderID])
	var history int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&history))
	assert.Zero(t, history)
}

func TestPresenceWriterValidationPrecedesPresenceService(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)

	settings := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 9,
	})
	overrides := invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
		EncryptedData: "not-base64",
	})

	assert.Equal(t, http.StatusBadRequest, settings.Code)
	assert.Equal(t, http.StatusBadRequest, overrides.Code)
}

func TestUpdatePresenceSettingsPreservesTierAudienceSemantics(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	friendID := testhelpers.CreateUser(t, db)
	serverPeerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, friendID)
	serverID := testhelpers.CreateServer(t, db, senderID)
	testhelpers.AddServerMember(t, db, serverID, senderID)
	testhelpers.AddServerMember(t, db, serverID, serverPeerID)
	delivery := &task9Delivery{}
	h := newTask9Handler(t, ts, delivery)

	friends := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "friends only",
	})
	require.Equal(t, http.StatusOK, friends.Code, friends.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.True(t, plans[0].UpdateRecipients[senderID], "sender self-sync is required")
	assert.True(t, plans[0].UpdateRecipients[friendID])
	assert.False(t, plans[0].UpdateRecipients[serverPeerID])

	servers := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 2,
	})
	require.Equal(t, http.StatusOK, servers.Code, servers.Body.String())
	plans = delivery.snapshot()
	require.Len(t, plans, 2)
	assert.True(t, plans[1].UpdateRecipients[serverPeerID])

	narrow := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
	})
	require.Equal(t, http.StatusOK, narrow.Code, narrow.Body.String())
	plans = delivery.snapshot()
	require.Len(t, plans, 3)
	assert.True(t, plans[2].ClearRecipients[serverPeerID])
	assert.True(t, plans[2].UpdateRecipients[friendID])

	off := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 0,
	})
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())
	plans = delivery.snapshot()
	require.Len(t, plans, 4)
	assert.True(t, plans[3].ClearRecipients[senderID])
	assert.True(t, plans[3].ClearRecipients[friendID])
	assert.Empty(t, plans[3].UpdateRecipients)
	assert.Nil(t, plans[3].Payload)
}

func TestUpdatePresenceSettings_MasterOffClearsAudienceAndPreservesSavedStatusAndHistory(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, activity_history_enabled, activity_history_retention_days,
			activity_history_consent_version, activity_history_consent_copy_hash,
			activity_history_consented_at
		) VALUES ($1, TRUE, 30, 1, $2, clock_timestamp())
	`, senderID, task9ConsentHash)
	require.NoError(t, err)
	delivery := &task9Delivery{}
	h := newTask9Handler(t, ts, delivery)

	set := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier":  1,
		"custom_text":       "saved through master toggles",
		"custom_text_emoji": "lock",
	})
	require.Equal(t, http.StatusOK, set.Code, set.Body.String())
	var originalHistoryID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT id FROM presence_history
		WHERE sender_id = $1 AND ended_at IS NULL
	`, senderID).Scan(&originalHistoryID))

	off := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": false})
	require.Equal(t, http.StatusOK, off.Code, off.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 2)
	plan := plans[1]
	assert.Equal(t, presencehistory.DeliveryExactDelta, plan.Mode)
	assert.True(t, plan.ClearRecipients[senderID])
	assert.True(t, plan.ClearRecipients[viewerID])
	assert.Empty(t, plan.UpdateRecipients)
	assert.Nil(t, plan.Payload)

	var master bool
	var tier int
	var text, emoji string
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled, custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&master, &tier, &text, &emoji))
	assert.False(t, master)
	assert.Equal(t, 1, tier)
	assert.Equal(t, "saved through master toggles", text)
	assert.Equal(t, "lock", emoji)
	var historyID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT id FROM presence_history
		WHERE sender_id = $1 AND ended_at IS NULL
	`, senderID).Scan(&historyID))
	assert.Equal(t, originalHistoryID, historyID, "master-only off must not close Custom Status history")

	on := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": true})
	require.Equal(t, http.StatusOK, on.Code, on.Body.String())
	require.NoError(t, db.QueryRow(`
		SELECT id FROM presence_history
		WHERE sender_id = $1 AND ended_at IS NULL
	`, senderID).Scan(&historyID))
	assert.Equal(t, originalHistoryID, historyID, "master-only on must not reopen Custom Status history")
}

func TestUpdatePresenceSettings_MasterOnRecomputesCurrentAudience(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	formerFriendID := testhelpers.CreateUser(t, db)
	currentFriendID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, formerFriendID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, custom_text_tier, custom_text
		) VALUES ($1, FALSE, 1, 'saved while master is off')
	`, senderID)
	require.NoError(t, err)
	_, err = db.Exec(
		`DELETE FROM friendships WHERE requester_id = $1 AND addressee_id = $2`,
		senderID, formerFriendID,
	)
	require.NoError(t, err)
	testhelpers.AddFriendship(t, db, senderID, currentFriendID)
	delivery := &task9Delivery{}
	h := newTask9Handler(t, ts, delivery)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": true})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.False(t, plans[0].UpdateRecipients[formerFriendID])
	assert.True(t, plans[0].UpdateRecipients[currentFriendID])
	assert.True(t, plans[0].UpdateRecipients[senderID])
	assert.Empty(t, plans[0].ClearRecipients)
	require.NotNil(t, plans[0].Payload)
	assert.Equal(t, "saved while master is off", plans[0].Payload.Text)
}

func TestUpdatePresenceSettings_MasterOffSupersedesUnexpiredOrdinaryPendingConservatively(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, custom_text_tier, custom_text
		) VALUES ($1, TRUE, 1, 'saved across supersession')
	`, senderID)
	require.NoError(t, err)
	delivery := &task9Delivery{}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	ordinaryTx, err := service.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	_, err = service.BeginAudienceOperation(
		context.Background(), ordinaryTx, senderID, presencehistory.OrdinaryAudienceWrite,
	)
	require.NoError(t, err)
	require.NoError(t, service.CommitTx(ordinaryTx))
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": false})

	require.Equal(t, http.StatusOK, w.Code, w.Body.String())
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, plans[0].Mode)
	assert.Nil(t, plans[0].ClearRecipients)
	assert.Nil(t, plans[0].UpdateRecipients)
	assert.Nil(t, plans[0].Payload)
	var master bool
	var tier int
	var text string
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled, custom_text_tier, custom_text
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&master, &tier, &text))
	assert.False(t, master)
	assert.Equal(t, 1, tier)
	assert.Equal(t, "saved across supersession", text)
}

func TestUpdatePresenceSettings_MasterOffSupersedesEligiblePendingWhenDeliveryFails(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	oldOperationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id,
			master_enabled,
			custom_text_tier,
			custom_text,
			presence_settings_version,
			presence_settings_operation_id
		) VALUES ($1, TRUE, 0, 'saved while tier is off', 1, $2)
	`, senderID, oldOperationID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id,
			operation_id,
			prior_settings_version,
			created_at,
			reconcile_after
		) VALUES ($1, $2, 0, clock_timestamp() - INTERVAL '1 minute', clock_timestamp() - INTERVAL '1 second')
	`, senderID, oldOperationID)
	require.NoError(t, err)
	delivery := &task9Delivery{err: errors.New("delivery unavailable")}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": false})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, plans[0].Mode)
	assert.NotEqual(t, oldOperationID, plans[0].OperationID)
	var master bool
	var tier int
	var text string
	var version, priorVersion int64
	var settingsOperationID, pendingOperationID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT settings.master_enabled,
		       settings.custom_text_tier,
		       settings.custom_text,
		       settings.presence_settings_version,
		       settings.presence_settings_operation_id,
		       pending.operation_id,
		       pending.prior_settings_version
		FROM user_presence_settings AS settings
		JOIN presence_settings_pending_operations AS pending ON pending.user_id = settings.user_id
		WHERE settings.user_id = $1
	`, senderID).Scan(
		&master,
		&tier,
		&text,
		&version,
		&settingsOperationID,
		&pendingOperationID,
		&priorVersion,
	))
	assert.False(t, master, "forced master-off must close the durable gate before returning")
	assert.Equal(t, 0, tier)
	assert.Equal(t, "saved while tier is off", text)
	assert.Equal(t, int64(2), version)
	assert.NotEqual(t, oldOperationID, settingsOperationID)
	assert.Equal(t, settingsOperationID, pendingOperationID)
	assert.Equal(t, plans[0].OperationID, pendingOperationID)
	assert.Equal(t, int64(1), priorVersion)
}

func TestUpdatePresenceSettings_MasterOffDeliveryFailureReturns503AndRetainsQuarantine(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, master_enabled, custom_text_tier, custom_text
		) VALUES ($1, TRUE, 1, 'saved despite delivery failure')
	`, senderID)
	require.NoError(t, err)
	delivery := &task9Delivery{err: errors.New("master-off delivery failed")}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	w := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{"master_enabled": false})

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	var pending int
	var master bool
	var text string
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled, custom_text FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&master, &text))
	assert.Equal(t, 1, pending)
	assert.False(t, master)
	assert.Equal(t, "saved despite delivery failure", text)
}

func TestUpdatePresenceSettingsHistorySemanticTransitionsAndNoOps(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, activity_history_enabled, activity_history_retention_days,
			activity_history_consent_version, activity_history_consent_copy_hash,
			activity_history_consented_at
		) VALUES ($1, TRUE, 30, 1, $2, clock_timestamp())
	`, senderID, task9ConsentHash)
	require.NoError(t, err)
	h := newTask9Handler(t, ts, &task9Delivery{})

	for _, body := range []map[string]interface{}{
		{"custom_text_tier": 1, "custom_text": "first", "custom_text_emoji": "a"},
		{"custom_text_tier": 2},
		{"custom_text": "first", "custom_text_emoji": "a"},
		{"custom_text": "second", "custom_text_emoji": "b"},
		{"custom_text": "", "custom_text_emoji": ""},
	} {
		response := invokePresenceSettingsPATCH(h, senderID, body)
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	}

	var count, open int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*), COUNT(*) FILTER (WHERE ended_at IS NULL)
		 FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&count, &open))
	assert.Equal(t, 2, count, "tier-only and semantic-idempotent writes must not add intervals")
	assert.Zero(t, open, "clearing text closes the current interval")
}

func TestUpdatePresenceSettingsDisabledHistoryInsertsNothing(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	h := newTask9Handler(t, ts, &task9Delivery{})

	response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "not retained",
	})
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	var count int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&count))
	assert.Zero(t, count)
}

func TestUpdatePresenceSettingsRecorderFailureRollsBackSettingsMarkerAndHistory(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		RecordTransition: func(
			context.Context,
			*sql.Tx,
			uuid.UUID,
			presencehistory.CustomTextState,
			presencehistory.CustomTextState,
		) error {
			return errors.New("recorder failure")
		},
	})
	t.Cleanup(restore)
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "must roll back",
	})
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	var settings, pending, history int
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&settings))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&history))
	assert.Zero(t, settings)
	assert.Zero(t, pending)
	assert.Zero(t, history)
	assert.Empty(t, delivery.snapshot())
}

func TestUpdatePresenceSettingsMainCommitClassification(t *testing.T) {
	t.Run("committed then error still claims before success", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		delivery := &task9Delivery{}
		service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		commitCalls := 0
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				commitCalls++
				if commitCalls == 1 {
					require.NoError(t, tx.Commit())
					return errors.New("ambiguous committed write")
				}
				return tx.Commit()
			},
		})
		t.Cleanup(restore)
		h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
		h.SetPresenceHistory(service)
		response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
			"custom_text_tier": 1,
			"custom_text":      "committed",
		})
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
		require.Len(t, delivery.snapshot(), 1)
	})

	t.Run("confirmed rollback sends no frame or success", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		delivery := &task9Delivery{}
		service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Commit: func(*sql.Tx) error { return errors.New("commit rejected") },
		})
		t.Cleanup(restore)
		h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
		h.SetPresenceHistory(service)
		response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
			"custom_text_tier": 1,
			"custom_text":      "rolled back",
		})
		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Empty(t, delivery.snapshot())
	})

	t.Run("superseded write sends no frame or success", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		senderID := testhelpers.CreateUser(t, db)
		delivery := &task9Delivery{}
		service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
		require.NoError(t, service.BindDelivery(delivery))
		laterOperationID := uuid.New()
		restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
			Commit: func(tx *sql.Tx) error {
				require.NoError(t, tx.Rollback())
				_, err := db.Exec(`
					INSERT INTO user_presence_settings (
						user_id, presence_settings_version, presence_settings_operation_id
					) VALUES ($1, 1, $2)
				`, senderID, laterOperationID)
				require.NoError(t, err)
				return errors.New("superseded commit result")
			},
		})
		t.Cleanup(restore)
		h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
		h.SetPresenceHistory(service)
		response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
			"custom_text_tier": 1,
			"custom_text":      "stale write",
		})
		assert.Equal(t, http.StatusInternalServerError, response.Code)
		assert.Empty(t, delivery.snapshot())
		var durableOperationID uuid.UUID
		require.NoError(t, db.QueryRow(
			`SELECT presence_settings_operation_id FROM user_presence_settings WHERE user_id = $1`, senderID,
		).Scan(&durableOperationID))
		assert.Equal(t, laterOperationID, durableOperationID)
	})
}

func TestUpdatePresenceSettingsUnexpiredPendingReturnsRetryAfterWithoutMutation(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	operationID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text,
			presence_settings_version, presence_settings_operation_id
		) VALUES ($1, 1, 'existing', 1, $2)
	`, senderID, operationID)
	require.NoError(t, err)
	_, err = db.Exec(`
		INSERT INTO presence_settings_pending_operations (
			user_id, operation_id, prior_settings_version, created_at, reconcile_after
		) VALUES ($1, $2, 0, clock_timestamp(), clock_timestamp() + INTERVAL '30 seconds')
	`, senderID, operationID)
	require.NoError(t, err)
	delivery := &task9Delivery{}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text": "must wait",
	})
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	assert.NotEmpty(t, response.Header().Get("Retry-After"))
	var text string
	require.NoError(t, db.QueryRow(
		`SELECT custom_text FROM user_presence_settings WHERE user_id = $1`, senderID,
	).Scan(&text))
	assert.Equal(t, "existing", text)
	assert.Empty(t, delivery.snapshot())
}

func TestReplacePresenceOverridesDeliveryFailureRetainsCommittedQuarantine(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	ts := &testhelpers.TestServer{DB: db}
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{err: errors.New("delivery failed")}
	h := newTask9Handler(t, ts, delivery)

	response := invokePresenceOverridePUT(t, h, senderID, presenceOverridePUTBody{
		EncryptedData:   "YQ==",
		ExpectedVersion: 0,
		ExcludedUserIDs: []string{},
	})
	assert.Equal(t, http.StatusServiceUnavailable, response.Code)
	var overrideVersion, pending int
	require.NoError(t, db.QueryRow(
		`SELECT version FROM presence_override_preferences
		 WHERE user_id = $1 AND category = 'custom_text'`, senderID,
	).Scan(&overrideVersion))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	assert.Equal(t, 1, overrideVersion)
	assert.Equal(t, 1, pending)
}

func TestUpdatePresenceSettingsUnresolvedCommitRunsMarkerPreservingEmergencyReset(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	delivery := &task9Delivery{}
	service := presencehistory.NewService(db, presencehistory.DisclosureState{}, false)
	require.NoError(t, service.BindDelivery(delivery))
	commitCalls := 0
	restore := service.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			commitCalls++
			if commitCalls != 1 {
				return tx.Commit()
			}
			require.NoError(t, tx.Commit())
			_, err := db.Exec(
				`DELETE FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			)
			require.NoError(t, err)
			return errors.New("unresolved committed write")
		},
	})
	t.Cleanup(restore)
	h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	h.SetPresenceHistory(service)

	response := invokePresenceSettingsPATCH(h, senderID, map[string]interface{}{
		"custom_text_tier": 1,
		"custom_text":      "uncertain",
	})
	assert.Equal(t, http.StatusInternalServerError, response.Code)
	plans := delivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, presencehistory.DeliveryConservativeReset, plans[0].Mode)
}

func TestPresenceSettingsDirectErrorBoundaries(t *testing.T) {
	t.Run("database failure", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		cleanup()
		h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", uuid.NewString())
		h.GetPresenceSettings(c)
		assert.Equal(t, http.StatusInternalServerError, w.Code)
	})
	t.Run("malformed authenticated id", func(t *testing.T) {
		db, _ := testhelpers.SetupTestDB(t)
		h := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", "not-a-uuid")
		c.Request = httptest.NewRequest(
			http.MethodPatch,
			"/api/v1/users/me/presence-settings",
			bytes.NewBufferString(`{"custom_text_tier":1}`),
		)
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdatePresenceSettings(c)
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})
}

var _ presencehistory.Delivery = (*task9Delivery)(nil)
var _ = uuid.Nil
