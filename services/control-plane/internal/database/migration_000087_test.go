package database_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000087_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	historyExists := migration000087TableExists(t, ts.DB, "presence_history")
	pendingExists := migration000087TableExists(t, ts.DB, "presence_settings_pending_operations")
	assert.True(t, historyExists, "up migration must create presence_history")
	assert.True(t, pendingExists, "up migration must create presence_settings_pending_operations")
	if !historyExists || !pendingExists {
		return
	}

	for _, column := range migration000087SettingsColumns {
		assert.True(t, migration000087ColumnExists(t, ts.DB, "user_presence_settings", column),
			"up migration must add %s", column)
	}

	downSQL := migration000087SQL(t, "down")
	migration000087RequireOrderedDown(t, downSQL)
	_, err := ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "down migration must remove dependent tables before settings columns")
	assert.False(t, migration000087TableExists(t, ts.DB, "presence_settings_pending_operations"))
	assert.False(t, migration000087TableExists(t, ts.DB, "presence_history"))
	for _, column := range migration000087SettingsColumns {
		assert.False(t, migration000087ColumnExists(t, ts.DB, "user_presence_settings", column),
			"down migration must remove %s", column)
	}

	_, err = ts.DB.ExecContext(ctx, migration000087SQL(t, "up"))
	require.NoError(t, err, "up migration must re-apply after a clean rollback")
	assert.True(t, migration000087TableExists(t, ts.DB, "presence_history"))
	assert.True(t, migration000087TableExists(t, ts.DB, "presence_settings_pending_operations"))
	for _, column := range migration000087SettingsColumns {
		assert.True(t, migration000087ColumnExists(t, ts.DB, "user_presence_settings", column),
			"re-applied migration must restore %s", column)
	}
}

func TestMigration000087_ConstraintsIndexesDefaultsAndCascades(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	t.Run("settings defaults", func(t *testing.T) {
		user := ts.CreateTestUser(t, "presence_history_defaults")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, user.ID)
		require.NoError(t, err)

		var (
			settingsVersion int64
			operationID     sql.NullString
			enabled         bool
			retentionDays   int16
			consentVersion  sql.NullInt64
			consentHash     sql.NullString
			consentedAt     sql.NullTime
			reconsent       bool
		)
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT presence_settings_version,
			       presence_settings_operation_id::TEXT,
			       activity_history_enabled,
			       activity_history_retention_days,
			       activity_history_consent_version,
			       activity_history_consent_copy_hash,
			       activity_history_consented_at,
			       activity_history_reconsent_required
			FROM user_presence_settings
			WHERE user_id = $1
		`, user.ID).Scan(
			&settingsVersion,
			&operationID,
			&enabled,
			&retentionDays,
			&consentVersion,
			&consentHash,
			&consentedAt,
			&reconsent,
		))
		assert.Zero(t, settingsVersion)
		assert.False(t, operationID.Valid)
		assert.False(t, enabled)
		assert.Equal(t, int16(30), retentionDays)
		assert.False(t, consentVersion.Valid)
		assert.False(t, consentHash.Valid)
		assert.False(t, consentedAt.Valid)
		assert.False(t, reconsent)
	})

	t.Run("retention choices", func(t *testing.T) {
		user := ts.CreateTestUser(t, "presence_history_retention")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, user.ID)
		require.NoError(t, err)

		for _, days := range []int{7, 30, 90, 365} {
			_, err = ts.DB.ExecContext(ctx, `
				UPDATE user_presence_settings
				SET activity_history_retention_days = $2
				WHERE user_id = $1
			`, user.ID, days)
			require.NoErrorf(t, err, "retention %d must be accepted", days)
		}
		for _, days := range []int{-1, 0, 6, 8, 29, 31, 89, 91, 364, 366} {
			_, err = ts.DB.ExecContext(ctx, `
				UPDATE user_presence_settings
				SET activity_history_retention_days = $2
				WHERE user_id = $1
			`, user.ID, days)
			migration000087RequireConstraint(t, err, "user_presence_settings_history_retention_check")
		}
	})

	t.Run("settings version and consent invariants", func(t *testing.T) {
		valid := ts.CreateTestUser(t, "presence_history_valid_consent")
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO user_presence_settings (
				user_id,
				activity_history_enabled,
				activity_history_consent_version,
				activity_history_consent_copy_hash,
				activity_history_consented_at
			) VALUES ($1, TRUE, 1, $2, clock_timestamp())
		`, valid.ID, strings.Repeat("a", 64))
		require.NoError(t, err, "complete current consent must be accepted")

		negativeVersion := ts.CreateTestUser(t, "presence_history_negative_version")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO user_presence_settings (user_id, presence_settings_version)
			VALUES ($1, -1)
		`, negativeVersion.ID)
		migration000087RequireConstraint(t, err, "user_presence_settings_version_nonnegative")

		for _, tc := range []struct {
			name           string
			consentVersion any
			hash           any
			consentedAt    any
			reconsent      bool
			constraint     string
		}{
			{
				name: "null consent version", consentVersion: nil, hash: strings.Repeat("b", 64),
				consentedAt: time.Now().UTC(), constraint: "user_presence_settings_history_consent_check",
			},
			{
				name: "zero consent version", consentVersion: 0, hash: strings.Repeat("b", 64),
				consentedAt: time.Now().UTC(), constraint: "user_presence_settings_history_consent_check",
			},
			{
				name: "missing hash", consentVersion: 1, hash: nil,
				consentedAt: time.Now().UTC(), constraint: "user_presence_settings_history_consent_check",
			},
			{
				name: "missing timestamp", consentVersion: 1, hash: strings.Repeat("b", 64),
				consentedAt: nil, constraint: "user_presence_settings_history_consent_check",
			},
			{
				name: "enabled while reconsent required", consentVersion: 1, hash: strings.Repeat("b", 64),
				consentedAt: time.Now().UTC(), reconsent: true,
				constraint: "user_presence_settings_history_consent_check",
			},
			{
				name: "uppercase hash", consentVersion: 1, hash: strings.Repeat("A", 64),
				consentedAt: time.Now().UTC(), constraint: "user_presence_settings_history_hash_check",
			},
			{
				name: "short hash", consentVersion: 1, hash: strings.Repeat("b", 63),
				consentedAt: time.Now().UTC(), constraint: "user_presence_settings_history_hash_check",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				user := ts.CreateTestUser(t, "presence_history_"+strings.ReplaceAll(tc.name, " ", "_"))
				_, err := ts.DB.ExecContext(ctx, `
					INSERT INTO user_presence_settings (
						user_id,
						activity_history_enabled,
						activity_history_consent_version,
						activity_history_consent_copy_hash,
						activity_history_consented_at,
						activity_history_reconsent_required
					) VALUES ($1, TRUE, $2, $3, $4, $5)
				`, user.ID, tc.consentVersion, tc.hash, tc.consentedAt, tc.reconsent)
				migration000087RequireConstraint(t, err, tc.constraint)
			})
		}

		disabledWithConsent := ts.CreateTestUser(t, "presence_history_disabled_with_consent")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO user_presence_settings (
				user_id,
				activity_history_enabled,
				activity_history_consent_version,
				activity_history_consent_copy_hash,
				activity_history_consented_at
			) VALUES ($1, FALSE, 1, $2, clock_timestamp())
		`, disabledWithConsent.ID, strings.Repeat("c", 64))
		migration000087RequireConstraint(t, err, "user_presence_settings_history_consent_check")

		disabledReconsent := ts.CreateTestUser(t, "presence_history_disabled_reconsent")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO user_presence_settings (
				user_id,
				activity_history_reconsent_required
			) VALUES ($1, TRUE)
		`, disabledReconsent.ID)
		require.NoError(t, err, "a disabled quarantined row may require reconsent without accepted-consent metadata")
	})

	t.Run("history taxonomy payload and time constraints", func(t *testing.T) {
		user := ts.CreateTestUser(t, "presence_history_categories")
		now := time.Now().UTC().Truncate(time.Microsecond)
		for _, category := range []string{
			"server_voice",
			"private_call",
			"games",
			"music",
			"streaming",
			"browser",
			"productivity",
			"creator",
			"custom_text",
		} {
			err := migration000087InsertHistory(
				ts.DB, user.ID, category, 1, `{"value":"test"}`, now, now, now, now.Add(24*time.Hour),
			)
			require.NoErrorf(t, err, "category %q must be accepted", category)
		}

		err := migration000087InsertHistory(
			ts.DB, user.ID, "unknown", 1, `{"value":"test"}`, now, now, now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "presence_history_category_check")

		err = migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `["not-an-object"]`, now, now, now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "presence_history_payload_object_check")

		err = migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 0, `{"value":"test"}`, now, now, now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "presence_history_payload_version_positive")

		err = migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `{"value":"test"}`, now, now.Add(-time.Second), now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "presence_history_ended_at_check")

		err = migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `{"value":"test"}`, now, now, now, now,
		)
		migration000087RequireConstraint(t, err, "presence_history_expires_at_check")

		maxPayload := `{"value":"` + strings.Repeat("x", 4083) + `"}`
		var payloadBytes int
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT octet_length($1::JSONB::TEXT)`, maxPayload,
		).Scan(&payloadBytes))
		require.Equal(t, 4096, payloadBytes, "fixture must exercise the exact 4 KiB JSONB boundary")
		require.NoError(t, migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, maxPayload, now, now, now, now.Add(24*time.Hour),
		))

		overPayload := `{"value":"` + strings.Repeat("x", 4084) + `"}`
		require.NoError(t, ts.DB.QueryRowContext(ctx,
			`SELECT octet_length($1::JSONB::TEXT)`, overPayload,
		).Scan(&payloadBytes))
		require.Equal(t, 4097, payloadBytes)
		err = migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, overPayload, now, now, now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "presence_history_payload_size_check")
	})

	t.Run("one open interval per sender and category", func(t *testing.T) {
		user := ts.CreateTestUser(t, "presence_history_one_open")
		now := time.Now().UTC().Truncate(time.Microsecond)
		require.NoError(t, migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `{"value":"first"}`, now, nil, now, now.Add(24*time.Hour),
		))
		err := migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `{"value":"second"}`, now, nil, now, now.Add(24*time.Hour),
		)
		migration000087RequireConstraint(t, err, "idx_presence_history_one_open")
	})

	t.Run("pending operation constraints and database timestamps", func(t *testing.T) {
		user := ts.CreateTestUser(t, "presence_history_pending")
		operationID := uuid.NewString()
		_, err := ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id, operation_id, prior_settings_version
			) VALUES ($1, $2, 0)
		`, user.ID, operationID)
		require.NoError(t, err)

		var createdAt, reconcileAfter time.Time
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT created_at, reconcile_after
			FROM presence_settings_pending_operations
			WHERE user_id = $1
		`, user.ID).Scan(&createdAt, &reconcileAfter))
		delay := reconcileAfter.Sub(createdAt)
		assert.GreaterOrEqual(t, delay, 30*time.Second)
		assert.Less(t, delay, 31*time.Second)

		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id, operation_id, prior_settings_version
			) VALUES ($1, $2, 0)
		`, user.ID, uuid.NewString())
		migration000087RequireConstraint(t, err, "presence_settings_pending_operations_pkey")

		duplicateOperationUser := ts.CreateTestUser(t, "presence_history_pending_duplicate_operation")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id, operation_id, prior_settings_version
			) VALUES ($1, $2, 0)
		`, duplicateOperationUser.ID, operationID)
		migration000087RequireConstraint(t, err, "presence_settings_pending_operations_operation_id_key")

		negativeVersionUser := ts.CreateTestUser(t, "presence_history_pending_negative")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id, operation_id, prior_settings_version
			) VALUES ($1, $2, -1)
		`, negativeVersionUser.ID, uuid.NewString())
		migration000087RequireConstraint(t, err, "presence_settings_pending_operations_prior_version_check")

		badTimeUser := ts.CreateTestUser(t, "presence_history_pending_bad_time")
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id,
				operation_id,
				prior_settings_version,
				created_at,
				reconcile_after
			) VALUES ($1, $2, 0, $3, $3)
		`, badTimeUser.ID, uuid.NewString(), time.Now().UTC())
		migration000087RequireConstraint(t, err, "presence_settings_pending_operations_reconcile_check")

		assert.Equal(t, "user_id", migration000087PrimaryKeyColumns(t, ts.DB,
			"presence_settings_pending_operations"))
		assert.Equal(t,
			"user_id,operation_id,prior_settings_version,created_at,reconcile_after",
			migration000087Columns(t, ts.DB, "presence_settings_pending_operations"),
			"pending-operation storage must contain no status, exception, or history payload",
		)
	})

	t.Run("indexes and cascades", func(t *testing.T) {
		assert.Equal(t, "id", migration000087PrimaryKeyColumns(t, ts.DB, "presence_history"))
		assert.Equal(t, "c", migration000087ForeignKeyDeleteAction(
			t, ts.DB, "presence_history", "presence_history_sender_id_fkey"))
		assert.Equal(t, "c", migration000087ForeignKeyDeleteAction(
			t, ts.DB, "presence_settings_pending_operations",
			"presence_settings_pending_operations_user_id_fkey"))

		oneOpen := migration000087IndexDefinition(t, ts.DB, "idx_presence_history_one_open")
		assert.Contains(t, oneOpen, "CREATE UNIQUE INDEX")
		assert.Contains(t, oneOpen, "(sender_id, category)")
		assert.Contains(t, oneOpen, "WHERE (ended_at IS NULL)")

		senderPage := migration000087IndexDefinition(t, ts.DB, "idx_presence_history_sender_page")
		assert.Contains(t, senderPage, "(sender_id, recorded_at DESC, id DESC)")
		expiry := migration000087IndexDefinition(t, ts.DB, "idx_presence_history_expiry")
		assert.Contains(t, expiry, "(expires_at, id)")

		user := ts.CreateTestUser(t, "presence_history_cascade")
		_, err := ts.DB.ExecContext(ctx,
			`INSERT INTO user_presence_settings (user_id) VALUES ($1)`, user.ID)
		require.NoError(t, err)
		now := time.Now().UTC().Truncate(time.Microsecond)
		require.NoError(t, migration000087InsertHistory(
			ts.DB, user.ID, "custom_text", 1, `{"value":"test"}`, now, nil, now, now.Add(24*time.Hour),
		))
		_, err = ts.DB.ExecContext(ctx, `
			INSERT INTO presence_settings_pending_operations (
				user_id, operation_id, prior_settings_version
			) VALUES ($1, $2, 0)
		`, user.ID, uuid.NewString())
		require.NoError(t, err)

		_, err = ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, user.ID)
		require.NoError(t, err)
		assert.Zero(t, migration000087RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM user_presence_settings WHERE user_id = $1`, user.ID))
		assert.Zero(t, migration000087RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM presence_history WHERE sender_id = $1`, user.ID))
		assert.Zero(t, migration000087RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM presence_settings_pending_operations WHERE user_id = $1`, user.ID))
	})
}

var migration000087SettingsColumns = []string{
	"presence_settings_version",
	"presence_settings_operation_id",
	"activity_history_enabled",
	"activity_history_retention_days",
	"activity_history_consent_version",
	"activity_history_consent_copy_hash",
	"activity_history_consented_at",
	"activity_history_reconsent_required",
}

func migration000087InsertHistory(
	db *sql.DB,
	senderID string,
	category string,
	payloadVersion int,
	payload string,
	startedAt time.Time,
	endedAt any,
	recordedAt time.Time,
	expiresAt time.Time,
) error {
	_, err := db.ExecContext(context.Background(), `
		INSERT INTO presence_history (
			id,
			sender_id,
			category,
			payload_version,
			payload,
			started_at,
			ended_at,
			recorded_at,
			expires_at
		) VALUES ($1, $2, $3, $4, $5::JSONB, $6, $7, $8, $9)
	`, uuid.NewString(), senderID, category, payloadVersion, payload,
		startedAt, endedAt, recordedAt, expiresAt)
	return err
}

func migration000087TableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	qualifiedTable, ok := map[string]string{
		"presence_history":                     "public.presence_history",
		"presence_settings_pending_operations": "public.presence_settings_pending_operations",
	}[table]
	require.True(t, ok, "unexpected migration table %q", table)
	var relation sql.NullString
	require.NoError(t, db.QueryRow(`SELECT to_regclass($1)`, qualifiedTable).Scan(&relation))
	return relation.Valid
}

func migration000087ColumnExists(t *testing.T, db *sql.DB, table, column string) bool {
	t.Helper()

	var exists bool
	require.NoError(t, db.QueryRow(`
		SELECT EXISTS (
			SELECT 1
			FROM information_schema.columns
			WHERE table_schema = 'public'
				AND table_name = $1
				AND column_name = $2
		)
	`, table, column).Scan(&exists))
	return exists
}

func migration000087Columns(t *testing.T, db *sql.DB, table string) string {
	t.Helper()

	var columns string
	require.NoError(t, db.QueryRow(`
		SELECT string_agg(column_name, ',' ORDER BY ordinal_position)
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1
	`, table).Scan(&columns))
	return columns
}

func migration000087PrimaryKeyColumns(t *testing.T, db *sql.DB, table string) string {
	t.Helper()

	var columns string
	require.NoError(t, db.QueryRow(`
		SELECT string_agg(attribute.attname, ',' ORDER BY key_column.ordinality)
		FROM pg_constraint AS constraint_definition
		JOIN pg_class AS relation ON relation.oid = constraint_definition.conrelid
		JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		CROSS JOIN LATERAL unnest(constraint_definition.conkey)
			WITH ORDINALITY AS key_column(attnum, ordinality)
		JOIN pg_attribute AS attribute
			ON attribute.attrelid = relation.oid AND attribute.attnum = key_column.attnum
		WHERE namespace.nspname = 'public'
			AND relation.relname = $1
			AND constraint_definition.contype = 'p'
	`, table).Scan(&columns))
	return columns
}

func migration000087ForeignKeyDeleteAction(t *testing.T, db *sql.DB, table, constraint string) string {
	t.Helper()

	var action string
	require.NoError(t, db.QueryRow(`
		SELECT constraint_definition.confdeltype::TEXT
		FROM pg_constraint AS constraint_definition
		JOIN pg_class AS relation ON relation.oid = constraint_definition.conrelid
		JOIN pg_namespace AS namespace ON namespace.oid = relation.relnamespace
		WHERE namespace.nspname = 'public'
			AND relation.relname = $1
			AND constraint_definition.conname = $2
			AND constraint_definition.contype = 'f'
	`, table, constraint).Scan(&action))
	return action
}

func migration000087IndexDefinition(t *testing.T, db *sql.DB, index string) string {
	t.Helper()

	var definition string
	require.NoError(t, db.QueryRow(`
		SELECT indexdef
		FROM pg_indexes
		WHERE schemaname = 'public' AND indexname = $1
	`, index).Scan(&definition))
	return definition
}

func migration000087RequireConstraint(t *testing.T, err error, name string) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL constraint error, got %T", err)
	assert.Equal(t, name, pqErr.Constraint)
}

func migration000087RowCount(t *testing.T, db *sql.DB, query string, arg string) int {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRow(query, arg).Scan(&count))
	return count
}

func migration000087RequireOrderedDown(t *testing.T, contents string) {
	t.Helper()

	steps := []string{
		"DROP TABLE presence_settings_pending_operations",
		"DROP TABLE presence_history",
		"DROP CONSTRAINT user_presence_settings_history_consent_check",
		"DROP COLUMN activity_history_reconsent_required",
	}
	previous := -1
	for _, step := range steps {
		position := strings.Index(contents, step)
		require.NotEqualf(t, -1, position, "down migration must contain %q", step)
		assert.Greater(t, position, previous, "down migration step %q is out of order", step)
		previous = position
	}
}

func migration000087SQL(t *testing.T, direction string) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations",
		"000087_presence_history."+direction+".sql")
	// #nosec G304 -- path is constructed from runtime.Caller and a fixed migration filename.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
