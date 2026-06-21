import { vi, describe, it, expect, beforeEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
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

describe('Presence fan-out (integration)', () => {
  it('presence event updates memberStore, dmStore, and friendStore', () => {
    useUserStore.setState({ user: mockUser as never });

    // Seed DM participant
    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'friend1', status: 'offline' }],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    // Seed friend
    useFriendStore.getState().addFriend({
      id: 'friend-1',
      userId: 'user-2',
      username: 'friend1',
      status: 'offline',
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('presence');
    expect(handler).toBeDefined();

    act(() => {
      handler!({ type: 'presence', data: { user_id: 'user-2', status: 'online' } });
    });

    // memberStore: userStatuses Map updated
    expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('online');

    // dmStore participant status updated
    const conv = useDMStore.getState().conversations[0];
    const participant = conv.participants.find((p) => p.userId === 'user-2');
    expect(participant?.status).toBe('online');

    // friendStore status updated
    const friend = useFriendStore.getState().friends.find((f) => f.userId === 'user-2');
    expect(friend?.status).toBe('online');
  });

  it('presence event to offline updates all three stores', () => {
    useUserStore.setState({ user: mockUser as never });

    useDMStore.setState({
      conversations: [
        {
          id: 'conv-1',
          isGroup: false,
          isPersonal: false,
          name: null,
          participants: [{ userId: 'user-2', username: 'friend1', status: 'online' }],
          lastMessage: null,
          unreadCount: 0,
          createdAt: '2025-01-01T00:00:00Z',
        },
      ],
    } as never);

    useFriendStore.getState().addFriend({
      id: 'friend-1',
      userId: 'user-2',
      username: 'friend1',
      status: 'online',
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('presence');
    act(() => {
      handler!({
        type: 'presence',
        data: { user_id: 'user-2', status: 'offline', timestamp: 1700000000 },
      });
    });

    expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('offline');

    const conv = useDMStore.getState().conversations[0];
    const participant = conv.participants.find((p) => p.userId === 'user-2');
    expect(participant?.status).toBe('offline');

    const friend = useFriendStore.getState().friends.find((f) => f.userId === 'user-2');
    expect(friend?.status).toBe('offline');
  });

  it('presence event for self user is ignored', () => {
    useUserStore.setState({ user: mockUser as never });

    useFriendStore.getState().addFriend({
      id: 'friend-self',
      userId: 'user-1',
      username: 'testuser',
      status: 'online',
    });

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('presence');
    act(() => {
      // Server broadcasts "offline" for the self user (invisible case)
      handler!({ type: 'presence', data: { user_id: 'user-1', status: 'offline' } });
    });

    // Self status in memberStore should not be overridden
    // (setUserStatus is skipped for self)
    expect(useMemberStore.getState().userStatuses.get('user-1')).toBeUndefined();
  });

  // regression for #803 — Member List + UserPopover show the connected
  // self-user as Offline. The server's connect-time presence_snapshot INCLUDES
  // self (hub.go sendPresenceSnapshot), but the renderer never reconciles the
  // self-user's status into memberStore.selfStatus — the value MemberList
  // (getMemberStatus) / UserPopover read as the self source of truth.
  // setPresenceSnapshot writes only userStatuses/onlineUserIds, and the
  // `presence` handler explicitly skips self. So a stale/offline selfStatus is
  // never corrected on (re)connect.
  it('reconciles self status from connect-time presence_snapshot (regression #803)', () => {
    useUserStore.setState({ user: mockUser as never }); // mockUser.id === 'user-1'

    // Stale local state: self currently displayed Offline (e.g. carried over
    // from a prior disconnect — nothing reconciles it back to Online on connect).
    useMemberStore.getState().setSelfStatus('offline');

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    const handler = wsService.handlers.get('presence_snapshot');
    expect(handler).toBeDefined();

    act(() => {
      // Server snapshot on connect: self ('user-1') is online, and a peer.
      handler!({
        type: 'presence_snapshot',
        data: {
          users: [
            { user_id: 'user-1', status: 'online' },
            { user_id: 'user-2', status: 'online' },
          ],
        },
      });
    });

    // Peer presence still works (regression guard for "other users").
    expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('online');

    // BUG: self must reflect the server's authoritative 'online', not the stale
    // local 'offline'. Fails on current code — selfStatus is never reconciled.
    expect(useMemberStore.getState().selfStatus).toBe('online');
  });

  // #803 — supplementary coverage for the snapshot self-reconciliation branches.

  it('adopts self status verbatim from snapshot — invisible is not clobbered to offline (#803)', () => {
    useUserStore.setState({ user: mockUser as never }); // self === 'user-1'
    useMemberStore.getState().setSelfStatus('online');

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    act(() => {
      // The server snapshot is self-aware (resolveVisibleStatus): the self viewer
      // sees their REAL status, so an invisible user must land as 'invisible',
      // never the broadcast-to-others 'offline' the `presence` self-skip guards.
      wsService.handlers.get('presence_snapshot')!({
        type: 'presence_snapshot',
        data: { users: [{ user_id: 'user-1', status: 'invisible' }] },
      });
    });

    expect(useMemberStore.getState().selfStatus).toBe('invisible');
  });

  it('leaves self status untouched when the snapshot omits self — back-compat guard (#803)', () => {
    useUserStore.setState({ user: mockUser as never });
    useMemberStore.getState().setSelfStatus('dnd');

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    act(() => {
      // A server that does not echo self in the snapshot must not clobber the
      // local selfStatus.
      wsService.handlers.get('presence_snapshot')!({
        type: 'presence_snapshot',
        data: { users: [{ user_id: 'user-2', status: 'online' }] },
      });
    });

    expect(useMemberStore.getState().selfStatus).toBe('dnd');
  });

  it('reconciles self to online from a legacy online_user_ids snapshot (#803)', () => {
    useUserStore.setState({ user: mockUser as never });
    useMemberStore.getState().setSelfStatus('offline');

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    act(() => {
      // Legacy snapshots carry only IDs (no per-user status); self present ⇒ online.
      wsService.handlers.get('presence_snapshot')!({
        type: 'presence_snapshot',
        data: { online_user_ids: ['user-1', 'user-2'] },
      });
    });

    expect(useMemberStore.getState().selfStatus).toBe('online');
  });

  it('does not downgrade a dnd self on the legacy online_user_ids path (#803)', () => {
    useUserStore.setState({ user: mockUser as never });
    useMemberStore.getState().setSelfStatus('dnd');

    const wsService = createMockWsService();
    renderHook(() => useWebSocketMessages(wsService as never));

    act(() => {
      // Self is in the online list, but the legacy path carries no status and
      // must NOT clobber the deliberate 'dnd' choice down to 'online'.
      wsService.handlers.get('presence_snapshot')!({
        type: 'presence_snapshot',
        data: { online_user_ids: ['user-1', 'user-2'] },
      });
    });

    expect(useMemberStore.getState().selfStatus).toBe('dnd');
  });
});
