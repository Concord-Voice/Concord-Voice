/**
 * Cross-account leak: a privacy response that lands AFTER logout.
 *
 * `clearPrivacy()` is synchronous; an in-flight GET/PATCH on
 * `/users/me/privacy` is not. Before the identity fence, the continuation wrote
 * the previous account's real privacy posture back into the store — stamped
 * `loaded: true`, so the next account on the device was shown account A's
 * searchability, DM posture and `allow_friend_requests_from` as SERVER-CONFIRMED.
 *
 * Found by an adversarial pass on PR #2888, which supplied these cases. The
 * window is not user-triggered: App.tsx refires `fetchPrivacy` on every
 * `accessToken` change, so every token refresh opens one.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';

vi.mock('@/renderer/services/apiClient', () => ({ apiFetch: vi.fn() }));

import { usePrivacyStore } from '@/renderer/stores/privacyStore';
import { apiFetch } from '@/renderer/services/apiClient';

const mockApiFetch = apiFetch as unknown as ReturnType<typeof vi.fn>;

/** Account A's real privacy posture: locked down. */
const ACCOUNT_A_PRIVACY = {
  messages_friends_only: true,
  messages_server_members: false,
  dm_privacy_level: 0,
  dm_friends_of_friends: false,
  auto_accept_friend_codes: false,
  searchable_by_username: false,
  searchable_by_email: true, // A's real, private choice
  searchable_by_phone: true, // A's real, private choice
  allow_embedded_content: false,
  load_gifs_automatically: false,
  share_personalization_with_gif_provider: false,
  require_auth_before_purge: false,
  allow_friend_requests_from: 'nobody',
};

beforeEach(() => {
  mockApiFetch.mockReset();
  usePrivacyStore.getState().clearPrivacy();
});

describe('privacyStore — identity fence across logout', () => {
  it('A logs out mid-fetch: A settings resurrect AND are stamped loaded:true', async () => {
    let release!: (r: unknown) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise((r) => {
        release = r;
      })
    );

    // 1. Account A is signed in; App.tsx's accessToken effect fires fetchPrivacy().
    const inFlight = usePrivacyStore.getState().fetchPrivacy();

    // 2. A logs out. resetService.gracefulReset() calls clearPrivacy().
    usePrivacyStore.getState().clearPrivacy();
    expect(usePrivacyStore.getState().loaded).toBe(false);
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');

    // 3. Account A's response finally lands, after the logout.
    release({ ok: true, status: 200, json: async () => ({ privacy: ACCOUNT_A_PRIVACY }) });
    await inFlight;

    const after = usePrivacyStore.getState();

    // EXPECTED (fenced): the clear stands. Account B, who logs in next on this
    // shared device, must never see A's posture presented as confirmed.
    expect(after.loaded).toBe(false);
    expect(after.settings.allowFriendRequestsFrom).toBe('everyone');
    expect(after.settings.searchableByEmail).toBe(false);
    expect(after.settings.searchableByPhone).toBe(false);
    expect(after.settings.dmPrivacyLevel).toBe(2);
  });

  it('an in-flight PATCH from A also survives the logout', async () => {
    let release!: (r: unknown) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise((r) => {
        release = r;
      })
    );

    const inFlight = usePrivacyStore
      .getState()
      .updatePrivacy({ allowFriendRequestsFrom: 'nobody' });

    usePrivacyStore.getState().clearPrivacy();

    release({ ok: true, status: 200, json: async () => ({ privacy: ACCOUNT_A_PRIVACY }) });
    await inFlight;

    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
    expect(usePrivacyStore.getState().settings.searchableByEmail).toBe(false);
  });
});

// The settings-write cases above are actually satisfied by the ORDERING fence,
// because clearPrivacy retires every issued ticket (privacySettled =
// privacyIssued). The identity counter earns its place on the path ordering
// deliberately does not cover: error reporting is fenced on identity ONLY, so a
// failure is still reported when a newer request has superseded it — but must
// not be reported to a DIFFERENT account.
describe('privacyStore — identity fences error reporting across logout', () => {
  beforeEach(() => {
    mockApiFetch.mockReset();
    usePrivacyStore.getState().clearPrivacy();
  });

  it("a fetch failure from the previous account is not recorded on the next one's store", async () => {
    let fail!: (e: Error) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise((_res, rej) => {
        fail = rej as (e: Error) => void;
      })
    );

    const inFlight = usePrivacyStore.getState().fetchPrivacy(); // account A
    usePrivacyStore.getState().clearPrivacy(); // A logs out
    fail(new Error("account A's network died"));
    await inFlight;

    // Account B must not inherit A's error banner.
    expect(usePrivacyStore.getState().error).toBeNull();
  });

  it('a failure for the CURRENT account is still recorded', async () => {
    mockApiFetch.mockRejectedValueOnce(new Error('my own network died'));
    await usePrivacyStore.getState().fetchPrivacy();
    expect(usePrivacyStore.getState().error).toBe('my own network died');
  });
});
