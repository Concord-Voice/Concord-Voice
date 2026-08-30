import {
  usePrivacyStore,
  classifyPrivacyRefusal,
  FRIEND_REQUEST_SKEW_MESSAGE,
  PURGE_AUTH_SKEW_MESSAGE,
  type FriendRequestPrivacyMode,
} from '@/renderer/stores/ui/privacyStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
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
    // #1766: GIF auto-load defaults ON for new users.
    expect(settings.loadGifsAutomatically).toBe(true);
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

// ── #1241: allow_friend_requests_from ────────────────────────────────────────

describe('allowFriendRequestsFrom (#1241)', () => {
  it('defaults to everyone before any fetch', () => {
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
  });

  it('hydrates the value from GET /users/me/privacy', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/me/privacy`, () =>
        HttpResponse.json({ privacy: { allow_friend_requests_from: 'nobody' } })
      )
    );
    await usePrivacyStore.getState().fetchPrivacy();
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('nobody');
  });

  // A pre-#1240 server omits the key. Absent must read as the permissive
  // default (matching the column default), not as undefined.
  it('treats an absent key as everyone', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/me/privacy`, () => HttpResponse.json({ privacy: {} }))
    );
    await usePrivacyStore.getState().fetchPrivacy();
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
  });

  it('sends the snake_case wire field on PATCH', async () => {
    let received: Record<string, unknown> | null = null;
    server.use(
      http.patch(`${API_BASE}/api/v1/users/me/privacy`, async ({ request }) => {
        received = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({ privacy: { allow_friend_requests_from: 'mutual_servers' } });
      })
    );
    await usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'mutual_servers' });
    expect(received).toEqual({ allow_friend_requests_from: 'mutual_servers' });
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('mutual_servers');
  });

  it('surfaces the server enum-validation message', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/users/me/privacy`, () =>
        HttpResponse.json(
          { error: 'allow_friend_requests_from must be everyone, mutual_servers, or nobody' },
          { status: 400 }
        )
      )
    );
    await expect(
      usePrivacyStore
        .getState()
        .updatePrivacy({ allowFriendRequestsFrom: 'bogus' as FriendRequestPrivacyMode })
    ).rejects.toThrow('allow_friend_requests_from must be everyone, mutual_servers, or nobody');
  });

  // Version skew: a PATCH carrying only this field against a pre-#1240 server
  // maps to zero SET clauses, and the server answers this exact 400.
  it('maps the skew 400 to the version-skew copy end to end', async () => {
    server.use(
      http.patch(`${API_BASE}/api/v1/users/me/privacy`, () =>
        HttpResponse.json({ error: 'No fields to update' }, { status: 400 })
      )
    );
    await expect(
      usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'nobody' })
    ).rejects.toThrow(FRIEND_REQUEST_SKEW_MESSAGE);
  });
});

describe('classifyPrivacyRefusal — updatedFields (#1241)', () => {
  it('maps the skew 400 to the friend-request message when that field was sent', () => {
    expect(
      classifyPrivacyRefusal(400, { error: 'No fields to update' }, ['allowFriendRequestsFrom'])
    ).toEqual({ kind: 'refused', message: FRIEND_REQUEST_SKEW_MESSAGE });
  });

  // #1354 must not regress: the purge arm keeps its own copy, byte-identical.
  it('still maps the skew 400 to the purge message when that field was sent', () => {
    expect(
      classifyPrivacyRefusal(400, { error: 'No fields to update' }, ['requireAuthBeforePurge'])
    ).toEqual({ kind: 'refused', message: PURGE_AUTH_SKEW_MESSAGE });
  });

  it('falls through to the generic refusal when no skew-bearing field was sent', () => {
    const r = classifyPrivacyRefusal(400, { error: 'No fields to update' }, ['searchableByEmail']);
    expect(r.kind).toBe('refused');
    expect(r.message).toBe('No fields to update');
  });

  // #2765: a plain 400 on the gated transition still means "no credential can
  // satisfy this", which is a different arm from version skew.
  it('keeps stepUpImpossible for a non-skew 400 on the purge fence', () => {
    const r = classifyPrivacyRefusal(400, { error: 'Nope' }, ['requireAuthBeforePurge']);
    expect(r.kind).toBe('stepUpImpossible');
  });
});

// A successful PATCH is a server confirmation. Without this, a failed initial
// GET followed by any successful write left privacy controls disabled forever.
describe('privacyStore — loaded (#1241)', () => {
  it('is false before any server response', () => {
    expect(usePrivacyStore.getState().loaded).toBe(false);
  });

  it('becomes true after a successful PATCH even if the initial fetch failed', async () => {
    server.use(
      http.get(`${API_BASE}/api/v1/users/me/privacy`, () => HttpResponse.error()),
      http.patch(`${API_BASE}/api/v1/users/me/privacy`, () =>
        HttpResponse.json({ privacy: { allow_friend_requests_from: 'nobody' } })
      )
    );
    await usePrivacyStore.getState().fetchPrivacy();
    expect(usePrivacyStore.getState().loaded).toBe(false);

    await usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'nobody' });
    expect(usePrivacyStore.getState().loaded).toBe(true);
  });

  it('resets on clearPrivacy so the next account does not inherit confirmation', () => {
    usePrivacyStore.setState({ loaded: true });
    usePrivacyStore.getState().clearPrivacy();
    expect(usePrivacyStore.getState().loaded).toBe(false);
  });
});
