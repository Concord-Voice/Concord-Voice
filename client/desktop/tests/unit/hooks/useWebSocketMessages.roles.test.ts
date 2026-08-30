import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { resetAllStores } from '../../helpers/store-helpers';

// Mock side-effecting services so the hook mounts cleanly (mirrors the
// harness in useWebSocketMessages.richPresence.test.ts).
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({
  speak: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: { handleRemoteUpdate: vi.fn() },
}));

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
}));

vi.mock('@/renderer/services/notificationSoundService', () => ({
  notificationSoundService: {
    play: vi.fn(),
    playLoop: vi.fn(),
    stopLoop: vi.fn(),
    stopAllLoops: vi.fn(),
    isLooping: vi.fn().mockReturnValue(false),
    init: vi.fn(),
  },
}));

import { useWebSocketMessages } from '@/renderer/hooks/messaging/useWebSocketMessages';
import { apiFetch } from '@/renderer/services/apiClient';
import { createMockWsService, requireHandler } from '../../helpers/wsServiceMock';

const ACTIVE_SERVER = '11111111-1111-4111-8111-111111111111';
const OTHER_SERVER = '22222222-2222-4222-8222-222222222222';
const ROLE_A = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const ROLE_B = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';

/** Roles the *server* considers authoritative after the reorder. */
const serverTruth = [
  { id: ROLE_B, name: 'B', position: 1, permissions: '0', color: null },
  { id: ROLE_A, name: 'A', position: 0, permissions: '0', color: null },
];

function rolesResponse(roles: unknown[]) {
  return { ok: true, json: () => Promise.resolve({ roles }) };
}

/** Dispatch one envelope through its captured handler and drain the refetch. */
async function dispatchEvent(
  ws: ReturnType<typeof createMockWsService>,
  type: string,
  data: Record<string, unknown>
) {
  const handler = requireHandler(ws, type);
  await act(async () => {
    handler({ type, data });
    await Promise.resolve();
    await Promise.resolve();
  });
}

/** Dispatch a `roles_reordered` envelope and drain the handler's refetch. */
async function dispatchReorder(
  ws: ReturnType<typeof createMockWsService>,
  serverId: string,
  roleIds: string[]
) {
  await dispatchEvent(ws, 'roles_reordered', { server_id: serverId, role_ids: roleIds });
}

/** A minimal `role_created` role DTO — the handler reads only `server_id`. */
function rolePayload(id: string, serverId: string) {
  return {
    id,
    server_id: serverId,
    name: 'New Role',
    position: 1,
    permissions: '0',
    is_default: false,
    is_managed: false,
    display_separately: false,
    mentionable: false,
    created_at: '2025-01-01T00:00:00Z',
    updated_at: '2025-01-01T00:00:00Z',
  };
}

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useServerStore.setState({ activeServerId: ACTIVE_SERVER });
  vi.mocked(apiFetch)
    .mockReset()
    .mockResolvedValue(rolesResponse([]) as never);
});

describe('useWebSocketMessages — roles_reordered (#2859)', () => {
  it('registers a roles_reordered handler', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('roles_reordered')).toBeDefined();
  });

  it('refetches the roles of the server named in the payload', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchReorder(ws, ACTIVE_SERVER, [ROLE_B, ROLE_A]);

    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${ACTIVE_SERVER}/roles`);
  });

  it('refetches for a server that is NOT the active server', async () => {
    // The server-settings overlay is opened with an explicit serverId and can be
    // showing a server other than the active one — the exact window this handler
    // exists to close. An active-server gate here would be a silent regression.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchReorder(ws, OTHER_SERVER, [ROLE_B, ROLE_A]);

    expect(useServerStore.getState().activeServerId).toBe(ACTIVE_SERVER);
    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${OTHER_SERVER}/roles`);
  });

  it('takes the hierarchy from the refetch, not from the payload role_ids', async () => {
    // `role_ids` is the client-supplied id slice the server applied and carries
    // no positions, so reconciling from it locally would invent a hierarchy.
    vi.mocked(apiFetch).mockResolvedValueOnce(rolesResponse(serverTruth) as never);
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    // Payload order deliberately disagrees with what the server returns.
    await dispatchReorder(ws, ACTIVE_SERVER, [ROLE_A, ROLE_B]);

    expect(usePermissionStore.getState().serverRoles[ACTIVE_SERVER]).toEqual(serverTruth);
  });

  it('unregisters the roles_reordered handler on unmount', () => {
    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('roles_reordered')).toBeDefined();

    unmount();

    expect(ws.handlers.get('roles_reordered')).toBeUndefined();
  });
});

describe('useWebSocketMessages — role_created / role_deleted (#2859)', () => {
  // These two events change the role SET the reorder band is derived from, so a
  // rail built before one of them keeps rendering a stale band. On Apply that
  // yields either a payload naming a deleted role (404 → `unexpected`, a dead
  // end the user cannot retry out of) or a payload MISSING a band member, which
  // the server renumbers around — committing duplicate positions under HTTP 200.
  it('registers role_created and role_deleted handlers', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    expect(ws.handlers.get('role_created')).toBeDefined();
    expect(ws.handlers.get('role_deleted')).toBeDefined();
  });

  it('role_created refetches the roles of the server named in the payload', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchEvent(ws, 'role_created', {
      server_id: ACTIVE_SERVER,
      role: rolePayload(ROLE_A, ACTIVE_SERVER),
    });

    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${ACTIVE_SERVER}/roles`);
  });

  it('role_created refetches for a server that is NOT the active server', async () => {
    // Same property the roles_reordered handler carries: server settings can be
    // open on a server other than the active one, so an active-server gate here
    // would be a silent regression — and it is exactly the kind of gate a later
    // "only refetch what is on screen" optimisation would add.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchEvent(ws, 'role_created', {
      server_id: OTHER_SERVER,
      role: rolePayload(ROLE_A, OTHER_SERVER),
    });

    expect(useServerStore.getState().activeServerId).toBe(ACTIVE_SERVER);
    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${OTHER_SERVER}/roles`);
  });

  it('role_deleted refetches the roles of the server named in the payload', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchEvent(ws, 'role_deleted', { server_id: ACTIVE_SERVER, role_id: ROLE_B });

    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${ACTIVE_SERVER}/roles`);
  });

  it('role_deleted refetches for a server that is NOT the active server', async () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchEvent(ws, 'role_deleted', { server_id: OTHER_SERVER, role_id: ROLE_B });

    expect(useServerStore.getState().activeServerId).toBe(ACTIVE_SERVER);
    expect(apiFetch).toHaveBeenCalledWith(`/api/v1/servers/${OTHER_SERVER}/roles`);
  });

  it('takes the refreshed role set from the fetch, not from the event payload', async () => {
    // `role_created` carries the new role DTO, but splicing it in locally would
    // invent positions for every other role. The refetch is the only source.
    vi.mocked(apiFetch).mockResolvedValueOnce(rolesResponse(serverTruth) as never);
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    await dispatchEvent(ws, 'role_created', {
      server_id: ACTIVE_SERVER,
      role: rolePayload('cccccccc-cccc-4ccc-8ccc-cccccccccccc', ACTIVE_SERVER),
    });

    expect(usePermissionStore.getState().serverRoles[ACTIVE_SERVER]).toEqual(serverTruth);
  });

  it('unregisters both handlers on unmount', () => {
    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.get('role_created')).toBeDefined();
    expect(ws.handlers.get('role_deleted')).toBeDefined();

    unmount();

    expect(ws.handlers.get('role_created')).toBeUndefined();
    expect(ws.handlers.get('role_deleted')).toBeUndefined();
  });
});
