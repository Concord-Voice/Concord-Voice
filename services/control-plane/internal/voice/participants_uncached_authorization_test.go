package voice_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/stretchr/testify/require"
)

func TestGetParticipants_StaleViewCacheCannotBypassRevocation(t *testing.T) {
	ts := setupTS(t)
	owner := ts.CreateTestUser(t, "vp_stale_owner")
	member := ts.CreateTestUser(t, "vp_stale_member")
	serverID := ts.CreateTestServer(t, owner.ID, "Voice Participant Cache Revoke")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateVoiceChannel(t, serverID, "voice-participant-cache-revoke")

	// Seed the permission the caller had before its temporary access was revoked.
	cache := rbac.NewPermissionCache(ts.Redis)
	tags := testhelpers.SeedPermissionGenerations(t, ts.Redis, serverID, member.ID)
	require.NoError(t, cache.Set(context.Background(), serverID, member.ID, channelID, rbac.PermViewVoiceChannels, tags))
	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	w := ts.DoRequest(
		"GET", pathChannelsPrefix+channelID+pathVoiceParticipants, nil, testhelpers.AuthHeaders(member.AccessToken),
	)
	require.Equal(t, http.StatusForbidden, w.Code)
}
