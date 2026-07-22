package database_test

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000098_SchemaGuardedDownAndReUp(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx := context.Background()
	require.True(t, migration000098TableExists(t, ts.DB))
	upSQL := migration000098SQL(t, "up")
	migrationApplied := true
	t.Cleanup(func() {
		if migrationApplied {
			return
		}
		_, err := ts.DB.ExecContext(context.Background(), upSQL)
		assert.NoError(t, err, "restore migration 000098 after test failure")
	})

	owner := ts.CreateTestUser(t, "activity_cleanup_migration")
	operationID := uuid.New()
	_, err := ts.DB.ExecContext(ctx, `
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, '{"version":1}'::jsonb)
	`, owner.ID, operationID)
	require.NoError(t, err)

	downSQL := migration000098SQL(t, "down")
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.Error(t, err, "rollback must refuse to erase unresolved cleanup evidence")
	assert.True(t, migration000098TableExists(t, ts.DB))

	_, err = ts.DB.ExecContext(ctx,
		`DELETE FROM activity_settings_pending_cleanups WHERE user_id = $1`, owner.ID)
	require.NoError(t, err)
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err)
	migrationApplied = false
	assert.False(t, migration000098TableExists(t, ts.DB))
	_, err = ts.DB.ExecContext(ctx, downSQL)
	require.NoError(t, err, "down migration must tolerate a partially absent table")

	_, err = ts.DB.ExecContext(ctx, upSQL)
	require.NoError(t, err)
	migrationApplied = true
	require.True(t, migration000098TableExists(t, ts.DB))

	var tableComment, evidenceComment string
	require.NoError(t, ts.DB.QueryRowContext(ctx, `
		SELECT obj_description('activity_settings_pending_cleanups'::regclass),
		       col_description(
		         'activity_settings_pending_cleanups'::regclass,
		         (SELECT ordinal_position
		          FROM information_schema.columns
		          WHERE table_schema = 'public'
		            AND table_name = 'activity_settings_pending_cleanups'
		            AND column_name = 'evidence')
		       )
	`).Scan(&tableComment, &evidenceComment))
	assert.NotEmpty(t, tableComment)
	assert.NotEmpty(t, evidenceComment)
}

func TestMigration000098_DownCannotRaceCommittedCleanupEvidence(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	upSQL := migration000098SQL(t, "up")
	downSQL := migration000098SQL(t, "down")
	t.Cleanup(func() {
		var exists bool
		if err := ts.DB.QueryRow(`
			SELECT to_regclass('public.activity_settings_pending_cleanups') IS NOT NULL
		`).Scan(&exists); !assert.NoError(t, err) {
			return
		}
		if !exists {
			_, err := ts.DB.Exec(upSQL)
			assert.NoError(t, err, "restore migration 000098 after interleaving test")
			return
		}
		_, err := ts.DB.Exec(`DELETE FROM activity_settings_pending_cleanups`)
		assert.NoError(t, err)
	})
	owner := ts.CreateTestUser(t, "activity_cleanup_down_race")
	insertTx, err := ts.DB.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() {
		if rollbackErr := insertTx.Rollback(); rollbackErr != nil && rollbackErr != sql.ErrTxDone {
			t.Errorf("roll back cleanup-evidence insert: %v", rollbackErr)
		}
	})
	_, err = insertTx.ExecContext(ctx,
		`LOCK TABLE activity_settings_pending_cleanups IN ROW EXCLUSIVE MODE`)
	require.NoError(t, err)

	downConn, err := ts.DB.Conn(ctx)
	require.NoError(t, err)
	t.Cleanup(func() { assert.NoError(t, downConn.Close()) })
	var downPID int
	require.NoError(t, downConn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&downPID))
	downDone := make(chan error, 1)
	go func() {
		_, execErr := downConn.ExecContext(ctx, downSQL)
		downDone <- execErr
	}()
	require.Eventually(t, func() bool {
		var waiting bool
		err := ts.DB.QueryRowContext(ctx, `
			SELECT EXISTS (
				SELECT 1
				FROM pg_locks
				WHERE pid = $1
				  AND relation = to_regclass('public.activity_settings_pending_cleanups')
				  AND mode = 'AccessExclusiveLock'
				  AND NOT granted
			)
		`, downPID).Scan(&waiting)
		return err == nil && waiting
	}, 2*time.Second, 10*time.Millisecond,
		"down migration never reached its table-drop serialization boundary")

	_, err = insertTx.ExecContext(ctx, `
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, '{"version":1}'::jsonb)
	`, owner.ID, uuid.New())
	require.NoError(t, err)
	require.NoError(t, insertTx.Commit())

	select {
	case downErr := <-downDone:
		require.ErrorContains(t, downErr, "while cleanup evidence remains",
			"rollback must re-check under its write-conflicting lock and retain committed evidence")
	case <-time.After(2 * time.Second):
		t.Fatal("down migration remained blocked after the concurrent insert committed")
	}
	require.True(t, migration000098TableExists(t, ts.DB))
	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, owner.ID).Scan(&count))
	assert.Equal(t, 1, count)
}

func TestMigration000098_UserDeleteRetainsPendingCleanup(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	upSQL := migration000098SQL(t, "up")
	migrationApplied := true
	t.Cleanup(func() {
		if migrationApplied {
			return
		}
		_, cleanupErr := ts.DB.ExecContext(context.Background(), upSQL)
		assert.NoError(t, cleanupErr, "restore migration 000098 after user-delete test")
	})
	_, err := ts.DB.Exec(`DELETE FROM activity_settings_pending_cleanups`)
	require.NoError(t, err)
	_, err = ts.DB.Exec(migration000098SQL(t, "down"))
	require.NoError(t, err)
	migrationApplied = false
	_, err = ts.DB.Exec(upSQL)
	require.NoError(t, err)
	migrationApplied = true

	owner := ts.CreateTestUser(t, "activity_cleanup_restrict")
	_, err = ts.DB.Exec(`
		INSERT INTO activity_settings_pending_cleanups (
			user_id, operation_id, evidence
		) VALUES ($1, $2, '{"version":1}'::jsonb)
	`, owner.ID, uuid.New())
	require.NoError(t, err)

	_, err = ts.DB.Exec(`DELETE FROM users WHERE id = $1`, owner.ID)
	require.Error(t, err, "account deletion must not erase unresolved cleanup evidence")
	var count int
	require.NoError(t, ts.DB.QueryRow(`
		SELECT COUNT(*) FROM activity_settings_pending_cleanups WHERE user_id = $1
	`, owner.ID).Scan(&count))
	assert.Equal(t, 1, count)

	var userCount int
	require.NoError(t, ts.DB.QueryRow(
		`SELECT COUNT(*) FROM users WHERE id = $1`, owner.ID,
	).Scan(&userCount))
	assert.Equal(t, 1, userCount, "the parent delete must fail atomically")

	_, err = ts.DB.Exec(
		`DELETE FROM activity_settings_pending_cleanups WHERE user_id = $1`, owner.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`DELETE FROM users WHERE id = $1`, owner.ID)
	require.NoError(t, err, "account deletion may proceed after cleanup evidence is resolved")
}

func migration000098TableExists(t *testing.T, db interface {
	QueryRow(string, ...any) *sql.Row
}) bool {
	t.Helper()
	var exists bool
	require.NoError(t, db.QueryRow(`
		SELECT to_regclass('public.activity_settings_pending_cleanups') IS NOT NULL
	`).Scan(&exists))
	return exists
}

func migration000098SQL(t *testing.T, direction string) string {
	t.Helper()
	filename, ok := map[string]string{
		"up":   "000098_add_activity_settings_cleanup_marker.up.sql",
		"down": "000098_add_activity_settings_cleanup_marker.down.sql",
	}[direction]
	require.True(t, ok, "unexpected migration direction %q", direction)
	_, currentFile, _, ok := runtime.Caller(0)
	require.True(t, ok)
	path := filepath.Join(filepath.Dir(currentFile), "..", "..", "migrations", filename)
	// #nosec G304 -- fixed migration filename resolved from this test file.
	contents, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(contents)
}
