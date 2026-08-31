/**
 * Permission Store — manages RBAC roles, effective permissions, and channel overrides.
 * Integrates with the backend RBAC/SBAC system.
 */

import { createStore } from '../../utils/runtime/createStore';
import { apiFetch } from '../../services/system/apiClient';
import {
  Role,
  type ReorderOutcome,
  type RoleReorderPayload,
  type RoleViewer,
} from '../../types/server';
import { hasPermission, parsePermissions } from '../../utils/policy/permissions';

export interface ChannelOverride {
  id: string;
  channel_id: string;
  target_type: 'user' | 'role';
  target_id: string;
  allow: number | string;
  deny: number | string;
  created_at: string;
  updated_at: string;
}

export interface UpsertOverrideRequest {
  target_type: 'user' | 'role';
  target_id: string;
  allow: string;
  deny: string;
}

/** Shown when a 403 carries no readable `error` string — never a re-worded server reason. */
const REORDER_DENIED_FALLBACK = 'You cannot reorder these roles.';

/** Upper bound on the server-supplied denial text rendered in the reorder banner. */
const MAX_DENIAL_REASON_CHARS = 200;

/**
 * Read the self-scoped `viewer` block from GET /servers/{id}/roles.
 *
 * Absent, malformed, or a non-integer ceiling all collapse to `unknown`, which
 * drives the reorder UI read-only. A control plane older than the `viewer` block
 * is a real self-hosted deployment, so the ceiling is NEVER guessed or derived —
 * guessing a ceiling is what shipped the previous attempt's owner-only bug.
 *
 * Wire is snake_case (`max_role_position`); the client model is camelCase.
 */
function parseRoleViewer(raw: unknown): RoleViewer {
  if (typeof raw !== 'object' || raw === null) return { kind: 'unknown' };
  const viewer = raw as { kind?: unknown; max_role_position?: unknown };
  if (viewer.kind === 'owner') return { kind: 'owner' };
  if (
    viewer.kind === 'bounded' &&
    typeof viewer.max_role_position === 'number' &&
    Number.isInteger(viewer.max_role_position) &&
    // `>= 0` is explicit rather than emergent. A negative ceiling already fails
    // closed — every role reads as above it, the band empties, and the rail goes
    // read-only — but that outcome falls out of downstream arithmetic rather
    // than being stated here, so a later change to `isAboveCeiling` could
    // silently turn it into an open failure. Positions are non-negative by
    // construction; say so at the boundary.
    viewer.max_role_position >= 0
  ) {
    return { kind: 'bounded', maxRolePosition: viewer.max_role_position };
  }
  return { kind: 'unknown' };
}

/**
 * Carry the server's actionable denial text verbatim — the reorder guards return
 * distinct, user-facing strings ("Cannot reorder roles at or above your own
 * position" vs "Reorder would create roles at or above your position") and the
 * banner renders whichever one came back. Body parsing has its own guard so a
 * non-JSON 403 degrades to the fallback instead of surfacing as a network error.
 */
async function readDenialReason(res: Response): Promise<string> {
  try {
    const body = (await res.json()) as { error?: unknown };
    if (typeof body.error === 'string' && body.error.trim() !== '') {
      // Bounded because this string is SERVER-CONTROLLED and is rendered into the
      // banner verbatim. React escapes it, so there is no injection path; the
      // residual is a hostile or misconfigured self-hosted control plane emitting
      // a multi-kilobyte or misleading banner. The real guard reasons are all
      // short static literals, so this truncation can only ever fire on a
      // response the genuine server would not send.
      return body.error.slice(0, MAX_DENIAL_REASON_CHARS);
    }
  } catch {
    // Non-JSON or truncated body — fall through to the fallback.
  }
  return REORDER_DENIED_FALLBACK;
}

interface PermissionState {
  // Server roles keyed by server ID
  serverRoles: Record<string, Role[]>;
  // Reorder ceiling of the current user per server, from the roles `viewer` block
  roleViewer: Record<string, RoleViewer>;
  // User's effective permissions per server (BigInt as string for storage)
  serverPermissions: Record<string, bigint>;
  // User's effective permissions per channel
  channelPermissions: Record<string, bigint>;
  // Channel overrides keyed by channel ID
  channelOverrides: Record<string, ChannelOverride[]>;

  // --- Permission checks ---
  hasServerPermission: (serverId: string, perm: bigint) => boolean;

  // --- Role management ---
  fetchRoles: (serverId: string) => Promise<boolean>;
  createRole: (
    serverId: string,
    data: { name: string; color?: string; permissions?: string }
  ) => Promise<Role | null>;
  updateRole: (
    serverId: string,
    roleId: string,
    data: Partial<{
      name: string;
      color: string;
      emoji: string;
      permissions: string;
      display_separately: boolean;
      mentionable: boolean;
    }>
  ) => Promise<boolean>;
  deleteRole: (serverId: string, roleId: string) => Promise<boolean>;
  reorderRoles: (serverId: string, payload: RoleReorderPayload) => Promise<ReorderOutcome>;
  assignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;
  unassignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;

  // --- Server permissions ---
  fetchServerPermissions: (serverId: string) => Promise<void>;
  fetchChannelPermissions: (channelId: string) => Promise<void>;

  // --- Channel overrides (SBAC) ---
  fetchChannelOverrides: (channelId: string) => Promise<void>;
  upsertChannelOverride: (channelId: string, data: UpsertOverrideRequest) => Promise<boolean>;
  deleteChannelOverride: (channelId: string, overrideId: string) => Promise<boolean>;

  // --- Category overrides ---
  fetchCategoryOverrides: (categoryId: string) => Promise<void>;
  upsertCategoryOverride: (categoryId: string, data: UpsertOverrideRequest) => Promise<boolean>;
  deleteCategoryOverride: (categoryId: string, overrideId: string) => Promise<boolean>;

  // --- Category sync ---
  setCategorySync: (channelId: string, sync: boolean) => Promise<boolean>;
}

export const usePermissionStore = createStore<PermissionState>()((set, get) => ({
  serverRoles: {},
  roleViewer: {},
  serverPermissions: {},
  channelPermissions: {},
  channelOverrides: {},

  hasServerPermission: (serverId: string, perm: bigint): boolean => {
    const perms = get().serverPermissions[serverId];
    if (perms === undefined) return false;
    return hasPermission(perms, perm);
  },

  // ─── Role Management ──────────────────────────────────────────────

  // Returns whether THIS call refreshed the view. Callers that need to know
  // whether their own read landed (reorderRoles reporting `reconciled`) must use
  // this return value, NOT an observation of shared state: a per-server revision
  // counter is satisfied by ANY concurrent successful fetch, so a fetch that
  // started before a write and landed after it would mark a failed refetch as
  // reconciled while the view still showed pre-write data. That is one-sided in
  // the unsafe direction, and `roles_reordered` makes concurrent fetches routine.
  // Existing callers that ignore the value keep the previous swallow behaviour.
  fetchRoles: async (serverId: string): Promise<boolean> => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles`);
      if (!res.ok) return false;
      const data = await res.json();
      set((state) => ({
        serverRoles: { ...state.serverRoles, [serverId]: data.roles ?? [] },
        roleViewer: { ...state.roleViewer, [serverId]: parseRoleViewer(data.viewer) },
      }));
      return true;
    } catch {
      // Network error — leave existing state
      return false;
    }
  },

  createRole: async (serverId: string, data) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
      });
      if (!res.ok) return null;
      const json = await res.json();
      const role = json.role as Role;
      // Add to local state
      set((state) => ({
        serverRoles: {
          ...state.serverRoles,
          [serverId]: [...(state.serverRoles[serverId] ?? []), role].sort(
            (a, b) => b.position - a.position
          ),
        },
      }));
      return role;
    } catch {
      return null;
    }
  },

  updateRole: async (serverId: string, roleId: string, data) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles/${roleId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
      });
      if (!res.ok) return false;
      const json = await res.json();
      const serverRole = json.role as Role | undefined;
      set((state) => ({
        serverRoles: {
          ...state.serverRoles,
          [serverId]: (state.serverRoles[serverId] ?? []).map((r) => {
            if (r.id !== roleId) return r;
            // Prefer server-returned role; fall back to optimistic merge from sent data
            if (serverRole) return serverRole;
            return {
              ...r,
              ...(data.name !== undefined && { name: data.name }),
              ...(data.color !== undefined && { color: data.color }),
              ...(data.emoji !== undefined && { emoji: data.emoji }),
              ...(data.permissions !== undefined && { permissions: data.permissions }),
              ...(data.display_separately !== undefined && {
                display_separately: data.display_separately,
              }),
              ...(data.mentionable !== undefined && { mentionable: data.mentionable }),
            };
          }),
        },
      }));
      return true;
    } catch {
      return false;
    }
  },

  deleteRole: async (serverId: string, roleId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles/${roleId}`, {
        method: 'DELETE',
      });
      if (!res.ok) return false;
      set((state) => ({
        serverRoles: {
          ...state.serverRoles,
          [serverId]: (state.serverRoles[serverId] ?? []).filter((r) => r.id !== roleId),
        },
      }));
      return true;
    } catch {
      return false;
    }
  },

  reorderRoles: async (serverId: string, payload: RoleReorderPayload): Promise<ReorderOutcome> => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles/reorder`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        // The brand is a compile-time custody guard, never part of the wire body.
        body: JSON.stringify({ role_ids: payload.role_ids }),
      });

      if (res.ok) {
        // Refetch to pick up the new positions. `reconciled` reports whether THIS
        // read landed: the write is committed either way, but the view may be
        // stale, and the UI says so rather than lying in either direction.
        const reconciled = await get().fetchRoles(serverId);
        return { ok: true, reconciled };
      }

      if (res.status === 403) {
        return { ok: false, kind: 'denied', reason: await readDenialReason(res) };
      }

      // 429 is deliberately NOT folded into `denied`. Apply is a whole-band write
      // against a 5/min/user limit, so a user correcting a mistake reaches it in
      // ordinary use — and `denied` discards the draft, destroying that work over a
      // transient throttle.
      if (res.status === 429) return { ok: false, kind: 'throttled' };

      return { ok: false, kind: 'unexpected' };
    } catch {
      return { ok: false, kind: 'network' };
    }
  },

  assignRole: async (serverId: string, userId: string, roleId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/members/${userId}/roles`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role_id: roleId }),
      });
      return res.ok;
    } catch {
      return false;
    }
  },

  unassignRole: async (serverId: string, userId: string, roleId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/members/${userId}/roles/${roleId}`, {
        method: 'DELETE',
      });
      return res.ok;
    } catch {
      return false;
    }
  },

  // ─── Server Permissions ────────────────────────────────────────────

  fetchServerPermissions: async (serverId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/permissions`);
      if (!res.ok) return;
      const data = await res.json();
      set((state) => ({
        serverPermissions: {
          ...state.serverPermissions,
          [serverId]: parsePermissions(data.permissions),
        },
      }));
    } catch {
      // Network error
    }
  },

  fetchChannelPermissions: async (channelId: string) => {
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/permissions`);
      if (!res.ok) return;
      const data = await res.json();
      set((state) => ({
        channelPermissions: {
          ...state.channelPermissions,
          [channelId]: parsePermissions(data.permissions),
        },
      }));
    } catch {
      // Network error
    }
  },

  // ─── Channel Overrides (SBAC) ──────────────────────────────────────

  fetchChannelOverrides: async (channelId: string) => {
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/overrides`);
      if (!res.ok) return;
      const data = await res.json();
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [channelId]: data.overrides ?? [],
        },
      }));
    } catch {
      // Network error
    }
  },

  upsertChannelOverride: async (channelId: string, data: UpsertOverrideRequest) => {
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/overrides`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
      });
      if (!res.ok) return false;
      // Refetch to get updated list
      await get().fetchChannelOverrides(channelId);
      return true;
    } catch {
      return false;
    }
  },

  deleteChannelOverride: async (channelId: string, overrideId: string) => {
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/overrides/${overrideId}`, {
        method: 'DELETE',
      });
      if (!res.ok) return false;
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [channelId]: (state.channelOverrides[channelId] ?? []).filter((o) => o.id !== overrideId),
        },
      }));
      return true;
    } catch {
      return false;
    }
  },

  // ─── Category Overrides ────────────────────────────────────────────

  fetchCategoryOverrides: async (categoryId: string) => {
    try {
      const res = await apiFetch(`/api/v1/categories/${categoryId}/overrides`);
      if (!res.ok) return;
      const data = await res.json();
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [`category:${categoryId}`]: data.overrides ?? [],
        },
      }));
    } catch {
      // Network error
    }
  },

  upsertCategoryOverride: async (categoryId: string, data: UpsertOverrideRequest) => {
    try {
      const res = await apiFetch(`/api/v1/categories/${categoryId}/overrides`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(data),
      });
      if (!res.ok) return false;
      await get().fetchCategoryOverrides(categoryId);
      return true;
    } catch {
      return false;
    }
  },

  deleteCategoryOverride: async (categoryId: string, overrideId: string) => {
    try {
      const res = await apiFetch(`/api/v1/categories/${categoryId}/overrides/${overrideId}`, {
        method: 'DELETE',
      });
      if (!res.ok) return false;
      set((state) => ({
        channelOverrides: {
          ...state.channelOverrides,
          [`category:${categoryId}`]: (
            state.channelOverrides[`category:${categoryId}`] ?? []
          ).filter((o) => o.id !== overrideId),
        },
      }));
      return true;
    } catch {
      return false;
    }
  },

  // ─── Category Sync ─────────────────────────────────────────────────

  setCategorySync: async (channelId: string, sync: boolean) => {
    try {
      const res = await apiFetch(`/api/v1/channels/${channelId}/permission-sync`, {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ sync_permissions: sync }),
      });
      return res.ok;
    } catch {
      return false;
    }
  },
}));
