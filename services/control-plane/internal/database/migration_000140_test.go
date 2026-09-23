package database_test

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const migration000140Table = "server_voice_terminal_outbox"

func TestMigration000140_ServerVoiceTerminalOutboxContract(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	var exists bool
	require.NoError(t, db.QueryRow(`SELECT to_regclass('public.server_voice_terminal_outbox') IS NOT NULL`).Scan(&exists))
	require.True(t, exists, "positive control: migration harness must apply 000140")

	columns := map[string]struct {
		dataType, nullable, defaultSQL string
	}{}
	rows, err := db.Query(`
		SELECT column_name, data_type, is_nullable, COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = $1`, migration000140Table)
	require.NoError(t, err)
	for rows.Next() {
		var name, dataType, nullable, defaultSQL string
		require.NoError(t, rows.Scan(&name, &dataType, &nullable, &defaultSQL))
		columns[name] = struct{ dataType, nullable, defaultSQL string }{dataType, nullable, defaultSQL}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())
	require.GreaterOrEqual(t, len(columns), 6)
	for _, name := range []string{"channel_id", "user_id", "server_id", "operation_id"} {
		assert.Equal(t, "uuid", columns[name].dataType)
		assert.Equal(t, "NO", columns[name].nullable)
	}
	for _, name := range []string{"created_at", "reconcile_after"} {
		assert.Equal(t, "timestamp with time zone", columns[name].dataType)
		assert.Equal(t, "NO", columns[name].nullable)
		assert.Contains(t, columns[name].defaultSQL, "clock_timestamp()")
	}

	var primaryKey, uniqueKey, checkConstraint string
	require.NoError(t, db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'p'`, migration000140Table).Scan(&primaryKey))
	require.NoError(t, db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'u'`, migration000140Table).Scan(&uniqueKey))
	require.NoError(t, db.QueryRow(`SELECT pg_get_constraintdef(oid) FROM pg_constraint WHERE conrelid = $1::regclass AND contype = 'c'`, migration000140Table).Scan(&checkConstraint))
	assert.Equal(t, "PRIMARY KEY (channel_id, user_id)", primaryKey)
	assert.Equal(t, "UNIQUE (operation_id)", uniqueKey)
	assert.Contains(t, checkConstraint, "reconcile_after >= created_at")

	var userDeleteAction string
	require.NoError(t, db.QueryRow(`
		SELECT confdeltype::TEXT
		FROM pg_constraint
		WHERE conrelid = $1::regclass
			AND conname = 'server_voice_terminal_outbox_user_id_fkey'
			AND contype = 'f'`, migration000140Table).Scan(&userDeleteAction))
	assert.Equal(t, "c", userDeleteAction,
		"account erasure must cascade the user-owned terminal obligation")

	var indexDef string
	require.NoError(t, db.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND tablename = $1 AND indexname = 'idx_server_voice_terminal_outbox_due'`, migration000140Table).Scan(&indexDef))
	assert.Contains(t, indexDef, "(reconcile_after, created_at, channel_id, user_id)")

	var tableComment string
	require.NoError(t, db.QueryRow(`SELECT obj_description($1::regclass)`, migration000140Table).Scan(&tableComment))
	assert.Contains(t, tableComment, "Durable Server Voice terminal obligations")
	for _, column := range []string{"channel_id", "user_id", "server_id", "operation_id", "created_at", "reconcile_after"} {
		var comment sql.NullString
		require.NoError(t, db.QueryRow(`SELECT col_description($1::regclass, ordinal_position) FROM information_schema.columns WHERE table_schema = 'public' AND table_name = $2 AND column_name = $3`, migration000140Table, migration000140Table, column).Scan(&comment))
		assert.True(t, comment.Valid, "column %s must be documented", column)
	}

	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			t.Errorf("rollback migration 000140 contract transaction: %v", rollbackErr)
		}
	}()
	channelID, userID, serverID, operationID := uuid.New(), testhelpers.CreateUser(t, db), uuid.New(), uuid.New()
	_, err = tx.Exec(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES ($1, $2, $3, $4)`, channelID, userID, serverID, operationID)
	require.NoError(t, err)
	var createdAt, reconcileAfter time.Time
	require.NoError(t, tx.QueryRow(`SELECT created_at, reconcile_after FROM server_voice_terminal_outbox WHERE channel_id = $1 AND user_id = $2`, channelID, userID).Scan(&createdAt, &reconcileAfter))
	assert.False(t, createdAt.IsZero())
	assert.False(t, reconcileAfter.Before(createdAt))
	reject := func(query, wantConstraint string, args ...any) {
		t.Helper()
		_, err := tx.Exec(`SAVEPOINT migration_000140_rejection`)
		require.NoError(t, err)
		_, err = tx.Exec(query, args...)
		require.Error(t, err)
		var pqErr *pq.Error
		require.True(t, errors.As(err, &pqErr), "expected PostgreSQL constraint error, got %T", err)
		assert.Equal(t, wantConstraint, pqErr.Constraint)
		_, err = tx.Exec(`ROLLBACK TO SAVEPOINT migration_000140_rejection`)
		require.NoError(t, err)
		_, err = tx.Exec(`RELEASE SAVEPOINT migration_000140_rejection`)
		require.NoError(t, err)
	}
	reject(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES ($1, $2, $3, $4)`, "server_voice_terminal_outbox_pkey", channelID, userID, serverID, uuid.New())
	reject(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES ($1, $2, $3, $4)`, "server_voice_terminal_outbox_operation_id_key", uuid.New(), userID, serverID, operationID)
	reject(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id, created_at, reconcile_after) VALUES ($1, $2, $3, $4, $5, $6)`, "server_voice_terminal_outbox_reconcile_after_check", uuid.New(), userID, serverID, uuid.New(), time.Unix(2, 0), time.Unix(1, 0))
}

func TestMigration000140_DownGuardAndUpDownUp(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)
	down := migrationReadFile(t, "../../migrations/000140_add_server_voice_terminal_outbox.down.sql")
	up := migrationReadFile(t, "../../migrations/000140_add_server_voice_terminal_outbox.up.sql")

	lock := strings.Index(down, "LOCK TABLE public.server_voice_terminal_outbox IN ACCESS EXCLUSIVE MODE")
	check := strings.Index(down, "SELECT EXISTS (SELECT 1 FROM public.server_voice_terminal_outbox)")
	require.NotEqual(t, -1, lock, "down migration must lock the outbox")
	require.NotEqual(t, -1, check, "down migration must check for pending rows")
	assert.Less(t, lock, check, "table lock must precede emptiness check")

	t.Run("pending row refuses rollback", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback guarded down transaction: %v", rollbackErr)
			}
		}()
		_, err = tx.Exec(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES ($1, $2, $3, $4)`, uuid.New(), userID, uuid.New(), uuid.New())
		require.NoError(t, err)
		_, err = tx.Exec(down)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "undelivered Server Voice terminal obligations remain")
	})

	t.Run("empty rollback drops and up restores", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				t.Errorf("rollback up/down/up transaction: %v", rollbackErr)
			}
		}()
		_, err = tx.Exec(down)
		require.NoError(t, err)
		var exists bool
		require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.server_voice_terminal_outbox') IS NOT NULL`).Scan(&exists))
		assert.False(t, exists)
		_, err = tx.Exec(up)
		require.NoError(t, err)
		require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.server_voice_terminal_outbox') IS NOT NULL`).Scan(&exists))
		assert.True(t, exists)
		channelID, serverID, operationID := uuid.New(), uuid.New(), uuid.New()
		_, err = tx.Exec(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES ($1, $2, $3, $4)`, channelID, userID, serverID, operationID)
		require.NoError(t, err)
		_, err = tx.Exec(`DELETE FROM users WHERE id = $1`, userID)
		require.NoError(t, err)
		var pending int
		require.NoError(t, tx.QueryRow(`SELECT COUNT(*) FROM server_voice_terminal_outbox WHERE channel_id = $1`, channelID).Scan(&pending))
		assert.Zero(t, pending,
			"account erasure must cascade a pending terminal obligation after the migration is applied")
	})
}
