import { describe, it, expect } from 'vitest';
import {
  resolveChannelPermissions,
  MENTION_EVERYONE,
  MENTION_USERS,
  MENTION_ROLES,
  ADMINISTRATOR,
  type PermissionOverride,
} from '@/renderer/utils/permissions';

const VIEWER = 'user-1';
const ROLE = 'role-mod';
const roleSet = new Set([ROLE]);

const roleOverride = (allow: bigint, deny: bigint): PermissionOverride => ({
  target_type: 'role',
  target_id: ROLE,
  allow: allow.toString(),
  deny: deny.toString(),
});
const userOverride = (allow: bigint, deny: bigint): PermissionOverride => ({
  target_type: 'user',
  target_id: VIEWER,
  allow: allow.toString(),
  deny: deny.toString(),
});

describe('resolveChannelPermissions', () => {
  it('returns base unchanged when there are no overrides', () => {
    const base = MENTION_USERS;
    expect(resolveChannelPermissions(base, undefined, VIEWER, roleSet)).toBe(base);
    expect(resolveChannelPermissions(base, [], VIEWER, roleSet)).toBe(base);
  });

  it('bypasses overrides for an administrator base', () => {
    const base = ADMINISTRATOR | MENTION_USERS;
    const overrides = [roleOverride(0n, MENTION_USERS)]; // deny ignored under admin bypass
    expect(resolveChannelPermissions(base, overrides, VIEWER, roleSet)).toBe(base);
  });

  it('bypasses overrides for a server owner (owner-id bypass — OwnerPermissions has no ADMINISTRATOR bit)', () => {
    // A real owner's base carries no ADMINISTRATOR bit (types.go: OwnerPermissions excludes it);
    // the backend exempts owners via owner-id, so the resolver must honor the explicit owner flag.
    const base = MENTION_EVERYONE | MENTION_USERS;
    const overrides = [roleOverride(0n, MENTION_EVERYONE)]; // would deny, but owner is immune
    expect(resolveChannelPermissions(base, overrides, VIEWER, roleSet, true)).toBe(base);
  });

  it('falls back to base for an unidentified viewer (empty userId + roles) even with overrides', () => {
    // Store-hydration race: no loaded viewer. '' matches no user override; empty role set
    // matches no role override → base unchanged (conservative degradation, spec §7).
    const base = MENTION_USERS;
    const overrides = [userOverride(0n, MENTION_USERS), roleOverride(0n, MENTION_USERS)];
    expect(resolveChannelPermissions(base, overrides, '', new Set())).toBe(base);
  });

  it('adds a role-allowed bit not present in base', () => {
    const eff = resolveChannelPermissions(
      MENTION_USERS,
      [roleOverride(MENTION_ROLES, 0n)],
      VIEWER,
      roleSet
    );
    expect(eff & MENTION_ROLES).toBe(MENTION_ROLES);
  });

  it('removes a base bit via role deny', () => {
    const eff = resolveChannelPermissions(
      MENTION_USERS | MENTION_EVERYONE,
      [roleOverride(0n, MENTION_EVERYONE)],
      VIEWER,
      roleSet
    );
    expect(eff & MENTION_EVERYONE).toBe(0n);
    expect(eff & MENTION_USERS).toBe(MENTION_USERS);
  });

  it('lets a user allow re-grant a role-denied bit (user allow beats role deny)', () => {
    const eff = resolveChannelPermissions(
      MENTION_EVERYONE,
      [roleOverride(0n, MENTION_EVERYONE), userOverride(MENTION_EVERYONE, 0n)],
      VIEWER,
      roleSet
    );
    expect(eff & MENTION_EVERYONE).toBe(MENTION_EVERYONE);
  });

  it('treats user deny as final authority', () => {
    const eff = resolveChannelPermissions(
      MENTION_USERS,
      [roleOverride(MENTION_USERS, 0n), userOverride(0n, MENTION_USERS)],
      VIEWER,
      roleSet
    );
    expect(eff & MENTION_USERS).toBe(0n);
  });

  it('OR-accumulates across multiple of the viewer roles', () => {
    const twoRoles = new Set(['role-a', 'role-b']);
    const overrides: PermissionOverride[] = [
      { target_type: 'role', target_id: 'role-a', allow: MENTION_USERS.toString(), deny: '0' },
      { target_type: 'role', target_id: 'role-b', allow: MENTION_ROLES.toString(), deny: '0' },
    ];
    const eff = resolveChannelPermissions(0n, overrides, VIEWER, twoRoles);
    expect(eff & MENTION_USERS).toBe(MENTION_USERS);
    expect(eff & MENTION_ROLES).toBe(MENTION_ROLES);
  });

  it('ignores overrides for roles the viewer does not have', () => {
    const overrides: PermissionOverride[] = [
      { target_type: 'role', target_id: 'role-other', allow: '0', deny: MENTION_USERS.toString() },
    ];
    const eff = resolveChannelPermissions(MENTION_USERS, overrides, VIEWER, roleSet);
    expect(eff & MENTION_USERS).toBe(MENTION_USERS);
  });
});
