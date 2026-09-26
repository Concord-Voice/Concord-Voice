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

import (
	"context"
	"fmt"

	"github.com/Concord-Voice/Concord-Voice-Alpha/services/control-plane/internal/stepup"
)

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

	// Moderation permissions
	PermTimeoutMembers Permission = 1 << 29 // Temporarily bar members from sending messages and joining voice

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
	PermMuteMembers | PermDeafenMembers | PermMoveMembers | PermTimeoutMembers

// AdminPermissions extends moderator permissions with administrative capabilities
var AdminPermissions = ModeratorPermissions | PermManageChannels | PermManageRoles |
	PermManageRolesAssign | PermInvite | PermBan | PermViewAuditLog | PermManageDevResources

// OwnerPermissions grants all non-administrator permissions plus server management
// Owner does NOT get PermAdministrator by default (explicit security decision)
var OwnerPermissions = AdminPermissions | PermManageServer | PermManageCryptoRotation | PermMentionEveryone

// DangerousPermissions is the set an enforcing server withholds from a member
// with no inline MFA factor (#3453). Bits 0, 1, 3, 4, 7, 8, 14 and 25.
// ManageRolesAssign is deliberately absent (D6), as are the reversible voice
// moderation bits, Invite, ViewAuditLog and MentionEveryone (spec §4, Q6).
const DangerousPermissions = PermManageServer | PermManageRoles | PermManageChannels |
	PermManageCryptoRotation | PermKick | PermBan | PermManageAllMessages | PermManageDevResources

// ConcretePermissions is every NAMED permission except PermAdministrator: what
// bit 62 means once it is expanded into real bits. It is a const rather than a
// value derived from PermissionNames because that map is a mutable var, and a
// new Perm* forgotten here must fail a pin test, not shift silently at runtime.
const ConcretePermissions = PermManageServer | PermManageRoles | PermManageRolesAssign |
	PermManageChannels | PermManageCryptoRotation | PermViewAuditLog | PermInvite | PermKick |
	PermBan | PermViewVoiceChannels | PermViewTextChannels | PermSendMessages |
	PermReadMessageHistory | PermManageOwnMessages | PermManageAllMessages | PermPinMessages |
	PermJoinVoice | PermSpeak | PermMuteMembers | PermDeafenMembers | PermMoveMembers |
	PermScreenShare | PermAttachFiles | PermUseExternalEmoji | PermMentionEveryone |
	PermManageDevResources | PermMentionRoles | PermMentionUsers | PermVideo | PermTimeoutMembers

// MFAMask is the MFA-enforcement state of one member on one server (#3453).
// The zero value masks nothing.
//
// The mask applies to a RESULT, never to an input. Every resolver bypass — the
// owner short-circuit and the Administrator override bypass — is decided on the
// raw value, and Apply runs once, at each public entry point's single exit. An
// unenrolled Administrator therefore still ignores channel overrides (raw bit
// 62 decides that) and only THEN loses the dangerous bits. Masking before a
// bypass decision is the defect this ordering exists to prevent: with bit 62
// gone the member would fall into channel-override evaluation, where a DENY
// that bit 62 is supposed to ignore would start to apply.
type MFAMask struct {
	Enforcing bool
	Enrolled  bool
}

// Apply returns p as an enforcing server exposes it (spec §4, invariant I1).
// When the mask does not bind (not enforcing, or enrolled) p is returned
// unchanged. Otherwise bit 62 is EXPANDED into ConcretePermissions — an
// Administrator keeps every non-dangerous concrete bit, because Permission.Has
// stops short-circuiting once bit 62 is cleared — and the dangerous bits are
// removed. Undefined bits 30–61 pass through untouched; bit 63 is never set.
// Apply is idempotent.
func (m MFAMask) Apply(p Permission) Permission {
	if !m.Enforcing || m.Enrolled {
		return p
	}
	if p&PermAdministrator != 0 {
		p = (p &^ PermAdministrator) | ConcretePermissions
	}
	return p &^ DangerousPermissions
}

// MaskFor reads userID's MFA enrollment when, and only when, the server
// enforces. A non-enforcing server issues no statement, which is what keeps the
// resolver's statement count unchanged when enforcement is off (spec I2, C-1).
//
// Enrollment is policy P1, read through stepup.InlineMFAMethods so P1 has one
// definition. A read error is returned wrapped AND with the restrictive mask
// (enforcing, not enrolled) beside it, so a caller that logs the error and
// carries on still fails closed on the permission.
func MaskFor(ctx context.Context, q rowQuerier, userID string, enforcing bool) (MFAMask, error) {
	if !enforcing {
		return MFAMask{}, nil
	}
	methods, err := stepup.InlineMFAMethods(ctx, q, userID)
	if err != nil {
		return MFAMask{Enforcing: true}, fmt.Errorf("resolve MFA enrollment: %w", err)
	}
	return MFAMask{Enforcing: true, Enrolled: len(methods) > 0}, nil
}

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
	PermTimeoutMembers:       "timeout_members",
	PermAdministrator:        "administrator",
}
