// @vitest-environment node
/**
 * The rotation watermark records the epoch the server SERVED, never the one the
 * client ASKED for. Regression test for a red-team-proven chain:
 *
 *   attacker uploads a DM attachment attesting key_version = 2147483647
 *   -> DownloadAttachment reflects it as X-File-Key-Version
 *   -> the viewer's client calls getChannelKeyByVersion(conversationId, that)
 *   -> getDMKeyResponse ignored ?version= and answered 200 + the CURRENT key
 *   -> the client cached under the FABRICATED epoch and recorded it as the
 *      rotation watermark, which is MONOTONIC -- so every genuine rotation
 *      afterwards compared <= and was dropped, the sender kept encrypting under
 *      a CSK a revocation was meant to retire, and a sender-key re-base aimed at
 *      an epoch that does not exist tore down media.
 *
 * Three changes close it, and this file pins the client one, which is the only
 * one that also covers the OTHER entry point into the same primitive (an
 * unvalidated message.key_version, useMessageFetch.ts):
 *
 *   - client: record data.key.key_version, not the requested version (here)
 *   - server: fetchDMKey exact-matches ?version= (channels/handlers.go)
 *   - server: validateAttestedEpoch bounds the attested epoch (media/)
 *
 * The mock models a server that still answers 200 with the wrong version. That
 * is deliberate: with fetchDMKey landed our own server no longer does, and this
 * asserts the client is correct anyway rather than depending on it.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
  exportChannelKey,
} from '@/renderer/utils/crypto/crypto';
import { useE2EEStore } from '@/renderer/stores/auth/e2eeStore';
import { e2eeService } from '@/renderer/services/e2ee/e2eeService';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));
import { apiFetch } from '@/renderer/services/system/apiClient';
const mockApiFetch = vi.mocked(apiFetch);

const FORGED_EPOCH = 2147483647; // int32 max — accepted by key_version >= 1

const FORGED = 2147483647; // int32 max — passed the old `key_version >= 1` bound

describe('versioned key fetch — the SERVED epoch is authoritative', () => {
  const pw = 'TestPassword123!';
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;

  beforeEach(async () => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
    vi.clearAllMocks();
    regKeys = await generateRegistrationKeys(pw);
    await e2eeService.initialize(pw, regKeys.wrappedPrivateKey, regKeys.keyDerivationSalt);
  });
  afterEach(() => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
  });

  it('a 200 carrying a DIFFERENT epoch cannot move the watermark', async () => {
    const conv = '11111111-1111-4111-8111-111111111111';
    const cskEpoch3 = await generateChannelKey();
    const cskEpoch4 = await generateChannelKey();
    const wrapped3 = await wrapChannelKey(cskEpoch3, regKeys.publicKey);
    const wrapped4 = await wrapChannelKey(cskEpoch4, regKeys.publicKey);

    // The OLD getDMKeyResponse: ORDER BY key_version DESC LIMIT 1, ?version=
    // never read. It answers 200 with the current key whatever you ask for.
    let currentWrapped = wrapped3;
    let currentEpoch = 3;
    mockApiFetch.mockImplementation(
      async () =>
        ({
          ok: true,
          json: async () => ({ key: { wrapped_key: currentWrapped, key_version: currentEpoch } }),
        }) as unknown as Response
    );

    const announced: Array<{ channelId: string; keyVersion: number }> = [];
    const off = e2eeService.onKeyRotation((e) => announced.push(e));

    // Baseline at the real epoch.
    await e2eeService.getChannelKeyByVersion(conv, 3);
    expect(announced).toEqual([]);
    expect(e2eeService.getHighestSeenKeyVersion(conv)).toBe(3);

    // THE ATTACK. The server still answers 200 with epoch 3.
    await e2eeService.getChannelKeyByVersion(conv, FORGED);

    // No announce, and the watermark holds at what the server served.
    expect(announced).toEqual([]);
    expect(e2eeService.getHighestSeenKeyVersion(conv)).toBe(3);

    // POSITIVE CONTROL: the watermark is not simply frozen — a REAL rotation
    // still moves it and still announces. Without this, a fix that just stopped
    // calling noteChannelVersion would pass everything above.
    currentWrapped = wrapped4;
    currentEpoch = 4;
    await e2eeService.getChannelKeyByVersion(conv, 4);
    expect(announced).toEqual([{ channelId: conv, keyVersion: 4 }]);
    expect(e2eeService.getHighestSeenKeyVersion(conv)).toBe(4);

    off();
  });

  it('the forged epoch is not cached under its own number', async () => {
    // Caching the current CSK under a fabricated epoch is the other half: a
    // later lookup for that epoch resolved from cache to a key the rotation had
    // retired, with no network call to correct it.
    const conv = '33333333-3333-4333-8333-333333333333';
    const csk = await generateChannelKey();
    const wrapped = await wrapChannelKey(csk, regKeys.publicKey);
    mockApiFetch.mockImplementation(
      async () =>
        ({
          ok: true,
          json: async () => ({ key: { wrapped_key: wrapped, key_version: 3 } }),
        }) as unknown as Response
    );

    await e2eeService.getChannelKeyByVersion(conv, FORGED);
    mockApiFetch.mockClear();

    // Asking again must go BACK to the server rather than hitting a cache entry
    // filed under the forged number.
    await e2eeService.getChannelKeyByVersion(conv, FORGED);
    expect(mockApiFetch).toHaveBeenCalled();

    // POSITIVE CONTROL: the served epoch IS cached, so this is not just "the
    // cache is broken".
    mockApiFetch.mockClear();
    await e2eeService.getChannelKeyByVersion(conv, 3);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });
});
