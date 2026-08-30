import {
  useDMStore,
  type DMConversation,
  type DMLastMessage,
} from '@/renderer/stores/chat/dmStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';
import { clearIndex, indexMessage, isIndexed } from '@/renderer/services/searchService';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { mockMessage } from '../../mocks/fixtures';
import { http, HttpResponse } from 'msw';
import { deferred } from '../../helpers/deferred';

const mockInvalidateChannelKey = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: false,
    getChannelKey: vi.fn(),
    createChannelKeys: vi.fn(),
    clearKeys: vi.fn(),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
    revokeChannelAccess: (...args: unknown[]) => mockInvalidateChannelKey(...args),
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
  vi.clearAllMocks();
  clearIndex();
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

    it('removes only the deleted conversation from the in-memory search index', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      indexMessage('removed-dm-message', 'revoked DM plaintext', 'conv-1');
      indexMessage('retained-dm-message', 'retained DM plaintext', 'conv-2');

      useDMStore.getState().removeConversation('conv-1');

      expect(isIndexed('removed-dm-message')).toBe(false);
      expect(isIndexed('retained-dm-message')).toBe(true);
    });

    it('purges chat rows and cached key state for the removed conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useChatStore.getState().addMessage('conv-1', {
        ...mockMessage,
        id: 'dm-access-revoked',
        channel_id: 'conv-1',
      });

      useDMStore.getState().removeConversation('conv-1');

      expect(useChatStore.getState().messagesByChannel.get('conv-1')).toBeUndefined();
      expect(mockInvalidateChannelKey).toHaveBeenCalledOnce();
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('conv-1');
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

    it('maps call-event metadata from the last message', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () =>
          HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: {
                  content: '',
                  user_id: 'user-1',
                  created_at: '2026-07-13T12:00:00Z',
                  type: 'call_event',
                  call_event_payload: {
                    caller_user_id: 'user-1',
                    started_at: '2026-07-13T12:00:00Z',
                    status: 'missed',
                    duration_seconds: 0,
                  },
                },
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          })
        )
      );

      await useDMStore.getState().fetchConversations();

      expect(useDMStore.getState().conversations[0].lastMessage).toMatchObject({
        type: 'call_event',
        callEventPayload: {
          caller_user_id: 'user-1',
          status: 'missed',
        },
      });
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

    it('queues one authoritative refetch when requested during an active fetch', async () => {
      const firstStarted = deferred();
      const releaseFirst = deferred();
      let callCount = 0;
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, async () => {
          callCount++;
          if (callCount === 1) {
            firstStarted.resolve();
            await releaseFirst.promise;
          }
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-api-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message:
                  callCount === 1
                    ? null
                    : {
                        content: '',
                        user_id: 'user-1',
                        created_at: '2026-07-13T12:00:00Z',
                        type: 'call_event',
                        call_event_payload: {
                          caller_user_id: 'user-1',
                          started_at: '2026-07-13T12:00:00Z',
                          status: 'missed',
                          duration_seconds: 0,
                        },
                      },
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      const firstFetch = useDMStore.getState().fetchConversations();
      await firstStarted.promise;
      await useDMStore.getState().fetchConversations();
      expect(callCount).toBe(1);

      releaseFirst.resolve();
      await firstFetch;

      await vi.waitFor(() => expect(callCount).toBe(2));
      await vi.waitFor(() =>
        expect(useDMStore.getState().conversations[0].lastMessage?.type).toBe('call_event')
      );
    });

    it('purges conversations missing from an authoritative re-fetch', async () => {
      useDMStore.setState({ isLoading: false });
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      useChatStore.getState().addMessage('conv-1', {
        ...mockMessage,
        id: 'removed-dm-message',
        channel_id: 'conv-1',
      });
      useChatStore.getState().addMessage('conv-2', {
        ...mockMessage,
        id: 'retained-dm-message',
        channel_id: 'conv-2',
      });
      indexMessage('removed-dm-message', 'revoked plaintext', 'conv-1');
      indexMessage('retained-dm-message', 'retained plaintext', 'conv-2');
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, () =>
          HttpResponse.json({
            conversations: [
              {
                id: 'conv-2',
                is_group: true,
                is_personal: false,
                name: 'Group Chat',
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-02T00:00:00Z',
              },
            ],
          })
        )
      );

      await useDMStore.getState().fetchConversations();

      expect(mockInvalidateChannelKey).toHaveBeenCalledOnce();
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('conv-1');
      expect(useChatStore.getState().messagesByChannel.has('conv-1')).toBe(false);
      expect(useChatStore.getState().messagesByChannel.has('conv-2')).toBe(true);
      expect(isIndexed('removed-dm-message')).toBe(false);
      expect(isIndexed('retained-dm-message')).toBe(true);
    });

    it('preserves newer message and unread updates while a stale response is in flight', async () => {
      const started = deferred();
      const release = deferred();
      useDMStore.getState().addConversation({ ...mockConversation, unreadCount: 5 });
      useDMStore.getState().addConversation(mockConversation2);
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-2',
                is_group: true,
                is_personal: false,
                name: 'Group Chat',
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-02T00:00:00Z',
              },
              {
                id: 'conv-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: {
                  content: 'stale preview',
                  user_id: 'user-2',
                  username: 'bob',
                  created_at: '2025-01-01T12:00:00Z',
                },
                unread_count: 9,
                created_at: '2025-01-01T00:00:00Z',
              },
            ],
          });
        })
      );

      const fetchPromise = useDMStore.getState().fetchConversations();
      await started.promise;
      useDMStore.getState().updateLastMessage('conv-1', {
        content: 'new WebSocket preview',
        userId: 'user-1',
        username: 'alice',
        createdAt: '2026-01-01T00:00:00Z',
      });
      useDMStore.getState().clearUnread('conv-1');
      release.resolve();
      await fetchPromise;

      const conversations = useDMStore.getState().conversations;
      expect(conversations.map((conversation) => conversation.id)).toEqual(['conv-1', 'conv-2']);
      expect(conversations[0].lastMessage?.content).toBe('new WebSocket preview');
      expect(conversations[0].unreadCount).toBe(0);
    });

    it('preserves participant, role, profile, presence, and name updates during a stale fetch', async () => {
      const started = deferred();
      const release = deferred();
      useDMStore.getState().addConversation({
        ...mockConversation2,
        name: 'Original group',
        participants: mockConversation2.participants.map((participant) => ({
          ...participant,
          role: participant.userId === 'user-1' ? 'admin' : 'member',
          status: 'offline',
        })),
      });
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-2',
                is_group: true,
                is_personal: false,
                name: 'Original group',
                participants: [
                  { user_id: 'user-1', username: 'alice', role: 'admin', status: 'offline' },
                  {
                    user_id: 'user-2',
                    username: 'bob',
                    display_name: 'Stale Bob',
                    avatar_url: 'stale.png',
                    color_scheme: 'stale',
                    role: 'member',
                    status: 'offline',
                  },
                  { user_id: 'user-3', username: 'charlie', role: 'member', status: 'offline' },
                ],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-02T00:00:00Z',
              },
            ],
          });
        })
      );

      const fetchPromise = useDMStore.getState().fetchConversations();
      await started.promise;

      const beforeMembershipUpdate = useDMStore.getState().conversations[0];
      useDMStore.getState().updateConversation('conv-2', {
        participants: [
          ...beforeMembershipUpdate.participants,
          { userId: 'user-4', username: 'dana', role: 'member', status: 'online' },
        ],
      });
      const beforeRoleUpdate = useDMStore.getState().conversations[0];
      useDMStore.getState().updateConversation('conv-2', {
        participants: beforeRoleUpdate.participants.map((participant) =>
          participant.userId === 'user-2' ? { ...participant, role: 'admin' } : participant
        ),
      });
      useDMStore.getState().updateParticipantProfile('user-2', {
        username: 'robert',
        displayName: 'Robert',
        avatarUrl: 'fresh.png',
        colorScheme: 'fresh',
        status: 'online',
      });
      useDMStore.getState().updateConversation('conv-2', { name: 'Renamed live' });

      release.resolve();
      await fetchPromise;

      const conversation = useDMStore.getState().conversations[0];
      expect(conversation.name).toBe('Renamed live');
      expect(conversation.participants.map((participant) => participant.userId)).toEqual([
        'user-1',
        'user-2',
        'user-3',
        'user-4',
      ]);
      expect(
        conversation.participants.find((participant) => participant.userId === 'user-2')
      ).toMatchObject({
        username: 'robert',
        displayName: 'Robert',
        avatarUrl: 'fresh.png',
        colorScheme: 'fresh',
        status: 'online',
        role: 'admin',
      });
    });

    it('keeps conversations added after fetch start without purging their access state', async () => {
      const started = deferred();
      const release = deferred();
      useDMStore.getState().addConversation(mockConversation);
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-1',
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

      const fetchPromise = useDMStore.getState().fetchConversations();
      await started.promise;
      useDMStore.getState().addConversation(mockConversation2);
      useDMStore.getState().setActiveConversation('conv-2');
      useChatStore.getState().addMessage('conv-2', {
        ...mockMessage,
        id: 'new-dm-message',
        channel_id: 'conv-2',
      });
      indexMessage('new-dm-message', 'new DM plaintext', 'conv-2');
      release.resolve();
      await fetchPromise;

      expect(useDMStore.getState().conversations.map((conversation) => conversation.id)).toEqual([
        'conv-2',
        'conv-1',
      ]);
      expect(useDMStore.getState().activeConversationId).toBe('conv-2');
      expect(mockInvalidateChannelKey).not.toHaveBeenCalledWith('conv-2');
      expect(useChatStore.getState().messagesByChannel.has('conv-2')).toBe(true);
      expect(isIndexed('new-dm-message')).toBe(true);
    });

    it.each([
      ['removeConversation', () => useDMStore.getState().removeConversation('conv-1'), ['conv-2']],
      ['clearDMs', () => useDMStore.getState().clearDMs(), []],
    ])('reconciles a stale response after %s', async (_name, revokeAccess, expectedIds) => {
      const started = deferred();
      const release = deferred();
      server.use(
        http.get(`${API_BASE}/api/v1/dm/conversations`, async () => {
          started.resolve();
          await release.promise;
          return HttpResponse.json({
            conversations: [
              {
                id: 'conv-1',
                is_group: false,
                is_personal: false,
                name: null,
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-01T00:00:00Z',
              },
              {
                id: 'conv-2',
                is_group: true,
                is_personal: false,
                name: 'Retained group',
                participants: [],
                last_message: null,
                unread_count: 0,
                created_at: '2025-01-02T00:00:00Z',
              },
            ],
          });
        })
      );

      const fetchPromise = useDMStore.getState().fetchConversations();
      await started.promise;
      revokeAccess();
      release.resolve();
      await fetchPromise;

      expect(useDMStore.getState().conversations.map((conversation) => conversation.id)).toEqual(
        expectedIds
      );
      expect(
        useDMStore.getState().conversations.some((conversation) => conversation.id === 'conv-1')
      ).toBe(false);
      expect(useDMStore.getState().isLoading).toBe(false);
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

    it('purges access state for every conversation', () => {
      useDMStore.getState().addConversation(mockConversation);
      useDMStore.getState().addConversation(mockConversation2);
      useChatStore.getState().addMessage('conv-1', mockMessage);
      indexMessage('conv-1-message', 'first plaintext', 'conv-1');
      indexMessage('conv-2-message', 'second plaintext', 'conv-2');

      useDMStore.getState().clearDMs();

      expect(mockInvalidateChannelKey).toHaveBeenCalledTimes(2);
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('conv-1');
      expect(mockInvalidateChannelKey).toHaveBeenCalledWith('conv-2');
      expect(useChatStore.getState().messagesByChannel.has('conv-1')).toBe(false);
      expect(isIndexed('conv-1-message')).toBe(false);
      expect(isIndexed('conv-2-message')).toBe(false);
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
        indexMessage('left-group-message', 'private group plaintext', 'group-1');
        useChatStore.getState().addMessage('group-1', {
          ...mockMessage,
          id: 'left-group-message',
          channel_id: 'group-1',
        });

        // leaveGroup dynamically imports userStore — set user state
        const { useUserStore } = await import('@/renderer/stores/auth/userStore');
        useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as any });

        server.use(
          http.delete(`${API_BASE}/api/v1/dm/conversations/group-1/members/user-1`, () => {
            return HttpResponse.json({ success: true });
          })
        );

        await useDMStore.getState().leaveGroup('group-1');
        expect(useDMStore.getState().conversations.find((c) => c.id === 'group-1')).toBeUndefined();
        expect(useDMStore.getState().activeConversationId).toBeNull();
        expect(isIndexed('left-group-message')).toBe(false);
        expect(useChatStore.getState().messagesByChannel.get('group-1')).toBeUndefined();
        expect(mockInvalidateChannelKey).toHaveBeenCalledWith('group-1');
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
        expect(mockInvalidateChannelKey).toHaveBeenCalledWith('group-1');
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
        const { useUserStore } = await import('@/renderer/stores/auth/userStore');
        useUserStore.setState({ user: null });

        await expect(useDMStore.getState().leaveGroup('group-1')).rejects.toThrow(
          'Not authenticated'
        );
      });

      it('throws on API error with message', async () => {
        const { useUserStore } = await import('@/renderer/stores/auth/userStore');
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
        const { useUserStore } = await import('@/renderer/stores/auth/userStore');
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

        const { useUserStore } = await import('@/renderer/stores/auth/userStore');
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
      // A personal thread has one participant, so no peer can ever service a
      // pending row and the server answers NO_KEY_YET with pending:false. That
      // is the real absence shape — a bare Error('NO_KEY') is not something
      // e2eeService emits, and mocking it hid that ensurePersonalThreadKey
      // admitted distribution on ANY throw, including a WebCrypto unwrap
      // failure against a key that does exist.
      e2eeMock.getChannelKey.mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', false));

      // userStore needs to have user set for personal thread E2EE path
      const { useUserStore } = await import('@/renderer/stores/auth/userStore');
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

      const { useUserStore } = await import('@/renderer/stores/auth/userStore');
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

  // ── DM E2EE key distribution rate limiting (#1218) ────────────────────

  describe('DM E2EE key distribution rate limiting (#1218)', () => {
    let e2eeMock: {
      isInitialized: boolean;
      getChannelKey: ReturnType<typeof vi.fn>;
      createChannelKeys: ReturnType<typeof vi.fn>;
    };

    beforeEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      e2eeMock = mod.e2eeService as typeof e2eeMock;
      e2eeMock.isInitialized = true;
      e2eeMock.getChannelKey.mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', true));
      e2eeMock.createChannelKeys.mockResolvedValue(new Map([['user-1', 'wrapped-key-1']]));
    });

    afterEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      const mock = mod.e2eeService as typeof e2eeMock;
      mock.isInitialized = false;
      mock.getChannelKey.mockReset();
      mock.createChannelKeys.mockReset();
    });

    function conversationResponse(id: string, peerUserId: string) {
      return HttpResponse.json({
        conversation: {
          id,
          is_group: false,
          is_personal: false,
          name: null,
          participants: [
            { user_id: 'user-1', username: 'alice' },
            { user_id: peerUserId, username: 'peer' },
          ],
          last_message: null,
          unread_count: 0,
          created_at: '2025-01-01T00:00:00Z',
        },
      });
    }

    it('does not re-POST distribution while the conversation is rate limited', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-rl-1', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '45', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      await useDMStore.getState().openDM('user-2');

      // Second attempt for the SAME conversation must be suppressed by the
      // recorded per-conversation rate-limit deadline — the POST fires once.
      expect(postedConvIds).toHaveLength(1);
    });

    it('does not suppress distribution for a different conversation', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, async ({ request }) => {
          const body = (await request.json()) as { user_id: string };
          const convId = body.user_id === 'user-2' ? 'conv-rl-a' : 'conv-rl-b';
          return conversationResponse(convId, body.user_id);
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          const convId = params.convId as string;
          postedConvIds.push(convId);
          if (convId === 'conv-rl-a') {
            return new HttpResponse(null, {
              status: 429,
              headers: { 'Retry-After': '45', 'X-RateLimit-Limit': '40' },
            });
          }
          return HttpResponse.json({ distributed: 1 });
        })
      );

      await useDMStore.getState().openDM('user-2');
      await useDMStore.getState().openDM('user-3');

      // Rate limiting a DM conversation must not affect an unrelated one —
      // both conversations issue their own distribution POST.
      expect(postedConvIds).toEqual(['conv-rl-a', 'conv-rl-b']);
    });

    // `Retry-After: 0` means "the window has already rolled, retry now". Both
    // server responders build the header with int(ttl.Seconds()), which
    // truncates a sub-second remaining TTL to 0, so this is emittable rather
    // than hypothetical. Treating 0 as invalid installs a 60s local deadline
    // and leaves the conversation without its key for a minute the server
    // never asked for.
    it('honors a zero-second Retry-After instead of falling back to 60s', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, async ({ request }) => {
          const body = (await request.json()) as { user_id: string };
          return conversationResponse('conv-rl-zero', body.user_id);
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '0', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);

      // A zero deadline is already in the past, so the next attempt must go out.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(2);
    });
    it('sends wrapped_key_versions so the server can run its freshness guard', async () => {
      let sentBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-versions', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, ({ params }) =>
          HttpResponse.json({
            public_key: 'mock-public-key',
            key_version: params.userId === 'user-1' ? 3 : 7,
          })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, async ({ request }) => {
          sentBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ success: true });
        })
      );

      e2eeMock.createChannelKeys.mockResolvedValue(
        new Map([
          ['user-1', 'wrapped-1'],
          ['user-2', 'wrapped-2'],
        ])
      );

      await useDMStore.getState().openDM('user-2');

      // Without this field the server's recipientKeyFresh guard (#2420) takes
      // its fail-open branch on every insert, so a wrap against a rotated
      // identity key is stored with no self-heal row enqueued. GET
      // /public-key already returns key_version; this store used to drop it.
      expect(sentBody).not.toBeNull();
      expect(sentBody!.wrapped_key_versions).toEqual({ 'user-1': 3, 'user-2': 7 });
    });

    it('omits wrapped_key_versions when the server supplies no key_version', async () => {
      let sentBody: Record<string, unknown> | null = null;
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-noversions', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, async ({ request }) => {
          sentBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({ success: true });
        })
      );

      await useDMStore.getState().openDM('user-2');

      // An older server that does not return key_version must keep working —
      // the guard's documented legacy fail-open, not a client-side failure.
      expect(sentBody).not.toBeNull();
      expect(sentBody).not.toHaveProperty('wrapped_key_versions');
    });

    it('throws on a non-429 failure instead of reporting success', async () => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-500', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(
          `${API_BASE}/api/v1/e2ee/keys/:convId`,
          () => new HttpResponse(null, { status: 500 })
        )
      );

      await useDMStore.getState().openDM('user-2');

      await vi.waitFor(() => {
        expect(consoleSpy).toHaveBeenCalledWith(
          'Failed to distribute E2EE key for DM:',
          'Failed to distribute E2EE key for DM: 500'
        );
      });
      consoleSpy.mockRestore();
    });

    it('does not record a deadline for an account torn down mid-request', async () => {
      const postedConvIds: string[] = [];
      let releaseResponse: (() => void) | null = null;
      const held = new Promise<void>((resolve) => {
        releaseResponse = resolve;
      });

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-teardown-race', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, async ({ params }) => {
          postedConvIds.push(params.convId as string);
          // Hold the response open so the teardown below lands strictly
          // between the request going out and its 429 coming back. That
          // ordering is the whole finding — clearDMs() cannot cancel a
          // continuation that is already awaiting.
          if (postedConvIds.length === 1) await held;
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      const inFlight = useDMStore.getState().openDM('user-2');
      await vi.waitFor(() => expect(postedConvIds).toHaveLength(1));

      // Account switch / logout while the 429 is still in flight.
      useDMStore.getState().clearDMs();
      releaseResponse!();
      await inFlight;

      // The successor account opens the SAME conversation — reachable for real,
      // because a group DM's id is shared by its participants. It must not
      // inherit a window the previous account spent.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(2);
    });

    it('abandons a distribution when the account changes during the key fetch', async () => {
      const postedConvIds: string[] = [];
      let releaseKeys: (() => void) | null = null;
      const heldKeys = new Promise<void>((resolve) => {
        releaseKeys = resolve;
      });
      let pkCalls = 0;
      // Both participants are fetched concurrently via Promise.allSettled, so
      // gate on a flag rather than a call count.
      let holdingKeys = true;

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-early-teardown', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, async () => {
          // Hold the PUBLIC-KEY fetch open, not the POST. This is the window the
          // first teardown test missed: clearDMs() lands before
          // distributeChannelKeys is even entered, so a generation captured
          // inside that function would already read the SUCCESSOR's value,
          // match at the write, and let the dead account install a deadline.
          pkCalls += 1;
          if (holdingKeys) await heldKeys;
          return HttpResponse.json({ public_key: 'mock-public-key' });
        }),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      const inFlight = useDMStore.getState().openDM('user-2');
      await vi.waitFor(() => expect(pkCalls).toBeGreaterThan(0));

      useDMStore.getState().clearDMs();
      holdingKeys = false;
      releaseKeys!();
      await inFlight;

      // The superseded operation must not have POSTed at all — it is a dead
      // session's request, and refusing it also skips a CSK generation and an
      // RSA wrap per participant.
      expect(postedConvIds).toHaveLength(0);

      // And the successor account is not suppressed by anything it left behind.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);
    });

    it('skips the public-key fetch entirely while a deadline is active', async () => {
      const postedConvIds: string[] = [];
      let pkCalls = 0;
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-pk-budget', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () => {
          pkCalls += 1;
          return HttpResponse.json({ public_key: 'mock-public-key' });
        }),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);
      const pkAfterFirst = pkCalls;
      expect(pkAfterFirst).toBeGreaterThan(0);

      // GET /users/:id/public-key carries its own RateLimitByUser(30, 1m).
      // Asserting only on the POST count would pass even while each suppressed
      // reopen burned N of those 30 — which is what made three retries able to
      // exhaust the budget that key wrapping needs once distribution reopens.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);
      expect(pkCalls).toBe(pkAfterFirst);
    });

    it('does not POST when the account changes during the RSA wrap', async () => {
      const postedConvIds: string[] = [];
      let releaseWrap: (() => void) | null = null;
      const heldWrap = new Promise<void>((resolve) => {
        releaseWrap = resolve;
      });
      let wrapCalls = 0;

      // createChannelKeys awaits an RSA-OAEP wrap per recipient, so this is the
      // widest await in the operation — and the one window neither earlier
      // teardown test covers. apiFetch attaches whatever token is current when
      // it is CALLED, so a POST that survives this window carries the previous
      // account's wraps under the successor's credentials.
      e2eeMock.createChannelKeys.mockImplementation(async () => {
        wrapCalls += 1;
        if (wrapCalls === 1) await heldWrap;
        return new Map([['user-1', 'wrapped-key-1']]);
      });

      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-wrap-teardown', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return HttpResponse.json({ success: true });
        })
      );

      const inFlight = useDMStore.getState().openDM('user-2');
      await vi.waitFor(() => expect(wrapCalls).toBe(1));

      useDMStore.getState().clearDMs();
      releaseWrap!();
      await inFlight;

      expect(postedConvIds).toHaveLength(0);
    });

    it('scopes a route-wide 429 across conversations, not to one', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, async ({ request }) => {
          const body = (await request.json()) as { user_id: string };
          return conversationResponse(`conv-user-rl-${body.user_id}`, body.user_id);
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          // Limit 10 is the route's per-USER middleware, whose Redis key is
          // built from the route PATTERN — so the budget is already spent for
          // every context, not just this one.
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '10' },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);

      // A DIFFERENT conversation. Scoping the route-wide deadline to conv-1
      // would let this one redo the public-key fetches, a fresh CSK, an RSA
      // wrap per participant and the POST, purely to collect another 429.
      await useDMStore.getState().openDM('user-3');
      expect(postedConvIds).toHaveLength(1);
    });

    it('keeps a per-conversation 429 from suppressing other conversations', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, async ({ request }) => {
          const body = (await request.json()) as { user_id: string };
          return conversationResponse(`conv-scope-${body.user_id}`, body.user_id);
        }),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      // The negative control for the test above: widening the per-conversation
      // deadline into the route-wide one would pass that test and fail this.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);

      await useDMStore.getState().openDM('user-3');
      expect(postedConvIds).toHaveLength(2);
    });

    it('does not self-block on a 429 that carries no rate-limit headers', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-edge-429', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          // The API is always Cloudflare-proxied, so an edge-issued 429 with
          // neither X-RateLimit-Limit nor Retry-After is realistic. Installing
          // a per-CONVERSATION deadline for it would be a scope guess about an
          // error we never diagnosed.
          return new HttpResponse(null, { status: 429 });
        })
      );

      await useDMStore.getState().openDM('user-2');
      await useDMStore.getState().openDM('user-2');

      expect(postedConvIds).toHaveLength(2);
    });

    it('clears recorded deadlines on clearDMs so they cannot outlive an account', async () => {
      const postedConvIds: string[] = [];
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations`, () =>
          conversationResponse('conv-rl-reset', 'user-2')
        ),
        http.get(`${API_BASE}/api/v1/users/:userId/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key' })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, ({ params }) => {
          postedConvIds.push(params.convId as string);
          return new HttpResponse(null, {
            status: 429,
            headers: { 'Retry-After': '600', 'X-RateLimit-Limit': '40' },
          });
        })
      );

      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);

      // Suppressed while the deadline stands.
      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(1);

      // The map is module-scope, so no set() reaches it and no store reset
      // would have. gracefulReset() calls clearDMs() on every login-screen and
      // reload transition — that is the account boundary it has to respect.
      useDMStore.getState().clearDMs();

      await useDMStore.getState().openDM('user-2');
      expect(postedConvIds).toHaveLength(2);
    });
  });

  // ── Personal-thread key-absence narrowing (#1218 review) ───────────────

  describe('ensurePersonalThreadKey error narrowing', () => {
    let e2eeMock: {
      isInitialized: boolean;
      getChannelKey: ReturnType<typeof vi.fn>;
      createChannelKeys: ReturnType<typeof vi.fn>;
    };

    beforeEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      e2eeMock = mod.e2eeService as typeof e2eeMock;
      e2eeMock.isInitialized = true;
      e2eeMock.createChannelKeys.mockResolvedValue(new Map([['user-1', 'wrapped-key-1']]));
      // Load-bearing: ensurePersonalThreadKey returns early when no user is
      // set, so without this every case below passes whether or not the
      // narrowing works. The positive control is what surfaced that.
      const { useUserStore } = await import('@/renderer/stores/auth/userStore');
      useUserStore.setState({ user: { id: 'user-1', username: 'alice' } as never });
      server.use(
        http.post(`${API_BASE}/api/v1/dm/conversations/personal`, () =>
          HttpResponse.json({
            conversation: {
              id: 'conv-personal-narrow',
              is_group: false,
              is_personal: true,
              name: null,
              participants: [{ user_id: 'user-1', username: 'alice' }],
              last_message: null,
              unread_count: 0,
              created_at: '2025-01-01T00:00:00Z',
            },
          })
        ),
        http.get(`${API_BASE}/api/v1/users/user-1/public-key`, () =>
          HttpResponse.json({ public_key: 'mock-public-key', key_version: 2 })
        ),
        http.post(`${API_BASE}/api/v1/e2ee/keys/:convId`, () =>
          HttpResponse.json({ success: true })
        )
      );
    });

    afterEach(async () => {
      const mod = await import('@/renderer/services/e2eeService');
      const mock = mod.e2eeService as typeof e2eeMock;
      mock.isInitialized = false;
      mock.getChannelKey.mockReset();
      mock.createChannelKeys.mockReset();
    });

    // NO_KEY_YET with pending:false is the REAL absence shape for a personal
    // thread — one participant means no peer can service a pending row, and
    // throwKeyFetchError defaults pending to false. Narrowing this path with
    // isPendingKeyError (as ensureE2EEKey does) would therefore stop personal
    // threads getting a first key at all. This is the positive control that
    // keeps the negative cases below from passing for the wrong reason.
    it('distributes when the key is genuinely absent', async () => {
      e2eeMock.getChannelKey.mockRejectedValue(new E2EEKeyUnavailableError('NO_KEY_YET', false));
      await useDMStore.getState().openPersonalThread();
      expect(e2eeMock.createChannelKeys).toHaveBeenCalled();
    });

    it.each([
      ['NOT_MEMBER', new E2EEKeyUnavailableError('NOT_MEMBER', false)],
      ['REVOKED_EPOCH', new E2EEKeyUnavailableError('REVOKED_EPOCH', false)],
      ['MALFORMED_PAYLOAD', new E2EEKeyUnavailableError('MALFORMED_PAYLOAD', false)],
      [
        'a raw WebCrypto failure',
        new Error('The operation failed for an operation-specific reason'),
      ],
    ])('does not distribute on %s', async (_label, thrown) => {
      const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      e2eeMock.getChannelKey.mockRejectedValue(thrown);

      await useDMStore.getState().openPersonalThread();

      // A bare `catch {}` admitted all of these. At an unchanged version the
      // server then drops the insert via ON CONFLICT DO NOTHING and answers
      // 200, so the loop never converged — it just respent the conversation's
      // budget on every open, with nothing logged to say why.
      expect(e2eeMock.createChannelKeys).not.toHaveBeenCalled();
      expect(consoleSpy).toHaveBeenCalledWith(
        'Personal thread key check failed:',
        expect.anything()
      );
      consoleSpy.mockRestore();
    });
  });
});
