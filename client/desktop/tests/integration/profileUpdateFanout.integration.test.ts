import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
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

describe('Profile update fan-out (integration)', () => {
  it('profile_updated propagates to all 4 stores', () => {
    useUserStore.setState({ user: mockUser as never });

    // Seed user-2 in all stores
    useMemberStore.getState().addMember({
      user_id: 'user-2',
      username: 'oldname',
      role: 'member',
      joined_at: '2025-01-01T00:00:00Z',
      roles: [],
    });

    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'oldname' }],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    useFriendStore.getState().addFriend({
      id: 'friend-1',
      userId: 'user-2',
      username: 'oldname',
      status: 'online',
    });

    useVoiceStore.getState().addParticipant({
      userId: 'user-2',
      username: 'oldname',
      isMuted: false,
      isDeafened: false,
      serverMuted: false,
      serverDeafened: false,
      isVideoOn: false,
      isScreenSharing: false,
      isSpeaking: false,
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('profile_updated');
    expect(handler).toBeDefined();

    act(() => {
      handler!({
        type: 'profile_updated',
        data: {
          user_id: 'user-2',
          username: 'newname',
          display_name: 'New Display',
          avatar_url: 'https://example.com/new.png',
        },
      });
    });

    // memberStore: username updated
    const member = useMemberStore.getState().members.find((m) => m.user_id === 'user-2');
    expect(member?.username).toBe('newname');
    expect(member?.display_name).toBe('New Display');

    // dmStore: participant username and displayName updated
    const conv = useDMStore.getState().conversations[0];
    expect(conv.participants[0].username).toBe('newname');
    expect(conv.participants[0].displayName).toBe('New Display');

    // friendStore: username and displayName updated
    const friend = useFriendStore.getState().friends.find((f) => f.userId === 'user-2');
    expect(friend?.username).toBe('newname');
    expect(friend?.displayName).toBe('New Display');

    // voiceStore: active participant updated
    const vp = useVoiceStore.getState().participants['user-2'];
    expect(vp.username).toBe('newname');
    expect(vp.displayName).toBe('New Display');
  });

  it('profile_updated for user not in voice store is a no-op for voice', () => {
    useUserStore.setState({ user: mockUser as never });

    useMemberStore.getState().addMember({
      user_id: 'user-2',
      username: 'oldname',
      role: 'member',
      joined_at: '2025-01-01T00:00:00Z',
      roles: [],
    });

    // user-2 not in voice store
    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('profile_updated');
    act(() => {
      handler!({
        type: 'profile_updated',
        data: {
          user_id: 'user-2',
          username: 'newname',
          display_name: 'New Display',
          avatar_url: 'https://example.com/new.png',
        },
      });
    });

    // memberStore still updated
    const member = useMemberStore.getState().members.find((m) => m.user_id === 'user-2');
    expect(member?.username).toBe('newname');

    // voiceStore participants unchanged (user-2 not present)
    expect(useVoiceStore.getState().participants['user-2']).toBeUndefined();
  });

  it('profile_updated with avatar_url propagates to all stores', () => {
    useUserStore.setState({ user: mockUser as never });

    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [
            { userId: 'user-2', username: 'oldname', avatarUrl: 'https://example.com/old.png' },
          ],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    useFriendStore.getState().addFriend({
      id: 'friend-1',
      userId: 'user-2',
      username: 'oldname',
      status: 'online',
      avatarUrl: 'https://example.com/old.png',
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('profile_updated');
    act(() => {
      handler!({
        type: 'profile_updated',
        data: {
          user_id: 'user-2',
          username: 'oldname',
          display_name: undefined,
          avatar_url: 'https://example.com/new.png',
        },
      });
    });

    // dmStore avatar updated
    const conv = useDMStore.getState().conversations[0];
    expect(conv.participants[0].avatarUrl).toBe('https://example.com/new.png');

    // friendStore avatar updated
    const friend = useFriendStore.getState().friends.find((f) => f.userId === 'user-2');
    expect(friend?.avatarUrl).toBe('https://example.com/new.png');
  });
});
