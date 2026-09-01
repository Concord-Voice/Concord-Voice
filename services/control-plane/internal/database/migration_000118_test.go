package database_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000118_PreservesExistingObligationsAndAddsTimestamp(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000118_tier1_erasure_key_tombstones.up.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	// Recreate the exact 000117 state, then prove the 000118 upgrade keeps its
	// durable row while adding the nullable metadata column.
	_, err = tx.Exec(`ALTER TABLE tier1_erasure_delete_obligations DROP COLUMN last_delete_at`)
	require.NoError(t, err)
	const key = "avatars/migration-000118-preserved"
	_, err = tx.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ($1)`, key)
	require.NoError(t, err)
	_, err = tx.Exec(up)
	require.NoError(t, err)

	var gotKey string
	var lastDeleteAt any
	require.NoError(t, tx.QueryRow(`
		SELECT storage_key, last_delete_at
		FROM tier1_erasure_delete_obligations WHERE storage_key = $1`, key).
		Scan(&gotKey, &lastDeleteAt))
	assert.Equal(t, key, gotKey)
	assert.Nil(t, lastDeleteAt, "000118 must not invent a delete timestamp for 000117 rows")

	var dataType string
	require.NoError(t, tx.QueryRow(`
		SELECT data_type FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'tier1_erasure_delete_obligations'
		  AND column_name = 'last_delete_at'`).Scan(&dataType))
	assert.Equal(t, "timestamp with time zone", dataType)
}

func TestMigration000118_DownGuardRequiresEmptyTombstones(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	down := migrationReadFile(t, "../../migrations/000118_tier1_erasure_key_tombstones.down.sql")
	up := migrationReadFile(t, "../../migrations/000118_tier1_erasure_key_tombstones.up.sql")

	t.Run("refuses while a tombstone survives", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(`INSERT INTO tier1_erasure_delete_obligations (storage_key) VALUES ('avatars/migration-000118-guard')`)
		require.NoError(t, err)
		_, err = tx.Exec(down)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "cannot remove Tier-1 erasure tombstone metadata")
	})

	t.Run("removes metadata when empty and can be reapplied", func(t *testing.T) {
		tx, err := db.Begin()
		require.NoError(t, err)
		defer func() { _ = tx.Rollback() }()
		_, err = tx.Exec(`DELETE FROM tier1_erasure_delete_obligations`)
		require.NoError(t, err)
		_, err = tx.Exec(down)
		require.NoError(t, err)

		var exists bool
		require.NoError(t, tx.QueryRow(`
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = 'public'
				  AND table_name = 'tier1_erasure_delete_obligations'
				  AND column_name = 'last_delete_at')`).Scan(&exists))
		assert.False(t, exists)
		_, err = tx.Exec(up)
		require.NoError(t, err, "the guarded down must leave a state that 000118 up can restore")
	})
}
