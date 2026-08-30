/**
 * Privacy writes must be last-REQUEST-wins, not last-RESPONSE-wins.
 *
 * Two reachable orderings, both proven by an adversarial pass on PR #2888:
 *   1. a superseded PATCH echo overwriting a newer one;
 *   2. a GET issued before a PATCH committed landing after it and rolling the
 *      display back — which needs no racy clicking, just a token refresh
 *      mid-edit.
 *
 * The component-level guard in `commitPrivacyTier` cannot cover either: it sits
 * in the rejection path, and it cannot fence `fetchPrivacy` at all. The fence
 * has to live in the store.
 */
import { vi, describe, it, expect, beforeEach } from 'vitest';

vi.mock('@/renderer/services/system/apiClient', () => ({ apiFetch: vi.fn() }));

import { usePrivacyStore } from '@/renderer/stores/ui/privacyStore';
import { apiFetch } from '@/renderer/services/system/apiClient';

const mockApiFetch = apiFetch as unknown as ReturnType<typeof vi.fn>;

const privacyBody = (mode: string) => ({
  ok: true,
  status: 200,
  json: async () => ({
    privacy: {
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
      require_auth_before_purge: true,
      allow_friend_requests_from: mode,
    },
  }),
});

const refusedPatch = (message: string) =>
  ({ ok: false, status: 400, json: async () => ({ error: message }) }) as unknown as Response;

/** A promise plus its resolver, so response arrival order is under test control. */
function deferred() {
  let release!: (v: unknown) => void;
  const promise = new Promise((r) => {
    release = r;
  });
  return { promise, release };
}

beforeEach(() => {
  mockApiFetch.mockReset();
  usePrivacyStore.getState().clearPrivacy();
});

describe('privacyStore — write ordering', () => {
  it('a superseded PATCH echo overwrites the newer one (success path is unguarded)', async () => {
    const first = deferred();
    const second = deferred();
    mockApiFetch.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise);

    // The user's first selection: lock the account down.
    const p1 = usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'nobody' });
    // >300 ms later they change their mind and open it up. This is the newer,
    // authoritative intent; the server will end up holding 'everyone'.
    const p2 = usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'everyone' });

    // The newer PATCH's response arrives first; the superseded one straggles.
    second.release(privacyBody('everyone'));
    await p2;
    first.release(privacyBody('nobody'));
    await p1;

    // EXPECTED: the store reflects the newest write the user made.
    // ACTUAL: the superseded echo wins, and PrivacySecuritySection's sync effect
    // drives the slider to "No One" — telling the user nobody can friend-request
    // them while the server accepts requests from everyone.
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
  });

  it('a stale GET issued before a PATCH committed overwrites the PATCH echo', async () => {
    const get = deferred();
    const patch = deferred();
    mockApiFetch.mockReturnValueOnce(get.promise).mockReturnValueOnce(patch.promise);

    // App.tsx re-runs fetchPrivacy() on EVERY accessToken change — i.e. on every
    // proactive token refresh and on every 401 recovery. Server still holds the
    // old value at the moment this GET is served.
    const g = usePrivacyStore.getState().fetchPrivacy();

    // The user meanwhile locks the account down; the PATCH commits server-side.
    const p = usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'nobody' });

    patch.release(privacyBody('nobody'));
    await p;
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('nobody');

    // The stale GET response lands last.
    get.release(privacyBody('everyone'));
    await g;

    // EXPECTED: a response that predates the committed PATCH must not be able to
    // roll the displayed setting back.
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('nobody');
  });
});

// #2903 AC-2. A refusal is an outcome like a success, so it settles on the same
// watermark. Without that, a slow 400 landing after a newer PATCH succeeded
// puts a validation error on screen about a choice the user has already
// replaced — while the store correctly holds the newer, accepted value.
describe('privacyStore — a superseded refusal is not recorded (#2903 AC-2)', () => {
  beforeEach(() => {
    mockApiFetch.mockReset();
    usePrivacyStore.getState().clearPrivacy();
  });

  it('a 400 landing after a newer PATCH succeeded records no error', async () => {
    let respondA!: (r: Response) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise<Response>((r) => {
        respondA = r;
      })
    );
    // It still throws for its own caller — only the STORE write is fenced.
    const slowA = usePrivacyStore
      .getState()
      .updatePrivacy({ allowFriendRequestsFrom: 'nobody' })
      .catch(() => undefined);

    mockApiFetch.mockResolvedValueOnce(privacyBody('everyone') as unknown as Response);
    await usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'everyone' });

    respondA(
      refusedPatch('allow_friend_requests_from must be everyone, mutual_servers, or nobody')
    );
    await slowA;

    expect(usePrivacyStore.getState().error).toBeNull();
    expect(usePrivacyStore.getState().settings.allowFriendRequestsFrom).toBe('everyone');
  });

  it('an UNsuperseded refusal is still recorded', async () => {
    mockApiFetch.mockResolvedValueOnce(refusedPatch('nope'));
    await expect(
      usePrivacyStore.getState().updatePrivacy({ allowFriendRequestsFrom: 'nobody' })
    ).rejects.toThrow('nope');
    expect(usePrivacyStore.getState().error).toBe('nope');
  });
});
