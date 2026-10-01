package dmvisibility_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmvisibility"
	testhelpers "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
)

// TestRespawn_ClearsOnlyParticipantsHiddenBeforeTheRow: a row respawns the
// thread for everyone who hid it earlier, and names exactly them, because
// callers publish the visible state to that list and nobody else.
func TestRespawn_ClearsOnlyParticipantsHiddenBeforeTheRow(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	hidBefore := testhelpers.CreateUser(t, db)
	hidAfter := testhelpers.CreateUser(t, db)
	visible := testhelpers.CreateUser(t, db)
	conversation := uuid.New()
	_, err := db.Exec(`INSERT INTO dm_conversations (id, created_by) VALUES ($1, $2)`, conversation, hidBefore)
	require.NoError(t, err)

	rowAt := time.Now().UTC().Truncate(time.Microsecond)
	for user, hiddenAt := range map[uuid.UUID]sql.NullTime{
		hidBefore: {Time: rowAt.Add(-time.Minute), Valid: true},
		hidAfter:  {Time: rowAt.Add(time.Minute), Valid: true},
		visible:   {},
	} {
		_, err = db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, hidden_at) VALUES ($1, $2, $3)`,
			conversation, user, hiddenAt)
		require.NoError(t, err)
	}

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	respawned, err := dmvisibility.Respawn(t.Context(), tx, conversation, rowAt)
	require.NoError(t, err)
	require.NoError(t, tx.Commit())

	assert.Equal(t, []uuid.UUID{hidBefore}, respawned)
	hidden := func(user uuid.UUID) bool {
		var at sql.NullTime
		require.NoError(t, db.QueryRow(
			`SELECT hidden_at FROM dm_participants WHERE conversation_id = $1 AND user_id = $2`,
			conversation, user).Scan(&at))
		return at.Valid
	}
	assert.False(t, hidden(hidBefore), "an earlier hide is respawned")
	assert.True(t, hidden(hidAfter), "a hide after the row stays hidden")
	assert.False(t, hidden(visible))
}

func TestRespawn_ReportsAQueryFailure(t *testing.T) {
	db, cleanup := testhelpers.SetupTestDB(t)
	defer cleanup()

	tx, err := db.BeginTx(t.Context(), nil)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()

	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	respawned, err := dmvisibility.Respawn(ctx, tx, uuid.New(), time.Now())
	require.Error(t, err)
	assert.ErrorContains(t, err, "respawn DM participant visibility")
	assert.Nil(t, respawned)
}
