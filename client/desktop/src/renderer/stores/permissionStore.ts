/**
 * Permission Store — manages RBAC roles, effective permissions, and channel overrides.
 * Integrates with the backend RBAC/SBAC system.
 */

import { createStore } from '../utils/createStore';
import { apiFetch } from '../services/apiClient';
import { Role } from '../types/server';
import { hasPermission, parsePermissions } from '../utils/permissions';

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

interface PermissionState {
  // Server roles keyed by server ID
  serverRoles: Record<string, Role[]>;
  // User's effective permissions per server (BigInt as string for storage)
  serverPermissions: Record<string, bigint>;
  // Channel overrides keyed by channel ID
  channelOverrides: Record<string, ChannelOverride[]>;

  // --- Permission checks ---
  hasServerPermission: (serverId: string, perm: bigint) => boolean;

  // --- Role management ---
  fetchRoles: (serverId: string) => Promise<void>;
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
  reorderRoles: (serverId: string, roleIds: string[]) => Promise<boolean>;
  assignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;
  unassignRole: (serverId: string, userId: string, roleId: string) => Promise<boolean>;

  // --- Server permissions ---
  fetchServerPermissions: (serverId: string) => Promise<void>;

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
  serverPermissions: {},
  channelOverrides: {},

  hasServerPermission: (serverId: string, perm: bigint): boolean => {
    const perms = get().serverPermissions[serverId];
    if (perms === undefined) return false;
    return hasPermission(perms, perm);
  },

  // ─── Role Management ──────────────────────────────────────────────

  fetchRoles: async (serverId: string) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles`);
      if (!res.ok) return;
      const data = await res.json();
      set((state) => ({
        serverRoles: { ...state.serverRoles, [serverId]: data.roles ?? [] },
      }));
    } catch {
      // Network error — leave existing state
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

  reorderRoles: async (serverId: string, roleIds: string[]) => {
    try {
      const res = await apiFetch(`/api/v1/servers/${serverId}/roles/reorder`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ role_ids: roleIds }),
      });
      if (!res.ok) return false;
      // Refetch to get updated positions
      await get().fetchRoles(serverId);
      return true;
    } catch {
      return false;
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
