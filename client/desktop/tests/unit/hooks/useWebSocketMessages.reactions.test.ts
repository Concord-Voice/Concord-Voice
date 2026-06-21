import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockMessage } from '../../mocks/fixtures';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    decryptMessage: vi.fn((content: string) => Promise.resolve(content)),
    hasKey: vi.fn().mockReturnValue(false),
  },
}));

vi.mock('@/renderer/services/ttsService', () => ({ speak: vi.fn() }));
vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn().mockResolvedValue({
    ok: true,
    json: () => Promise.resolve({ participants: [] }),
  }),
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
  };
}

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
  useChannelStore.getState().addChannel(mockChannel);
  useChatStore.setState({ isConnected: true });
  useUserStore.setState({
    user: { id: 'user-1', username: 'testuser', email: 'test@test.com' } as never,
  });
});

describe('useWebSocketMessages — reaction handlers', () => {
  it('registers message_reaction_added handler', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.has('message_reaction_added')).toBe(true);
  });

  it('registers message_reaction_removed handler', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.has('message_reaction_removed')).toBe(true);
  });

  it('adds reaction to message on message_reaction_added', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    // Seed a message in the store
    useChatStore.getState().addMessage('channel-1', { ...mockMessage, reactions: [] });

    const handler = ws.handlers.get('message_reaction_added')!;
    act(() => {
      handler({
        type: 'message_reaction_added',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
          emoji: '👍',
          user_id: 'user-1',
          reaction_summary: {
            emoji: '👍',
            count: 1,
            users: [{ user_id: 'user-1', username: 'testuser' }],
            me: false, // server sends generic me flag; handler recalculates
          },
        },
      });
    });

    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    const msg = messages?.find((m) => m.id === 'msg-1');
    expect(msg?.reactions).toHaveLength(1);
    expect(msg?.reactions?.[0].emoji).toBe('👍');
    expect(msg?.reactions?.[0].count).toBe(1);
    expect(msg?.reactions?.[0].me).toBe(true); // recalculated: user-1 is self
  });

  it('updates existing reaction count on message_reaction_added', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    useChatStore.getState().addMessage('channel-1', {
      ...mockMessage,
      reactions: [
        { emoji: '👍', count: 1, users: [{ user_id: 'user-2', username: 'other' }], me: false },
      ],
    });

    const handler = ws.handlers.get('message_reaction_added')!;
    act(() => {
      handler({
        type: 'message_reaction_added',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
          emoji: '👍',
          user_id: 'user-1',
          reaction_summary: {
            emoji: '👍',
            count: 2,
            users: [
              { user_id: 'user-2', username: 'other' },
              { user_id: 'user-1', username: 'testuser' },
            ],
            me: false,
          },
        },
      });
    });

    const msg = useChatStore
      .getState()
      .messagesByChannel.get('channel-1')
      ?.find((m) => m.id === 'msg-1');
    expect(msg?.reactions?.[0].count).toBe(2);
    expect(msg?.reactions?.[0].me).toBe(true);
  });

  it('removes reaction group on message_reaction_removed with null summary', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    useChatStore.getState().addMessage('channel-1', {
      ...mockMessage,
      reactions: [
        { emoji: '👍', count: 1, users: [{ user_id: 'user-1', username: 'testuser' }], me: true },
      ],
    });

    const handler = ws.handlers.get('message_reaction_removed')!;
    act(() => {
      handler({
        type: 'message_reaction_removed',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
          emoji: '👍',
          user_id: 'user-1',
          reaction_summary: null,
        },
      });
    });

    const msg = useChatStore
      .getState()
      .messagesByChannel.get('channel-1')
      ?.find((m) => m.id === 'msg-1');
    expect(msg?.reactions).toHaveLength(0);
  });

  it('decrements reaction count on message_reaction_removed', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    useChatStore.getState().addMessage('channel-1', {
      ...mockMessage,
      reactions: [
        {
          emoji: '👍',
          count: 2,
          users: [
            { user_id: 'user-1', username: 'testuser' },
            { user_id: 'user-2', username: 'other' },
          ],
          me: true,
        },
      ],
    });

    const handler = ws.handlers.get('message_reaction_removed')!;
    act(() => {
      handler({
        type: 'message_reaction_removed',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
          emoji: '👍',
          user_id: 'user-1',
          reaction_summary: {
            emoji: '👍',
            count: 1,
            users: [{ user_id: 'user-2', username: 'other' }],
            me: false,
          },
        },
      });
    });

    const msg = useChatStore
      .getState()
      .messagesByChannel.get('channel-1')
      ?.find((m) => m.id === 'msg-1');
    expect(msg?.reactions?.[0].count).toBe(1);
    expect(msg?.reactions?.[0].me).toBe(false);
  });

  it('ignores reaction event for unknown message', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    // No message in store
    const handler = ws.handlers.get('message_reaction_added')!;
    act(() => {
      handler({
        type: 'message_reaction_added',
        data: {
          channel_id: 'channel-1',
          message_id: 'nonexistent',
          emoji: '👍',
          user_id: 'user-1',
          reaction_summary: { emoji: '👍', count: 1, users: [], me: true },
        },
      });
    });

    // Should not crash — no messages in store
    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    expect(messages).toBeUndefined();
  });

  it('ignores reaction event with missing channel_id', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    const handler = ws.handlers.get('message_reaction_added')!;
    // Should not crash with missing data
    act(() => {
      handler({ type: 'message_reaction_added', data: {} });
    });

    // Store should remain unchanged — no channel-1 messages created
    expect(useChatStore.getState().messagesByChannel.size).toBe(0);
  });

  it('unsubscribes reaction handlers on unmount', () => {
    const ws = createMockWsService();
    const { unmount } = renderHook(() => useWebSocketMessages(ws as never));

    expect(ws.handlers.has('message_reaction_added')).toBe(true);
    expect(ws.handlers.has('message_reaction_removed')).toBe(true);

    unmount();

    expect(ws.handlers.has('message_reaction_added')).toBe(false);
    expect(ws.handlers.has('message_reaction_removed')).toBe(false);
  });
});
