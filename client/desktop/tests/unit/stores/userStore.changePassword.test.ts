import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

// Mock all external dependencies before importing the store

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  // #2415: changePassword warms the per-origin machine-id cache before the
  // rotating POST, because apiFetch reads it SYNCHRONOUSLY and a session-restore
  // launch never populates it — leaving the continuation row with machine_id
  // NULL, which the server treats as permissive for theft detection.
  ensureMachineId: vi.fn().mockResolvedValue('test-machine-id'),
}));

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({ disconnect: vi.fn(), sendProfileUpdate: vi.fn() }),
  ConnectionState: { CONNECTED: 'connected', DISCONNECTED: 'disconnected' },
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    clearKeys: vi.fn(),
    isInitialized: false,
    initialize: vi.fn().mockResolvedValue(undefined),
    encryptPreferences: vi.fn(),
    // #2415: the post-adoption E2EE re-persist reads the freshly derived keyset.
    getSessionKeys: vi.fn().mockReturnValue(null),
  },
}));

// changePassword fails closed via nuclearReset when the new keyset is torn down after the
// server-side change already committed (#2333 salvage).
const mockNuclearReset = vi.fn();
vi.mock('@/renderer/services/resetService', () => ({
  nuclearReset: (...args: unknown[]) => mockNuclearReset(...args),
  gracefulReset: vi.fn(),
  recoveryReset: vi.fn(),
  softRestart: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    stopWatching: vi.fn(),
    startWatching: vi.fn(),
    pushPreferences: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    stopWatching: vi.fn(),
    startWatching: vi.fn(),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    stopWatching: vi.fn(),
    startWatching: vi.fn(),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    reset: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(true),
    save: vi.fn().mockResolvedValue(true),
  },
}));

// #2200: rotation fetches go through the transport as a unit (its own test file
// covers wire behavior); default every domain to authoritative absence.
const mockFetchBlobRowForRotation = vi.fn();
const mockPushEncryptedBlob = vi.fn();
vi.mock('@/renderer/services/e2eeBlobTransport', () => ({
  fetchBlobRowForRotation: (...args: unknown[]) => mockFetchBlobRowForRotation(...args),
  pushEncryptedBlob: (...args: unknown[]) => mockPushEncryptedBlob(...args),
}));

// Mock all crypto functions
vi.mock('@/renderer/utils/crypto', () => ({
  deriveKeyFromPassword: vi.fn().mockResolvedValue({} as CryptoKey),
  deriveKeyArgon2id: vi.fn().mockResolvedValue({} as CryptoKey),
  derivePreferencesKeyArgon2id: vi.fn().mockResolvedValue({} as CryptoKey),
  unwrapPrivateKey: vi.fn().mockResolvedValue({} as CryptoKey),
  wrapPrivateKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
  encryptBlob: vi.fn().mockResolvedValue('new-preference-key-ciphertext'),
  generateSalt: vi.fn().mockReturnValue(new Uint8Array(16)),
  base64ToArrayBuffer: vi.fn().mockReturnValue(new ArrayBuffer(32)),
  arrayBufferToBase64: vi.fn().mockReturnValue('mock-base64-string'),
}));

import { useUserStore } from '@/renderer/stores/auth/userStore';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { ensureMachineId } from '@/renderer/services/apiClient';
import { E2EEInitTeardownError } from '@/renderer/services/e2eeErrors';
import { apiFetch } from '@/renderer/services/apiClient';
import { e2eeService } from '@/renderer/services/e2eeService';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import {
  derivePreferencesKeyArgon2id,
  encryptBlob,
  unwrapPrivateKey,
} from '@/renderer/utils/crypto';
import { usePresenceOverrideStore } from '@/renderer/stores/ui/presenceOverrideStore';
import { useSavedGifsStore } from '@/renderer/stores/chat/savedGifsStore';
import { useFriendOrgStore } from '@/renderer/stores/chat/friendOrgStore';
import { mockUser } from '../../mocks/fixtures';
import { deferred } from '../../helpers/deferred';

const mockApiFetch = vi.mocked(apiFetch);
const mockDerivePreferencesKey = vi.mocked(derivePreferencesKeyArgon2id);
const mockEncryptBlob = vi.mocked(encryptBlob);
const mockUnwrapPrivateKey = vi.mocked(unwrapPrivateKey);

/**
 * #2415: the continuation triple an ADOPTING server appends to a committed 2xx
 * (`appendContinuationPair`). Spread into every committed-2xx fixture that models
 * a current server — without it the body is the "new client / old server" case
 * (spec 5), which deliberately routes to `failClosedToReauth` and returns
 * `{ success: false }`. That is intended behaviour, not a regression, so a fixture
 * asserting a successful change must carry all three fields.
 */
const CONTINUATION_FIELDS = {
  access_token: 'cont-at',
  refresh_token: 'cont-rt',
  session_id: 'cont-sid',
} as const;

describe('userStore - changePassword', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useUserStore.setState({ user: mockUser as any, isLoading: false, error: null });
    useAuthStore.getState().setAccessToken('mock-access');
    useAuthStore.getState().setLoginNotice(null);
    usePresenceOverrideStore.getState().reset();
    useSavedGifsStore.getState().reset();
    useFriendOrgStore.getState().reset();
    vi.mocked(presenceOverrideSyncService.fetchAndApply).mockResolvedValue(true);
    vi.mocked(presenceOverrideSyncService.save).mockResolvedValue(true);
    mockFetchBlobRowForRotation.mockResolvedValue({ kind: 'absent' });
    mockPushEncryptedBlob.mockResolvedValue(undefined);
    // vi.clearAllMocks() clears usage data only, so restate the defaults here.
    // Every persistent implementation a later test installs (`mockResolvedValue`,
    // not `...Once`) outlives that test and silently becomes the module default
    // for the rest of the file — an order-dependence that only stays green
    // because the default file order happens to run the victims first. Restating
    // each one here makes every test start from the declared module default,
    // whatever order the runner picks.
    vi.mocked(e2eeService.getSessionKeys).mockReturnValue(null);
    mockEncryptBlob.mockResolvedValue('new-preference-key-ciphertext');
    vi.mocked(e2eeService.encryptPreferences).mockResolvedValue(undefined as never);
  });

  it('successfully changes password with re-wrapped keys', async () => {
    const excludedUserIds = ['11111111-1111-4111-8111-111111111111'];
    usePresenceOverrideStore.getState().apply(excludedUserIds, 7);
    const savedGifSnapshot = [{ slug: 'focus-cat', savedAt: 123 }];
    useSavedGifsStore.getState()._setGifs(savedGifSnapshot);
    const friendOrgSnapshot = {
      v: 1 as const,
      categories: [
        {
          id: 'cat_11111111-1111-4111-8111-111111111111',
          name: 'Core team',
          emoji: '🔒',
          color: '#123456',
          memberIds: ['22222222-2222-4222-8222-222222222222'],
        },
      ],
      sectionOrder: ['cat_11111111-1111-4111-8111-111111111111'],
    };
    useFriendOrgStore.getState()._hydrate(friendOrgSnapshot);

    // Mock GET /api/v1/users/me/keys
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({
        e2ee_keys: {
          wrapped_private_key: 'old-wrapped-key',
          key_derivation_salt: 'old-salt',
        },
      }),
    } as Response);

    // Mock POST /api/v1/users/me/password
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({
        message: 'Password changed successfully',
        presence_override_version: 8,
        ...CONTINUATION_FIELDS,
      }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({ success: true });
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
    expect(mockApiFetch).toHaveBeenNthCalledWith(
      1,
      '/api/v1/users/me/keys',
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    );
    expect(mockApiFetch).toHaveBeenNthCalledWith(
      2,
      '/api/v1/users/me/password',
      expect.objectContaining({
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
    );
    const requestBody = JSON.parse(String(mockApiFetch.mock.calls[1]?.[1]?.body)) as Record<
      string,
      unknown
    >;
    expect(requestBody.presence_override).toEqual({
      encrypted_data: 'new-preference-key-ciphertext',
      expected_version: 7,
    });
    expect(mockDerivePreferencesKey).toHaveBeenCalledWith('newpass', expect.any(Uint8Array));
    expect(mockEncryptBlob).toHaveBeenCalledWith({ v: 1, excludedUserIds }, expect.anything());
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual(excludedUserIds);
    expect(usePresenceOverrideStore.getState().appliedVersion).toBe(8);
    expect(presenceOverrideSyncService.save).not.toHaveBeenCalled();
    // #2200: the domains ride the atomic sync_domains submission; the old
    // post-commit best-effort pushes are gone (see the dedicated suite below).
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    expect(requestBody.sync_domains).toEqual({
      preferences: { expected_version: 0 },
      saved_gifs: { expected_version: 0 },
      friend_organization: { expected_version: 0 },
    });
  });

  it('refreshes authoritative overrides after a conflict so an explicit retry can succeed', async () => {
    const excludedUserIds = ['11111111-1111-4111-8111-111111111111'];
    const authoritativeUserIds = ['22222222-2222-4222-8222-222222222222'];
    usePresenceOverrideStore.getState().apply(excludedUserIds, 7);
    vi.mocked(presenceOverrideSyncService.fetchAndApply).mockImplementationOnce(async () => {
      usePresenceOverrideStore.getState().apply(authoritativeUserIds, 8);
      return true;
    });
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: false,
        status: 409,
        json: async () => ({
          code: 'presence_override_version_conflict',
          current_version: 8,
        }),
      } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({
      success: false,
      error: 'Presence exceptions changed on another device. Please retry password change.',
    });
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    expect(presenceOverrideSyncService.save).not.toHaveBeenCalled();
    expect(presenceOverrideSyncService.fetchAndApply).toHaveBeenCalledOnce();
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual(authoritativeUserIds);
    expect(usePresenceOverrideStore.getState().appliedVersion).toBe(8);

    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 9, ...CONTINUATION_FIELDS }),
      } as Response);

    await expect(useUserStore.getState().changePassword('oldpass', 'newpass')).resolves.toEqual({
      success: true,
    });
    const retryBody = JSON.parse(String(mockApiFetch.mock.calls[3]?.[1]?.body)) as {
      presence_override?: { expected_version?: number };
    };
    expect(retryBody.presence_override?.expected_version).toBe(8);
  });

  it('sends version zero for an absent override preference and keeps it absent locally', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          message: 'Password changed successfully',
          presence_override_version: 0,
          ...CONTINUATION_FIELDS,
        }),
      } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({ success: true });
    const requestBody = JSON.parse(String(mockApiFetch.mock.calls[1]?.[1]?.body)) as Record<
      string,
      { expected_version?: number }
    >;
    expect(requestBody.presence_override?.expected_version).toBe(0);
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual([]);
    expect(usePresenceOverrideStore.getState().appliedVersion).toBe(0);
    expect(presenceOverrideSyncService.save).not.toHaveBeenCalled();
  });

  it('cancels a held password POST when logout clears the initiating account', async () => {
    const heldPost = deferred<Response>();
    usePresenceOverrideStore.getState().apply(['11111111-1111-4111-8111-111111111111'], 7);
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockImplementationOnce(() => heldPost.promise);

    const passwordChange = useUserStore.getState().changePassword('oldpass', 'newpass');
    await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));

    const postSignal = (mockApiFetch.mock.calls[1]?.[1] as RequestInit | undefined)?.signal;
    useUserStore.getState().clearUser();

    expect(postSignal?.aborted).toBe(true);
    heldPost.resolve({
      ok: true,
      status: 200,
      json: async () => ({ presence_override_version: 8 }),
    } as Response);

    await expect(passwordChange).resolves.toEqual({
      success: false,
      error: 'Password change was cancelled',
    });
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState().appliedVersion).toBe(7);
  });

  it('cancels a held password POST when a token-only auth lifecycle switches', async () => {
    const heldPost = deferred<Response>();
    useAuthStore.getState().setSessionId(null);
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockImplementationOnce(() => heldPost.promise);

    const passwordChange = useUserStore.getState().changePassword('oldpass', 'newpass');
    await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
    const postSignal = (mockApiFetch.mock.calls[1]?.[1] as RequestInit | undefined)?.signal;

    useAuthStore.getState().setAccessToken('different-account-token');

    expect(postSignal?.aborted).toBe(true);
    heldPost.resolve({
      ok: true,
      status: 200,
      json: async () => ({ presence_override_version: 8 }),
    } as Response);
    await expect(passwordChange).resolves.toEqual({
      success: false,
      error: 'Password change was cancelled',
    });
    expect(e2eeService.initialize).not.toHaveBeenCalled();
  });

  it('does not apply old-account state when the account switches during E2EE initialization', async () => {
    const heldInitialize = deferred<void>();
    const excludedUserIds = ['11111111-1111-4111-8111-111111111111'];
    usePresenceOverrideStore.getState().apply(excludedUserIds, 7);
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 8, ...CONTINUATION_FIELDS }),
      } as Response);
    vi.mocked(e2eeService.initialize).mockImplementationOnce(() => heldInitialize.promise);

    const passwordChange = useUserStore.getState().changePassword('oldpass', 'newpass');
    await vi.waitFor(() => expect(e2eeService.initialize).toHaveBeenCalledTimes(1));

    useUserStore.getState().setUser({ ...mockUser, id: '22222222-2222-4222-8222-222222222222' });
    const initializationGuard = vi.mocked(e2eeService.initialize).mock.calls[0]?.[4];
    expect(initializationGuard?.signal.aborted).toBe(true);
    heldInitialize.resolve();

    await expect(passwordChange).resolves.toEqual({
      success: false,
      error: 'Password change was cancelled',
    });
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual(excludedUserIds);
    expect(usePresenceOverrideStore.getState().appliedVersion).toBe(7);
  });

  it('fails closed (re-auth notice) when the keyset is torn down AFTER the password POST committed (#2333)', async () => {
    // The POST has already rotated the password + revoked refresh tokens. A teardown
    // during the new-key derivation surfaces as E2EEInitTeardownError (#2337). Reporting
    // a retryable "Password change was cancelled" would be a lie (the old password is
    // gone) and leave the session admitted-without-E2EE. It must fail closed with an
    // honest re-auth message + a login-screen notice that survives nuclearReset.
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: { wrapped_private_key: 'old-wrapped-key', key_derivation_salt: 'old-salt' },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 8, ...CONTINUATION_FIELDS }),
      } as Response);
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new E2EEInitTeardownError());

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    // NOT the retryable-cancellation lie:
    expect(result.error).not.toBe('Password change was cancelled');
    expect(result.error).toContain('sign in again with your new password');
    // Reset-surviving login notice staged (auth store preserves it across nuclearReset):
    expect(useAuthStore.getState().loginNotice).toContain('sign in again with your new password');
    // Fail-closed teardown so the session cannot linger admitted-without-E2EE:
    expect(mockNuclearReset).toHaveBeenCalledTimes(1);
    // ...and no post-change work ran against the aborted keyset.
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
  });

  // #2422: a fulfilled 2xx whose body is unreadable/malformed is commit evidence,
  // so it must fail closed to re-auth (clear old-key custody, keep watchers
  // stopped) rather than throw → be reported as ordinary failure → restart the
  // old-key sync watchers over an already-rotated server.
  const keysOk = {
    ok: true,
    status: 200,
    json: async () => ({
      e2ee_keys: { wrapped_private_key: 'old-wrapped-key', key_derivation_salt: 'old-salt' },
    }),
  } as Response;

  it('#2422: 200 whose JSON is unreadable fails closed to re-auth and leaves watchers stopped', async () => {
    // A real nuclearReset clears the token; model that (Once, so it does not leak
    // into later tests) so the finally sees the superseded lifecycle and does NOT
    // restart the quiesced watchers.
    mockNuclearReset.mockImplementationOnce(() => {
      useAuthStore.getState().clearAccessToken();
    });
    mockApiFetch.mockResolvedValueOnce(keysOk).mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => {
        throw new SyntaxError('Unexpected end of JSON input');
      },
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    expect(result.error).toContain('sign in again with your new password');
    expect(useAuthStore.getState().loginNotice).toContain('sign in again with your new password');
    expect(mockNuclearReset).toHaveBeenCalledTimes(1);
    // Never proceeded to reinit or re-pushed domains against an already-committed change.
    expect(e2eeService.initialize).not.toHaveBeenCalled();
    expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    // The data-loss crux: watchers were stopped for the rotation and NOT restarted.
    expect(preferencesSyncService.stopWatching).toHaveBeenCalled();
    expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
    expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();
    expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
  });

  it('#2422: 200 with an invalid presence_override_version fails closed to re-auth', async () => {
    mockApiFetch.mockResolvedValueOnce(keysOk).mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({ presence_override_version: 'not-a-number' }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.error).toContain('sign in again with your new password');
    expect(mockNuclearReset).toHaveBeenCalledTimes(1);
    expect(e2eeService.initialize).not.toHaveBeenCalled();
  });

  it('#2422: 200 with malformed sync_domain_versions fails closed to re-auth', async () => {
    mockApiFetch.mockResolvedValueOnce(keysOk).mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({ presence_override_version: 8, sync_domain_versions: 'garbage' }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.error).toContain('sign in again with your new password');
    expect(mockNuclearReset).toHaveBeenCalledTimes(1);
  });

  it('#2422: a lifecycle superseded during the unreadable-2xx parse does not reset the successor', async () => {
    mockApiFetch.mockResolvedValueOnce(keysOk).mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => {
        // A newer session became current (token cleared) before we read the body.
        useAuthStore.getState().clearAccessToken();
        throw new SyntaxError('Unexpected end of JSON input');
      },
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    // Cancellation stays a cancellation; the stale flow must NOT nuclearReset the successor.
    expect(result.error).not.toContain('sign in again with your new password');
    expect(mockNuclearReset).not.toHaveBeenCalled();
  });

  it('#2422: a malformed non-2xx (500) is NOT promoted to a committed outcome', async () => {
    mockApiFetch.mockResolvedValueOnce(keysOk).mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => {
        throw new SyntaxError('Unexpected end of JSON input');
      },
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    // Not a committed outcome: no re-auth notice, no custody teardown.
    expect(result.error).not.toContain('sign in again with your new password');
    expect(mockNuclearReset).not.toHaveBeenCalled();
  });

  it('stages the notice but skips a redundant nuclearReset when the teardown already superseded this change (#2333)', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: { wrapped_private_key: 'old-wrapped-key', key_derivation_salt: 'old-salt' },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 8, ...CONTINUATION_FIELDS }),
      } as Response);
    // The teardown that throws E2EEInitTeardownError also cleared the token (as a real
    // nuclearReset would), so the password-change lifecycle is already superseded when the
    // fail-closed path runs: nuclearReset must NOT fire again (isCurrent() is false), but
    // the loginNotice still stages (clearAccessToken preserves it).
    vi.mocked(e2eeService.initialize).mockImplementationOnce(async () => {
      useAuthStore.getState().clearAccessToken();
      throw new E2EEInitTeardownError();
    });

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.error).toContain('sign in again with your new password');
    expect(useAuthStore.getState().loginNotice).toContain('sign in again with your new password');
    expect(mockNuclearReset).not.toHaveBeenCalled();
  });

  it('does NOT stage the notice or reset when a DIFFERENT authenticated session became current (#2333)', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: { wrapped_private_key: 'old-wrapped-key', key_derivation_salt: 'old-salt' },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 8, ...CONTINUATION_FIELDS }),
      } as Response);
    // A newer login became current mid-re-init (its token replaced ours). The stale
    // password-change flow must NOT nuclearReset it and must NOT clobber its loginNotice.
    vi.mocked(e2eeService.initialize).mockImplementationOnce(async () => {
      useAuthStore.getState().setAccessToken('newer-session-token');
      throw new E2EEInitTeardownError();
    });

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    // The newer session's notice channel is left untouched.
    expect(useAuthStore.getState().loginNotice).toBeNull();
    expect(mockNuclearReset).not.toHaveBeenCalled();
  });

  it('propagates a NON-teardown init error to the generic failure (no re-auth notice) (#2333)', async () => {
    mockApiFetch
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: { wrapped_private_key: 'old-wrapped-key', key_derivation_salt: 'old-salt' },
        }),
      } as Response)
      .mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ presence_override_version: 8, ...CONTINUATION_FIELDS }),
      } as Response);
    // A non-teardown error is a normal failure — it must propagate unchanged, NOT trigger
    // the fail-closed re-auth path (no notice, no nuclearReset).
    vi.mocked(e2eeService.initialize).mockRejectedValueOnce(new Error('init boom'));

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result.success).toBe(false);
    expect(result.error).toBe('init boom');
    expect(useAuthStore.getState().loginNotice).toBeNull();
    expect(mockNuclearReset).not.toHaveBeenCalled();
  });

  it('returns error when current password is incorrect (401)', async () => {
    // Mock GET /api/v1/users/me/keys - succeeds
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({
        e2ee_keys: {
          wrapped_private_key: 'old-wrapped-key',
          key_derivation_salt: 'old-salt',
        },
      }),
    } as Response);

    // Mock POST /api/v1/users/me/password - returns 401 with password error
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 401,
      json: async () => ({ error: 'Current password is incorrect' }),
    } as Response);

    const result = await useUserStore.getState().changePassword('wrongpass', 'newpass');

    expect(result).toEqual({ success: false, error: 'Current password is incorrect' });
    // Auth tokens should NOT be cleared when the error is about wrong password
    expect(useAuthStore.getState().accessToken).toBe('mock-access');
  });

  it('clears auth and returns session expired for generic 401', async () => {
    // Mock GET /api/v1/users/me/keys - succeeds
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({
        e2ee_keys: {
          wrapped_private_key: 'old-wrapped-key',
          key_derivation_salt: 'old-salt',
        },
      }),
    } as Response);

    // Mock POST /api/v1/users/me/password - returns generic 401
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 401,
      json: async () => ({ error: 'Token expired' }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({ success: false, error: 'Session expired' });
    // Auth tokens should be cleared for a generic 401
    expect(useAuthStore.getState().accessToken).toBeNull();
  });

  it('handles general error (e.g., key fetch failure)', async () => {
    // Mock GET /api/v1/users/me/keys - fails
    mockApiFetch.mockResolvedValueOnce({
      ok: false,
      status: 500,
      json: async () => ({ error: 'Internal server error' }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({ success: false, error: 'Internal server error' });
    // Should only have called the keys endpoint, not the password endpoint
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  it('handles crypto unwrap failure gracefully', async () => {
    // Mock GET /api/v1/users/me/keys - succeeds
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      status: 200,
      json: async () => ({
        e2ee_keys: {
          wrapped_private_key: 'old-wrapped-key',
          key_derivation_salt: 'old-salt',
        },
      }),
    } as Response);

    // Simulate crypto failure (wrong password causes unwrap to fail)
    mockUnwrapPrivateKey.mockRejectedValueOnce(new Error('Decryption failed'));

    const result = await useUserStore.getState().changePassword('wrongpass', 'newpass');

    expect(result).toEqual({ success: false, error: 'Decryption failed' });
    // The password endpoint should not have been called
    expect(mockApiFetch).toHaveBeenCalledTimes(1);
  });

  describe('atomic sync-domain rotation (#2200)', () => {
    const keysResponse = {
      ok: true,
      status: 200,
      json: async () => ({
        e2ee_keys: {
          wrapped_private_key: 'old-wrapped-key',
          key_derivation_salt: 'old-salt',
        },
      }),
    } as Response;
    const successResponse = (syncVersions: Record<string, number>) =>
      ({
        ok: true,
        status: 200,
        json: async () => ({
          message: 'Password changed successfully',
          presence_override_version: 0,
          ...CONTINUATION_FIELDS,
          sync_domain_versions: syncVersions,
        }),
      }) as Response;

    function mockDomainRows(rows: {
      preferences?: unknown;
      saved_gifs?: unknown;
      friend_organization?: unknown;
    }): void {
      mockFetchBlobRowForRotation.mockImplementation(
        async (_endpoint: unknown, responseKey: unknown) =>
          rows[responseKey as keyof typeof rows] ?? { kind: 'absent' }
      );
    }

    function lastPasswordPostBody(): Record<string, any> {
      const postCall = mockApiFetch.mock.calls
        .filter(([url]) => url === '/api/v1/users/me/password')
        .at(-1);
      expect(postCall).toBeDefined();
      return JSON.parse((postCall![1] as RequestInit).body as string);
    }

    it('assembles rotate/absent/preserve submissions from the SERVER rows, not local stores', async () => {
      // Local stores deliberately seeded non-empty: absence must come from the server fetch.
      useSavedGifsStore.getState()._setGifs([{ slug: 'local-only-gif', savedAt: 1 }]);
      mockDomainRows({
        preferences: { kind: 'present', version: 4, plaintext: { v: 1, data: { theme: 'dark' } } },
        saved_gifs: { kind: 'absent' },
        friend_organization: { kind: 'undecryptable', version: 9 },
      });
      mockEncryptBlob.mockResolvedValue('rotated-prefs-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce(
        successResponse({ preferences: 5, saved_gifs: 0, friend_organization: 9 })
      );

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      const body = lastPasswordPostBody();
      expect(body.sync_domains).toEqual({
        preferences: { encrypted_data: 'rotated-prefs-ciphertext', expected_version: 4 },
        saved_gifs: { expected_version: 0 },
        friend_organization: { expected_version: 9 },
      });
      // Re-encryption input is the FETCHED plaintext under the NEW preferences key.
      const newKey = await mockDerivePreferencesKey.mock.results[0]!.value;
      expect(mockEncryptBlob).toHaveBeenCalledWith({ v: 1, data: { theme: 'dark' } }, newKey);
    });

    it('aborts before the POST when any rotation fetch fails', async () => {
      mockDomainRows({ saved_gifs: { kind: 'error' } });
      mockApiFetch.mockResolvedValueOnce(keysResponse);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/Could not fetch encrypted settings/);
      expect(
        mockApiFetch.mock.calls.filter(([url]) => url === '/api/v1/users/me/password')
      ).toHaveLength(0);
    });

    it('retries exactly once on a sync-domain version conflict with fresh fetches', async () => {
      mockDomainRows({
        preferences: { kind: 'present', version: 4, plaintext: { v: 1, data: {} } },
      });
      mockEncryptBlob.mockResolvedValue('rotated-prefs-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 409,
        json: async () => ({
          code: 'sync_domain_version_conflict',
          domain: 'preferences',
          current_version: 5,
        }),
      } as Response);
      mockApiFetch.mockResolvedValueOnce(
        successResponse({ preferences: 5, saved_gifs: 0, friend_organization: 0 })
      );

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      expect(
        mockApiFetch.mock.calls.filter(([url]) => url === '/api/v1/users/me/password')
      ).toHaveLength(2);
      expect(mockFetchBlobRowForRotation.mock.calls.length).toBe(6); // 3 domains × 2 attempts
    });

    it('surfaces a retryable error after a second sync-domain conflict', async () => {
      const conflictResponse = {
        ok: false,
        status: 409,
        json: async () => ({
          code: 'sync_domain_version_conflict',
          domain: 'saved_gifs',
          current_version: 3,
        }),
      } as Response;
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce(conflictResponse);
      mockApiFetch.mockResolvedValueOnce(conflictResponse);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/changed on another device/);
      expect(
        mockApiFetch.mock.calls.filter(([url]) => url === '/api/v1/users/me/password')
      ).toHaveLength(2);
    });

    // #2415 BEHAVIOUR CHANGE (was: "treats a transport failure as COMMITTED").
    // A `committed-ambiguous` outcome has NO response body and therefore no
    // continuation pair, so adoption fails closed by design. The salt-match
    // reconciliation still proves the change COMMITTED — fail-closed re-auth is
    // exactly what a committed-but-unadoptable session must produce, and is not a
    // retryable cancellation. (Context: `reconcileAmbiguousPasswordCommit`
    // re-fetches with the OLD access token, which the credential-epoch fence now
    // rejects, so a true commit rarely reaches this arm at all; the arm and this
    // lock stay because the 'not-committed' branch below is still live.)
    it('fails closed to re-auth when a transport failure resolves to a COMMITTED change', async () => {
      // A real nuclearReset clears the token; model it (Once) so the finally sees
      // the superseded lifecycle and leaves the quiesced watchers stopped.
      mockNuclearReset.mockImplementationOnce(() => {
        useAuthStore.getState().clearAccessToken();
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockRejectedValueOnce(new TypeError('network dropped'));
      // Reconcile fetch: server already persisted the salt this attempt submitted
      // (arrayBufferToBase64 is mocked to a constant, so the submitted salt is 'mock-base64-string').
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'new-wrapped-key', // pragma: allowlist secret
            key_derivation_salt: 'mock-base64-string',
          },
        }),
      } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      expect(useAuthStore.getState().loginNotice).toContain('sign in again with your new password');
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      // Nothing sequenced after adoption may run on the fenced old token: no
      // reinit, no presence refetch.
      expect(e2eeService.initialize).not.toHaveBeenCalled();
      expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();
      // Absence of a pair is NEVER retried (the routes are rate-limited).
      expect(
        mockApiFetch.mock.calls.filter(([url]) => url === '/api/v1/users/me/password')
      ).toHaveLength(1);
      // Watchers quiesced for the rotation and deliberately left stopped.
      expect(preferencesSyncService.stopWatching).toHaveBeenCalled();
      expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
      expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();
      expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
    });

    it('reports a retryable not-committed outcome when the persisted salt is unchanged', async () => {
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockRejectedValueOnce(new TypeError('network dropped'));
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/did not complete/);
      expect(e2eeService.initialize).not.toHaveBeenCalled();
    });

    it('reports an unknown outcome when the reconcile fetch itself fails', async () => {
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockRejectedValueOnce(new TypeError('network dropped'));
      mockApiFetch.mockRejectedValueOnce(new TypeError('still down'));

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/outcome is unknown/);
      expect(e2eeService.initialize).not.toHaveBeenCalled();
    });

    it('re-pushes decryptable domains under the new key when a skewed server omits sync_domain_versions', async () => {
      mockDomainRows({
        preferences: { kind: 'present', version: 4, plaintext: { v: 1, data: { theme: 'dark' } } },
        saved_gifs: { kind: 'absent' },
        friend_organization: { kind: 'undecryptable', version: 9 },
      });
      vi.mocked(e2eeService.encryptPreferences).mockResolvedValue('repair-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      // Old server: commits the password, returns NO sync_domain_versions.
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          message: 'Password changed successfully',
          presence_override_version: 0,
          ...CONTINUATION_FIELDS,
        }),
      } as Response);
      // Repair PUT for the one decryptable domain.
      mockApiFetch.mockResolvedValueOnce({ ok: true, status: 200 } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      // Only the decryptable domain has plaintext to restore; absent and
      // undecryptable domains have nothing to push.
      expect(e2eeService.encryptPreferences).toHaveBeenCalledWith({
        v: 1,
        data: { theme: 'dark' },
      });
      const repairCall = mockApiFetch.mock.calls.at(-1)!;
      expect(repairCall[0]).toBe('/api/v1/users/me/preferences');
      expect((repairCall[1] as RequestInit).method).toBe('PUT');
      expect(JSON.parse((repairCall[1] as RequestInit).body as string)).toEqual({
        encrypted_data: 'repair-ciphertext',
      });
    });

    it('surfaces a repair failure honestly instead of reporting clean success', async () => {
      mockDomainRows({
        preferences: { kind: 'present', version: 4, plaintext: { v: 1, data: {} } },
      });
      vi.mocked(e2eeService.encryptPreferences).mockResolvedValue('repair-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({ message: 'ok', presence_override_version: 0, ...CONTINUATION_FIELDS }),
      } as Response);
      // Repair PUT fails.
      mockApiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/password WAS changed/);
      expect(result.error).toMatch(/preferences/);
    });

    // #2415 BEHAVIOUR CHANGE (was: "runs the same new-key repair after an
    // ambiguous commit resolves to committed"). The repair PUTs would run on the
    // OLD access token, which the credential-epoch fence rejects, and the
    // ambiguous arm can carry no continuation pair to replace it — so the flow
    // fails closed BEFORE the repush instead of issuing PUTs that must 401.
    it('never repushes domains after an ambiguous commit — it fails closed first', async () => {
      mockNuclearReset.mockImplementationOnce(() => {
        useAuthStore.getState().clearAccessToken();
      });
      mockDomainRows({
        saved_gifs: { kind: 'present', version: 2, plaintext: { v: 1, data: { gifs: [] } } },
      });
      vi.mocked(e2eeService.encryptPreferences).mockResolvedValue('repair-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockRejectedValueOnce(new TypeError('network dropped'));
      // Reconcile: persisted salt matches the submitted one → committed-ambiguous.
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'new-wrapped-key', // pragma: allowlist secret
            key_derivation_salt: 'mock-base64-string',
          },
        }),
      } as Response);
      // Deliberately NO repair-PUT response queued: a repush must never be attempted,
      // and an unconsumed `...Once` would leak into the next test in this file.

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      expect(e2eeService.encryptPreferences).not.toHaveBeenCalled();
      expect(
        mockApiFetch.mock.calls.filter(
          ([, init]) => (init as RequestInit | undefined)?.method === 'PUT'
        )
      ).toHaveLength(0);
    });

    it('quiesces the sync watchers around the rotation and restarts them for the current session', async () => {
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce(
        successResponse({ preferences: 0, saved_gifs: 0, friend_organization: 0 })
      );

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      expect(preferencesSyncService.stopWatching).toHaveBeenCalled();
      expect(savedGifsSyncService.stopWatching).toHaveBeenCalled();
      expect(friendOrgSyncService.stopWatching).toHaveBeenCalled();
      expect(preferencesSyncService.startWatching).toHaveBeenCalled();
      expect(savedGifsSyncService.startWatching).toHaveBeenCalled();
      expect(friendOrgSyncService.startWatching).toHaveBeenCalled();
    });

    it('no longer issues the superseded post-commit domain pushes on success', async () => {
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockResolvedValueOnce(
        successResponse({ preferences: 0, saved_gifs: 0, friend_organization: 0 })
      );

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      // The superseded snapshot-push methods were removed outright; only the
      // still-existing preferences push can regress here.
      expect(preferencesSyncService.pushPreferences).not.toHaveBeenCalled();
    });
  });

  describe('continuation-pair adoption (#2415)', () => {
    const keysResponse = () =>
      ({
        ok: true,
        status: 200,
        json: async () => ({
          e2ee_keys: {
            wrapped_private_key: 'old-wrapped-key',
            key_derivation_salt: 'old-salt',
          },
        }),
      }) as Response;

    const committedWithPair = () =>
      ({
        ok: true,
        status: 200,
        json: async () => ({
          message: 'Password changed successfully',
          presence_override_version: 2,
          sync_domain_versions: { preferences: 1, saved_gifs: 1, friend_organization: 1 },
          access_token: 'cont-at',
          refresh_token: 'cont-rt',
          session_id: 'cont-sid',
        }),
      }) as Response;

    // tests/setup.ts defines window.electron non-configurably, so vi.stubGlobal
    // cannot redefine it; assign through and restore, as clipboard.test.ts does.
    const realElectron = globalThis.electron;
    function stubElectron(overrides: Record<string, unknown>): void {
      globalThis.electron = { ...realElectron, ...overrides } as typeof globalThis.electron;
    }
    beforeEach(() => {
      // vi.clearAllMocks() clears usage data but NOT queued `...Once` values or
      // implementations, so an upstream case that consumed fewer responses than
      // it seeded would otherwise hand this suite its leftovers (including a
      // deliberately never-resolving `initialize`). Seed both from empty.
      mockApiFetch.mockReset();
      vi.mocked(e2eeService.initialize).mockReset().mockResolvedValue(undefined);
    });

    afterEach(() => {
      globalThis.electron = realElectron;
    });

    it('adopts the continuation pair before any follow-up network call', async () => {
      const storeRefreshToken = vi.fn().mockResolvedValue(3);
      stubElectron({
        storeRefreshToken,
        storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true),
      });
      useAuthStore.setState({
        accessToken: 'old-at',
        sessionId: 'old-sid',
        authGeneration: 1,
        rememberMe: false,
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      // The adopted session survives: the change must NOT fail closed, and the
      // lifecycle watcher must not read the new session id as a switch.
      expect(result).toEqual({ success: true });
      expect(useAuthStore.getState().accessToken).toBe('cont-at');
      expect(useAuthStore.getState().sessionId).toBe('cont-sid');
      // A NEW lifecycle, not a refresh rotation — the bump is the point.
      expect(useAuthStore.getState().authGeneration).toBe(2);
      expect(useAuthStore.getState().loginNotice).toBeNull();
      expect(mockNuclearReset).not.toHaveBeenCalled();
      // The client's OWN rememberMe, never a server-supplied remember_me (spec 2.6 SE-2).
      expect(storeRefreshToken).toHaveBeenCalledTimes(1);
      expect(storeRefreshToken).toHaveBeenCalledWith(
        expect.objectContaining({
          refreshToken: 'cont-rt',
          accessToken: 'cont-at',
          rememberMe: false,
        })
      );
      // Adoption precedes the E2EE reinit, and therefore every later network call.
      expect(storeRefreshToken.mock.invocationCallOrder[0]).toBeLessThan(
        vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0]
      );
    });

    it('proceeds when the machine-id warm-up REJECTS, instead of escaping as an unhandled rejection', async () => {
      // The warm-up runs BEFORE beginPasswordChange, so an unguarded rejection
      // would escape changePassword entirely and the caller's spinner/error
      // handling would never run (Gitar, #2849). It degrades to warn-and-proceed:
      // ensureMachineId already returns '' when there is no bridge, so the only
      // way to reject is a broken bridge, and blocking a password change on that
      // is the worse trade.
      vi.mocked(ensureMachineId).mockRejectedValueOnce(new Error('IPC torn down'));
      const storeRefreshToken = vi.fn().mockResolvedValue(3);
      stubElectron({ storeRefreshToken, storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true) });
      useAuthStore.setState({
        accessToken: 'old-at',
        sessionId: 'old-sid',
        authGeneration: 1,
        rememberMe: false,
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      // Resolves normally — the caller can render an outcome either way.
      expect(result).toEqual({ success: true });
      expect(useAuthStore.getState().accessToken).toBe('cont-at');
    });

    it('re-persists E2EE keys under the new credential owner after the change', async () => {
      const storeRefreshToken = vi.fn().mockResolvedValue(9);
      const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
      stubElectron({ storeRefreshToken, storeE2EEKeysIfOwner });
      vi.mocked(e2eeService.getSessionKeys).mockReturnValue({
        marker: 'post-change-keys',
      } as unknown as ReturnType<typeof e2eeService.getSessionKeys>);
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      expect(result).toEqual({ success: true });
      expect(storeE2EEKeysIfOwner).toHaveBeenCalledWith({ marker: 'post-change-keys' }, 9);
      // Ordering is load-bearing: storeRefreshToken mints the owner BEFORE the
      // reinit, but the keys it persists do not exist until the reinit has run.
      expect(storeRefreshToken.mock.invocationCallOrder[0]).toBeLessThan(
        vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0]
      );
      expect(vi.mocked(e2eeService.initialize).mock.invocationCallOrder[0]).toBeLessThan(
        storeE2EEKeysIfOwner.mock.invocationCallOrder[0]
      );
    });

    // ── Case 2: absent / partial continuation fields ────────────────────────
    // The server omits all three fields when a CONCURRENT destructive flow
    // advanced the credential epoch in the post-commit window. Absence is a
    // deliberate security outcome, never a transport error, and is NEVER
    // retried: the epoch signal is unrecoverable from any later 401 (whose body
    // is deliberately generic, so it cannot serve as an epoch oracle) and both
    // routes are rate-limited per user. A partial set cannot come from a healthy
    // server — `appendContinuationPair` writes all three or none — so a proxy
    // that stripped one field, or a version-skewed server, must be
    // indistinguishable from full absence.
    const committedWithoutPair = (continuationFields: Record<string, unknown>) =>
      ({
        ok: true,
        status: 200,
        json: async () => ({
          message: 'Password changed successfully',
          presence_override_version: 8,
          sync_domain_versions: { preferences: 1, saved_gifs: 1, friend_organization: 1 },
          ...continuationFields,
        }),
      }) as Response;

    it.each([
      ['no continuation fields at all', {}],
      [
        'only access_token and refresh_token',
        { access_token: 'cont-at', refresh_token: 'cont-rt' },
      ],
      ['only access_token and session_id', { access_token: 'cont-at', session_id: 'cont-sid' }],
      ['only refresh_token and session_id', { refresh_token: 'cont-rt', session_id: 'cont-sid' }],
      [
        'an empty-string field in an otherwise complete set',
        { access_token: '', refresh_token: 'cont-rt', session_id: 'cont-sid' },
      ],
    ] as ReadonlyArray<[string, Record<string, unknown>]>)(
      'fails closed to re-auth when a committed 2xx carries %s',
      async (_label, continuationFields) => {
        // A real nuclearReset clears the token; model that (Once, so it does not
        // leak into later tests) so the `finally` sees the superseded lifecycle
        // and leaves the quiesced watchers stopped — matching the #2422 case above.
        mockNuclearReset.mockImplementationOnce(() => {
          useAuthStore.getState().clearAccessToken();
        });
        const storeRefreshToken = vi.fn().mockResolvedValue(3);
        stubElectron({ storeRefreshToken, storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true) });
        useAuthStore.setState({
          accessToken: 'old-at',
          sessionId: 'old-sid',
          authGeneration: 1,
          rememberMe: true,
          loginNotice: null,
        });
        mockApiFetch.mockResolvedValueOnce(keysResponse());
        mockApiFetch.mockResolvedValueOnce(committedWithoutPair(continuationFields));

        const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

        expect(result.success).toBe(false);
        expect(result.error).toContain('sign in again with your new password');
        // The notice survives clearAccessToken so the login screen can render it.
        expect(useAuthStore.getState().loginNotice).toContain(
          'sign in again with your new password'
        );
        expect(mockNuclearReset).toHaveBeenCalledTimes(1);
        // No partial adoption: a stripped field is never installed, and nothing
        // reaches the keychain on the fail-closed path.
        expect(useAuthStore.getState().accessToken).toBeNull();
        expect(storeRefreshToken).not.toHaveBeenCalled();
        // Nothing sequenced after adoption runs on the fenced old token.
        expect(e2eeService.initialize).not.toHaveBeenCalled();
        expect(presenceOverrideSyncService.fetchAndApply).not.toHaveBeenCalled();
        // ZERO retries — exactly the keys GET plus the one password POST.
        expect(mockApiFetch).toHaveBeenCalledTimes(2);
        // The data-loss crux: watchers quiesced for the rotation, left stopped.
        expect(preferencesSyncService.stopWatching).toHaveBeenCalled();
        expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
        expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();
        expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
      }
    );

    // ── Case 3: concurrent-epoch supersession ───────────────────────────────
    it('declines the CAS when the auth generation moved, leaving the successor session intact', async () => {
      const storeRefreshToken = vi.fn().mockResolvedValue(3);
      stubElectron({
        storeRefreshToken,
        storeE2EEKeysIfOwner: vi.fn().mockResolvedValue(true),
      });
      useAuthStore.setState({
        accessToken: 'old-at',
        sessionId: 'old-sid',
        authGeneration: 1,
        rememberMe: false,
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 200,
        json: async () => {
          // A successor lifecycle becomes current while the committed body is
          // being read. It keeps this session id on purpose: the coarse
          // lifecycle watcher keys on the session id, so leaving it unchanged
          // isolates the generation CAS as the ONLY guard left standing.
          useAuthStore.getState().beginAuthLifecycleIfCurrent(1, 'successor-at', 'old-sid');
          return {
            message: 'Password changed successfully',
            presence_override_version: 2,
            sync_domain_versions: { preferences: 1, saved_gifs: 1, friend_organization: 1 },
            access_token: 'cont-at',
            refresh_token: 'cont-rt',
            session_id: 'cont-sid',
          };
        },
      } as Response);

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      // The successor is NOT clobbered — no field of the pair was installed.
      expect(useAuthStore.getState().accessToken).toBe('successor-at');
      expect(useAuthStore.getState().sessionId).toBe('old-sid');
      expect(useAuthStore.getState().authGeneration).toBe(2);
      // No partial adoption: the republish never runs on a declined CAS, so the
      // keychain keeps the successor's refresh token.
      expect(storeRefreshToken).not.toHaveBeenCalled();
      expect(e2eeService.initialize).not.toHaveBeenCalled();
      // A declined CAS is itself proof a successor became current, so this arm
      // returns the notice DIRECTLY instead of routing through
      // failClosedToReauth — whose isCurrent() is blind to a generation-only
      // supersession and would nuclearReset the SUCCESSOR's credentials, E2EE
      // custody and disk token (CWE-672).
      expect(mockNuclearReset).not.toHaveBeenCalled();
    });

    // ── Case 4: the keychain republish ──────────────────────────────────────
    // `ipcRenderer.invoke` REJECTS when the main handler throws or the bridge is
    // torn down mid-call, and resolves a non-number when main declines. Both
    // mean restart-survival is gone and the keychain owner is unknown, so the
    // change must fail closed rather than continue with an adopted session whose
    // custody nobody holds.
    it('fails closed when the keychain republish REJECTS', async () => {
      // A real nuclearReset clears the token; model it (Once) so the `finally`
      // sees the superseded lifecycle and leaves the quiesced watchers stopped.
      // Without this the watcher assertions below would pass for the wrong
      // reason (see the #2422 case earlier in this file).
      mockNuclearReset.mockImplementationOnce(() => {
        useAuthStore.getState().clearAccessToken();
      });
      const storeRefreshToken = vi.fn().mockRejectedValue(new Error('keychain unavailable'));
      const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
      stubElectron({ storeRefreshToken, storeE2EEKeysIfOwner });
      useAuthStore.setState({
        accessToken: 'old-at',
        sessionId: 'old-sid',
        authGeneration: 1,
        rememberMe: false,
        loginNotice: null,
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      // The rejection is CAUGHT: unguarded it would unwind into changePassword's
      // catch and be reported as an ordinary retryable failure for a change that
      // COMMITTED, with the sync watchers restarted under the OLD preferences
      // key against already-rotated rows (the CWE-212 class #2422 fenced off).
      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      expect(useAuthStore.getState().loginNotice).toContain('sign in again with your new password');
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      expect(storeRefreshToken).toHaveBeenCalledTimes(1);
      // Nothing sequenced after the republish ran.
      expect(e2eeService.initialize).not.toHaveBeenCalled();
      expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
      // Watchers quiesced for the rotation and deliberately left stopped.
      expect(preferencesSyncService.stopWatching).toHaveBeenCalled();
      expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
      expect(savedGifsSyncService.startWatching).not.toHaveBeenCalled();
      expect(friendOrgSyncService.startWatching).not.toHaveBeenCalled();
    });

    it('fails closed — and never persists keys — when the republish resolves a NON-owner', async () => {
      mockNuclearReset.mockImplementationOnce(() => {
        useAuthStore.getState().clearAccessToken();
      });
      // Main declined the publish: the resolved value is not a CredentialOwner.
      const storeRefreshToken = vi.fn().mockResolvedValue({ status: 'rejected' });
      const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
      stubElectron({ storeRefreshToken, storeE2EEKeysIfOwner });
      vi.mocked(e2eeService.getSessionKeys).mockReturnValue({
        marker: 'post-change-keys',
      } as unknown as ReturnType<typeof e2eeService.getSessionKeys>);
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      // The actual hazard: the keychain owner is UNKNOWN, so writing the new
      // keyset under it would persist E2EE material against a bogus owner.
      expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
      expect(e2eeService.initialize).not.toHaveBeenCalled();
      expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
    });

    it('fails closed when the bridge is present but storeRefreshToken is MISSING', async () => {
      mockNuclearReset.mockImplementationOnce(() => {
        useAuthStore.getState().clearAccessToken();
      });
      // A preload regression, NOT a web/dev shell. Silently continuing would
      // leave the OLD, now-revoked token on the keychain and skip the E2EE
      // re-persist — restart survival lost with no signal.
      const storeE2EEKeysIfOwner = vi.fn().mockResolvedValue(true);
      const bridge: Record<string, unknown> = { ...realElectron, storeE2EEKeysIfOwner };
      delete bridge.storeRefreshToken;
      globalThis.electron = bridge as unknown as typeof globalThis.electron;
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      expect(result.success).toBe(false);
      expect(result.error).toContain('sign in again with your new password');
      expect(mockNuclearReset).toHaveBeenCalledTimes(1);
      expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
      expect(e2eeService.initialize).not.toHaveBeenCalled();
      expect(preferencesSyncService.startWatching).not.toHaveBeenCalled();
    });

    it('succeeds with NO republish and NO key re-persist when there is no bridge at all', async () => {
      // The web/dev shell. Named explicitly because it was previously covered
      // only incidentally, as a side effect of every success-path test — nothing
      // asserted that the *absence* of a bridge is a continue rather than a
      // fail-closed, which is the distinction the missing-method case above
      // draws against.
      globalThis.electron = undefined as unknown as typeof globalThis.electron;
      vi.mocked(e2eeService.getSessionKeys).mockReturnValue({
        marker: 'post-change-keys',
      } as unknown as ReturnType<typeof e2eeService.getSessionKeys>);
      useAuthStore.setState({
        accessToken: 'old-at',
        sessionId: 'old-sid',
        authGeneration: 1,
        rememberMe: false,
        loginNotice: null,
      });
      mockApiFetch.mockResolvedValueOnce(keysResponse());
      mockApiFetch.mockResolvedValueOnce(committedWithPair());

      const result = await useUserStore.getState().changePassword('old-pw', 'new-pw');

      expect(result).toEqual({ success: true });
      // The pair is still adopted in renderer state — only the persistence legs
      // are skipped, exactly as before #2415 (there was nothing to persist to).
      expect(useAuthStore.getState().accessToken).toBe('cont-at');
      expect(useAuthStore.getState().sessionId).toBe('cont-sid');
      expect(mockNuclearReset).not.toHaveBeenCalled();
      expect(useAuthStore.getState().loginNotice).toBeNull();
      // No owner was minted, so the E2EE re-persist is skipped outright rather
      // than run under a null owner.
      expect(realElectron?.storeRefreshToken).not.toHaveBeenCalled();
      expect(realElectron?.storeE2EEKeysIfOwner).not.toHaveBeenCalled();
      // A success still restarts the watchers for the adopted session.
      expect(preferencesSyncService.startWatching).toHaveBeenCalled();
    });
  });
});
