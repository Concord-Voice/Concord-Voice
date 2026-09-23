package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestMigration000143_AddsServerVoiceTerminalDeliveryClaim(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	up := migrationReadFile(t, "../../migrations/000143_add_server_voice_terminal_delivery_claim.up.sql")
	down := migrationReadFile(t, "../../migrations/000143_add_server_voice_terminal_delivery_claim.down.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { require.NoError(t, tx.Rollback()) }()

	_, err = tx.Exec(down)
	require.NoError(t, err)
	assertMigration000143Column(t, tx, "delivery_claim_id", false)
	assertMigration000143Column(t, tx, "delivery_claim_until", false)

	_, err = tx.Exec(up)
	require.NoError(t, err)
	assertMigration000143Column(t, tx, "delivery_claim_id", true)
	assertMigration000143Column(t, tx, "delivery_claim_until", true)
}

func assertMigration000143Column(t *testing.T, tx *sql.Tx, column string, wantExists bool) {
	t.Helper()
	var dataType, nullable string
	var comment sql.NullString
	err := tx.QueryRow(`
		SELECT data_type, is_nullable,
		       col_description('server_voice_terminal_outbox'::regclass, ordinal_position)
		FROM information_schema.columns
		WHERE table_schema = 'public'
		  AND table_name = 'server_voice_terminal_outbox'
		  AND column_name = $1`, column).Scan(&dataType, &nullable, &comment)
	if !wantExists {
		assert.ErrorIs(t, err, sql.ErrNoRows)
		return
	}
	require.NoError(t, err)
	assert.Equal(t, "YES", nullable)
	assert.True(t, comment.Valid)
	if column == "delivery_claim_id" {
		assert.Equal(t, "uuid", dataType)
		assert.Contains(t, comment.String, "claiming")
		return
	}
	assert.Equal(t, "timestamp with time zone", dataType)
	assert.Contains(t, comment.String, "Expiration")
}
