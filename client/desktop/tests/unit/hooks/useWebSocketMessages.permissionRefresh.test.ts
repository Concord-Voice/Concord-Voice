// #3456: useWebSocketMessages wires `server_permissions_changed` and
// `permissions_changed` to the permission refresh schedule and tears it down with
// the effect. The schedule itself is covered in
// tests/unit/services/permissionRefresh.test.ts; this file proves the hook
// subscribes both events, forwards the right argument, and owns the lifecycle.
//
// Oracle: the real schedule runs against real stores with only the store actions
// that perform the reads replaced by spies, so a handler that is not registered,
// is registered for the wrong event, or is left armed after unmount shows up as
// a missing or extra read.
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { usePermissionStore } from '@/renderer/stores/chat/permissionStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { resetAllStores } from '../../helpers/store-helpers';

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
  },
}));

vi.mock('@/renderer/services/system/ttsService', () => ({ speak: vi.fn() }));

vi.mock('@/renderer/services/system/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));

vi.mock('@/renderer/services/system/presenceOverrideSync', () => ({
  presenceOverrideSyncService: { handleRemoteUpdate: vi.fn() },
}));

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));

vi.mock('@/renderer/services/system/notificationSoundService', () => ({
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
import { apiFetch } from '@/renderer/services/system/apiClient';
import { createMockWsService, requireHandler } from '../../helpers/wsServiceMock';

const SERVER_A = '11111111-1111-4111-8111-111111111111';
const SERVER_B = '22222222-2222-4222-8222-222222222222';
const UNKNOWN_SERVER = '99999999-9999-4999-8999-999999999999';
const CH_A1 = 'a1a1a1a1-a1a1-4a1a-8a1a-a1a1a1a1a1a1';

const fetchServerPermissions = vi.fn();
const fetchChannelPermissions = vi.fn();
const evictChannelPermissions = vi.fn();
const evictServerPermissions = vi.fn();
const fetchServers = vi.fn();

function dispatch(ws: ReturnType<typeof createMockWsService>, type: string, data: object): void {
  const handler = requireHandler(ws, type);
  act(() => handler({ type, data }));
}

beforeEach(() => {
  vi.useFakeTimers();
  resetAllStores();
  vi.mocked(apiFetch)
    .mockReset()
    .mockResolvedValue({ ok: true, json: () => Promise.resolve({}) } as never);
  fetchServerPermissions.mockReset().mockResolvedValue(undefined);
  fetchChannelPermissions.mockReset().mockResolvedValue(undefined);
  evictChannelPermissions.mockReset();
  evictServerPermissions.mockReset();
  fetchServers.mockReset().mockResolvedValue(undefined);
  useAuthStore.getState().beginAuthLifecycle('token-a', 'session-a');
  usePermissionStore.setState({
    fetchServerPermissions,
    fetchChannelPermissions,
    evictChannelPermissions,
    evictServerPermissions,
    serverPermissions: { [SERVER_A]: 1n },
  });
  useServerStore.setState({
    servers: [
      { id: SERVER_A, name: 'a' },
      { id: SERVER_B, name: 'b' },
    ] as never,
    activeServerId: SERVER_A,
    fetchServers,
  });
  useChannelStore.setState({
    activeChannelId: CH_A1,
    channelIdsByServer: { [SERVER_A]: [CH_A1] },
  });
  // Jitter draw 0.5 -> floor(0.5 * 5000) = 2500 ms through the real fullJitter.
  vi.spyOn(Math, 'random').mockReturnValue(0.5);
});

afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('useWebSocketMessages: permission refresh events (#3456)', () => {
  it('registers a handler for each permission-change event', () => {
    // Mutation: delete either wsService.on registration.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    expect(ws.handlers.get('server_permissions_changed')).toBeDefined();
    expect(ws.handlers.get('permissions_changed')).toBeDefined();
  });

  it('server_permissions_changed refetches the named server after the jitter, not before', () => {
    // Mutation: pass a different field than msg.data.server_id to the schedule.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    dispatch(ws, 'server_permissions_changed', { server_id: SERVER_A });
    vi.advanceTimersByTime(2499);
    expect(fetchServerPermissions).not.toHaveBeenCalled();

    vi.advanceTimersByTime(1);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
    expect(fetchChannelPermissions).toHaveBeenCalledExactlyOnceWith(CH_A1);
  });

  it('server_permissions_changed for an unknown server arms nothing', () => {
    // Mutation: bypass the schedule's membership check in the handler.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    dispatch(ws, 'server_permissions_changed', { server_id: UNKNOWN_SERVER });

    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(10_000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(evictChannelPermissions).not.toHaveBeenCalled();
  });

  it('a second server_permissions_changed for the same server while pending is dropped', () => {
    // Mutation: build a new schedule per event instead of sharing one per effect.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    dispatch(ws, 'server_permissions_changed', { server_id: SERVER_A });
    dispatch(ws, 'server_permissions_changed', { server_id: SERVER_A });

    expect(vi.getTimerCount()).toBe(1);
    vi.advanceTimersByTime(10_000);
    expect(fetchServerPermissions).toHaveBeenCalledTimes(1);
  });

  it('permissions_changed debounces a burst into one membership read plus the active server and channel', async () => {
    // Mutation: register the handler on the wrong event, or skip the debounce.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    dispatch(ws, 'permissions_changed', {});
    dispatch(ws, 'permissions_changed', {});
    dispatch(ws, 'permissions_changed', {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });

    expect(fetchServers).toHaveBeenCalledTimes(1);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
    expect(fetchChannelPermissions).toHaveBeenCalledExactlyOnceWith(CH_A1);
  });

  it('permissions_changed evicts the cached answers of the servers it does not re-read', async () => {
    // Mutation: stop refreshOwn evicting, or evict the active server too.
    usePermissionStore.setState({ serverPermissions: { [SERVER_A]: 1n, [SERVER_B]: 2n } });
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    dispatch(ws, 'permissions_changed', {});
    await act(async () => {
      await vi.advanceTimersByTimeAsync(300);
    });

    expect(evictServerPermissions).toHaveBeenCalledExactlyOnceWith([SERVER_B]);
    expect(fetchServerPermissions).toHaveBeenCalledExactlyOnceWith(SERVER_A);
  });

  it('unmount unregisters both handlers and clears every pending timer', () => {
    // Mutation: drop permissionRefresh.dispose() or either unsubscribe from the cleanup.
    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));
    dispatch(ws, 'server_permissions_changed', { server_id: SERVER_A });
    dispatch(ws, 'permissions_changed', {});
    expect(vi.getTimerCount()).toBe(2);

    unmount();

    expect(ws.handlers.get('server_permissions_changed')).toBeUndefined();
    expect(ws.handlers.get('permissions_changed')).toBeUndefined();
    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(60_000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
    expect(fetchServers).not.toHaveBeenCalled();
  });

  it('an account switch clears the timers the previous account armed', () => {
    // Mutation: drop the authGeneration subscription from the schedule.
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    dispatch(ws, 'server_permissions_changed', { server_id: SERVER_A });
    expect(vi.getTimerCount()).toBe(1);

    act(() => {
      useAuthStore.getState().beginAuthLifecycle('token-b', 'session-b');
    });

    expect(vi.getTimerCount()).toBe(0);
    vi.advanceTimersByTime(10_000);
    expect(fetchServerPermissions).not.toHaveBeenCalled();
  });
});
