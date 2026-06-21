/**
 * Permission bitfield constants and utilities.
 * Must stay in sync with services/control-plane/internal/rbac/types.go
 */

// Administrative permissions
export const MANAGE_SERVER = 1n << 0n;
export const MANAGE_ROLES = 1n << 1n;
export const MANAGE_ROLES_ASSIGN = 1n << 2n;
export const MANAGE_CHANNELS = 1n << 3n;
export const MANAGE_CRYPTO_ROTATION = 1n << 4n;
export const VIEW_AUDIT_LOG = 1n << 5n;

// Member management permissions
export const INVITE = 1n << 6n;
export const KICK = 1n << 7n;
export const BAN = 1n << 8n;

// Channel permissions (overridable per-channel via SBAC)
export const VIEW_TEXT_CHANNELS = 1n << 10n;
export const VIEW_VOICE_CHANNELS = 1n << 9n;
export const SEND_MESSAGES = 1n << 11n;
export const READ_MESSAGE_HISTORY = 1n << 12n;
export const MANAGE_OWN_MESSAGES = 1n << 13n;
export const MANAGE_ALL_MESSAGES = 1n << 14n;
export const PIN_MESSAGES = 1n << 15n;

// Voice permissions
export const JOIN_VOICE = 1n << 16n;
export const SPEAK = 1n << 17n;
export const MUTE_MEMBERS = 1n << 18n;
export const DEAFEN_MEMBERS = 1n << 19n;
export const MOVE_MEMBERS = 1n << 20n;
export const SCREEN_SHARE = 1n << 21n;
export const VIDEO = 1n << 28n;

// Content permissions
export const ATTACH_FILES = 1n << 22n;
export const USE_EXTERNAL_EMOJI = 1n << 23n;

// Mention permissions
export const MENTION_EVERYONE = 1n << 24n;
export const MENTION_ROLES = 1n << 26n;
export const MENTION_USERS = 1n << 27n;

// Integration permissions
export const MANAGE_DEV_RESOURCES = 1n << 25n;

// Administrator (superuser) - grants all permissions
export const ADMINISTRATOR = 1n << 62n;

// Permission sets (match backend presets)
export const BASE_PERMISSIONS =
  VIEW_TEXT_CHANNELS |
  VIEW_VOICE_CHANNELS |
  SEND_MESSAGES |
  READ_MESSAGE_HISTORY |
  MANAGE_OWN_MESSAGES |
  PIN_MESSAGES |
  JOIN_VOICE |
  SPEAK |
  SCREEN_SHARE |
  ATTACH_FILES |
  USE_EXTERNAL_EMOJI |
  MENTION_ROLES |
  MENTION_USERS |
  VIDEO;

export const MODERATOR_PERMISSIONS =
  BASE_PERMISSIONS | MANAGE_ALL_MESSAGES | KICK | MUTE_MEMBERS | DEAFEN_MEMBERS | MOVE_MEMBERS;

export const ADMIN_PERMISSIONS =
  MODERATOR_PERMISSIONS |
  MANAGE_CHANNELS |
  MANAGE_ROLES |
  MANAGE_ROLES_ASSIGN |
  INVITE |
  BAN |
  VIEW_AUDIT_LOG |
  MANAGE_DEV_RESOURCES;

// Convenience object for component imports: Permissions.MANAGE_SERVER etc.
export const Permissions = {
  MANAGE_SERVER,
  MANAGE_ROLES,
  MANAGE_ROLES_ASSIGN,
  MANAGE_CHANNELS,
  MANAGE_CRYPTO_ROTATION,
  VIEW_AUDIT_LOG,
  INVITE,
  KICK,
  BAN,
  VIEW_TEXT_CHANNELS,
  VIEW_VOICE_CHANNELS,
  SEND_MESSAGES,
  READ_MESSAGE_HISTORY,
  MANAGE_OWN_MESSAGES,
  MANAGE_ALL_MESSAGES,
  PIN_MESSAGES,
  JOIN_VOICE,
  SPEAK,
  MUTE_MEMBERS,
  DEAFEN_MEMBERS,
  MOVE_MEMBERS,
  SCREEN_SHARE,
  ATTACH_FILES,
  USE_EXTERNAL_EMOJI,
  MENTION_EVERYONE,
  MENTION_ROLES,
  MENTION_USERS,
  MANAGE_DEV_RESOURCES,
  VIDEO,
  ADMINISTRATOR,
} as const;

/** Count the number of set bits in a permission bitfield */
export function countBits(n: number | string | bigint): number {
  let v = typeof n === 'bigint' ? n : BigInt(n || 0);
  let count = 0;
  while (v > 0n) {
    if (v & 1n) count++;
    v >>= 1n;
  }
  return count;
}

/** Check if a bitfield has a specific permission */
export function hasPermission(bitfield: bigint, perm: bigint): boolean {
  // Administrator bypasses all permission checks
  if ((bitfield & ADMINISTRATOR) !== 0n) return true;
  return (bitfield & perm) !== 0n;
}

/** Add a permission to a bitfield */
export function addPermission(bitfield: bigint, perm: bigint): bigint {
  return bitfield | perm;
}

/** Remove a permission from a bitfield */
export function removePermission(bitfield: bigint, perm: bigint): bigint {
  return bitfield & ~perm;
}

/** Parse a permissions string (from API) into a BigInt */
export function parsePermissions(permsStr: string | number | undefined): bigint {
  if (permsStr === undefined || permsStr === null) return 0n;
  try {
    return BigInt(permsStr);
  } catch {
    return 0n;
  }
}

/**
 * Minimal structural shape of a channel permission override. Declared locally so
 * permissions.ts stays free of any store import (avoids a util→store cycle).
 * `ChannelOverride` from permissionStore is structurally assignable to this.
 */
export interface PermissionOverride {
  target_type: 'user' | 'role';
  target_id: string;
  allow: number | string;
  deny: number | string;
}

/**
 * Compute the viewer's channel-effective permission bitmask by folding SBAC channel
 * overrides into a server-level base permission. Mirrors the control-plane resolver
 * (services/control-plane/internal/rbac/resolver.go `applyChannelOverrides`) exactly:
 *
 *   effective = ((base | role_allow) & ~role_deny | user_allow) & ~user_deny
 *
 * Owner/Administrator bypass — both are immune to channel overrides, but for DIFFERENT
 * reasons in the backend, so the frontend must mirror both:
 *   - OWNER: bypassed via an owner-id match in computeEffectivePermissions (resolver.go
 *     step 2) — returns OwnerPermissions and skips applyChannelOverrides entirely.
 *     OwnerPermissions does NOT carry PermAdministrator (types.go: "Owner does NOT get
 *     PermAdministrator by default"), so the ADMINISTRATOR-bit check below would MISS an
 *     owner. The caller passes `viewerIsOwner` (from the viewer's `ServerMember.role`).
 *   - ADMINISTRATOR: bypassed inside applyChannelOverrides when base carries
 *     PermAdministrator (resolver.go:162-163) — mirrored by the `& ADMINISTRATOR` check.
 * Role masks are OR-accumulated across ONLY the viewer's roles; the user override is final.
 *
 * @param basePerm      Server-level effective permission for the viewer (no channel overrides).
 * @param overrides     Channel overrides for the target channel, or undefined if not loaded.
 * @param viewerUserId  The viewer's user id (selects the user-target override).
 * @param viewerRoleIds The viewer's role ids in this server (selects role-target overrides).
 * @param viewerIsOwner True if the viewer is the server owner (owner-id bypass mirror).
 */
export function resolveChannelPermissions(
  basePerm: bigint,
  overrides: readonly PermissionOverride[] | undefined,
  viewerUserId: string,
  viewerRoleIds: ReadonlySet<string>,
  viewerIsOwner = false
): bigint {
  // Owner & Administrator are immune to channel overrides (see the two backend paths above).
  if (viewerIsOwner || (basePerm & ADMINISTRATOR) !== 0n) return basePerm;
  if (!overrides || overrides.length === 0) return basePerm;

  let roleAllow = 0n;
  let roleDeny = 0n;
  let userAllow = 0n;
  let userDeny = 0n;

  for (const o of overrides) {
    if (o.target_type === 'role' && viewerRoleIds.has(o.target_id)) {
      roleAllow |= parsePermissions(o.allow);
      roleDeny |= parsePermissions(o.deny);
    } else if (o.target_type === 'user' && o.target_id === viewerUserId) {
      userAllow |= parsePermissions(o.allow);
      userDeny |= parsePermissions(o.deny);
    }
  }

  // base → role allow → role deny → user allow → user deny (user deny = final authority).
  return (((basePerm | roleAllow) & ~roleDeny) | userAllow) & ~userDeny;
}

/**
 * Permission metadata for the UI toggle grid.
 * Grouped by category for display.
 */
export interface PermissionInfo {
  key: string;
  bit: bigint;
  label: string;
  description: string;
}

export interface PermissionCategory {
  name: string;
  permissions: PermissionInfo[];
}

export const PERMISSION_CATEGORIES: PermissionCategory[] = [
  {
    name: 'General',
    permissions: [
      {
        key: 'VIEW_TEXT_CHANNELS',
        bit: VIEW_TEXT_CHANNELS,
        label: 'View Text Channels',
        description: 'See text and bulletin channels in the channel list',
      },
      {
        key: 'VIEW_VOICE_CHANNELS',
        bit: VIEW_VOICE_CHANNELS,
        label: 'View Voice Channels',
        description: 'See voice channels in the channel list',
      },
      {
        key: 'MANAGE_CHANNELS',
        bit: MANAGE_CHANNELS,
        label: 'Manage Channels',
        description: 'Create, edit, and delete channels',
      },
      {
        key: 'MANAGE_SERVER',
        bit: MANAGE_SERVER,
        label: 'Manage Server',
        description: 'Edit server settings, icon, and banner',
      },
      {
        key: 'VIEW_AUDIT_LOG',
        bit: VIEW_AUDIT_LOG,
        label: 'View Audit Log',
        description: 'View the server audit log',
      },
    ],
  },
  {
    name: 'Membership',
    permissions: [
      {
        key: 'INVITE',
        bit: INVITE,
        label: 'Create Invite',
        description: 'Create server invite links',
      },
      {
        key: 'KICK',
        bit: KICK,
        label: 'Kick Members',
        description: 'Remove members from the server',
      },
      {
        key: 'BAN',
        bit: BAN,
        label: 'Ban Members',
        description: 'Permanently ban members from the server',
      },
      {
        key: 'MENTION_EVERYONE',
        bit: MENTION_EVERYONE,
        label: 'Mention All and Here',
        description: 'Use @all and @here mentions',
      },
      {
        key: 'MENTION_ROLES',
        bit: MENTION_ROLES,
        label: 'Mention Roles',
        description: 'Mention roles that are set as mentionable',
      },
      {
        key: 'MENTION_USERS',
        bit: MENTION_USERS,
        label: 'Mention Users',
        description: 'Mention individual users with @username',
      },
    ],
  },
  {
    name: 'Text Channels',
    permissions: [
      {
        key: 'SEND_MESSAGES',
        bit: SEND_MESSAGES,
        label: 'Send Messages',
        description: 'Send text messages in channels',
      },
      {
        key: 'READ_MESSAGE_HISTORY',
        bit: READ_MESSAGE_HISTORY,
        label: 'Read Message History',
        description: 'Read past messages in channels',
      },
      {
        key: 'MANAGE_OWN_MESSAGES',
        bit: MANAGE_OWN_MESSAGES,
        label: 'Manage Own Messages',
        description: 'Edit and delete your own messages',
      },
      {
        key: 'MANAGE_ALL_MESSAGES',
        bit: MANAGE_ALL_MESSAGES,
        label: 'Manage All Messages',
        description: 'Delete any message and suppress embeds (moderation)',
      },
      {
        key: 'PIN_MESSAGES',
        bit: PIN_MESSAGES,
        label: 'Pin Messages',
        description: 'Pin or unpin messages in channels',
      },
      {
        key: 'ATTACH_FILES',
        bit: ATTACH_FILES,
        label: 'Attach Files',
        description: 'Upload files and images in channels',
      },
      {
        key: 'USE_EXTERNAL_EMOJI',
        bit: USE_EXTERNAL_EMOJI,
        label: 'Use Other Server Emojis',
        description: 'Use custom emoji from other servers',
      },
    ],
  },
  {
    name: 'Voice Channels',
    permissions: [
      {
        key: 'JOIN_VOICE',
        bit: JOIN_VOICE,
        label: 'Join Voice',
        description: 'Connect to voice channels',
      },
      { key: 'SPEAK', bit: SPEAK, label: 'Speak', description: 'Transmit audio in voice channels' },
      {
        key: 'SCREEN_SHARE',
        bit: SCREEN_SHARE,
        label: 'Screen Share',
        description: 'Share your screen in voice channels',
      },
      {
        key: 'VIDEO',
        bit: VIDEO,
        label: 'Video',
        description: 'Enable camera/video in voice channels',
      },
      {
        key: 'MUTE_MEMBERS',
        bit: MUTE_MEMBERS,
        label: 'Mute Members',
        description: 'Server-mute other members',
      },
      {
        key: 'DEAFEN_MEMBERS',
        bit: DEAFEN_MEMBERS,
        label: 'Deafen Members',
        description: 'Server-deafen other members',
      },
      {
        key: 'MOVE_MEMBERS',
        bit: MOVE_MEMBERS,
        label: 'Move Members',
        description: 'Move members between voice channels',
      },
    ],
  },
  {
    name: 'Roles',
    permissions: [
      {
        key: 'MANAGE_ROLES',
        bit: MANAGE_ROLES,
        label: 'Manage Roles',
        description: 'Create, edit, and delete roles',
      },
      {
        key: 'MANAGE_ROLES_ASSIGN',
        bit: MANAGE_ROLES_ASSIGN,
        label: 'Assign Roles',
        description: 'Assign and unassign roles to members',
      },
    ],
  },
  {
    name: 'Advanced',
    permissions: [
      {
        key: 'MANAGE_CRYPTO_ROTATION',
        bit: MANAGE_CRYPTO_ROTATION,
        label: 'Manage E2EE Keys',
        description: 'Manually rotate encryption keys',
      },
      {
        key: 'MANAGE_DEV_RESOURCES',
        bit: MANAGE_DEV_RESOURCES,
        label: 'Manage Developer Resources',
        description: 'Manage webhooks, API keys, and bot access for this server',
      },
      {
        key: 'ADMINISTRATOR',
        bit: ADMINISTRATOR,
        label: 'Administrator',
        description: 'Grants all permissions and bypasses channel overrides',
      },
    ],
  },
];
