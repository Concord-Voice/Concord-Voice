package presencehistory

import (
	"context"
	"database/sql"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStartupReconcileStaleDisclosurePausesOnlyStaleEnabledRowsAndPreservesHistory(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	service := NewService(ts.DB, disclosure, true)
	staleUser := uuid.MustParse(ts.CreateTestUser(t, "startup_stale_hash").ID)
	staleVersionUser := uuid.MustParse(ts.CreateTestUser(t, "startup_stale_version").ID)
	currentUser := uuid.MustParse(ts.CreateTestUser(t, "startup_current").ID)
	disabledUser := uuid.MustParse(ts.CreateTestUser(t, "startup_disabled").ID)
	enableHistory(t, ts.DB, staleUser, disclosure, 90)
	enableHistory(t, ts.DB, staleVersionUser, disclosure, 7)
	enableHistory(t, ts.DB, currentUser, disclosure, 30)
	_, err := ts.DB.Exec(`
		UPDATE user_presence_settings
		SET activity_history_consent_copy_hash = $2
		WHERE user_id = $1
	`, staleUser, strings.Repeat("0", 64))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		UPDATE user_presence_settings
		SET activity_history_consent_version = $2
		WHERE user_id = $1
	`, staleVersionUser, disclosure.RequiredConsent.Version+1)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, disabledUser)
	require.NoError(t, err)
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	expiresAt := now.Add(90 * 24 * time.Hour)
	historyID := insertHistoryRow(t, ts.DB, staleUser, CategoryCustomText, 1,
		`{"text":"preserve me"}`, now, nil, now, expiresAt)
	currentHistoryID := insertHistoryRow(t, ts.DB, currentUser, CategoryCustomText, 1,
		`{"text":"still current"}`, now, nil, now, expiresAt)

	paused, err := service.ReconcileStaleDisclosure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), paused)
	assertPausedConsentState(t, ts.DB, staleUser, 90)
	assertPausedConsentState(t, ts.DB, staleVersionUser, 7)
	assert.True(t, historyIDExists(t, ts.DB, historyID))
	assert.Zero(t, openHistoryRowCount(t, ts.DB, staleUser))
	var retainedExpiry time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, historyID).Scan(&retainedExpiry))
	assert.Equal(t, expiresAt, retainedExpiry)

	current, err := service.GetSettings(context.Background(), currentUser)
	require.NoError(t, err)
	assert.True(t, current.Enabled)
	assert.False(t, current.ReconsentRequired)
	assert.Equal(t, 1, openHistoryRowCount(t, ts.DB, currentUser))
	assert.True(t, historyIDExists(t, ts.DB, currentHistoryID))
	var disabledReconsent bool
	require.NoError(t, ts.DB.QueryRow(`
		SELECT activity_history_reconsent_required
		FROM user_presence_settings
		WHERE user_id = $1
	`, disabledUser).Scan(&disabledReconsent))
	assert.False(t, disabledReconsent, "disabled rows must remain untouched")

	paused, err = service.ReconcileStaleDisclosure(context.Background())
	require.NoError(t, err)
	assert.Zero(t, paused, "startup reconciliation must be idempotent")
}

func TestStartupReconcileStaleDisclosureUnavailablePausesEveryEnabledRow(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	current := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	first := uuid.MustParse(ts.CreateTestUser(t, "startup_unavailable_first").ID)
	second := uuid.MustParse(ts.CreateTestUser(t, "startup_unavailable_second").ID)
	enableHistory(t, ts.DB, first, current, 7)
	enableHistory(t, ts.DB, second, current, 365)
	now := time.Now().UTC().Add(-time.Hour).Truncate(time.Microsecond)
	expiresAt := now.Add(7 * 24 * time.Hour)
	historyID := insertHistoryRow(t, ts.DB, first, CategoryCustomText, 1,
		`{"text":"preserved while unavailable"}`, now, nil, now, expiresAt)
	unavailable := BuildDisclosure(DisclosureOptions{InstanceType: "self-hosted"})

	paused, err := NewService(ts.DB, unavailable, true).ReconcileStaleDisclosure(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(2), paused)
	assertPausedConsentState(t, ts.DB, first, 7)
	assertPausedConsentState(t, ts.DB, second, 365)
	assert.True(t, historyIDExists(t, ts.DB, historyID))
	assert.Zero(t, openHistoryRowCount(t, ts.DB, first))
	var retainedExpiry time.Time
	require.NoError(t, ts.DB.QueryRow(`SELECT expires_at FROM presence_history WHERE id = $1`, historyID).Scan(&retainedExpiry))
	assert.Equal(t, expiresAt, retainedExpiry)
}

func TestStartupReconcileStaleDisclosureFailsClosedWithoutDatabase(t *testing.T) {
	var nilService *Service
	_, err := nilService.ReconcileStaleDisclosure(context.Background())
	require.Error(t, err)

	_, err = NewService(nil, DisclosureState{}, false).ReconcileStaleDisclosure(context.Background())
	require.Error(t, err)
}

func TestActivityHistoryActivationDisabledKeepsRecordingForAlreadyEnabledCurrentConsent(t *testing.T) {
	ts := setupPresenceHistoryTestServer(t)
	disclosure := BuildDisclosure(DisclosureOptions{InstanceType: "saas"})
	userID := uuid.MustParse(ts.CreateTestUser(t, "history_gate_off_drain").ID)
	enableHistory(t, ts.DB, userID, disclosure, 30)
	service := NewService(ts.DB, disclosure, false)

	tx, err := ts.DB.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	require.NoError(t, service.RecordCustomTextTransition(
		context.Background(),
		tx,
		userID,
		CustomTextState{},
		CustomTextState{Text: "record during rollback drain"},
	))
	require.NoError(t, tx.Commit())
	assert.Equal(t, 1, historyRowCount(t, ts.DB, userID))
}

func assertPausedConsentState(t *testing.T, db *sql.DB, userID uuid.UUID, retention int16) {
	t.Helper()
	var enabled, reconsent bool
	var version sql.NullInt64
	var hash sql.NullString
	var consentedAt sql.NullTime
	var retained int16
	require.NoError(t, db.QueryRow(`
		SELECT activity_history_enabled,
		       activity_history_reconsent_required,
		       activity_history_consent_version,
		       activity_history_consent_copy_hash,
		       activity_history_consented_at,
		       activity_history_retention_days
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(&enabled, &reconsent, &version, &hash, &consentedAt, &retained))
	assert.False(t, enabled)
	assert.True(t, reconsent)
	assert.False(t, version.Valid)
	assert.False(t, hash.Valid)
	assert.False(t, consentedAt.Valid)
	assert.Equal(t, retention, retained)
}
