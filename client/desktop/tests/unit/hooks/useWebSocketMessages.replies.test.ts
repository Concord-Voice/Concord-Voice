import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel } from '../../mocks/fixtures';

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

describe('useWebSocketMessages — reply passthrough', () => {
  it('passes reply_to_id through to stored message', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    const handler = ws.handlers.get('message')!;
    act(() => {
      handler({
        type: 'message',
        data: {
          id: 'msg-reply-1',
          channel_id: 'channel-1',
          user_id: 'user-2',
          username: 'otheruser',
          content: 'This is a reply',
          reply_to_id: 'msg-original',
          created_at: '2025-01-01T12:00:00Z',
          updated_at: '2025-01-01T12:00:00Z',
        },
      });
    });

    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    const msg = messages?.find((m) => m.id === 'msg-reply-1');
    expect(msg).toBeDefined();
    expect(msg?.reply_to_id).toBe('msg-original');
  });

  it('passes replied_to object through to stored message', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    const repliedTo = {
      id: 'msg-original',
      user_id: 'user-1',
      username: 'testuser',
      display_name: 'Test User',
      content: 'Original message',
      key_version: 1,
    };

    const handler = ws.handlers.get('message')!;
    act(() => {
      handler({
        type: 'message',
        data: {
          id: 'msg-reply-2',
          channel_id: 'channel-1',
          user_id: 'user-2',
          username: 'otheruser',
          content: 'Reply with context',
          reply_to_id: 'msg-original',
          replied_to: repliedTo,
          created_at: '2025-01-01T12:00:00Z',
          updated_at: '2025-01-01T12:00:00Z',
        },
      });
    });

    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    const msg = messages?.find((m) => m.id === 'msg-reply-2');
    expect(msg?.replied_to).toBeDefined();
    expect(msg?.replied_to?.id).toBe('msg-original');
    expect(msg?.replied_to?.content).toBe('Original message');
  });

  it('stores message without reply fields when not a reply', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    const handler = ws.handlers.get('message')!;
    act(() => {
      handler({
        type: 'message',
        data: {
          id: 'msg-normal',
          channel_id: 'channel-1',
          user_id: 'user-2',
          username: 'otheruser',
          content: 'Just a normal message',
          created_at: '2025-01-01T12:00:00Z',
          updated_at: '2025-01-01T12:00:00Z',
        },
      });
    });

    const messages = useChatStore.getState().messagesByChannel.get('channel-1');
    const msg = messages?.find((m) => m.id === 'msg-normal');
    expect(msg).toBeDefined();
    expect(msg?.reply_to_id).toBeUndefined();
    expect(msg?.replied_to).toBeUndefined();
  });
});
