// Package rbac implements Role-Based Access Control (RBAC) and Server-Based Access Control (SBAC)
// for the Concord control plane. It provides a two-layer permission model:
//
// 1. RBAC (Server-wide roles): Users are assigned roles that grant base permissions across the server
// 2. SBAC (Channel-specific overrides): Channels can override role permissions for specific users/roles
//
// Permission Resolution Order:
// - Check if user has server membership (required for all operations)
// - Compute base permissions from all user's roles (OR'd together)
// - Apply channel-specific overrides (deny takes precedence over allow)
// - Cache result in Redis for 5 minutes
package rbac

// Permission represents a single permission bit in the bitfield
type Permission int64

// Server-level permissions (apply server-wide via roles)
const (
	// Administrative permissions
	PermManageServer         Permission = 1 << 0 // Edit server settings, icon, banner
	PermManageRoles          Permission = 1 << 1 // Create, edit, delete roles
	PermManageRolesAssign    Permission = 1 << 2 // Assign/unassign roles to members
	PermManageChannels       Permission = 1 << 3 // Create, edit, delete channels
	PermManageCryptoRotation Permission = 1 << 4 // Manually rotate E2EE keys (CSK)
	PermViewAuditLog         Permission = 1 << 5 // View permission audit log

	// Member management permissions
	PermInvite Permission = 1 << 6 // Create server invites
	PermKick   Permission = 1 << 7 // Remove members (temporary)
	PermBan    Permission = 1 << 8 // Ban members (prevents rejoin)

	// Channel permissions (can be overridden per-channel via SBAC)
	PermViewTextChannels   Permission = 1 << 10 // See text and bulletin channels in the channel list
	PermViewVoiceChannels  Permission = 1 << 9  // See voice channels in the channel list
	PermSendMessages       Permission = 1 << 11 // Send text messages in channels
	PermReadMessageHistory Permission = 1 << 12 // Read past messages (vs. only seeing new ones)
	PermManageOwnMessages  Permission = 1 << 13 // Edit and delete own messages
	PermManageAllMessages  Permission = 1 << 14 // Delete any message and suppress embeds (moderation)
	PermPinMessages        Permission = 1 << 15 // Pin or unpin messages in channels

	// Voice permissions
	PermJoinVoice     Permission = 1 << 16 // Join voice channels
	PermSpeak         Permission = 1 << 17 // Transmit audio in voice channels
	PermMuteMembers   Permission = 1 << 18 // Server-mute other members
	PermDeafenMembers Permission = 1 << 19 // Server-deafen other members
	PermMoveMembers   Permission = 1 << 20 // Move members between voice channels
	PermScreenShare   Permission = 1 << 21 // Share screen in voice channels
	PermVideo         Permission = 1 << 28 // Enable camera/video in voice channels

	// Content permissions
	PermAttachFiles      Permission = 1 << 22 // Upload files and images in channels
	PermUseExternalEmoji Permission = 1 << 23 // Use emoji from other servers

	// Mention permissions
	PermMentionEveryone Permission = 1 << 24 // Use @all and @here mentions
	PermMentionRoles    Permission = 1 << 26 // Mention roles that are set as mentionable
	PermMentionUsers    Permission = 1 << 27 // Mention individual users with @username

	// Integration permissions
	PermManageDevResources Permission = 1 << 25 // Manage webhooks, API keys, and bot access

	// Administrator (superuser) - grants all permissions
	// Use bit 62 instead of 63 to avoid int64 overflow (bit 63 is sign bit)
	PermAdministrator Permission = 1 << 62
)

// BasePermissions defines the minimal permissions for the @all default role
// These are granted to all server members by default
var BasePermissions = PermViewTextChannels | PermViewVoiceChannels | PermSendMessages | PermReadMessageHistory |
	PermManageOwnMessages | PermPinMessages | PermJoinVoice | PermSpeak | PermScreenShare |
	PermAttachFiles | PermUseExternalEmoji | PermMentionRoles | PermMentionUsers | PermVideo

// ModeratorPermissions extends base permissions with moderation capabilities
var ModeratorPermissions = BasePermissions | PermManageAllMessages | PermKick |
	PermMuteMembers | PermDeafenMembers | PermMoveMembers

// AdminPermissions extends moderator permissions with administrative capabilities
var AdminPermissions = ModeratorPermissions | PermManageChannels | PermManageRoles |
	PermManageRolesAssign | PermInvite | PermBan | PermViewAuditLog | PermManageDevResources

// OwnerPermissions grants all non-administrator permissions plus server management
// Owner does NOT get PermAdministrator by default (explicit security decision)
var OwnerPermissions = AdminPermissions | PermManageServer | PermManageCryptoRotation

// Has checks if a permission bitfield contains a specific permission
func (p Permission) Has(perm Permission) bool {
	// Administrator bypasses all permission checks
	if p&PermAdministrator != 0 {
		return true
	}
	return p&perm != 0
}

// Add adds a permission to the bitfield
func (p Permission) Add(perm Permission) Permission {
	return p | perm
}

// Remove removes a permission from the bitfield
func (p Permission) Remove(perm Permission) Permission {
	return p &^ perm
}

// PermissionNames maps permission constants to human-readable names (for audit log)
var PermissionNames = map[Permission]string{
	PermManageServer:         "manage_server",
	PermManageRoles:          "manage_roles",
	PermManageRolesAssign:    "manage_roles_assign",
	PermManageChannels:       "manage_channels",
	PermManageCryptoRotation: "manage_crypto_rotation",
	PermViewAuditLog:         "view_audit_log",
	PermInvite:               "create_invite",
	PermKick:                 "kick_members",
	PermBan:                  "ban_members",
	PermViewTextChannels:     "view_text_channels",
	PermViewVoiceChannels:    "view_voice_channels",
	PermSendMessages:         "send_messages",
	PermReadMessageHistory:   "read_message_history",
	PermManageOwnMessages:    "manage_own_messages",
	PermManageAllMessages:    "manage_all_messages",
	PermPinMessages:          "pin_messages",
	PermJoinVoice:            "join_voice",
	PermSpeak:                "speak",
	PermMuteMembers:          "mute_members",
	PermDeafenMembers:        "deafen_members",
	PermMoveMembers:          "move_members",
	PermScreenShare:          "screen_share",
	PermAttachFiles:          "attach_files",
	PermUseExternalEmoji:     "use_external_emoji",
	PermMentionEveryone:      "mention_everyone",
	PermMentionRoles:         "mention_roles",
	PermMentionUsers:         "mention_users",
	PermManageDevResources:   "manage_dev_resources",
	PermVideo:                "video",
	PermAdministrator:        "administrator",
}
