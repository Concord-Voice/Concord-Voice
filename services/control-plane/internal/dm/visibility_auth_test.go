package dm

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRequireClearAuth_MissingSettingFailsClosed(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	var userID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO users (id, username, email, password_hash)
		VALUES (gen_random_uuid(), 'clear_auth_missing', 'clear_auth_missing@example.test', '')
		RETURNING id`).Scan(&userID))

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	required, err := requireClearAuth(context.Background(), tx, userID)
	require.NoError(t, err)
	assert.True(t, required)
	require.NoError(t, tx.Rollback())
}

func TestRequireClearAuth_ExplicitOptOutSkipsStepUp(t *testing.T) {
	db, _ := testdb.SetupTestDB(t)
	var userID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO users (id, username, email, password_hash)
		VALUES (gen_random_uuid(), 'clear_auth_disabled', 'clear_auth_disabled@example.test', '')
		RETURNING id`).Scan(&userID))
	_, err := db.Exec(`INSERT INTO privacy_settings (user_id, require_auth_before_purge) VALUES ($1, false)`, userID)
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	required, err := requireClearAuth(context.Background(), tx, userID)
	require.NoError(t, err)
	assert.False(t, required)
	require.NoError(t, tx.Rollback())
}
