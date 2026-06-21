package rbac

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPermissionHasBasic(t *testing.T) {
	perms := PermViewTextChannels | PermSendMessages

	assert.True(t, perms.Has(PermViewTextChannels))
	assert.True(t, perms.Has(PermSendMessages))
	assert.False(t, perms.Has(PermManageChannels))
	assert.False(t, perms.Has(PermKick))
}

func TestPermissionHasAdministratorBypass(t *testing.T) {
	perms := PermAdministrator // Only admin bit set

	// Administrator should bypass all permission checks
	assert.True(t, perms.Has(PermViewTextChannels))
	assert.True(t, perms.Has(PermViewVoiceChannels))
	assert.True(t, perms.Has(PermSendMessages))
	assert.True(t, perms.Has(PermManageServer))
	assert.True(t, perms.Has(PermBan))
	assert.True(t, perms.Has(PermManageCryptoRotation))
}

func TestPermissionAdd(t *testing.T) {
	perms := PermViewTextChannels
	perms = perms.Add(PermSendMessages)

	assert.True(t, perms.Has(PermViewTextChannels))
	assert.True(t, perms.Has(PermSendMessages))
	assert.False(t, perms.Has(PermManageChannels))
}

func TestPermissionRemove(t *testing.T) {
	perms := PermViewTextChannels | PermSendMessages | PermManageChannels
	perms = perms.Remove(PermManageChannels)

	assert.True(t, perms.Has(PermViewTextChannels))
	assert.True(t, perms.Has(PermSendMessages))
	assert.False(t, perms.Has(PermManageChannels))
}

func TestPermissionAddIdempotent(t *testing.T) {
	perms := PermViewTextChannels
	perms = perms.Add(PermViewTextChannels) // Adding same perm again

	assert.Equal(t, PermViewTextChannels, perms)
}

func TestBasePermissionsContainsExpected(t *testing.T) {
	// BasePermissions should include all default member permissions
	assert.True(t, BasePermissions.Has(PermViewTextChannels))
	assert.True(t, BasePermissions.Has(PermViewVoiceChannels))
	assert.True(t, BasePermissions.Has(PermSendMessages))
	assert.True(t, BasePermissions.Has(PermReadMessageHistory))
	assert.True(t, BasePermissions.Has(PermManageOwnMessages))
	assert.True(t, BasePermissions.Has(PermPinMessages))
	assert.True(t, BasePermissions.Has(PermJoinVoice))
	assert.True(t, BasePermissions.Has(PermSpeak))
	assert.True(t, BasePermissions.Has(PermScreenShare))
	assert.True(t, BasePermissions.Has(PermVideo))
	assert.True(t, BasePermissions.Has(PermAttachFiles))
	assert.True(t, BasePermissions.Has(PermUseExternalEmoji))
	assert.True(t, BasePermissions.Has(PermMentionRoles))
	assert.True(t, BasePermissions.Has(PermMentionUsers))

	// BasePermissions should NOT include moderation/admin permissions
	assert.False(t, BasePermissions.Has(PermManageChannels))
	assert.False(t, BasePermissions.Has(PermManageRoles))
	assert.False(t, BasePermissions.Has(PermKick))
	assert.False(t, BasePermissions.Has(PermBan))
	assert.False(t, BasePermissions.Has(PermManageServer))
	assert.False(t, BasePermissions.Has(PermMentionEveryone))
	assert.False(t, BasePermissions.Has(PermManageDevResources))
}

func TestModeratorPermissionsExtendsBase(t *testing.T) {
	// Moderator should have all base permissions
	assert.True(t, ModeratorPermissions.Has(PermViewTextChannels))
	assert.True(t, ModeratorPermissions.Has(PermViewVoiceChannels))
	assert.True(t, ModeratorPermissions.Has(PermSendMessages))

	// Plus moderation permissions
	assert.True(t, ModeratorPermissions.Has(PermManageAllMessages))
	assert.True(t, ModeratorPermissions.Has(PermKick))
	assert.True(t, ModeratorPermissions.Has(PermMuteMembers))
}

func TestAdminPermissionsExtendsModerator(t *testing.T) {
	assert.True(t, AdminPermissions.Has(PermManageChannels))
	assert.True(t, AdminPermissions.Has(PermManageRoles))
	assert.True(t, AdminPermissions.Has(PermBan))
	assert.True(t, AdminPermissions.Has(PermViewAuditLog))
	assert.True(t, AdminPermissions.Has(PermManageDevResources))
}

func TestOwnerPermissionsExtendsAdmin(t *testing.T) {
	assert.True(t, OwnerPermissions.Has(PermManageServer))
	assert.True(t, OwnerPermissions.Has(PermManageCryptoRotation))

	// Owner does NOT get PermAdministrator (explicit design decision)
	// Check the bit directly rather than via Has() which has admin-bypass logic
	assert.Equal(t, Permission(0), OwnerPermissions&PermAdministrator, "OwnerPermissions must not contain PermAdministrator bit")
}

func TestPermissionNamesAllMapped(t *testing.T) {
	// Every defined permission constant should have a name entry
	allPerms := []Permission{
		PermManageServer, PermManageRoles, PermManageRolesAssign, PermManageChannels,
		PermManageCryptoRotation, PermViewAuditLog, PermInvite, PermKick, PermBan,
		PermViewTextChannels, PermViewVoiceChannels, PermSendMessages, PermReadMessageHistory, PermManageOwnMessages,
		PermManageAllMessages, PermPinMessages, PermJoinVoice, PermSpeak, PermMuteMembers, PermDeafenMembers,
		PermMoveMembers, PermScreenShare, PermAttachFiles, PermUseExternalEmoji,
		PermMentionEveryone, PermMentionRoles, PermMentionUsers, PermManageDevResources, PermVideo, PermAdministrator,
	}

	for _, perm := range allPerms {
		name, exists := PermissionNames[perm]
		assert.True(t, exists, "Permission %d should have a name mapping", perm)
		assert.NotEmpty(t, name, "Permission %d name should not be empty", perm)
	}
}

func TestPermissionNoBitOverlap(t *testing.T) {
	// Verify no two permissions share the same bit
	allPerms := []Permission{
		PermManageServer, PermManageRoles, PermManageRolesAssign, PermManageChannels,
		PermManageCryptoRotation, PermViewAuditLog, PermInvite, PermKick, PermBan,
		PermViewTextChannels, PermViewVoiceChannels, PermSendMessages, PermReadMessageHistory, PermManageOwnMessages,
		PermManageAllMessages, PermPinMessages, PermJoinVoice, PermSpeak, PermMuteMembers, PermDeafenMembers,
		PermMoveMembers, PermScreenShare, PermAttachFiles, PermUseExternalEmoji,
		PermMentionEveryone, PermMentionRoles, PermMentionUsers, PermManageDevResources, PermVideo, PermAdministrator,
	}

	for i := 0; i < len(allPerms); i++ {
		for j := i + 1; j < len(allPerms); j++ {
			assert.Equal(t, Permission(0), allPerms[i]&allPerms[j],
				"Permissions %d and %d should not share bits", allPerms[i], allPerms[j])
		}
	}
}
