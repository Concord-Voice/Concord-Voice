package dm

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/dmblock"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func Test2907AddMemberTxRejectsMalformedTopologySnapshot(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	h := NewHandler(HandlerDeps{DB: db})
	_, err := h.addMemberTx(context.Background(), uuid.NewString(), uuid.NewString(), uuid.NewString(), "", []string{"not-a-uuid"})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse add-member topology user")
}

func Test2907DMVoiceEnforcementAuthorityFailsClosed(t *testing.T) {
	db, _ := dbtest.SetupTestDB(t)
	users := seedTopologyUsers(t, db, 3)
	var groupID string
	require.NoError(t, db.QueryRow(`
		INSERT INTO dm_conversations (is_group, created_by) VALUES (true, $1) RETURNING id`, users[0]).Scan(&groupID))
	_, err := db.Exec(`INSERT INTO dm_participants (conversation_id, user_id, role) VALUES ($1, $2, 'admin'), ($1, $3, 'member')`, groupID, users[0], users[1])
	require.NoError(t, err)

	for name, actor := range map[string]uuid.UUID{
		"non-admin member": users[1],
		"outsider":         users[2],
	} {
		t.Run(name, func(t *testing.T) {
			tx, err := db.BeginTx(context.Background(), nil)
			require.NoError(t, err)
			err = revalidateDMVoiceEnforcementAuthority(context.Background(), tx, groupID, actor.String())
			require.NoError(t, tx.Rollback())
			assert.ErrorIs(t, err, dmblock.ErrUnavailable)
		})
	}

	tx, err := db.BeginTx(context.Background(), nil)
	require.NoError(t, err)
	assert.NoError(t, revalidateDMVoiceEnforcementAuthority(context.Background(), tx, groupID, users[0].String()))
	assert.ErrorIs(t, revalidateDMVoiceEnforcementAuthority(context.Background(), tx, uuid.NewString(), users[0].String()), dmblock.ErrUnavailable)
	require.NoError(t, tx.Rollback())
}
