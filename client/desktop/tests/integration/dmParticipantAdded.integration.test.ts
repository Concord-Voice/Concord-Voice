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

const mockApiFetch = vi.fn().mockResolvedValue({
  ok: true,
  json: () => Promise.resolve({ participants: [] }),
});
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
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
  mockApiFetch.mockClear();
  useAuthStore.getState().setAccessToken('mock-token');
  useUserStore.setState({ user: mockUser as never });
});

describe('DM participant added — local mutation (integration)', () => {
  it('adds participant locally without fetching conversations', () => {
    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: true,
          isPersonal: false,
          name: 'Group',
          participants: [{ userId: 'user-1', username: 'testuser' }],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('dm_participant_added');
    expect(handler).toBeDefined();

    act(() => {
      handler!({
        type: 'dm_participant_added',
        data: {
          conversation_id: 'conv-1',
          user_id: 'user-3',
          username: 'newuser',
          display_name: 'New User',
        },
      });
    });

    // Participant added locally
    const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-1');
    expect(conv?.participants).toHaveLength(2);
    expect(conv?.participants[1].username).toBe('newuser');
    expect(conv?.participants[1].displayName).toBe('New User');

    // No full conversations fetch — only voice participant fetches use /participants
    const conversationFetchCalls = mockApiFetch.mock.calls.filter(
      (call: unknown[]) => call[0] === '/api/v1/dm/conversations'
    );
    expect(conversationFetchCalls).toHaveLength(0);
  });

  it('fetches conversations when the conversation is not in state', () => {
    // No conversations seeded — conversation unknown
    useDMStore.setState({ conversations: [] } as never);

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('dm_participant_added');

    act(() => {
      handler!({
        type: 'dm_participant_added',
        data: {
          conversation_id: 'unknown-conv',
          user_id: 'user-3',
          username: 'newuser',
          display_name: 'New User',
        },
      });
    });

    // fetchConversations is called when conversation not found
    const convFetchCalls = mockApiFetch.mock.calls.filter(
      (call: unknown[]) =>
        typeof call[0] === 'string' && (call[0] as string).includes('/dm/conversations')
    );
    expect(convFetchCalls.length).toBeGreaterThan(0);
  });

  it('does not add duplicate participant', () => {
    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: true,
          isPersonal: false,
          name: 'Group',
          participants: [
            { userId: 'user-1', username: 'testuser' },
            { userId: 'user-3', username: 'alreadyhere' },
          ],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('dm_participant_added');
    act(() => {
      handler!({
        type: 'dm_participant_added',
        data: {
          conversation_id: 'conv-1',
          user_id: 'user-3',
          username: 'alreadyhere',
          display_name: 'Already Here',
        },
      });
    });

    // No duplicate added
    const conv = useDMStore.getState().conversations.find((c) => c.id === 'conv-1');
    expect(conv?.participants).toHaveLength(2);
  });
});
