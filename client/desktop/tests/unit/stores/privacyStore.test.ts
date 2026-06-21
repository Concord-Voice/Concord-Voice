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

describe('privacyStore', () => {
  it('has correct defaults', () => {
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

  it('fetchPrivacy loads settings from API', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/me/privacy`, () => {
        return HttpResponse.json({
          privacy: {
            messages_friends_only: false,
            messages_server_members: false,
            dm_privacy_level: 1,
            dm_friends_of_friends: true,
            auto_accept_friend_codes: true,
            searchable_by_username: true,
            searchable_by_email: true,
            searchable_by_phone: false,
            allow_embedded_content: true,
          },
        });
      })
    );

    await usePrivacyStore.getState().fetchPrivacy();
    const { settings, isLoading, error } = usePrivacyStore.getState();
    expect(isLoading).toBe(false);
    expect(error).toBeNull();
    expect(settings.messagesFriendsOnly).toBe(false);
    expect(settings.dmPrivacyLevel).toBe(1);
    expect(settings.dmFriendsOfFriends).toBe(true);
    expect(settings.searchableByUsername).toBe(true);
    expect(settings.allowEmbeddedContent).toBe(true);
  });

  it('fetchPrivacy handles API errors', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/me/privacy`, () => {
        return HttpResponse.json({ error: 'Server error' }, { status: 500 });
      })
    );

    await usePrivacyStore.getState().fetchPrivacy();
    const { error, isLoading } = usePrivacyStore.getState();
    expect(isLoading).toBe(false);
    expect(error).not.toBeNull();
  });

  it('updatePrivacy sends PATCH and updates state', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/users/me/privacy`, () => {
        return HttpResponse.json({
          privacy: {
            messages_friends_only: true,
            messages_server_members: true,
            dm_privacy_level: 3,
            dm_friends_of_friends: false,
            auto_accept_friend_codes: false,
            searchable_by_username: true,
            searchable_by_email: false,
            searchable_by_phone: false,
            allow_embedded_content: false,
          },
        });
      })
    );

    await usePrivacyStore
      .getState()
      .updatePrivacy({ dmPrivacyLevel: 3, searchableByUsername: true });
    const { settings } = usePrivacyStore.getState();
    expect(settings.dmPrivacyLevel).toBe(3);
    expect(settings.searchableByUsername).toBe(true);
  });

  it('clearPrivacy resets to defaults', () => {
    usePrivacyStore.setState({
      settings: {
        messagesFriendsOnly: false,
        messagesServerMembers: false,
        dmPrivacyLevel: 0,
        dmFriendsOfFriends: true,
        autoAcceptFriendCodes: true,
        searchableByUsername: true,
        searchableByEmail: true,
        searchableByPhone: true,
        allowEmbeddedContent: true,
      },
      error: 'some error',
    });

    usePrivacyStore.getState().clearPrivacy();
    const { settings, error } = usePrivacyStore.getState();
    expect(settings.messagesFriendsOnly).toBe(true);
    expect(settings.dmPrivacyLevel).toBe(2);
    expect(error).toBeNull();
  });
});
