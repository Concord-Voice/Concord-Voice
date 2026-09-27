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

func TestPreemptiveBanRecordsDurableBanWithoutMembershipFence(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)
	owner := dbtest.CreateUser(t, db)
	target := dbtest.CreateUser(t, db)
	serverID := uuid.NewString()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'preemptive-ban-server', $2)`, serverID, owner)
	require.NoError(t, err)
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM servers WHERE id = $1`, serverID) })

	h := &Handler{db: db, log: logger.New("coverage-preemptive-ban")}
	require.NoError(t, h.execBanTx(context.Background(), serverID, target.String(), owner.String(), nil, false))

	var count int
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM server_bans WHERE server_id = $1 AND user_id = $2`, serverID, target).Scan(&count))
	require.Equal(t, 1, count)
	require.NoError(t, db.QueryRow(`SELECT COUNT(*) FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, target).Scan(&count))
	require.Zero(t, count)
}
