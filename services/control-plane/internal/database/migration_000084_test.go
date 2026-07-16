package database_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000084_UpDownReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	preferenceExists := migration000084TableExists(t, ts.DB, "presence_override_preferences")
	overridesExist := migration000084TableExists(t, ts.DB, "user_presence_overrides")
	assert.True(t, preferenceExists, "up migration must create presence_override_preferences")
	assert.True(t, overridesExist, "up migration must create user_presence_overrides")
	if !preferenceExists || !overridesExist {
		return
	}

	_, err := ts.DB.ExecContext(ctx, migration000084SQL(t, "down"))
	require.NoError(t, err, "down migration must drop enforcement before preferences")
	assert.False(t, migration000084TableExists(t, ts.DB, "user_presence_overrides"))
	assert.False(t, migration000084TableExists(t, ts.DB, "presence_override_preferences"))

	_, err = ts.DB.ExecContext(ctx, migration000084SQL(t, "up"))
	require.NoError(t, err, "up migration must re-apply after a rollback")
	assert.True(t, migration000084TableExists(t, ts.DB, "presence_override_preferences"))
	assert.True(t, migration000084TableExists(t, ts.DB, "user_presence_overrides"))

	var indexDefinition string
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_user_presence_overrides_target'`,
	).Scan(&indexDefinition))
	assert.Contains(t, indexDefinition, "(target_user_id)")
}

func TestMigration000084_ConstraintsAndCascades(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	assert.Equal(t, "user_id,category", migration000084PrimaryKeyColumns(t, ts.DB, "presence_override_preferences"))
	assert.Equal(t, "sender_id,category,target_user_id", migration000084PrimaryKeyColumns(t, ts.DB, "user_presence_overrides"))

	var senderDeleteAction string
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT confdeltype::TEXT
		FROM pg_constraint
		WHERE conrelid = 'user_presence_overrides'::regclass
			AND conname = 'user_presence_overrides_sender_id_fkey'
			AND contype = 'f'
	`).Scan(&senderDeleteAction))
	assert.Equal(t, "c", senderDeleteAction, "sender FK must use ON DELETE CASCADE")

	sender := ts.CreateTestUser(t, "presence_override_sender")
	target := ts.CreateTestUser(t, "presence_override_target")
	_, err := ts.DB.ExecContext(ctx,
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		 VALUES ($1, 'custom_text', 'dGVzdA==')`, sender.ID)
	require.NoError(t, err)

	var version int
	require.NoError(t, ts.DB.QueryRowContext(ctx,
		`SELECT version FROM presence_override_preferences
		 WHERE user_id = $1 AND category = 'custom_text'`, sender.ID,
	).Scan(&version))
	assert.Equal(t, 1, version, "preference version must default to one")

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		 VALUES ($1, 'custom_text', 'ZHVwbGljYXRl')`, sender.ID)
	migration000084RequireConstraint(t, err, "presence_override_preferences_pkey")

	invalidVersionUser := ts.CreateTestUser(t, "presence_override_invalid_version")
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data, version)
		 VALUES ($1, 'custom_text', 'dGVzdA==', 0)`, invalidVersionUser.ID)
	migration000084RequireConstraint(t, err, "presence_override_preferences_version_positive")

	invalidCategoryUser := ts.CreateTestUser(t, "presence_override_invalid_category")
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		 VALUES ($1, 'music', 'dGVzdA==')`, invalidCategoryUser.ID)
	migration000084RequireConstraint(t, err, "presence_override_preferences_category_check")

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'custom_text', $2)`, sender.ID, target.ID)
	require.NoError(t, err)

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'music', $2)`, sender.ID, target.ID)
	migration000084RequireConstraint(t, err, "user_presence_overrides_category_check")

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'custom_text', $1)`, sender.ID)
	migration000084RequireConstraint(t, err, "user_presence_overrides_not_self_check")

	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'custom_text', $2)`, sender.ID, target.ID)
	migration000084RequireConstraint(t, err, "user_presence_overrides_pkey")

	t.Run("sender deletion cascades preferences and enforcement targets", func(t *testing.T) {
		cascadeSender := ts.CreateTestUser(t, "presence_override_sender_cascade")
		cascadeTarget := ts.CreateTestUser(t, "presence_override_sender_cascade_target")
		migration000084InsertPreferenceAndOverride(t, ts.DB, cascadeSender.ID, cascadeTarget.ID)

		_, err := ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, cascadeSender.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, migration000084RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, cascadeSender.ID))
		assert.Equal(t, 0, migration000084RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM user_presence_overrides WHERE sender_id = $1`, cascadeSender.ID))
	})

	t.Run("target deletion cascades only the enforcement target", func(t *testing.T) {
		cascadeSender := ts.CreateTestUser(t, "presence_override_target_cascade_sender")
		cascadeTarget := ts.CreateTestUser(t, "presence_override_target_cascade")
		migration000084InsertPreferenceAndOverride(t, ts.DB, cascadeSender.ID, cascadeTarget.ID)

		_, err := ts.DB.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, cascadeTarget.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, migration000084RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM user_presence_overrides WHERE target_user_id = $1`, cascadeTarget.ID))
		assert.Equal(t, 1, migration000084RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM presence_override_preferences WHERE user_id = $1`, cascadeSender.ID))
	})

	t.Run("preference deletion cascades its enforcement targets", func(t *testing.T) {
		cascadeSender := ts.CreateTestUser(t, "presence_override_preference_cascade")
		cascadeTarget := ts.CreateTestUser(t, "presence_override_preference_cascade_target")
		migration000084InsertPreferenceAndOverride(t, ts.DB, cascadeSender.ID, cascadeTarget.ID)

		_, err := ts.DB.ExecContext(ctx,
			`DELETE FROM presence_override_preferences
			 WHERE user_id = $1 AND category = 'custom_text'`, cascadeSender.ID)
		require.NoError(t, err)
		assert.Equal(t, 0, migration000084RowCount(t, ts.DB,
			`SELECT COUNT(*) FROM user_presence_overrides WHERE sender_id = $1`, cascadeSender.ID))
	})
}

func migration000084TableExists(t *testing.T, db *sql.DB, table string) bool {
	t.Helper()

	const tableExistsQuery = `SELECT to_regclass($1)`
	qualifiedTable, ok := map[string]string{
		"presence_override_preferences": "public.presence_override_preferences",
		"user_presence_overrides":       "public.user_presence_overrides",
	}[table]
	require.True(t, ok, "unexpected migration table %q", table)
	var relation sql.NullString
	require.NoError(t, db.QueryRow(tableExistsQuery, qualifiedTable).Scan(&relation))
	return relation.Valid
}

func migration000084PrimaryKeyColumns(t *testing.T, db *sql.DB, table string) string {
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

func migration000084RequireConstraint(t *testing.T, err error, name string) {
	t.Helper()
	require.Error(t, err)

	var pqErr *pq.Error
	require.True(t, errors.As(err, &pqErr), "expected PostgreSQL constraint error, got %T", err)
	assert.Equal(t, name, pqErr.Constraint)
}

func migration000084InsertPreferenceAndOverride(t *testing.T, db *sql.DB, senderID, targetID string) {
	t.Helper()
	ctx := context.Background()

	_, err := db.ExecContext(ctx,
		`INSERT INTO presence_override_preferences (user_id, category, encrypted_data)
		 VALUES ($1, 'custom_text', 'dGVzdA==')`, senderID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx,
		`INSERT INTO user_presence_overrides (sender_id, category, target_user_id)
		 VALUES ($1, 'custom_text', $2)`, senderID, targetID)
	require.NoError(t, err)
}

func migration000084RowCount(t *testing.T, db *sql.DB, query string, arg string) int {
	t.Helper()

	var count int
	require.NoError(t, db.QueryRow(query, arg).Scan(&count))
	return count
}

func migration000084SQL(t *testing.T, direction string) string {
	t.Helper()

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	path := filepath.Join(filepath.Dir(filename), "..", "..", "migrations",
		"000084_user_presence_overrides."+direction+".sql")
	// #nosec G304 -- path is constructed from runtime.Caller and a fixed migration filename.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
