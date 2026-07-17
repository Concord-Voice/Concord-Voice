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
    pushPreferences: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    stopWatching: vi.fn(),
    pushSavedGifsSnapshot: vi.fn().mockResolvedValue(undefined),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    stopWatching: vi.fn(),
    pushFriendOrgSnapshot: vi.fn().mockResolvedValue(true),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    reset: vi.fn(),
    fetchAndApply: vi.fn().mockResolvedValue(true),
    save: vi.fn().mockResolvedValue(true),
  },
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
    expect(preferencesSyncService.pushPreferences).toHaveBeenCalledWith(
      expect.objectContaining({ signal: expect.any(AbortSignal), isCurrent: expect.any(Function) })
    );
    expect(savedGifsSyncService.pushSavedGifsSnapshot).toHaveBeenCalledWith(
      savedGifSnapshot,
      expect.objectContaining({ signal: expect.any(AbortSignal), isCurrent: expect.any(Function) })
    );
    expect(friendOrgSyncService.pushFriendOrgSnapshot).toHaveBeenCalledWith(
      friendOrgSnapshot,
      expect.objectContaining({ signal: expect.any(AbortSignal), isCurrent: expect.any(Function) })
    );
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
});
