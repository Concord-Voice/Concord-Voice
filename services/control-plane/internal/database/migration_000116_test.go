package database_test

import (
	"database/sql"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Migration 000116 adds only the retry-fairness counter. Its default keeps rows
// created by older code immediately eligible, while the down migration removes
// the column without leaving a constraint or index behind.
func TestMigration000116_ReapAttemptsShapeAndRollback(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	var dataType, isNullable string
	var columnDefault sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT data_type, is_nullable, column_default
		FROM information_schema.columns
		WHERE table_name = 'media_files' AND column_name = 'reap_attempts'`,
	).Scan(&dataType, &isNullable, &columnDefault))
	assert.Equal(t, "integer", dataType)
	assert.Equal(t, "NO", isNullable)
	assert.True(t, columnDefault.Valid)
	assert.Contains(t, columnDefault.String, "0")

	var comment sql.NullString
	require.NoError(t, db.QueryRow(`
		SELECT col_description('media_files'::regclass, ordinal_position)
		FROM information_schema.columns
		WHERE table_name = 'media_files' AND column_name = 'reap_attempts'`,
	).Scan(&comment))
	assert.True(t, comment.Valid)
	assert.Contains(t, comment.String, "failed straggler-sweep attempts")

	downSQL := migrationReadFile(t, "../../migrations/000116_media_files_reap_attempts.down.sql")
	upSQL := migrationReadFile(t, "../../migrations/000116_media_files_reap_attempts.up.sql")
	tx, err := db.Begin()
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(downSQL)
	require.NoError(t, err)
	var exists bool
	require.NoError(t, tx.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'media_files' AND column_name = 'reap_attempts'
		)`).Scan(&exists))
	assert.False(t, exists)

	_, err = tx.Exec(upSQL)
	require.NoError(t, err)
	require.NoError(t, tx.QueryRow(`
		SELECT EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'media_files' AND column_name = 'reap_attempts'
		)`).Scan(&exists))
	assert.True(t, exists)
}
