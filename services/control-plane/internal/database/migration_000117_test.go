package database_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

// TestMigration000117_DurableObligationSchema pins the key-only erasure rail
// against the live PostgreSQL catalog. Migration 000118 adds the nullable
// last_delete_at metadata column without changing the 000117 base shape.
func TestMigration000117_DurableObligationSchema(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	// Positive control: prove SetupTestDB reached an existing migrated relation
	// before asserting the new relation's shape.
	var usersExists bool
	require.NoError(t, db.QueryRow(`
		SELECT to_regclass('public.users') IS NOT NULL
	`).Scan(&usersExists))
	require.True(t, usersExists, "positive control: migration harness must reach users")

	var obligationsExists bool
	require.NoError(t, db.QueryRow(`
		SELECT to_regclass('public.tier1_erasure_delete_obligations') IS NOT NULL
	`).Scan(&obligationsExists))
	require.True(t, obligationsExists,
		"tier1_erasure_delete_obligations relation must exist after migrations")

	columns := map[string]struct {
		dataType   string
		maxLength  *int
		nullable   string
		defaultSQL string
	}{}
	rows, err := db.Query(`
		SELECT column_name, data_type, character_maximum_length, is_nullable,
		       COALESCE(column_default, '')
		FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'tier1_erasure_delete_obligations'`)
	require.NoError(t, err)
	for rows.Next() {
		var name, dataType, nullable, defaultSQL string
		var maxLength *int
		require.NoError(t, rows.Scan(&name, &dataType, &maxLength, &nullable, &defaultSQL))
		columns[name] = struct {
			dataType   string
			maxLength  *int
			nullable   string
			defaultSQL string
		}{dataType, maxLength, nullable, defaultSQL}
	}
	require.NoError(t, rows.Err())
	require.NoError(t, rows.Close())

	assert.Len(t, columns, 5, "obligation table must contain the four 000117 columns plus 000118 metadata")
	key, ok := columns["storage_key"]
	require.True(t, ok, "storage_key column must exist")
	assert.Equal(t, "character varying", key.dataType)
	require.NotNil(t, key.maxLength, "storage_key must declare its VARCHAR length")
	assert.Equal(t, 500, *key.maxLength)
	assert.Equal(t, "NO", key.nullable)
	attempts, ok := columns["attempts"]
	require.True(t, ok, "attempts column must exist")
	assert.Equal(t, "integer", attempts.dataType)
	assert.Equal(t, "NO", attempts.nullable)
	assert.Contains(t, attempts.defaultSQL, "0")
	reconcileAfter, ok := columns["reconcile_after"]
	require.True(t, ok, "reconcile_after column must exist")
	assert.Equal(t, "timestamp with time zone", reconcileAfter.dataType)
	assert.Equal(t, "NO", reconcileAfter.nullable)
	assert.Contains(t, reconcileAfter.defaultSQL, "clock_timestamp()")
	createdAt, ok := columns["created_at"]
	require.True(t, ok, "created_at column must exist")
	assert.Equal(t, "timestamp with time zone", createdAt.dataType)
	assert.Equal(t, "NO", createdAt.nullable)
	assert.Contains(t, createdAt.defaultSQL, "clock_timestamp()")
	lastDeleteAt, ok := columns["last_delete_at"]
	require.True(t, ok, "000118 last_delete_at column must exist")
	assert.Equal(t, "timestamp with time zone", lastDeleteAt.dataType)
	assert.Equal(t, "YES", lastDeleteAt.nullable)

	var indexDef string
	require.NoError(t, db.QueryRow(`
		SELECT indexdef FROM pg_indexes
		WHERE schemaname = 'public'
		  AND tablename = 'tier1_erasure_delete_obligations'
		  AND indexname = 'idx_tier1_erasure_delete_obligations_due'
	`).Scan(&indexDef), "due index must exist on reconcile_after")
	assert.Contains(t, indexDef, "reconcile_after",
		"due index must support reconcile_after discovery")

	var primaryKeyDefinition string
	require.NoError(t, db.QueryRow(`
		SELECT pg_get_constraintdef(c.oid)
		FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = r.relnamespace
		WHERE n.nspname = 'public'
		  AND r.relname = 'tier1_erasure_delete_obligations'
		  AND c.contype = 'p'
	`).Scan(&primaryKeyDefinition))
	assert.Equal(t, "PRIMARY KEY (storage_key)", primaryKeyDefinition,
		"storage_key must be protected by the table primary key")

	var fkCount int
	require.NoError(t, db.QueryRow(`
		SELECT COUNT(*)
		FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		JOIN pg_namespace n ON n.oid = r.relnamespace
		WHERE n.nspname = 'public'
		  AND r.relname = 'tier1_erasure_delete_obligations'
		  AND c.contype = 'f'
	`).Scan(&fkCount))
	assert.Zero(t, fkCount, "key-only obligation table must not have a user foreign key")

	_, err = db.Exec(`
		INSERT INTO tier1_erasure_delete_obligations (storage_key, attempts)
		VALUES ('avatars/erasure-negative-attempts', -1)`)
	require.Error(t, err, "attempts >= 0 must reject a negative retry count")

	userID := testhelpers.CreateUser(t, db)
	const storageKey = "avatars/erasure-red-repro"
	_, err = db.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, storageKey)
	require.NoError(t, err, "obligation row must be insertable without a user foreign key")
	_, err = db.Exec(`DELETE FROM users WHERE id = $1`, userID)
	require.NoError(t, err, "deleting the user must not discard the key-only obligation")

	var retainedKey string
	require.NoError(t, db.QueryRow(`
		SELECT storage_key FROM tier1_erasure_delete_obligations WHERE storage_key = $1
	`, storageKey).Scan(&retainedKey))
	assert.Equal(t, storageKey, retainedKey,
		"obligation must survive deletion of the user it serves")
}

// TestMigration000117_DownGuard refuses to discard unresolved evidence and
// drops the table once its obligations have drained.
func TestMigration000117_DownGuard(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down := migrationReadFile(t, "../../migrations/000117_tier1_erasure_delete_obligations.down.sql")

	t.Run("refuses while an obligation survives", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(`
			INSERT INTO tier1_erasure_delete_obligations (storage_key)
			VALUES ('avatars/erasure-down-guard')`)
		require.NoError(t, err)

		_, err = tx.Exec(down)
		require.Error(t, err)
		assert.Contains(t, err.Error(),
			"cannot drop tier1_erasure_delete_obligations while unresolved Tier-1 erasure obligations remain")
	})

	t.Run("drops cleanly when empty", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()

		_, err = tx.Exec(down)
		require.NoError(t, err)

		var exists bool
		require.NoError(t, tx.QueryRow(
			`SELECT to_regclass('public.tier1_erasure_delete_obligations') IS NOT NULL`,
		).Scan(&exists))
		assert.False(t, exists, "empty down migration must drop the obligation table")
	})
}
