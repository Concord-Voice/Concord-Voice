/**
 * DM manual rotation and the missing-key backoff.
 *
 * Prod, 2026-09-17: "Rotate Encryption Key" on a DM posted a bodyless
 * request, the server recorded the revocation of epoch 1, and no successor
 * epoch was ever wrapped for anyone — the conversation was undecryptable for
 * every participant. `rotateDMKey` wraps the successor for every participant
 * and posts the batch the server now commits with the revocation, or refuses.
 *
 * The backoff closes the second finding from the same console: the sidebar's
 * preview decrypt re-ran on every store change and re-fetched the same 404 a
 * dozen times a minute per keyless conversation.
 */
import { describe, it, expect, beforeEach, afterEach, vi } from 'vitest';
import {
  generateRegistrationKeys,
  generateChannelKey,
  wrapChannelKey,
  exportPublicKey,
} from '@/renderer/utils/crypto/crypto';

import { e2eeService } from '@/renderer/services/e2ee/e2eeService';
import {
  DMRotationError,
  E2EEEpochClaimStaleError,
  E2EEKeyUnavailableError,
} from '@/renderer/services/e2ee/e2eeErrors';

vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: vi.fn(),
  safeJson: async (res: { json: () => Promise<unknown> }) => res.json(),
  API_BASE: 'http://localhost:8080',
}));

import { apiFetch } from '@/renderer/services/system/apiClient';
const mockApiFetch = vi.mocked(apiFetch);

const CONV = 'conv-dm-1';
const KEYS_PATH = `/api/v1/e2ee/keys/${CONV}`;
const ROTATE_PATH = `/api/v1/dm/conversations/${CONV}/rotate-key`;
const publicKeyPath = (userId: string) => `/api/v1/users/${userId}/public-key`;

type Route = (init?: RequestInit) => Response | Promise<Response>;

function jsonResponse(status: number, body: unknown): Response {
  return {
    ok: status >= 200 && status < 300,
    status,
    headers: new Headers(),
    json: () => Promise.resolve(body),
  } as unknown as Response;
}

function routes(table: Record<string, Route>) {
  mockApiFetch.mockImplementation((path: string, init?: RequestInit) => {
    const route = table[path];
    if (!route) return Promise.resolve(jsonResponse(500, { error: `unrouted ${path}` }));
    return Promise.resolve(route(init));
  });
}

function callsTo(path: string) {
  return mockApiFetch.mock.calls.filter(([p]) => p === path);
}

describe('e2eeService — DM rotation', () => {
  const password = 'TestPassword123!'; // pragma: allowlist secret
  let regKeys: Awaited<ReturnType<typeof generateRegistrationKeys>>;
  let pubKeyBase64: string;
  let wrappedCurrent: string;

  beforeEach(async () => {
    e2eeService.clearKeys();
    (e2eeService as unknown as { rateLimitedUntil: number }).rateLimitedUntil = 0;
    vi.clearAllMocks();
    regKeys = await generateRegistrationKeys(password);
    pubKeyBase64 = await exportPublicKey(regKeys.publicKey);
    wrappedCurrent = await wrapChannelKey(await generateChannelKey(), regKeys.publicKey);
    await e2eeService.initialize(password, regKeys.wrappedPrivateKey, regKeys.keyDerivationSalt);
  });

  afterEach(() => {
    e2eeService.clearKeys();
  });

  describe('rotateDMKey', () => {
    it('wraps the successor for every participant and posts it at the next epoch', async () => {
      let posted: Record<string, unknown> | null = null;
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 4 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 2 }),
        [publicKeyPath('u2')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        [ROTATE_PATH]: (init) => {
          posted = JSON.parse(String(init?.body));
          return jsonResponse(200, { message: 'Key rotated', new_key_version: 5 });
        },
      });

      const res = await e2eeService.rotateDMKey(CONV, ['u1', 'u2', 'u1']);

      expect(res.ok).toBe(true);
      expect(posted).not.toBeNull();
      const body = posted as unknown as {
        wrapped_keys: Record<string, string>;
        key_version: number;
        key_fingerprint: string;
        wrapped_key_versions: Record<string, number>;
      };
      expect(Object.keys(body.wrapped_keys).sort()).toEqual(['u1', 'u2']);
      expect(body.wrapped_keys.u1).not.toBe(body.wrapped_keys.u2); // RSA-OAEP is randomized
      expect(body.key_version).toBe(5);
      expect(body.key_fingerprint).toMatch(/^[A-Za-z0-9+/]+=*$/);
      expect(body.wrapped_key_versions).toEqual({ u1: 2, u2: 1 });
      // The duplicate participant id was fetched once.
      expect(callsTo(publicKeyPath('u1'))).toHaveLength(1);
      expect(callsTo(ROTATE_PATH)[0][1]).toEqual(expect.objectContaining({ method: 'POST' }));

      // Success invalidates the cached epoch so the next use fetches the successor.
      expect(callsTo(KEYS_PATH)).toHaveLength(1);
      await e2eeService.getChannelKeyMaterial(CONV);
      expect(callsTo(KEYS_PATH)).toHaveLength(2);
    });

    it('returns the refusal unchanged and keeps the current epoch cached', async () => {
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 1 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        [ROTATE_PATH]: () => jsonResponse(429, { retry_after: 60 }),
      });

      const res = await e2eeService.rotateDMKey(CONV, ['u1']);

      expect(res.status).toBe(429);
      await e2eeService.getChannelKeyMaterial(CONV);
      expect(callsTo(KEYS_PATH)).toHaveLength(1);
    });

    it('refuses before posting when a participant has no public key', async () => {
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 1 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        [publicKeyPath('u2')]: () => jsonResponse(404, { error: 'not found' }),
      });

      await expect(e2eeService.rotateDMKey(CONV, ['u1', 'u2'])).rejects.toThrow(
        new DMRotationError('A participant has no encryption key yet')
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
    });

    it('refuses before posting when a participant key version is not positive', async () => {
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 1 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        // key_version 0 is anomalous; it must be treated as a missing version,
        // not posted (the server would answer 409 stale_recipients).
        [publicKeyPath('u2')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 0 }),
      });

      await expect(e2eeService.rotateDMKey(CONV, ['u1', 'u2'])).rejects.toThrow(
        new DMRotationError("A participant's encryption key is not ready")
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
    });

    it('refuses before posting when a participant has a key but no version', async () => {
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 1 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        // A key with no key_version: on a rotation the server would store this
        // wrap without the #2420 freshness check, so rotateDMKey must refuse.
        [publicKeyPath('u2')]: () => jsonResponse(200, { public_key: pubKeyBase64 }),
      });

      await expect(e2eeService.rotateDMKey(CONV, ['u1', 'u2'])).rejects.toThrow(
        new DMRotationError("A participant's encryption key is not ready")
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
    });

    it("refuses before posting when a participant's public key cannot be used", async () => {
      routes({
        [KEYS_PATH]: () =>
          jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 1 } }),
        [publicKeyPath('u1')]: () =>
          jsonResponse(200, { public_key: pubKeyBase64, key_version: 1 }),
        [publicKeyPath('u2')]: () => jsonResponse(200, { public_key: 'not-a-key', key_version: 1 }),
      });
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});

      await expect(e2eeService.rotateDMKey(CONV, ['u1', 'u2'])).rejects.toThrow(
        new DMRotationError("A participant's encryption key could not be used")
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
      warn.mockRestore();
    });

    it('refuses when this device holds no current key', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(404, { code: 'NO_KEY_YET', pending: true }),
        [`/api/v1/e2ee/keys/${CONV}/rewrap`]: () => jsonResponse(200, {}),
      });

      await expect(e2eeService.rotateDMKey(CONV, ['u1'])).rejects.toThrow(
        new DMRotationError('You need the current key to rotate it')
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
    });

    it('refuses while the held epoch is revoked and being re-keyed', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(404, { code: 'REVOKED_EPOCH' }),
      });

      await expect(e2eeService.rotateDMKey(CONV, ['u1'])).rejects.toThrow(
        new DMRotationError('This conversation is being re-keyed; try again shortly')
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
    });

    it('surfaces a transient key-read failure as retryable, not a missing-key precondition', async () => {
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => {});
      // A network rejection reading the current key is not a precondition the
      // user can satisfy by acquiring a key; it must read as retryable.
      mockApiFetch.mockImplementation((path: string) =>
        path === KEYS_PATH
          ? Promise.reject(new Error('network down'))
          : Promise.resolve(jsonResponse(500, { error: 'unrouted' }))
      );

      await expect(e2eeService.rotateDMKey(CONV, ['u1'])).rejects.toThrow(
        new DMRotationError("Couldn't read the current key; try again")
      );
      expect(callsTo(ROTATE_PATH)).toHaveLength(0);
      debug.mockRestore();
    });
  });

  describe('rotateChannelKey stale-claim refusal', () => {
    it('surfaces the epoch the server names on a 409 with current_version', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(409, { error: 'next epoch', current_version: 3 }),
      });
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => {});

      const attempt = e2eeService.rotateChannelKey(CONV, 2, new Map([['u1', pubKeyBase64]]));
      await expect(attempt).rejects.toBeInstanceOf(E2EEEpochClaimStaleError);
      await expect(attempt).rejects.toMatchObject({ currentVersion: 3 });
      debug.mockRestore();
    });

    it('keeps the generic failure for a 409 that names no epoch', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(409, { error: 'conflict' }),
      });
      const debug = vi.spyOn(console, 'debug').mockImplementation(() => {});

      await expect(
        e2eeService.rotateChannelKey(CONV, 2, new Map([['u1', pubKeyBase64]]))
      ).rejects.toThrow('channel key rotation distribution failed');
      debug.mockRestore();
    });
  });

  describe('missing-key backoff', () => {
    it('replays a NO_KEY_YET refusal without a second request inside the window', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(404, { code: 'NO_KEY_YET', pending: true }),
        [`/api/v1/e2ee/keys/${CONV}/rewrap`]: () => jsonResponse(200, {}),
      });

      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
        code: 'NO_KEY_YET',
        pending: true,
      });
      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
        code: 'NO_KEY_YET',
        pending: true,
      });
      expect(callsTo(KEYS_PATH)).toHaveLength(1);
    });

    it('is cleared by invalidateChannelKey, which every key-arrival path calls', async () => {
      routes({
        [KEYS_PATH]: () => jsonResponse(404, { code: 'NO_KEY_YET', pending: true }),
        [`/api/v1/e2ee/keys/${CONV}/rewrap`]: () => jsonResponse(200, {}),
      });

      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toBeInstanceOf(
        E2EEKeyUnavailableError
      );
      e2eeService.invalidateChannelKey(CONV);
      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toBeInstanceOf(
        E2EEKeyUnavailableError
      );
      expect(callsTo(KEYS_PATH)).toHaveLength(2);
    });

    it('does not record a miss from a fetch that invalidation already superseded', async () => {
      let resolveStale: (res: Response) => void = () => {};
      const stale = new Promise<Response>((resolve) => {
        resolveStale = resolve;
      });
      let keysCalls = 0;
      mockApiFetch.mockImplementation((path: string) => {
        if (path === KEYS_PATH) {
          keysCalls += 1;
          return keysCalls === 1
            ? stale
            : Promise.resolve(
                jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 2 } })
              );
        }
        return Promise.resolve(jsonResponse(200, {}));
      });

      const staleFetch = e2eeService.getChannelKeyMaterial(CONV).catch((err) => err);
      e2eeService.invalidateChannelKey(CONV);
      const freshFetch = e2eeService.getChannelKeyMaterial(CONV);
      resolveStale(jsonResponse(404, { code: 'NO_KEY_YET', pending: true }));
      await expect(staleFetch).resolves.toBeInstanceOf(E2EEKeyUnavailableError);

      // The refusal belonged to the superseded fetch: nothing is backed off,
      // and a third caller joins the replacement instead of being refused.
      await expect(e2eeService.getChannelKeyMaterial(CONV)).resolves.toMatchObject({
        keyVersion: 2,
      });
      await expect(freshFetch).resolves.toMatchObject({ keyVersion: 2 });
      expect(keysCalls).toBe(2);
    });

    it('does not cover a transport failure', async () => {
      mockApiFetch.mockRejectedValue(new Error('offline'));

      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toThrow('offline');
      await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toThrow('offline');
      expect(callsTo(KEYS_PATH)).toHaveLength(2);
    });

    it('cues the rotation coordinator once per REVOKED_EPOCH refusal, at the next epoch this device saw', async () => {
      const events: Array<{ channelId: string; newEpoch: number; reason: string }> = [];
      const listener = (event: Event) => events.push((event as CustomEvent).detail);
      globalThis.addEventListener('e2ee-key-rotation', listener);
      try {
        routes({
          [KEYS_PATH]: () =>
            jsonResponse(200, { key: { wrapped_key: wrappedCurrent, key_version: 3 } }),
        });
        await e2eeService.getChannelKeyMaterial(CONV);
        expect(e2eeService.getHighestSeenKeyVersion(CONV)).toBe(3);

        // The peer rotated; this device's epoch is now refused.
        e2eeService.invalidateChannelKey(CONV);
        routes({
          [KEYS_PATH]: () => jsonResponse(404, { code: 'REVOKED_EPOCH' }),
        });
        await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
          code: 'REVOKED_EPOCH',
        });
        // Replayed from the backoff: same answer, no second cue.
        await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
          code: 'REVOKED_EPOCH',
        });

        expect(events).toEqual([{ channelId: CONV, newEpoch: 4, reason: 'revoked_epoch' }]);
      } finally {
        globalThis.removeEventListener('e2ee-key-rotation', listener);
      }
    });

    it('claims the successor the refusal names, even on a device that saw no epoch', async () => {
      const events: Array<{ newEpoch: number }> = [];
      const listener = (event: Event) => events.push((event as CustomEvent).detail);
      globalThis.addEventListener('e2ee-key-rotation', listener);
      try {
        routes({
          [KEYS_PATH]: () => jsonResponse(404, { code: 'REVOKED_EPOCH', successor_epoch: 5 }),
        });
        await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
          code: 'REVOKED_EPOCH',
          successorEpoch: 5,
        });
        expect(events).toEqual([expect.objectContaining({ newEpoch: 5 })]);
      } finally {
        globalThis.removeEventListener('e2ee-key-rotation', listener);
      }
    });

    // A guess at or below the current epoch is a rewrap the server answers
    // 200, which would clear this backoff and repeat every window: with no
    // epoch known from either side there is nothing sound to claim.
    it('does not cue when neither the refusal nor this device names an epoch', async () => {
      const events: unknown[] = [];
      const listener = (event: Event) => events.push((event as CustomEvent).detail);
      globalThis.addEventListener('e2ee-key-rotation', listener);
      try {
        routes({
          [KEYS_PATH]: () => jsonResponse(404, { code: 'REVOKED_EPOCH' }),
        });
        await expect(e2eeService.getChannelKeyMaterial(CONV)).rejects.toMatchObject({
          code: 'REVOKED_EPOCH',
        });
        expect(events).toEqual([]);
      } finally {
        globalThis.removeEventListener('e2ee-key-rotation', listener);
      }
    });
  });
});
