import { usePermissionStore } from '@/renderer/stores/permissionStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';
import type { Role } from '@/renderer/types/server';
import { ADMINISTRATOR, MANAGE_SERVER, SEND_MESSAGES } from '@/renderer/utils/permissions';

const API_BASE = 'http://localhost:8080';

const makeRole = (overrides: Partial<Role> = {}): Role => ({
  id: 'role-1',
  server_id: 'server-1',
  name: '@all',
  position: 0,
  permissions: '1023',
  is_default: true,
  display_separately: false,
  mentionable: false,
  require_mfa: false,
  created_at: '2025-01-01T00:00:00Z',
  updated_at: '2025-01-01T00:00:00Z',
  ...overrides,
});

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  usePermissionStore.setState({
    serverRoles: {},
    serverPermissions: {},
    channelPermissions: {},
    channelOverrides: {},
  });
});

describe('permissionStore', () => {
  // ── Initial state ─────────────────────────────────────────────────────

  describe('initial state', () => {
    it('starts with empty state', () => {
      const state = usePermissionStore.getState();
      expect(state.serverRoles).toEqual({});
      expect(state.serverPermissions).toEqual({});
      expect(state.channelPermissions).toEqual({});
      expect(state.channelOverrides).toEqual({});
    });
  });

  // ── hasServerPermission ───────────────────────────────────────────────

  describe('hasServerPermission', () => {
    it('returns false when no permissions loaded', () => {
      expect(usePermissionStore.getState().hasServerPermission('server-1', 1n)).toBe(false);
    });

    it('returns true when permission is set', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': 0b111n },
      });
      expect(usePermissionStore.getState().hasServerPermission('server-1', 0b001n)).toBe(true);
      expect(usePermissionStore.getState().hasServerPermission('server-1', 0b100n)).toBe(true);
    });

    it('returns false when permission not in bitmask', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': 0b001n },
      });
      expect(usePermissionStore.getState().hasServerPermission('server-1', 0b100n)).toBe(false);
    });

    it('returns true for any permission when ADMINISTRATOR is set', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': ADMINISTRATOR },
      });
      expect(usePermissionStore.getState().hasServerPermission('server-1', MANAGE_SERVER)).toBe(
        true
      );
      expect(usePermissionStore.getState().hasServerPermission('server-1', SEND_MESSAGES)).toBe(
        true
      );
    });

    it('returns false for different server', () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-1': 0b111n },
      });
      expect(usePermissionStore.getState().hasServerPermission('server-2', 0b001n)).toBe(false);
    });
  });

  // ── fetchRoles ────────────────────────────────────────────────────────

  describe('fetchRoles', () => {
    it('loads roles from API', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({
            roles: [
              makeRole({ id: 'role-1', name: '@all', position: 0 }),
              makeRole({ id: 'role-2', name: 'Admin', position: 10, is_default: false }),
            ],
          });
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles).toHaveLength(2);
      expect(roles[0].name).toBe('@all');
    });

    it('handles API error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      // No roles should be set
      expect(usePermissionStore.getState().serverRoles['server-1']).toBeUndefined();
    });

    it('handles network error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.error();
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      expect(usePermissionStore.getState().serverRoles['server-1']).toBeUndefined();
    });

    it('replaces existing roles for the server', async () => {
      usePermissionStore.setState({
        serverRoles: { 'server-1': [makeRole({ id: 'old-role' })] },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({
            roles: [makeRole({ id: 'new-role', name: 'New' })],
          });
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles).toHaveLength(1);
      expect(roles[0].id).toBe('new-role');
    });

    it('does not affect other servers', async () => {
      usePermissionStore.setState({
        serverRoles: { 'server-2': [makeRole({ id: 'role-s2' })] },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({
            roles: [makeRole({ id: 'role-s1' })],
          });
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      expect(usePermissionStore.getState().serverRoles['server-2']).toHaveLength(1);
      expect(usePermissionStore.getState().serverRoles['server-2'][0].id).toBe('role-s2');
    });

    it('handles null/undefined roles in response', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ roles: null });
        })
      );

      await usePermissionStore.getState().fetchRoles('server-1');
      expect(usePermissionStore.getState().serverRoles['server-1']).toEqual([]);
    });
  });

  // ── createRole ────────────────────────────────────────────────────────

  describe('createRole', () => {
    it('creates a role and adds to server roles', async () => {
      const newRole = makeRole({
        id: 'role-new',
        name: 'Moderator',
        position: 5,
        is_default: false,
      });
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ role: newRole }, { status: 201 });
        })
      );

      usePermissionStore.setState({ serverRoles: { 'server-1': [] } });
      const result = await usePermissionStore.getState().createRole('server-1', {
        name: 'Moderator',
        permissions: '128',
      });

      expect(result).not.toBeNull();
      expect(result?.name).toBe('Moderator');
      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles).toHaveLength(1);
    });

    it('sorts roles by position descending after creation', async () => {
      const existingRole = makeRole({ id: 'role-1', name: '@all', position: 0 });
      const newRole = makeRole({ id: 'role-new', name: 'Admin', position: 10, is_default: false });

      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ role: newRole }, { status: 201 });
        })
      );

      usePermissionStore.setState({ serverRoles: { 'server-1': [existingRole] } });
      await usePermissionStore.getState().createRole('server-1', { name: 'Admin' });

      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles[0].position).toBeGreaterThanOrEqual(roles[1].position);
    });

    it('returns null on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().createRole('server-1', { name: 'Mod' });
      expect(result).toBeNull();
    });

    it('returns null on network error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().createRole('server-1', { name: 'Mod' });
      expect(result).toBeNull();
    });

    it('initializes server roles array if none exists', async () => {
      const newRole = makeRole({ id: 'role-new', position: 5, is_default: false });
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ role: newRole }, { status: 201 });
        })
      );

      // No existing roles for server-1
      const result = await usePermissionStore.getState().createRole('server-1', { name: 'Role' });
      expect(result).not.toBeNull();
      expect(usePermissionStore.getState().serverRoles['server-1']).toHaveLength(1);
    });
  });

  // ── updateRole ────────────────────────────────────────────────────────

  describe('updateRole', () => {
    it('updates a role with server-returned data', async () => {
      const updatedRole = makeRole({ id: 'role-1', name: 'Updated Name', position: 0 });
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({ role: updatedRole });
        })
      );

      usePermissionStore.setState({
        serverRoles: { 'server-1': [makeRole()] },
      });

      const result = await usePermissionStore.getState().updateRole('server-1', 'role-1', {
        name: 'Updated Name',
      });
      expect(result).toBe(true);
      expect(usePermissionStore.getState().serverRoles['server-1'][0].name).toBe('Updated Name');
    });

    it('falls back to optimistic merge when server returns no role', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({}); // No role in response
        })
      );

      usePermissionStore.setState({
        serverRoles: { 'server-1': [makeRole({ name: 'Original' })] },
      });

      await usePermissionStore.getState().updateRole('server-1', 'role-1', {
        name: 'Optimistic',
        color: '#ff0000',
      });

      const role = usePermissionStore.getState().serverRoles['server-1'][0];
      expect(role.name).toBe('Optimistic');
      expect(role.color).toBe('#ff0000');
    });

    it('returns false on API error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().updateRole('server-1', 'role-1', {
        name: 'X',
      });
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().updateRole('server-1', 'role-1', {
        name: 'X',
      });
      expect(result).toBe(false);
    });

    it('updates partial fields (only name)', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({}); // No role — triggers optimistic merge
        })
      );

      const original = makeRole({ name: 'Original', color: '#00ff00' });
      usePermissionStore.setState({ serverRoles: { 'server-1': [original] } });

      await usePermissionStore.getState().updateRole('server-1', 'role-1', { name: 'Renamed' });

      const role = usePermissionStore.getState().serverRoles['server-1'][0];
      expect(role.name).toBe('Renamed');
      expect(role.color).toBe('#00ff00'); // Unchanged
    });

    it('can update display_separately and mentionable', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({}); // Optimistic merge
        })
      );

      usePermissionStore.setState({ serverRoles: { 'server-1': [makeRole()] } });

      await usePermissionStore.getState().updateRole('server-1', 'role-1', {
        display_separately: true,
        mentionable: true,
      });

      const role = usePermissionStore.getState().serverRoles['server-1'][0];
      expect(role.display_separately).toBe(true);
      expect(role.mentionable).toBe(true);
    });
  });

  // ── deleteRole ────────────────────────────────────────────────────────

  describe('deleteRole', () => {
    it('deletes a role and removes from state', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({ message: 'Role deleted' });
        })
      );

      usePermissionStore.setState({
        serverRoles: {
          'server-1': [
            makeRole({ id: 'role-1' }),
            makeRole({ id: 'role-2', name: 'Mod', position: 5, is_default: false }),
          ],
        },
      });

      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(result).toBe(true);
      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles).toHaveLength(1);
      expect(roles[0].id).toBe('role-2');
    });

    it('returns false on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/roles/:roleId`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().deleteRole('server-1', 'role-1');
      expect(result).toBe(false);
    });
  });

  // ── reorderRoles ──────────────────────────────────────────────────────

  describe('reorderRoles', () => {
    it('reorders roles and refetches', async () => {
      const reorderedRoles = [
        makeRole({ id: 'role-2', position: 1 }),
        makeRole({ id: 'role-1', position: 0 }),
      ];

      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/reorder`, () => {
          return HttpResponse.json({ message: 'Reordered' });
        }),
        http.get(`${API_BASE}/api/v1/servers/:id/roles`, () => {
          return HttpResponse.json({ roles: reorderedRoles });
        })
      );

      const result = await usePermissionStore
        .getState()
        .reorderRoles('server-1', ['role-2', 'role-1']);
      expect(result).toBe(true);

      // Should have refetched roles
      const roles = usePermissionStore.getState().serverRoles['server-1'];
      expect(roles).toHaveLength(2);
    });

    it('returns false on API error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/reorder`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore
        .getState()
        .reorderRoles('server-1', ['role-1', 'role-2']);
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/servers/:id/roles/reorder`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().reorderRoles('server-1', ['role-1']);
      expect(result).toBe(false);
    });
  });

  // ── assignRole / unassignRole ─────────────────────────────────────────

  describe('assignRole', () => {
    it('assigns a role and returns true', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/members/:userId/roles`, () => {
          return HttpResponse.json({ message: 'Assigned' });
        })
      );

      const result = await usePermissionStore.getState().assignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(true);
    });

    it('returns false on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/members/:userId/roles`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );

      const result = await usePermissionStore.getState().assignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/servers/:id/members/:userId/roles`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().assignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(false);
    });
  });

  describe('unassignRole', () => {
    it('unassigns a role and returns true', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/members/:userId/roles/:roleId`, () => {
          return HttpResponse.json({ message: 'Unassigned' });
        })
      );

      const result = await usePermissionStore
        .getState()
        .unassignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(true);
    });

    it('returns false on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/members/:userId/roles/:roleId`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore
        .getState()
        .unassignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/servers/:id/members/:userId/roles/:roleId`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore
        .getState()
        .unassignRole('server-1', 'user-1', 'role-1');
      expect(result).toBe(false);
    });
  });

  // ── fetchServerPermissions ────────────────────────────────────────────

  describe('fetchServerPermissions', () => {
    it('loads effective permissions and parses as bigint', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/permissions`, () => {
          return HttpResponse.json({ permissions: '1023' });
        })
      );

      await usePermissionStore.getState().fetchServerPermissions('server-1');
      const perms = usePermissionStore.getState().serverPermissions['server-1'];
      expect(perms).toBe(1023n);
    });

    it('handles API error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/permissions`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await usePermissionStore.getState().fetchServerPermissions('server-1');
      expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
    });

    it('handles network error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/permissions`, () => {
          return HttpResponse.error();
        })
      );

      await usePermissionStore.getState().fetchServerPermissions('server-1');
      expect(usePermissionStore.getState().serverPermissions['server-1']).toBeUndefined();
    });

    it('does not affect other servers', async () => {
      usePermissionStore.setState({
        serverPermissions: { 'server-2': 999n },
      });

      server.use(
        http.get(`${API_BASE}/api/v1/servers/:id/permissions`, () => {
          return HttpResponse.json({ permissions: '1023' });
        })
      );

      await usePermissionStore.getState().fetchServerPermissions('server-1');
      expect(usePermissionStore.getState().serverPermissions['server-2']).toBe(999n);
    });
  });

  // ── fetchChannelPermissions ──────────────────────────────────────────

  describe('fetchChannelPermissions', () => {
    it('loads effective channel permissions and parses as bigint', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/channels/:id/permissions`, () => {
          return HttpResponse.json({ permissions: '2048' });
        })
      );

      await usePermissionStore.getState().fetchChannelPermissions('ch-1');
      const perms = usePermissionStore.getState().channelPermissions['ch-1'];
      expect(perms).toBe(2048n);
    });

    it('handles API error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/channels/:id/permissions`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await usePermissionStore.getState().fetchChannelPermissions('ch-1');
      expect(usePermissionStore.getState().channelPermissions['ch-1']).toBeUndefined();
    });
  });

  // ── Channel overrides ─────────────────────────────────────────────────

  describe('fetchChannelOverrides', () => {
    it('fetches channel overrides', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({
            overrides: [
              {
                id: 'ov-1',
                channel_id: 'ch-1',
                target_type: 'role',
                target_id: 'role-1',
                allow: '0',
                deny: '2048',
                created_at: '2025-01-01T00:00:00Z',
                updated_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await usePermissionStore.getState().fetchChannelOverrides('ch-1');
      const overrides = usePermissionStore.getState().channelOverrides['ch-1'];
      expect(overrides).toHaveLength(1);
      expect(overrides[0].target_type).toBe('role');
    });

    it('handles API error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await usePermissionStore.getState().fetchChannelOverrides('ch-1');
      expect(usePermissionStore.getState().channelOverrides['ch-1']).toBeUndefined();
    });

    it('handles null overrides', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({ overrides: null });
        })
      );

      await usePermissionStore.getState().fetchChannelOverrides('ch-1');
      expect(usePermissionStore.getState().channelOverrides['ch-1']).toEqual([]);
    });
  });

  describe('upsertChannelOverride', () => {
    it('upserts override and refetches', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({ message: 'Upserted' });
        }),
        http.get(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({
            overrides: [
              {
                id: 'ov-new',
                channel_id: 'ch-1',
                target_type: 'role',
                target_id: 'role-1',
                allow: '1024',
                deny: '0',
                created_at: '2025-01-01T00:00:00Z',
                updated_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      const result = await usePermissionStore.getState().upsertChannelOverride('ch-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '1024',
        deny: '0',
      });

      expect(result).toBe(true);
      expect(usePermissionStore.getState().channelOverrides['ch-1']).toHaveLength(1);
    });

    it('returns false on API error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().upsertChannelOverride('ch-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '0',
        deny: '0',
      });
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/overrides`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().upsertChannelOverride('ch-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '0',
        deny: '0',
      });
      expect(result).toBe(false);
    });
  });

  describe('deleteChannelOverride', () => {
    it('deletes override and removes from state', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/channels/:id/overrides/:ovId`, () => {
          return HttpResponse.json({ message: 'Deleted' });
        })
      );

      usePermissionStore.setState({
        channelOverrides: {
          'ch-1': [
            {
              id: 'ov-1',
              channel_id: 'ch-1',
              target_type: 'role',
              target_id: 'role-1',
              allow: '0',
              deny: '2048',
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
            {
              id: 'ov-2',
              channel_id: 'ch-1',
              target_type: 'user',
              target_id: 'user-1',
              allow: '1024',
              deny: '0',
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });

      const result = await usePermissionStore.getState().deleteChannelOverride('ch-1', 'ov-1');
      expect(result).toBe(true);
      const overrides = usePermissionStore.getState().channelOverrides['ch-1'];
      expect(overrides).toHaveLength(1);
      expect(overrides[0].id).toBe('ov-2');
    });

    it('returns false on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/channels/:id/overrides/:ovId`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );

      const result = await usePermissionStore.getState().deleteChannelOverride('ch-1', 'ov-1');
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/channels/:id/overrides/:ovId`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().deleteChannelOverride('ch-1', 'ov-1');
      expect(result).toBe(false);
    });
  });

  // ── Category overrides ────────────────────────────────────────────────

  describe('fetchCategoryOverrides', () => {
    it('fetches category overrides and stores under category: prefix', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.json({
            overrides: [
              {
                id: 'ov-cat-1',
                channel_id: 'cat-1',
                target_type: 'role',
                target_id: 'role-1',
                allow: '1024',
                deny: '0',
                created_at: '2025-01-01T00:00:00Z',
                updated_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await usePermissionStore.getState().fetchCategoryOverrides('cat-1');
      const overrides = usePermissionStore.getState().channelOverrides['category:cat-1'];
      expect(overrides).toHaveLength(1);
      expect(overrides[0].id).toBe('ov-cat-1');
    });

    it('handles API error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await usePermissionStore.getState().fetchCategoryOverrides('cat-1');
      expect(usePermissionStore.getState().channelOverrides['category:cat-1']).toBeUndefined();
    });

    it('handles network error silently', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.error();
        })
      );

      await usePermissionStore.getState().fetchCategoryOverrides('cat-1');
      expect(usePermissionStore.getState().channelOverrides['category:cat-1']).toBeUndefined();
    });
  });

  describe('upsertCategoryOverride', () => {
    it('upserts category override and refetches', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.json({ message: 'Upserted' });
        }),
        http.get(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.json({
            overrides: [
              {
                id: 'ov-cat-new',
                channel_id: 'cat-1',
                target_type: 'role',
                target_id: 'role-1',
                allow: '1024',
                deny: '0',
                created_at: '2025-01-01T00:00:00Z',
                updated_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      const result = await usePermissionStore.getState().upsertCategoryOverride('cat-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '1024',
        deny: '0',
      });

      expect(result).toBe(true);
      expect(usePermissionStore.getState().channelOverrides['category:cat-1']).toHaveLength(1);
    });

    it('returns false on API error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().upsertCategoryOverride('cat-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '0',
        deny: '0',
      });
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/categories/:id/overrides`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().upsertCategoryOverride('cat-1', {
        target_type: 'role',
        target_id: 'role-1',
        allow: '0',
        deny: '0',
      });
      expect(result).toBe(false);
    });
  });

  describe('deleteCategoryOverride', () => {
    it('deletes category override from state', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/categories/:id/overrides/:ovId`, () => {
          return HttpResponse.json({ message: 'Deleted' });
        })
      );

      usePermissionStore.setState({
        channelOverrides: {
          'category:cat-1': [
            {
              id: 'ov-cat-1',
              channel_id: 'cat-1',
              target_type: 'role',
              target_id: 'role-1',
              allow: '1024',
              deny: '0',
              created_at: '2025-01-01T00:00:00Z',
              updated_at: '2025-01-01T00:00:00Z',
            },
          ],
        },
      });

      const result = await usePermissionStore
        .getState()
        .deleteCategoryOverride('cat-1', 'ov-cat-1');
      expect(result).toBe(true);
      expect(usePermissionStore.getState().channelOverrides['category:cat-1']).toHaveLength(0);
    });

    it('returns false on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/categories/:id/overrides/:ovId`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );

      const result = await usePermissionStore.getState().deleteCategoryOverride('cat-1', 'ov-1');
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/categories/:id/overrides/:ovId`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().deleteCategoryOverride('cat-1', 'ov-1');
      expect(result).toBe(false);
    });
  });

  // ── setCategorySync ───────────────────────────────────────────────────

  describe('setCategorySync', () => {
    it('sends sync request and returns true', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/permission-sync`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ message: 'Synced' });
        })
      );

      const result = await usePermissionStore.getState().setCategorySync('ch-1', true);
      expect(result).toBe(true);
      expect(capturedBody).toEqual({ sync_permissions: true });
    });

    it('sends false when disabling sync', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/permission-sync`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ message: 'Unsynced' });
        })
      );

      const result = await usePermissionStore.getState().setCategorySync('ch-1', false);
      expect(result).toBe(true);
      expect(capturedBody).toEqual({ sync_permissions: false });
    });

    it('returns false on API error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/permission-sync`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      const result = await usePermissionStore.getState().setCategorySync('ch-1', true);
      expect(result).toBe(false);
    });

    it('returns false on network error', async () => {
      server.use(
        http.put(`${API_BASE}/api/v1/channels/:id/permission-sync`, () => {
          return HttpResponse.error();
        })
      );

      const result = await usePermissionStore.getState().setCategorySync('ch-1', true);
      expect(result).toBe(false);
    });
  });
});
