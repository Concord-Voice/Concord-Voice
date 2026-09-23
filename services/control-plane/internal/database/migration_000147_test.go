package database_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000147_DurableVoiceEjectionConstraintsAndRollbackGuard(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	conversationID := uuid.New()
	userID := uuid.New()
	_, err := db.Exec(`
		INSERT INTO dm_block_voice_ejections (conversation_id, user_id)
		VALUES ($1, $2)`, conversationID, userID)
	require.NoError(t, err, "deleted conversation/user rows must not erase delivery evidence")
	_, err = db.Exec(`UPDATE dm_block_voice_ejections SET updated_at = created_at - interval '1 second' WHERE conversation_id = $1 AND user_id = $2`, conversationID, userID)
	require.Error(t, err, "updated_at cannot precede created_at")
	_, err = db.Exec(`
		INSERT INTO dm_block_voice_ejections (conversation_id, user_id, attempts)
		VALUES ($1, $2, -1)`, uuid.New(), uuid.New())
	require.Error(t, err)
	_, err = db.Exec(`
		INSERT INTO dm_block_voice_ejections (conversation_id, user_id, failure_class)
		VALUES ($1, $2, 'database')`, uuid.New(), uuid.New())
	require.Error(t, err)

	down := migrationReadFile(t, "../../migrations/000147_add_dm_block_voice_ejections.down.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	_, err = tx.Exec(down)
	require.Error(t, err, "rollback must retain undelivered media evidence")
	require.NoError(t, tx.Rollback())

	tx, err = db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`DELETE FROM dm_block_voice_ejections`)
	require.NoError(t, err)
	_, err = tx.Exec(down)
	require.NoError(t, err)
	var exists bool
	require.NoError(t, tx.QueryRow(`
		SELECT to_regclass('public.dm_block_voice_ejections') IS NOT NULL`).Scan(&exists))
	require.False(t, exists)
	_, err = tx.Exec(migrationReadFile(t, "../../migrations/000147_add_dm_block_voice_ejections.up.sql"))
	require.NoError(t, err, "migration must replay after clean rollback")
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.dm_block_voice_ejections') IS NOT NULL`).Scan(&exists))
	require.True(t, exists)
}
