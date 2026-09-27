package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A synchronized channel is materialized from its category. User-created
// temporary move grants own that authority and must block a concurrent manual
// category move instead of being silently replaced.
func TestUpdateChannel_RejectsTemporaryMoveGrant(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	sourceID := createGroup(t, ts, serverID, "handler-source", owner.AccessToken)
	destinationID := createGroup(t, ts, serverID, "handler-destination", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "handler-protected-channel")
	assignChannelToCategory(t, ts, channelID, sourceID, true)
	member := ts.CreateTestUser(t, "handler-protected-member")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO channel_permission_overrides
		(id, channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0, TRUE, 'move_granted', NOW())`,
		channelID, member.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodPatch, pathChannelsPrefix+channelID, map[string]interface{}{
		"name": "handler-protected-channel", "type": "voice", "group_id": destinationID,
	}, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "system-managed")
	assert.Equal(t, 1, rowCount(t, ts,
		`SELECT COUNT(*) FROM channels WHERE id = $1 AND group_id = $2 AND sync_permissions`, channelID, sourceID),
		"a system-managed temporary move must retain the original authority source")
}
