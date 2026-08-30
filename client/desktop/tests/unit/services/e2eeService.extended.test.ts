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
  exportChannelKey,
  exportPublicKey,
  arrayBufferToBase64,
} from '@/renderer/utils/crypto';

import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';

// Mock apiFetch for channel key fetching
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
import { deferred } from '../../helpers/deferred';
const mockApiFetch = vi.mocked(apiFetch);

describe('e2eeService — extended', () => {
  const testPassword = 'TestPassword123!';
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;

  type PromiseOutcome<T> =
    { status: 'fulfilled'; value: T } | { status: 'rejected'; reason: unknown };

  function observePromise<T>(promise: Promise<T>): Promise<PromiseOutcome<T>> {
    return promise.then(
      (value) => ({ status: 'fulfilled' as const, value }),
      (reason: unknown) => ({ status: 'rejected' as const, reason })
    );
  }

  function keyResponse(wrappedKey: string, keyVersion: number): Response {
    return {
      ok: true,
      json: () => Promise.resolve({ key: { wrapped_key: wrappedKey, key_version: keyVersion } }),
    } as Response;
  }

  function channelGuard(channelId: string) {
    return e2eeService.createChannelOperationGuard(channelId);
  }

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

  describe('encryptForChannelWithVersion', () => {
    it('rejects when the selected epoch is invalidated during key unwrap', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'channel-atomic-edit';
      const epochTwoKey = await generateChannelKey();
      const epochThreeKey = await generateChannelKey();
      const wrappedEpochTwo = await wrapChannelKey(epochTwoKey, regKeys.publicKey);
      const wrappedEpochThree = await wrapChannelKey(epochThreeKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedEpochTwo, key_version: 2 } }),
      } as Response);
      await e2eeService.getChannelKey(channelId);

      type ServiceInternals = {
        channelKeyCache: Map<
          string,
          {
            wrappedKey: string;
            keyVersion: number;
            lastUsed: number;
            refetchAfterMalformed: number;
          }
        >;
        derivePrivateKey: () => Promise<CryptoKey>;
      };
      const internals = e2eeService as unknown as ServiceInternals;
      const originalDerivePrivateKey = internals.derivePrivateKey.bind(e2eeService);
      const privateKey = await originalDerivePrivateKey();

      let releaseDerivation: ((key: CryptoKey) => void) | undefined;
      const blockedDerivation = new Promise<CryptoKey>((resolve) => {
        releaseDerivation = resolve;
      });
      internals.derivePrivateKey = () => blockedDerivation;

      let outcome:
        | PromiseOutcome<Awaited<ReturnType<typeof e2eeService.encryptForChannelWithVersion>>>
        | undefined;
      try {
        const encryptionOutcome = observePromise(
          e2eeService.encryptForChannelWithVersion(channelId, 'edited text')
        );

        // getChannelKey has selected epoch 2 and is now awaiting derivePrivateKey.
        // Simulate a rotation invalidating/replacing the mutable main-cache slot.
        e2eeService.invalidateChannelKey(channelId);
        internals.channelKeyCache.set(channelId, {
          wrappedKey: wrappedEpochThree,
          keyVersion: 3,
          lastUsed: Date.now(),
          refetchAfterMalformed: 0,
        });

        if (!releaseDerivation) throw new Error('derivePrivateKey was not deferred');
        releaseDerivation(privateKey);
        outcome = await encryptionOutcome;
      } finally {
        internals.derivePrivateKey = originalDerivePrivateKey;
      }

      expect(outcome?.status).toBe('rejected');
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
      const decrypted = await e2eeService.decryptWithKey(encrypted, key, channelGuard('ch-direct'));
      expect(decrypted).toBe('Direct decrypt');
    });

    it('rejects a retained pre-fetched key after its channel is invalidated', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-retained-key';
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedForUser, 2));
      const retainedKey = await e2eeService.getChannelKey(channelId);
      const ciphertext = await encryptMessage('must stay fenced', channelKey);
      const guard = channelGuard(channelId);

      e2eeService.invalidateChannelKey(channelId);

      await expect(e2eeService.decryptWithKey(ciphertext, retainedKey, guard)).rejects.toThrow(
        'E2EE key unavailable: NO_KEY_YET'
      );
    });

    it('distinguishes terminal access loss from retryable rotation generations', () => {
      const channelId = 'ch-access-generation';
      const beforeRevocation = channelGuard(channelId);

      e2eeService.revokeChannelAccess(channelId);

      expect(() => beforeRevocation.assertCurrent()).toThrow('E2EE key unavailable: NOT_MEMBER');

      // A newly captured context can be used after access is re-established.
      const afterRegain = channelGuard(channelId);
      expect(() => afterRegain.assertCurrent()).not.toThrow();

      // An ordinary rotation remains retryable for that live context.
      e2eeService.invalidateChannelKey(channelId);
      try {
        afterRegain.assertCurrent();
        throw new Error('expected the rotated generation to be fenced');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('NO_KEY_YET');
        expect((err as E2EEKeyUnavailableError).pending).toBe(true);
      }
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

    it('coalesces concurrent pending-key signals into one rerun', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const firstPending = deferred<Response>();
      mockApiFetch
        .mockImplementationOnce(() => firstPending.promise)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ pending_requests: [] }),
        } as Response);

      const first = e2eeService.processPendingKeyRequests();
      const second = e2eeService.processPendingKeyRequests();
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      firstPending.resolve({
        ok: true,
        json: () => Promise.resolve({ pending_requests: [] }),
      } as Response);
      await Promise.all([first, second]);

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });

    it('does not join a pending-key drain from a cleared session', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const stalePending = deferred<Response>();
      mockApiFetch
        .mockImplementationOnce(() => stalePending.promise)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ pending_requests: [] }),
        } as Response);

      const staleDrain = e2eeService.processPendingKeyRequests();
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      e2eeService.clearKeys();
      const nextAccountKeys = await generateRegistrationKeys('NextAccountPassword!');
      await e2eeService.initialize(
        'NextAccountPassword!',
        nextAccountKeys.wrappedPrivateKey,
        nextAccountKeys.keyDerivationSalt
      );
      await e2eeService.processPendingKeyRequests();
      expect(mockApiFetch).toHaveBeenCalledTimes(2);

      stalePending.resolve({
        ok: true,
        json: () => Promise.resolve({ pending_requests: [] }),
      } as Response);
      await staleDrain;
    });

    it('fetches and processes pending requests', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKeyV1 = await generateChannelKey();
      const channelKeyV2 = await generateChannelKey();
      const wrappedForV1 = await wrapChannelKey(channelKeyV1, regKeys.publicKey);
      const wrappedForV2 = await wrapChannelKey(channelKeyV2, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);

      mockApiFetch
        // Prime the stale current-key cache. The pending request below must not
        // reuse this v1 key when its durable marker requests v2.
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { wrapped_key: wrappedForV1, key_version: 1 } }),
        } as Response)
        // pending-keys endpoint
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending', key_version: 2 }],
            }),
        } as Response)
        // public key for the new member (carries the recipient's key_version)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64, key_version: 7 }),
        } as Response)
        // exact v2 key for the pending-marker recovery
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { wrapped_key: wrappedForV2, key_version: 2 } }),
        } as Response)
        // upload wrapped key
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({}),
        } as Response);

      await e2eeService.getChannelKey('ch-pending');
      await e2eeService.processPendingKeyRequests();

      // The v1 cache priming plus pending/public-key/v2/upload calls.
      expect(mockApiFetch.mock.calls.length).toBe(5);
      expect(mockApiFetch.mock.calls[3][0]).toBe('/api/v1/e2ee/keys/ch-pending?version=2');

      // #2420: the distribution POST echoes the recipient's public-key version the
      // CSK was wrapped against, activating the server-side recipient-freshness guard.
      const uploadCall = mockApiFetch.mock.calls[4];
      expect(uploadCall[0]).toBe('/api/v1/e2ee/keys/ch-pending');
      const uploadBody = JSON.parse((uploadCall[1] as RequestInit).body as string);
      expect(Object.keys(uploadBody.wrapped_keys)).toEqual(['user-new']);
      expect(typeof uploadBody.wrapped_keys['user-new']).toBe('string');
      expect(uploadBody.key_fingerprint).toBe(
        arrayBufferToBase64(
          await crypto.subtle.digest('SHA-256', await exportChannelKey(channelKeyV2))
        )
      );
      expect(uploadBody.wrapped_key_versions).toEqual({ 'user-new': 7 });
      expect(uploadBody.key_version).toBe(2);
    });

    it('omits wrapped_key_versions when the server returns no key_version (#2420 fail-open)', async () => {
      // An old control-plane's GetPublicKey response has no `key_version`. The
      // echo must degrade to the legacy insert: wrapped_keys is still sent, but
      // wrapped_key_versions is omitted so the server does not activate the
      // recipient-freshness guard against a version it never supplied.
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);

      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
            }),
        } as Response)
        // public key WITHOUT key_version (legacy server)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64 }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({}),
        } as Response);

      await e2eeService.processPendingKeyRequests();

      const uploadCall = mockApiFetch.mock.calls[3];
      expect(uploadCall[0]).toBe('/api/v1/e2ee/keys/ch-pending');
      const uploadBody = JSON.parse((uploadCall[1] as RequestInit).body as string);
      expect(Object.keys(uploadBody.wrapped_keys)).toEqual(['user-new']);
      expect(uploadBody.key_fingerprint).toMatch(/^[A-Za-z0-9+/]{43}=$/);
      expect(uploadBody.wrapped_key_versions).toBeUndefined();
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

    it('retries a transient pending-keys fetch failure', async () => {
      vi.useFakeTimers();
      try {
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
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch).toHaveBeenCalledTimes(2);
        expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries a malformed pending-keys response', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            headers: new Headers({ 'Content-Type': 'application/json' }),
            json: () => Promise.reject(new Error('invalid JSON')),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries a rejected pending-keys fetch', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockRejectedValueOnce(new TypeError('network unavailable'))
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('schedules a fresh retry after a same-session fence', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: false,
            status: 500,
            statusText: 'Internal Server Error',
          } as Response)
          .mockResolvedValueOnce({
            ok: false,
            status: 500,
            statusText: 'Internal Server Error',
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        e2eeService.fencePendingOperations();
        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch).toHaveBeenCalledTimes(3);
      } finally {
        vi.useRealTimers();
      }
    });

    it('continues after a non-retryable pending recipient failure', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [
                { user_id: 'user-invalid', channel_id: 'ch-pending' },
                { user_id: 'user-valid', channel_id: 'ch-pending' },
              ],
            }),
        } as Response)
        .mockResolvedValueOnce({ ok: false, status: 404 } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64, key_version: 1 }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
        } as Response)
        .mockResolvedValueOnce({ ok: true, json: () => Promise.resolve({}) } as Response);

      await e2eeService.processPendingKeyRequests();

      expect(mockApiFetch.mock.calls[4][0]).toBe('/api/v1/e2ee/keys/ch-pending');
    });

    it('retries after a pending recipient public-key network failure', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
              }),
          } as Response)
          .mockRejectedValueOnce(new Error('network unavailable'))
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[2][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries after a retryable pending recipient public-key response', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
              }),
          } as Response)
          .mockResolvedValueOnce({ ok: false, status: 500 } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[2][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries after a malformed pending recipient public-key response', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.reject(new Error('invalid JSON')),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[2][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('continues after a recipient public key cannot be imported', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [
                { user_id: 'user-invalid', channel_id: 'ch-pending' },
                { user_id: 'user-valid', channel_id: 'ch-pending' },
              ],
            }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: 'not-an-rsa-spki' }),
        } as Response)
        .mockResolvedValueOnce(keyResponse(wrappedForUser, 1))
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64, key_version: 1 }),
        } as Response)
        .mockResolvedValueOnce({ ok: true } as Response);

      await e2eeService.processPendingKeyRequests();

      expect(mockApiFetch.mock.calls[4][0]).toBe('/api/v1/e2ee/keys/ch-pending');
    });

    it('retries when the requested pending-key epoch is temporarily unavailable', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [
                  { user_id: 'user-new', channel_id: 'ch-pending', key_version: 2 },
                ],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ public_key: pubKeyBase64 }),
          } as Response)
          .mockResolvedValueOnce({
            ok: false,
            status: 500,
            headers: new Headers(),
            json: () => Promise.resolve({ code: 'INTERNAL_ERROR' }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[3][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries after an exact pending-key fetch network failure', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [
                  { user_id: 'user-new', channel_id: 'ch-pending', key_version: 2 },
                ],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ public_key: pubKeyBase64 }),
          } as Response)
          .mockRejectedValueOnce(new TypeError('network unavailable'))
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[3][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries after a rate-limited exact pending-key fetch', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [
                  { user_id: 'user-new', channel_id: 'ch-pending', key_version: 2 },
                ],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ public_key: pubKeyBase64 }),
          } as Response)
          .mockResolvedValueOnce({
            ok: false,
            status: 429,
            headers: new Headers(),
            json: () => Promise.resolve({}),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[3][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries pending distribution after a retryable upload failure', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        const channelKey = await generateChannelKey();
        const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
        const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ public_key: pubKeyBase64, key_version: 1 }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser, key_version: 1 } }),
          } as Response)
          .mockResolvedValueOnce({ ok: false, status: 429 } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[4][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries pending distribution after an upload network failure', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        const channelKey = await generateChannelKey();
        const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
        const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () =>
              Promise.resolve({
                pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
              }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ public_key: pubKeyBase64 }),
          } as Response)
          .mockResolvedValueOnce(keyResponse(wrappedForUser, 1))
          .mockRejectedValueOnce(new Error('network unavailable'))
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[4][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('retries after an unexpected pending-requests payload shape', async () => {
      vi.useFakeTimers();
      try {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        mockApiFetch
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: {} }),
          } as Response)
          .mockResolvedValueOnce({
            ok: true,
            json: () => Promise.resolve({ pending_requests: [] }),
          } as Response);

        await e2eeService.processPendingKeyRequests();
        await vi.advanceTimersByTimeAsync(60_000);

        expect(mockApiFetch.mock.calls[1][0]).toBe('/api/v1/e2ee/pending-keys');
      } finally {
        vi.useRealTimers();
      }
    });

    it('does not upload a wrapped key after a session fence', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      mockApiFetch
        .mockResolvedValueOnce({
          ok: true,
          json: () =>
            Promise.resolve({
              pending_requests: [{ user_id: 'user-new', channel_id: 'ch-pending' }],
            }),
        } as Response)
        .mockResolvedValueOnce({
          ok: true,
          json: () => Promise.resolve({ public_key: pubKeyBase64 }),
        } as Response)
        .mockResolvedValueOnce(keyResponse(wrappedForUser, 1));

      const originalEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
      const wrapStarted = deferred<void>();
      const blockedWrap = deferred<ArrayBuffer>();
      let blockWrap = true;
      const encryptSpy = vi
        .spyOn(crypto.subtle, 'encrypt')
        .mockImplementation(async (algorithm, key, data) => {
          if (algorithm.name === 'RSA-OAEP' && blockWrap) {
            blockWrap = false;
            wrapStarted.resolve(undefined);
            return blockedWrap.promise;
          }
          return originalEncrypt(algorithm, key, data);
        });

      try {
        const processing = e2eeService.processPendingKeyRequests();
        await wrapStarted.promise;
        e2eeService.fencePendingOperations();
        blockedWrap.resolve(new Uint8Array(512).buffer);
        await processing;
      } finally {
        encryptSpy.mockRestore();
      }

      expect(mockApiFetch).toHaveBeenCalledTimes(3);
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

    it('fails rotation when distribution upload fails', async () => {
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

      await expect(e2eeService.rotateChannelKey('ch-fail', 3, memberKeys)).rejects.toThrow(
        'channel key rotation distribution failed'
      );

      expect(mockApiFetch).toHaveBeenCalled();
    });

    it('sends recipient key versions with a rotation batch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce({ ok: true, json: () => Promise.resolve({}) } as Response);

      await e2eeService.rotateChannelKey(
        'ch-versioned-rotate',
        3,
        new Map([['user-1', pubKeyBase64]]),
        { 'user-1': 7 }
      );

      const body = JSON.parse((mockApiFetch.mock.calls[0][1] as RequestInit).body as string);
      expect(body.wrapped_key_versions).toEqual({ 'user-1': 7 });
    });

    it('does not complete a rotation after its operation guard is fenced', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const uploadStarted = deferred<void>();
      const upload = deferred<Response>();
      mockApiFetch.mockImplementationOnce(() => {
        uploadStarted.resolve(undefined);
        return upload.promise;
      });
      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const guard = e2eeService.createChannelRotationGuard('ch-fenced-rotate');
      const rotation = e2eeService.rotateChannelKey(
        'ch-fenced-rotate',
        3,
        new Map([['user-1', pubKeyBase64]]),
        undefined,
        guard
      );

      await uploadStarted.promise;
      e2eeService.fencePendingOperations();
      upload.resolve({ ok: true, json: () => Promise.resolve({}) } as Response);

      await expect(rotation).rejects.toThrow('E2EE key session changed');
    });

    it('fences a rotation after channel access is revoked', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const guard = e2eeService.createChannelRotationGuard('ch-revoked-rotate');
      e2eeService.revokeChannelAccess('ch-revoked-rotate');

      expect(() => guard.assertCurrent()).toThrow('E2EE key unavailable: NOT_MEMBER');
    });

    it('splits rotation uploads at the server batch limit', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const memberKeys = new Map<string, string>(
        Array.from({ length: 501 }, (_, index): [string, string] => [`user-${index}`, pubKeyBase64])
      );
      let uploadCount = 0;
      mockApiFetch.mockImplementation(() => {
        uploadCount += 1;
        if (uploadCount === 1) e2eeService.invalidateChannelKey('ch-batched');
        return Promise.resolve({ ok: true, json: () => Promise.resolve({}) } as Response);
      });

      await e2eeService.rotateChannelKey(
        'ch-batched',
        3,
        memberKeys,
        undefined,
        e2eeService.createChannelRotationGuard('ch-batched')
      );

      expect(mockApiFetch).toHaveBeenCalledTimes(2);
      const first = JSON.parse((mockApiFetch.mock.calls[0][1] as RequestInit).body as string);
      const second = JSON.parse((mockApiFetch.mock.calls[1][1] as RequestInit).body as string);
      expect(Object.keys(first.wrapped_keys)).toHaveLength(500);
      expect(Object.keys(second.wrapped_keys)).toHaveLength(1);
      expect(first.key_fingerprint).toMatch(/^[A-Za-z0-9+/]{43}=$/);
      expect(second.key_fingerprint).toBe(first.key_fingerprint);
    });

    it('skips malformed member keys before uploading any batch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
      const memberKeys = new Map<string, string>(
        Array.from({ length: 501 }, (_, index): [string, string] => [
          `user-${index}`,
          index === 0 ? 'not-an-rsa-spki' : pubKeyBase64,
        ])
      );
      mockApiFetch.mockResolvedValue({ ok: true, json: () => Promise.resolve({}) } as Response);

      await e2eeService.rotateChannelKey('ch-batch-fail', 3, memberKeys);

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      const wrappedKeys = JSON.parse(
        (mockApiFetch.mock.calls[0][1] as RequestInit).body as string
      ).wrapped_keys;
      expect(Object.keys(wrappedKeys)).toHaveLength(500);
      expect(wrappedKeys['user-0']).toBeUndefined();
    });

    it('rejects rotation when every member public key is malformed', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      await expect(
        e2eeService.rotateChannelKey(
          'ch-no-valid-recipients',
          3,
          new Map([['user-invalid', 'not-an-rsa-spki']])
        )
      ).rejects.toThrow('channel key rotation has no valid recipients');
      expect(mockApiFetch).not.toHaveBeenCalled();
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

  describe('async key lifecycle boundaries', () => {
    it.each([
      [
        'current-version encryption',
        (channelId: string) => e2eeService.encryptForChannel(channelId, 'revoked plaintext'),
      ],
      [
        'version-bound encryption',
        (channelId: string) =>
          e2eeService.encryptForChannelWithVersion(channelId, 'revoked plaintext'),
      ],
    ])(
      'rejects %s that crosses channel invalidation after key acquisition',
      async (_name, encrypt) => {
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );

        const channelId = 'ch-encrypt-rotation-boundary';
        const channelKey = await generateChannelKey();
        const wrappedKey = await wrapChannelKey(channelKey, regKeys.publicKey);
        mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedKey, 4));
        await e2eeService.getChannelKey(channelId);

        const originalEncrypt = crypto.subtle.encrypt.bind(crypto.subtle);
        const encryptStarted = deferred<void>();
        const blockedCiphertext = deferred<ArrayBuffer>();
        const encryptSpy = vi
          .spyOn(crypto.subtle, 'encrypt')
          .mockImplementation(async (algorithm, key, data) => {
            if (algorithm.name === 'AES-GCM') {
              encryptStarted.resolve(undefined);
              return blockedCiphertext.promise;
            }
            return originalEncrypt(algorithm, key, data);
          });

        let outcome: PromiseOutcome<unknown> | undefined;
        try {
          const outcomePromise = observePromise(encrypt(channelId));
          await encryptStarted.promise;
          e2eeService.invalidateChannelKey(channelId);
          blockedCiphertext.resolve(new Uint8Array(32).buffer);
          outcome = await outcomePromise;
        } finally {
          encryptSpy.mockRestore();
        }

        expect(outcome?.status).toBe('rejected');
        if (outcome?.status === 'rejected') {
          expect(outcome.reason).toBeInstanceOf(E2EEKeyUnavailableError);
        }
      }
    );

    it('rejects a current-key fetch invalidated by rotation without joining or overwriting the replacement fetch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-current-rotation-boundary';
      const staleKey = await generateChannelKey();
      const freshKey = await generateChannelKey();
      const staleWrappedKey = await wrapChannelKey(staleKey, regKeys.publicKey);
      const freshWrappedKey = await wrapChannelKey(freshKey, regKeys.publicKey);
      const staleResponse = deferred<Response>();
      const freshResponse = deferred<Response>();

      mockApiFetch
        .mockImplementationOnce(() => staleResponse.promise)
        .mockImplementationOnce(() => freshResponse.promise);

      const staleOutcomePromise = observePromise(e2eeService.getChannelKey(channelId));

      e2eeService.invalidateChannelKey(channelId);
      const freshFetch = e2eeService.getChannelKey(channelId);

      staleResponse.resolve(keyResponse(staleWrappedKey, 2));
      const staleOutcome = await staleOutcomePromise;

      const joinedFreshFetch = e2eeService.getChannelKey(channelId);
      const apiFetchCallCount = mockApiFetch.mock.calls.length;

      freshResponse.resolve(keyResponse(freshWrappedKey, 3));
      const [freshResult, joinedFreshResult] = await Promise.all([freshFetch, joinedFreshFetch]);
      // A failing implementation may leave the second one-shot response unused.
      // Reset it before assertions so this regression cannot poison later tests.
      mockApiFetch.mockReset();
      const freshCiphertext = await encryptMessage('fresh current key', freshKey);

      expect.soft(apiFetchCallCount).toBe(2);
      expect.soft(staleOutcome.status).toBe('rejected');
      await expect(
        e2eeService.decryptWithKey(freshCiphertext, freshResult, channelGuard(channelId))
      ).resolves.toBe('fresh current key');
      await expect(
        e2eeService.decryptWithKey(freshCiphertext, joinedFreshResult, channelGuard(channelId))
      ).resolves.toBe('fresh current key');
      expect(e2eeService.getCurrentKeyVersion(channelId)).toBe(3);
    });

    it('rejects a versioned-key fetch invalidated by rotation without joining or overwriting the replacement fetch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-versioned-rotation-boundary';
      const version = 7;
      const staleKey = await generateChannelKey();
      const freshKey = await generateChannelKey();
      const staleWrappedKey = await wrapChannelKey(staleKey, regKeys.publicKey);
      const freshWrappedKey = await wrapChannelKey(freshKey, regKeys.publicKey);
      const staleResponse = deferred<Response>();
      const freshResponse = deferred<Response>();

      mockApiFetch
        .mockImplementationOnce(() => staleResponse.promise)
        .mockImplementationOnce(() => freshResponse.promise);

      const staleOutcomePromise = observePromise(
        e2eeService.getChannelKeyByVersion(channelId, version)
      );

      e2eeService.invalidateChannelKey(channelId);
      const freshFetch = e2eeService.getChannelKeyByVersion(channelId, version);

      staleResponse.resolve(keyResponse(staleWrappedKey, version));
      const staleOutcome = await staleOutcomePromise;

      const joinedFreshFetch = e2eeService.getChannelKeyByVersion(channelId, version);
      const apiFetchCallCount = mockApiFetch.mock.calls.length;

      freshResponse.resolve(keyResponse(freshWrappedKey, version));
      const [freshResult, joinedFreshResult] = await Promise.all([freshFetch, joinedFreshFetch]);
      // A failing implementation may leave the second one-shot response unused.
      // Reset it before assertions so this regression cannot poison later tests.
      mockApiFetch.mockReset();
      const freshCiphertext = await encryptMessage('fresh versioned key', freshKey);

      expect.soft(apiFetchCallCount).toBe(2);
      expect.soft(staleOutcome.status).toBe('rejected');
      await expect(
        e2eeService.decryptWithKey(freshCiphertext, freshResult, channelGuard(channelId))
      ).resolves.toBe('fresh versioned key');
      await expect(
        e2eeService.decryptWithKey(freshCiphertext, joinedFreshResult, channelGuard(channelId))
      ).resolves.toBe('fresh versioned key');
    });

    it('rejects a current-key cache-hit unwrap that crosses channel invalidation', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-cache-hit-rotation-boundary';
      const channelKey = await generateChannelKey();
      const wrappedKey = await wrapChannelKey(channelKey, regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedKey, 4));
      await e2eeService.getChannelKey(channelId);

      type ServiceInternals = {
        derivePrivateKey: () => Promise<CryptoKey>;
      };
      const internals = e2eeService as unknown as ServiceInternals;
      const originalDerivePrivateKey = internals.derivePrivateKey.bind(e2eeService);
      const privateKey = await originalDerivePrivateKey();
      const blockedDerivation = deferred<CryptoKey>();
      internals.derivePrivateKey = () => blockedDerivation.promise;

      let outcome: PromiseOutcome<CryptoKey> | undefined;
      try {
        const outcomePromise = observePromise(e2eeService.getChannelKey(channelId));
        e2eeService.invalidateChannelKey(channelId);
        blockedDerivation.resolve(privateKey);
        outcome = await outcomePromise;
      } finally {
        internals.derivePrivateKey = originalDerivePrivateKey;
      }

      expect(outcome?.status).toBe('rejected');
      expect(e2eeService.getCurrentKeyVersion(channelId)).toBe(1);
    });

    it('rejects a decrypt that crosses channel invalidation after key acquisition', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-decrypt-rotation-boundary';
      const channelKey = await generateChannelKey();
      const wrappedKey = await wrapChannelKey(channelKey, regKeys.publicKey);
      const ciphertext = await encryptMessage('revoked plaintext', channelKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedKey, 4));
      await e2eeService.getChannelKey(channelId);

      const originalDecrypt = crypto.subtle.decrypt.bind(crypto.subtle);
      const decryptStarted = deferred<void>();
      const blockedPlaintext = deferred<ArrayBuffer>();
      const decryptSpy = vi
        .spyOn(crypto.subtle, 'decrypt')
        .mockImplementation(async (algorithm, key, data) => {
          if (algorithm.name === 'AES-GCM') {
            decryptStarted.resolve(undefined);
            return blockedPlaintext.promise;
          }
          return originalDecrypt(algorithm, key, data);
        });

      let outcome: PromiseOutcome<string> | undefined;
      try {
        const outcomePromise = observePromise(e2eeService.decryptForChannel(channelId, ciphertext));
        await decryptStarted.promise;
        e2eeService.invalidateChannelKey(channelId);
        blockedPlaintext.resolve(new TextEncoder().encode('revoked plaintext').buffer);
        outcome = await outcomePromise;
      } finally {
        decryptSpy.mockRestore();
      }

      expect(outcome?.status).toBe('rejected');
    });

    it('rejects a fenced decrypt while retaining keys for the next decrypt', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-decrypt-reset-boundary';
      const channelKey = await generateChannelKey();
      const wrappedKey = await wrapChannelKey(channelKey, regKeys.publicKey);
      const ciphertext = await encryptMessage('retained plaintext', channelKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedKey, 4));
      await e2eeService.getChannelKey(channelId);

      const originalDecrypt = crypto.subtle.decrypt.bind(crypto.subtle);
      const decryptStarted = deferred<void>();
      const blockedPlaintext = deferred<ArrayBuffer>();
      let blockFirstDecrypt = true;
      const decryptSpy = vi
        .spyOn(crypto.subtle, 'decrypt')
        .mockImplementation(async (algorithm, key, data) => {
          if (algorithm.name === 'AES-GCM' && blockFirstDecrypt) {
            blockFirstDecrypt = false;
            decryptStarted.resolve(undefined);
            return blockedPlaintext.promise;
          }
          return originalDecrypt(algorithm, key, data);
        });

      let staleOutcome: PromiseOutcome<string> | undefined;
      let freshPlaintext: string | undefined;
      try {
        const staleOutcomePromise = observePromise(
          e2eeService.decryptForChannel(channelId, ciphertext)
        );
        await decryptStarted.promise;
        e2eeService.fencePendingOperations();
        blockedPlaintext.resolve(new TextEncoder().encode('retained plaintext').buffer);
        staleOutcome = await staleOutcomePromise;
        freshPlaintext = await e2eeService.decryptForChannel(channelId, ciphertext);
      } finally {
        decryptSpy.mockRestore();
      }

      expect.soft(staleOutcome?.status).toBe('rejected');
      expect.soft(freshPlaintext).toBe('retained plaintext');
      expect.soft(e2eeService.isInitialized).toBe(true);
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('rejects a prior-session current-key fetch without caching it or deleting the newer pending fetch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-current-session-boundary';
      const priorSessionKey = await generateChannelKey();
      const newerSessionKey = await generateChannelKey();
      const priorWrappedKey = await wrapChannelKey(priorSessionKey, regKeys.publicKey);
      const newerWrappedKey = await wrapChannelKey(newerSessionKey, regKeys.publicKey);
      const priorResponse = deferred<Response>();
      const newerResponse = deferred<Response>();

      mockApiFetch
        .mockImplementationOnce(() => priorResponse.promise)
        .mockImplementationOnce(() => newerResponse.promise)
        .mockResolvedValue(keyResponse(newerWrappedKey, 3));

      const priorOutcomePromise = observePromise(e2eeService.getChannelKey(channelId));

      e2eeService.clearKeys();
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const newerFetch = e2eeService.getChannelKey(channelId);

      priorResponse.resolve(keyResponse(priorWrappedKey, 2));
      const priorOutcome = await priorOutcomePromise;

      expect.soft(priorOutcome.status).toBe('rejected');
      expect.soft(e2eeService.getCurrentKeyVersion(channelId)).toBe(1);

      // This call observes whether the old request's finally block incorrectly
      // removed the newer pending promise.
      const joinedFetch = e2eeService.getChannelKey(channelId);
      expect.soft(mockApiFetch).toHaveBeenCalledTimes(2);

      newerResponse.resolve(keyResponse(newerWrappedKey, 3));
      const [newerKey, joinedKey] = await Promise.all([newerFetch, joinedFetch]);
      const newerCiphertext = await encryptMessage('newer current key', newerSessionKey);

      await expect(
        e2eeService.decryptWithKey(newerCiphertext, newerKey, channelGuard(channelId))
      ).resolves.toBe('newer current key');
      await expect(
        e2eeService.decryptWithKey(newerCiphertext, joinedKey, channelGuard(channelId))
      ).resolves.toBe('newer current key');
      expect(e2eeService.getCurrentKeyVersion(channelId)).toBe(3);
    });

    it('rejects a prior-session versioned fetch without caching it or deleting the newer pending fetch', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-versioned-session-boundary';
      const version = 7;
      const priorSessionKey = await generateChannelKey();
      const newerSessionKey = await generateChannelKey();
      const priorWrappedKey = await wrapChannelKey(priorSessionKey, regKeys.publicKey);
      const newerWrappedKey = await wrapChannelKey(newerSessionKey, regKeys.publicKey);
      const priorResponse = deferred<Response>();
      const newerResponse = deferred<Response>();

      mockApiFetch
        .mockImplementationOnce(() => priorResponse.promise)
        .mockImplementationOnce(() => newerResponse.promise)
        .mockResolvedValue(keyResponse(newerWrappedKey, version));

      const priorOutcomePromise = observePromise(
        e2eeService.getChannelKeyByVersion(channelId, version)
      );

      e2eeService.clearKeys();
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const newerFetch = e2eeService.getChannelKeyByVersion(channelId, version);

      priorResponse.resolve(keyResponse(priorWrappedKey, version));
      const priorOutcome = await priorOutcomePromise;
      type VersionedCacheInternals = {
        versionedKeyCache: Map<string, Map<number, unknown>>;
      };
      const internals = e2eeService as unknown as VersionedCacheInternals;

      expect.soft(priorOutcome.status).toBe('rejected');
      expect.soft(internals.versionedKeyCache.get(channelId)?.has(version) ?? false).toBe(false);

      const joinedFetch = e2eeService.getChannelKeyByVersion(channelId, version);
      expect.soft(mockApiFetch).toHaveBeenCalledTimes(2);

      newerResponse.resolve(keyResponse(newerWrappedKey, version));
      const [newerKey, joinedKey] = await Promise.all([newerFetch, joinedFetch]);
      const newerCiphertext = await encryptMessage('newer versioned key', newerSessionKey);

      await expect(
        e2eeService.decryptWithKey(newerCiphertext, newerKey, channelGuard(channelId))
      ).resolves.toBe('newer versioned key');
      await expect(
        e2eeService.decryptWithKey(newerCiphertext, joinedKey, channelGuard(channelId))
      ).resolves.toBe('newer versioned key');
    });

    it('rejects a cache-hit unwrap that crosses clearKeys and reinitialization', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-cache-hit-session-boundary';
      const channelKey = await generateChannelKey();
      const wrappedKey = await wrapChannelKey(channelKey, regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(wrappedKey, 4));
      await e2eeService.getChannelKey(channelId);

      type ServiceInternals = {
        derivePrivateKey: () => Promise<CryptoKey>;
      };
      const internals = e2eeService as unknown as ServiceInternals;
      const originalDerivePrivateKey = internals.derivePrivateKey.bind(e2eeService);
      const privateKey = await originalDerivePrivateKey();
      const blockedDerivation = deferred<CryptoKey>();
      internals.derivePrivateKey = () => blockedDerivation.promise;

      let outcome: PromiseOutcome<CryptoKey> | undefined;
      try {
        const outcomePromise = observePromise(e2eeService.getChannelKey(channelId));
        e2eeService.clearKeys();
        await e2eeService.initialize(
          testPassword,
          regKeys.wrappedPrivateKey,
          regKeys.keyDerivationSalt
        );
        blockedDerivation.resolve(privateKey);
        outcome = await outcomePromise;
      } finally {
        internals.derivePrivateKey = originalDerivePrivateKey;
      }

      expect(outcome?.status).toBe('rejected');
    });

    it('does not let a stale current-key error enroll rewrap after clearKeys', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-current-stale-error';
      const staleBody = deferred<unknown>();
      const staleJson = vi.fn(() => staleBody.promise);
      const newerResponse = deferred<Response>();
      const newerSessionKey = await generateChannelKey();
      const newerWrappedKey = await wrapChannelKey(newerSessionKey, regKeys.publicKey);
      const requestRewrapSpy = vi.spyOn(e2eeService, 'requestRewrap').mockResolvedValue(undefined);

      mockApiFetch
        .mockResolvedValueOnce({
          ok: false,
          status: 404,
          headers: new Headers(),
          json: staleJson,
        } as unknown as Response)
        .mockImplementationOnce(() => newerResponse.promise);

      const staleOutcomePromise = observePromise(e2eeService.getChannelKey(channelId));
      await Promise.resolve();
      await Promise.resolve();
      const staleJsonCalls = staleJson.mock.calls.length;

      e2eeService.clearKeys();
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const newerFetch = e2eeService.getChannelKey(channelId);

      staleBody.resolve({ code: 'NO_KEY_YET', pending: true });
      const staleOutcome = await staleOutcomePromise;
      const joinedFetch = e2eeService.getChannelKey(channelId);
      const apiCallsBeforeResolution = mockApiFetch.mock.calls.length;

      newerResponse.resolve(keyResponse(newerWrappedKey, 5));
      const [newerKey, joinedKey] = await Promise.all([newerFetch, joinedFetch]);
      const requestRewrapCalls = requestRewrapSpy.mock.calls.length;
      requestRewrapSpy.mockRestore();

      expect(staleJsonCalls).toBe(1);
      expect(staleOutcome.status).toBe('rejected');
      expect(requestRewrapCalls).toBe(0);
      expect(apiCallsBeforeResolution).toBe(2);
      expect(newerKey).toBeDefined();
      expect(joinedKey).toBeDefined();
    });

    it('does not let a stale versioned-key error enroll rewrap after clearKeys', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelId = 'ch-versioned-stale-error';
      const version = 8;
      const staleBody = deferred<unknown>();
      const staleJson = vi.fn(() => staleBody.promise);
      const newerResponse = deferred<Response>();
      const newerSessionKey = await generateChannelKey();
      const newerWrappedKey = await wrapChannelKey(newerSessionKey, regKeys.publicKey);
      const requestRewrapSpy = vi.spyOn(e2eeService, 'requestRewrap').mockResolvedValue(undefined);

      mockApiFetch
        .mockResolvedValueOnce({
          ok: false,
          status: 404,
          headers: new Headers(),
          json: staleJson,
        } as unknown as Response)
        .mockImplementationOnce(() => newerResponse.promise);

      const staleOutcomePromise = observePromise(
        e2eeService.getChannelKeyByVersion(channelId, version)
      );
      await Promise.resolve();
      await Promise.resolve();
      const staleJsonCalls = staleJson.mock.calls.length;

      e2eeService.clearKeys();
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const newerFetch = e2eeService.getChannelKeyByVersion(channelId, version);

      staleBody.resolve({ code: 'NO_KEY_YET', pending: true });
      const staleOutcome = await staleOutcomePromise;
      const joinedFetch = e2eeService.getChannelKeyByVersion(channelId, version);
      const apiCallsBeforeResolution = mockApiFetch.mock.calls.length;

      newerResponse.resolve(keyResponse(newerWrappedKey, version));
      const [newerKey, joinedKey] = await Promise.all([newerFetch, joinedFetch]);
      const requestRewrapCalls = requestRewrapSpy.mock.calls.length;
      requestRewrapSpy.mockRestore();

      expect(staleJsonCalls).toBe(1);
      expect(staleOutcome.status).toBe('rejected');
      expect(requestRewrapCalls).toBe(0);
      expect(apiCallsBeforeResolution).toBe(2);
      expect(newerKey).toBeDefined();
      expect(joinedKey).toBeDefined();
    });

    it('clears a prior-session 429 so the new session can fetch a key', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '60' }),
        json: () => Promise.resolve({ code: 'NO_KEY_YET', pending: false }),
      } as unknown as Response);
      await expect(e2eeService.getChannelKey('ch-prior-rate-limit')).rejects.toBeInstanceOf(
        E2EEKeyUnavailableError
      );

      e2eeService.clearKeys();
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      const newerSessionKey = await generateChannelKey();
      const newerWrappedKey = await wrapChannelKey(newerSessionKey, regKeys.publicKey);
      mockApiFetch.mockResolvedValueOnce(keyResponse(newerWrappedKey, 2));

      await expect(
        e2eeService.getChannelKey('ch-new-session-after-rate-limit')
      ).resolves.toBeDefined();
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
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
