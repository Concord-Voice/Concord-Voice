package channels_test

import (
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A category delete must refuse to remove a synchronized channel while a
// temporary move grant still owns its authority. This is the API-level guard;
// the sibling reorder guard is covered separately.
func TestDeleteChannelGroup_RejectsTemporaryMoveGrant(t *testing.T) {
	ts, owner, serverID := setupWithServer(t)
	groupID := createGroup(t, ts, serverID, "protected-delete", owner.AccessToken)
	channelID := ts.CreateVoiceChannel(t, serverID, "protected-delete-channel")
	assignChannelToCategory(t, ts, channelID, groupID, true)
	viewer := ts.CreateTestUser(t, "protected-delete-viewer")
	ts.AddMemberToServer(t, serverID, viewer.ID, roleMember)
	_, err := ts.DB.Exec(`
		INSERT INTO channel_permission_overrides
		(id, channel_id, target_type, target_id, allow, deny, is_temporary, temporary_reason, granted_at)
		VALUES (gen_random_uuid(), $1, 'user', $2, $3, 0, TRUE, 'move_granted', NOW())`,
		channelID, viewer.ID, int64(rbac.PermViewVoiceChannels|rbac.PermJoinVoice))
	require.NoError(t, err)

	w := ts.DoRequest(http.MethodDelete, groupPath(serverID, groupID), nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, http.StatusConflict, w.Code, w.Body.String())
	assert.Contains(t, w.Body.String(), "system-managed")
	assert.Equal(t, 1, rowCount(t, ts, `SELECT COUNT(*) FROM channel_groups WHERE id = $1`, groupID))
	assert.Equal(t, 1, rowCount(t, ts, `SELECT COUNT(*) FROM channels WHERE id = $1 AND group_id = $2`, channelID, groupID))
}
