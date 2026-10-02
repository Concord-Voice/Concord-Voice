// @vitest-environment node
/**
 * Channel-key epoch atomicity — `getChannelKeyMaterial` must hand back a key
 * and the epoch that belongs to it, and must never resolve with an epoch that
 * is not a positive safe integer.
 *
 * L1: a current-key response whose `key_version` is missing, null, zero,
 *     negative, a string or fractional is a malformed payload. Before this
 *     was enforced, `fetchAndUnwrapChannelKey` coalesced it with `|| 1` or
 *     passed it through unchecked, so the send path could stamp an epoch the
 *     key was never issued under.
 * L2: after a malformed wrap, the REFETCHED epoch is the one returned, and the
 *     cache slot reads 0 while the refetch is in flight.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
  encryptMessage,
  decryptMessage,
} from '@/renderer/utils/crypto/crypto';

import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
import { deferred } from '../../helpers/deferred';
const mockApiFetch = vi.mocked(apiFetch);

function keyResponseBody(key: Record<string, unknown>): Response {
  return {
    ok: true,
    json: () => Promise.resolve({ key }),
  } as Response;
}

// Base64 of 3 bytes: decodes fine but is not the 512 bytes RSA-OAEP-4096 emits,
// so validateWrapShape rejects it with MALFORMED_PAYLOAD.
const SHORT_WRAP = 'AAAA';

describe('channel key epoch atomicity', () => {
  const testPassword = 'TestPassword123!'; // pragma: allowlist secret
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;

  beforeEach(async () => {
    e2eeService.clearKeys();
    // Private field, not cleared by clearKeys()
    (e2eeService as any).rateLimitedUntil = 0;
    vi.clearAllMocks();
    // clearAllMocks does not drain a *Once queue; mockReset does.
    mockApiFetch.mockReset();
    regKeys = await generateRegistrationKeys(testPassword);
    await e2eeService.initialize(
      testPassword,
      regKeys.wrappedPrivateKey,
      regKeys.keyDerivationSalt
    );
  });

  afterEach(() => {
    e2eeService.clearKeys();
  });

  describe('current-key response with an invalid key_version', () => {
    // regression: dm-send-atomic-epoch review L1
    const badVersions: Array<[string, Record<string, unknown>]> = [
      ['missing field', {}],
      ['null', { key_version: null }],
      ['0', { key_version: 0 }],
      ['-3', { key_version: -3 }],
      ["'2' (string)", { key_version: '2' }],
      ['2.5', { key_version: 2.5 }],
      ['2**53 (beyond the safe-integer range)', { key_version: 2 ** 53 }],
    ];

    it.each(badVersions)(
      'rejects with MALFORMED_PAYLOAD after one refetch when key_version is %s',
      async (_label, versionField) => {
        const channelKey = await generateChannelKey();
        const validWrap = await wrapChannelKey(channelKey, regKeys.publicKey);
        mockApiFetch.mockResolvedValue(
          keyResponseBody({ wrapped_key: validWrap, ...versionField })
        );

        const outcome = await e2eeService.getChannelKeyMaterial('ch-bad-version').then(
          (value) => ({ status: 'fulfilled' as const, keyVersion: value.keyVersion }),
          (reason: unknown) => ({ status: 'rejected' as const, reason })
        );

        // toMatchObject (not a bare status compare) so a resolve prints the epoch it resolved with.
        expect(outcome).toMatchObject({ status: 'rejected' });
        if (outcome.status === 'rejected') {
          expect(outcome.reason).toBeInstanceOf(E2EEKeyUnavailableError);
          expect((outcome.reason as E2EEKeyUnavailableError).code).toBe('MALFORMED_PAYLOAD');
        }
        expect(mockApiFetch).toHaveBeenCalledTimes(2);
      }
    );

    // regression: dm-send-atomic-epoch review L1
    it('resolves with the refetched epoch when the first key_version is invalid and the second is 4', async () => {
      const channelKey = await generateChannelKey();
      const validWrap = await wrapChannelKey(channelKey, regKeys.publicKey);
      mockApiFetch
        .mockResolvedValueOnce(keyResponseBody({ wrapped_key: validWrap, key_version: 0 }))
        .mockResolvedValueOnce(keyResponseBody({ wrapped_key: validWrap, key_version: 4 }));

      const material = await e2eeService.getChannelKeyMaterial('ch-bad-then-good');

      expect(material.keyVersion).toBe(4);
      expect(mockApiFetch).toHaveBeenCalledTimes(2);
    });
  });

  describe('malformed wrap refetch', () => {
    // regression: dm-send-atomic-epoch review L2
    // The two responses carry DIFFERENT epochs, so the test can tell which one
    // the result took. With both at 4 it passed even if the version came from
    // the malformed first response.
    it('returns the refetched epoch with its own key, and reads 0 from the cache slot while the refetch is pending', async () => {
      const channelId = 'ch-malformed-wrap';
      const channelKey = await generateChannelKey();
      const validWrap = await wrapChannelKey(channelKey, regKeys.publicKey);
      const refetch = deferred<Response>();

      mockApiFetch
        .mockResolvedValueOnce(keyResponseBody({ wrapped_key: SHORT_WRAP, key_version: 3 }))
        .mockReturnValueOnce(refetch.promise);

      const pending = e2eeService.getChannelKeyMaterial(channelId);

      // Wait until the refetch has been issued (the first response was malformed).
      await vi.waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
      expect(e2eeService.getCurrentKeyVersion(channelId)).toBe(0);

      refetch.resolve(keyResponseBody({ wrapped_key: validWrap, key_version: 4 }));
      const material = await pending;

      expect(material.keyVersion).toBe(4);
      const ciphertext = await encryptMessage('epoch four payload', channelKey);
      await expect(decryptMessage(ciphertext, material.channelKey)).resolves.toBe(
        'epoch four payload'
      );
      expect(e2eeService.getCurrentKeyVersion(channelId)).toBe(4);
    });
  });
});
