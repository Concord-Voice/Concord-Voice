/**
 * Purge ↔ local search index (#1352 wire events, #1354 UI, #1741 invariant).
 *
 * A purge is irreversible deletion, so purged plaintext must not survive
 * anywhere locally. The renderer keeps a decrypted MiniSearch index, so the
 * purge handlers pair `clearMessages` with `removeScope` exactly as the
 * canonical scope-loss paths in channelStore/dmStore do.
 *
 * This suite deliberately uses the REAL searchService: a mocked one can only
 * prove a call was made, and the earlier gap was invisible precisely because
 * the sibling suite stubs the module.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import {
  beginSearchBackfill,
  canIndexBackfillMessage,
  clearIndex,
  indexMessage,
  searchMessages,
  subscribeSearchScopeInvalidations,
} from '@/renderer/services/searchService';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: vi.fn().mockResolvedValue('plaintext'),
    decryptForChannelWithVersion: vi.fn().mockResolvedValue('plaintext'),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({ speak: vi.fn() }));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
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

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
}));

import { useWebSocketMessages } from '@/renderer/hooks/useWebSocketMessages';

// eslint-disable-next-line @typescript-eslint/no-explicit-any
type AnyHandler = (...args: any[]) => void;

function createMockWsService() {
  const handlers = new Map<string, AnyHandler>();
  return {
    handlers,
    on: vi.fn((type: string, handler: AnyHandler) => {
      handlers.set(type, handler);
      return () => handlers.delete(type);
    }),
    onConnectionChange: vi.fn(() => () => {}),
    disconnect: vi.fn(),
  };
}

function requireHandler(ws: ReturnType<typeof createMockWsService>, eventName: string): AnyHandler {
  const handler = ws.handlers.get(eventName);
  if (!handler) throw new Error(`missing ${eventName} handler`);
  return handler;
}

const PURGED_BY = '22222222-2222-4222-8222-222222222222';
const CHANNEL_ID = '11111111-1111-4111-8111-111111111111';
const CONVERSATION_ID = '44444444-4444-4444-8444-444444444444';
const MESSAGE_ID = '55555555-5555-4555-8555-555555555555';
const SERVER_ID = '77777777-7777-4777-8777-777777777777';
const OTHER_SERVER_ID = '88888888-8888-4888-8888-888888888888';
const CHANNEL_B_ID = '33333333-3333-4333-8333-333333333333';
const OTHER_SERVER_CHANNEL_ID = 'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa';
const MESSAGE_B_ID = 'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb';
const MESSAGE_C_ID = 'cccccccc-cccc-4ccc-8ccc-cccccccccccc';

function channelPurgedEvent(channelId: string) {
  return {
    type: 'channel_purged',
    data: { channel_id: channelId, purged_by: PURGED_BY, deleted_count: 4, range: '7d' },
  };
}

function dmPurgedEvent(conversationId: string) {
  return {
    type: 'dm_purged',
    data: { conversation_id: conversationId, purged_by: PURGED_BY, deleted_count: 4, range: '7d' },
  };
}

// No count on the wire: a server purge's per-channel deleted_count is 0 by design.
function serverPurgedEvent(serverId: string) {
  return {
    type: 'server_purged',
    data: { server_id: serverId, purged_by: PURGED_BY, range: 'all' },
  };
}

function addChannel(channelId: string, serverId: string) {
  useChannelStore.getState().addChannel({ ...mockChannel, id: channelId, server_id: serverId });
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  clearIndex();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.getState().setUser({ id: 'user-1', username: 'testuser' });
});

afterEach(() => {
  clearIndex();
  vi.restoreAllMocks();
});

describe('purge removes decrypted plaintext from the local search index', () => {
  it('channel_purged makes purged plaintext unsearchable', () => {
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);
    expect(searchMessages('confidential', CHANNEL_ID)).toContain(MESSAGE_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'channel_purged')(channelPurgedEvent(CHANNEL_ID));
    });

    expect(searchMessages('confidential', CHANNEL_ID)).toEqual([]);
  });

  it('dm_purged makes purged plaintext unsearchable', () => {
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CONVERSATION_ID);
    expect(searchMessages('confidential', CONVERSATION_ID)).toContain(MESSAGE_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'dm_purged')(dmPurgedEvent(CONVERSATION_ID));
    });

    expect(searchMessages('confidential', CONVERSATION_ID)).toEqual([]);
  });

  it('leaves an untouched scope searchable', () => {
    const otherScope = '99999999-9999-4999-8999-999999999999';
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);
    indexMessage('66666666-6666-4666-8666-666666666666', 'unrelated chatter', otherScope);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'channel_purged')(channelPurgedEvent(CHANNEL_ID));
    });

    expect(searchMessages('unrelated', otherScope)).toHaveLength(1);
  });

  // A backfill that started before the purge holds decrypted rows of its own;
  // without the scope fence it would RE-index purged plaintext afterwards.
  it('fences an in-flight backfill so it cannot re-index purged plaintext', () => {
    const backfill = beginSearchBackfill([CHANNEL_ID]);
    expect(canIndexBackfillMessage(backfill, MESSAGE_ID, CHANNEL_ID)).toBe(true);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'channel_purged')(channelPurgedEvent(CHANNEL_ID));
    });

    expect(canIndexBackfillMessage(backfill, MESSAGE_ID, CHANNEL_ID)).toBe(false);
    backfill.close();
  });

  // Positive control for the fence above: `false` alone would also be the answer
  // if the purge had disabled backfill indexing wholesale, so prove a backfill
  // over an UNPURGED scope still indexes after the same event.
  it('leaves a backfill over an unpurged scope able to index', () => {
    const backfill = beginSearchBackfill([CHANNEL_ID, CHANNEL_B_ID]);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'channel_purged')(channelPurgedEvent(CHANNEL_ID));
    });

    expect(canIndexBackfillMessage(backfill, MESSAGE_ID, CHANNEL_ID)).toBe(false);
    expect(canIndexBackfillMessage(backfill, MESSAGE_B_ID, CHANNEL_B_ID)).toBe(true);
    backfill.close();
  });

  // Copies already emitted into an open search pane are separate objects the
  // index no longer owns; the scope invalidation is what dismisses them.
  it('notifies open search surfaces that the scope was invalidated', () => {
    const invalidated: Array<string | null> = [];
    const unsubscribe = subscribeSearchScopeInvalidations((scope) => invalidated.push(scope));

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'dm_purged')(dmPurgedEvent(CONVERSATION_ID));
    });

    unsubscribe();
    expect(invalidated).toContain(CONVERSATION_ID);
  });
});

// The purge ACTOR is the case the echo cannot cover. `channel_purged` reaches
// only subscribers of the purged channel — in practice the one the client has
// mounted — so an actor purging a channel they are not viewing receives no
// WebSocket event for their own purge, and their MiniSearch index would keep
// serving the plaintext they just destroyed. `PurgeMessagesModal` dispatches
// `messages-purged` locally on every terminal outcome; the handler lane listens
// to that event so the clear runs either way (#1741).
describe('the actor’s own purge clears their index without any echo', () => {
  it('clears the dispatched scope with no WebSocket event', () => {
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_B_ID);
    expect(searchMessages('confidential', CHANNEL_B_ID)).toContain(MESSAGE_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: CHANNEL_B_ID } })
      );
    });

    expect(searchMessages('confidential', CHANNEL_B_ID)).toEqual([]);
  });

  it('leaves every other scope searchable', () => {
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_B_ID);
    indexMessage(MESSAGE_C_ID, 'confidential unrelated chatter', CHANNEL_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: CHANNEL_B_ID } })
      );
    });

    expect(searchMessages('confidential', CHANNEL_ID)).toEqual([MESSAGE_C_ID]);
  });

  // A SERVER purge is the sharpest form of the same gap. The actor's dispatch
  // carries a null scope (a server id can never name a mounted channel), so the
  // clear cannot key on it — it keys on `serverId`, which travels alongside.
  // Waiting for `server_purged` instead would fail in precisely the case that
  // needs it: a transport rejection means the WebSocket is plausibly down too,
  // so the echo may never arrive.
  it('clears every known channel of the server from the actor’s dispatch alone', () => {
    addChannel(CHANNEL_ID, SERVER_ID);
    useChannelStore.setState((state) => ({
      channelIdsByServer: { ...state.channelIdsByServer, [SERVER_ID]: [CHANNEL_B_ID] },
    }));
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);
    indexMessage(MESSAGE_B_ID, 'confidential merger memo', CHANNEL_B_ID);
    indexMessage(MESSAGE_C_ID, 'confidential unrelated chatter', OTHER_SERVER_CHANNEL_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: null, serverId: SERVER_ID } })
      );
    });

    // Both enumeration sources, and no collateral damage to another server.
    expect(searchMessages('confidential', CHANNEL_ID)).toEqual([]);
    expect(searchMessages('confidential', CHANNEL_B_ID)).toEqual([]);
    expect(searchMessages('confidential', OTHER_SERVER_CHANNEL_ID)).toEqual([MESSAGE_C_ID]);
  });

  // Negative control: a bare null scope still means "refetch what is mounted"
  // and nothing more. Only an explicit `serverId` authorizes a server-wide clear.
  it('does not clear a server when the dispatch carries a bare null scope', () => {
    addChannel(CHANNEL_ID, SERVER_ID);
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId: null } }));
    });

    expect(searchMessages('confidential', CHANNEL_ID)).toContain(MESSAGE_ID);
  });
});

// `channel_purged` reaches only the channel a client is subscribed to — the one
// it has mounted — so before `server_purged` every OTHER purged channel kept its
// decrypted plaintext locally and stayed reachable through multi-scope search.
describe('server_purged clears every known channel of the server', () => {
  it('makes plaintext from MULTIPLE channels of that server unsearchable', () => {
    addChannel(CHANNEL_ID, SERVER_ID);
    addChannel(CHANNEL_B_ID, SERVER_ID);
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);
    indexMessage(MESSAGE_B_ID, 'confidential merger memo', CHANNEL_B_ID);
    expect(searchMessages('confidential', CHANNEL_ID)).toContain(MESSAGE_ID);
    expect(searchMessages('confidential', CHANNEL_B_ID)).toContain(MESSAGE_B_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'server_purged')(serverPurgedEvent(SERVER_ID));
    });

    expect(searchMessages('confidential', CHANNEL_ID)).toEqual([]);
    expect(searchMessages('confidential', CHANNEL_B_ID)).toEqual([]);
  });

  // The loaded `channels` list holds only the current server's channels; ids for
  // servers the user has visited survive in `channelIdsByServer`. Enumerating one
  // source alone would leave the other's plaintext indexed.
  it('clears a channel known only from channelIdsByServer', () => {
    useChannelStore.setState((state) => ({
      channelIdsByServer: { ...state.channelIdsByServer, [SERVER_ID]: [CHANNEL_B_ID] },
    }));
    indexMessage(MESSAGE_B_ID, 'confidential merger memo', CHANNEL_B_ID);
    expect(useChannelStore.getState().channels.some((c) => c.id === CHANNEL_B_ID)).toBe(false);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'server_purged')(serverPurgedEvent(SERVER_ID));
    });

    expect(searchMessages('confidential', CHANNEL_B_ID)).toEqual([]);
  });

  it('clears a channel present only in the loaded channel list', () => {
    useChannelStore.setState({
      channels: [{ ...mockChannel, id: CHANNEL_B_ID, server_id: SERVER_ID }],
      channelIdsByServer: {},
    });
    indexMessage(MESSAGE_B_ID, 'confidential merger memo', CHANNEL_B_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'server_purged')(serverPurgedEvent(SERVER_ID));
    });

    expect(searchMessages('confidential', CHANNEL_B_ID)).toEqual([]);
  });

  it('leaves a channel of a DIFFERENT server searchable', () => {
    addChannel(CHANNEL_ID, SERVER_ID);
    addChannel(OTHER_SERVER_CHANNEL_ID, OTHER_SERVER_ID);
    indexMessage(MESSAGE_ID, 'confidential quarterly numbers', CHANNEL_ID);
    indexMessage(MESSAGE_C_ID, 'confidential unrelated chatter', OTHER_SERVER_CHANNEL_ID);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'server_purged')(serverPurgedEvent(SERVER_ID));
    });

    expect(searchMessages('confidential', CHANNEL_ID)).toEqual([]);
    expect(searchMessages('confidential', OTHER_SERVER_CHANNEL_ID)).toEqual([MESSAGE_C_ID]);
  });

  // One dispatch, not one per channel: `useMessageFetch` reads a null scopeId as
  // "refetch whatever is mounted", so the mounted scope refetches even if it was
  // not enumerable from the local channel list. `serverId` rides along and is
  // what the clearing lane keys on — the echo and the actor's own dispatch emit
  // the identical shape, which is what makes them share one enumeration.
  it('dispatches exactly one messages-purged, null scope plus the server id', () => {
    addChannel(CHANNEL_ID, SERVER_ID);
    addChannel(CHANNEL_B_ID, SERVER_ID);
    const details: unknown[] = [];
    const listener = (e: Event) => {
      details.push((e as CustomEvent).detail);
    };
    globalThis.addEventListener('messages-purged', listener);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'server_purged')(serverPurgedEvent(SERVER_ID));
    });

    globalThis.removeEventListener('messages-purged', listener);
    expect(details).toEqual([{ scopeId: null, serverId: SERVER_ID }]);
  });
});
