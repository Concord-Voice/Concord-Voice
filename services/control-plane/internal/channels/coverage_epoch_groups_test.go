package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These requests complete real family auth before epoch rotation, then
// must be rejected by the in-transaction credential fence before any group or
// reorder mutation commits.
func TestChannelGroupMutationsRejectStaleCredentialEpoch(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		boundaryTS, boundary := testhelpers.SetupStaleBoundaryServer(t)
		ts, owner, serverID := setupWithServer(t, boundaryTS)
		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{channelEpochGuardSQL}, owner.ID, nil, nil)

		w := ts.DoRequest(http.MethodPost, groupsPath(serverID), map[string]interface{}{"name": "fenced-group"}, testhelpers.AuthHeaders(owner.AccessToken))
		boundary.RequireRotated(t)
		seen, hookErr := boundary.Hook.Report()
		require.NoError(t, hookErr)
		require.Equal(t, 1, seen, "handler GuardTx must be reached after admitted epoch rotation")
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var count int
		require.NoError(t, ts.DB.QueryRow(`SELECT COUNT(*) FROM channel_groups WHERE server_id = $1 AND name = 'fenced-group'`, serverID).Scan(&count))
		assert.Zero(t, count)
	})

	t.Run("update", func(t *testing.T) {
		boundaryTS, boundary := testhelpers.SetupStaleBoundaryServer(t)
		ts, owner, serverID := setupWithServer(t, boundaryTS)
		groupID := createGroup(t, ts, serverID, "fenced-update", owner.AccessToken)
		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{channelEpochGuardSQL}, owner.ID, nil, nil)

		w := ts.DoRequest(http.MethodPatch, groupPath(serverID, groupID), map[string]interface{}{"name": "must-not-commit"}, testhelpers.AuthHeaders(owner.AccessToken))
		boundary.RequireRotated(t)
		seen, hookErr := boundary.Hook.Report()
		require.NoError(t, hookErr)
		require.Equal(t, 1, seen, "handler GuardTx must be reached after admitted epoch rotation")
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var name string
		require.NoError(t, ts.DB.QueryRow(`SELECT name FROM channel_groups WHERE id = $1`, groupID).Scan(&name))
		assert.Equal(t, "fenced-update", name)
	})

	t.Run("reorder", func(t *testing.T) {
		boundaryTS, boundary := testhelpers.SetupStaleBoundaryServer(t)
		ts, owner, serverID := setupWithServer(t, boundaryTS)
		channelID := ts.CreateTestChannel(t, serverID, "fenced-reorder")
		require.NoError(t, boundary.Arm(owner.AccessToken))
		boundary.Hook.ArmArg([]string{channelEpochGuardSQL}, owner.ID, nil, nil)

		w := ts.DoRequest(http.MethodPut, reorderPath(serverID), map[string]interface{}{
			"channels": []map[string]interface{}{{"channel_id": channelID, "position": 99}},
		}, testhelpers.AuthHeaders(owner.AccessToken))
		boundary.RequireRotated(t)
		seen, hookErr := boundary.Hook.Report()
		require.NoError(t, hookErr)
		require.Equal(t, 1, seen, "handler GuardTx must be reached after admitted epoch rotation")
		assert.Equal(t, http.StatusUnauthorized, w.Code, w.Body.String())
		var position int
		require.NoError(t, ts.DB.QueryRow(`SELECT position FROM channels WHERE id = $1`, channelID).Scan(&position))
		assert.NotEqual(t, 99, position)
	})
}
