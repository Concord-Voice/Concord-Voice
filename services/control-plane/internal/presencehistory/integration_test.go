package presencehistory

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	testhelpers "github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSettingsDefaultReadDoesNotCreateRow(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	user := ts.CreateTestUser(t, "history_settings_default")
	userID := uuid.MustParse(user.ID)

	settings, err := service.GetSettings(context.Background(), userID)
	require.NoError(t, err)
	assert.True(t, settings.Available)
	assert.False(t, settings.Enabled)
	assert.False(t, settings.ReconsentRequired)
	assert.Equal(t, int16(30), settings.RetentionDays)
	assert.Nil(t, settings.ConsentVersion)
	assert.Nil(t, settings.ConsentCopyHash)
	assert.Nil(t, settings.ConsentedAt)
	require.NotNil(t, settings.RequiredConsent)
	assert.Equal(t, disclosure.RequiredConsent.CopyHash, settings.RequiredConsent.CopyHash)
	assert.Zero(t, settingsRowCount(t, ts.DB, userID))
}

func TestSettingsUnavailableDisclosureReadRemainsAvailableForDeletion(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	service := NewService(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "self-hosted"}), true)
	user := ts.CreateTestUser(t, "history_settings_unavailable")

	settings, err := service.GetSettings(context.Background(), uuid.MustParse(user.ID))
	require.NoError(t, err)
	assert.False(t, settings.Available)
	assert.Nil(t, settings.RequiredConsent)
}

func TestSettingsEnableRequiresCurrentExplicitConsent(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	user := ts.CreateTestUser(t, "history_enable")
	userID := uuid.MustParse(user.ID)

	valid := UpdateSettingsRequest{
		Enabled:         boolPointer(true),
		RetentionDays:   int16Pointer(30),
		Acknowledged:    boolPointer(true),
		ConsentVersion:  int16Pointer(disclosure.RequiredConsent.Version),
		ConsentCopyHash: stringPointer(disclosure.RequiredConsent.CopyHash),
	}
	settings, err := NewService(ts.DB, disclosure, true).UpdateSettings(context.Background(), userID, valid)
	require.NoError(t, err)
	assert.True(t, settings.Enabled)
	require.NotNil(t, settings.ConsentedAt)
	assert.Equal(t, int16(30), settings.RetentionDays)
	assert.Zero(t, historyRowCount(t, ts.DB, userID), "enable must not backfill current Custom Status")

	for _, tc := range []struct {
		name    string
		service *Service
		request UpdateSettingsRequest
		code    string
	}{
		{
			name:    "activation gate false",
			service: NewService(ts.DB, disclosure, false),
			request: valid,
			code:    "activity_history_activation_unavailable",
		},
		{
			name: "disclosure unavailable",
			service: NewService(ts.DB, BuildDisclosure(DisclosureOptions{
				InstanceType: "self-hosted",
			}), true),
			request: valid,
			code:    "activity_history_disclosure_unavailable",
		},
		{
			name:    "missing acknowledgement",
			service: NewService(ts.DB, disclosure, true),
			request: UpdateSettingsRequest{
				Enabled:         boolPointer(true),
				RetentionDays:   int16Pointer(30),
				ConsentVersion:  valid.ConsentVersion,
				ConsentCopyHash: valid.ConsentCopyHash,
			},
			code: "activity_history_invalid_request",
		},
		{
			name:    "stale version",
			service: NewService(ts.DB, disclosure, true),
			request: UpdateSettingsRequest{
				Enabled:         boolPointer(true),
				RetentionDays:   int16Pointer(30),
				Acknowledged:    boolPointer(true),
				ConsentVersion:  int16Pointer(disclosure.RequiredConsent.Version + 1),
				ConsentCopyHash: valid.ConsentCopyHash,
			},
			code: "activity_history_consent_mismatch",
		},
		{
			name:    "stale hash",
			service: NewService(ts.DB, disclosure, true),
			request: UpdateSettingsRequest{
				Enabled:         boolPointer(true),
				RetentionDays:   int16Pointer(30),
				Acknowledged:    boolPointer(true),
				ConsentVersion:  valid.ConsentVersion,
				ConsentCopyHash: stringPointer(strings.Repeat("0", 64)),
			},
			code: "activity_history_consent_mismatch",
		},
		{
			name:    "invalid retention",
			service: NewService(ts.DB, disclosure, true),
			request: UpdateSettingsRequest{
				Enabled:         boolPointer(true),
				RetentionDays:   int16Pointer(8),
				Acknowledged:    boolPointer(true),
				ConsentVersion:  valid.ConsentVersion,
				ConsentCopyHash: valid.ConsentCopyHash,
			},
			code: "activity_history_invalid_request",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := ts.CreateTestUser(t, "history_enable_"+strings.ReplaceAll(tc.name, " ", "_"))
			_, err := tc.service.UpdateSettings(context.Background(), uuid.MustParse(other.ID), tc.request)
			requireServiceCode(t, err, tc.code)
			assert.Zero(t, settingsRowCount(t, ts.DB, uuid.MustParse(other.ID)),
				"validation must finish before opening a write transaction")
		})
	}
}

func TestRetentionUpdateDeletesExpiredWithoutResurrection(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	user := ts.CreateTestUser(t, "history_retention")
	userID := uuid.MustParse(user.ID)
	enableThroughService(t, service, disclosure, userID, 30)

	now := time.Now().UTC().Truncate(time.Microsecond)
	oldRecorded := now.Add(-20 * 24 * time.Hour)
	oldID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"too old for seven days"}`, oldRecorded, oldRecorded.Add(time.Hour),
		oldRecorded, oldRecorded.Add(30*24*time.Hour))
	recentRecorded := now.Add(-2 * 24 * time.Hour)
	recentID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"recent"}`, recentRecorded, recentRecorded.Add(time.Hour),
		recentRecorded, recentRecorded.Add(30*24*time.Hour))
	alreadyExpiredRecorded := now.Add(-40 * 24 * time.Hour)
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"already expired"}`, alreadyExpiredRecorded, alreadyExpiredRecorded.Add(time.Hour),
		alreadyExpiredRecorded, now.Add(-time.Second))

	settings, err := service.UpdateSettings(ctx, userID, UpdateSettingsRequest{RetentionDays: int16Pointer(7)})
	require.NoError(t, err)
	assert.Equal(t, int16(7), settings.RetentionDays)
	assert.False(t, historyIDExists(t, ts.DB, oldID))
	assert.True(t, historyIDExists(t, ts.DB, recentID))
	var expiresAt time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, recentID).Scan(&expiresAt))
	assert.Equal(t, recentRecorded.Add(7*24*time.Hour), expiresAt)

	settings, err = service.UpdateSettings(ctx, userID, UpdateSettingsRequest{RetentionDays: int16Pointer(90)})
	require.NoError(t, err)
	assert.Equal(t, int16(90), settings.RetentionDays)
	assert.False(t, historyIDExists(t, ts.DB, oldID), "an increase cannot resurrect a deleted row")
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, recentID).Scan(&expiresAt))
	assert.Equal(t, recentRecorded.Add(90*24*time.Hour), expiresAt)
}

func TestRetentionReconsentRules(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	user := ts.CreateTestUser(t, "history_reconsent")
	userID := uuid.MustParse(user.ID)
	enableThroughService(t, service, disclosure, userID, 30)
	recordedAt := time.Now().UTC().Add(-2 * 24 * time.Hour).Truncate(time.Microsecond)
	historyID := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"preserved during reconsent"}`, recordedAt, recordedAt.Add(time.Hour),
		recordedAt, recordedAt.Add(30*24*time.Hour))
	_, err := ts.DB.Exec(`
		UPDATE user_presence_settings
		SET activity_history_enabled = FALSE,
		    activity_history_consent_version = NULL,
		    activity_history_consent_copy_hash = NULL,
		    activity_history_consented_at = NULL,
		    activity_history_reconsent_required = TRUE
		WHERE user_id = $1
	`, userID)
	require.NoError(t, err)

	settings, err := service.UpdateSettings(ctx, userID, UpdateSettingsRequest{RetentionDays: int16Pointer(7)})
	require.NoError(t, err)
	assert.Equal(t, int16(7), settings.RetentionDays)
	assert.True(t, settings.ReconsentRequired)

	_, err = service.UpdateSettings(ctx, userID, UpdateSettingsRequest{RetentionDays: int16Pointer(90)})
	requireServiceCode(t, err, "activity_history_reconsent_required")
	var expiresAt time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, historyID).Scan(&expiresAt))
	assert.Equal(t, recordedAt.Add(7*24*time.Hour), expiresAt)

	settings, err = service.UpdateSettings(ctx, userID, currentEnableRequest(disclosure, 90))
	require.NoError(t, err)
	assert.True(t, settings.Enabled)
	assert.False(t, settings.ReconsentRequired)
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, historyID).Scan(&expiresAt))
	assert.Equal(t, recordedAt.Add(90*24*time.Hour), expiresAt,
		"same-request current consent must authorize the selected retention change")
}

func TestDisableAndDeleteIsIdempotentAndPristineSafe(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	ctx := context.Background()
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)

	pristine := ts.CreateTestUser(t, "history_disable_pristine")
	pristineID := uuid.MustParse(pristine.ID)
	_, err := service.UpdateSettings(ctx, pristineID, UpdateSettingsRequest{Enabled: boolPointer(false)})
	require.NoError(t, err)
	require.NoError(t, service.DisableAndDelete(ctx, pristineID))
	require.NoError(t, service.DisableAndDelete(ctx, pristineID))
	assert.Zero(t, settingsRowCount(t, ts.DB, pristineID))

	user := ts.CreateTestUser(t, "history_disable_existing")
	userID := uuid.MustParse(user.ID)
	enableThroughService(t, service, disclosure, userID, 30)
	_, err = ts.DB.Exec(`
		UPDATE user_presence_settings
		SET custom_text_tier = 2, custom_text = 'live status', custom_text_emoji = '🔒'
		WHERE user_id = $1
	`, userID)
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"live status","emoji":"🔒"}`, now, nil, now, now.Add(time.Hour))

	require.NoError(t, service.DisableAndDelete(ctx, userID))
	assert.Zero(t, historyRowCount(t, ts.DB, userID))
	var enabled, reconsent bool
	var text, emoji sql.NullString
	var tier int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT activity_history_enabled, activity_history_reconsent_required,
		       custom_text_tier, custom_text, custom_text_emoji
		FROM user_presence_settings WHERE user_id = $1
	`, userID).Scan(&enabled, &reconsent, &tier, &text, &emoji))
	assert.False(t, enabled)
	assert.False(t, reconsent)
	assert.Equal(t, 2, tier)
	assert.Equal(t, "live status", text.String)
	assert.Equal(t, "🔒", emoji.String)
}

func TestDisableConvergesWithStatusWriter(t *testing.T) {
	t.Run("writer first", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
		service := NewService(db, disclosure, true)
		userID := testhelpers.CreateUser(t, db)
		enableThroughService(t, service, disclosure, userID, 30)

		writer, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() {
			if rollbackErr := writer.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback writer-first transaction: %v", rollbackErr)
			}
		})
		require.NoError(t, service.RecordCustomTextTransition(ctx, writer, userID,
			CustomTextState{}, CustomTextState{Text: "racing"}))
		writerPID := historyBackendPID(ctx, t, writer)

		disableDone := make(chan error, 1)
		go func() { disableDone <- service.DisableAndDelete(ctx, userID) }()
		_ = waitForHistoryBlockedBackend(ctx, t, db, writerPID)

		require.NoError(t, writer.Commit())
		require.NoError(t, receiveHistoryOperation(ctx, t, disableDone))
		assert.Zero(t, historyRowCount(t, db, userID))
	})

	t.Run("disable first", func(t *testing.T) {
		db, cleanup := testhelpers.SetupTestDB(t)
		defer cleanup()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
		service := NewService(db, disclosure, true)
		userID := testhelpers.CreateUser(t, db)
		enableThroughService(t, service, disclosure, userID, 30)
		require.NoError(t, recordHistoryTransitionAndCommit(
			ctx,
			service,
			db,
			userID,
			CustomTextState{},
			CustomTextState{Text: "before disable"},
		))

		historyBlocker, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		t.Cleanup(func() {
			if rollbackErr := historyBlocker.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback disable-first blocker: %v", rollbackErr)
			}
		})
		var lockedID uuid.UUID
		require.NoError(t, historyBlocker.QueryRowContext(ctx, `
			SELECT id
			FROM presence_history
			WHERE sender_id = $1
			FOR UPDATE
		`, userID).Scan(&lockedID))
		blockerPID := historyBackendPID(ctx, t, historyBlocker)

		disableDone := make(chan error, 1)
		go func() { disableDone <- service.DisableAndDelete(ctx, userID) }()
		disablePID := waitForHistoryBlockedBackend(ctx, t, db, blockerPID)

		writerDone := make(chan error, 1)
		go func() {
			writerDone <- recordHistoryTransitionAndCommit(
				ctx,
				service,
				db,
				userID,
				CustomTextState{Text: "before disable"},
				CustomTextState{Text: "after disable"},
			)
		}()
		_ = waitForHistoryBlockedBackend(ctx, t, db, disablePID)

		require.NoError(t, historyBlocker.Commit())
		require.NoError(t, receiveHistoryOperation(ctx, t, disableDone))
		require.NoError(t, receiveHistoryOperation(ctx, t, writerDone))
		assert.Zero(t, historyRowCount(t, db, userID))
	})
}

func historyBackendPID(ctx context.Context, t *testing.T, tx *sql.Tx) int {
	t.Helper()
	var pid int
	require.NoError(t, tx.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid))
	return pid
}

func waitForHistoryBlockedBackend(
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
		if err != nil {
			t.Fatalf("observe blocked activity history backend: %v", err)
		}
		if blockedPID != 0 {
			return blockedPID
		}
		select {
		case <-ctx.Done():
			t.Fatalf("activity history lock waiter was not observed: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func recordHistoryTransitionAndCommit(
	ctx context.Context,
	service *Service,
	db *sql.DB,
	userID uuid.UUID,
	before CustomTextState,
	after CustomTextState,
) (returnErr error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			returnErr = errors.Join(returnErr, rollbackErr)
		}
	}()
	if err := service.RecordCustomTextTransition(ctx, tx, userID, before, after); err != nil {
		return err
	}
	return tx.Commit()
}

func receiveHistoryOperation(ctx context.Context, t *testing.T, result <-chan error) error {
	t.Helper()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		t.Fatalf("activity history operation did not finish: %v", ctx.Err())
		return nil
	}
}

func TestHistoryListPrunesExactCutoffAndValidatesOptions(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	service := NewService(ts.DB, BuildDisclosure(DisclosureOptions{InstanceType: "saas"}), true)
	user := ts.CreateTestUser(t, "history_service_list")
	userID := uuid.MustParse(user.ID)
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Microsecond)
	id := insertHistoryRow(t, ts.DB, userID, CategoryCustomText, 1,
		`{"text":"cutoff"}`, now, now.Add(time.Second), now, time.Now().UTC().Add(time.Second))
	_, err := ts.DB.Exec(`UPDATE presence_history SET expires_at = clock_timestamp() WHERE id = $1`, id)
	require.NoError(t, err)

	page, err := service.List(context.Background(), userID, ListOptions{Limit: 50})
	require.NoError(t, err)
	assert.Empty(t, page.Items)
	assert.False(t, historyIDExists(t, ts.DB, id), "exact-cutoff rows are physically pruned before listing")
	for _, limit := range []int{1, 50, 100} {
		_, err = service.List(context.Background(), userID, ListOptions{Limit: limit})
		require.NoError(t, err)
	}
	for _, limit := range []int{-1, 101} {
		_, err = service.List(context.Background(), userID, ListOptions{Limit: limit})
		requireServiceCode(t, err, "activity_history_invalid_request")
	}
	_, err = service.List(context.Background(), userID, ListOptions{Limit: 50, Before: &PageCursor{Version: 2}})
	requireServiceCode(t, err, "activity_history_invalid_request")
}

func currentEnableRequest(disclosure DisclosureState, retention int16) UpdateSettingsRequest {
	return UpdateSettingsRequest{
		Enabled:         boolPointer(true),
		RetentionDays:   int16Pointer(retention),
		Acknowledged:    boolPointer(true),
		ConsentVersion:  int16Pointer(disclosure.RequiredConsent.Version),
		ConsentCopyHash: stringPointer(disclosure.RequiredConsent.CopyHash),
	}
}

func enableThroughService(t *testing.T, service *Service, disclosure DisclosureState, userID uuid.UUID, retention int16) {
	t.Helper()
	_, err := service.UpdateSettings(context.Background(), userID, currentEnableRequest(disclosure, retention))
	require.NoError(t, err)
}

func requireServiceCode(t *testing.T, err error, code string) {
	t.Helper()
	require.Error(t, err)
	var serviceErr *ServiceError
	require.True(t, errors.As(err, &serviceErr), "expected ServiceError, got %T", err)
	assert.Equal(t, code, serviceErr.Code)
}

func settingsRowCount(t *testing.T, db *sql.DB, userID uuid.UUID) int {
	t.Helper()
	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, userID).Scan(&count))
	return count
}

func historyIDExists(t *testing.T, db *sql.DB, id uuid.UUID) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`SELECT EXISTS (SELECT 1 FROM presence_history WHERE id = $1)`, id).Scan(&exists))
	return exists
}

func boolPointer(value bool) *bool       { return &value }
func int16Pointer(value int16) *int16    { return &value }
func stringPointer(value string) *string { return &value }
