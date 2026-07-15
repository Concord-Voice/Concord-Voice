package rbac_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	chanText1  = "text-1"
	chanVoice1 = "voice-1"
)

// --- GetVisibleChannelIDs Tests ---

func TestGetVisibleChannelIDsOwnerSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner1")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 1")
	ch1 := ts.CreateTestChannel(t, serverID, chanText1)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, owner.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
	assert.Len(t, ids, 2)
}

func TestGetVisibleChannelIDsNonMemberSeesNone(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner2")
	outsider := ts.CreateTestUser(t, "visoutsider2")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 2")
	ts.CreateTestChannel(t, serverID, chanText1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, outsider.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestGetVisibleChannelIDsBaseMemberSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner3")
	member := ts.CreateTestUser(t, "vismember3")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 3")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch1 := ts.CreateTestChannel(t, serverID, chanText1)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
}

func TestGetVisibleChannelIDsDenyHidesChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner4")
	member := ts.CreateTestUser(t, "vismember4")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 4")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chVisible := ts.CreateTestChannel(t, serverID, "visible")
	chHidden := ts.CreateTestChannel(t, serverID, "hidden")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny PermViewTextChannels for hidden channel
	ts.CreateChannelOverride(t, chHidden, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chVisible, "visible channel should be visible")
	assert.NotContains(t, ids, chHidden, "hidden channel should not be visible")
}

func TestGetVisibleChannelIDsDenyVoiceHidesVoiceChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner5")
	member := ts.CreateTestUser(t, "vismember5")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 5")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText := ts.CreateTestChannel(t, serverID, "text-chan")
	chVoice := ts.CreateVoiceChannel(t, serverID, "voice-chan")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny PermViewVoiceChannels for voice channel
	ts.CreateChannelOverride(t, chVoice, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chText, "text channel should still be visible")
	assert.NotContains(t, ids, chVoice, "voice channel with deny should be hidden")
}

func TestGetVisibleChannelIDsAdminSeesAll(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner6")
	admin := ts.CreateTestUser(t, "visadmin6")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 6")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")

	// Create admin role with Administrator bit
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	ch1 := ts.CreateTestChannel(t, serverID, chanText1)
	ch2 := ts.CreateVoiceChannel(t, serverID, chanVoice1)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, admin.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
	assert.Len(t, ids, 2)
}

func TestGetVisibleChannelIDsEmptyServer(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner7")
	serverID := ts.CreateTestServer(t, owner.ID, "Empty Vis Server")
	// No channels

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, owner.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
}

func TestGetVisibleChannelIDsUserAllowOverridesRoleDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner8")
	member := ts.CreateTestUser(t, "vismember8")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 8")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "restricted")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny view for @all role
	ts.CreateChannelOverride(t, ch, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	// But allow for this specific user
	ts.CreateChannelOverride(t, ch, "user", member.ID, int64(rbac.PermViewTextChannels), 0)

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "user-allow should override role-deny for visibility")
}

func TestGetVisibleChannelIDsUserDenyHidesChannel(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner9")
	member := ts.CreateTestUser(t, "vismember9")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 9")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chVisible := ts.CreateTestChannel(t, serverID, "visible-9")
	chHidden := ts.CreateTestChannel(t, serverID, "hidden-9")

	// User-deny PermViewTextChannels on hidden channel
	ts.CreateChannelOverride(t, chHidden, "user", member.ID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chVisible, "channel without user-deny should be visible")
	assert.NotContains(t, ids, chHidden, "channel with user-deny should be hidden")
}

func TestGetVisibleChannelIDsUserDenyOverridesUserAllow(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner10")
	member := ts.CreateTestUser(t, "vismember10")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 10")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "contested-10")

	// Channel is visible from base permissions (no role overrides)
	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "channel should be visible from base permissions")

	// Apply user-allow AND user-deny for the same permission on the same channel.
	// No role-level deny — isolates the user-allow → user-deny precedence.
	ts.CreateChannelOverride(t, ch, "user", member.ID,
		int64(rbac.PermViewTextChannels), // allow
		int64(rbac.PermViewTextChannels), // deny (final authority)
	)

	ids, err = resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.NotContains(t, ids, ch, "user-deny should override user-allow (deny is final authority)")
}

func TestGetVisibleChannelIDsMultipleRoleOverridesBITOR(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner11")
	member := ts.CreateTestUser(t, "vismember11")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 11")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "multi-role-11")

	// Strip PermViewTextChannels from @all base permissions so visibility
	// depends entirely on channel overrides
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE roles SET permissions = permissions & ~$1::bigint WHERE id = $2`,
		int64(rbac.PermViewTextChannels), allRoleID)
	require.NoError(t, err)

	// Role-A override: allows PermSendMessages only (not view)
	roleA := ts.CreateTestRole(t, serverID, "RoleA-11", 2, 0)
	ts.AssignRoleToUser(t, serverID, member.ID, roleA)
	ts.CreateChannelOverride(t, ch, "role", roleA, int64(rbac.PermSendMessages), 0)

	// Role-B override: allows PermViewTextChannels
	roleB := ts.CreateTestRole(t, serverID, "RoleB-11", 3, 0)
	ts.AssignRoleToUser(t, serverID, member.ID, roleB)
	ts.CreateChannelOverride(t, ch, "role", roleB, int64(rbac.PermViewTextChannels), 0)

	// BIT_OR of role overrides = PermSendMessages | PermViewTextChannels
	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "BIT_OR of multiple role allow overrides should grant visibility")
}

func TestGetVisibleChannelIDsRoleAllowGrantsVisibility(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner12")
	member := ts.CreateTestUser(t, "vismember12")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 12")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "regrant-12")

	// Strip PermViewTextChannels from @all base permissions so the member
	// has no view bit in their base perms
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	_, err = ts.DB.Exec(`UPDATE roles SET permissions = permissions & ~$1::bigint WHERE id = $2`,
		int64(rbac.PermViewTextChannels), allRoleID)
	require.NoError(t, err)

	// Without override, channel should be hidden
	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.NotContains(t, ids, ch, "channel should be hidden without view permission")

	// Custom role override grants PermViewTextChannels on the channel
	customRole := ts.CreateTestRole(t, serverID, "Viewer-12", 5, 0)
	ts.AssignRoleToUser(t, serverID, member.ID, customRole)
	ts.CreateChannelOverride(t, ch, "role", customRole, int64(rbac.PermViewTextChannels), 0)

	ids, err = resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "role-allow override should grant visibility when base perms lack view bit")
}

func TestGetVisibleChannelIDsNoViewPermsSeesNothing(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner13")
	member := ts.CreateTestUser(t, "vismember13")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 13")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText1 := ts.CreateTestChannel(t, serverID, "text-13a")
	chText2 := ts.CreateTestChannel(t, serverID, "text-13b")
	chVoice := ts.CreateVoiceChannel(t, serverID, "voice-13")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny both view permissions on every channel
	ts.CreateChannelOverride(t, chText1, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, chText2, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, chVoice, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Empty(t, ids, "member with all view perms denied should see no channels")
	assert.Equal(t, []string{}, ids, "should return empty slice, not nil")
}

func TestGetVisibleChannelIDsTextVisibleVoiceHidden(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner14")
	member := ts.CreateTestUser(t, "vismember14")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 14")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText1 := ts.CreateTestChannel(t, serverID, "text-14a")
	chText2 := ts.CreateTestChannel(t, serverID, "text-14b")
	chVoice1 := ts.CreateVoiceChannel(t, serverID, "voice-14a")
	chVoice2 := ts.CreateVoiceChannel(t, serverID, "voice-14b")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny voice view on both voice channels
	ts.CreateChannelOverride(t, chVoice1, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, chVoice2, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Len(t, ids, 2, "should see only text channels")
	assert.Contains(t, ids, chText1)
	assert.Contains(t, ids, chText2)
	assert.NotContains(t, ids, chVoice1)
	assert.NotContains(t, ids, chVoice2)
}

func TestGetVisibleChannelIDsVoiceVisibleTextHidden(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner15")
	member := ts.CreateTestUser(t, "vismember15")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 15")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText1 := ts.CreateTestChannel(t, serverID, "text-15a")
	chText2 := ts.CreateTestChannel(t, serverID, "text-15b")
	chVoice1 := ts.CreateVoiceChannel(t, serverID, "voice-15a")
	chVoice2 := ts.CreateVoiceChannel(t, serverID, "voice-15b")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny text view on both text channels
	ts.CreateChannelOverride(t, chText1, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, chText2, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, member.ID)
	require.NoError(t, err)
	assert.Len(t, ids, 2, "should see only voice channels")
	assert.Contains(t, ids, chVoice1)
	assert.Contains(t, ids, chVoice2)
	assert.NotContains(t, ids, chText1)
	assert.NotContains(t, ids, chText2)
}

func TestGetVisibleChannelIDsAdminIgnoresDenyOverrides(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "visowner16")
	admin := ts.CreateTestUser(t, "visadmin16")
	serverID := ts.CreateTestServer(t, owner.ID, "Visibility Server 16")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")

	// Create admin role
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin-16", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	ch1 := ts.CreateTestChannel(t, serverID, "text-16")
	ch2 := ts.CreateVoiceChannel(t, serverID, "voice-16")

	// Get @all role
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Deny view on both channels via role override
	ts.CreateChannelOverride(t, ch1, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, ch2, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))
	// Also add user-deny on admin for good measure
	ts.CreateChannelOverride(t, ch1, "user", admin.ID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetVisibleChannelIDs(ctx, serverID, admin.ID)
	require.NoError(t, err)
	assert.Len(t, ids, 2, "admin should see all channels despite deny overrides")
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
}

func TestFilterVisibleUserIDsForChannelFreshMatchesPerViewerVisibility(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "channelviewowner")
	visible := ts.CreateTestUser(t, "channelviewvisible")
	admin := ts.CreateTestUser(t, "channelviewadmin")
	denied := ts.CreateTestUser(t, "channelviewdenied")
	overrideAdmin := ts.CreateTestUser(t, "channelviewoverrideadmin")
	temporary := ts.CreateTestUser(t, "channelviewtemporary")
	outsider := ts.CreateTestUser(t, "channelviewoutsider")
	serverID := ts.CreateTestServer(t, owner.ID, "Channel Viewer Server")
	for _, userID := range []string{visible.ID, admin.ID, denied.ID, overrideAdmin.ID, temporary.ID} {
		ts.AddMemberToServer(t, serverID, userID, "member")
	}

	adminRoleID := ts.CreateTestRole(t, serverID, "Channel Viewer Admin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)
	channelID := ts.CreateVoiceChannel(t, serverID, "Channel Viewer Voice")
	viewVoice := int64(rbac.PermViewVoiceChannels)
	ts.CreateChannelOverride(t, channelID, "user", denied.ID, 0, viewVoice)
	ts.CreateChannelOverride(t, channelID, "user", outsider.ID, viewVoice, 0)
	ts.CreateChannelOverride(
		t,
		channelID,
		"user",
		overrideAdmin.ID,
		int64(rbac.PermAdministrator|rbac.PermViewTextChannels|rbac.PermJoinVoice|rbac.PermSpeak),
		viewVoice,
	)
	temporaryDeniedRole := ts.CreateTestRole(t, serverID, "Temporary Viewer Deny", 2, 0)
	ts.AssignRoleToUser(t, serverID, temporary.ID, temporaryDeniedRole)
	ts.CreateChannelOverride(t, channelID, "role", temporaryDeniedRole, 0, viewVoice)

	candidates := []string{
		owner.ID,
		visible.ID,
		admin.ID,
		denied.ID,
		overrideAdmin.ID,
		temporary.ID,
		outsider.ID,
		visible.ID,
	}
	bulk, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, candidates)
	require.NoError(t, err)

	expected := make([]string, 0, len(candidates))
	seen := make(map[string]bool)
	for _, candidateID := range candidates {
		if seen[candidateID] {
			continue
		}
		seen[candidateID] = true
		visibleChannelIDs, resolveErr := resolver.GetVisibleChannelIDs(ctx, serverID, candidateID)
		require.NoError(t, resolveErr)
		if slices.Contains(visibleChannelIDs, channelID) {
			expected = append(expected, candidateID)
		}
	}
	require.ElementsMatch(t, expected, bulk)
	require.NotContains(t, bulk, denied.ID)
	require.NotContains(t, bulk, overrideAdmin.ID, "SBAC-granted administrator is not a bypass")
	require.NotContains(t, bulk, temporary.ID)
	require.NotContains(t, bulk, outsider.ID)

	ts.CreateChannelOverride(t, channelID, "user", temporary.ID, viewVoice, 0)
	_, err = ts.DB.Exec(`
		UPDATE channel_permission_overrides
		SET is_temporary = TRUE, temporary_reason = 'move_granted', granted_at = NOW()
		WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2
	`, channelID, temporary.ID)
	require.NoError(t, err)
	temporaryViewers, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, []string{temporary.ID})
	require.NoError(t, err)
	require.Equal(t, []string{temporary.ID}, temporaryViewers, "fresh temporary allow overrides role deny")
	_, err = ts.DB.Exec(
		`DELETE FROM channel_permission_overrides
		 WHERE channel_id = $1 AND target_type = 'user' AND target_id = $2 AND is_temporary = TRUE`,
		channelID,
		temporary.ID,
	)
	require.NoError(t, err)
	temporaryViewers, err = resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, []string{temporary.ID})
	require.NoError(t, err)
	require.Empty(t, temporaryViewers, "revoked temporary access disappears without a cache read")

	_, err = ts.DB.Exec(`DELETE FROM server_members WHERE server_id = $1 AND user_id = $2`, serverID, owner.ID)
	require.NoError(t, err)
	staleOwner, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, []string{owner.ID})
	require.NoError(t, err)
	require.Empty(t, staleOwner, "owner bypass still requires current membership")

	empty, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, channelID, nil)
	require.NoError(t, err)
	require.Equal(t, []string{}, empty)

	otherOwner := ts.CreateTestUser(t, "channelviewotherowner")
	otherServerID := ts.CreateTestServer(t, otherOwner.ID, "Other Channel Viewer Server")
	otherChannelID := ts.CreateVoiceChannel(t, otherServerID, "Other Channel Viewer Voice")
	wrongScope, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverID, otherChannelID, candidates)
	require.NoError(t, err)
	require.Empty(t, wrongScope)

	invalid, err := resolver.FilterVisibleUserIDsForChannelFresh(
		ctx,
		serverID,
		channelID,
		[]string{visible.ID, "not-a-uuid"},
	)
	require.Error(t, err)
	require.Nil(t, invalid, "one malformed candidate must fail the whole fresh decision")
}

func TestFilterVisibleUserIDsForChannelFreshIgnoresCrossServerRoleMapping(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	ownerA := ts.CreateTestUser(t, "channelviewcrossownera")
	member := ts.CreateTestUser(t, "channelviewcrossmember")
	serverA := ts.CreateTestServer(t, ownerA.ID, "Channel Viewer Cross A")
	ts.AddMemberToServer(t, serverA, member.ID, "member")
	channelA := ts.CreateVoiceChannel(t, serverA, "Channel Viewer Cross Voice")

	var defaultRoleA string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverA,
	).Scan(&defaultRoleA))
	viewVoice := int64(rbac.PermViewVoiceChannels)
	_, err := ts.DB.Exec(
		`UPDATE roles SET permissions = permissions & ~$1::bigint WHERE id = $2`,
		viewVoice,
		defaultRoleA,
	)
	require.NoError(t, err)

	ownerB := ts.CreateTestUser(t, "channelviewcrossownerb")
	serverB := ts.CreateTestServer(t, ownerB.ID, "Channel Viewer Cross B")
	foreignRole := ts.CreateTestRole(t, serverB, "Foreign Viewer", 2, 0)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverA,
		member.ID,
		foreignRole,
	)
	require.NoError(t, err, "schema permits the malformed cross-server role mapping fixture")
	ts.CreateChannelOverride(t, channelA, "role", foreignRole, viewVoice, 0)

	viewers, err := resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverA, channelA, []string{member.ID})
	require.NoError(t, err)
	require.Empty(t, viewers, "a foreign role allow must not authorize this server")

	_, err = ts.DB.Exec(
		`UPDATE roles SET permissions = permissions | $1::bigint WHERE id = $2`,
		viewVoice,
		defaultRoleA,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`UPDATE channel_permission_overrides SET allow = 0, deny = $1
		 WHERE channel_id = $2 AND target_type = 'role' AND target_id = $3`,
		viewVoice,
		channelA,
		foreignRole,
	)
	require.NoError(t, err)

	viewers, err = resolver.FilterVisibleUserIDsForChannelFresh(ctx, serverA, channelA, []string{member.ID})
	require.NoError(t, err)
	require.Equal(t, []string{member.ID}, viewers, "a foreign role deny must not hide this server")
}

func TestFilterVisibleUserIDsForChannelFreshUsesOneQueryForCandidateSet(t *testing.T) {
	candidates := make([]string, 1000)
	for i := range candidates {
		candidates[i] = uuid.NewString()
	}
	connection := &channelViewerCountingConnection{resultIDs: []string{candidates[999]}}
	db := sql.OpenDB(&channelViewerCountingConnector{connection: connection})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	resolver := rbac.NewResolver(db, nil, nil)

	viewers, err := resolver.FilterVisibleUserIDsForChannelFresh(
		context.Background(),
		uuid.NewString(),
		uuid.NewString(),
		candidates,
	)
	require.NoError(t, err)
	require.Equal(t, []string{candidates[999]}, viewers)
	require.Equal(t, 1, connection.queryCount)
	require.Contains(t, connection.query, "unnest($3::uuid[])")
	require.Len(t, connection.args, 6)

	viewers, err = resolver.FilterVisibleUserIDsForChannelFresh(
		context.Background(),
		uuid.NewString(),
		uuid.NewString(),
		nil,
	)
	require.NoError(t, err)
	require.Equal(t, []string{}, viewers)
	require.Equal(t, 1, connection.queryCount, "an empty candidate set must not query")
}

type channelViewerCountingConnector struct {
	connection *channelViewerCountingConnection
}

func (connector *channelViewerCountingConnector) Connect(context.Context) (driver.Conn, error) {
	return connector.connection, nil
}

func (connector *channelViewerCountingConnector) Driver() driver.Driver {
	return channelViewerCountingDriver{}
}

type channelViewerCountingDriver struct{}

func (channelViewerCountingDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("channel viewer test driver requires Connector")
}

type channelViewerCountingConnection struct {
	queryCount int
	query      string
	args       []driver.NamedValue
	resultIDs  []string
}

func (*channelViewerCountingConnection) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}

func (*channelViewerCountingConnection) Close() error { return nil }

func (*channelViewerCountingConnection) Begin() (driver.Tx, error) {
	return nil, errors.New("unexpected transaction")
}

func (connection *channelViewerCountingConnection) QueryContext(
	_ context.Context,
	query string,
	args []driver.NamedValue,
) (driver.Rows, error) {
	connection.queryCount++
	connection.query = query
	connection.args = append([]driver.NamedValue(nil), args...)
	return &channelViewerCountingRows{ids: append([]string(nil), connection.resultIDs...)}, nil
}

type channelViewerCountingRows struct {
	ids   []string
	index int
}

func (*channelViewerCountingRows) Columns() []string { return []string{"user_id"} }

func (*channelViewerCountingRows) Close() error { return nil }

func (rows *channelViewerCountingRows) Next(values []driver.Value) error {
	if rows.index >= len(rows.ids) {
		return io.EOF
	}
	values[0] = rows.ids[rows.index]
	rows.index++
	return nil
}

// --- GetAllVisibleChannelIDs Tests ---
//
// GetAllVisibleChannelIDs is the single-query, cross-server counterpart to
// GetVisibleChannelIDs. These tests verify it produces the same visibility
// decisions (owner/admin fast paths, per-channel SBAC allow/deny, type-aware
// view bits) while aggregating across every server the user belongs to.

func TestGetAllVisibleChannelIDsAggregatesAcrossServers(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	user := ts.CreateTestUser(t, "allvisuser1")
	otherOwner := ts.CreateTestUser(t, "allvisother1")

	// Server A: user is owner.
	serverA := ts.CreateTestServer(t, user.ID, "AllVis Server A")
	a1 := ts.CreateTestChannel(t, serverA, "a-text")
	a2 := ts.CreateVoiceChannel(t, serverA, "a-voice")

	// Server B: user is a plain member.
	serverB := ts.CreateTestServer(t, otherOwner.ID, "AllVis Server B")
	ts.AddMemberToServer(t, serverB, user.ID, "member")
	b1 := ts.CreateTestChannel(t, serverB, "b-text")

	// Server C: user is NOT a member.
	serverC := ts.CreateTestServer(t, otherOwner.ID, "AllVis Server C")
	c1 := ts.CreateTestChannel(t, serverC, "c-text")

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, user.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, a1)
	assert.Contains(t, ids, a2)
	assert.Contains(t, ids, b1)
	assert.NotContains(t, ids, c1, "channels in a server the user does not belong to must be excluded")
	assert.Len(t, ids, 3)
}

func TestGetAllVisibleChannelIDsEmptyForNonMember(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	outsider := ts.CreateTestUser(t, "allvisoutsider2")
	owner := ts.CreateTestUser(t, "allvisowner2")
	serverID := ts.CreateTestServer(t, owner.ID, "AllVis Server 2")
	ts.CreateTestChannel(t, serverID, "lonely")

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, outsider.ID)
	require.NoError(t, err)
	assert.Empty(t, ids)
	assert.Equal(t, []string{}, ids, "should return empty slice, not nil")
}

func TestGetAllVisibleChannelIDsRespectsDenyOverride(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner3")
	member := ts.CreateTestUser(t, "allvismember3")
	serverID := ts.CreateTestServer(t, owner.ID, "AllVis Server 3")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chVisible := ts.CreateTestChannel(t, serverID, "visible-3")
	chHidden := ts.CreateTestChannel(t, serverID, "hidden-3")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, chHidden, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chVisible)
	assert.NotContains(t, ids, chHidden, "role-deny of the view bit must hide the channel")
}

func TestGetAllVisibleChannelIDsAdminSeesAllDespiteDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner4")
	admin := ts.CreateTestUser(t, "allvisadmin4")
	serverID := ts.CreateTestServer(t, owner.ID, "AllVis Server 4")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	adminRoleID := ts.CreateTestRole(t, serverID, "Admin-av4", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	ch1 := ts.CreateTestChannel(t, serverID, "text-av4")
	ch2 := ts.CreateVoiceChannel(t, serverID, "voice-av4")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, ch1, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, ch2, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))
	ts.CreateChannelOverride(t, ch1, "user", admin.ID, 0, int64(rbac.PermViewTextChannels))

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, admin.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch1)
	assert.Contains(t, ids, ch2)
	assert.Len(t, ids, 2, "administrator sees all channels; SBAC cannot restrict them")
}

func TestGetAllVisibleChannelIDsTypeAware(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner5")
	member := ts.CreateTestUser(t, "allvismember5")
	serverID := ts.CreateTestServer(t, owner.ID, "AllVis Server 5")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	chText := ts.CreateTestChannel(t, serverID, "text-av5")
	chVoice := ts.CreateVoiceChannel(t, serverID, "voice-av5")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	// Deny only the voice view bit — the text channel stays visible.
	ts.CreateChannelOverride(t, chVoice, "role", allRoleID, 0, int64(rbac.PermViewVoiceChannels))

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, chText)
	assert.NotContains(t, ids, chVoice, "voice-view deny must not hide via the text bit")
}

func TestGetAllVisibleChannelIDsUserAllowOverridesRoleDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner6")
	member := ts.CreateTestUser(t, "allvismember6")
	serverID := ts.CreateTestServer(t, owner.ID, "AllVis Server 6")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	ch := ts.CreateTestChannel(t, serverID, "restricted-av6")

	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)
	ts.CreateChannelOverride(t, ch, "role", allRoleID, 0, int64(rbac.PermViewTextChannels))
	ts.CreateChannelOverride(t, ch, "user", member.ID, int64(rbac.PermViewTextChannels), 0)

	ids, err := resolver.GetAllVisibleChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, ids, ch, "user-allow should override role-deny for visibility")
}

// TestGetAllVisibleChannelIDsMatchesPerServer asserts parity with the per-server
// resolver across a mixed multi-server setup: the aggregate result must equal the
// union of GetVisibleChannelIDs over each of the user's servers.
func TestGetAllVisibleChannelIDsMatchesPerServer(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner7")
	member := ts.CreateTestUser(t, "allvismember7")

	// Server 1: member with a hidden channel.
	server1 := ts.CreateTestServer(t, owner.ID, "AllVis Parity 1")
	ts.AddMemberToServer(t, server1, member.ID, "member")
	ts.CreateTestChannel(t, server1, "p1-visible")
	p1Hidden := ts.CreateTestChannel(t, server1, "p1-hidden")
	var role1 string
	require.NoError(t, ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, server1).Scan(&role1))
	ts.CreateChannelOverride(t, p1Hidden, "role", role1, 0, int64(rbac.PermViewTextChannels))

	// Server 2: member is the owner (sees all).
	server2 := ts.CreateTestServer(t, member.ID, "AllVis Parity 2")
	ts.CreateTestChannel(t, server2, "p2-text")
	ts.CreateVoiceChannel(t, server2, "p2-voice")

	// Server 3: member is not part of it — must contribute nothing.
	server3 := ts.CreateTestServer(t, owner.ID, "AllVis Parity 3")
	ts.CreateTestChannel(t, server3, "p3-text")

	expected := map[string]bool{}
	for _, sid := range []string{server1, server2} {
		perServer, err := resolver.GetVisibleChannelIDs(ctx, sid, member.ID)
		require.NoError(t, err)
		for _, id := range perServer {
			expected[id] = true
		}
	}

	all, err := resolver.GetAllVisibleChannelIDs(ctx, member.ID)
	require.NoError(t, err)

	got := map[string]bool{}
	for _, id := range all {
		got[id] = true
	}
	assert.Equal(t, expected, got, "aggregate must equal the union of per-server visibility")
}

// TestGetAllVisibleChannelIDsScopesRoleOverridesByServer verifies that a channel
// override targeting a role the user holds in a DIFFERENT server does not leak into
// the cross-server resolver. user_roles spans every server the user belongs to, so
// without server-scoping the role match, a role from server B could satisfy an
// override on a channel in server A (target_id is only UUID-validated) and make
// GetAllVisibleChannelIDs disagree with the per-server GetVisibleChannelIDs.
func TestGetAllVisibleChannelIDsScopesRoleOverridesByServer(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "allvisowner8")
	member := ts.CreateTestUser(t, "allvismember8")

	// Server A: member sees chA via base @all permissions.
	serverA := ts.CreateTestServer(t, owner.ID, "AllVis CrossRole A")
	ts.AddMemberToServer(t, serverA, member.ID, "member")
	chA := ts.CreateTestChannel(t, serverA, "a-text-8")

	// Server B: member holds server B's @all role.
	serverB := ts.CreateTestServer(t, owner.ID, "AllVis CrossRole B")
	ts.AddMemberToServer(t, serverB, member.ID, "member")
	var roleBAll string
	require.NoError(t, ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverB).Scan(&roleBAll))

	// Cross-server override: deny text-view on chA (server A) via a role that lives
	// in server B. The per-server resolver ignores it (roleBAll is not a server-A
	// role); the cross-server resolver must ignore it too.
	ts.CreateChannelOverride(t, chA, "role", roleBAll, 0, int64(rbac.PermViewTextChannels))

	// Per-server visibility is unaffected: chA stays visible in server A.
	perServer, err := resolver.GetVisibleChannelIDs(ctx, serverA, member.ID)
	require.NoError(t, err)
	assert.Contains(t, perServer, chA, "cross-server role override must not hide the channel per-server")

	// Cross-server visibility must agree: chA remains visible.
	all, err := resolver.GetAllVisibleChannelIDs(ctx, member.ID)
	require.NoError(t, err)
	assert.Contains(t, all, chA, "a role override targeting another server's role must not hide the channel")
}

// --- GetEffectivePermissions Additional Tests ---

func TestGetEffectivePermissionsCacheHit(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "effowner1")
	member := ts.CreateTestUser(t, "effmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "EffPerm Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// First call: computes and caches
	perms1, err := resolver.GetEffectivePermissions(ctx, serverID, member.ID, "")
	require.NoError(t, err)
	assert.True(t, perms1.Has(rbac.PermViewTextChannels))

	// Second call: should hit cache and return same result
	perms2, err := resolver.GetEffectivePermissions(ctx, serverID, member.ID, "")
	require.NoError(t, err)
	assert.Equal(t, perms1, perms2)
}

func TestGetEffectivePermissionsNonMemberReturnsError(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "effowner2")
	outsider := ts.CreateTestUser(t, "effoutsider2")
	serverID := ts.CreateTestServer(t, owner.ID, "EffPerm Server 2")

	perms, err := resolver.GetEffectivePermissions(ctx, serverID, outsider.ID, "")
	assert.Error(t, err)
	assert.Equal(t, rbac.Permission(0), perms)
}

func TestHasPermissionCacheHit(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "hpowner1")
	member := ts.CreateTestUser(t, "hpmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "HP Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// First call populates cache
	hasPerm, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermViewTextChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm)

	// Verify cache was populated
	cache := rbac.NewPermissionCache(ts.Redis)
	cached, ok := cache.Get(ctx, serverID, member.ID, "")
	assert.True(t, ok, "cache should be populated after HasPermission call")
	assert.True(t, cached.Has(rbac.PermViewTextChannels))
}
