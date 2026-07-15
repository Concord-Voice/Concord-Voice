package database_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000089_DefaultsConstraintsAndStructure(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	user := ts.CreateTestUser(t, "presence_category_defaults")

	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, user.ID)
	require.NoError(t, err)

	var (
		masterEnabled          bool
		serverVoiceTier        int16
		serverVoiceShowDetails bool
		privateCallTier        int16
		privateCallShowDetails bool
	)
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT master_enabled,
		       server_voice_tier,
		       server_voice_show_details,
		       private_call_tier,
		       private_call_show_details
		FROM user_presence_settings
		WHERE user_id = $1
	`, user.ID).Scan(
		&masterEnabled,
		&serverVoiceTier,
		&serverVoiceShowDetails,
		&privateCallTier,
		&privateCallShowDetails,
	))
	assert.True(t, masterEnabled)
	assert.Equal(t, int16(1), serverVoiceTier)
	assert.True(t, serverVoiceShowDetails)
	assert.Equal(t, int16(0), privateCallTier)
	assert.False(t, privateCallShowDetails)

	tierColumns := []struct {
		name       string
		updateSQL  string
		constraint string
	}{
		{
			name:       "server voice",
			updateSQL:  `UPDATE user_presence_settings SET server_voice_tier = $2 WHERE user_id = $1`,
			constraint: "user_presence_settings_server_voice_tier_check",
		},
		{
			name:       "private call",
			updateSQL:  `UPDATE user_presence_settings SET private_call_tier = $2 WHERE user_id = $1`,
			constraint: "user_presence_settings_private_call_tier_check",
		},
	}
	for _, tierColumn := range tierColumns {
		t.Run(tierColumn.name, func(t *testing.T) {
			for _, tier := range []int16{0, 1, 2} {
				_, err := ts.DB.ExecContext(ctx, tierColumn.updateSQL, user.ID, tier)
				require.NoErrorf(t, err, "tier %d must be accepted", tier)
			}
			for _, tier := range []int16{-1, 3} {
				_, err := ts.DB.ExecContext(ctx, tierColumn.updateSQL, user.ID, tier)
				migration000089RequireConstraint(t, err, tierColumn.constraint)
			}
		})
	}

	migration000089AssertStructure(t, ts.DB)
	migration000089AssertUnvalidatedConstraints(t, ts.DB)
}

func TestMigration000089_DownUpPreservesExistingState(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	downSQL := migration000089SQL(t, "down")
	upSQL := migration000089SQL(t, "up")

	_, err := ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "down migration must remove the category settings extension")
	migrationApplied := false
	t.Cleanup(func() {
		if migrationApplied {
			return
		}
		_, cleanupErr := ts.DB.ExecContext(context.Background(), upSQL)
		assert.NoError(t, cleanupErr, "restore migration 000089 after rollback test")
	})

	user := ts.CreateTestUser(t, "presence_category_legacy")
	target := ts.CreateTestUser(t, "presence_category_override_target")
	operationID := uuid.NewString()
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO user_presence_settings (
			user_id,
			custom_text_tier,
			custom_text,
			custom_text_emoji,
			created_at,
			updated_at,
			presence_settings_version,
			presence_settings_operation_id,
			activity_history_enabled,
			activity_history_retention_days,
			activity_history_consent_version,
			activity_history_consent_copy_hash,
			activity_history_consented_at,
			activity_history_reconsent_required
		) VALUES (
			$1,
			2,
			'Heads-down migration work',
			'🛠️',
			TIMESTAMPTZ '2026-01-02 03:04:05+00',
			TIMESTAMPTZ '2026-02-03 04:05:06+00',
			7,
			$2,
			TRUE,
			90,
			3,
			repeat('a', 64),
			TIMESTAMPTZ '2026-03-04 05:06:07+00',
			FALSE
		)
	`, user.ID, operationID)
	require.NoError(t, err)

	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO presence_override_preferences (
			user_id, category, encrypted_data, version, updated_at
		) VALUES (
			$1, 'custom_text', 'cHJlc2VydmVk', 4, TIMESTAMPTZ '2026-04-05 06:07:08+00'
		)
	`, user.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO user_presence_overrides (
			sender_id, category, target_user_id, created_at
		) VALUES (
			$1, 'custom_text', $2, TIMESTAMPTZ '2026-05-06 07:08:09+00'
		)
	`, user.ID, target.ID)
	require.NoError(t, err)

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err, "up migration must extend a populated settings table")
	migrationApplied = true

	migration000089AssertLegacyState(t, ts.DB, user.ID, target.ID, operationID)
	migration000089AssertDefaults(t, ts.DB, user.ID)
	migration000089AssertStructure(t, ts.DB)
	migration000089AssertUnvalidatedConstraints(t, ts.DB)
	columnsWithExtension := migration000089Columns(t, ts.DB)

	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "down migration must remove only migration 000089 state")
	migrationApplied = false

	columnsWithoutExtension := migration000089Columns(t, ts.DB)
	assert.ElementsMatch(t, migration000089SettingsColumns,
		migration000089Difference(columnsWithExtension, columnsWithoutExtension))
	assert.Empty(t, migration000089Difference(columnsWithoutExtension, columnsWithExtension))
	migration000089AssertLegacyState(t, ts.DB, user.ID, target.ID, operationID)
	migration000089AssertStructure(t, ts.DB)
}

var migration000089SettingsColumns = []string{
	"master_enabled",
	"server_voice_tier",
	"server_voice_show_details",
	"private_call_tier",
	"private_call_show_details",
}

func migration000089AssertDefaults(t *testing.T, db *sql.DB, userID string) {
	t.Helper()

	var (
		masterEnabled          bool
		serverVoiceTier        int16
		serverVoiceShowDetails bool
		privateCallTier        int16
		privateCallShowDetails bool
	)
	require.NoError(t, db.QueryRow(`
		SELECT master_enabled,
		       server_voice_tier,
		       server_voice_show_details,
		       private_call_tier,
		       private_call_show_details
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(
		&masterEnabled,
		&serverVoiceTier,
		&serverVoiceShowDetails,
		&privateCallTier,
		&privateCallShowDetails,
	))
	assert.True(t, masterEnabled)
	assert.Equal(t, int16(1), serverVoiceTier)
	assert.True(t, serverVoiceShowDetails)
	assert.Equal(t, int16(0), privateCallTier)
	assert.False(t, privateCallShowDetails)
}

func migration000089AssertLegacyState(
	t *testing.T,
	db *sql.DB,
	userID string,
	targetID string,
	operationID string,
) {
	t.Helper()

	var (
		customTextTier           int16
		customText               string
		customTextEmoji          string
		createdAt                time.Time
		updatedAt                time.Time
		settingsVersion          int64
		storedOperationID        string
		historyEnabled           bool
		historyRetentionDays     int16
		historyConsentVersion    int16
		historyConsentHash       string
		historyConsentedAt       time.Time
		historyReconsentRequired bool
		overrideEncryptedData    string
		overrideVersion          int
		overrideUpdatedAt        time.Time
		overrideCategory         string
		overrideTargetID         string
		overrideCreatedAt        time.Time
	)
	require.NoError(t, db.QueryRow(`
		SELECT custom_text_tier,
		       custom_text,
		       custom_text_emoji,
		       created_at,
		       updated_at,
		       presence_settings_version,
		       presence_settings_operation_id::TEXT,
		       activity_history_enabled,
		       activity_history_retention_days,
		       activity_history_consent_version,
		       activity_history_consent_copy_hash,
		       activity_history_consented_at,
		       activity_history_reconsent_required
		FROM user_presence_settings
		WHERE user_id = $1
	`, userID).Scan(
		&customTextTier,
		&customText,
		&customTextEmoji,
		&createdAt,
		&updatedAt,
		&settingsVersion,
		&storedOperationID,
		&historyEnabled,
		&historyRetentionDays,
		&historyConsentVersion,
		&historyConsentHash,
		&historyConsentedAt,
		&historyReconsentRequired,
	))
	assert.Equal(t, int16(2), customTextTier)
	assert.Equal(t, "Heads-down migration work", customText)
	assert.Equal(t, "🛠️", customTextEmoji)
	assert.WithinDuration(t, time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), createdAt, 0)
	assert.WithinDuration(t, time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC), updatedAt, 0)
	assert.Equal(t, int64(7), settingsVersion)
	assert.Equal(t, operationID, storedOperationID)
	assert.True(t, historyEnabled)
	assert.Equal(t, int16(90), historyRetentionDays)
	assert.Equal(t, int16(3), historyConsentVersion)
	assert.Equal(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", historyConsentHash)
	assert.WithinDuration(t, time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC), historyConsentedAt, 0)
	assert.False(t, historyReconsentRequired)

	require.NoError(t, db.QueryRow(`
		SELECT encrypted_data, version, updated_at
		FROM presence_override_preferences
		WHERE user_id = $1 AND category = 'custom_text'
	`, userID).Scan(&overrideEncryptedData, &overrideVersion, &overrideUpdatedAt))
	assert.Equal(t, "cHJlc2VydmVk", overrideEncryptedData)
	assert.Equal(t, 4, overrideVersion)
	assert.WithinDuration(t, time.Date(2026, 4, 5, 6, 7, 8, 0, time.UTC), overrideUpdatedAt, 0)

	require.NoError(t, db.QueryRow(`
		SELECT category, target_user_id::TEXT, created_at
		FROM user_presence_overrides
		WHERE sender_id = $1
	`, userID).Scan(&overrideCategory, &overrideTargetID, &overrideCreatedAt))
	assert.Equal(t, "custom_text", overrideCategory)
	assert.Equal(t, targetID, overrideTargetID)
	assert.WithinDuration(t, time.Date(2026, 5, 6, 7, 8, 9, 0, time.UTC), overrideCreatedAt, 0)
}

func migration000089AssertStructure(t *testing.T, db *sql.DB) {
	t.Helper()

	var categoryTable sql.NullString
	require.NoError(t, db.QueryRow(
		`SELECT to_regclass('public.presence_category_settings')::TEXT`,
	).Scan(&categoryTable))
	assert.False(t, categoryTable.Valid, "migration must not create presence_category_settings")

	var indexCount, primaryIndexCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*), COUNT(*) FILTER (WHERE indisprimary)
		FROM pg_index
		WHERE indrelid = 'public.user_presence_settings'::regclass
	`).Scan(&indexCount, &primaryIndexCount))
	assert.Equal(t, 1, indexCount, "user_presence_settings must have no redundant indexes")
	assert.Equal(t, 1, primaryIndexCount, "the sole settings index must be its primary key")

	var triggerCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*)
		FROM pg_trigger
		WHERE tgrelid = 'public.user_presence_settings'::regclass
			AND NOT tgisinternal
	`).Scan(&triggerCount))
	assert.Zero(t, triggerCount, "user_presence_settings must have no user triggers")
}

func migration000089AssertUnvalidatedConstraints(t *testing.T, db *sql.DB) {
	t.Helper()

	for _, constraint := range []string{
		"user_presence_settings_server_voice_tier_check",
		"user_presence_settings_private_call_tier_check",
	} {
		var validated bool
		require.NoErrorf(t, db.QueryRow(`
			SELECT convalidated
			FROM pg_constraint
			WHERE conrelid = 'public.user_presence_settings'::regclass
				AND conname = $1
				AND contype = 'c'
		`, constraint).Scan(&validated), "constraint %s must exist", constraint)
		assert.Falsef(t, validated, "constraint %s must remain NOT VALID", constraint)
	}
}

func migration000089Columns(t *testing.T, db *sql.DB) []string {
	t.Helper()

	rows, err := db.Query(`
		SELECT column_name
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'user_presence_settings'
		ORDER BY column_name
	`)
	require.NoError(t, err)
	defer func() {
		assert.NoError(t, rows.Close())
	}()

	var columns []string
	for rows.Next() {
		var column string
		require.NoError(t, rows.Scan(&column))
		columns = append(columns, column)
	}
	require.NoError(t, rows.Err())
	return columns
}

func migration000089Difference(left, right []string) []string {
	rightSet := make(map[string]struct{}, len(right))
	for _, value := range right {
		rightSet[value] = struct{}{}
	}

	var difference []string
	for _, value := range left {
		if _, ok := rightSet[value]; !ok {
			difference = append(difference, value)
		}
	}
	return difference
}

func migration000089RequireConstraint(t *testing.T, err error, name string) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL constraint error, got %T", err)
	assert.Equal(t, name, pqErr.Constraint)
}

func migration000089SQL(t *testing.T, direction string) string {
	t.Helper()

	migrationFile, ok := map[string]string{
		"up":   "000089_add_presence_category_settings.up.sql",
		"down": "000089_add_presence_category_settings.down.sql",
	}[direction]
	require.True(t, ok, "unexpected migration direction %q", direction)

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations", migrationFile)
	// #nosec G304 -- path is constructed from runtime.Caller and a fixed migration filename.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
