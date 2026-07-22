import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock all external dependencies before importing the store

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
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

import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
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
import { usePresenceOverrideStore } from '@/renderer/stores/presenceOverrideStore';
import { useSavedGifsStore } from '@/renderer/stores/savedGifsStore';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { mockUser } from '../../mocks/fixtures';

const mockApiFetch = vi.mocked(apiFetch);
const mockDerivePreferencesKey = vi.mocked(derivePreferencesKeyArgon2id);
const mockEncryptBlob = vi.mocked(encryptBlob);
const mockUnwrapPrivateKey = vi.mocked(unwrapPrivateKey);

function deferred<T>(): {
  promise: Promise<T>;
  resolve: (value: T) => void;
} {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((resolvePromise) => {
    resolve = resolvePromise;
  });
  return { promise, resolve };
}

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
        json: async () => ({ presence_override_version: 9 }),
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
        json: async () => ({ presence_override_version: 8 }),
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
        json: async () => ({ presence_override_version: 8 }),
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
        json: async () => ({ presence_override_version: 8 }),
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
        json: async () => ({ presence_override_version: 8 }),
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
        json: async () => ({ presence_override_version: 8 }),
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

    it('treats a transport failure as COMMITTED when the persisted salt matches the submitted one', async () => {
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

      expect(result).toEqual({ success: true });
      expect(e2eeService.initialize).toHaveBeenCalled();
      // No response body existed, so the presence version is reconciled by refetch.
      expect(presenceOverrideSyncService.fetchAndApply).toHaveBeenCalled();
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
        json: async () => ({ message: 'ok', presence_override_version: 0 }),
      } as Response);
      // Repair PUT fails.
      mockApiFetch.mockResolvedValueOnce({ ok: false, status: 500 } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result.success).toBe(false);
      expect(result.error).toMatch(/password WAS changed/);
      expect(result.error).toMatch(/preferences/);
    });

    it('runs the same new-key repair after an ambiguous commit resolves to committed', async () => {
      mockDomainRows({
        saved_gifs: { kind: 'present', version: 2, plaintext: { v: 1, data: { gifs: [] } } },
      });
      vi.mocked(e2eeService.encryptPreferences).mockResolvedValue('repair-ciphertext');
      mockApiFetch.mockResolvedValueOnce(keysResponse);
      mockApiFetch.mockRejectedValueOnce(new TypeError('network dropped'));
      // Reconcile: persisted salt matches the submitted one → committed.
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
      // Repair PUT for the decryptable domain.
      mockApiFetch.mockResolvedValueOnce({ ok: true, status: 200 } as Response);

      const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

      expect(result).toEqual({ success: true });
      const repairCall = mockApiFetch.mock.calls.at(-1)!;
      expect(repairCall[0]).toBe('/api/v1/users/me/saved-gifs');
      expect((repairCall[1] as RequestInit).method).toBe('PUT');
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
});
