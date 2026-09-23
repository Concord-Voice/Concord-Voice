package database_test

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
)

func TestMigration000154_VoiceEnforcementSessionsContract(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000154_add_voice_enforcement_sessions.up.sql")
	down := migrationReadFile(t, "../../migrations/000154_add_voice_enforcement_sessions.down.sql")
	ctx := t.Context()

	var tableExists bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT to_regclass('public.voice_enforcement_sessions') IS NOT NULL`).Scan(&tableExists))
	require.True(t, tableExists)

	var rolloutColumns int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'voice_enforcement_rollout'
		  AND column_name IN ('id', 'activated_at', 'updated_at')`).Scan(&rolloutColumns))
	require.Equal(t, 3, rolloutColumns)

	var rolloutActive bool
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT activated_at IS NOT NULL FROM voice_enforcement_rollout
		WHERE id = TRUE`).Scan(&rolloutActive))
	require.False(t, rolloutActive, "new rollout must remain inactive until deployment activation")

	var columns int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM information_schema.columns
		WHERE table_schema = 'public' AND table_name = 'voice_enforcement_sessions'
		  AND column_name IN ('session_generation', 'node_boot_id', 'room_id', 'room_kind',
			'user_id', 'credential_epoch', 'socket_id', 'created_at')`).Scan(&columns))
	require.Equal(t, 8, columns)

	var foreignKeys int
	require.NoError(t, db.QueryRowContext(ctx, `
		SELECT count(*) FROM pg_constraint c
		JOIN pg_class r ON r.oid = c.conrelid
		WHERE r.relname = 'voice_enforcement_sessions' AND c.contype = 'f'`).Scan(&foreignKeys))
	require.Zero(t, foreignKeys, "session evidence must survive parent deletion")

	for _, indexName := range []string{
		"idx_voice_enforcement_sessions_room_user_generation",
		"idx_voice_enforcement_sessions_user_epoch_generation",
	} {
		var exists bool
		require.NoError(t, db.QueryRowContext(ctx, `
			SELECT EXISTS (SELECT 1 FROM pg_indexes
			WHERE schemaname = 'public' AND indexname = $1)`, indexName).Scan(&exists))
		require.True(t, exists, indexName)
	}

	userID, nodeBootID, roomID, sessionGeneration := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	epoch := "0123456789abcdef0123456789abcdef" // pragma: allowlist secret
	_, err := db.ExecContext(ctx, `INSERT INTO voice_enforcement_sessions
		(session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id)
		VALUES ($1, $2, $3, 'dm', $4, $5, 'socket-1')`,
		sessionGeneration, nodeBootID, roomID, userID, epoch)
	require.NoError(t, err)

	_, err = db.ExecContext(ctx, `UPDATE voice_enforcement_sessions
		SET socket_id = 'socket-2' WHERE session_generation = $1`, sessionGeneration)
	require.Error(t, err, "exact session evidence must be immutable")

	for _, query := range []string{
		`INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id) VALUES ($1, $2, $3, 'other', $4, $5, 'socket-2')`,
		`INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id) VALUES ($1, $2, $3, 'dm', $4, 'not-an-epoch', 'socket-2')`,
		`INSERT INTO voice_enforcement_sessions (session_generation, node_boot_id, room_id, room_kind, user_id, credential_epoch, socket_id) VALUES ($1, $2, $3, 'dm', $4, $5, '')`,
	} {
		_, err = db.ExecContext(ctx, query, uuid.New(), uuid.New(), roomID, userID, epoch)
		require.Error(t, err, "constraint must reject malformed session evidence")
	}

	// A down migration must refuse while evidence remains.
	tx, err := db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.Error(t, err)
	require.NoError(t, tx.Rollback())

	// Once evidence drains, down/up must work and restore the trigger contract.
	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM voice_enforcement_sessions`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE voice_enforcement_rollout SET activated_at = NOW() WHERE id = TRUE`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.Error(t, err, "rollback must refuse after rollout activation")
	require.NoError(t, tx.Rollback())

	tx, err = db.BeginTx(ctx, nil)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `DELETE FROM voice_enforcement_sessions`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, `UPDATE voice_enforcement_rollout SET activated_at = NULL WHERE id = TRUE`)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, down)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, up)
	require.NoError(t, err)
	require.NoError(t, tx.Rollback())
}
