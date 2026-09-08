//go:build integration

package expiration_test

import (
	"context"
	"database/sql"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	controldatabase "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/database"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/golang-migrate/migrate/v4"
	"github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMessageExpirationMigrationsRoundTrip(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	ctx := context.Background()

	version, dirty := migrationVersion(t, db)
	require.GreaterOrEqual(t, version, int64(129), "SetupTestDB must apply the schema through migration 000129")
	require.False(t, dirty)
	assertExpirationColumns(t, db)
	assertExpirationIndexes(t, db)
	runIsolatedMigrationRoundTrip(t)

	ownerID := testdb.CreateUser(t, db)
	serverID := uuid.New()
	channelID := uuid.New()
	conversationID := uuid.New()
	otherUserID := testdb.CreateUser(t, db)
	_, err := db.ExecContext(ctx, `INSERT INTO servers (id, name, owner_id) VALUES ($1, 'expiration migration server', $2)`, serverID, ownerID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, 'expiration migration channel', 'text')`, channelID, serverID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO dm_conversations (id, is_group, is_personal, created_by) VALUES ($1, false, true, $2)`, conversationID, ownerID)
	require.NoError(t, err)
	_, err = db.ExecContext(ctx, `INSERT INTO dm_participants (conversation_id, user_id) VALUES ($1, $2), ($1, $3)`, conversationID, ownerID, otherUserID)
	require.NoError(t, err)

	for _, window := range []int{3600, 86400, 604800, 2592000} {
		_, err = db.ExecContext(ctx, `UPDATE channels SET expiration_window_seconds = $2 WHERE id = $1`, channelID, window)
		require.NoErrorf(t, err, "channel window %d must be accepted", window)
		_, err = db.ExecContext(ctx, `UPDATE dm_conversations SET expiration_window_seconds = $2 WHERE id = $1`, conversationID, window)
		require.NoErrorf(t, err, "DM window %d must be accepted", window)
	}
	for _, invalid := range []int{-1, 0, 3599, 3601, 86399, 604801, 2592001} {
		_, err = db.ExecContext(ctx, `UPDATE channels SET expiration_window_seconds = $2 WHERE id = $1`, channelID, invalid)
		assert.Error(t, err, "channel window %d must be rejected", invalid)
		_, err = db.ExecContext(ctx, `UPDATE dm_conversations SET expiration_window_seconds = $2 WHERE id = $1`, conversationID, invalid)
		assert.Error(t, err, "DM window %d must be rejected", invalid)
	}

	for _, mode := range []string{"apply", "clear"} {
		_, err = db.ExecContext(ctx, `UPDATE channels SET expiration_backfill_mode = $2 WHERE id = $1`, channelID, mode)
		require.NoErrorf(t, err, "channel mode %q must be accepted", mode)
		_, err = db.ExecContext(ctx, `UPDATE dm_conversations SET expiration_backfill_mode = $2 WHERE id = $1`, conversationID, mode)
		require.NoErrorf(t, err, "DM mode %q must be accepted", mode)
	}
	_, err = db.ExecContext(ctx, `UPDATE channels SET expiration_backfill_mode = 'invalid' WHERE id = $1`, channelID)
	assert.Error(t, err, "invalid channel backfill mode must be rejected")
	_, err = db.ExecContext(ctx, `UPDATE dm_conversations SET expiration_backfill_mode = 'invalid' WHERE id = $1`, conversationID)
	assert.Error(t, err, "invalid DM backfill mode must be rejected")
}

func runIsolatedMigrationRoundTrip(t *testing.T) {
	t.Helper()
	baseURL := testdb.DatabaseURL()
	parsed, err := url.Parse(baseURL)
	require.NoError(t, err)
	databaseName := "concord_expiration_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	quotedName := pq.QuoteIdentifier(databaseName)
	adminDB, err := sql.Open("postgres", baseURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
		if _, cleanupErr := adminDB.Exec(`DROP DATABASE IF EXISTS ` + quotedName + ` WITH (FORCE)`); cleanupErr != nil {
			t.Errorf("drop disposable database %s: %v", databaseName, cleanupErr)
		}
		if cleanupErr := adminDB.Close(); cleanupErr != nil {
			t.Errorf("close disposable database admin connection: %v", cleanupErr)
		}
	})
	require.NoError(t, adminDB.Ping())
	// nosemgrep: go.lang.security.audit.database.string-formatted-query.string-formatted-query -- UUID-derived database name is safely quoted with pq.QuoteIdentifier.
	_, err = adminDB.Exec(`CREATE DATABASE ` + quotedName)
	require.NoError(t, err)

	parsed.Path = "/" + databaseName
	disposableURL := parsed.String()
	db, err := sql.Open("postgres", disposableURL)
	require.NoError(t, err)
	t.Cleanup(func() {
		if cleanupErr := db.Close(); cleanupErr != nil {
			t.Errorf("close disposable migration database: %v", cleanupErr)
		}
	})
	require.NoError(t, db.Ping())

	_, filename, _, ok := runtime.Caller(0)
	require.True(t, ok)
	migrationDir := filepath.Join(filepath.Dir(filename), "..", "..", "migrations")
	driver, err := postgres.WithInstance(db, &postgres.Config{})
	require.NoError(t, err)
	m, err := migrate.NewWithDatabaseInstance("file://"+migrationDir, "postgres", driver)
	require.NoError(t, err)
	t.Cleanup(func() {
		sourceErr, databaseErr := m.Close()
		if sourceErr != nil {
			t.Errorf("close disposable migration source: %v", sourceErr)
		}
		if databaseErr != nil {
			t.Errorf("close disposable migration database driver: %v", databaseErr)
		}
	})

	require.NoError(t, m.Migrate(126), "disposable database must migrate to the pre-expiration rail")
	version, dirty := migrationVersion(t, db)
	require.Equal(t, int64(126), version)
	require.False(t, dirty)

	require.NoError(t, m.Steps(3), "migrations 000127-000129 must apply")
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(129), version)
	require.False(t, dirty)
	assertExpirationColumns(t, db)
	assertExpirationIndexes(t, db)

	// 000130 installs the broader reason check without validation; 000131
	// validates it separately, and its down restores NOT VALID.
	require.NoError(t, m.Steps(1))
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(130), version)
	require.False(t, dirty)
	assertPurgeReasonConstraint(t, db, false)

	evidenceID := uuid.NewString()
	contextID := uuid.NewString()
	_, err = db.Exec(`
		INSERT INTO message_purges (id, context_type, context_id, range_to, reason)
		VALUES ($1, 'channel', $2, $3, 'expiry')`, evidenceID, contextID,
		time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC))
	require.NoError(t, err)

	require.NoError(t, m.Steps(1))
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(131), version)
	require.False(t, dirty)
	assertPurgeReasonConstraint(t, db, true)

	require.NoError(t, m.Steps(-1))
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(130), version)
	require.False(t, dirty)
	assertPurgeReasonConstraint(t, db, false)

	err = m.Steps(-1)
	require.Error(t, err, "000130 down must refuse to discard expiry evidence")
	var evidenceCount int
	require.NoError(t, db.QueryRow(`SELECT count(*) FROM message_purges WHERE id = $1`, evidenceID).Scan(&evidenceCount))
	assert.Equal(t, 1, evidenceCount, "failed downgrade must leave expiry evidence intact")
	assertPurgeReasonConstraint(t, db, false)
	// Reset the dirty marker only after asserting preservation, so the existing
	// 127-129 rollback checks can continue on this disposable database.
	_, err = db.Exec(`UPDATE schema_migrations SET version = 130, dirty = FALSE`)
	require.NoError(t, err)
	_, err = db.Exec(`DELETE FROM message_purges WHERE id = $1`, evidenceID)
	require.NoError(t, err)
	require.NoError(t, m.Steps(-1), "000130 down should succeed once expiry evidence is gone")
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(129), version)
	require.False(t, dirty)
	_, err = db.Exec(`
		INSERT INTO message_purges (context_type, context_id, reason)
		VALUES ('channel', $1, 'manual')`, uuid.NewString())
	require.NoError(t, err, "legacy purge reasons must remain accepted after downgrade")
	_, err = db.Exec(`
		INSERT INTO message_purges (context_type, context_id, reason)
		VALUES ('channel', $1, 'expiry')`, uuid.NewString())
	assert.Error(t, err, "expiry reason must be rejected after downgrade")

	t.Chdir(filepath.Join(filepath.Dir(filename), "..", ".."))
	for _, dirtyVersion := range []int64{128, 129} {
		_, err = db.Exec(`UPDATE schema_migrations SET version = $1, dirty = TRUE`, dirtyVersion)
		require.NoError(t, err)
		runErr := controldatabase.RunMigrations(db)
		require.Error(t, runErr)
		assert.Contains(t, runErr.Error(), "is dirty")
		version, dirty = migrationVersion(t, db)
		assert.Equal(t, dirtyVersion, version)
		assert.True(t, dirty)
		var messagesIndex, dmMessagesIndex bool
		require.NoError(t, db.QueryRow(`SELECT to_regclass('idx_messages_expires_at') IS NOT NULL`).Scan(&messagesIndex))
		require.NoError(t, db.QueryRow(`SELECT to_regclass('idx_dm_messages_expires_at') IS NOT NULL`).Scan(&dmMessagesIndex))
		assert.True(t, messagesIndex)
		assert.True(t, dmMessagesIndex)
		_, err = db.Exec(`UPDATE schema_migrations SET version = 129, dirty = FALSE`)
		require.NoError(t, err)
	}

	require.NoError(t, m.Steps(-3), "migrations 000127-000129 must roll back")
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(126), version)
	require.False(t, dirty)
	assertExpirationObjectsAbsent(t, db)

	require.NoError(t, m.Steps(3), "migrations 000127-000129 must re-apply")
	version, dirty = migrationVersion(t, db)
	require.Equal(t, int64(129), version)
	require.False(t, dirty)
	assertExpirationColumns(t, db)
	assertExpirationIndexes(t, db)
}

func migrationVersion(t *testing.T, db *sql.DB) (int64, bool) {
	t.Helper()
	var version int64
	var dirty bool
	err := db.QueryRow(`SELECT version, dirty FROM schema_migrations`).Scan(&version, &dirty)
	require.NoError(t, err)
	return version, dirty
}

func assertPurgeReasonConstraint(t *testing.T, db *sql.DB, validated bool) {
	t.Helper()
	var got bool
	require.NoError(t, db.QueryRow(`
		SELECT convalidated
		FROM pg_constraint
		WHERE conrelid = 'message_purges'::regclass AND conname = 'message_purges_reason_check'
	`).Scan(&got))
	assert.Equal(t, validated, got)
}

func assertExpirationObjectsAbsent(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"messages", "dm_messages", "channels", "dm_conversations"} {
		for _, column := range []string{"expires_at", "expiration_window_seconds", "expiration_updated_at", "expiration_revision", "expiration_backfill_mode", "expiration_backfill_cutoff"} {
			var exists bool
			err := db.QueryRow(`SELECT EXISTS (
				SELECT 1 FROM information_schema.columns WHERE table_name = $1 AND column_name = $2
			)`, table, column).Scan(&exists)
			require.NoError(t, err)
			assert.False(t, exists, "%s.%s must be removed by rollback", table, column)
		}
	}
	for _, index := range []string{"idx_messages_expires_at", "idx_dm_messages_expires_at"} {
		var exists bool
		require.NoError(t, db.QueryRow(`SELECT to_regclass($1) IS NOT NULL`, index).Scan(&exists))
		assert.False(t, exists, "%s must be removed by rollback", index)
	}
}

func assertExpirationColumns(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, table := range []string{"messages", "dm_messages"} {
		assertColumnType(t, db, table, "expires_at", "timestamp with time zone")
		assertColumnShape(t, db, table, "expires_at", true, false)
	}
	for _, table := range []string{"channels", "dm_conversations"} {
		assertColumnType(t, db, table, "expiration_window_seconds", "integer")
		assertColumnShape(t, db, table, "expiration_window_seconds", true, false)
		assertColumnType(t, db, table, "expiration_updated_at", "timestamp with time zone")
		assertColumnShape(t, db, table, "expiration_updated_at", true, false)
		assertColumnType(t, db, table, "expiration_revision", "bigint")
		assertColumnShape(t, db, table, "expiration_revision", false, true)
		assertColumnType(t, db, table, "expiration_backfill_mode", "text")
		assertColumnShape(t, db, table, "expiration_backfill_mode", true, false)
		assertColumnType(t, db, table, "expiration_backfill_cutoff", "timestamp with time zone")
		assertColumnShape(t, db, table, "expiration_backfill_cutoff", true, false)
	}
}

func assertColumnType(t *testing.T, db *sql.DB, table, column, want string) {
	t.Helper()
	var got string
	require.NoError(t, db.QueryRow(`SELECT data_type FROM information_schema.columns WHERE table_name = $1 AND column_name = $2`, table, column).Scan(&got))
	assert.Equal(t, want, got, "%s.%s type", table, column)
}

func assertColumnShape(t *testing.T, db *sql.DB, table, column string, nullable, defaultZero bool) {
	t.Helper()
	var isNullable string
	var columnDefault sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = $1 AND column_name = $2
	`, table, column).Scan(&isNullable, &columnDefault))
	assert.Equal(t, nullable, isNullable == "YES", "%s.%s nullability", table, column)
	if defaultZero {
		require.True(t, columnDefault.Valid, "%s.%s must have a default", table, column)
		assert.Contains(t, columnDefault.String, "0", "%s.%s default", table, column)
	} else {
		assert.False(t, columnDefault.Valid, "%s.%s must not have a default", table, column)
	}
}

func assertExpirationIndexes(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, index := range []string{"idx_messages_expires_at", "idx_dm_messages_expires_at"} {
		var valid, partial bool
		err := db.QueryRow(`
			SELECT i.indisvalid, i.indpred IS NOT NULL
			FROM pg_class c JOIN pg_index i ON i.indexrelid = c.oid
			WHERE c.relname = $1
		`, index).Scan(&valid, &partial)
		require.NoError(t, err, "index %s must exist", index)
		assert.True(t, valid, "%s must be valid", index)
		assert.True(t, partial, "%s must be partial", index)
	}
}
