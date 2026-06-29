import { renderHook, act } from '@testing-library/react';
import { useWebSocket } from '@/renderer/hooks/useWebSocket';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useMemberStore } from '@/renderer/stores/memberStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { ConnectionState } from '@/renderer/services/websocketService';
import { resetAllStores } from '../../helpers/store-helpers';

// Capture registered handlers so we can invoke them in tests
type HandlerFn = (...args: unknown[]) => void;
const registeredHandlers = new Map<string, HandlerFn>();
let connectionChangeHandler: HandlerFn | null = null;

const mockWsService = {
  connect: vi.fn(),
  disconnect: vi.fn(),
  updateToken: vi.fn(),
  resetReconnectState: vi.fn(),
  on: vi.fn((type: string, handler: HandlerFn) => {
    registeredHandlers.set(type, handler);
    return () => {
      registeredHandlers.delete(type);
    };
  }),
  onConnectionChange: vi.fn((handler: HandlerFn) => {
    connectionChangeHandler = handler;
    // Immediately call with DISCONNECTED (like the real service)
    handler(ConnectionState.DISCONNECTED);
    return () => {
      connectionChangeHandler = null;
    };
  }),
  getConnectionInfo: vi.fn(() => null),
  getState: vi.fn(() => ConnectionState.DISCONNECTED),
  subscribe: vi.fn(),
  unsubscribe: vi.fn(),
  sendMessage: vi.fn(),
  sendTypingIndicator: vi.fn(),
};

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => mockWsService,
  ConnectionState: {
    DISCONNECTED: 'disconnected',
    CONNECTING: 'connecting',
    CONNECTED: 'connected',
    RECONNECTING: 'reconnecting',
    ERROR: 'error',
  },
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    processPendingKeyRequests: vi.fn().mockResolvedValue(undefined),
    decryptForChannel: vi.fn(),
    invalidateChannelKey: vi.fn(),
  },
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    fetchAndApply: vi.fn(),
  },
}));

describe('useWebSocket', () => {
  beforeEach(() => {
    resetAllStores();
    registeredHandlers.clear();
    connectionChangeHandler = null;
    vi.clearAllMocks();
    // vi.clearAllMocks() clears call history but does NOT reset
    // mockReturnValue overrides. Tests that mutate getState.mockReturnValue
    // (e.g., the token-rotation regression that simulates CONNECTED) would
    // leak that value into subsequent tests. Reset to the documented default
    // here so every test starts in DISCONNECTED state.
    mockWsService.getState.mockReturnValue(ConnectionState.DISCONNECTED);

    // Reset presence state
    useMemberStore.setState({
      onlineUserIds: new Set(),
      userStatuses: new Map(),
      lastSeenByUser: new Map(),
      selfStatus: 'online',
    });
  });

  describe('connection management', () => {
    it('connects when access token is available', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());
      expect(mockWsService.connect).toHaveBeenCalledWith('test-token');
    });

    it('disconnects when no access token', () => {
      renderHook(() => useWebSocket());
      expect(mockWsService.disconnect).toHaveBeenCalled();
    });

    it('disconnects on unmount', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { unmount } = renderHook(() => useWebSocket());
      mockWsService.disconnect.mockClear();
      unmount();
      expect(mockWsService.disconnect).toHaveBeenCalled();
    });

    // Regression for the JWT-refresh churn: previously every accessToken
    // change tore down the WS via useEffect cleanup and reconnected with the
    // new token, retransmitting all subscribe frames and stopping/starting
    // the message queue. The hook now uses wsService.updateToken on
    // rotation, leaving the open socket intact.
    it('calls updateToken (not disconnect/connect) when access token rotates while connected', () => {
      // First mount: getState returns DISCONNECTED, so connect() is the right path.
      useAuthStore.getState().setAccessToken('token-1');
      renderHook(() => useWebSocket());
      expect(mockWsService.connect).toHaveBeenCalledWith('token-1');

      // Simulate the socket reaching CONNECTED before the rotation arrives.
      mockWsService.getState.mockReturnValue(ConnectionState.CONNECTED);
      mockWsService.connect.mockClear();
      mockWsService.disconnect.mockClear();
      mockWsService.updateToken.mockClear();

      // Token rotates (proactive refresh or main-process onTokenRefreshed).
      act(() => {
        useAuthStore.getState().setAccessToken('token-2');
      });

      expect(mockWsService.updateToken).toHaveBeenCalledWith('token-2');
      expect(mockWsService.connect).not.toHaveBeenCalled();
      expect(mockWsService.disconnect).not.toHaveBeenCalled();
    });

    it('restarts a connecting handshake when the access token rotates (#1977)', () => {
      useAuthStore.getState().setAccessToken('stale-token');
      renderHook(() => useWebSocket());
      expect(mockWsService.connect).toHaveBeenCalledWith('stale-token');

      mockWsService.getState.mockReturnValue(ConnectionState.CONNECTING);
      mockWsService.connect.mockClear();
      mockWsService.updateToken.mockClear();
      mockWsService.resetReconnectState.mockClear();

      act(() => {
        useAuthStore.getState().setAccessToken('fresh-token');
      });

      expect(mockWsService.resetReconnectState).toHaveBeenCalledTimes(1);
      expect(mockWsService.connect).toHaveBeenCalledWith('fresh-token');
      expect(mockWsService.updateToken).not.toHaveBeenCalled();
    });
  });

  describe('handler registration', () => {
    it('registers message handlers', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      // Should register handlers for all expected message types
      expect(registeredHandlers.has('message')).toBe(true);
      expect(registeredHandlers.has('message_update')).toBe(true);
      expect(registeredHandlers.has('message_delete')).toBe(true);
      expect(registeredHandlers.has('typing')).toBe(true);
      expect(registeredHandlers.has('message_ack')).toBe(true);
      expect(registeredHandlers.has('member_joined')).toBe(true);
      expect(registeredHandlers.has('profile_updated')).toBe(true);
      expect(registeredHandlers.has('server_updated')).toBe(true);
      expect(registeredHandlers.has('channel_updated')).toBe(true);
      expect(registeredHandlers.has('channel_deleted')).toBe(true);
      expect(registeredHandlers.has('server_deleted')).toBe(true);
      expect(registeredHandlers.has('member_removed')).toBe(true);
      expect(registeredHandlers.has('unread_notify')).toBe(true);
      expect(registeredHandlers.has('key_needed')).toBe(true);
      expect(registeredHandlers.has('key_delivered')).toBe(true);
      expect(registeredHandlers.has('preferences_updated')).toBe(true);
      expect(registeredHandlers.has('presence_snapshot')).toBe(true);
      expect(registeredHandlers.has('presence')).toBe(true);
      expect(registeredHandlers.has('server_online_counts')).toBe(true);
    });
  });

  describe('message handler', () => {
    it('adds messages to chat store (blanked when e2ee not initialized)', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('message')!;
      act(() => {
        handler({
          type: 'message',
          data: {
            id: 'msg-1',
            channel_id: 'ch-1',
            user_id: 'user-1',
            username: 'alice',
            content: 'Hello!',
            created_at: '2025-01-01T00:00:00Z',
          },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('ch-1');
      expect(msgs).toBeDefined();
      // E2EE not initialized — content is blanked fail-closed
      expect(msgs![0].content).toBe('');
      expect(msgs![0].decryptFailed).toBe(true);
    });

    it('ignores messages without channel_id', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('message')!;
      // Should not throw
      act(() => {
        handler({ type: 'message', data: { content: 'no channel' } });
      });
    });
  });

  describe('message_update handler', () => {
    it('updates existing messages', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      // Add a message first
      useChatStore.getState().addMessage('ch-1', {
        id: 'msg-1',
        channel_id: 'ch-1',
        user_id: 'user-1',
        username: 'alice',
        content: 'Original',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = registeredHandlers.get('message_update')!;
      act(() => {
        handler({
          type: 'message_update',
          data: {
            channel_id: 'ch-1',
            id: 'msg-1',
            content: 'Edited',
            edited_at: '2025-01-01T00:01:00Z',
          },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('ch-1');
      expect(msgs![0].content).toBe('Edited');
    });
  });

  describe('message_delete handler', () => {
    it('deletes messages from chat store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      useChatStore.getState().addMessage('ch-1', {
        id: 'msg-1',
        channel_id: 'ch-1',
        user_id: 'user-1',
        username: 'alice',
        content: 'To delete',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = registeredHandlers.get('message_delete')!;
      act(() => {
        handler({ type: 'message_delete', data: { channel_id: 'ch-1', id: 'msg-1' } });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('ch-1');
      expect(msgs).toHaveLength(0);
    });
  });

  describe('typing handler', () => {
    it('sets typing status in chat store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('typing')!;
      act(() => {
        handler({
          type: 'typing',
          data: { channel_id: 'ch-1', user_id: 'user-1', is_typing: true, username: 'alice' },
        });
      });

      const typing = useChatStore.getState().typingByChannel.get('ch-1');
      expect(typing?.has('user-1')).toBe(true);
    });
  });

  describe('message_ack handler', () => {
    it('updates message status from pending to delivered', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      useChatStore.getState().addMessage('ch-1', {
        id: 'nonce-1',
        channel_id: 'ch-1',
        user_id: 'user-1',
        username: 'alice',
        content: 'Pending',
        status: 'pending',
        clientMessageId: 'nonce-1',
        created_at: '2025-01-01T00:00:00Z',
        updated_at: '2025-01-01T00:00:00Z',
      });

      const handler = registeredHandlers.get('message_ack')!;
      act(() => {
        handler({
          type: 'message_ack',
          data: { nonce: 'nonce-1', id: 'server-msg-1', channel_id: 'ch-1' },
        });
      });

      const msgs = useChatStore.getState().messagesByChannel.get('ch-1');
      expect(msgs![0].status).toBe('delivered');
    });
  });

  describe('member_joined handler', () => {
    it('adds member to store when event is for active server', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('member_joined')!;
      act(() => {
        handler({
          type: 'member_joined',
          data: { server_id: 'server-1', user_id: 'user-2', username: 'bob', role: 'member' },
        });
      });

      expect(useMemberStore.getState().members.some((m) => m.user_id === 'user-2')).toBe(true);
    });

    it('ignores events for non-active server', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('member_joined')!;
      act(() => {
        handler({
          type: 'member_joined',
          data: { server_id: 'server-2', user_id: 'user-2', username: 'bob' },
        });
      });

      expect(useMemberStore.getState().members).toHaveLength(0);
    });
  });

  describe('server_updated handler', () => {
    it('updates server in store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().addServer({
        id: 'server-1',
        name: 'Old Name',
        owner_id: 'user-1',
        created_at: '',
        updated_at: '',
        role: 'owner',
        member_count: 1,
        online_count: 1,
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('server_updated')!;
      act(() => {
        handler({
          type: 'server_updated',
          data: { server_id: 'server-1', name: 'New Name', icon_url: null },
        });
      });

      expect(useServerStore.getState().servers[0].name).toBe('New Name');
    });
  });

  describe('channel_updated handler', () => {
    it('updates channel in store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useChannelStore.getState().addChannel({
        id: 'ch-1',
        server_id: 'server-1',
        name: 'old-name',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('channel_updated')!;
      act(() => {
        handler({
          type: 'channel_updated',
          data: { channel_id: 'ch-1', name: 'new-name', type: 'text' },
        });
      });

      expect(useChannelStore.getState().channels[0].name).toBe('new-name');
    });
  });

  describe('channel_deleted handler', () => {
    it('removes channel from store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useChannelStore.getState().addChannel({
        id: 'ch-1',
        server_id: 'server-1',
        name: 'doomed',
        type: 'text',
        position: 0,
        created_at: '',
        updated_at: '',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('channel_deleted')!;
      act(() => {
        handler({ type: 'channel_deleted', data: { channel_id: 'ch-1' } });
      });

      expect(useChannelStore.getState().channels).toHaveLength(0);
    });
  });

  describe('server_deleted handler', () => {
    it('removes server from store', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().addServer({
        id: 'server-1',
        name: 'Doomed',
        owner_id: 'user-1',
        created_at: '',
        updated_at: '',
        role: 'owner',
        member_count: 1,
        online_count: 1,
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('server_deleted')!;
      act(() => {
        handler({ type: 'server_deleted', data: { server_id: 'server-1' } });
      });

      expect(useServerStore.getState().servers).toHaveLength(0);
    });
  });

  describe('member_removed handler', () => {
    it('removes server when self is removed', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.getState().setUser({
        id: 'user-1',
        username: 'me',
      });
      useServerStore.getState().addServer({
        id: 'server-1',
        name: 'Test',
        owner_id: 'user-2',
        created_at: '',
        updated_at: '',
        role: 'member',
        member_count: 1,
        online_count: 1,
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('member_removed')!;
      act(() => {
        handler({
          type: 'member_removed',
          data: { server_id: 'server-1', user_id: 'user-1' },
        });
      });

      expect(useServerStore.getState().servers).toHaveLength(0);
    });

    it('removes other member from member list', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.getState().setUser({
        id: 'user-1',
        username: 'me',
      });
      useServerStore.getState().setActiveServer('server-1');
      useMemberStore.getState().addMember({
        user_id: 'user-2',
        username: 'other',
        role: 'member',
        joined_at: '',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('member_removed')!;
      act(() => {
        handler({
          type: 'member_removed',
          data: { server_id: 'server-1', user_id: 'user-2' },
        });
      });

      expect(useMemberStore.getState().members).toHaveLength(0);
    });
  });

  describe('unread_notify handler', () => {
    it('marks server unread for non-active server', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().setActiveServer('server-1');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('unread_notify')!;
      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-2', channel_id: 'ch-1' },
        });
      });

      expect(useUnreadStore.getState().serverUnreadSet.has('server-2')).toBe(true);
    });

    it('increments unread for non-active channel in active server', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().setActiveServer('server-1');
      useChannelStore.setState({ activeChannelId: 'ch-1' });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('unread_notify')!;
      act(() => {
        handler({
          type: 'unread_notify',
          data: { server_id: 'server-1', channel_id: 'ch-2' },
        });
      });

      expect(useUnreadStore.getState().unreadCounts.get('ch-2')).toBe(1);
    });
  });

  describe('presence_snapshot handler', () => {
    it('sets presence from user array', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('presence_snapshot')!;
      act(() => {
        handler({
          type: 'presence_snapshot',
          data: {
            users: [
              { user_id: 'user-1', status: 'online' },
              { user_id: 'user-2', status: 'dnd' },
            ],
          },
        });
      });

      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('online');
      expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('dnd');
    });

    it('falls back to online_user_ids format', () => {
      useAuthStore.getState().setAccessToken('test-token');
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('presence_snapshot')!;
      act(() => {
        handler({
          type: 'presence_snapshot',
          data: { online_user_ids: ['user-1', 'user-2'] },
        });
      });

      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
    });
  });

  describe('presence handler', () => {
    it('updates user status', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.getState().setUser({
        id: 'self-user',
        username: 'me',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('presence')!;
      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-2', status: 'dnd' },
        });
      });

      expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('dnd');
    });

    it('skips self user presence updates', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.getState().setUser({
        id: 'self-user',
        username: 'me',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('presence')!;
      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'self-user', status: 'offline' },
        });
      });

      // Should NOT have set self user to offline
      expect(useMemberStore.getState().userStatuses.has('self-user')).toBe(false);
    });

    it('records last_seen timestamp for offline transitions', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useUserStore.getState().setUser({
        id: 'self-user',
        username: 'me',
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('presence')!;
      act(() => {
        handler({
          type: 'presence',
          data: { user_id: 'user-2', status: 'offline', timestamp: 1700000000 },
        });
      });

      expect(useMemberStore.getState().lastSeenByUser.get('user-2')).toBe(1700000000);
    });
  });

  describe('server_online_counts handler', () => {
    it('updates server online counts', () => {
      useAuthStore.getState().setAccessToken('test-token');
      useServerStore.getState().addServer({
        id: 'server-1',
        name: 'Test',
        owner_id: 'user-1',
        created_at: '',
        updated_at: '',
        role: 'owner',
        member_count: 10,
        online_count: 1,
      });
      renderHook(() => useWebSocket());

      const handler = registeredHandlers.get('server_online_counts')!;
      act(() => {
        handler({
          type: 'server_online_counts',
          data: { counts: { 'server-1': 7 } },
        });
      });

      expect(useServerStore.getState().servers[0].online_count).toBe(7);
    });
  });

  describe('returned API', () => {
    it('returns subscribe/unsubscribe/sendMessage/sendTyping/getState functions', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { result } = renderHook(() => useWebSocket());

      expect(typeof result.current.subscribe).toBe('function');
      expect(typeof result.current.unsubscribe).toBe('function');
      expect(typeof result.current.sendMessage).toBe('function');
      expect(typeof result.current.sendTyping).toBe('function');
      expect(typeof result.current.getState).toBe('function');
    });

    it('subscribe delegates to wsService', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { result } = renderHook(() => useWebSocket());

      result.current.subscribe('ch-1');
      expect(mockWsService.subscribe).toHaveBeenCalledWith('ch-1');
    });

    it('sendMessage delegates to wsService', () => {
      useAuthStore.getState().setAccessToken('test-token');
      const { result } = renderHook(() => useWebSocket());

      result.current.sendMessage('ch-1', 'Hello');
      expect(mockWsService.sendMessage).toHaveBeenCalledWith('ch-1', 'Hello');
    });
  });
});
