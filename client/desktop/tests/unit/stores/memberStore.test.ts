import { useMemberStore } from '@/renderer/stores/memberStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockMember, mockMember2 } from '../../mocks/fixtures';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('memberStore', () => {
  beforeEach(() => {
    resetAllStores();
    // clearMembers() preserves presence state — reset it explicitly for test isolation
    useMemberStore.setState({
      onlineUserIds: new Set(),
      userStatuses: new Map(),
      lastSeenByUser: new Map(),
      selfStatus: 'online',
    });
    useAuthStore.getState().setAccessToken('mock-token');
  });

  describe('fetchMembers', () => {
    it('fetches members from API', async () => {
      await useMemberStore.getState().fetchMembers('server-1');
      const state = useMemberStore.getState();
      expect(state.members).toHaveLength(2);
      expect(state.isLoading).toBe(false);
      expect(state.error).toBeNull();
    });

    it('populates lastSeenByUser from response', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/members`, () =>
          HttpResponse.json({
            members: [
              { ...mockMember, last_seen: 1700000000 },
              { ...mockMember2, last_seen: null },
            ],
          })
        )
      );
      await useMemberStore.getState().fetchMembers('server-1');
      expect(useMemberStore.getState().lastSeenByUser.get('user-1')).toBe(1700000000);
      expect(useMemberStore.getState().lastSeenByUser.has('user-2')).toBe(false);
    });

    it('sets error on fetch failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/members`, () =>
          HttpResponse.json({ error: 'Forbidden' }, { status: 403 })
        )
      );
      await useMemberStore.getState().fetchMembers('server-1');
      expect(useMemberStore.getState().error).toBe('Forbidden');
      expect(useMemberStore.getState().isLoading).toBe(false);
    });

    it('handles empty member list', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/servers/server-1/members`, () =>
          HttpResponse.json({ members: [] })
        )
      );
      await useMemberStore.getState().fetchMembers('server-1');
      expect(useMemberStore.getState().members).toHaveLength(0);
    });
  });

  describe('addMember', () => {
    it('adds a member', () => {
      useMemberStore.getState().addMember(mockMember);
      expect(useMemberStore.getState().members).toHaveLength(1);
      expect(useMemberStore.getState().members[0].user_id).toBe('user-1');
    });

    it('prevents duplicate members', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().addMember(mockMember);
      expect(useMemberStore.getState().members).toHaveLength(1);
    });
  });

  describe('removeMember', () => {
    it('removes a member by userId', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().addMember(mockMember2);
      useMemberStore.getState().removeMember('user-1');
      expect(useMemberStore.getState().members).toHaveLength(1);
      expect(useMemberStore.getState().members[0].user_id).toBe('user-2');
    });
  });

  describe('updateMemberProfile', () => {
    it('updates display_name for existing member', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().updateMemberProfile('user-1', { display_name: 'New Name' });
      expect(useMemberStore.getState().members[0].display_name).toBe('New Name');
    });

    it('updates username for existing member', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().updateMemberProfile('user-1', { username: 'newusername' });
      expect(useMemberStore.getState().members[0].username).toBe('newusername');
    });

    it('does nothing for nonexistent member', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().updateMemberProfile('nonexistent', { display_name: 'Test' });
      expect(useMemberStore.getState().members).toHaveLength(1);
      expect(useMemberStore.getState().members[0].display_name).toBe('Test User');
    });
  });

  describe('setOnlineUsers', () => {
    it('sets online user IDs', () => {
      useMemberStore.getState().setOnlineUsers(['user-1', 'user-2']);
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
      expect(useMemberStore.getState().onlineUserIds.has('user-2')).toBe(true);
    });

    it('preserves DND status when updating online users', () => {
      useMemberStore.getState().setUserStatus('user-1', 'dnd');
      useMemberStore.getState().setOnlineUsers(['user-1', 'user-2']);
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('dnd');
    });

    it('sets new users to online by default', () => {
      useMemberStore.getState().setOnlineUsers(['user-1']);
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('online');
    });
  });

  describe('setUserOnline', () => {
    it('adds user to onlineUserIds', () => {
      useMemberStore.getState().setUserOnline('user-1');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('online');
    });
  });

  describe('setUserOffline', () => {
    it('removes user from onlineUserIds', () => {
      useMemberStore.getState().setUserOnline('user-1');
      useMemberStore.getState().setUserOffline('user-1');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(false);
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('offline');
    });
  });

  describe('setUserStatus', () => {
    it('sets specific status', () => {
      useMemberStore.getState().setUserStatus('user-1', 'dnd');
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('dnd');
    });

    it('adds to onlineUserIds for online status', () => {
      useMemberStore.getState().setUserStatus('user-1', 'online');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
    });

    it('adds to onlineUserIds for dnd status', () => {
      useMemberStore.getState().setUserStatus('user-1', 'dnd');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
    });

    it('removes from onlineUserIds for offline status', () => {
      useMemberStore.getState().setUserStatus('user-1', 'online');
      useMemberStore.getState().setUserStatus('user-1', 'offline');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(false);
    });

    it('removes from onlineUserIds for invisible status', () => {
      useMemberStore.getState().setUserStatus('user-1', 'online');
      useMemberStore.getState().setUserStatus('user-1', 'invisible');
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(false);
    });
  });

  describe('setUserLastSeen', () => {
    it('sets last seen timestamp', () => {
      useMemberStore.getState().setUserLastSeen('user-1', 1700000000);
      expect(useMemberStore.getState().lastSeenByUser.get('user-1')).toBe(1700000000);
    });
  });

  describe('setPresenceSnapshot', () => {
    it('bulk sets presence', () => {
      useMemberStore.getState().setPresenceSnapshot([
        { user_id: 'user-1', status: 'online' },
        { user_id: 'user-2', status: 'dnd' },
      ]);
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
      expect(useMemberStore.getState().onlineUserIds.has('user-2')).toBe(true);
      expect(useMemberStore.getState().userStatuses.get('user-2')).toBe('dnd');
    });

    it('does not add offline users to onlineUserIds', () => {
      useMemberStore.getState().setPresenceSnapshot([{ user_id: 'user-1', status: 'offline' }]);
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(false);
      expect(useMemberStore.getState().userStatuses.get('user-1')).toBe('offline');
    });
  });

  describe('setSelfStatus', () => {
    it('sets self status', () => {
      useMemberStore.getState().setSelfStatus('dnd');
      expect(useMemberStore.getState().selfStatus).toBe('dnd');
    });

    it('defaults to online', () => {
      expect(useMemberStore.getState().selfStatus).toBe('online');
    });
  });

  describe('server enforcement fields', () => {
    it('should include server_muted and server_deafened in member data', () => {
      useMemberStore.getState().addMember({
        ...mockMember,
        user_id: 'u-enforced',
        username: 'enforced-user',
        server_muted: true,
        server_deafened: false,
      });
      const member = useMemberStore.getState().members.find((m) => m.user_id === 'u-enforced');
      expect(member?.server_muted).toBe(true);
      expect(member?.server_deafened).toBe(false);
    });

    it('updates and clears timed_out_until for existing members', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().setMemberTimeout('user-1', '2026-06-28T12:00:00Z');
      expect(useMemberStore.getState().members[0].timed_out_until).toBe('2026-06-28T12:00:00Z');

      useMemberStore.getState().setMemberTimeout('user-1', null);
      expect(useMemberStore.getState().members[0].timed_out_until).toBeNull();
    });
  });

  describe('clearMembers', () => {
    it('clears members list but preserves presence', () => {
      useMemberStore.getState().addMember(mockMember);
      useMemberStore.getState().setUserOnline('user-1');
      useMemberStore.getState().clearMembers();
      expect(useMemberStore.getState().members).toHaveLength(0);
      // Presence is preserved (not tied to specific server)
      expect(useMemberStore.getState().onlineUserIds.has('user-1')).toBe(true);
    });
  });
});
