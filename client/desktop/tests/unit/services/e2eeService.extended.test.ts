// @vitest-environment node
/**
 * Extended tests for E2EE service — covers initializeFromStoredKeys,
 * getSessionKeys, getChannelKeyByVersion, decryptForChannelWithVersion,
 * createChannelKeys, wrapKeyForMember, rotateChannelKey, rate limiting,
 * concurrent fetch deduplication, and PBKDF2-to-Argon2id migration path.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
  encryptMessage,
  exportPublicKey,
} from '@/renderer/utils/crypto';

import { e2eeService } from '@/renderer/services/e2eeService';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';

// Mock apiFetch for channel key fetching
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

import { apiFetch } from '@/renderer/services/apiClient';
const mockApiFetch = vi.mocked(apiFetch);

describe('e2eeService — extended', () => {
  const testPassword = 'TestPassword123!';
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;

  beforeEach(async () => {
    e2eeService.clearKeys();
    // Reset rate limiter state (private field, not cleared by clearKeys)
    (e2eeService as any).rateLimitedUntil = 0;
    vi.clearAllMocks();
    regKeys = await generateRegistrationKeys(testPassword);
  });

  afterEach(() => {
    e2eeService.clearKeys();
  });

  describe('getSessionKeys', () => {
    it('returns null before initialization', () => {
      expect(e2eeService.getSessionKeys()).toBeNull();
    });

    it('returns session keys after initialization', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const keys = e2eeService.getSessionKeys();
      expect(keys).not.toBeNull();
      expect(keys!.wrappingKeyBase64).toBeTruthy();
      expect(keys!.preferencesKeyBase64).toBeTruthy();
      expect(keys!.wrappedPrivateKeyBase64).toBeTruthy();
    });
  });

  describe('initializeFromStoredKeys', () => {
    it('restores service from exported session keys', async () => {
      // First, initialize normally to get session keys
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const keys = e2eeService.getSessionKeys()!;

      // Clear and restore
      e2eeService.clearKeys();
      expect(e2eeService.isInitialized).toBe(false);

      await e2eeService.initializeFromStoredKeys(keys);
      expect(e2eeService.isInitialized).toBe(true);

      // Verify preferences round-trip works with restored keys
      const testData = { theme: 'dark', fontSize: 16 };
      const encrypted = await e2eeService.encryptPreferences(testData);
      const decrypted = await e2eeService.decryptPreferences<typeof testData>(encrypted);
      expect(decrypted).toEqual(testData);
    });
  });

  describe('getWrappingKey / getWrappedPrivateKey', () => {
    it('returns null wrapping key before initialization', () => {
      expect(e2eeService.getWrappingKey()).toBeNull();
    });

    it('returns empty string for wrapped private key before initialization', () => {
      expect(e2eeService.getWrappedPrivateKey()).toBe('');
    });

    it('returns wrapping key after initialization', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      expect(e2eeService.getWrappingKey()).not.toBeNull();
      expect(e2eeService.getWrappedPrivateKey()).toBeTruthy();
    });
  });

  describe('getPreferencesKeyBase64', () => {
    it('returns null before initialization', () => {
      expect(e2eeService.getPreferencesKeyBase64()).toBeNull();
    });

    it('returns base64 key after initialization', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const key = e2eeService.getPreferencesKeyBase64();
      expect(key).toBeTruthy();
      expect(typeof key).toBe('string');
    });
  });

  describe('getCurrentKeyVersion', () => {
    it('returns 1 for uncached channel', () => {
      expect(e2eeService.getCurrentKeyVersion('unknown-channel')).toBe(1);
    });

    it('returns cached version after fetching', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 5 } }),
      } as Response);

      await e2eeService.getChannelKey('channel-version-test');
      expect(e2eeService.getCurrentKeyVersion('channel-version-test')).toBe(5);
    });
  });

  describe('decryptWithKey', () => {
    it('decrypts using a pre-fetched key', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
      } as Response);

      // Get the key and use it directly
      const key = await e2eeService.getChannelKey('ch-direct');
      const encrypted = await encryptMessage('Direct decrypt', channelKey);
      const decrypted = await e2eeService.decryptWithKey(encrypted, key);
      expect(decrypted).toBe('Direct decrypt');
    });
  });

  describe('getChannelKeyByVersion', () => {
    it('fetches specific version from server', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 3 } }),
      } as Response);

      const key = await e2eeService.getChannelKeyByVersion('ch-v', 3);
      expect(key).toBeDefined();
      expect(key.algorithm.name).toBe('AES-GCM');

      // Verify the correct URL was called with version param
      expect(mockApiFetch).toHaveBeenCalledWith(expect.stringContaining('version=3'));
    });

    it('uses main cache when version matches current', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      // First fetch the current key with version 2
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 2 } }),
      } as Response);

      await e2eeService.getChannelKey('ch-ver-match');
      mockApiFetch.mockClear();

      // Now request version 2 — should use main cache
      const key = await e2eeService.getChannelKeyByVersion('ch-ver-match', 2);
      expect(key).toBeDefined();
      expect(mockApiFetch).not.toHaveBeenCalled(); // Used cache
    });

    it('caches versioned keys', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 4 } }),
      } as Response);

      await e2eeService.getChannelKeyByVersion('ch-ver-cache', 4);
      mockApiFetch.mockClear();

      // Second call should use versioned cache
      await e2eeService.getChannelKeyByVersion('ch-ver-cache', 4);
      expect(mockApiFetch).not.toHaveBeenCalled();
    });

    it('throws on rate limit (429)', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '10' }),
        json: () => Promise.resolve({}),
      } as unknown as Response);

      await expect(e2eeService.getChannelKeyByVersion('ch-rate', 5)).rejects.toBeInstanceOf(
        E2EEKeyUnavailableError
      );
    });
  });

  describe('decryptForChannelWithVersion', () => {
    it('falls back to current key for version 0', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
      } as Response);

      const encrypted = await encryptMessage('Test v0', channelKey);
      const decrypted = await e2eeService.decryptForChannelWithVersion('ch-v0', encrypted, 0);
      expect(decrypted).toBe('Test v0');
    });

    it('falls back to current key for version 1', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
      } as Response);

      const encrypted = await encryptMessage('Test v1', channelKey);
      const decrypted = await e2eeService.decryptForChannelWithVersion('ch-v1', encrypted, 1);
      expect(decrypted).toBe('Test v1');
    });

    it('uses current key when requested version matches cached', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 3 } }),
      } as Response);

      // Prime cache
      await e2eeService.getChannelKey('ch-match');

      const encrypted = await encryptMessage('Test match', channelKey);
      const decrypted = await e2eeService.decryptForChannelWithVersion('ch-match', encrypted, 3);
      expect(decrypted).toBe('Test match');
    });
  });

  describe('createChannelKeys', () => {
    it('generates and wraps keys for multiple members', async () => {
      const regKeys2 = await generateRegistrationKeys('Password2!');
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const pubKey2Base64 = await exportPublicKey(regKeys2.publicKey);

      const memberKeys = new Map<string, string>([
        ['user-1', pubKeyBase64],
        ['user-2', pubKey2Base64],
      ]);

      const wrappedKeys = await e2eeService.createChannelKeys(memberKeys);
      expect(wrappedKeys.size).toBe(2);
      expect(wrappedKeys.has('user-1')).toBe(true);
      expect(wrappedKeys.has('user-2')).toBe(true);
    });
  });

  describe('wrapKeyForMember', () => {
    it('wraps an existing channel key for a new member', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
      } as Response);

      const newMemberKeys = await generateRegistrationKeys('NewMember!');
      const newPubKeyBase64 = await exportPublicKey(newMemberKeys.publicKey);

      const wrappedForNew = await e2eeService.wrapKeyForMember('ch-wrap', newPubKeyBase64);
      expect(typeof wrappedForNew).toBe('string');
      expect(wrappedForNew.length).toBeGreaterThan(0);
    });
  });

  describe('processPendingKeyRequests', () => {
    it('does nothing when not initialized', async () => {
      await e2eeService.processPendingKeyRequests();
      expect(mockApiFetch).not.toHaveBeenCalled();
    });

    it('fetches and processes pending requests', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);

      mockApiFetch
        // pending-keys endpoint
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
            }),
        } as Response)
        // public key for the new member
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64 }),
        } as Response)
        // getChannelKey (for wrapKeyForMember)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
        } as Response)
        // upload wrapped key
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({}),
        } as Response);

      await e2eeService.processPendingKeyRequests();

      // Should have made 4 API calls
      expect(mockApiFetch.mock.calls.length).toBe(4);
    });

    it('handles empty pending requests', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ pending_requests: [] }),
      } as Response);

      // Should complete without errors
      await e2eeService.processPendingKeyRequests();
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('handles pending-keys fetch failure', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        statusText: 'Internal Server Error',
        json: () => Promise.resolve({}),
      } as Response);

      // Should not throw
      await e2eeService.processPendingKeyRequests();

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });
  });

  describe('rotateChannelKey', () => {
    it('generates new key, wraps for all members, and uploads', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const memberKeys = new Map([['user-1', pubKeyBase64]]);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({}),
      } as Response);

      await e2eeService.rotateChannelKey('ch-rotate', 3, memberKeys);

      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/e2ee/keys/ch-rotate'),
        expect.objectContaining({ method: 'POST' })
      );
    });

    it('handles upload failure gracefully', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const memberKeys = new Map([['user-1', pubKeyBase64]]);

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 409,
        json: () => Promise.resolve({ error: 'conflict' }),
      } as Response);

      // Should not throw
      await e2eeService.rotateChannelKey('ch-fail', 3, memberKeys);

      expect(mockApiFetch).toHaveBeenCalled();
    });

    it('invalidates channel cache after rotation', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      // Prime the cache
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
      } as Response);

      await e2eeService.getChannelKey('ch-inv');

      // Now rotate
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({}),
      } as Response);

      await e2eeService.rotateChannelKey('ch-inv', 2, new Map([['user-1', pubKeyBase64]]));

      // Cache should be invalidated — next getChannelKey should fetch
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 2 } }),
      } as Response);

      await e2eeService.getChannelKey('ch-inv');
      // Should have fetched again
      expect(mockApiFetch.mock.calls.length).toBe(3);
    });
  });

  describe('rate limiting', () => {
    it('blocks requests when rate limited', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      // Trigger a 429
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '30' }),
        json: () => Promise.resolve({}),
      } as unknown as Response);

      await expect(e2eeService.getChannelKey('ch-rate')).rejects.toThrow('E2EE key unavailable');

      // Subsequent request should also fail (rate limited)
      await expect(e2eeService.getChannelKey('ch-rate-2')).rejects.toThrow('E2EE key unavailable');
    });
  });

  describe('concurrent fetch deduplication', () => {
    it('deduplicates concurrent getChannelKey calls', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValue({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
      } as Response);

      // Fire two concurrent requests for the same channel
      const [key1, key2] = await Promise.all([
        e2eeService.getChannelKey('ch-dedup'),
        e2eeService.getChannelKey('ch-dedup'),
      ]);

      // Both should succeed
      expect(key1).toBeDefined();
      expect(key2).toBeDefined();

      // Only one API call should have been made
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });
  });

  describe('clearKeys', () => {
    it('clears all caches and session keys', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      expect(e2eeService.isInitialized).toBe(true);
      expect(e2eeService.getSessionKeys()).not.toBeNull();

      e2eeService.clearKeys();

      expect(e2eeService.isInitialized).toBe(false);
      expect(e2eeService.getSessionKeys()).toBeNull();
      expect(e2eeService.getWrappingKey()).toBeNull();
      expect(e2eeService.getWrappedPrivateKey()).toBe('');
      expect(e2eeService.getPreferencesKeyBase64()).toBeNull();
    });
  });

  describe('requestRewrap (#1023)', () => {
    it('POSTs to the rewrap endpoint with no body and resolves on 202', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 202,
        headers: new Headers(),
        json: () => Promise.resolve({ enrolled: true, kind: 'dm' }),
      } as unknown as Response);

      await expect(
        e2eeService.requestRewrap('11111111-1111-1111-1111-111111111111')
      ).resolves.toBeUndefined();

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      const [url, init] = mockApiFetch.mock.calls[0];
      expect(url).toMatch(/\/api\/v1\/e2ee\/keys\/11111111-1111-1111-1111-111111111111\/rewrap$/);
      expect(init?.method).toBe('POST');
      expect(init?.body).toBeUndefined();
    });

    it('updates rateLimitedUntil on 429 and resolves silently', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '30' }),
        json: () => Promise.resolve({}),
      } as unknown as Response);

      await expect(
        e2eeService.requestRewrap('22222222-2222-2222-2222-222222222222')
      ).resolves.toBeUndefined();

      // Observable side-effect: the next call is a no-op (does not POST).
      mockApiFetch.mockClear();
      await e2eeService.requestRewrap('22222222-2222-2222-2222-222222222222');
      expect(mockApiFetch).not.toHaveBeenCalled();
    });

    it('throws on other non-2xx responses', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 403,
        headers: new Headers(),
        json: () => Promise.resolve({}),
      } as unknown as Response);

      await expect(
        e2eeService.requestRewrap('33333333-3333-3333-3333-333333333333')
      ).rejects.toThrow(/requestRewrap failed: 403/);
    });

    it('no-ops when rateLimitedUntil is in the future', async () => {
      // Prime the rate-limit by triggering a 429 first
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '60' }),
        json: () => Promise.resolve({}),
      } as unknown as Response);
      await e2eeService.requestRewrap('44444444-4444-4444-4444-444444444444');

      // Second call should not hit the network
      mockApiFetch.mockClear();
      await e2eeService.requestRewrap('44444444-4444-4444-4444-444444444444');
      expect(mockApiFetch).not.toHaveBeenCalled();
    });
  });

  describe('fetchAndUnwrapChannelKey fire-and-forget rewrap (#1023)', () => {
    it('fires requestRewrap on NO_KEY_YET pending:true and still throws', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      // First fetch: GET /e2ee/keys/X returns 404+pending:true
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        headers: new Headers(),
        json: () =>
          Promise.resolve({ error: 'no key', code: 'NO_KEY_YET', kind: 'DM', pending: true }),
      } as unknown as Response);
      // Second fetch: POST /rewrap returns 202
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        status: 202,
        headers: new Headers(),
        json: () => Promise.resolve({ enrolled: true, kind: 'dm' }),
      } as unknown as Response);

      const channelId = '55555555-5555-5555-5555-555555555555';
      await expect(e2eeService.getChannelKey(channelId)).rejects.toMatchObject({
        name: 'E2EEKeyUnavailableError',
        code: 'NO_KEY_YET',
        pending: true,
      });

      // The throw fires synchronously; the fire-and-forget POST happens before/around
      // the throw. Allow a microtask to flush so the POST registers.
      await new Promise((r) => setTimeout(r, 0));

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      expect(mockApiFetch.mock.calls[1][0]).toMatch(/\/rewrap$/);
    });

    it('does not throw even if requestRewrap rejects', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      // First fetch: GET /e2ee/keys/X returns 404+pending:true
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        headers: new Headers(),
        json: () =>
          Promise.resolve({
            error: 'no key',
            code: 'NO_KEY_YET',
            kind: 'channel',
            pending: true,
          }),
      } as unknown as Response);
      // rewrap POST fails with 500
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 500,
        headers: new Headers(),
        json: () => Promise.resolve({}),
      } as unknown as Response);

      const channelId = '66666666-6666-6666-6666-666666666666';
      await expect(e2eeService.getChannelKey(channelId)).rejects.toMatchObject({
        name: 'E2EEKeyUnavailableError',
      });

      // Allow microtask for the fire-and-forget promise to settle
      await new Promise((r) => setTimeout(r, 0));

      // No unhandled rejection — the .catch() in the fire-and-forget pattern
      // swallows the requestRewrap error. The original key-fetch error still surfaces.
    });
  });
});
