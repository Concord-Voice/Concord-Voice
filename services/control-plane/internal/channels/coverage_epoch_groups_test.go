package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These requests are admitted by the stale permission-cache snapshot, then
// must be rejected by the in-transaction credential fence before any group or
// reorder mutation commits.
func TestChannelGroupMutationsRejectStaleCredentialEpoch(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		stale := ts.SimulateStaleEpochWindow(t, owner.ID)

		w := ts.DoRequest(http.MethodPost, groupsPath(serverID), map[string]interface{}{"name": "fenced-group"}, testhelpers.AuthHeaders(stale))
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var count int
		require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_groups WHERE server_id = $1 AND name = 'fenced-group'`, serverID).Scan(&count))
		assert.Zero(t, count)
	})

	t.Run("update", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		groupID := createGroup(t, ts, serverID, "fenced-update", owner.AccessToken)
		stale := ts.SimulateStaleEpochWindow(t, owner.ID)

		w := ts.DoRequest(http.MethodPatch, groupPath(serverID, groupID), map[string]interface{}{"name": "must-not-commit"}, testhelpers.AuthHeaders(stale))
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var name string
		require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channel_groups WHERE id = $1`, groupID).Scan(&name))
		assert.Equal(t, "fenced-update", name)
	})

	t.Run("reorder", func(t *testing.T) {
		ts, owner, serverID := setupWithServer(t)
		channelID := ts.CreateTestChannel(t, serverID, "fenced-reorder")
		stale := ts.SimulateStaleEpochWindow(t, owner.ID)

		w := ts.DoRequest(http.MethodPut, reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{{"channel_id": channelID, "position": 99}},
		}, testhelpers.AuthHeaders(stale))
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var position int
		require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, channelID).Scan(&position))
		assert.NotEqual(t, 99, position)
	})
}
