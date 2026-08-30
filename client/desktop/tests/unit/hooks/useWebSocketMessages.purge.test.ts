/**
 * Bulk message-purge handlers (#1352 wire events, #1354 UI).
 *
 * `channel_purged` / `dm_purged` are invalidation signals, never diffs: they
 * carry no message IDs, and a whole-server purge emits one `channel_purged`
 * PER CHANNEL with `deleted_count: 0` by design. Convergence is therefore
 * invalidate → clear → refetch, and the count is never branched on.
 */
import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockMessage } from '../../mocks/fixtures';

const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    invalidateChannelKey: vi.fn(),
    revokeChannelAccess: vi.fn(),
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
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

vi.mock('@/renderer/services/searchService', () => ({
  indexMessage: vi.fn(),
  removeMessage: vi.fn(),
  removeScope: vi.fn(),
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

function deferred<T>() {
  let resolvePromise: ((value: T) => void) | undefined;
  const promise = new Promise<T>((resolve) => {
    resolvePromise = resolve;
  });
  return {
    promise,
    resolve(value: T) {
      if (!resolvePromise) throw new Error('deferred promise was not initialized');
      resolvePromise(value);
    },
  };
}

const PURGED_BY = '22222222-2222-4222-8222-222222222222';

function dmPurgedEvent(conversationId: string) {
  return {
    type: 'dm_purged',
    data: { conversation_id: conversationId, purged_by: PURGED_BY, deleted_count: 2, range: '1d' },
  };
}

/** Seed one conversation carrying a decrypted preview, the second plaintext copy. */
function seedConversation(conversationId: string, preview: string) {
  useDMStore.setState({
    conversations: [
      {
        id: conversationId,
        isGroup: false,
        isPersonal: false,
        name: null,
        participants: [],
        lastMessage: {
          content: preview,
          userId: PURGED_BY,
          username: 'peer',
          createdAt: '2026-08-11T00:00:00.000Z',
        },
        unreadCount: 0,
        createdAt: '2026-08-11T00:00:00.000Z',
      },
    ],
  });
}

function findConversation(conversationId: string) {
  return useDMStore.getState().conversations.find((c) => c.id === conversationId);
}

beforeEach(() => {
  resetAllStores();
  vi.clearAllMocks();
  mockDecryptForChannel.mockImplementation((_channelId: string, content: string) =>
    Promise.resolve(content)
  );
  mockDecryptForChannelWithVersion.mockImplementation((_channelId: string, content: string) =>
    Promise.resolve(content)
  );
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.getState().setUser({ id: 'user-1', username: 'testuser' });
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('channel_purged handler', () => {
  it('clears the channel and dispatches messages-purged', () => {
    const channelId = '11111111-1111-4111-8111-111111111111';
    useChatStore.getState().setMessages(channelId, [{ ...mockMessage, channel_id: channelId }]);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = requireHandler(ws, 'channel_purged');
    const spy = vi.fn();
    globalThis.addEventListener('messages-purged', spy);

    act(() => {
      handler({
        type: 'channel_purged',
        data: {
          channel_id: channelId,
          purged_by: PURGED_BY,
          deleted_count: 3,
          range: '7d',
        },
      });
    });

    expect(useChatStore.getState().messagesByChannel.get(channelId)).toBeUndefined();
    expect(spy).toHaveBeenCalledOnce();
    expect((spy.mock.calls[0][0] as CustomEvent).detail).toEqual({ scopeId: channelId });

    globalThis.removeEventListener('messages-purged', spy);
  });

  it('treats a server-origin deleted_count of 0 as an invalidation, not a no-op', () => {
    const channelId = '33333333-3333-4333-8333-333333333333';
    useChatStore.getState().setMessages(channelId, [{ ...mockMessage, channel_id: channelId }]);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = requireHandler(ws, 'channel_purged');

    act(() => {
      handler({
        type: 'channel_purged',
        data: {
          channel_id: channelId,
          purged_by: PURGED_BY,
          deleted_count: 0, // a whole-server purge emits 0 per channel, by design
          range: 'all',
        },
      });
    });

    expect(useChatStore.getState().messagesByChannel.get(channelId)).toBeUndefined();
  });

  // #1741 content invariant: a live deletion must never be restorable by a
  // decrypt continuation that started before the purge landed.
  it('does not let a decrypt started before the purge overwrite refetched content', async () => {
    const channelId = '66666666-6666-4666-8666-666666666666';
    const messageId = '77777777-7777-4777-8777-777777777777';
    const pendingDecrypt = deferred<string>();
    mockDecryptForChannel.mockReturnValueOnce(pendingDecrypt.promise);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const messageHandler = requireHandler(ws, 'message');
    const purgeHandler = requireHandler(ws, 'channel_purged');

    act(() => {
      messageHandler({
        type: 'message',
        data: {
          id: messageId,
          channel_id: channelId,
          user_id: 'user-2',
          username: 'someone',
          content: 'ciphertext',
          created_at: '2025-01-01T12:00:00Z',
          updated_at: '2025-01-01T12:00:00Z',
        },
      });
    });

    act(() => {
      purgeHandler({
        type: 'channel_purged',
        data: {
          channel_id: channelId,
          purged_by: PURGED_BY,
          deleted_count: 1,
          range: '1h',
        },
      });
    });

    // The refetch the purge triggers re-establishes truth for this scope.
    useChatStore
      .getState()
      .setMessages(channelId, [
        { ...mockMessage, id: messageId, channel_id: channelId, content: 'authoritative refetch' },
      ]);

    await act(async () => {
      pendingDecrypt.resolve('stale plaintext');
      await pendingDecrypt.promise;
    });

    const message = useChatStore
      .getState()
      .messagesByChannel.get(channelId)
      ?.find((m) => m.id === messageId);
    expect(message?.content).toBe('authoritative refetch');
  });
});

describe('dm_purged handler', () => {
  it('clears the conversation scope and dispatches messages-purged', () => {
    const conversationId = '44444444-4444-4444-8444-444444444444';
    useChatStore
      .getState()
      .setMessages(conversationId, [{ ...mockMessage, channel_id: conversationId }]);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    const handler = requireHandler(ws, 'dm_purged');
    const spy = vi.fn();
    globalThis.addEventListener('messages-purged', spy);

    act(() => {
      handler({
        type: 'dm_purged',
        data: {
          conversation_id: conversationId,
          purged_by: PURGED_BY,
          deleted_count: 2,
          range: '1d',
        },
      });
    });

    expect(useChatStore.getState().messagesByChannel.get(conversationId)).toBeUndefined();
    expect((spy.mock.calls[0][0] as CustomEvent).detail).toEqual({ scopeId: conversationId });

    globalThis.removeEventListener('messages-purged', spy);
  });

  // The conversation-list preview is a SECOND copy of decrypted plaintext, held
  // on dmStore rather than chatStore. It is cleared in this lane, not in
  // ConversationList, so an unmounted list cannot leave a purged preview to
  // reappear on next mount.
  it('clears the conversation preview without ConversationList mounted', () => {
    const conversationId = '44444444-4444-4444-8444-444444444444';
    seedConversation(conversationId, 'confidential quarterly numbers');

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(ws, 'dm_purged')(dmPurgedEvent(conversationId));
    });

    expect(findConversation(conversationId)?.lastMessage).toBeNull();
  });
});

// The actor of a purge is NOT guaranteed to receive their own echo:
// `channel_purged` reaches only subscribers of the purged channel, so purging a
// channel the client is not viewing produces no WebSocket event for the actor at
// all. `PurgeMessagesModal` dispatches `messages-purged` locally on every
// terminal outcome, and this lane listens to that event — which is what makes the
// actor's own purge converge either way.
describe('locally dispatched purge (the actor has no echo)', () => {
  it('clears the scope with no WebSocket event', () => {
    const conversationId = '44444444-4444-4444-8444-444444444444';
    useChatStore
      .getState()
      .setMessages(conversationId, [{ ...mockMessage, channel_id: conversationId }]);
    seedConversation(conversationId, 'confidential quarterly numbers');

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: conversationId } })
      );
    });

    expect(useChatStore.getState().messagesByChannel.get(conversationId)).toBeUndefined();
    expect(findConversation(conversationId)?.lastMessage).toBeNull();
  });

  it('ignores the null server-wide scope, which carries no channel list', () => {
    const channelId = mockChannel.id;
    useChatStore.getState().setMessages(channelId, [{ ...mockMessage, channel_id: channelId }]);

    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId: null } }));
    });

    // Not a leak: a server purge reaches the actor as `server_purged`, which
    // enumerates the channels this event cannot name.
    expect(useChatStore.getState().messagesByChannel.get(channelId)).toHaveLength(1);
  });

  it('stops clearing once the hook unmounts', () => {
    const conversationId = '44444444-4444-4444-8444-444444444444';

    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));
    unmount();

    seedConversation(conversationId, 'confidential quarterly numbers');
    act(() => {
      globalThis.dispatchEvent(
        new CustomEvent('messages-purged', { detail: { scopeId: conversationId } })
      );
    });

    expect(findConversation(conversationId)?.lastMessage).not.toBeNull();
  });
});

// #1741 names placeholders explicitly: the scope invalidator makes the decrypt
// continuation return before `onSettled`, so a purge that does not settle the
// scope's placeholders leaves them orphaned until unmount — where the sweep
// then deletes any row that merely LOOKS like a placeholder (empty content,
// no decrypt failure, no pending keys), e.g. a refetched attachment-only row.
describe('purge placeholder settlement', () => {
  it('settles the purged scope so the unmount sweep cannot delete a refetched row', async () => {
    const channelId = '88888888-8888-4888-8888-888888888888';
    const messageId = '99999999-9999-4999-8999-999999999999';
    const pendingDecrypt = deferred<string>();
    mockDecryptForChannel.mockReturnValueOnce(pendingDecrypt.promise);

    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));

    act(() => {
      requireHandler(
        ws,
        'message'
      )({
        type: 'message',
        data: {
          id: messageId,
          channel_id: channelId,
          user_id: 'user-2',
          username: 'someone',
          content: 'ciphertext',
          created_at: '2025-01-01T12:00:00Z',
          updated_at: '2025-01-01T12:00:00Z',
        },
      });
    });

    act(() => {
      requireHandler(
        ws,
        'channel_purged'
      )({
        type: 'channel_purged',
        data: { channel_id: channelId, purged_by: PURGED_BY, deleted_count: 1, range: '1h' },
      });
    });

    // The refetch the purge triggers returns a surviving attachment-only row:
    // legitimately empty content, decrypted fine, no pending keys.
    useChatStore.getState().setMessages(channelId, [
      {
        ...mockMessage,
        id: messageId,
        channel_id: channelId,
        content: '',
        decryptFailed: false,
        pendingKeys: false,
      },
    ]);

    await act(async () => {
      pendingDecrypt.resolve('purged plaintext');
      await pendingDecrypt.promise;
    });

    unmount();

    const surviving = useChatStore.getState().messagesByChannel.get(channelId);
    expect(surviving?.map((message) => message.id)).toEqual([messageId]);
  });
});
