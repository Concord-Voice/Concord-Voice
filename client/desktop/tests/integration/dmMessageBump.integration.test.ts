import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { resetAllStores } from '../helpers/store-helpers';
import { mockUser } from '../mocks/fixtures';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: { isInitialized: false },
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
vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: { fetchAndApply: vi.fn() },
}));
vi.mock('@/renderer/services/searchService', () => ({
  indexMessage: vi.fn(),
}));
vi.mock('@/renderer/services/desktopNotificationService', () => ({
  desktopNotificationService: {
    shouldNotify: vi.fn().mockReturnValue(false),
    notify: vi.fn(),
    incrementBadge: vi.fn(),
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
});

describe('DM message bump (integration)', () => {
  it('dm_message reorders conversation list by timestamp', () => {
    useUserStore.setState({ user: mockUser as never });

    // Seed two conversations — conv-2 is first (newer last message)
    useDMStore.setState({
      conversations: [
        {
          id: 'conv-2',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'user2' }],
          lastMessage: {
            content: 'newer',
            userId: 'user-2',
            username: 'user2',
            createdAt: '2025-01-02T00:00:00Z',
          },
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-3', username: 'user3' }],
          lastMessage: {
            content: 'older',
            userId: 'user-3',
            username: 'user3',
            createdAt: '2025-01-01T00:00:00Z',
          },
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('dm_message');
    expect(handler).toBeDefined();

    act(() => {
      handler!({
        type: 'dm_message',
        data: {
          conversation_id: 'conv-1',
          id: 'msg-99',
          user_id: 'user-3',
          username: 'user3',
          content: 'newest message',
          created_at: '2025-01-03T00:00:00Z',
        },
      });
    });

    // conv-1 should now be first (newest message)
    const convs = useDMStore.getState().conversations;
    expect(convs[0].id).toBe('conv-1');
    expect(convs[0].lastMessage?.content).toBe('newest message');
  });

  it('dm_message from self does not bump conversation', () => {
    useUserStore.setState({ user: mockUser as never });

    useDMStore.setState({
      conversations: [
        {
          id: 'conv-2',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'user2' }],
          lastMessage: {
            content: 'newer',
            userId: 'user-2',
            username: 'user2',
            createdAt: '2025-01-02T00:00:00Z',
          },
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-3', username: 'user3' }],
          lastMessage: {
            content: 'older',
            userId: 'user-3',
            username: 'user3',
            createdAt: '2025-01-01T00:00:00Z',
          },
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('dm_message');
    act(() => {
      handler!({
        type: 'dm_message',
        data: {
          conversation_id: 'conv-1',
          id: 'msg-99',
          // Self message — user_id matches the current user
          user_id: 'user-1',
          username: 'testuser',
          content: 'self message',
          created_at: '2025-01-03T00:00:00Z',
        },
      });
    });

    // conv-2 should still be first since self messages don't bump
    const convs = useDMStore.getState().conversations;
    expect(convs[0].id).toBe('conv-2');
  });
});
