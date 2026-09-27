package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	dbtest "github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers/testdb"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The channel can change authority between preflight and the mutation
// transaction. The first attempt must roll back on that mismatch, then retry
// from the live state and materialize only the destination category's rows.
func TestUpdateChannel_RetriesAfterAuthoritySetChange(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	initialID := createGroup(t, ts, serverID, "retry-initial", owner.AccessToken)
	intermediateID := createGroup(t, ts, serverID, "retry-intermediate", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "retry-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "retry-channel")
	assignChannelToCategory(t, ts, channelID, initialID, true)
	member := ts.CreateTestUser(t, "retry-authority-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO category_permission_overrides (id, category_id, target_type, target_id, allow, deny)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0),
		       (gen_random_uuid(), $4, 'user', $2, 0, $5)`,
		initialID, member.ID, int64(rbac.PermViewVoiceChannels), destinationID, int64(rbac.PermJoinVoice))
	require.NoError(t, err)
	_, err = ts.DB.Exec(`
		INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		SELECT gen_random_uuid(), $1, target_type, target_id, allow, deny
		FROM category_permission_overrides WHERE category_id = $2`, channelID, initialID)
	require.NoError(t, err)

	barrier, probe := holdGroupMutationVisibility(t, ts, serverID)
	response := make(chan int, 1)
	go func() {
		w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+channelID, map[string]interface{}{
			"name": "retry-channel", "type": "voice", "group_id": destinationID,
		}, testhelpers.AuthHeaders(owner.AccessToken))
		response <- w.Code
	}()
	dbtest.WaitForAdvisoryLockWaiter(t, probe, mustGroupVisibilityKey(t, serverID))

	// Change the materialized authority while the request is between preflight
	// and its first mutation transaction. This makes attempt one stale.
	_, err = barrier.Exec(`UPDATE channels SET group_id = $1, sync_permissions = TRUE WHERE id = $2`, intermediateID, channelID)
	require.NoError(t, err)
	require.NoError(t, barrier.Commit())

	assert.Equal(t, http.StatusOK, <-response)
	var groupID string
	var synced bool
	require.NoError(t, ts.DB.QueryRow(`SELECT group_id, sync_permissions FROM channels WHERE id = $1`, channelID).Scan(&groupID, &synced))
	assert.Equal(t, destinationID, groupID)
	assert.True(t, synced)
	assert.Equal(t, 1, rowCount(t, ts, `
		SELECT COUNT(*) FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, member.ID))
	var allow, deny int64
	require.NoError(t, ts.DB.QueryRow(`
		SELECT allow, deny FROM channel_permission_overrides
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2`, channelID, member.ID).Scan(&allow, &deny))
	assert.Zero(t, allow, "the stale first attempt must not leave the initial authority behind")
	assert.Equal(t, int64(rbac.PermJoinVoice), deny, "the successful retry must materialize destination authority")
}
