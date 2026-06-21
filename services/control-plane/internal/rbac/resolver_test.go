package rbac_test

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/rbac"
	"github.com/markdrogersjr/Concord/services/control-plane/internal/testhelpers"
	"github.com/markdrogersjr/Concord/services/control-plane/pkg/logger"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setupResolver creates a Resolver backed by real DB + Redis for integration tests.
func setupResolver(t *testing.T) (*rbac.Resolver, *testhelpers.TestServer) {
	t.Helper()
	ts := testhelpers.SetupTestServer(t)
	log := logger.New("test")
	cache := rbac.NewPermissionCache(ts.Redis)
	resolver := rbac.NewResolver(ts.DB, cache, log)
	return resolver, ts
}

func TestHasPermissionBaseMember(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner1")
	member := ts.CreateTestUser(t, "member1")
	serverID := ts.CreateTestServer(t, owner.ID, "Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Base member should have all BasePermissions via @all role
	hasPerm, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermViewTextChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm, "base member should have PermViewTextChannels")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermViewVoiceChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm, "base member should have PermViewVoiceChannels")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermSendMessages)
	require.NoError(t, err)
	assert.True(t, hasPerm, "base member should have PermSendMessages")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermJoinVoice)
	require.NoError(t, err)
	assert.True(t, hasPerm, "base member should have PermJoinVoice")

	// Base member should NOT have moderation permissions
	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermManageChannels)
	require.NoError(t, err)
	assert.False(t, hasPerm, "base member should not have PermManageChannels")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermKick)
	require.NoError(t, err)
	assert.False(t, hasPerm, "base member should not have PermKick")
}

func TestHasPermissionAdministratorBypass(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner2")
	admin := ts.CreateTestUser(t, "admin2")
	serverID := ts.CreateTestServer(t, owner.ID, "Admin Test Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")

	// Create an admin role with PermAdministrator and assign it
	adminRoleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'Admin', 10, $3)`,
		adminRoleID, serverID, int64(rbac.PermAdministrator),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, admin.ID, adminRoleID,
	)
	require.NoError(t, err)

	// Administrator should bypass all permission checks
	hasPerm, err := resolver.HasPermission(ctx, serverID, admin.ID, "", rbac.PermManageServer)
	require.NoError(t, err)
	assert.True(t, hasPerm, "administrator should bypass PermManageServer")

	hasPerm, err = resolver.HasPermission(ctx, serverID, admin.ID, "", rbac.PermBan)
	require.NoError(t, err)
	assert.True(t, hasPerm, "administrator should bypass PermBan")

	hasPerm, err = resolver.HasPermission(ctx, serverID, admin.ID, "", rbac.PermManageCryptoRotation)
	require.NoError(t, err)
	assert.True(t, hasPerm, "administrator should bypass PermManageCryptoRotation")
}

func TestHasPermissionOwnerPerms(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner3")
	serverID := ts.CreateTestServer(t, owner.ID, "Owner Test Server")

	// Owner should get OwnerPermissions (all non-admin permissions)
	hasPerm, err := resolver.HasPermission(ctx, serverID, owner.ID, "", rbac.PermManageServer)
	require.NoError(t, err)
	assert.True(t, hasPerm, "owner should have PermManageServer")

	hasPerm, err = resolver.HasPermission(ctx, serverID, owner.ID, "", rbac.PermManageCryptoRotation)
	require.NoError(t, err)
	assert.True(t, hasPerm, "owner should have PermManageCryptoRotation")

	hasPerm, err = resolver.HasPermission(ctx, serverID, owner.ID, "", rbac.PermBan)
	require.NoError(t, err)
	assert.True(t, hasPerm, "owner should have PermBan")

	// Owner does NOT get PermAdministrator (explicit design decision)
	perms, err := resolver.GetEffectivePermissions(ctx, serverID, owner.ID, "")
	require.NoError(t, err)
	assert.False(t, perms.Has(rbac.PermAdministrator), "owner should not have PermAdministrator bit set directly (bypasses via OwnerPermissions)")
}

func TestHasPermissionNonMember(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner4")
	outsider := ts.CreateTestUser(t, "outsider4")
	serverID := ts.CreateTestServer(t, owner.ID, "NonMember Test Server")

	// Non-member should return false, not an error
	hasPerm, err := resolver.HasPermission(ctx, serverID, outsider.ID, "", rbac.PermViewTextChannels)
	require.NoError(t, err, "non-member check should not return error")
	assert.False(t, hasPerm, "non-member should not have any permissions")
}

func TestHasPermissionMultipleRolesBITOR(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner5")
	member := ts.CreateTestUser(t, "member5")
	serverID := ts.CreateTestServer(t, owner.ID, "MultiRole Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Create two custom roles with different permissions
	modRoleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'Moderator', 5, $3)`,
		modRoleID, serverID, int64(rbac.PermKick|rbac.PermManageAllMessages),
	)
	require.NoError(t, err)

	inviterRoleID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'Inviter', 3, $3)`,
		inviterRoleID, serverID, int64(rbac.PermInvite|rbac.PermBan),
	)
	require.NoError(t, err)

	// Assign both roles to the member
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, member.ID, modRoleID,
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, member.ID, inviterRoleID,
	)
	require.NoError(t, err)

	// Member should have union of all role permissions (BIT_OR)
	hasPerm, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermKick)
	require.NoError(t, err)
	assert.True(t, hasPerm, "should have PermKick from Moderator role")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermManageAllMessages)
	require.NoError(t, err)
	assert.True(t, hasPerm, "should have PermManageMessages from Moderator role")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermInvite)
	require.NoError(t, err)
	assert.True(t, hasPerm, "should have PermInvite from Inviter role")

	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermBan)
	require.NoError(t, err)
	assert.True(t, hasPerm, "should have PermBan from Inviter role")

	// Should still have base permissions from @all
	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermViewTextChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm, "should still have PermViewTextChannels from @all")
}

func TestHasPermissionChannelDenyOverride(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner6")
	member := ts.CreateTestUser(t, "member6")
	serverID := ts.CreateTestServer(t, owner.ID, "SBAC Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "announcements")

	// Verify member has PermSendMessages at server level
	hasPerm, err := resolver.HasPermission(ctx, serverID, member.ID, "", rbac.PermSendMessages)
	require.NoError(t, err)
	assert.True(t, hasPerm, "member should have PermSendMessages at server level")

	// Get the @all role ID for this server
	var allRoleID string
	err = ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		serverID,
	).Scan(&allRoleID)
	require.NoError(t, err)

	// Create a channel override that denies PermSendMessages for @all role
	overrideID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		 VALUES ($1, $2, 'role', $3, 0, $4)`,
		overrideID, channelID, allRoleID, int64(rbac.PermSendMessages),
	)
	require.NoError(t, err)

	// Now the member should NOT have PermSendMessages in this channel (SBAC deny overrides RBAC allow)
	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.False(t, hasPerm, "channel deny override should revoke PermSendMessages")

	// Other permissions should still work in this channel
	hasPerm, err = resolver.HasPermission(ctx, serverID, member.ID, channelID, rbac.PermViewTextChannels)
	require.NoError(t, err)
	assert.True(t, hasPerm, "PermViewTextChannels should still work (not denied)")
}

func TestOwnerImmuneToChannelDenyOverrides(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner8")
	serverID := ts.CreateTestServer(t, owner.ID, "Owner SBAC Immunity Test")
	channelID := ts.CreateTestChannel(t, serverID, "restricted")

	// Get the @all role ID
	var allRoleID string
	err := ts.DB.QueryRow(
		`SELECT id FROM roles WHERE server_id = $1 AND is_default = TRUE`,
		serverID,
	).Scan(&allRoleID)
	require.NoError(t, err)

	// Create a channel override that denies PermSendMessages for @all role
	overrideID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		 VALUES ($1, $2, 'role', $3, 0, $4)`,
		overrideID, channelID, allRoleID, int64(rbac.PermSendMessages),
	)
	require.NoError(t, err)

	// Also create a user-specific deny override targeting the owner directly
	userOverrideID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO channel_permission_overrides (id, channel_id, target_type, target_id, allow, deny)
		 VALUES ($1, $2, 'user', $3, 0, $4)`,
		userOverrideID, channelID, owner.ID, int64(rbac.PermManageAllMessages),
	)
	require.NoError(t, err)

	// Owner should STILL have PermSendMessages despite role deny override
	hasPerm, err := resolver.HasPermission(ctx, serverID, owner.ID, channelID, rbac.PermSendMessages)
	require.NoError(t, err)
	assert.True(t, hasPerm, "owner should be immune to channel role deny override")

	// Owner should STILL have PermManageMessages despite user-specific deny override
	hasPerm, err = resolver.HasPermission(ctx, serverID, owner.ID, channelID, rbac.PermManageAllMessages)
	require.NoError(t, err)
	assert.True(t, hasPerm, "owner should be immune to channel user deny override")

	// Verify effective permissions are unchanged (OwnerPermissions, bypasses SBAC entirely)
	perms, err := resolver.GetEffectivePermissions(ctx, serverID, owner.ID, channelID)
	require.NoError(t, err)
	assert.Equal(t, rbac.OwnerPermissions, perms, "owner effective permissions should equal OwnerPermissions regardless of channel overrides")
}

func TestCheckHierarchy(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "owner7")
	admin := ts.CreateTestUser(t, "admin7")
	member := ts.CreateTestUser(t, "member7")
	serverID := ts.CreateTestServer(t, owner.ID, "Hierarchy Test Server")
	ts.AddMemberToServer(t, serverID, admin.ID, "member")
	ts.AddMemberToServer(t, serverID, member.ID, "member")

	// Create a high-position role and assign to admin
	adminRoleID := uuid.New().String()
	_, err := ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'Admin', 10, $3)`,
		adminRoleID, serverID, int64(rbac.AdminPermissions),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, admin.ID, adminRoleID,
	)
	require.NoError(t, err)

	// Admin (position 10) should outrank member (position 0, @all only)
	err = resolver.CheckHierarchy(ctx, serverID, admin.ID, member.ID)
	assert.NoError(t, err, "admin should outrank base member")

	// Member should NOT outrank admin
	err = resolver.CheckHierarchy(ctx, serverID, member.ID, admin.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "member should not outrank admin")

	// Equal position should fail (member vs member, both at position 0)
	member2 := ts.CreateTestUser(t, "member7b")
	ts.AddMemberToServer(t, serverID, member2.ID, "member")
	err = resolver.CheckHierarchy(ctx, serverID, member.ID, member2.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "equal position should fail hierarchy check")

	// Owner should outrank everyone, regardless of role position
	err = resolver.CheckHierarchy(ctx, serverID, owner.ID, admin.ID)
	assert.NoError(t, err, "owner should outrank admin (owner bypass)")

	err = resolver.CheckHierarchy(ctx, serverID, owner.ID, member.ID)
	assert.NoError(t, err, "owner should outrank base member (owner bypass)")

	// Administrator should outrank non-admin members
	err = resolver.CheckHierarchy(ctx, serverID, admin.ID, member.ID)
	assert.NoError(t, err, "administrator should outrank base member")

	// Administrator should NOT outrank the server owner
	err = resolver.CheckHierarchy(ctx, serverID, admin.ID, owner.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "administrator should not outrank server owner")

	// Member should NOT outrank owner
	err = resolver.CheckHierarchy(ctx, serverID, member.ID, owner.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "member should not outrank owner")

	// High-position non-admin member should NOT outrank owner
	// (owner may only have @all at position 0, but is still immune)
	highPosMember := ts.CreateTestUser(t, "highpos7")
	ts.AddMemberToServer(t, serverID, highPosMember.ID, "member")
	highPosRoleID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'HighPos', 50, $3)`,
		highPosRoleID, serverID, int64(rbac.PermKick),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, highPosMember.ID, highPosRoleID,
	)
	require.NoError(t, err)
	err = resolver.CheckHierarchy(ctx, serverID, highPosMember.ID, owner.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "high-position non-admin should not outrank owner")

	// User with explicit PermAdministrator should outrank non-admin members
	adminUser := ts.CreateTestUser(t, "permadmin7")
	ts.AddMemberToServer(t, serverID, adminUser.ID, "member")
	permAdminRoleID := uuid.New().String()
	_, err = ts.DB.Exec(
		`INSERT INTO roles (id, server_id, name, position, permissions) VALUES ($1, $2, 'PermAdmin', 5, $3)`,
		permAdminRoleID, serverID, int64(rbac.PermAdministrator),
	)
	require.NoError(t, err)
	_, err = ts.DB.Exec(
		`INSERT INTO member_roles (server_id, user_id, role_id) VALUES ($1, $2, $3)`,
		serverID, adminUser.ID, permAdminRoleID,
	)
	require.NoError(t, err)

	// PermAdministrator bypasses position: can moderate base member
	err = resolver.CheckHierarchy(ctx, serverID, adminUser.ID, member.ID)
	assert.NoError(t, err, "user with PermAdministrator should outrank base member")

	// PermAdministrator cannot moderate server owner
	err = resolver.CheckHierarchy(ctx, serverID, adminUser.ID, owner.ID)
	assert.ErrorIs(t, err, rbac.ErrHierarchyViolation, "PermAdministrator should not outrank server owner")
}

// TestResolver_InvalidateChannel_DelegatesToCache verifies the public
// Resolver.InvalidateChannel passthrough (#487) clears the channel-scoped
// permission cache entries that GetEffectivePermissions populates. The voice
// package relies on this after a temporary-SBAC grant/revoke.
func TestResolver_InvalidateChannel_DelegatesToCache(t *testing.T) {
	resolver, ts := setupResolver(t)
	ctx := context.Background()

	owner := ts.CreateTestUser(t, "ownerInv")
	member := ts.CreateTestUser(t, "memberInv")
	serverID := ts.CreateTestServer(t, owner.ID, "Invalidate Test Server")
	ts.AddMemberToServer(t, serverID, member.ID, "member")
	channelID := ts.CreateTestChannel(t, serverID, "voice-room")

	// Populate the channel-scoped cache entry for this user.
	_, err := resolver.GetEffectivePermissions(ctx, serverID, member.ID, channelID)
	require.NoError(t, err)

	// The cache key is "perm:{serverID}:{userID}:{channelID}" — assert it exists.
	cacheKey := "perm:" + serverID + ":" + member.ID + ":" + channelID
	existsBefore, err := ts.Redis.Exists(ctx, cacheKey).Result()
	require.NoError(t, err)
	require.Equal(t, int64(1), existsBefore, "channel-scoped permission should be cached after GetEffectivePermissions")

	// Invalidate via the public passthrough.
	require.NoError(t, resolver.InvalidateChannel(ctx, serverID, channelID))

	// The channel-scoped key for this (server, *, channel) must be cleared.
	existsAfter, err := ts.Redis.Exists(ctx, cacheKey).Result()
	require.NoError(t, err)
	assert.Equal(t, int64(0), existsAfter, "InvalidateChannel should clear the channel-scoped cache entry")
}
