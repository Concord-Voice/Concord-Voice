package database_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000093_BackfillDefaultsAndGuardedRollback(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()

	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertColumn(t, ts.DB, table)
	}

	upSQL := strings.Join([]string{
		migration000093ReadFile(t, "000093_voice_lifecycle_watermarks.up.sql"),
		migration000093ReadFile(t, "000094_backfill_voice_lifecycle_watermarks.up.sql"),
		migration000093ReadFile(t, "000095_guard_voice_lifecycle_watermarks_not_null.up.sql"),
		migration000093ReadFile(t, "000096_validate_voice_lifecycle_watermarks_not_null.up.sql"),
		migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.up.sql"),
	}, "\n")
	setupDownSQL := migration000093ReadFile(t, "000093_voice_lifecycle_watermarks.down.sql")
	downSQL := strings.Join([]string{
		migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000096_validate_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000095_guard_voice_lifecycle_watermarks_not_null.down.sql"),
		migration000093ReadFile(t, "000094_backfill_voice_lifecycle_watermarks.down.sql"),
		setupDownSQL,
	}, "\n")
	migrationApplied := true
	t.Cleanup(func() {
		if migrationApplied {
			return
		}
		_, err := ts.DB.ExecContext(context.Background(), upSQL)
		assert.NoError(t, err, "restore migration 000093 after rollback test")
	})

	_, err := ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err)
	migrationApplied = false

	owner := ts.CreateTestUser(t, "voice_lifecycle_owner")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice lifecycle migration")
	channelID := ts.CreateTestChannel(t, serverID, "voice-lifecycle")
	conversationID := uuid.NewString()
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`,
		conversationID, owner.ID)
	require.NoError(t, err)

	serverJoinedAt := time.Date(2026, 7, 1, 2, 3, 4, 0, time.UTC)
	privateJoinedAt := time.Date(2026, 7, 2, 3, 4, 5, 0, time.UTC)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO voice_participants (channel_id, user_id, joined_at)
		VALUES ($1, $2, $3)
	`, channelID, owner.ID, serverJoinedAt)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO dm_voice_participants (conversation_id, user_id, joined_at)
		VALUES ($1, $2, $3)
	`, conversationID, owner.ID, privateJoinedAt)
	require.NoError(t, err)

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	migrationApplied = true

	var serverLifecycleAt, privateLifecycleAt time.Time
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT lifecycle_event_at
		FROM voice_participants
		WHERE channel_id = $1 AND user_id = $2
	`, channelID, owner.ID).Scan(&serverLifecycleAt))
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT lifecycle_event_at
		FROM dm_voice_participants
		WHERE conversation_id = $1 AND user_id = $2
	`, conversationID, owner.ID).Scan(&privateLifecycleAt))
	assert.WithinDuration(t, serverJoinedAt, serverLifecycleAt, 0)
	assert.WithinDuration(t, privateJoinedAt, privateLifecycleAt, 0)

	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertColumn(t, ts.DB, table)
	}

	defaultUser := ts.CreateTestUser(t, "voice_lifecycle_default")
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, defaultUser.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx,
		`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`,
		conversationID, defaultUser.ID)
	require.NoError(t, err)

	for _, query := range []struct {
		sql      string
		parentID string
	}{
		{
			sql:      `SELECT joined_at, lifecycle_event_at FROM voice_participants WHERE channel_id = $1 AND user_id = $2`,
			parentID: channelID,
		},
		{
			sql:      `SELECT joined_at, lifecycle_event_at FROM dm_voice_participants WHERE conversation_id = $1 AND user_id = $2`,
			parentID: conversationID,
		},
	} {
		var joinedAt, lifecycleAt time.Time
		require.NoError(t, ts.DB.QueryRowContext(ctx, query.sql,
			query.parentID, defaultUser.ID).Scan(&joinedAt, &lifecycleAt))
		assert.WithinDuration(t, joinedAt, lifecycleAt, 0)
	}

	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.ErrorContains(t, err, "while voice participants are active")
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertColumn(t, ts.DB, table)
	}

	_, err = ts.DB.ExecContext(ctx,
		`DELETE FROM dm_voice_participants WHERE user_id IN ($1, $2)`,
		owner.ID, defaultUser.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.ErrorContains(t, err, "while voice participants are active",
		"server voice participants alone must block rollback")
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertColumn(t, ts.DB, table)
	}

	_, err = ts.DB.ExecContext(ctx,
		`DELETE FROM voice_participants WHERE user_id IN ($1, $2)`,
		owner.ID, defaultUser.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, `
		INSERT INTO dm_voice_participants (conversation_id, user_id)
		VALUES ($1, $2)
	`, conversationID, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.ErrorContains(t, err, "while voice participants are active",
		"private-call participants alone must block rollback")
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		migration000093AssertColumn(t, ts.DB, table)
	}

	_, err = ts.DB.ExecContext(ctx,
		`DELETE FROM dm_voice_participants WHERE user_id = $1`, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "down migration must succeed after both participant tables drain")
	migrationApplied = false
	_, err = ts.DB.ExecContext(ctx, setupDownSQL)
	require.NoError(t, err, "migration 000093 down must be idempotent")
	for _, table := range []string{"voice_participants", "dm_voice_participants"} {
		var count int
		require.NoError(t, ts.DB.QueryRowContext(ctx, `
			SELECT COUNT(*)
			FROM information_schema.columns
			WHERE table_schema = 'public'
			  AND table_name = $1
			  AND column_name = 'lifecycle_event_at'
		`, table).Scan(&count))
		assert.Zero(t, count)
	}

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err, "up migration must reapply after rollback")
	migrationApplied = true
}

func migration000093AssertColumn(t *testing.T, db *sql.DB, table string) {
	t.Helper()

	var dataType, nullable, defaultExpression, comment string
	err := db.QueryRow(`
		SELECT c.data_type,
		       c.is_nullable,
		       c.column_default,
		       col_description(format('%I.%I', c.table_schema, c.table_name)::regclass, c.ordinal_position)
		FROM information_schema.columns c
		WHERE c.table_schema = 'public'
		  AND c.table_name = $1
		  AND c.column_name = 'lifecycle_event_at'
	`, table).Scan(&dataType, &nullable, &defaultExpression, &comment)
	require.NoErrorf(t, err, "%s.lifecycle_event_at must exist", table)
	assert.Equal(t, "timestamp with time zone", dataType)
	assert.Equal(t, "NO", nullable)
	assert.Equal(t, "now()", defaultExpression)
	assert.NotEmpty(t, comment)
}

func TestMigration000093_PhasesSeparateExclusiveAndScanningWork(t *testing.T) {
	t.Parallel()

	setup := migration000093ReadFile(t, "000093_voice_lifecycle_watermarks.up.sql")
	backfill := migration000093ReadFile(t, "000094_backfill_voice_lifecycle_watermarks.up.sql")
	guard := migration000093ReadFile(t, "000095_guard_voice_lifecycle_watermarks_not_null.up.sql")
	validate := migration000093ReadFile(t, "000096_validate_voice_lifecycle_watermarks_not_null.up.sql")
	enforce := migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.up.sql")
	enforceDown := migration000093ReadFile(t, "000097_enforce_voice_lifecycle_watermarks_not_null.down.sql")
	cleanup := migration000093ReadFile(t, "000098_add_activity_settings_cleanup_marker.up.sql")

	assert.NotContains(t, setup, "CHECK (lifecycle_event_at IS NOT NULL) NOT VALID")
	assert.NotContains(t, setup, "ADD CONSTRAINT")
	assert.NotContains(t, setup, "UPDATE voice_participants")
	assert.NotContains(t, setup, "UPDATE dm_voice_participants")
	assert.NotContains(t, setup, "SET NOT NULL")

	assert.Contains(t, backfill, "SET lifecycle_event_at = joined_at")
	assert.NotContains(t, backfill, "VALIDATE CONSTRAINT")
	assert.NotContains(t, backfill, "ADD COLUMN")
	assert.NotContains(t, backfill, "ADD CONSTRAINT")
	assert.NotContains(t, backfill, "SET NOT NULL")

	assert.Equal(t, 2, strings.Count(guard,
		"CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;"))
	assert.NotContains(t, guard, "VALIDATE CONSTRAINT")
	assert.NotContains(t, guard, "UPDATE ")

	assert.Equal(t, 2, strings.Count(validate, "VALIDATE CONSTRAINT"))
	assert.NotContains(t, validate, "ADD CONSTRAINT")
	assert.NotContains(t, validate, "UPDATE ")

	assert.Equal(t, 2, strings.Count(enforce,
		"ALTER COLUMN lifecycle_event_at SET NOT NULL;"))
	assert.Contains(t, enforce,
		"DROP CONSTRAINT voice_participants_lifecycle_event_at_not_null;")
	assert.Contains(t, enforce,
		"DROP CONSTRAINT dm_voice_participants_lifecycle_event_at_not_null;")
	assert.NotContains(t, enforce, "UPDATE ")
	assert.Equal(t, 2, strings.Count(enforce, "VALIDATE CONSTRAINT"))
	assert.NotContains(t, enforce, "ADD CONSTRAINT")
	assert.Less(t,
		strings.LastIndex(enforce, "VALIDATE CONSTRAINT"),
		strings.Index(enforce, "ALTER COLUMN lifecycle_event_at SET NOT NULL;"),
		"both validations must finish before the first ACCESS EXCLUSIVE operation",
	)
	assert.Equal(t, 2, strings.Count(enforceDown,
		"CHECK (lifecycle_event_at IS NOT NULL) NOT VALID;"))
	assert.Equal(t, 2, strings.Count(enforceDown,
		"ALTER COLUMN lifecycle_event_at DROP NOT NULL;"))
	assert.NotContains(t, enforceDown, "VALIDATE CONSTRAINT")

	assert.Contains(t, cleanup, "ON DELETE RESTRICT")
}

func TestMigration000093_PhaseFilesAreSequentialAndPaired(t *testing.T) {
	t.Parallel()

	for _, base := range []string{
		"000093_voice_lifecycle_watermarks",
		"000094_backfill_voice_lifecycle_watermarks",
		"000095_guard_voice_lifecycle_watermarks_not_null",
		"000096_validate_voice_lifecycle_watermarks_not_null",
		"000097_enforce_voice_lifecycle_watermarks_not_null",
		"000098_add_activity_settings_cleanup_marker",
	} {
		assert.NotEmpty(t, migration000093ReadFile(t, base+".up.sql"))
		assert.NotEmpty(t, migration000093ReadFile(t, base+".down.sql"))
	}

	_, err := os.Stat(migration000093FilePath(
		t, "000096_add_activity_settings_cleanup_marker.up.sql",
	))
	assert.ErrorIs(t, err, os.ErrNotExist)
}

func migration000093ReadFile(t *testing.T, filename string) string {
	t.Helper()
	contents, err := os.ReadFile(migration000093FilePath(t, filename))
	require.NoError(t, err)
	return string(contents)
}

func migration000093FilePath(t *testing.T, filename string) string {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok, "resolve migration test path")
	return filepath.Join(
		filepath.Dir(currentFile), "..", "..", "migrations", filename,
	)
}
