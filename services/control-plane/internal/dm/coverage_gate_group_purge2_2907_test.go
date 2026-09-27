package dm

import (
	"context"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The group-delete transaction must reject both authority drift and a newly
// joined voice participant after its candidate gates were acquired.
func Test2907DMGroupDeleteGuardsRejectDrift(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 3)
	ctx := context.Background()

	var groupID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (true, $1)
		RETURNING id`, users[0]).Scan(&groupID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role)
		VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, groupID, users[0], users[1])
	require.NoError(t, err)

	t.Run("non-group authority drift", func(t *testing.T) {
		var dmID string
		require.NoError(t, db.QueryRow(`
			INSERT INTO dm_conversations (is_group, created_by) VALUES (false, $1)
			RETURNING id`, users[0]).Scan(&dmID))
		_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role)
			VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, dmID, users[0], users[1])
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		assert.ErrorIs(t, revalidateDeleteGroupAuthority(ctx, tx, dmID, users[0].String()), errMemberRemovalStateDrifted)
		assert.NoError(t, tx.Rollback())
	})

	t.Run("new voice participant drift", func(t *testing.T) {
		_, err := db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, groupID, users[1])
		require.NoError(t, err)
		tx, err := db.BeginTx(ctx, nil)
		require.NoError(t, err)
		assert.ErrorIs(t, revalidateVoiceCandidates(ctx, tx, groupID, nil), errCandidateSetDrifted)
		assert.NoError(t, tx.Rollback())
	})
}

func Test2907DMGroupDeleteCandidateSetAllowsKnownParticipants(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 2)
	var groupID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (true, $1)
		RETURNING id`, users[0]).Scan(&groupID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role)
		VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, groupID, users[0], users[1])
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO dm_voice_participants (conversation_id, user_id) VALUES ($1, $2)`, groupID, users[1])
	require.NoError(t, err)

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	assert.NoError(t, revalidateVoiceCandidates(context.Background(), tx, groupID, []uuid.UUID{users[1]}))
	assert.NoError(t, tx.Rollback())
}
