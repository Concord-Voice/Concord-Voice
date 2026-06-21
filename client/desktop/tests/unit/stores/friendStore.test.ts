import {
  useFriendStore,
  type Friend,
  type FriendRequest,
  type FriendCode,
} from '@/renderer/stores/friendStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

const mockFriend: Friend = {
  id: 'friendship-1',
  userId: 'user-2',
  username: 'bob',
  displayName: 'Bob',
  status: 'offline',
};

const mockFriend2: Friend = {
  id: 'friendship-2',
  userId: 'user-3',
  username: 'charlie',
  displayName: 'Charlie',
  avatarUrl: 'https://example.com/charlie.png',
  colorScheme: 'morky',
  status: 'online',
};

const mockRequest: FriendRequest = {
  id: 'req-1',
  fromUserId: 'user-3',
  fromUsername: 'charlie',
  fromDisplayName: 'Charlie',
  fromAvatarUrl: 'https://example.com/charlie.png',
  toUserId: 'user-1',
  toUsername: 'alice',
  direction: 'received',
  createdAt: '2025-01-01T00:00:00Z',
};

const mockSentRequest: FriendRequest = {
  id: 'req-2',
  fromUserId: 'user-1',
  fromUsername: 'alice',
  toUserId: 'user-4',
  toUsername: 'dave',
  direction: 'sent',
  createdAt: '2025-01-01T00:00:00Z',
};

const mockFriendCode: FriendCode = {
  id: 'fc-1',
  code: 'ABC123',
  maxUses: 5,
  useCount: 0,
  expiresAt: '2025-06-01T00:00:00Z',
  isRevoked: false,
  autoAccept: true,
  createdAt: '2025-01-01T00:00:00Z',
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
});

describe('friendStore', () => {
  // ── Initial state ─────────────────────────────────────────────────────

  describe('initial state', () => {
    it('starts with empty arrays and no errors', () => {
      const state = useFriendStore.getState();
      expect(state.friends).toEqual([]);
      expect(state.pendingRequests).toEqual([]);
      expect(state.blockedUserIds).toEqual([]);
      expect(state.friendCodes).toEqual([]);
      expect(state.isLoading).toBe(false);
      expect(state.error).toBeNull();
    });
  });

  // ── Real-time update actions ──────────────────────────────────────────

  describe('real-time update actions', () => {
    it('addFriend adds to list', () => {
      useFriendStore.getState().addFriend(mockFriend);
      expect(useFriendStore.getState().friends).toHaveLength(1);
      expect(useFriendStore.getState().friends[0].userId).toBe('user-2');
    });

    it('addFriend does not duplicate by userId', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend);
      expect(useFriendStore.getState().friends).toHaveLength(1);
    });

    it('addFriend does not duplicate even with different friendship id', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend({ ...mockFriend, id: 'friendship-99' });
      expect(useFriendStore.getState().friends).toHaveLength(1);
    });

    it('addFriend adds multiple distinct friends', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend2);
      expect(useFriendStore.getState().friends).toHaveLength(2);
    });

    it('removeFriendByUserId removes from list', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().removeFriendByUserId('user-2');
      expect(useFriendStore.getState().friends).toHaveLength(0);
    });

    it('removeFriendByUserId is a no-op for non-existent userId', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().removeFriendByUserId('user-999');
      expect(useFriendStore.getState().friends).toHaveLength(1);
    });

    it('removeFriendByUserId only removes the targeted friend', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend2);
      useFriendStore.getState().removeFriendByUserId('user-2');
      expect(useFriendStore.getState().friends).toHaveLength(1);
      expect(useFriendStore.getState().friends[0].userId).toBe('user-3');
    });

    it('addRequest adds to pending list', () => {
      useFriendStore.getState().addRequest(mockRequest);
      expect(useFriendStore.getState().pendingRequests).toHaveLength(1);
    });

    it('addRequest does not duplicate by request id', () => {
      useFriendStore.getState().addRequest(mockRequest);
      useFriendStore.getState().addRequest(mockRequest);
      expect(useFriendStore.getState().pendingRequests).toHaveLength(1);
    });

    it('addRequest allows multiple distinct requests', () => {
      useFriendStore.getState().addRequest(mockRequest);
      useFriendStore.getState().addRequest(mockSentRequest);
      expect(useFriendStore.getState().pendingRequests).toHaveLength(2);
    });

    it('removeRequest removes from pending list', () => {
      useFriendStore.getState().addRequest(mockRequest);
      useFriendStore.getState().removeRequest('req-1');
      expect(useFriendStore.getState().pendingRequests).toHaveLength(0);
    });

    it('removeRequest is a no-op for non-existent requestId', () => {
      useFriendStore.getState().addRequest(mockRequest);
      useFriendStore.getState().removeRequest('req-999');
      expect(useFriendStore.getState().pendingRequests).toHaveLength(1);
    });

    it('updateFriendPresence changes status', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().updateFriendPresence('user-2', 'online');
      expect(useFriendStore.getState().friends[0].status).toBe('online');
    });

    it('updateFriendPresence to dnd', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().updateFriendPresence('user-2', 'dnd');
      expect(useFriendStore.getState().friends[0].status).toBe('dnd');
    });

    it('updateFriendPresence does not affect other friends', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend2);
      useFriendStore.getState().updateFriendPresence('user-2', 'online');
      expect(useFriendStore.getState().friends[1].status).toBe('online'); // mockFriend2 unchanged
    });

    it('updateFriendProfile updates display info', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().updateFriendProfile('user-2', { displayName: 'Bobby' });
      expect(useFriendStore.getState().friends[0].displayName).toBe('Bobby');
    });

    it('updateFriendProfile can update multiple fields', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().updateFriendProfile('user-2', {
        displayName: 'Bobby',
        avatarUrl: 'https://example.com/new.png',
        colorScheme: 'hacker',
      });
      const friend = useFriendStore.getState().friends[0];
      expect(friend.displayName).toBe('Bobby');
      expect(friend.avatarUrl).toBe('https://example.com/new.png');
      expect(friend.colorScheme).toBe('hacker');
    });

    it('updateFriendProfile does not modify other friends', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend2);
      useFriendStore.getState().updateFriendProfile('user-2', { displayName: 'Bobby' });
      expect(useFriendStore.getState().friends[1].displayName).toBe('Charlie');
    });

    it('updateFriendProfile preserves immutable fields (userId, id, status)', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().updateFriendProfile('user-2', { username: 'bobby_new' });
      const friend = useFriendStore.getState().friends[0];
      expect(friend.username).toBe('bobby_new');
      expect(friend.userId).toBe('user-2');
      expect(friend.id).toBe('friendship-1');
      expect(friend.status).toBe('offline');
    });
  });

  // ── fetchFriends ──────────────────────────────────────────────────────

  describe('fetchFriends', () => {
    it('loads friends from API', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({
            friends: [
              {
                id: 'f-1',
                user_id: 'user-2',
                username: 'bob',
                display_name: 'Bob',
                avatar_url: null,
                color_scheme: null,
                status: 'online',
              },
            ],
          });
        })
      );

      await useFriendStore.getState().fetchFriends();
      const state = useFriendStore.getState();
      expect(state.friends).toHaveLength(1);
      expect(state.friends[0].userId).toBe('user-2');
      expect(state.friends[0].username).toBe('bob');
      expect(state.friends[0].status).toBe('online');
      expect(state.isLoading).toBe(false);
      expect(state.error).toBeNull();
    });

    it('maps snake_case API response to camelCase', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({
            friends: [
              {
                id: 'f-1',
                user_id: 'user-2',
                username: 'bob',
                display_name: 'Bob Display',
                avatar_url: 'https://example.com/bob.png',
                color_scheme: 'morky',
                status: 'dnd',
              },
            ],
          });
        })
      );

      await useFriendStore.getState().fetchFriends();
      const friend = useFriendStore.getState().friends[0];
      expect(friend.displayName).toBe('Bob Display');
      expect(friend.avatarUrl).toBe('https://example.com/bob.png');
      expect(friend.colorScheme).toBe('morky');
    });

    it('maps created_at to createdAt', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({
            friends: [
              {
                id: 'f-1',
                user_id: 'user-2',
                username: 'bob',
                status: 'online',
                created_at: '2025-03-14T12:00:00Z',
              },
            ],
          });
        })
      );

      await useFriendStore.getState().fetchFriends();
      const friend = useFriendStore.getState().friends[0];
      expect(friend.createdAt).toBe('2025-03-14T12:00:00Z');
    });

    it('defaults status to offline when not provided', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({
            friends: [{ id: 'f-1', user_id: 'user-2', username: 'bob' }],
          });
        })
      );

      await useFriendStore.getState().fetchFriends();
      expect(useFriendStore.getState().friends[0].status).toBe('offline');
    });

    it('sets error on API failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await useFriendStore.getState().fetchFriends();
      expect(useFriendStore.getState().error).toBe('Forbidden');
      expect(useFriendStore.getState().isLoading).toBe(false);
    });

    it('uses generic error message when none provided', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await useFriendStore.getState().fetchFriends();
      expect(useFriendStore.getState().error).toBe('Failed to load friends');
    });

    it('handles empty friends array', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.json({ friends: [] });
        })
      );

      await useFriendStore.getState().fetchFriends();
      expect(useFriendStore.getState().friends).toEqual([]);
      expect(useFriendStore.getState().isLoading).toBe(false);
    });

    it('handles network errors gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          return HttpResponse.error();
        })
      );

      await useFriendStore.getState().fetchFriends();
      // Network errors produce the native fetch error message
      expect(useFriendStore.getState().error).toBe('Failed to fetch');
    });

    it('does not fetch if already loading (deduplication guard)', async () => {
      let callCount = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/friends`, () => {
          callCount++;
          return HttpResponse.json({ friends: [] });
        })
      );

      useFriendStore.setState({ isLoading: true });
      await useFriendStore.getState().fetchFriends();
      expect(callCount).toBe(0);
    });
  });

  // ── fetchRequests ─────────────────────────────────────────────────────

  describe('fetchRequests', () => {
    it('loads requests from API', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/requests`, () => {
          return HttpResponse.json({
            requests: [
              {
                id: 'req-api-1',
                from_user_id: 'user-3',
                from_username: 'charlie',
                from_display_name: 'Charlie',
                from_avatar_url: null,
                to_user_id: 'user-1',
                to_username: 'alice',
                to_display_name: 'Alice',
                to_avatar_url: null,
                direction: 'received',
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useFriendStore.getState().fetchRequests();
      const requests = useFriendStore.getState().pendingRequests;
      expect(requests).toHaveLength(1);
      expect(requests[0].fromUserId).toBe('user-3');
      expect(requests[0].fromDisplayName).toBe('Charlie');
      expect(requests[0].toDisplayName).toBe('Alice');
      expect(requests[0].direction).toBe('received');
    });

    it('handles API error silently (console.error)', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      server.use(
        http.get(`${API_BASE}/api/v1/friends/requests`, () => {
          return HttpResponse.json({ error: 'Internal error' }, { status: 500 });
        })
      );

      await useFriendStore.getState().fetchRequests();
      expect(consoleSpy).toHaveBeenCalled();
      consoleSpy.mockRestore();
    });

    it('handles empty requests', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/requests`, () => {
          return HttpResponse.json({ requests: [] });
        })
      );

      await useFriendStore.getState().fetchRequests();
      expect(useFriendStore.getState().pendingRequests).toEqual([]);
    });

    it('handles network errors gracefully', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      server.use(
        http.get(`${API_BASE}/api/v1/friends/requests`, () => {
          return HttpResponse.error();
        })
      );

      await useFriendStore.getState().fetchRequests();
      expect(consoleSpy).toHaveBeenCalled();
      consoleSpy.mockRestore();
    });
  });

  // ── sendRequest ───────────────────────────────────────────────────────

  describe('sendRequest', () => {
    it('sends a friend request by user ID', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/friends/request`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ id: 'req-new', status: 'pending' }, { status: 201 });
        })
      );

      await useFriendStore.getState().sendRequest('user-5');
      expect(capturedBody).toEqual({ user_id: 'user-5' });
    });

    it('sends a friend request by username when byUsername is true', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/friends/request`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ id: 'req-new', status: 'pending' }, { status: 201 });
        })
      );

      await useFriendStore.getState().sendRequest('charlie', true);
      expect(capturedBody).toEqual({ username: 'charlie' });
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/request`, () => {
          return HttpResponse.json({ error: 'User not found' }, { status: 404 });
        })
      );

      await expect(useFriendStore.getState().sendRequest('user-999')).rejects.toThrow(
        'User not found'
      );
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/request`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useFriendStore.getState().sendRequest('user-5')).rejects.toThrow(
        'Failed to send friend request'
      );
    });
  });

  // ── acceptRequest ─────────────────────────────────────────────────────

  describe('acceptRequest', () => {
    it('accepts a request and removes it from pending', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Accepted' });
        })
      );

      useFriendStore.getState().addRequest(mockRequest);
      await useFriendStore.getState().acceptRequest('req-1');
      expect(useFriendStore.getState().pendingRequests).toHaveLength(0);
    });

    it('optimistically adds accepted friend to friends list', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Accepted' });
        })
      );

      useFriendStore.getState().addRequest(mockRequest);
      await useFriendStore.getState().acceptRequest('req-1');

      const friends = useFriendStore.getState().friends;
      expect(friends).toHaveLength(1);
      expect(friends[0].userId).toBe('user-3'); // fromUserId from the request
      expect(friends[0].username).toBe('charlie');
      expect(friends[0].displayName).toBe('Charlie');
      expect(friends[0].status).toBe('online');
    });

    it('does not duplicate friend if already in list', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Accepted' });
        })
      );

      useFriendStore.getState().addFriend(mockFriend2); // mockFriend2.userId = 'user-3'
      useFriendStore.getState().addRequest(mockRequest); // request from user-3
      await useFriendStore.getState().acceptRequest('req-1');

      expect(useFriendStore.getState().friends).toHaveLength(1);
    });

    it('handles accept when request is not in pending (no optimistic friend added)', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Accepted' });
        })
      );

      // No request added to store
      await useFriendStore.getState().acceptRequest('req-unknown');
      // Should not crash, friends unchanged
      expect(useFriendStore.getState().friends).toHaveLength(0);
    });

    it('throws on API error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ error: 'Already accepted' }, { status: 409 });
        })
      );

      await expect(useFriendStore.getState().acceptRequest('req-1')).rejects.toThrow(
        'Already accepted'
      );
    });
  });

  // ── declineRequest ────────────────────────────────────────────────────

  describe('declineRequest', () => {
    it('declines a request and removes from pending', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Declined' });
        })
      );

      useFriendStore.getState().addRequest(mockRequest);
      await useFriendStore.getState().declineRequest('req-1');
      expect(useFriendStore.getState().pendingRequests).toHaveLength(0);
    });

    it('does not add declined user to friends list', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ message: 'Declined' });
        })
      );

      useFriendStore.getState().addRequest(mockRequest);
      await useFriendStore.getState().declineRequest('req-1');
      expect(useFriendStore.getState().friends).toHaveLength(0);
    });

    it('throws on API error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );

      await expect(useFriendStore.getState().declineRequest('req-1')).rejects.toThrow('Not found');
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/friends/request/:id`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useFriendStore.getState().declineRequest('req-1')).rejects.toThrow(
        'Failed to decline friend request'
      );
    });
  });

  // ── removeFriend ──────────────────────────────────────────────────────

  describe('removeFriend', () => {
    it('removes friend via API and updates state', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/friends/:userId`, () => {
          return HttpResponse.json({ message: 'Removed' });
        })
      );

      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addFriend(mockFriend2);
      await useFriendStore.getState().removeFriend('user-2');

      expect(useFriendStore.getState().friends).toHaveLength(1);
      expect(useFriendStore.getState().friends[0].userId).toBe('user-3');
    });

    it('throws on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/friends/:userId`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await expect(useFriendStore.getState().removeFriend('user-2')).rejects.toThrow('Forbidden');
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/friends/:userId`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useFriendStore.getState().removeFriend('user-2')).rejects.toThrow(
        'Failed to remove friend'
      );
    });
  });

  // ── blockUser ─────────────────────────────────────────────────────────

  describe('blockUser', () => {
    it('blocks a user via API and updates state', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/:userId/block`, () => {
          return HttpResponse.json({ message: 'User blocked' });
        })
      );

      useFriendStore.getState().addFriend(mockFriend);
      await useFriendStore.getState().blockUser('user-2');

      const state = useFriendStore.getState();
      expect(state.friends).toHaveLength(0);
      expect(state.blockedUserIds).toContain('user-2');
    });

    it('adds to blocked list even if not a friend', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/:userId/block`, () => {
          return HttpResponse.json({ message: 'User blocked' });
        })
      );

      await useFriendStore.getState().blockUser('user-99');
      expect(useFriendStore.getState().blockedUserIds).toContain('user-99');
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/:userId/block`, () => {
          return HttpResponse.json({ error: 'Cannot block yourself' }, { status: 400 });
        })
      );

      await expect(useFriendStore.getState().blockUser('user-1')).rejects.toThrow(
        'Cannot block yourself'
      );
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/:userId/block`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useFriendStore.getState().blockUser('user-2')).rejects.toThrow(
        'Failed to block user'
      );
    });
  });

  // ── Friend codes ──────────────────────────────────────────────────────

  describe('fetchFriendCodes', () => {
    it('loads friend codes from API', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.json({
            friend_codes: [
              {
                id: 'fc-1',
                code: 'ABC123',
                max_uses: 5,
                use_count: 2,
                expires_at: '2025-06-01T00:00:00Z',
                is_revoked: false,
                auto_accept: true,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useFriendStore.getState().fetchFriendCodes();
      const codes = useFriendStore.getState().friendCodes;
      expect(codes).toHaveLength(1);
      expect(codes[0].code).toBe('ABC123');
      expect(codes[0].maxUses).toBe(5);
      expect(codes[0].useCount).toBe(2);
      expect(codes[0].autoAccept).toBe(true);
    });

    it('handles API error silently', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.json({ error: 'Error' }, { status: 500 });
        })
      );

      await useFriendStore.getState().fetchFriendCodes();
      // No crash, codes stay empty
      expect(useFriendStore.getState().friendCodes).toEqual([]);
      consoleSpy.mockRestore();
    });

    it('handles network error silently', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.error();
        })
      );

      await useFriendStore.getState().fetchFriendCodes();
      expect(useFriendStore.getState().friendCodes).toEqual([]);
      consoleSpy.mockRestore();
    });
  });

  describe('generateFriendCode', () => {
    it('generates a friend code and prepends to list', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.json({
            friend_code: {
              id: 'fc-new',
              code: 'XYZ789',
              max_uses: 10,
              use_count: 0,
              expires_at: null,
              is_revoked: false,
              auto_accept: false,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      // Pre-populate with an existing code
      useFriendStore.setState({ friendCodes: [mockFriendCode] });

      const result = await useFriendStore.getState().generateFriendCode({ maxUses: 10 });
      expect(result.code).toBe('XYZ789');
      expect(result.maxUses).toBe(10);

      const codes = useFriendStore.getState().friendCodes;
      expect(codes).toHaveLength(2);
      expect(codes[0].code).toBe('XYZ789'); // New code prepended
      expect(codes[1].code).toBe('ABC123'); // Old code at end
    });

    it('passes options to API', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/friends/codes`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({
            friend_code: {
              id: 'fc-new',
              code: 'CODE1',
              max_uses: 3,
              use_count: 0,
              expires_at: null,
              is_revoked: false,
              auto_accept: true,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      await useFriendStore.getState().generateFriendCode({
        maxUses: 3,
        expiresIn: 86400,
        autoAccept: true,
      });

      expect(capturedBody).toEqual({
        max_uses: 3,
        expires_in: 86400,
        auto_accept: true,
      });
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.json({ error: 'Limit reached' }, { status: 429 });
        })
      );

      await expect(useFriendStore.getState().generateFriendCode()).rejects.toThrow('Limit reached');
    });
  });

  describe('revokeFriendCode', () => {
    it('revokes and removes from list', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/friends/codes/:id`, () => {
          return HttpResponse.json({ message: 'Revoked' });
        })
      );

      useFriendStore.setState({ friendCodes: [mockFriendCode] });
      await useFriendStore.getState().revokeFriendCode('fc-1');
      expect(useFriendStore.getState().friendCodes).toHaveLength(0);
    });

    it('throws on API error', async () => {
      server.use(
        http.delete(`${API_BASE}/api/v1/friends/codes/:id`, () => {
          return HttpResponse.json({ error: 'Not found' }, { status: 404 });
        })
      );

      await expect(useFriendStore.getState().revokeFriendCode('fc-999')).rejects.toThrow(
        'Not found'
      );
    });
  });

  describe('previewFriendCode', () => {
    it('returns a preview of the friend code owner', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes/:code`, () => {
          return HttpResponse.json({
            user_id: 'user-5',
            username: 'eve',
            display_name: 'Eve',
            avatar_url: 'https://example.com/eve.png',
            valid: true,
          });
        })
      );

      const preview = await useFriendStore.getState().previewFriendCode('ABC123');
      expect(preview.userId).toBe('user-5');
      expect(preview.username).toBe('eve');
      expect(preview.displayName).toBe('Eve');
      expect(preview.valid).toBe(true);
    });

    it('throws on invalid code', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes/:code`, () => {
          return HttpResponse.json({ error: 'Code expired' }, { status: 404 });
        })
      );

      await expect(useFriendStore.getState().previewFriendCode('INVALID')).rejects.toThrow(
        'Code expired'
      );
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes/:code`, () => {
          return HttpResponse.json({}, { status: 400 });
        })
      );

      await expect(useFriendStore.getState().previewFriendCode('BAD')).rejects.toThrow(
        'Invalid friend code'
      );
    });
  });

  describe('claimFriendCode', () => {
    it('claims a friend code and returns result', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/codes/:code/claim`, () => {
          return HttpResponse.json({
            status: 'accepted',
            friendship_id: 'fs-99',
            user: {
              user_id: 'user-5',
              username: 'eve',
              display_name: 'Eve',
              avatar_url: null,
            },
          });
        })
      );

      const result = await useFriendStore.getState().claimFriendCode('ABC123');
      expect(result.status).toBe('accepted');
      expect(result.friendshipId).toBe('fs-99');
      expect(result.user.id).toBe('user-5');
      expect(result.user.username).toBe('eve');
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/friends/codes/:code/claim`, () => {
          return HttpResponse.json({ error: 'Already friends' }, { status: 409 });
        })
      );

      await expect(useFriendStore.getState().claimFriendCode('ABC123')).rejects.toThrow(
        'Already friends'
      );
    });
  });

  // ── searchUsers ───────────────────────────────────────────────────────

  describe('searchUsers', () => {
    it('returns search results', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/search`, () => {
          return HttpResponse.json({
            users: [
              { id: 'user-5', username: 'eve', display_name: 'Eve', avatar_url: null },
              { id: 'user-6', username: 'frank', display_name: null, avatar_url: null },
            ],
          });
        })
      );

      const results = await useFriendStore.getState().searchUsers('e');
      expect(results).toHaveLength(2);
      expect(results[0].id).toBe('user-5');
      expect(results[0].username).toBe('eve');
      expect(results[1].displayName).toBeNull();
    });

    it('returns empty array on API error', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/search`, () => {
          return HttpResponse.json({ error: 'Error' }, { status: 500 });
        })
      );

      const results = await useFriendStore.getState().searchUsers('test');
      expect(results).toEqual([]);
    });

    it('returns empty array when no users field', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/search`, () => {
          return HttpResponse.json({});
        })
      );

      const results = await useFriendStore.getState().searchUsers('test');
      expect(results).toEqual([]);
    });
  });

  // ── refreshFriendCodeUseCounts ────────────────────────────────────────

  describe('refreshFriendCodeUseCounts', () => {
    it('delegates to fetchFriendCodes', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/friends/codes`, () => {
          return HttpResponse.json({ friend_codes: [] });
        })
      );

      const fetchSpy = vi.spyOn(useFriendStore.getState(), 'fetchFriendCodes');
      useFriendStore.getState().refreshFriendCodeUseCounts();
      expect(fetchSpy).toHaveBeenCalled();
      fetchSpy.mockRestore();
    });
  });

  // ── clearFriends ──────────────────────────────────────────────────────

  describe('clearFriends', () => {
    it('resets all friend state', () => {
      useFriendStore.getState().addFriend(mockFriend);
      useFriendStore.getState().addRequest(mockRequest);
      useFriendStore.setState({ blockedUserIds: ['user-99'], friendCodes: [mockFriendCode] });

      useFriendStore.getState().clearFriends();

      const state = useFriendStore.getState();
      expect(state.friends).toHaveLength(0);
      expect(state.pendingRequests).toHaveLength(0);
      expect(state.blockedUserIds).toHaveLength(0);
      expect(state.friendCodes).toHaveLength(0);
    });
  });
});
