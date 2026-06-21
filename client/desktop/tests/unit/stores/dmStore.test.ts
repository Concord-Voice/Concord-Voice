import { useDMStore, type DMConversation, type DMLastMessage } from '@/renderer/stores/dmStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    getChannelKey: vi.fn(),
    createChannelKeys: vi.fn(),
    clearKeys: vi.fn(),
  },
}));

const API_BASE = 'http://localhost:8080';

const mockConversation: DMConversation = {
  id: 'conv-1',
  isGroup: false,
  isPersonal: false,
  name: null,
  participants: [
    { userId: 'user-1', username: 'alice' },
    { userId: 'user-2', username: 'bob' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-01T00:00:00Z',
};

const mockConversation2: DMConversation = {
  id: 'conv-2',
  isGroup: true,
  isPersonal: false,
  name: 'Group Chat',
  participants: [
    { userId: 'user-1', username: 'alice' },
    { userId: 'user-2', username: 'bob' },
    { userId: 'user-3', username: 'charlie' },
  ],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-02T00:00:00Z',
};

const mockPersonalConv: DMConversation = {
  id: 'conv-personal',
  isGroup: false,
  isPersonal: true,
  name: null,
  participants: [{ userId: 'user-1', username: 'alice' }],
  lastMessage: null,
  unreadCount: 0,
  createdAt: '2025-01-03T00:00:00Z',
};

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
});

describe('dmStore', () => {
  // ── Initial state ─────────────────────────────────────────────────────

  describe('initial state', () => {
    it('starts with empty state', () => {
      const state = useDMStore.getState();
      expect(state.conversations).toEqual([]);
      expect(state.activeConversationId).toBeNull();
      expect(state.isLoading).toBe(false);
      expect(state.error).toBeNull();
      // Removed in #1209: dmCallActive / dmCallConversationId tests
      // (fields deleted; DM call state is on voiceStore now).
    });
  });

  // ── addConversation ───────────────────────────────────────────────────

  describe('addConversation', () => {
    it('adds a conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      expect(useDMStore.getState().conversations).toHaveLength(1);
      expect(useDMStore.getState().conversations[0].id).toBe('conv-1');
    });

    it('does not duplicate conversations', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation);
      expect(useDMStore.getState().conversations).toHaveLength(1);
    });

    it('prepends new conversation to list', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      expect(useDMStore.getState().conversations[0].id).toBe('conv-2');
      expect(useDMStore.getState().conversations[1].id).toBe('conv-1');
    });
  });

  // ── updateConversation ────────────────────────────────────────────────

  describe('updateConversation', () => {
    it('updates conversation properties', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().updateConversation('conv-1', { name: 'Renamed' });
      expect(useDMStore.getState().conversations[0].name).toBe('Renamed');
    });

    it('does not affect other conversations', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      useDMStore.getState().updateConversation('conv-1', { name: 'Renamed' });
      expect(useDMStore.getState().conversations.find((c) => c.id === 'conv-2')?.name).toBe(
        'Group Chat'
      );
    });

    it('can update multiple fields at once', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().updateConversation('conv-1', {
        name: 'New Name',
        unreadCount: 5,
      });
      const conv = useDMStore.getState().conversations[0];
      expect(conv.name).toBe('New Name');
      expect(conv.unreadCount).toBe(5);
    });

    it('is a no-op for non-existent conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().updateConversation('conv-999', { name: 'Ghost' });
      expect(useDMStore.getState().conversations).toHaveLength(1);
      expect(useDMStore.getState().conversations[0].name).toBeNull();
    });
  });

  // ── removeConversation ────────────────────────────────────────────────

  describe('removeConversation', () => {
    it('removes a conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().removeConversation('conv-1');
      expect(useDMStore.getState().conversations).toHaveLength(0);
    });

    it('clears activeConversationId if removed', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().setActiveConversation('conv-1');
      useDMStore.getState().removeConversation('conv-1');
      expect(useDMStore.getState().activeConversationId).toBeNull();
    });

    it('preserves activeConversationId when different conversation is removed', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      useDMStore.getState().setActiveConversation('conv-1');
      useDMStore.getState().removeConversation('conv-2');
      expect(useDMStore.getState().activeConversationId).toBe('conv-1');
    });

    it('only removes the targeted conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      useDMStore.getState().removeConversation('conv-1');
      expect(useDMStore.getState().conversations).toHaveLength(1);
      expect(useDMStore.getState().conversations[0].id).toBe('conv-2');
    });
  });

  // ── updateLastMessage ─────────────────────────────────────────────────

  describe('updateLastMessage', () => {
    it('updates the last message for a conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      const lastMsg: DMLastMessage = {
        content: 'Hello!',
        userId: 'user-1',
        username: 'alice',
        createdAt: '2025-01-01T12:00:00Z',
      };
      useDMStore.getState().updateLastMessage('conv-1', lastMsg);
      expect(useDMStore.getState().conversations[0].lastMessage?.content).toBe('Hello!');
    });

    it('re-sorts conversations by most recent message', () => {
      useDMStore.getState().addConversation(mockConversation); // created 2025-01-01
      useDMStore.getState().addConversation(mockConversation2); // created 2025-01-02 (prepended first)

      // Send a newer message to conv-1 (which is currently second/last in list)
      const lastMsg: DMLastMessage = {
        content: 'Newer message',
        userId: 'user-1',
        username: 'alice',
        createdAt: '2025-06-01T00:00:00Z',
      };
      useDMStore.getState().updateLastMessage('conv-1', lastMsg);

      // conv-1 should now be first (most recent)
      expect(useDMStore.getState().conversations[0].id).toBe('conv-1');
    });

    it('sorts using createdAt as fallback when no lastMessage', () => {
      useDMStore.getState().addConversation(mockConversation); // 2025-01-01
      useDMStore.getState().addConversation(mockConversation2); // 2025-01-02

      // Send message only to conv-1
      const msg: DMLastMessage = {
        content: 'early',
        userId: 'user-1',
        username: 'alice',
        createdAt: '2024-01-01T00:00:00Z', // older than conv-2's createdAt
      };
      useDMStore.getState().updateLastMessage('conv-1', msg);

      // conv-2 (no lastMessage, createdAt=2025-01-02) should sort before conv-1 (lastMessage=2024)
      expect(useDMStore.getState().conversations[0].id).toBe('conv-2');
    });
  });

  // ── bumpConversation ──────────────────────────────────────────────────

  describe('bumpConversation', () => {
    it('updates last message and bumps conversation to top', () => {
      useDMStore.getState().addConversation(mockConversation); // 2025-01-01
      useDMStore.getState().addConversation(mockConversation2); // 2025-01-02 (prepended first)

      const msg: DMLastMessage = {
        content: 'Newest ping',
        userId: 'user-2',
        username: 'bob',
        createdAt: '2025-09-01T00:00:00Z',
      };
      useDMStore.getState().bumpConversation('conv-1', msg);

      const convs = useDMStore.getState().conversations;
      expect(convs[0].id).toBe('conv-1');
      expect(convs[0].lastMessage?.content).toBe('Newest ping');
    });

    it('re-sorts without mutating other conversations', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);

      const msg: DMLastMessage = {
        content: 'hi',
        userId: 'user-1',
        username: 'alice',
        createdAt: '2025-12-01T00:00:00Z',
      };
      useDMStore.getState().bumpConversation('conv-1', msg);

      const convs = useDMStore.getState().conversations;
      expect(convs).toHaveLength(2);
      const conv2 = convs.find((c) => c.id === 'conv-2');
      expect(conv2?.lastMessage).toBeNull();
    });

    it('is a no-op when the conversation is not in state', () => {
      useDMStore.getState().addConversation(mockConversation);
      const before = useDMStore.getState().conversations;

      const msg: DMLastMessage = {
        content: 'ghost',
        userId: 'user-9',
        username: 'noone',
        createdAt: '2025-12-01T00:00:00Z',
      };
      useDMStore.getState().bumpConversation('conv-unknown', msg);

      // Same array reference — store did not mutate
      expect(useDMStore.getState().conversations).toBe(before);
    });
  });

  // ── Unread management ─────────────────────────────────────────────────

  describe('unread management', () => {
    it('incrementUnread increases count', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().incrementUnread('conv-1');
      useDMStore.getState().incrementUnread('conv-1');
      expect(useDMStore.getState().conversations[0].unreadCount).toBe(2);
    });

    it('incrementUnread is a no-op for non-existent conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().incrementUnread('conv-999');
      expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    });

    it('clearUnread resets count to 0', () => {
      useDMStore.getState().addConversation({ ...mockConversation, unreadCount: 5 });
      useDMStore.getState().clearUnread('conv-1');
      expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    });

    it('clearUnread on already-zero is a no-op', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().clearUnread('conv-1');
      expect(useDMStore.getState().conversations[0].unreadCount).toBe(0);
    });
  });

  // ── setActiveConversation ─────────────────────────────────────────────

  describe('setActiveConversation', () => {
    it('sets the active conversation', () => {
      useDMStore.getState().setActiveConversation('conv-1');
      expect(useDMStore.getState().activeConversationId).toBe('conv-1');
    });

    it('can set to null', () => {
      useDMStore.getState().setActiveConversation('conv-1');
      useDMStore.getState().setActiveConversation(null);
      expect(useDMStore.getState().activeConversationId).toBeNull();
    });

    it('can switch between conversations', () => {
      useDMStore.getState().setActiveConversation('conv-1');
      useDMStore.getState().setActiveConversation('conv-2');
      expect(useDMStore.getState().activeConversationId).toBe('conv-2');
    });
  });

  // Removed in #1209: DM voice call state describe block — fields
  // dmCallActive / dmCallConversationId / setDMCallActive deleted.
  // DM call state lives on voiceStore (isDMCall, dmConversationId,
  // callState). The 4 tests below were tightly bound to the deleted
  // fields with no salvageable assertions for the new layout.

  // ── updateParticipantProfile ──────────────────────────────────────────

  describe('updateParticipantProfile', () => {
    it('updates participant across all conversations', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2); // also has user-2
      useDMStore.getState().updateParticipantProfile('user-2', { displayName: 'Bobby' });

      const conv1 = useDMStore.getState().conversations.find((c) => c.id === 'conv-1');
      const conv2 = useDMStore.getState().conversations.find((c) => c.id === 'conv-2');
      expect(conv1?.participants.find((p) => p.userId === 'user-2')?.displayName).toBe('Bobby');
      expect(conv2?.participants.find((p) => p.userId === 'user-2')?.displayName).toBe('Bobby');
    });

    it('does not affect other participants', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().updateParticipantProfile('user-2', { displayName: 'Bobby' });
      const alice = useDMStore
        .getState()
        .conversations[0].participants.find((p) => p.userId === 'user-1');
      expect(alice?.displayName).toBeUndefined();
    });

    it('updates multiple profile fields', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().updateParticipantProfile('user-2', {
        displayName: 'Bobby',
        avatarUrl: 'https://example.com/new.png',
        colorScheme: 'hacker',
        status: 'dnd',
      });
      const bob = useDMStore
        .getState()
        .conversations[0].participants.find((p) => p.userId === 'user-2');
      expect(bob?.displayName).toBe('Bobby');
      expect(bob?.avatarUrl).toBe('https://example.com/new.png');
      expect(bob?.colorScheme).toBe('hacker');
      expect(bob?.status).toBe('dnd');
    });
  });

  // ── fetchConversations ────────────────────────────────────────────────

  describe('fetchConversations', () => {
    it('fetches and populates conversations', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [
                  { user_id: 'user-1', username: 'alice' },
                  { user_id: 'user-2', username: 'bob' },
                ],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const state = useDMStore.getState();
      expect(state.isLoading).toBe(false);
      expect(state.conversations).toHaveLength(1);
      expect(state.conversations[0].id).toBe('conv-api-1');
      expect(state.conversations[0].participants).toHaveLength(2);
    });

    it('maps last_message from API response', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: {
                  content: 'Hi!',
                  user_id: 'user-1',
                  username: 'alice',
                  created_at: '2025-01-01T12:00:00Z',
                },
                unread_count: 3,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.lastMessage?.content).toBe('Hi!');
      expect(conv.lastMessage?.userId).toBe('user-1');
      expect(conv.unreadCount).toBe(3);
    });

    it('sets error on API failure', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({ error: 'Forbidden' }, { status: 403 });
        })
      );

      await useDMStore.getState().fetchConversations();
      expect(useDMStore.getState().error).toBe('Forbidden');
      expect(useDMStore.getState().isLoading).toBe(false);
    });

    it('uses generic error message when none provided', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await useDMStore.getState().fetchConversations();
      expect(useDMStore.getState().error).toBe('Failed to load conversations');
    });

    it('validates persisted activeConversationId against fetched conversations', async () => {
      // Set an active conversation that will not be in the API response
      useDMStore.setState({ activeConversationId: 'conv-gone' });

      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      // Since 'conv-gone' is not in the response, activeConversationId should be cleared
      expect(useDMStore.getState().activeConversationId).toBeNull();
    });

    it('preserves activeConversationId when it exists in fetched conversations', async () => {
      useDMStore.setState({ activeConversationId: 'conv-api-1' });

      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      expect(useDMStore.getState().activeConversationId).toBe('conv-api-1');
    });

    it('handles network error', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.error();
        })
      );

      await useDMStore.getState().fetchConversations();
      // Network errors produce the native fetch error message
      expect(useDMStore.getState().error).toBe('Failed to fetch');
    });

    it('handles empty conversations list', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({ conversations: [] });
        })
      );

      await useDMStore.getState().fetchConversations();
      expect(useDMStore.getState().conversations).toEqual([]);
    });

    it('does not fetch if already loading', async () => {
      let callCount = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          callCount++;
          return HttpResponse.json({ conversations: [] });
        })
      );

      useDMStore.setState({ isLoading: true });
      await useDMStore.getState().fetchConversations();
      expect(callCount).toBe(0);
    });
  });

  // ── openDM ────────────────────────────────────────────────────────────

  describe('openDM', () => {
    it('opens a DM and sets it as active', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-new',
              is_group: false,
              is_personal: false,
              name: null,
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      const conv = await useDMStore.getState().openDM('user-2');
      expect(conv.id).toBe('conv-new');
      expect(useDMStore.getState().activeConversationId).toBe('conv-new');
      expect(useDMStore.getState().conversations).toHaveLength(1);
    });

    it('does not duplicate conversation if already present', async () => {
      useDMStore.getState().addConversation(mockConversation);

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-1', // same as mockConversation
              is_group: false,
              is_personal: false,
              name: null,
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      expect(useDMStore.getState().conversations).toHaveLength(1);
      expect(useDMStore.getState().activeConversationId).toBe('conv-1');
    });

    it('throws privacy error for dm_disabled', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({ error: 'dm_disabled' }, { status: 403 });
        })
      );

      await expect(useDMStore.getState().openDM('user-2')).rejects.toThrow(
        "This user isn't accepting DMs right now"
      );
    });

    it('throws privacy error for privacy_blocked', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({ error: 'privacy_blocked' }, { status: 403 });
        })
      );

      await expect(useDMStore.getState().openDM('user-2')).rejects.toThrow(
        "This user isn't accepting DMs right now"
      );
    });

    it('throws generic error for other API failures', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({ error: 'Server error' }, { status: 500 });
        })
      );

      await expect(useDMStore.getState().openDM('user-2')).rejects.toThrow('Server error');
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useDMStore.getState().openDM('user-2')).rejects.toThrow('Failed to open DM');
    });
  });

  // ── createGroupDM ─────────────────────────────────────────────────────

  describe('createGroupDM', () => {
    it('creates a group DM and sets it as active', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/group`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-group-new',
              is_group: true,
              is_personal: false,
              name: 'My Group',
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
                { user_id: 'user-3', username: 'charlie' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      const conv = await useDMStore.getState().createGroupDM(['user-2', 'user-3'], 'My Group');
      expect(conv.id).toBe('conv-group-new');
      expect(conv.isGroup).toBe(true);
      expect(conv.name).toBe('My Group');
      expect(useDMStore.getState().activeConversationId).toBe('conv-group-new');
    });

    it('sends correct body to API', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/group`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({
            conversation: {
              id: 'conv-group-new',
              is_group: true,
              is_personal: false,
              name: null,
              participants: [],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      await useDMStore.getState().createGroupDM(['user-2', 'user-3']);
      expect(capturedBody).toEqual({ user_ids: ['user-2', 'user-3'] });
    });

    it('includes name when provided', async () => {
      let capturedBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/group`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({
            conversation: {
              id: 'conv-group-new',
              is_group: true,
              is_personal: false,
              name: 'Team Chat',
              participants: [],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      await useDMStore.getState().createGroupDM(['user-2'], 'Team Chat');
      expect(capturedBody).toEqual({ user_ids: ['user-2'], name: 'Team Chat' });
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/group`, () => {
          return HttpResponse.json({ error: 'Too many members' }, { status: 400 });
        })
      );

      await expect(useDMStore.getState().createGroupDM(['user-2', 'user-3'])).rejects.toThrow(
        'Too many members'
      );
    });
  });

  // ── openPersonalThread ────────────────────────────────────────────────

  describe('openPersonalThread', () => {
    it('opens a personal thread and sets it active', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-personal-new',
              is_group: false,
              is_personal: true,
              name: null,
              participants: [{ user_id: 'user-1', username: 'alice' }],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      const conv = await useDMStore.getState().openPersonalThread();
      expect(conv.id).toBe('conv-personal-new');
      expect(conv.isPersonal).toBe(true);
      expect(useDMStore.getState().activeConversationId).toBe('conv-personal-new');
    });

    it('does not duplicate personal thread if already in list', async () => {
      useDMStore.getState().addConversation(mockPersonalConv);

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-personal', // same ID
              is_group: false,
              is_personal: true,
              name: null,
              participants: [{ user_id: 'user-1', username: 'alice' }],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-03T00:00:00Z',
            },
          });
        })
      );

      await useDMStore.getState().openPersonalThread();
      expect(useDMStore.getState().conversations).toHaveLength(1);
    });

    it('throws on API error', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({ error: 'Personal threads disabled' }, { status: 403 });
        })
      );

      await expect(useDMStore.getState().openPersonalThread()).rejects.toThrow(
        'Personal threads disabled'
      );
    });

    it('uses generic error when none provided', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({}, { status: 500 });
        })
      );

      await expect(useDMStore.getState().openPersonalThread()).rejects.toThrow(
        'Failed to open personal thread'
      );
    });
  });

  // ── clearDMs ──────────────────────────────────────────────────────────

  describe('clearDMs', () => {
    it('resets all DM state', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().setActiveConversation('conv-1');

      useDMStore.getState().clearDMs();

      const state = useDMStore.getState();
      expect(state.conversations).toHaveLength(0);
      expect(state.activeConversationId).toBeNull();
      // Removed in #1209: dmCallActive / dmCallConversationId assertions
      // (fields deleted; DM call state is on voiceStore now).
    });
  });

  // ── mapConversation with new fields ───────────────────────────────

  describe('mapConversation with new fields', () => {
    it('maps role, iconUrl, and createdBy from API response', async () => {
      // Ensure isLoading is false (clearDMs does not reset it)
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'group-1',
                is_group: true,
                is_personal: false,
                name: 'Test Group',
                icon_url: '/api/v1/media/dm-icons/group-1',
                created_by: 'user-1',
                participants: [
                  { user_id: 'user-1', username: 'alice', role: 'admin' },
                  { user_id: 'user-2', username: 'bob', role: 'member' },
                ],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );
      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.iconUrl).toBe('/api/v1/media/dm-icons/group-1');
      expect(conv.createdBy).toBe('user-1');
      expect(conv.participants[0].role).toBe('admin');
      expect(conv.participants[1].role).toBe('member');
    });

    it('maps undefined iconUrl and createdBy when absent', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-no-icon',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [{ user_id: 'user-1', username: 'alice' }],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );
      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.iconUrl).toBeUndefined();
      expect(conv.createdBy).toBeUndefined();
    });
  });

  // ── group management actions ──────────────────────────────────────

  describe('group management actions', () => {
    const mockGroupConv: DMConversation = {
      id: 'group-1',
      isGroup: true,
      isPersonal: false,
      name: 'Test Group',
      createdBy: 'user-1',
      participants: [
        { userId: 'user-1', username: 'alice', role: 'admin' },
        { userId: 'user-2', username: 'bob', role: 'member' },
      ],
      lastMessage: null,
      unreadCount: 0,
      createdAt: '2025-01-01T00:00:00Z',
    };

    describe('addGroupMember', () => {
      it('adds a member and updates conversation', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.post(`${API_BASE}/api/v1/dm/conversations/group-1/members`, () => {
            return HttpResponse.json({
              conversation: {
                id: 'group-1',
                is_group: true,
                is_personal: false,
                name: 'Test Group',
                created_by: 'user-1',
                participants: [
                  { user_id: 'user-1', username: 'alice', role: 'admin' },
                  { user_id: 'user-2', username: 'bob', role: 'member' },
                  { user_id: 'user-3', username: 'charlie', role: 'member' },
                ],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            });
          })
        );

        await useDMStore.getState().addGroupMember('group-1', 'user-3');
        const conv = useDMStore.getState().conversations.find((c) => c.id === 'group-1');
        expect(conv?.participants).toHaveLength(3);
        expect(conv?.participants[2].username).toBe('charlie');
      });

      it('throws on API error', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.post(`${API_BASE}/api/v1/dm/conversations/group-1/members`, () => {
            return HttpResponse.json({ error: 'Not authorized' }, { status: 403 });
          })
        );

        await expect(useDMStore.getState().addGroupMember('group-1', 'user-3')).rejects.toThrow(
          'Not authorized'
        );
      });
    });

    describe('removeGroupMember', () => {
      it('calls DELETE endpoint successfully', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await expect(
          useDMStore.getState().removeGroupMember('group-1', 'user-2')
        ).resolves.toBeUndefined();
      });

      it('throws on non-admin', async () => {
        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({ error: 'Only admins can remove members' }, { status: 403 });
          })
        );

        await expect(useDMStore.getState().removeGroupMember('group-1', 'user-2')).rejects.toThrow(
          'Only admins can remove members'
        );
      });
    });

    describe('leaveGroup', () => {
      it('removes conversation from local state', async () => {
        useDMStore.getState().addConversation(mockGroupConv);
        useDMStore.getState().setActiveConversation('group-1');

        // leaveGroup dynamically imports userStore — set user state
        const { useUserStore } = await import('@/renderer/stores/userStore');
        useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().leaveGroup('group-1');
        expect(useDMStore.getState().conversations.find((c) => c.id === 'group-1')).toBeUndefined();
        expect(useDMStore.getState().activeConversationId).toBeNull();
      });
    });

    describe('updateMemberRole', () => {
      it('optimistically updates participant role', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.patch(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().updateMemberRole('group-1', 'user-2', 'admin');
        const conv = useDMStore.getState().conversations.find((c) => c.id === 'group-1');
        const bob = conv?.participants.find((p) => p.userId === 'user-2');
        expect(bob?.role).toBe('admin');
      });

      it('throws on API error', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.patch(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({ error: 'Not authorized' }, { status: 403 });
          })
        );

        await expect(
          useDMStore.getState().updateMemberRole('group-1', 'user-2', 'admin')
        ).rejects.toThrow('Not authorized');
      });
    });

    describe('deleteGroup', () => {
      it('removes conversation from state', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().deleteGroup('group-1');
        expect(useDMStore.getState().conversations.find((c) => c.id === 'group-1')).toBeUndefined();
      });

      it('clears activeConversationId if deleted group was active', async () => {
        useDMStore.getState().addConversation(mockGroupConv);
        useDMStore.getState().setActiveConversation('group-1');

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().deleteGroup('group-1');
        expect(useDMStore.getState().activeConversationId).toBeNull();
      });

      it('preserves activeConversationId if different group deleted', async () => {
        useDMStore.getState().addConversation(mockGroupConv);
        useDMStore.getState().addConversation(mockConversation);
        useDMStore.getState().setActiveConversation('conv-1');

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().deleteGroup('group-1');
        expect(useDMStore.getState().activeConversationId).toBe('conv-1');
      });

      it('throws on API error', async () => {
        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1`, () => {
            return HttpResponse.json({ error: 'Not the group creator' }, { status: 403 });
          })
        );

        await expect(useDMStore.getState().deleteGroup('group-1')).rejects.toThrow(
          'Not the group creator'
        );
      });

      it('uses generic error when none provided', async () => {
        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1`, () => {
            return HttpResponse.json({}, { status: 500 });
          })
        );

        await expect(useDMStore.getState().deleteGroup('group-1')).rejects.toThrow(
          'Failed to delete group'
        );
      });
    });

    describe('addGroupMember - edge cases', () => {
      it('does not update state when response has no conversation field', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.post(`${API_BASE}/api/v1/dm/conversations/group-1/members`, () => {
            // Response without conversation field
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().addGroupMember('group-1', 'user-3');
        const conv = useDMStore.getState().conversations.find((c) => c.id === 'group-1');
        // Participants unchanged since no conversation in response
        expect(conv?.participants).toHaveLength(2);
      });

      it('uses generic error when none provided', async () => {
        server.use(
          http.post(`${API_BASE}/api/v1/dm/conversations/group-1/members`, () => {
            return HttpResponse.json({}, { status: 500 });
          })
        );

        await expect(useDMStore.getState().addGroupMember('group-1', 'user-3')).rejects.toThrow(
          'Failed to add member'
        );
      });
    });

    describe('removeGroupMember - edge cases', () => {
      it('uses generic error when none provided', async () => {
        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({}, { status: 500 });
          })
        );

        await expect(useDMStore.getState().removeGroupMember('group-1', 'user-2')).rejects.toThrow(
          'Failed to remove member'
        );
      });
    });

    describe('leaveGroup - edge cases', () => {
      it('throws when user is not authenticated', async () => {
        const { useUserStore } = await import('@/renderer/stores/userStore');
        useUserStore.setState({ user: null });

        await expect(useDMStore.getState().leaveGroup('group-1')).rejects.toThrow(
          'Not authenticated'
        );
      });

      it('throws on API error with message', async () => {
        const { useUserStore } = await import('@/renderer/stores/userStore');
        useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-1`, () => {
            return HttpResponse.json({ error: 'Cannot leave group' }, { status: 400 });
          })
        );

        await expect(useDMStore.getState().leaveGroup('group-1')).rejects.toThrow(
          'Cannot leave group'
        );
      });

      it('uses generic error when none provided', async () => {
        const { useUserStore } = await import('@/renderer/stores/userStore');
        useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-1`, () => {
            return HttpResponse.json({}, { status: 500 });
          })
        );

        await expect(useDMStore.getState().leaveGroup('group-1')).rejects.toThrow(
          'Failed to leave group'
        );
      });

      it('preserves activeConversationId if leaving a different group', async () => {
        useDMStore.getState().addConversation(mockGroupConv);
        useDMStore.getState().addConversation(mockConversation);
        useDMStore.getState().setActiveConversation('conv-1');

        const { useUserStore } = await import('@/renderer/stores/userStore');
        useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().leaveGroup('group-1');
        expect(useDMStore.getState().activeConversationId).toBe('conv-1');
      });
    });

    describe('updateMemberRole - edge cases', () => {
      it('uses generic error when none provided', async () => {
        useDMStore.getState().addConversation(mockGroupConv);

        server.use(
          http.patch(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-2`, () => {
            return HttpResponse.json({}, { status: 500 });
          })
        );

        await expect(
          useDMStore.getState().updateMemberRole('group-1', 'user-2', 'admin')
        ).rejects.toThrow('Failed to update role');
      });
    });
  });

  // ── mapConversation edge cases ──────────────────────────────────────

  describe('mapConversation edge cases', () => {
    it('handles null participants array', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-null-parts',
                is_group: false,
                is_personal: false,
                name: null,
                participants: null,
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.participants).toEqual([]);
    });

    it('handles missing is_personal with false default', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-no-personal',
                is_group: false,
                // is_personal omitted
                name: null,
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.isPersonal).toBe(false);
    });

    it('handles missing unread_count with 0 default', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-no-unread',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: null,
                // unread_count omitted
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.unreadCount).toBe(0);
    });

    it('maps name to null when empty string', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-empty-name',
                is_group: false,
                is_personal: false,
                name: '',
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      await useDMStore.getState().fetchConversations();
      const conv = useDMStore.getState().conversations[0];
      expect(conv.name).toBeNull();
    });

    it('handles missing conversations key in API response', async () => {
      useDMStore.setState({ isLoading: false });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({});
        })
      );

      await useDMStore.getState().fetchConversations();
      expect(useDMStore.getState().conversations).toEqual([]);
    });
  });

  // ── E2EE key distribution paths ─────────────────────────────────────

  describe('E2EE key distribution paths', () => {
    let e2eeMock: {
      isInitialized: boolean;
      getChannelKey: ReturnType<typeof vi.fn>;
      createChannelKeys: ReturnType<typeof vi.fn>;
      clearKeys: ReturnType<typeof vi.fn>;
    };

    beforeEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      e2eeMock = mod.e2eeService as typeof e2eeMock;
      e2eeMock.isInitialized = true;
      e2eeMock.getChannelKey.mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', true));
      e2eeMock.createChannelKeys.mockResolvedValue(
        new Map([
          ['user-1', 'wrapped-key-1'],
          ['user-2', 'wrapped-key-2'],
        ])
      );
    });

    afterEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      const mock = mod.e2eeService as typeof e2eeMock;
      mock.isInitialized = false;
      mock.getChannelKey.mockReset();
      mock.createChannelKeys.mockReset();
    });

    it('createGroupDM distributes E2EE keys for encrypted group', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/group`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-e2ee-group',
              is_group: true,
              is_personal: false,
              name: 'Encrypted Group',
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () => {
          return HttpResponse.json({ public_key: 'mock-public-key' });
        }),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, () => {
          return HttpResponse.json({ success: true });
        })
      );

      await useDMStore.getState().createGroupDM(['user-2'], 'Encrypted Group');
      expect(e2eeMock.createChannelKeys).toHaveBeenCalled();
      expect(useDMStore.getState().activeConversationId).toBe('conv-e2ee-group');
    });

    it('openDM calls ensureE2EEKey for encrypted conversation', async () => {
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-e2ee-dm',
              is_group: false,
              is_personal: false,
              name: null,
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () => {
          return HttpResponse.json({ public_key: 'mock-public-key' });
        }),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, () => {
          return HttpResponse.json({ success: true });
        })
      );

      await useDMStore.getState().openDM('user-2');
      // ensureE2EEKey should have called getChannelKey, then distributed keys
      expect(e2eeMock.getChannelKey).toHaveBeenCalledWith('conv-e2ee-dm');
      expect(e2eeMock.createChannelKeys).toHaveBeenCalled();
    });

    it('openPersonalThread distributes E2EE key when key does not exist', async () => {
      // getChannelKey rejects (no existing key)
      e2eeMock.getChannelKey.mockRejectedValue(new Error('NO_KEY'));

      // userStore needs to have user set for personal thread E2EE path
      const { useUserStore } = await import('@/renderer/stores/userStore');
      useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-e2ee-personal',
              is_group: false,
              is_personal: true,
              name: null,
              participants: [{ user_id: 'user-1', username: 'alice' }],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        }),
        http.get(`${API_BASE}/api/v1/users/user-1/public-key`, () => {
          return HttpResponse.json({ public_key: 'mock-public-key' });
        }),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, () => {
          return HttpResponse.json({ success: true });
        })
      );

      await useDMStore.getState().openPersonalThread();
      expect(e2eeMock.getChannelKey).toHaveBeenCalledWith('conv-e2ee-personal');
      expect(e2eeMock.createChannelKeys).toHaveBeenCalled();
    });

    it('openPersonalThread skips key distribution when key already exists', async () => {
      // getChannelKey resolves (key exists)
      e2eeMock.getChannelKey.mockResolvedValue('existing-key');

      const { useUserStore } = await import('@/renderer/stores/userStore');
      useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-e2ee-personal-2',
              is_group: false,
              is_personal: true,
              name: null,
              participants: [{ user_id: 'user-1', username: 'alice' }],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        })
      );

      await useDMStore.getState().openPersonalThread();
      // Key already existed — should NOT create new keys
      expect(e2eeMock.createChannelKeys).not.toHaveBeenCalled();
    });

    it('logs error when createChannelKeys throws during DM E2EE key distribution', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      // createChannelKeys throws → distributeChannelKeys throws → outer catch fires
      e2eeMock.createChannelKeys.mockRejectedValueOnce(new Error('key wrap failed'));
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () => {
          return HttpResponse.json({
            conversation: {
              id: 'conv-e2ee-fail',
              is_group: false,
              is_personal: false,
              name: null,
              participants: [
                { user_id: 'user-1', username: 'alice' },
                { user_id: 'user-2', username: 'bob' },
              ],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          });
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () => {
          return HttpResponse.json({ public_key: 'mock-public-key' });
        })
      );

      // openDM triggers ensureE2EEKey → distributeChannelKeys → createChannelKeys throws
      await useDMStore.getState().openDM('user-2');

      await vi.waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith(
          'Failed to distribute E2EE key for DM:',
          'key wrap failed'
        );
      });
      consoleSpy.mockRestore();
    });
  });
});
