//go:build integration

package members

import (
	"context"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestModerationFenceRequiresAuthorityCoordinator(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	owner := dbtest.CreateUser(t, db)
	target := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'coverage moderation server', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, target)
	require.NoError(t, err)
	h := &Handler{db: db, log: logger.New("coverage-moderation")}

	t.Run("remove", func(t *testing.T) {
		err := h.execRemovalTx(context.Background(), serverID, target.String(), owner.String())
		require.Error(t, err)
		require.Contains(t, err.Error(), "authority coordinator unavailable")
	})
	t.Run("ban", func(t *testing.T) {
		err := h.execBanTx(context.Background(), serverID, target.String(), owner.String(), nil, true)
		require.Error(t, err)
		require.Contains(t, err.Error(), "authority coordinator unavailable")
	})
}
