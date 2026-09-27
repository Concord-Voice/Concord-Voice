//go:build integration

package ownership

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/websocket"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/pkg/logger"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestOwnershipStateGuardsPreserveState(t *testing.T) {
	db, cleanup := dbtest.SetupTestDB(t)
	t.Cleanup(cleanup)

	t.Run("removed target membership", func(t *testing.T) {
		serverID, from, to := seedOwnershipState(t, db)
		transferID := uuid.New()
		_, err := db.Exec(`INSERT INTO ownership_transfers
			(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
			VALUES ($1, $2, $3, $4, 'pending', $5, NOW(), NOW() + INTERVAL '1 hour')`,
			transferID, serverID, from, to, uuid.NewString())
		require.NoError(t, err)
		_, err = db.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, to)
		require.NoError(t, err)

		h := &Handler{db: db, log: logger.New("ownership-coverage"), authority: noOpOwnershipAuthority{}}
		err = h.executeTransfer(context.Background(), serverID.String(), transferID.String(), from.String(), to.String())
		require.ErrorIs(t, err, errToUserNotMember)
		assertOwnershipState(t, db, serverID, from, "pending", transferID)
	})

	t.Run("reversal without coordinator", func(t *testing.T) {
		serverID, from, to := seedOwnershipState(t, db)
		transferID := uuid.New()
		_, err := db.Exec(`INSERT INTO ownership_transfers
			(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at, completed_at)
			VALUES ($1, $2, $3, $4, 'completed', $5, NOW() - INTERVAL '2 days', NOW() - INTERVAL '1 day', NOW() - INTERVAL '1 day')`,
			transferID, serverID, from, to, uuid.NewString())
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE servers SET owner_id = $1 WHERE id = $2`, to, serverID)
		require.NoError(t, err)
		_, err = db.Exec(`UPDATE server_members SET role = CASE WHEN user_id = $2 THEN 'member' ELSE 'owner' END WHERE server_id = $1`, serverID, from)
		require.NoError(t, err)

		h := &Handler{db: db, log: logger.New("ownership-coverage")}
		_, err = h.executeReversal(context.Background(), &reversalRecord{
			transferID: transferID.String(), serverID: serverID.String(), fromUserID: from.String(), toUserID: to.String(),
			completedAt: time.Now().Add(-time.Hour),
		})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "authority coordinator unavailable")
		assertOwnershipState(t, db, serverID, to, "completed", transferID)
	})

	t.Run("expired transfer over channel limit", func(t *testing.T) {
		serverID, from, to := seedOwnershipState(t, db)
		transferID := uuid.New()
		_, err := db.Exec(`INSERT INTO ownership_transfers
			(id, server_id, from_user_id, to_user_id, status, reversal_token, requested_at, expires_at)
			VALUES ($1, $2, $3, $4, 'pending', $5, NOW() - INTERVAL '2 days', NOW() - INTERVAL '1 day')`,
			transferID, serverID, from, to, uuid.NewString())
		require.NoError(t, err)
		for i := 0; i < 501; i++ {
			_, err = db.Exec(`INSERT INTO channels (id, server_id, name, type) VALUES ($1, $2, $3, 'text')`, uuid.New(), serverID, uuid.NewString())
			require.NoError(t, err)
		}

		log := logger.New("ownership-coverage")
		resolver := rbac.NewResolver(db, nil, log)
		authority := rbac.NewHandler(db, log, nil, websocket.NewHub(db, nil), resolver, nil, nil)
		h := &Handler{db: db, hub: websocket.NewHub(db, nil), log: log, authority: authority}
		changed, err := h.completeExpiredTransfer(context.Background(), expiredTransfer{
			id: transferID.String(), serverID: serverID.String(), fromUserID: from.String(), toUserID: to.String(),
		})
		require.ErrorIs(t, err, rbac.ErrChannelAuthorityChannelLimit)
		assert.False(t, changed)
		assertOwnershipState(t, db, serverID, from, "pending", transferID)
	})
}

func seedOwnershipState(t *testing.T, db *sql.DB) (serverID, from, to uuid.UUID) {
	t.Helper()
	from = dbtest.CreateUser(t, db)
	to = dbtest.CreateUser(t, db)
	serverID = uuid.New()
	_, err := db.Exec(`INSERT INTO servers (id, name, owner_id) VALUES ($1, 'ownership state coverage', $2)`, serverID, from)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO server_members (server_id, user_id, role) VALUES ($1, $2, 'owner'), ($1, $3, 'member')`, serverID, from, to)
	require.NoError(t, err)
	return
}

func assertOwnershipState(t *testing.T, db *sql.DB, serverID, owner uuid.UUID, status string, transferID uuid.UUID) {
	t.Helper()
	var gotOwner, gotStatus string
	require.NoError(t, db.QueryRow(`SELECT owner_id FROM servers WHERE id = $1`, serverID).Scan(&gotOwner))
	require.NoError(t, db.QueryRow(`SELECT status FROM ownership_transfers WHERE id = $1`, transferID).Scan(&gotStatus))
	assert.Equal(t, owner.String(), gotOwner)
	assert.Equal(t, status, gotStatus)
}
