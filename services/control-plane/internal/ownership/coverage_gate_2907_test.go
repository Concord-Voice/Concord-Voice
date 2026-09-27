//go:build integration

package ownership

import (
	"context"
	"testing"

	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestOwnershipWritersRequireAuthorityCoordinator(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	defer cleanup()
	owner := dbtest.CreateUser(t, db)
	target := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	transferID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'coverage ownership server', $2)`, serverID, owner)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, owner, target)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO ownership_transfers (id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at) VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`, transferID, serverID, owner, target, uuid.NewString())
	require.NoError(t, err)
	h := &Handler{db: db, log: logger.New("coverage-ownership"), hub: nil}

	err = h.executeTransfer(context.Background(), serverID, transferID, owner.String(), target.String())
	require.Error(t, err)
	require.Contains(t, err.Error(), "authority coordinator unavailable")

	var status string
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&status))
	require.Equal(t, "pending", status)
}
