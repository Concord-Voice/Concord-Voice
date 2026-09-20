package database_test

import (
	"strings"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000141_AddsServerVoiceTerminalOutboxUserIndex(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000141_add_server_voice_terminal_outbox_user_index.up.sql")
	down := migrationReadFile(t, "../../migrations/000141_add_server_voice_terminal_outbox_user_index.down.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	_, err = tx.Exec(down)
	require.NoError(t, err)
	_, err = tx.Exec(up)
	require.NoError(t, err)

	var indexDef string
	require.NoError(t, tx.QueryRow(`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = 'idx_server_voice_terminal_outbox_user'`).Scan(&indexDef))
	assert.Contains(t, indexDef, "ON public.server_voice_terminal_outbox USING btree (user_id)")

	_, err = tx.Exec(down)
	require.NoError(t, err)
	var exists bool
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.idx_server_voice_terminal_outbox_user') IS NOT NULL`).Scan(&exists))
	assert.False(t, exists)
}

func TestMigration000141_DownKeepsIndexForPendingOutbox(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()
	userID := testhelpers.CreateUser(t, db)
	down := migrationReadFile(t, "../../migrations/000141_add_server_voice_terminal_outbox_user_index.down.sql")
	lock := strings.Index(down, "LOCK TABLE public.server_voice_terminal_outbox IN ACCESS EXCLUSIVE MODE")
	check := strings.Index(down, "SELECT 1 FROM public.server_voice_terminal_outbox")
	require.NotEqual(t, -1, lock, "down migration must lock the outbox before removing its cascade index")
	require.NotEqual(t, -1, check, "down migration must inspect pending obligations")
	assert.Less(t, lock, check, "outbox lock must precede the pending-obligation check")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	_, err = tx.Exec(`INSERT INTO server_voice_terminal_outbox (channel_id, user_id, server_id, operation_id) VALUES (gen_random_uuid(), $1, gen_random_uuid(), gen_random_uuid())`, userID)
	require.NoError(t, err)
	_, err = tx.Exec(down)
	require.NoError(t, err)

	var exists bool
	require.NoError(t, tx.QueryRow(`SELECT to_regclass('public.idx_server_voice_terminal_outbox_user') IS NOT NULL`).Scan(&exists))
	assert.True(t, exists, "a guarded 000140 rollback must retain the cascade index")
}
