import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { server } from '../../mocks/server';
import { http, HttpResponse } from 'msw';

const API_BASE = 'http://localhost:8080';
const PRIVACY_ENDPOINT = `${API_BASE}/api/v1/users/me/privacy`;

/**
 * A privacy payload from a control-plane that predates #1354 — every field the
 * old server knows about, and no `require_auth_before_purge`.
 */
const legacyPrivacy = {
  messages_friends_only: true,
  messages_server_members: true,
  dm_privacy_level: 2,
  dm_friends_of_friends: false,
  auto_accept_friend_codes: false,
  searchable_by_username: false,
  searchable_by_email: false,
  searchable_by_phone: false,
  allow_embedded_content: false,
  load_gifs_automatically: true,
  share_personalization_with_gif_provider: true,
};

beforeAll(() => server.listen({ onUnhandledRequest: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

beforeEach(() => {
  resetAllStores();
  useAuthStore.getState().setAccessToken('mock-token');
});

describe('privacyStore — require authentication before purging (#1354)', () => {
  it('defaults requireAuthBeforePurge to true before any fetch', () => {
    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(true);
  });

  it('treats a GET response missing the field as an old server and keeps it on', async () => {
    server.use(http.get(PRIVACY_ENDPOINT, () => HttpResponse.json({ privacy: legacyPrivacy })));

    await usePrivacyStore.getState().fetchPrivacy();

    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(true);
    expect(usePrivacyStore.getState().error).toBeNull();
  });

  it('reads the stored value when the server sends it off', async () => {
    server.use(
      http.get(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ privacy: { ...legacyPrivacy, require_auth_before_purge: false } })
      )
    );

    await usePrivacyStore.getState().fetchPrivacy();

    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(false);
  });

  it('reads the stored value when the server sends it on', async () => {
    server.use(
      http.get(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ privacy: { ...legacyPrivacy, require_auth_before_purge: true } })
      )
    );

    await usePrivacyStore.getState().fetchPrivacy();

    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(true);
  });

  it('PATCHes the wire field and stores the value the server echoes back', async () => {
    let capturedBody: Record<string, unknown> = {};
    server.use(
      http.patch(PRIVACY_ENDPOINT, async ({ request }) => {
        capturedBody = (await request.json()) as Record<string, unknown>;
        return HttpResponse.json({
          privacy: { ...legacyPrivacy, require_auth_before_purge: false },
        });
      })
    );

    await usePrivacyStore.getState().updatePrivacy({ requireAuthBeforePurge: false });

    expect(capturedBody).toEqual({ require_auth_before_purge: false });
    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(false);
    expect(usePrivacyStore.getState().error).toBeNull();
  });

  it('falls back to on when a PATCH response omits the field', async () => {
    server.use(http.patch(PRIVACY_ENDPOINT, () => HttpResponse.json({ privacy: legacyPrivacy })));

    await usePrivacyStore.getState().updatePrivacy({ requireAuthBeforePurge: false });

    expect(usePrivacyStore.getState().settings.requireAuthBeforePurge).toBe(true);
  });

  it('surfaces an old-server PATCH rejection instead of failing silently', async () => {
    server.use(
      http.patch(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ error: 'No fields to update' }, { status: 400 })
      )
    );

    await expect(
      usePrivacyStore.getState().updatePrivacy({ requireAuthBeforePurge: false })
    ).rejects.toThrow(/doesn't support this setting yet/i);
    expect(usePrivacyStore.getState().error).toMatch(/doesn't support this setting yet/i);
  });

  it('leaves an unrelated PATCH failure message untranslated', async () => {
    server.use(
      http.patch(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ error: 'Failed to update privacy settings' }, { status: 500 })
      )
    );

    await expect(
      usePrivacyStore.getState().updatePrivacy({ requireAuthBeforePurge: false })
    ).rejects.toThrow('Failed to update privacy settings');
  });

  it('does not translate an empty-body rejection for an unrelated update', async () => {
    server.use(
      http.patch(PRIVACY_ENDPOINT, () =>
        HttpResponse.json({ error: 'No fields to update' }, { status: 400 })
      )
    );

    await expect(
      usePrivacyStore.getState().updatePrivacy({ searchableByEmail: true })
    ).rejects.toThrow('No fields to update');
  });

  it('falls back to the generic message when a PATCH failure carries no JSON', async () => {
    // A proxy HTML 502: `response.json()` rejects with a SyntaxError, which
    // `PrivacySecuritySection` would otherwise render verbatim into its alert.
    server.use(
      http.patch(
        PRIVACY_ENDPOINT,
        () =>
          new HttpResponse('<html><body>502 Bad Gateway</body></html>', {
            status: 502,
            headers: { 'Content-Type': 'text/html' },
          })
      )
    );

    await expect(
      usePrivacyStore.getState().updatePrivacy({ requireAuthBeforePurge: false })
    ).rejects.toThrow('Failed to update privacy settings');
    expect(usePrivacyStore.getState().error).toBe('Failed to update privacy settings');
  });
});
