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

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    stopWatching: vi.fn(),
    pushPreferences: vi.fn().mockResolvedValue(undefined),
  },
}));

// Mock all crypto functions
vi.mock('@/renderer/utils/crypto', () => ({
  deriveKeyFromPassword: vi.fn().mockResolvedValue({} as CryptoKey),
  deriveKeyArgon2id: vi.fn().mockResolvedValue({} as CryptoKey),
  unwrapPrivateKey: vi.fn().mockResolvedValue({} as CryptoKey),
  wrapPrivateKey: vi.fn().mockResolvedValue(new ArrayBuffer(32)),
  generateSalt: vi.fn().mockReturnValue(new Uint8Array(16)),
  base64ToArrayBuffer: vi.fn().mockReturnValue(new ArrayBuffer(32)),
  arrayBufferToBase64: vi.fn().mockReturnValue('mock-base64-string'),
}));

import { useUserStore } from '@/renderer/stores/userStore';
import { useAuthStore } from '@/renderer/stores/authStore';
import { apiFetch } from '@/renderer/services/apiClient';
import { unwrapPrivateKey } from '@/renderer/utils/crypto';
import { mockUser } from '../../mocks/fixtures';

const mockApiFetch = vi.mocked(apiFetch);
const mockUnwrapPrivateKey = vi.mocked(unwrapPrivateKey);

describe('userStore - changePassword', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useUserStore.setState({ user: mockUser as any, isLoading: false, error: null });
    useAuthStore.getState().setAccessToken('mock-access');
  });

  it('successfully changes password with re-wrapped keys', async () => {
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
      json: async () => ({ success: true }),
    } as Response);

    const result = await useUserStore.getState().changePassword('oldpass', 'newpass');

    expect(result).toEqual({ success: true });
    expect(mockApiFetch).toHaveBeenCalledTimes(2);
    expect(mockApiFetch).toHaveBeenNthCalledWith(1, '/api/v1/users/me/keys');
    expect(mockApiFetch).toHaveBeenNthCalledWith(
      2,
      '/api/v1/users/me/password',
      expect.objectContaining({
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
      })
    );
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
