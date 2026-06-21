package rbac_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	pathServersPrefix = "/api/v1/servers/"
	roleMember        = "member"
	pathMembers       = "/members/"
	pathBans          = "/bans/"
	methodDelete      = "DELETE"
)

// invalidatePermCache clears the Redis permission cache for a user in a server.
// Required when tests assign roles via direct DB inserts (bypassing handler-level cache invalidation).
func invalidatePermCache(t *testing.T, ts *testhelpers.TestServer, serverID, userID string) {
	t.Helper()
	ctx := context.Background()
	// Delete server-level key
	require.NoError(t, ts.Redis.Del(ctx, fmt.Sprintf("perm:%s:%s", serverID, userID)).Err())
	// Scan and delete channel-level keys
	iter := ts.Redis.Scan(ctx, 0, fmt.Sprintf("perm:%s:%s:*", serverID, userID), 100).Iterator()
	var keys []string
	for iter.Next(ctx) {
		keys = append(keys, iter.Val())
	}
	require.NoError(t, iter.Err())
	if len(keys) > 0 {
		require.NoError(t, ts.Redis.Unlink(ctx, keys...).Err())
	}
}

// --- Exhaustive Permission Matrix Tests ---

// TestBasePermissions_CompleteAllowedSet tests every permission included in BasePermissions.
func TestBasePermissions_CompleteAllowedSet(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "bpowner")
	member := ts.CreateTestUser(t, "bpmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Base Perm Matrix")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	allowed := []struct {
		name string
		perm rbac.Permission
	}{
		{"ViewTextChannels", rbac.PermViewTextChannels},
		{"ViewVoiceChannels", rbac.PermViewVoiceChannels},
		{"SendMessages", rbac.PermSendMessages},
		{"ReadMessageHistory", rbac.PermReadMessageHistory},
		{"ManageOwnMessages", rbac.PermManageOwnMessages},
		{"PinMessages", rbac.PermPinMessages},
		{"JoinVoice", rbac.PermJoinVoice},
		{"Speak", rbac.PermSpeak},
		{"ScreenShare", rbac.PermScreenShare},
		{"AttachFiles", rbac.PermAttachFiles},
		{"UseExternalEmoji", rbac.PermUseExternalEmoji},
		{"MentionRoles", rbac.PermMentionRoles},
		{"MentionUsers", rbac.PermMentionUsers},
		{"Video", rbac.PermVideo},
	}

	for _, tc := range allowed {
		t.Run("Base_Allowed_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.True(t, has, "base member should have %s", tc.name)
		})
	}
}

// TestBasePermissions_CompleteDeniedSet tests every permission NOT in BasePermissions.
func TestBasePermissions_CompleteDeniedSet(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "bdowner")
	member := ts.CreateTestUser(t, "bdmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Base Denied Matrix")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	denied := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageServer", rbac.PermManageServer},
		{"ManageRoles", rbac.PermManageRoles},
		{"ManageRolesAssign", rbac.PermManageRolesAssign},
		{"ManageChannels", rbac.PermManageChannels},
		{"ManageCryptoRotation", rbac.PermManageCryptoRotation},
		{"ViewAuditLog", rbac.PermViewAuditLog},
		{"Invite", rbac.PermInvite},
		{"Kick", rbac.PermKick},
		{"Ban", rbac.PermBan},
		{"ManageAllMessages", rbac.PermManageAllMessages},
		{"MuteMembers", rbac.PermMuteMembers},
		{"DeafenMembers", rbac.PermDeafenMembers},
		{"MoveMembers", rbac.PermMoveMembers},
		{"MentionEveryone", rbac.PermMentionEveryone},
		{"ManageDevResources", rbac.PermManageDevResources},
		{"Administrator", rbac.PermAdministrator},
	}

	for _, tc := range denied {
		t.Run("Base_Denied_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.False(t, has, "base member should NOT have %s", tc.name)
		})
	}
}

// TestModeratorPermissions_GainedOverBase tests permissions gained by moderator tier.
func TestModeratorPermissions_GainedOverBase(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "mpowner")
	member := ts.CreateTestUser(t, "mpmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Mod Perm Matrix")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	modRoleID := ts.CreateTestRole(t, serverID, "Moderator", 5, int64(rbac.ModeratorPermissions))
	ts.AssignRoleToUser(t, serverID, member.ID, modRoleID)

	gained := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageAllMessages", rbac.PermManageAllMessages},
		{"Kick", rbac.PermKick},
		{"MuteMembers", rbac.PermMuteMembers},
		{"DeafenMembers", rbac.PermDeafenMembers},
		{"MoveMembers", rbac.PermMoveMembers},
	}

	for _, tc := range gained {
		t.Run("Mod_Gained_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.True(t, has, "moderator should have %s", tc.name)
		})
	}

	// Still denied
	stillDenied := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageChannels", rbac.PermManageChannels},
		{"ManageRoles", rbac.PermManageRoles},
		{"ManageRolesAssign", rbac.PermManageRolesAssign},
		{"Invite", rbac.PermInvite},
		{"Ban", rbac.PermBan},
		{"ViewAuditLog", rbac.PermViewAuditLog},
		{"ManageServer", rbac.PermManageServer},
		{"ManageCryptoRotation", rbac.PermManageCryptoRotation},
		{"ManageDevResources", rbac.PermManageDevResources},
	}

	for _, tc := range stillDenied {
		t.Run("Mod_StillDenied_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.False(t, has, "moderator should NOT have %s", tc.name)
		})
	}
}

// TestAdminPermissions_GainedOverModerator tests permissions gained by admin tier.
func TestAdminPermissions_GainedOverModerator(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "apowner")
	member := ts.CreateTestUser(t, "apmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Perm Matrix")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
	ts.AssignRoleToUser(t, serverID, member.ID, adminRoleID)

	gained := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageChannels", rbac.PermManageChannels},
		{"ManageRoles", rbac.PermManageRoles},
		{"ManageRolesAssign", rbac.PermManageRolesAssign},
		{"Invite", rbac.PermInvite},
		{"Ban", rbac.PermBan},
		{"ViewAuditLog", rbac.PermViewAuditLog},
		{"ManageDevResources", rbac.PermManageDevResources},
	}

	for _, tc := range gained {
		t.Run("Admin_Gained_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.True(t, has, "admin should have %s", tc.name)
		})
	}

	// Still denied (owner-only)
	stillDenied := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageServer", rbac.PermManageServer},
		{"ManageCryptoRotation", rbac.PermManageCryptoRotation},
	}

	for _, tc := range stillDenied {
		t.Run("Admin_StillDenied_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, member.ID, "", tc.perm)
			require.NoError(t, err)
			assert.False(t, has, "admin should NOT have %s", tc.name)
		})
	}
}

// TestOwnerPermissions_Complete tests owner has all non-Administrator permissions.
func TestOwnerPermissions_Complete(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "opowner")
	serverID := ts.CreateTestServer(t, owner.ID, "Owner Perm Matrix")

	ownerOnly := []struct {
		name string
		perm rbac.Permission
	}{
		{"ManageServer", rbac.PermManageServer},
		{"ManageCryptoRotation", rbac.PermManageCryptoRotation},
		{"ManageChannels", rbac.PermManageChannels},
		{"ManageRoles", rbac.PermManageRoles},
		{"Kick", rbac.PermKick},
		{"Ban", rbac.PermBan},
		{"ViewAuditLog", rbac.PermViewAuditLog},
		{"SendMessages", rbac.PermSendMessages},
		{"JoinVoice", rbac.PermJoinVoice},
		{"MuteMembers", rbac.PermMuteMembers},
	}

	for _, tc := range ownerOnly {
		t.Run("Owner_Has_"+tc.name, func(t *testing.T) {
			has, err := resolver.HasPermission(ctx, serverID, owner.ID, "", tc.perm)
			require.NoError(t, err)
			assert.True(t, has, "owner should have %s", tc.name)
		})
	}
}

// --- SBAC Channel Override Tests ---

func TestSBAC_UserAllowOverridesRoleDeny(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "sbacowner1")
	member := ts.CreateTestUser(t, "sbacmember1")
	serverID := ts.CreateTestServer(t, owner.ID, "SBAC User Allow")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "restricted")

	// Get @all role ID
	var allRoleID string
	err := ts.DB.QueryRow(`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`, serverID).Scan(&allRoleID)
	require.NoError(t, err)

	// Role-deny SendMessages for @all
	ts.CreateChannelOverride(t, channelID, "role", allRoleID, 0, int64(rbac.PermSendMessages))

	// Verify member is denied at channel level
	has, err := resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.False(t, has, "should be denied by role override")

	// User-allow SendMessages for this specific member
	ts.CreateChannelOverride(t, channelID, "user", member.ID, int64(rbac.PermSendMessages), 0)
	invalidatePermCache(t, ts, serverID, member.ID)

	// Now member should be allowed (user-allow overrides role-deny)
	has, err = resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.True(t, has, "user-allow should override role-deny")
}

func TestSBAC_UserDenyIsFinalAuthority(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "sbacowner2")
	member := ts.CreateTestUser(t, "sbacmember2")
	serverID := ts.CreateTestServer(t, owner.ID, "SBAC User Deny Final")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "locked-member")

	// User-deny SendMessages
	ts.CreateChannelOverride(t, channelID, "user", member.ID, 0, int64(rbac.PermSendMessages))

	has, err := resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.False(t, has, "user-deny should be final authority, cannot be overridden")
}

func TestSBAC_AdminBitBypassesOverrides(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "sbacowner3")
	admin := ts.CreateTestUser(t, "sbacadmin3")
	serverID := ts.CreateTestServer(t, owner.ID, "SBAC Admin Bypass")
	ts.AddMemberToServer(t, serverID, admin.ID, roleMember)
	channelID := ts.CreateTestChannel(t, serverID, "admin-test")

	// Give admin the PermAdministrator bit
	adminRoleID := ts.CreateTestRole(t, serverID, "SuperAdmin", 10, int64(rbac.PermAdministrator))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	// User-deny SendMessages for admin
	ts.CreateChannelOverride(t, channelID, "user", admin.ID, 0, int64(rbac.PermSendMessages))

	// Administrator should bypass all SBAC
	has, err := resolver.HasPermission(ctx, serverID, admin.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.True(t, has, "PermAdministrator should bypass channel user-deny override")
}

func TestSBAC_MultipleRolesORd_WithOverride(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "sbacowner4")
	member := ts.CreateTestUser(t, "sbacmember4")
	serverID := ts.CreateTestServer(t, owner.ID, "SBAC Multi Role")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Create two roles with distinct permissions
	roleA := ts.CreateTestRole(t, serverID, "RoleA", 3, int64(rbac.PermInvite))
	roleB := ts.CreateTestRole(t, serverID, "RoleB", 4, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, serverID, member.ID, roleA)
	ts.AssignRoleToUser(t, serverID, member.ID, roleB)

	// Member should have both permissions (OR'd)
	hasInvite, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermInvite)
	require.NoError(t, err)
	assert.True(t, hasInvite, "should have PermInvite from RoleA")

	hasKick, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermKick)
	require.NoError(t, err)
	assert.True(t, hasKick, "should have PermKick from RoleB")
}

// --- Per-Handler Permission Enforcement Tests (via HTTP) ---

func TestUpdateServer_RequiresManageServer(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "srvpowner")
	member := ts.CreateTestUser(t, "srvpmember")
	serverID := ts.CreateTestServer(t, owner.ID, "UpdateServer Perm Test")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	body := map[string]interface{}{"name": "Updated Name"}

	// Base member → 403
	w := ts.DoRequest("PATCH", pathServersPrefix+serverID, body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 403, w.Code, "base member should be denied UpdateServer")

	// Owner → 200
	w = ts.DoRequest("PATCH", pathServersPrefix+serverID, body, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, 200, w.Code, "owner should be allowed UpdateServer")
}

func TestCreateRole_RequiresManageRoles(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "crpowner")
	member := ts.CreateTestUser(t, "crpmember")
	serverID := ts.CreateTestServer(t, owner.ID, "CreateRole Perm Test")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	body := map[string]interface{}{"name": "TestRole", "permissions": "0"}

	// Base member → 403
	w := ts.DoRequest("POST", pathServersPrefix+serverID+"/roles", body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 403, w.Code, "base member should be denied CreateRole")

	// Give member ManageRoles permission
	roleID := ts.CreateTestRole(t, serverID, "RoleManager", 5, int64(rbac.PermManageRoles))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)
	invalidatePermCache(t, ts, serverID, member.ID)

	// Now should succeed
	w = ts.DoRequest("POST", pathServersPrefix+serverID+"/roles", body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 201, w.Code, "member with ManageRoles should be allowed CreateRole")
}

func TestGetAuditLog_RequiresViewAuditLog(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "alowner")
	member := ts.CreateTestUser(t, "almember")
	serverID := ts.CreateTestServer(t, owner.ID, "AuditLog Perm Test")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Base member → 403
	w := ts.DoRequest("GET", pathServersPrefix+serverID+"/audit-log", nil, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 403, w.Code, "base member should be denied ViewAuditLog")

	// Owner → 200
	w = ts.DoRequest("GET", pathServersPrefix+serverID+"/audit-log", nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, 200, w.Code, "owner should be allowed ViewAuditLog")
}

func TestCreateInvite_RequiresInvitePermission(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "invowner")
	member := ts.CreateTestUser(t, "invmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Invite Perm Test")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	body := map[string]interface{}{"max_uses": 10}

	// Base member → 403 (PermInvite not in BasePermissions)
	w := ts.DoRequest("POST", pathServersPrefix+serverID+"/invites", body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 403, w.Code, "base member should be denied CreateInvite")

	// Give member Invite permission
	roleID := ts.CreateTestRole(t, serverID, "Inviter", 3, int64(rbac.PermInvite))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)
	invalidatePermCache(t, ts, serverID, member.ID)

	w = ts.DoRequest("POST", pathServersPrefix+serverID+"/invites", body, testhelpers.AuthHeaders(member.AccessToken))
	assert.Equal(t, 201, w.Code, "member with PermInvite should be allowed")
}

func TestKickMember_RequiresKickAndHierarchy(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "kickowner")
	moderator := ts.CreateTestUser(t, "kickmod")
	target := ts.CreateTestUser(t, "kicktarget")
	baseMember := ts.CreateTestUser(t, "kickbase")
	serverID := ts.CreateTestServer(t, owner.ID, "Kick Perm Test")
	ts.AddMemberToServer(t, serverID, moderator.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	ts.AddMemberToServer(t, serverID, baseMember.ID, roleMember)

	// Give moderator Kick permission at position 5
	modRoleID := ts.CreateTestRole(t, serverID, "Mod", 5, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, serverID, moderator.ID, modRoleID)

	// Base member (no kick perm) → 403
	w := ts.DoRequest("DELETE", pathServersPrefix+serverID+pathMembers+target.ID, nil, testhelpers.AuthHeaders(baseMember.AccessToken))
	assert.Equal(t, 403, w.Code, "base member without PermKick should be denied")

	// Moderator kicks target member (lower rank) → 200
	w = ts.DoRequest("DELETE", pathServersPrefix+serverID+pathMembers+target.ID, nil, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, 200, w.Code, "moderator should kick lower-ranked member")

	// Re-add target for next test
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)

	// Give target a higher-ranked role
	highRoleID := ts.CreateTestRole(t, serverID, "HighRank", 10, int64(rbac.PermKick))
	ts.AssignRoleToUser(t, serverID, target.ID, highRoleID)

	// Moderator (pos 5) tries to kick target (pos 10) → 403 hierarchy violation
	w = ts.DoRequest("DELETE", pathServersPrefix+serverID+pathMembers+target.ID, nil, testhelpers.AuthHeaders(moderator.AccessToken))
	assert.Equal(t, 403, w.Code, "cannot kick higher-ranked member")

	// Owner can kick anyone
	w = ts.DoRequest("DELETE", pathServersPrefix+serverID+pathMembers+target.ID, nil, testhelpers.AuthHeaders(owner.AccessToken))
	assert.Equal(t, 200, w.Code, "owner should kick any member")
}

func TestBanMember_RequiresBanAndHierarchy(t *testing.T) {
	ts := testhelpers.SetupTestServer(t)
	owner := ts.CreateTestUser(t, "banowner")
	admin := ts.CreateTestUser(t, "banadmin")
	target := ts.CreateTestUser(t, "bantarget")
	baseMember := ts.CreateTestUser(t, "banbase")
	serverID := ts.CreateTestServer(t, owner.ID, "Ban Perm Test")
	ts.AddMemberToServer(t, serverID, admin.ID, roleMember)
	ts.AddMemberToServer(t, serverID, target.ID, roleMember)
	ts.AddMemberToServer(t, serverID, baseMember.ID, roleMember)

	adminRoleID := ts.CreateTestRole(t, serverID, "Admin", 10, int64(rbac.AdminPermissions))
	ts.AssignRoleToUser(t, serverID, admin.ID, adminRoleID)

	// Base member → 403
	w := ts.DoRequest("POST", pathServersPrefix+serverID+pathBans+target.ID, nil, testhelpers.AuthHeaders(baseMember.AccessToken))
	assert.Equal(t, 403, w.Code, "base member should be denied BanMember")

	// Admin bans target (lower-ranked member) → 200
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathBans+target.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, 200, w.Code, "admin should ban lower-ranked member")

	// Admin cannot ban owner → 403 hierarchy
	w = ts.DoRequest("POST", pathServersPrefix+serverID+pathBans+owner.ID, nil, testhelpers.AuthHeaders(admin.AccessToken))
	assert.Equal(t, 403, w.Code, "admin should not ban server owner")
}

// TestCustomRoleWithSinglePermission tests adding exactly one non-base permission.
func TestCustomRoleWithSinglePermission(t *testing.T) {
	log := logger.New("test")
	ts := testhelpers.SetupTestServer(t)
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, log)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "crowner")
	member := ts.CreateTestUser(t, "crmember")
	serverID := ts.CreateTestServer(t, owner.ID, "Custom Role Single")
	ts.AddMemberToServer(t, serverID, member.ID, roleMember)

	// Verify member does NOT have PermMentionEveryone initially
	has, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermMentionEveryone)
	require.NoError(t, err)
	assert.False(t, has)

	// Create role with just PermMentionEveryone
	roleID := ts.CreateTestRole(t, serverID, "Announcer", 3, int64(rbac.PermMentionEveryone))
	ts.AssignRoleToUser(t, serverID, member.ID, roleID)
	invalidatePermCache(t, ts, serverID, member.ID)

	// Now they should have it
	has, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermMentionEveryone)
	require.NoError(t, err)
	assert.True(t, has, "custom role should grant exactly PermMentionEveryone")

	// But still not other non-base permissions
	has, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermKick)
	require.NoError(t, err)
	assert.False(t, has, "custom role should not grant unrelated permissions")
}
