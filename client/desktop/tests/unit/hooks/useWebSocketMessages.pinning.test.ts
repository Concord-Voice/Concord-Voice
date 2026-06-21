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
    isInitialized: false,
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

describe('useWebSocketMessages — pinning handlers', () => {
  it('registers message_pinned handler', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.has('message_pinned')).toBe(true);
  });

  it('registers message_unpinned handler', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));
    expect(ws.handlers.has('message_unpinned')).toBe(true);
  });

  it('updates message with pinned fields on message_pinned', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    useChatStore.getState().addMessage('channel-1', { ...mockMessage, reactions: [] });

    const handler = ws.handlers.get('message_pinned')!;
    act(() => {
      handler({
        type: 'message_pinned',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
          pinned_at: '2025-06-01T12:00:00Z',
          pinned_by: 'user-1',
        },
      });
    });

    const msg = useChatStore
      .getState()
      .messagesByChannel.get('channel-1')
      ?.find((m) => m.id === 'msg-1');
    expect(msg?.pinned_at).toBe('2025-06-01T12:00:00Z');
    expect(msg?.pinned_by).toBe('user-1');
  });

  it('clears pinned fields on message_unpinned', () => {
    const ws = createMockWsService();
    renderHook(() => useWebSocketMessages(ws as never));

    useChatStore.getState().addMessage('channel-1', {
      ...mockMessage,
      pinned_at: '2025-06-01T12:00:00Z',
      pinned_by: 'user-1',
    });

    const handler = ws.handlers.get('message_unpinned')!;
    act(() => {
      handler({
        type: 'message_unpinned',
        data: {
          channel_id: 'channel-1',
          message_id: 'msg-1',
        },
      });
    });

    const msg = useChatStore
      .getState()
      .messagesByChannel.get('channel-1')
      ?.find((m) => m.id === 'msg-1');
    expect(msg?.pinned_at).toBeUndefined();
    expect(msg?.pinned_by).toBeUndefined();
  });
});
