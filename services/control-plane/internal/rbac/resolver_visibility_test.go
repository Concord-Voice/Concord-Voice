package rbac_test

import (
	"context"
	"testing"

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
