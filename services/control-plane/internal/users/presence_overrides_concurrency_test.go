package users_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presencehistory"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/users"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type gatedPresenceDelivery struct {
	once    sync.Once
	entered chan presencehistory.DeliveryPlan
	release chan struct{}
}

func (d *gatedPresenceDelivery) DeliverCustomText(
	ctx context.Context,
	plan presencehistory.DeliveryPlan,
) (presencehistory.DeliveryAck, error) {
	blocked := false
	d.once.Do(func() {
		blocked = true
		d.entered <- plan
	})
	if blocked {
		select {
		case <-d.release:
		case <-ctx.Done():
			return presencehistory.DeliveryAck{}, ctx.Err()
		}
	}
	return presencehistory.DeliveryAck{OperationID: plan.OperationID}, nil
}

func TestPresenceWritersSerializeAcrossServicesThroughClaimAcknowledgement(t *testing.T) {
	for _, firstWriter := range []string{"settings", "override"} {
		t.Run(firstWriter+" first", func(t *testing.T) {
			db, _ := testhelpers.SetupTestDB(t)
			senderID := testhelpers.CreateUser(t, db)
			viewerID := testhelpers.CreateUser(t, db)
			testhelpers.AddFriendship(t, db, senderID, viewerID)
			_, err := db.Exec(
				`INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
				 VALUES ($1, 1, 'focused')`, senderID,
			)
			require.NoError(t, err)

			delivery := &gatedPresenceDelivery{
				entered: make(chan presencehistory.DeliveryPlan, 1),
				release: make(chan struct{}),
			}
			firstHandler, _ := newConcurrencyHandler(t, db, delivery)
			secondHandler, _ := newConcurrencyHandler(t, db, immediatePresenceDelivery{})

			firstDone := make(chan int, 1)
			go func() {
				if firstWriter == "settings" {
					firstDone <- invokePresenceSettingsPATCH(firstHandler, senderID, map[string]interface{}{
						"custom_text": "settings-first",
					}).Code
					return
				}
				firstDone <- invokePresenceOverridePUT(t, firstHandler, senderID, presenceOverridePUTBody{
					EncryptedData:   "YQ==",
					ExpectedVersion: 0,
					ExcludedUserIDs: []string{viewerID.String()},
				}).Code
			}()

			var firstPlan presencehistory.DeliveryPlan
			select {
			case firstPlan = <-delivery.entered:
			case <-time.After(10 * time.Second):
				t.Fatal("first writer never reached acknowledged delivery")
			}
			assert.Equal(t, presencehistory.DeliveryExactDelta, firstPlan.Mode)

			secondDone := make(chan int, 1)
			go func() {
				if firstWriter == "settings" {
					secondDone <- invokePresenceOverridePUT(t, secondHandler, senderID, presenceOverridePUTBody{
						EncryptedData:   "Yg==",
						ExpectedVersion: 0,
						ExcludedUserIDs: []string{viewerID.String()},
					}).Code
					return
				}
				secondDone <- invokePresenceSettingsPATCH(secondHandler, senderID, map[string]interface{}{
					"custom_text": "settings-second",
				}).Code
			}()

			assertWriterStillBlocked(t, secondDone, "cross-Service writer escaped the claim locks")
			close(delivery.release)
			assertWriterStatus(t, firstDone, http.StatusOK)
			assertWriterStatus(t, secondDone, http.StatusOK)

			var version int64
			var pending int
			require.NoError(t, db.QueryRow(
				`SELECT presence_settings_version FROM user_presence_settings WHERE user_id = $1`, senderID,
			).Scan(&version))
			require.NoError(t, db.QueryRow(
				`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
			).Scan(&pending))
			assert.Equal(t, int64(2), version)
			assert.Zero(t, pending)
		})
	}
}

func TestPresenceWritersSerializeMasterOffAcrossServices(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := testhelpers.CreateUser(t, db)
	viewerID := testhelpers.CreateUser(t, db)
	testhelpers.AddFriendship(t, db, senderID, viewerID)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (user_id, custom_text_tier, custom_text)
		VALUES ($1, 1, 'visible before concurrent updates')
	`, senderID)
	require.NoError(t, err)

	firstDelivery := &gatedPresenceDelivery{
		entered: make(chan presencehistory.DeliveryPlan, 1),
		release: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseFirstDelivery := func() {
		releaseOnce.Do(func() { close(firstDelivery.release) })
	}
	t.Cleanup(releaseFirstDelivery)
	firstPIDs := make(chan int, 8)
	secondPIDs := make(chan int, 8)
	firstHandler, firstService := newConcurrencyHandler(t, db, firstDelivery)
	finalDelivery := &task9Delivery{}
	secondHandler, secondService := newConcurrencyHandler(t, db, finalDelivery)
	restoreFirst := firstService.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Begin: beginConcurrencyTxWithPID(db, firstPIDs),
	})
	defer restoreFirst()
	restoreSecond := secondService.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Begin: beginConcurrencyTxWithPID(db, secondPIDs),
	})
	defer restoreSecond()

	firstDone := make(chan int, 1)
	go func() {
		firstDone <- invokePresenceSettingsPATCH(firstHandler, senderID, map[string]interface{}{
			"custom_text": "ordinary write before master-off",
		}).Code
	}()

	var firstPlan presencehistory.DeliveryPlan
	select {
	case firstPlan = <-firstDelivery.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("ordinary writer never reached acknowledged delivery")
	}
	assert.Equal(t, presencehistory.DeliveryExactDelta, firstPlan.Mode)
	firstClaimPID := latestConcurrencyBackendPID(t, firstPIDs)

	var firstVersion int64
	var firstOperationID uuid.UUID
	var pending int
	require.NoError(t, db.QueryRow(`
		SELECT presence_settings_version, presence_settings_operation_id
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&firstVersion, &firstOperationID))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	assert.Equal(t, int64(1), firstVersion)
	assert.Equal(t, firstPlan.OperationID, firstOperationID)
	assert.Equal(t, 1, pending)

	secondDone := make(chan int, 1)
	go func() {
		secondDone <- invokePresenceSettingsPATCH(secondHandler, senderID, map[string]interface{}{
			"master_enabled": false,
		}).Code
	}()
	secondPID := waitForConcurrencyBackendPID(t, secondPIDs)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	blockedPID := waitForBlockedConcurrencyBackend(ctx, t, db, firstClaimPID)
	require.Equal(t, secondPID, blockedPID, "master-off writer must wait on the first delivery claim")
	assertWriterStillBlocked(t, secondDone, "master-off escaped sender serialization")

	releaseFirstDelivery()
	assertWriterStatus(t, firstDone, http.StatusOK)
	assertWriterStatus(t, secondDone, http.StatusOK)

	var masterEnabled bool
	var finalVersion int64
	var finalOperationID uuid.UUID
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled, presence_settings_version, presence_settings_operation_id
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&masterEnabled, &finalVersion, &finalOperationID))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, senderID,
	).Scan(&pending))
	assert.False(t, masterEnabled)
	assert.Equal(t, firstVersion+1, finalVersion)
	assert.NotEqual(t, firstOperationID, finalOperationID)
	assert.Zero(t, pending)

	plans := finalDelivery.snapshot()
	require.Len(t, plans, 1)
	assert.Equal(t, finalOperationID, plans[0].OperationID)
	assert.Equal(t, presencehistory.DeliveryExactDelta, plans[0].Mode)
	assert.True(t, plans[0].ClearRecipients[senderID])
	assert.True(t, plans[0].ClearRecipients[viewerID])
	assert.Empty(t, plans[0].UpdateRecipients)
	assert.Nil(t, plans[0].Payload)
}

func TestPresenceWriterAndHistoryDisableConvergeInBothLockOrders(t *testing.T) {
	t.Run("writer first", testPresenceWriterBeforeHistoryDisable)
	t.Run("disable first", testHistoryDisableBeforePresenceWriter)
}

func testPresenceWriterBeforeHistoryDisable(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := seedEnabledHistoryStatus(t, db, "before writer")
	handler, writerService := newConcurrencyHandler(t, db, immediatePresenceDelivery{})
	_, disableService := newConcurrencyHandler(t, db, immediatePresenceDelivery{})

	commitEntered := make(chan struct{})
	commitRelease := make(chan struct{})
	var once sync.Once
	restore := writerService.SetTransactionTestHooks(presencehistory.TransactionTestHooks{
		Commit: func(tx *sql.Tx) error {
			blocked := false
			once.Do(func() {
				blocked = true
				close(commitEntered)
			})
			if blocked {
				<-commitRelease
			}
			return tx.Commit()
		},
	})
	defer restore()

	writerDone := make(chan int, 1)
	go func() {
		writerDone <- invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
			"custom_text": "writer won lock",
		}).Code
	}()
	select {
	case <-commitEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("presence writer never reached its main commit boundary")
	}

	disableDone := make(chan error, 1)
	go func() { disableDone <- disableService.DisableAndDelete(context.Background(), senderID) }()
	assertOperationStillBlocked(t, disableDone, "history disable escaped writer-held canonical locks")
	close(commitRelease)
	assertWriterStatus(t, writerDone, http.StatusOK)
	assertOperationSuccess(t, disableDone)
	assertDisabledHistoryWithLiveText(t, db, senderID, "writer won lock")
}

func testHistoryDisableBeforePresenceWriter(t *testing.T) {
	db, _ := testhelpers.SetupTestDB(t)
	senderID := seedEnabledHistoryStatus(t, db, "before disable")
	historyID := seedOpenHistoryRow(t, db, senderID, "before disable")
	disableService := presencehistory.NewService(db, task9Disclosure(), true)
	handler, _ := newConcurrencyHandler(t, db, immediatePresenceDelivery{})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	blocker, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = blocker.Rollback() })
	var lockedID uuid.UUID
	require.NoError(t, blocker.QueryRowContext(ctx,
		`SELECT id FROM presence_history WHERE id = $1 FOR UPDATE`, historyID,
	).Scan(&lockedID))
	blockerPID := concurrencyBackendPID(ctx, t, blocker)

	disableDone := make(chan error, 1)
	go func() { disableDone <- disableService.DisableAndDelete(ctx, senderID) }()
	disablePID := waitForBlockedConcurrencyBackend(ctx, t, db, blockerPID)

	writerDone := make(chan int, 1)
	go func() {
		writerDone <- invokePresenceSettingsPATCH(handler, senderID, map[string]interface{}{
			"custom_text": "writer followed disable",
		}).Code
	}()
	_ = waitForBlockedConcurrencyBackend(ctx, t, db, disablePID)

	require.NoError(t, blocker.Commit())
	assertOperationSuccess(t, disableDone)
	assertWriterStatus(t, writerDone, http.StatusOK)
	assertDisabledHistoryWithLiveText(t, db, senderID, "writer followed disable")
}

func TestReplaceMyKeysWaitsOnPresenceHistoryServiceGate(t *testing.T) {
	ts := setupTS(t)
	user := ts.CreateTestUser(t, "replacekeysgate")
	target := ts.CreateTestUser(t, "replacekeysgatetarget")
	seedPresenceOverrideStateForKeyRotation(t, ts, user.ID, target.ID)
	senderID := uuid.MustParse(user.ID)
	handler, service := newConcurrencyHandler(t, ts.DB, immediatePresenceDelivery{})

	gateEntered := make(chan struct{})
	gateRelease := make(chan struct{})
	gateDone := make(chan error, 1)
	go func() {
		gateDone <- service.WithReadySender(context.Background(), senderID, func() error {
			close(gateEntered)
			<-gateRelease
			return nil
		})
	}()
	select {
	case <-gateEntered:
	case <-time.After(10 * time.Second):
		t.Fatal("test holder did not acquire the presence history Service gate")
	}

	publicKey, wrappedKey, salt := testhelpers.E2EETestKeys()
	body, err := json.Marshal(map[string]interface{}{
		keyWrappedPrivateKey:    wrappedKey,
		keyKeyDerivationSalt:    salt,
		"key_derivation_alg":    "argon2id",
		"public_key":            publicKey,
		"acknowledge_data_loss": true,
		keyCurrentPassword:      user.Password,
	})
	require.NoError(t, err)
	responseDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Set("user_id", user.ID)
		c.Request = httptest.NewRequest(http.MethodPut, urlUsersMeKeys, bytes.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		handler.ReplaceMyKeys(c)
		responseDone <- w
	}()
	select {
	case response := <-responseDone:
		t.Fatalf("ReplaceMyKeys escaped the shared Service gate with status %d", response.Code)
	case <-time.After(150 * time.Millisecond):
	}

	close(gateRelease)
	assertOperationSuccess(t, gateDone)
	select {
	case response := <-responseDone:
		require.Equal(t, http.StatusOK, response.Code, response.Body.String())
	case <-time.After(10 * time.Second):
		t.Fatal("ReplaceMyKeys did not resume after the Service gate released")
	}
	assertPresenceOverrideStateResetFailClosed(t, ts, user.ID)
}

func newConcurrencyHandler(
	t *testing.T,
	db *sql.DB,
	delivery presencehistory.Delivery,
) (*users.Handler, *presencehistory.Service) {
	t.Helper()
	service := presencehistory.NewService(db, task9Disclosure(), true)
	require.NoError(t, service.BindDelivery(delivery))
	handler := users.NewHandler(db, logger.NewWithWriter(io.Discard), nil, nil, nil)
	handler.SetPresenceHistory(service)
	bindNoopActivitySettingsSuppressor(handler)
	return handler, service
}

func task9Disclosure() presencehistory.DisclosureState {
	return presencehistory.DisclosureState{
		Available: true,
		RequiredConsent: &presencehistory.RequiredConsent{
			Version:  1,
			CopyHash: task9ConsentHash,
		},
	}
}

func seedEnabledHistoryStatus(t *testing.T, db *sql.DB, text string) uuid.UUID {
	t.Helper()
	senderID := testhelpers.CreateUser(t, db)
	_, err := db.Exec(`
		INSERT INTO user_presence_settings (
			user_id, custom_text_tier, custom_text, activity_history_enabled,
			activity_history_retention_days, activity_history_consent_version,
			activity_history_consent_copy_hash, activity_history_consented_at
		) VALUES ($1, 1, $2, TRUE, 30, 1, $3, clock_timestamp())
	`, senderID, text, task9ConsentHash)
	require.NoError(t, err)
	return senderID
}

func seedOpenHistoryRow(t *testing.T, db *sql.DB, senderID uuid.UUID, text string) uuid.UUID {
	t.Helper()
	historyID := uuid.New()
	recordedAt := time.Now().UTC().Add(-time.Minute)
	_, err := db.Exec(`
		INSERT INTO presence_history (
			id, sender_id, category, payload_version, payload,
			started_at, ended_at, recorded_at, expires_at
		) VALUES ($1, $2, 'custom_text', 1, jsonb_build_object('text', $3::TEXT),
			$4::TIMESTAMPTZ, NULL, $4::TIMESTAMPTZ, $4::TIMESTAMPTZ + INTERVAL '30 days')
	`, historyID, senderID, text, recordedAt)
	require.NoError(t, err)
	return historyID
}

func concurrencyBackendPID(ctx context.Context, t *testing.T, tx *sql.Tx) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

func beginConcurrencyTxWithPID(
	db *sql.DB,
	pids chan<- int,
) func(context.Context, *sql.TxOptions) (*sql.Tx, error) {
	return func(ctx context.Context, options *sql.TxOptions) (*sql.Tx, error) {
		tx, err := db.BeginTx(ctx, options)
		if err != nil {
			return nil, err
		}
		var pid int
		if err := tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		pids <- pid
		return tx, nil
	}
}

func latestConcurrencyBackendPID(t *testing.T, pids <-chan int) int {
	t.Helper()
	latest := 0
	for {
		select {
		case latest = <-pids:
		default:
			require.NotZero(t, latest, "transaction backend PID was not captured")
			return latest
		}
	}
}

func waitForConcurrencyBackendPID(t *testing.T, pids <-chan int) int {
	t.Helper()
	select {
	case pid := <-pids:
		return pid
	case <-time.After(10 * time.Second):
		t.Fatal("transaction backend PID was not captured")
		return 0
	}
}

func waitForBlockedConcurrencyBackend(
	ctx context.Context,
	t *testing.T,
	db *sql.DB,
	blockerPID int,
) int {
	t.Helper()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		var blockedPID int
		err := db.QueryRowContext(ctx, `
			SELECT COALESCE((
				SELECT activity.pid
				FROM pg_stat_activity AS activity
				WHERE $1::INTEGER = ANY(pg_blocking_pids(activity.pid))
				  AND activity.pid <> pg_backend_pid()
				ORDER BY activity.pid
				LIMIT 1
			), 0)
		`, blockerPID).Scan(&blockedPID)
		require.NoError(t, err)
		if blockedPID != 0 {
			return blockedPID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("lock waiter was not observed: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func assertWriterStillBlocked(t *testing.T, result <-chan int, message string) {
	t.Helper()
	select {
	case status := <-result:
		t.Fatalf("%s: status %d", message, status)
	case <-time.After(150 * time.Millisecond):
	}
}

func assertOperationStillBlocked(t *testing.T, result <-chan error, message string) {
	t.Helper()
	select {
	case err := <-result:
		t.Fatalf("%s: %v", message, err)
	case <-time.After(150 * time.Millisecond):
	}
}

func assertWriterStatus(t *testing.T, result <-chan int, want int) {
	t.Helper()
	select {
	case status := <-result:
		assert.Equal(t, want, status)
	case <-time.After(10 * time.Second):
		t.Fatal("presence writer did not finish")
	}
}

func assertOperationSuccess(t *testing.T, result <-chan error) {
	t.Helper()
	select {
	case err := <-result:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent operation did not finish")
	}
}

func assertDisabledHistoryWithLiveText(t *testing.T, db *sql.DB, senderID uuid.UUID, wantText string) {
	t.Helper()
	var enabled bool
	var text string
	var historyCount int
	require.NoError(t, db.QueryRow(`
		SELECT activity_history_enabled, custom_text
		FROM user_presence_settings WHERE user_id = $1
	`, senderID).Scan(&enabled, &text))
	require.NoError(t, db.QueryRow(
		`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, senderID,
	).Scan(&historyCount))
	assert.False(t, enabled)
	assert.Equal(t, wantText, text)
	assert.Zero(t, historyCount)
}

var _ presencehistory.Delivery = (*gatedPresenceDelivery)(nil)
var _ = uuid.Nil
