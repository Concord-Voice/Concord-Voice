package presence_test

import (
	"context"
	"testing"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/presence"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/rbac"
	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/testhelpers"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestAuthorizeAndMinimize_RealResolverEnforcesFreshVoiceVisibility(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	require.NoError(t, testhelpers.TruncateAllTables(ts.DB))
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "presence_rbac_owner")
	sender := ts.CreateTestUser(t, "presence_rbac_sender")
	baseMember := ts.CreateTestUser(t, "presence_rbac_base")
	administrator := ts.CreateTestUser(t, "presence_rbac_admin")
	deniedMember := ts.CreateTestUser(t, "presence_rbac_denied")
	serverID := ts.CreateTestServer(t, owner.ID, "Presence RBAC Server")
	for _, userID := range []string{sender.ID, baseMember.ID, administrator.ID, deniedMember.ID} {
		ts.AddMemberToServer(t, serverID, userID, "member")
	}
	administratorRoleID := ts.CreateTestRole(
		t,
		serverID,
		"Presence Administrator",
		100,
		int64(rbac.PermAdministrator),
	)
	ts.AssignRoleToUser(t, serverID, administrator.ID, administratorRoleID)
	channelID := ts.CreateVoiceChannel(t, serverID, "Presence Voice")
	_, err := ts.DB.Exec(
		`INSERT INTO voice_participants (channel_id, user_id) VALUES ($1, $2)`,
		channelID, sender.ID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO user_presence_settings (user_id, server_voice_tier, server_voice_show_details)
		 VALUES ($1, 2, TRUE)`,
		sender.ID,
	)
	require.NoError(t, err)

	serverUUID := uuid.MustParse(serverID)
	channelUUID := uuid.MustParse(channelID)
	senderUUID := uuid.MustParse(sender.ID)
	startedAt := int64(123)
	input := presence.PolicyInput{
		SenderID: senderUUID,
		Category: presence.CategoryServerVoice,
		ServerVoice: &presence.ServerVoicePolicyInput{
			Context: presence.ServerVoiceContext{ServerID: serverUUID, ChannelID: channelUUID},
			Payload: presence.ServerVoicePayload{
				ServerID: serverUUID, ChannelID: channelUUID,
				ServerName: "Presence RBAC Server", ChannelName: "Presence Voice", StartedAt: &startedAt,
			},
		},
	}
	resolver := rbac.NewResolver(ts.DB, nil, nil)

	decision, err := presence.AuthorizeAndMinimize(ctx, ts.DB, resolver, input)
	require.NoError(t, err)
	require.Equal(t, map[uuid.UUID]bool{
		uuid.MustParse(owner.ID):         true,
		uuid.MustParse(baseMember.ID):    true,
		uuid.MustParse(administrator.ID): true,
		uuid.MustParse(deniedMember.ID):  true,
	}, decision.Audience)

	viewVoice := int64(rbac.PermViewVoiceChannels)
	ts.CreateChannelOverride(t, channelID, "user", deniedMember.ID, viewVoice, viewVoice)
	decision, err = presence.AuthorizeAndMinimize(ctx, ts.DB, resolver, input)
	require.NoError(t, err)
	require.True(t, decision.Audience[uuid.MustParse(owner.ID)])
	require.True(t, decision.Audience[uuid.MustParse(baseMember.ID)])
	require.True(t, decision.Audience[uuid.MustParse(administrator.ID)])
	require.False(t, decision.Audience[uuid.MustParse(deniedMember.ID)], "user deny is final")

	var allRoleID string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID,
	).Scan(&allRoleID))
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, viewVoice)
	decision, err = presence.AuthorizeAndMinimize(ctx, ts.DB, resolver, input)
	require.NoError(t, err)
	require.True(t, decision.Audience[uuid.MustParse(owner.ID)], "owner bypasses SBAC")
	require.True(t, decision.Audience[uuid.MustParse(administrator.ID)], "administrator bypasses SBAC")
	require.False(t, decision.Audience[uuid.MustParse(baseMember.ID)])
	require.False(t, decision.Audience[uuid.MustParse(deniedMember.ID)])

	ts.CreateChannelOverride(t, channelID, "user", baseMember.ID, viewVoice, 0)
	result, err := ts.DB.Exec(`
		UPDATE channel_permission_overrides
		SET is_temporary = TRUE, temporary_reason = 'move_granted', granted_at = NOW()
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
	`, channelID, baseMember.ID)
	require.NoError(t, err)
	rowsAffected, err := result.RowsAffected()
	require.NoError(t, err)
	require.EqualValues(t, 1, rowsAffected)
	decision, err = presence.AuthorizeAndMinimize(ctx, ts.DB, resolver, input)
	require.NoError(t, err)
	require.True(t, decision.Audience[uuid.MustParse(owner.ID)])
	require.True(t, decision.Audience[uuid.MustParse(administrator.ID)])
	require.True(t, decision.Audience[uuid.MustParse(baseMember.ID)], "temporary user allow overrides role deny")
	require.False(t, decision.Audience[uuid.MustParse(deniedMember.ID)])
}
