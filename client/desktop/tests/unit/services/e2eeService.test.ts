// @vitest-environment node
import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
} from '@/renderer/utils/crypto';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2eeErrors';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';

// Test the singleton and clear between tests
import { e2eeService } from '@/renderer/services/e2eeService';

// Mock apiFetch for channel key fetching
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

import { apiFetch } from '@/renderer/services/apiClient';
const mockApiFetch = vi.mocked(apiFetch);

describe('e2eeService', () => {
  const testPassword = 'TestPassword123!';
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;

  beforeEach(async () => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
    vi.clearAllMocks();
    // Generate fresh keys for each test
    regKeys = await generateRegistrationKeys(testPassword);
  });

  afterEach(() => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
  });

  describe('initialize', () => {
    it('initializes the service', async () => {
      expect(e2eeService.isInitialized).toBe(false);
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      expect(e2eeService.isInitialized).toBe(true);
    });

    it('flips useE2EEStore.ready to true on success (#270 Task 21b)', async () => {
      expect(useE2EEStore.getState().ready).toBe(false);
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      expect(useE2EEStore.getState().ready).toBe(true);
    });
  });

  describe('clearKeys', () => {
    it('clears all keys and resets state', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      expect(e2eeService.isInitialized).toBe(true);

      e2eeService.clearKeys();
      expect(e2eeService.isInitialized).toBe(false);
    });

    it('flips useE2EEStore.ready back to false (#270 Task 21b)', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
      expect(useE2EEStore.getState().ready).toBe(true);

      e2eeService.clearKeys();
      expect(useE2EEStore.getState().ready).toBe(false);
    });
  });

  describe('encryptPreferences / decryptPreferences', () => {
    it('round-trips preferences data', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const prefs = { theme: 'dark', fontSize: 14 };
      const encrypted = await e2eeService.encryptPreferences(prefs);
      expect(typeof encrypted).toBe('string');

      const decrypted = await e2eeService.decryptPreferences<typeof prefs>(encrypted);
      expect(decrypted).toEqual(prefs);
    });

    it('throws when not initialized', async () => {
      await expect(e2eeService.encryptPreferences({ test: true })).rejects.toThrow(
        'E2EE service not initialized'
      );
    });
  });

  describe('getChannelKey', () => {
    it('fetches and unwraps channel key from server', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      // Create a channel key and wrap it for our user
      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      // Mock the API response
      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
      } as Response);

      const unwrapped = await e2eeService.getChannelKey('channel-1');
      expect(unwrapped).toBeDefined();
      expect(unwrapped.algorithm.name).toBe('AES-GCM');
    });

    it('caches wrapped keys', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch.mockResolvedValueOnce({
        ok: true,
        json: () => Promise.resolve({ key: { wrapped_key: wrappedForUser } }),
      } as Response);

      // First call fetches from server
      await e2eeService.getChannelKey('channel-1');
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      // Second call uses cache
      await e2eeService.getChannelKey('channel-1');
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('throws E2EEKeyUnavailableError with pending=true when server indicates pending', async () => {
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );

      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        json: () => Promise.resolve({ code: 'NO_KEY_YET', kind: 'channel', pending: true }),
      } as Response);

      try {
        await e2eeService.getChannelKey('channel-1');
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('NO_KEY_YET');
        expect((err as E2EEKeyUnavailableError).pending).toBe(true);
      }
    });
  });

  describe('encryptForChannel / decryptForChannel', () => {
    it('round-trips a message', async () => {
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

      const encrypted = await e2eeService.encryptForChannel('channel-1', 'Secret message');
      expect(typeof encrypted).toBe('string');
      expect(encrypted).not.toBe('Secret message');

      const decrypted = await e2eeService.decryptForChannel('channel-1', encrypted);
      expect(decrypted).toBe('Secret message');
    });
  });

  describe('invalidateChannelKey', () => {
    it('removes cached key for a channel', async () => {
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

      await e2eeService.getChannelKey('channel-1');
      expect(mockApiFetch).toHaveBeenCalledTimes(1);

      e2eeService.invalidateChannelKey('channel-1');

      // Should fetch again after invalidation
      await e2eeService.getChannelKey('channel-1');
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });
  });

  describe('fetchAndUnwrapChannelKey envelope parsing', () => {
    beforeEach(async () => {
      e2eeService.clearKeys();
      // Reset rate limiter state (private field, not cleared by clearKeys)
      (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
      vi.clearAllMocks();
      regKeys = await generateRegistrationKeys(testPassword);
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
    });

    it.each([
      ['NOT_MEMBER', false, 404],
      ['NO_KEY_YET', true, 404],
      ['REVOKED_EPOCH', false, 404],
      ['INVALID_REQUEST', false, 400],
    ] as const)(
      'throws E2EEKeyUnavailableError with code=%s on %i',
      async (code, pending, status) => {
        mockApiFetch.mockResolvedValueOnce({
          ok: false,
          status,
          headers: new Headers(),
          json: () => Promise.resolve({ error: 'test', code, kind: 'channel', pending }),
        } as unknown as Response);

        try {
          await e2eeService.getChannelKey('11111111-1111-1111-1111-111111111111');
          throw new Error('expected throw');
        } catch (err) {
          expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
          expect((err as E2EEKeyUnavailableError).code).toBe(code);
          expect((err as E2EEKeyUnavailableError).pending).toBe(pending);
        }
      }
    );

    it('defaults to NO_KEY_YET when server returns a bare error without envelope', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        headers: new Headers(),
        json: () => Promise.resolve({ error: 'legacy shape' }),
      } as unknown as Response);

      try {
        await e2eeService.getChannelKey('11111111-1111-1111-1111-111111111111');
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('NO_KEY_YET');
        expect((err as E2EEKeyUnavailableError).pending).toBe(false);
      }
    });

    it('rate-limit 429 sets rateLimitedUntil and throws typed error', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '60' }),
        json: () => Promise.resolve({ error: 'rate limit', code: 'NO_KEY_YET', kind: 'channel' }),
      } as unknown as Response);

      await expect(
        e2eeService.getChannelKey('11111111-1111-1111-1111-111111111111')
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);

      // Second call should short-circuit on rateLimitedUntil — no further mock needed
      await expect(
        e2eeService.getChannelKey('22222222-2222-2222-2222-222222222222')
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);
    });
  });

  describe('getChannelKeyByVersion envelope parsing', () => {
    beforeEach(async () => {
      e2eeService.clearKeys();
      // Reset rate limiter state (private field, not cleared by clearKeys)
      (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
      vi.clearAllMocks();
      regKeys = await generateRegistrationKeys(testPassword);
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
    });

    it.each([
      ['NOT_MEMBER', false, 404],
      ['NO_KEY_YET', true, 404],
      ['REVOKED_EPOCH', false, 404],
      ['INVALID_REQUEST', false, 400],
    ] as const)(
      'throws E2EEKeyUnavailableError with code=%s on %i',
      async (code, pending, status) => {
        mockApiFetch.mockResolvedValueOnce({
          ok: false,
          status,
          headers: new Headers(),
          json: () => Promise.resolve({ error: 'test', code, kind: 'channel', pending }),
        } as unknown as Response);

        try {
          await e2eeService.getChannelKeyByVersion('11111111-1111-1111-1111-111111111111', 2);
          throw new Error('expected throw');
        } catch (err) {
          expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
          expect((err as E2EEKeyUnavailableError).code).toBe(code);
          expect((err as E2EEKeyUnavailableError).pending).toBe(pending);
        }

        // Verify the URL carried the version query param
        expect(mockApiFetch).toHaveBeenCalledWith(
          expect.stringMatching(/\/api\/v1\/e2ee\/keys\/.*\?version=2/)
        );
      }
    );

    it('defaults to NO_KEY_YET when server returns a bare error without envelope', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 404,
        headers: new Headers(),
        json: () => Promise.resolve({ error: 'legacy shape' }),
      } as unknown as Response);

      try {
        await e2eeService.getChannelKeyByVersion('11111111-1111-1111-1111-111111111111', 2);
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('NO_KEY_YET');
        expect((err as E2EEKeyUnavailableError).pending).toBe(false);
      }
    });

    it('rate-limit 429 sets rateLimitedUntil and throws typed error', async () => {
      mockApiFetch.mockResolvedValueOnce({
        ok: false,
        status: 429,
        headers: new Headers({ 'Retry-After': '60' }),
        json: () => Promise.resolve({ error: 'rate limit', code: 'NO_KEY_YET', kind: 'channel' }),
      } as unknown as Response);

      await expect(
        e2eeService.getChannelKeyByVersion('11111111-1111-1111-1111-111111111111', 2)
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);

      // Second call should short-circuit on rateLimitedUntil — no further mock needed
      await expect(
        e2eeService.getChannelKeyByVersion('22222222-2222-2222-2222-222222222222', 3)
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);
    });
  });

  describe('validateWrapShape + cache-poison refetch', () => {
    beforeEach(async () => {
      e2eeService.clearKeys();
      // Reset rate limiter state (private field, not cleared by clearKeys)
      (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
      vi.clearAllMocks();
      regKeys = await generateRegistrationKeys(testPassword);
      await e2eeService.initialize(
        testPassword,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt
      );
    });

    /**
     * Helper: build a response body for the /keys/:channelId endpoint.
     */
    const okKeyResponse = (wrappedKey: string, keyVersion = 1) =>
      ({
        ok: true,
        status: 200,
        headers: new Headers(),
        json: () =>
          Promise.resolve({
            key: { wrapped_key: wrappedKey, key_version: keyVersion },
            kind: 'channel',
          }),
      }) as unknown as Response;

    it('throws MALFORMED_PAYLOAD when wrapped_key is not 512 bytes after base64 decode', async () => {
      // Seed a wrap that base64-decodes to 10 bytes (well under 512)
      const shortWrapBytes = new Uint8Array(10).fill(0x42);
      const shortWrap = btoa(String.fromCharCode.apply(null, Array.from(shortWrapBytes)));

      // BOTH fetch calls (first + refetch) return the bad wrap — refetch is
      // bounded and must terminate with MALFORMED_PAYLOAD.
      mockApiFetch.mockResolvedValue(okKeyResponse(shortWrap));

      try {
        await e2eeService.getChannelKey('11111111-1111-1111-1111-111111111111');
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }
    });

    it('refetch count is bounded at 1 — does not loop (getChannelKey)', async () => {
      const badWrapBytes = new Uint8Array(256).fill(0xaa);
      const badWrap = btoa(String.fromCharCode.apply(null, Array.from(badWrapBytes)));
      let callCount = 0;
      mockApiFetch.mockImplementation(async () => {
        callCount += 1;
        return okKeyResponse(badWrap);
      });

      try {
        await e2eeService.getChannelKey('11111111-1111-1111-1111-111111111111');
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }
      // Exactly one initial fetch + one refetch = 2
      expect(callCount).toBe(2);
    });

    it('refetch count is bounded at 1 — does not loop (getChannelKeyByVersion)', async () => {
      const badWrapBytes = new Uint8Array(256).fill(0xbb);
      const badWrap = btoa(String.fromCharCode.apply(null, Array.from(badWrapBytes)));
      let callCount = 0;
      mockApiFetch.mockImplementation(async () => {
        callCount += 1;
        return okKeyResponse(badWrap, 2);
      });

      try {
        await e2eeService.getChannelKeyByVersion('11111111-1111-1111-1111-111111111111', 2);
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }
      // Exactly one initial fetch + one refetch = 2
      expect(callCount).toBe(2);
    });

    it('evicts versioned cache entry after cache-poison refetch budget is exhausted', async () => {
      // T-9: the versioned cache entry for (channelId, version) must be removed
      // after the refetch-budget is exhausted, so a later call does not return
      // a stale malformed wrap from the versioned cache.
      const badWrapBytes = new Uint8Array(10).fill(0x42);
      const badWrap = btoa(String.fromCharCode.apply(null, Array.from(badWrapBytes)));
      const channelId = '66666666-6666-6666-6666-666666666666';
      const version = 2;

      mockApiFetch.mockResolvedValue(okKeyResponse(badWrap, version));

      try {
        await e2eeService.getChannelKeyByVersion(channelId, version);
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }

      // Assert the versioned cache entry for (channelId, version) is gone.
      const svc = e2eeService as unknown as {
        versionedKeyCache: Map<string, Map<number, unknown>>;
      };
      const versionMap = svc.versionedKeyCache.get(channelId);
      expect(versionMap?.get(version)).toBeUndefined();
    });

    it('refetch counter resets after successful recovery — later malformed wrap gets its own fair budget', async () => {
      // T-5: after a successful recovery (bad wrap → refetch returns valid wrap),
      // a LATER malformed wrap must get its own one-refetch budget, not be denied
      // because the previous cycle used the counter.
      //
      // Sequence (4 network calls expected):
      //   1. First call: malformed → triggers refetch
      //   2. Second call: valid wrap → successful recovery resets counter
      //   3. (test invalidates cache to force refetch) Third call: malformed → triggers refetch
      //   4. Fourth call: malformed → terminal MALFORMED_PAYLOAD
      const shortWrapBytes = new Uint8Array(10).fill(0x42);
      const shortWrap = btoa(String.fromCharCode.apply(null, Array.from(shortWrapBytes)));

      const channelKey = await generateChannelKey();
      const goodWrap = await wrapChannelKey(channelKey, regKeys.publicKey);
      const channelId = '77777777-7777-7777-7777-777777777777';

      mockApiFetch
        .mockResolvedValueOnce(okKeyResponse(shortWrap)) // 1: bad
        .mockResolvedValueOnce(okKeyResponse(goodWrap)) // 2: good (successful recovery)
        .mockResolvedValueOnce(okKeyResponse(shortWrap)) // 3: bad (fresh malformed event)
        .mockResolvedValueOnce(okKeyResponse(shortWrap)); // 4: bad again → terminal

      // First recovery: returns unwrapped key successfully after 2 calls.
      const unwrapped = await e2eeService.getChannelKey(channelId);
      expect(unwrapped).toBeDefined();
      expect(mockApiFetch).toHaveBeenCalledTimes(2);

      // After successful recovery the main-cache entry holds a real wrapped key
      // with refetchAfterMalformed: 0. Invalidate so the next call refetches
      // fresh from the server.
      e2eeService.invalidateChannelKey(channelId);

      // Second cycle: fresh malformed event gets its own bounded refetch.
      try {
        await e2eeService.getChannelKey(channelId);
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }
      // 2 calls from first recovery + 2 calls from second cycle = 4 total.
      expect(mockApiFetch).toHaveBeenCalledTimes(4);
    });

    it('accepts a valid 512-byte wrap (shape validator does not reject)', async () => {
      // A 512-byte buffer that passes the shape check but is not a real RSA
      // ciphertext — unwrapChannelKey will throw a native DOMException, which
      // is different from MALFORMED_PAYLOAD. The important assertion: the
      // validator does NOT reject a 512-byte wrap.
      const validSizeBytes = new Uint8Array(512).fill(0xff);
      const validSizeWrap = btoa(String.fromCharCode.apply(null, Array.from(validSizeBytes)));

      mockApiFetch.mockResolvedValueOnce(okKeyResponse(validSizeWrap));

      let thrown: unknown = null;
      try {
        await e2eeService.getChannelKey('55555555-5555-5555-5555-555555555555');
      } catch (err) {
        thrown = err;
      }
      // The fetch reached unwrapChannelKey (not rejected at shape validation).
      // unwrapChannelKey fails because 0xFF repeated is not a valid RSA-OAEP
      // ciphertext — the error is NOT MALFORMED_PAYLOAD.
      expect(thrown).not.toBeNull();
      if (thrown instanceof E2EEKeyUnavailableError) {
        // If this SOMEHOW became an E2EEKeyUnavailableError, the code must
        // NOT be MALFORMED_PAYLOAD (which would mean shape check rejected).
        expect(thrown.code).not.toBe('MALFORMED_PAYLOAD');
      }
      // Stronger positive: confirm the network got the expected URL
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/e2ee/keys/55555555-5555-5555-5555-555555555555')
      );
    });

    it('throws MALFORMED_PAYLOAD when wrapped_key is not valid base64', async () => {
      // Non-base64 input makes atob throw DOMException; validateWrapShape
      // must catch and classify it as MALFORMED_PAYLOAD so the cache-poison
      // defense triggers a bounded refetch (both calls return bad input here).
      const invalidBase64 = 'not!base64!!@#$';
      mockApiFetch.mockResolvedValue(okKeyResponse(invalidBase64));

      try {
        await e2eeService.getChannelKey('33333333-3333-3333-3333-333333333333');
        throw new Error('expected throw');
      } catch (err) {
        expect(err).toBeInstanceOf(E2EEKeyUnavailableError);
        expect((err as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
      }
    });

    it('refetch succeeds when the second fetch returns a valid wrap', async () => {
      // First call: malformed. Second call: valid wrap for the real channel key.
      const shortWrapBytes = new Uint8Array(10).fill(0x42);
      const shortWrap = btoa(String.fromCharCode.apply(null, Array.from(shortWrapBytes)));

      const channelKey = await generateChannelKey();
      const wrappedForUser = await wrapChannelKey(channelKey, regKeys.publicKey);

      mockApiFetch
        .mockResolvedValueOnce(okKeyResponse(shortWrap))
        .mockResolvedValueOnce(okKeyResponse(wrappedForUser));

      const unwrapped = await e2eeService.getChannelKey('33333333-3333-3333-3333-333333333333');
      expect(unwrapped).toBeDefined();
      expect(unwrapped.algorithm.name).toBe('AES-GCM');
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });

    it('invalidateChannelKey clears the refetch counter so a fresh malformed payload still triggers one refetch', async () => {
      const shortWrapBytes = new Uint8Array(10).fill(0x42);
      const shortWrap = btoa(String.fromCharCode.apply(null, Array.from(shortWrapBytes)));

      // First run: 2 calls (initial + refetch), both malformed → throw
      mockApiFetch.mockResolvedValue(okKeyResponse(shortWrap));
      await expect(
        e2eeService.getChannelKey('44444444-4444-4444-4444-444444444444')
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);
      expect(mockApiFetch).toHaveBeenCalledTimes(2);

      // Explicit invalidate → should reset refetch counter
      e2eeService.invalidateChannelKey('44444444-4444-4444-4444-444444444444');

      // Second run: should again allow initial + refetch (2 more calls)
      await expect(
        e2eeService.getChannelKey('44444444-4444-4444-4444-444444444444')
      ).rejects.toBeInstanceOf(E2EEKeyUnavailableError);
      expect(mockApiFetch).toHaveBeenCalledTimes(4);
    });
  });
});
