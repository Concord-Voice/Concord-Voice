import { usePrivacyStore } from '@/renderer/stores/privacyStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';

beforeAll(() => server.listen({ onUnhandledRequest: 'bypass' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
});

describe('privacyStore — extended coverage', () => {
  describe('fetchPrivacy with null/missing fields', () => {
    it('falls back to defaults when API returns null values', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/privacy`, () => {
          return HttpResponse.json({
            privacy: {
              // All null — should fall back to defaults
              messages_friends_only: null,
              messages_server_members: null,
              dm_privacy_level: null,
              dm_friends_of_friends: null,
              auto_accept_friend_codes: null,
              searchable_by_username: null,
              searchable_by_email: null,
              searchable_by_phone: null,
              allow_embedded_content: null,
            },
          });
        })
      );

      await usePrivacyStore.getState().fetchPrivacy();
      const { settings } = usePrivacyStore.getState();
      expect(settings.messagesFriendsOnly).toBe(true);
      expect(settings.messagesServerMembers).toBe(true);
      expect(settings.dmPrivacyLevel).toBe(2);
      expect(settings.dmFriendsOfFriends).toBe(false);
      expect(settings.autoAcceptFriendCodes).toBe(false);
      expect(settings.searchableByUsername).toBe(false);
      expect(settings.searchableByEmail).toBe(false);
      expect(settings.searchableByPhone).toBe(false);
      expect(settings.allowEmbeddedContent).toBe(false);
    });

    it('sets isLoading to true during fetch', async () => {
      let resolveHandler!: () => void;
      const pending = new Promise<void>((resolve) => {
        resolveHandler = resolve;
      });

      server.use(
        http.get(`${API_BASE}/api/v1/users/me/privacy`, async () => {
          await pending;
          return HttpResponse.json({
            privacy: {
              messages_friends_only: true,
              messages_server_members: true,
              dm_privacy_level: 2,
            },
          });
        })
      );

      const fetchPromise = usePrivacyStore.getState().fetchPrivacy();
      expect(usePrivacyStore.getState().isLoading).toBe(true);
      resolveHandler();
      await fetchPromise;
      expect(usePrivacyStore.getState().isLoading).toBe(false);
    });

    it('handles network errors gracefully', async () => {
      server.use(
        http.get(`${API_BASE}/api/v1/users/me/privacy`, () => {
          return HttpResponse.error();
        })
      );

      await usePrivacyStore.getState().fetchPrivacy();
      const { error, isLoading } = usePrivacyStore.getState();
      expect(isLoading).toBe(false);
      expect(error).toBeTruthy(); // Network error message varies by implementation
    });
  });

  describe('updatePrivacy', () => {
    it('throws on API error', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/users/me/privacy`, () => {
          return HttpResponse.json({ error: 'Unauthorized' }, { status: 401 });
        })
      );

      await expect(usePrivacyStore.getState().updatePrivacy({ dmPrivacyLevel: 0 })).rejects.toThrow(
        'Unauthorized'
      );
    });

    it('sends only changed fields in the request body', async () => {
      let capturedBody: Record<string, unknown> = {};

      server.use(
        http.patch(`${API_BASE}/api/v1/users/me/privacy`, async ({ request }) => {
          capturedBody = (await request.json()) as Record<string, unknown>;
          return HttpResponse.json({
            privacy: {
              messages_friends_only: true,
              messages_server_members: true,
              dm_privacy_level: 2,
              dm_friends_of_friends: false,
              auto_accept_friend_codes: false,
              searchable_by_username: true,
              searchable_by_email: false,
              searchable_by_phone: false,
              allow_embedded_content: true,
            },
          });
        })
      );

      await usePrivacyStore.getState().updatePrivacy({
        searchableByUsername: true,
        allowEmbeddedContent: true,
      });

      expect(capturedBody).toEqual({
        searchable_by_username: true,
        allow_embedded_content: true,
      });
    });

    it('updates all privacy fields when provided', async () => {
      server.use(
        http.patch(`${API_BASE}/api/v1/users/me/privacy`, () => {
          return HttpResponse.json({
            privacy: {
              messages_friends_only: false,
              messages_server_members: false,
              dm_privacy_level: 0,
              dm_friends_of_friends: true,
              auto_accept_friend_codes: true,
              searchable_by_username: true,
              searchable_by_email: true,
              searchable_by_phone: true,
              allow_embedded_content: true,
            },
          });
        })
      );

      await usePrivacyStore.getState().updatePrivacy({
        messagesFriendsOnly: false,
        messagesServerMembers: false,
        dmPrivacyLevel: 0,
        dmFriendsOfFriends: true,
        autoAcceptFriendCodes: true,
        searchableByUsername: true,
        searchableByEmail: true,
        searchableByPhone: true,
        allowEmbeddedContent: true,
      });

      const { settings } = usePrivacyStore.getState();
      expect(settings.messagesFriendsOnly).toBe(false);
      expect(settings.messagesServerMembers).toBe(false);
      expect(settings.dmPrivacyLevel).toBe(0);
      expect(settings.dmFriendsOfFriends).toBe(true);
      expect(settings.autoAcceptFriendCodes).toBe(true);
      expect(settings.searchableByUsername).toBe(true);
      expect(settings.searchableByEmail).toBe(true);
      expect(settings.searchableByPhone).toBe(true);
      expect(settings.allowEmbeddedContent).toBe(true);
    });
  });
});
