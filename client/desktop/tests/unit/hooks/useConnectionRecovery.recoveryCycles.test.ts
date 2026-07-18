/**
 * Repeated Recovery-A cycle regression (#2199, AC #7) + Recovery-A authoritative
 * refresh (#2329).
 *
 * Unlike the sibling recovery suites, this file does NOT mock resetService — the
 * real recoveryReset runs against real Zustand stores. Only the e2eeService crypto
 * singleton is stubbed. That is what makes this an integration regression rather
 * than a call assertion: it fails if recoveryReset is ever widened to clear
 * account-scoped state, or if the handler is pointed back at gracefulReset.
 *
 * The #2329 cases additionally exercise the authoritative refresh the handler now
 * performs after hydration: a re-fetch of servers + friend requests (session-
 * guarded) and a `connection-recovered` event that re-fetches the mounted
 * channel's messages missed during the outage.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { useConnectionStore } from '@/renderer/stores/connectionStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useDraftMessageStore } from '@/renderer/stores/draftMessageStore';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';

const mockWsService = {
  setAggressiveReconnect: vi.fn(),
  getState: vi.fn().mockReturnValue('CONNECTED'),
};

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => mockWsService,
  ConnectionState: {
    DISCONNECTED: 'DISCONNECTED',
    CONNECTING: 'CONNECTING',
    CONNECTED: 'CONNECTED',
    RECONNECTING: 'RECONNECTING',
  },
}));

const fencePendingOperations = vi.fn();
const messageOperationGuard = { assertCurrent: vi.fn() };
const getChannelKey = vi.fn().mockResolvedValue(null);
const decryptForChannel = vi.fn(async (_channelId: string, ciphertext: string) => ciphertext);
vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    fencePendingOperations: () => fencePendingOperations(),
    clearKeys: vi.fn(),
    revokeChannelAccess: vi.fn(),
    createChannelOperationGuard: vi.fn(() => messageOperationGuard),
    getChannelKey: (...args: unknown[]) => getChannelKey(...args),
    getChannelKeyByVersion: vi.fn(),
    decryptWithKey: vi.fn(async (ciphertext: string) => ciphertext),
    decryptForChannel: (...args: [string, string]) => decryptForChannel(...args),
    decryptForChannelWithVersion: vi.fn(
      async (_channelId: string, ciphertext: string) => ciphertext
    ),
    isInitialized: true,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    validateEpochs: vi.fn(),
  },
}));

// Deterministic per-endpoint routing. The recovery handler (#2329) now hits three
// endpoints — channel messages, /servers, /friends/requests — so the mock routes
// by URL and returns a Response whose json() yields the per-endpoint payload.
// safeJson delegates to res.json(), and serverStore reads res.json() directly, so
// both consumers see the same payload.
const jsonResponse = (body: unknown) => ({ ok: true, status: 200, json: async () => body });

// Per-test-adjustable payloads (reset in beforeEach).
let messagePages: Array<{ messages: unknown[] }> = [];
let serversResponse: unknown[] = [{ id: 'server-1', name: 'Test' }];
let requestsResponse: unknown = { requests: [] };

const isMessagesUrl = (url: unknown) => String(url).includes('/messages');
const messageFetchCount = () =>
  mockApiFetch.mock.calls.filter((call) => isMessagesUrl(call[0])).length;

const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

vi.mock('@/renderer/services/recoveryService', () => ({
  runPreflight: vi.fn(),
  clearCrashFlag: vi.fn(),
}));

const hydrateCalls = vi.fn();
vi.mock('@/renderer/services/postLoginHydration', () => ({
  hydratePostLogin: () => {
    hydrateCalls();
    return Promise.resolve();
  },
}));

vi.mock('@/renderer/services/postLoginHydrationLifecycle', () => ({
  beginPostLoginHydrationGuard: vi.fn(() => ({
    signal: new AbortController().signal,
    isCurrent: () => true,
  })),
  isHydrationLifecycleCurrent: vi.fn(() => true),
  resetPostLoginHydrationLifecycle: vi.fn(),
}));

import { useConnectionRecovery } from '@/renderer/hooks/useConnectionRecovery';
import { useMessageFetch } from '@/renderer/hooks/useMessageFetch';
import { useUserStore } from '@/renderer/stores/userStore';
import { e2eeService } from '@/renderer/services/e2eeService';
import { isHydrationLifecycleCurrent } from '@/renderer/services/postLoginHydrationLifecycle';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockMessage, mockMessage2 } from '../../mocks/fixtures';

const validateEpochs = vi.fn().mockResolvedValue(undefined);

const staleGuard = { signal: new AbortController().signal, isCurrent: () => false };

function seedAccountState() {
  useServerStore.setState({
    servers: [{ id: 'server-1', name: 'Test' }] as never,
    activeServerId: 'server-1',
  });
  useDraftMessageStore.getState().setDraft('channel-1', {
    text: 'unsent text',
    updatedAt: 1,
  });
  useE2EEStore.getState().setReady(true);
}

beforeEach(() => {
  vi.clearAllMocks();
  mockApiFetch.mockReset();
  mockSafeJson.mockReset();
  vi.mocked(isHydrationLifecycleCurrent).mockReturnValue(true);
  messagePages = [];
  serversResponse = [{ id: 'server-1', name: 'Test' }];
  requestsResponse = { requests: [] };
  mockApiFetch.mockImplementation((url: unknown) => {
    if (isMessagesUrl(url)) {
      return Promise.resolve(jsonResponse(messagePages.shift() ?? { messages: [] }));
    }
    if (String(url).startsWith('/api/v1/servers')) {
      return Promise.resolve(jsonResponse({ servers: serversResponse }));
    }
    if (String(url).startsWith('/api/v1/friends/requests')) {
      return Promise.resolve(jsonResponse(requestsResponse));
    }
    return Promise.resolve(jsonResponse({}));
  });
  mockSafeJson.mockImplementation(async (res: { json: () => Promise<unknown> }) => res.json());
  // [internal]rules/tests.md: reset ALL stores in beforeEach. This suite touches
  // real stores, so without it, module-level store state (drafts, channels,
  // localStorage, fetch journals) can leak from earlier tests in the worker and
  // make the regression order-dependent (#2199 review, P2-1).
  resetAllStores();
  useConnectionStore.getState().reset();
  useVoiceStore.setState({ activeChannelId: null, connectionState: 'disconnected' });
  useUserStore.setState({ fetchUser: vi.fn().mockResolvedValue(undefined) } as never);
  seedAccountState();
});

describe('repeated Recovery-A cycles (#2199 AC #7)', () => {
  it('survives 5 consecutive recovery cycles with no cumulative state loss', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    for (let i = 0; i < 5; i++) {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
      await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(i + 1));

      // Every cycle: account state intact, readiness agrees with the service.
      expect(useServerStore.getState().servers).toHaveLength(1);
      expect(useServerStore.getState().activeServerId).toBe('server-1');
      expect(useDraftMessageStore.getState().getDraft('channel-1')?.text).toBe('unsent text');
      expect(useE2EEStore.getState().ready).toBe(true);
      expect(useConnectionStore.getState().phase).toBe('stable');
    }

    // One fence per cycle — no duplicate or skipped fencing.
    expect(fencePendingOperations).toHaveBeenCalledTimes(5);
  });

  it('keeps E2EE readiness in agreement with the service across cycles (AC #3)', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    useConnectionStore.getState().enterRecoveryA();
    result.current('CONNECTED' as never);
    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));

    expect(useE2EEStore.getState().ready).toBe(e2eeService.isInitialized);
  });

  it('never clears E2EE key material — the handler gates on isInitialized', async () => {
    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    useConnectionStore.getState().enterRecoveryA();
    result.current('CONNECTED' as never);
    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));

    expect(e2eeService.clearKeys).not.toHaveBeenCalled();
  });
});

describe('Recovery-A authoritative refresh (#2329)', () => {
  it('backfills messages missed during Recovery-A without duplicating the mount fetch', async () => {
    const existing = {
      ...mockMessage,
      id: 'before-outage',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:00:00Z',
    };
    const missed = {
      ...mockMessage2,
      id: 'missed-during-outage',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:01:00Z',
    };

    useChannelStore.setState({
      activeChannelId: 'channel-1',
      currentServerId: 'server-1',
    });
    // Page 1 = mount fetch; page 2 = the post-recovery backfill.
    messagePages = [{ messages: [existing] }, { messages: [missed, existing] }];

    const { result } = renderHook(() => {
      useMessageFetch('channel-1', { type: 'channel' });
      return useConnectionRecovery(mockWsService as never, validateEpochs);
    });

    await vi.waitFor(() => {
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((message) => message.id)
      ).toEqual(['before-outage']);
    });
    // Only the mount fetch has hit the message endpoint so far — servers/requests
    // are separate endpoints and must NOT be counted as a duplicate message fetch.
    expect(messageFetchCount()).toBe(1);

    act(() => {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
    });

    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => {
      expect(messageFetchCount()).toBe(2);
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((message) => message.id)
      ).toEqual(['before-outage', 'missed-during-outage']);
    });
  });

  it('refreshes memberships — a removed server purges its channels + unread and a new friend request appears', async () => {
    useServerStore.setState({
      servers: [
        { id: 'server-1', name: 'Test' },
        { id: 'server-2', name: 'Removed while offline' },
      ] as never,
      activeServerId: 'server-1',
    });
    // Seed the channel + unread state that purgeMissingServerState must clear for
    // the removed server (asserts the guard-gated side-effects, not just the list).
    useChannelStore.setState({
      channelIdsByServer: { 'server-1': ['ch-s1'], 'server-2': ['ch-s2'] },
    } as never);
    useUnreadStore.setState({ serverUnreadSet: new Set(['server-1', 'server-2']) } as never);
    // Server-2 is gone from the authoritative list; a new friend request arrived.
    serversResponse = [{ id: 'server-1', name: 'Test' }];
    requestsResponse = {
      requests: [
        {
          id: 'req-1',
          from_user_id: 'user-9',
          from_username: 'newfriend',
          from_display_name: 'New Friend',
          from_avatar_url: null,
          to_user_id: 'me',
          to_username: 'me',
          to_display_name: 'Me',
          to_avatar_url: null,
          direction: 'incoming',
          created_at: '2025-01-01T12:00:00Z',
        },
      ],
    };

    const { result } = renderHook(() =>
      useConnectionRecovery(mockWsService as never, validateEpochs)
    );

    act(() => {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
    });

    await vi.waitFor(() => {
      expect(useServerStore.getState().servers.map((s) => s.id)).toEqual(['server-1']);
      expect(useFriendStore.getState().pendingRequests.map((r) => r.id)).toEqual(['req-1']);
      // The removed server's channels + unread are actually purged; the surviving
      // server's state is preserved.
      expect(useChannelStore.getState().channelIdsByServer['server-2']).toBeUndefined();
      expect(useChannelStore.getState().channelIdsByServer['server-1']).toEqual(['ch-s1']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-2')).toBe(false);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
    });
  });

  it('still backfills messages when the membership refresh fails, without corrupting the store', async () => {
    const existing = {
      ...mockMessage,
      id: 'ch-before',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:00:00Z',
    };
    const missed = {
      ...mockMessage2,
      id: 'ch-missed',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:01:00Z',
    };
    useChannelStore.setState({ activeChannelId: 'channel-1', currentServerId: 'server-1' });
    messagePages = [{ messages: [existing] }, { messages: [missed, existing] }];
    // servers + friend-requests endpoints 500 during recovery; messages still route.
    mockApiFetch.mockImplementation((url: unknown) => {
      const u = String(url);
      if (isMessagesUrl(u)) {
        return Promise.resolve(jsonResponse(messagePages.shift() ?? { messages: [] }));
      }
      if (u.startsWith('/api/v1/servers') || u.startsWith('/api/v1/friends/requests')) {
        return Promise.resolve({ ok: false, status: 500, json: async () => ({ error: 'boom' }) });
      }
      return Promise.resolve(jsonResponse({}));
    });

    const { result } = renderHook(() => {
      useMessageFetch('channel-1', { type: 'channel' });
      return useConnectionRecovery(mockWsService as never, validateEpochs);
    });

    await vi.waitFor(() => expect(messageFetchCount()).toBe(1));

    act(() => {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
    });

    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));
    // The failed membership refresh does not block the message backfill — the
    // dispatch is fail-safe — and it does not corrupt the seeded server list.
    await vi.waitFor(() => {
      expect(messageFetchCount()).toBe(2);
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((message) => message.id)
      ).toEqual(['ch-before', 'ch-missed']);
    });
    expect(useServerStore.getState().servers.map((s) => s.id)).toEqual(['server-1']);
  });

  it('backfills missed DM messages on Recovery-A (type: dm)', async () => {
    const existing = {
      ...mockMessage,
      id: 'dm-before',
      channel_id: 'dm-1',
      created_at: '2025-01-01T12:00:00Z',
    };
    const missed = {
      ...mockMessage2,
      id: 'dm-missed',
      channel_id: 'dm-1',
      created_at: '2025-01-01T12:01:00Z',
    };
    useChannelStore.setState({ activeChannelId: 'dm-1', currentServerId: null });
    messagePages = [{ messages: [existing] }, { messages: [missed, existing] }];

    const { result } = renderHook(() => {
      useMessageFetch('dm-1', { type: 'dm' });
      return useConnectionRecovery(mockWsService as never, validateEpochs);
    });

    await vi.waitFor(() => {
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('dm-1')
          ?.map((message) => message.id)
      ).toEqual(['dm-before']);
    });
    expect(messageFetchCount()).toBe(1);

    act(() => {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
    });

    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));
    await vi.waitFor(() => {
      expect(messageFetchCount()).toBe(2);
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('dm-1')
          ?.map((message) => message.id)
      ).toEqual(['dm-before', 'dm-missed']);
    });
  });

  it('fetchServers under a superseded session guard does not clobber the store (#2329 session-safety)', async () => {
    vi.mocked(isHydrationLifecycleCurrent).mockReturnValue(false);
    useServerStore.setState({
      servers: [{ id: 'server-1', name: 'Current account' }] as never,
      activeServerId: 'server-1',
    });
    // The in-flight fetch belongs to a superseded session; its list must not land.
    serversResponse = [{ id: 'server-other', name: 'Other account' }];

    await useServerStore.getState().fetchServers(staleGuard as never);

    expect(useServerStore.getState().servers.map((s) => s.id)).toEqual(['server-1']);
  });

  it('fetchRequests under a superseded session guard does not clobber the store (#2329 session-safety)', async () => {
    vi.mocked(isHydrationLifecycleCurrent).mockReturnValue(false);
    useFriendStore.setState({ pendingRequests: [] } as never);
    requestsResponse = {
      requests: [
        {
          id: 'stale-req',
          from_user_id: 'other',
          from_username: 'other',
          from_display_name: 'Other',
          from_avatar_url: null,
          to_user_id: 'other-me',
          to_username: 'other-me',
          to_display_name: 'Other Me',
          to_avatar_url: null,
          direction: 'incoming',
          created_at: '2025-01-01T12:00:00Z',
        },
      ],
    };

    await useFriendStore.getState().fetchRequests(staleGuard as never);

    expect(useFriendStore.getState().pendingRequests).toEqual([]);
  });

  it('does not backfill messages until the membership refresh resolves (#2358 ordering)', async () => {
    const existing = {
      ...mockMessage,
      id: 'ord-before',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:00:00Z',
    };
    const missed = {
      ...mockMessage2,
      id: 'ord-missed',
      channel_id: 'channel-1',
      created_at: '2025-01-01T12:01:00Z',
    };
    useChannelStore.setState({ activeChannelId: 'channel-1', currentServerId: 'server-1' });
    messagePages = [{ messages: [existing] }, { messages: [missed, existing] }];

    // Hold the /servers response open so the membership refresh stays in flight.
    let releaseServers!: () => void;
    const serversGate = new Promise<void>((resolve) => {
      releaseServers = resolve;
    });
    mockApiFetch.mockImplementation((url: unknown) => {
      const u = String(url);
      if (isMessagesUrl(u)) {
        return Promise.resolve(jsonResponse(messagePages.shift() ?? { messages: [] }));
      }
      if (u.startsWith('/api/v1/servers')) {
        return serversGate.then(() => jsonResponse({ servers: serversResponse }));
      }
      if (u.startsWith('/api/v1/friends/requests')) {
        return Promise.resolve(jsonResponse(requestsResponse));
      }
      return Promise.resolve(jsonResponse({}));
    });

    const { result } = renderHook(() => {
      useMessageFetch('channel-1', { type: 'channel' });
      return useConnectionRecovery(mockWsService as never, validateEpochs);
    });

    await vi.waitFor(() => expect(messageFetchCount()).toBe(1)); // mount fetch only

    act(() => {
      useConnectionStore.getState().enterRecoveryA();
      result.current('CONNECTED' as never);
    });

    await vi.waitFor(() => expect(hydrateCalls).toHaveBeenCalledTimes(1));
    // The recovery handler is now blocked on the in-flight membership refresh, so
    // `connection-recovered` has NOT been dispatched — the message backfill must
    // not have fired. Let microtasks drain, then assert the count is still 1.
    await Promise.resolve();
    await Promise.resolve();
    expect(messageFetchCount()).toBe(1);

    // Once memberships settle, the ordered message backfill runs.
    releaseServers();
    await vi.waitFor(() => {
      expect(messageFetchCount()).toBe(2);
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.map((message) => message.id)
      ).toEqual(['ord-before', 'ord-missed']);
    });
  });
});
