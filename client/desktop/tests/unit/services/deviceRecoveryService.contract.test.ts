import { beforeAll, afterAll, beforeEach, afterEach, describe, it, expect, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import vector from '../../../../../docs/design/trusted-recovery-v2-vectors.json';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import { mockUser } from '../../mocks/fixtures';

// P-384, HKDF, AES-GCM, HTTP, stores and selected-server lifecycle are real.
// Account-key custody/import uses narrow mocks; dedicated tests hold real
// WebCrypto export completion and stub only the Electron IPC bridge methods.
const custody = vi.hoisted(() => ({
  epoch: 0,
  validate: vi.fn<(bytes: ArrayBuffer) => Promise<void>>(),
  export: vi.fn<(wrapped: string, key: CryptoKey, guard: () => void) => Promise<ArrayBuffer>>(),
  wrapping: vi.fn(() => ({}) as CryptoKey),
  wrapped: vi.fn(() => 'synthetic-wrapped-account-key'),
}));
vi.mock('@/renderer/utils/crypto/crypto', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/utils/crypto/crypto')>()),
  validateRecoveryAccountKey: custody.validate,
  exportRecoveryAccountKey: custody.export,
}));
vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    captureTeardownEpoch: () => custody.epoch,
    wasTornDownSince: (epoch: number) => epoch !== custody.epoch,
    getWrappingKey: custody.wrapping,
    getWrappedPrivateKey: custody.wrapped,
  },
}));

import {
  RequesterDeviceRecoveryAttempt,
  ResponderDeviceRecoveryAttempt,
  listDeviceRecoveryRequests,
  rejectDeviceRecoveryRequest,
  type DeviceRecoveryView,
} from '@/renderer/services/system/deviceRecoveryService';
import type {
  DeviceRecoveryContext,
  DeviceRecoveryRequest,
  DeviceRecoveryCreateBody,
  DeviceRecoveryOffer,
  DeviceRecoveryRespondBody,
} from '@/renderer/services/system/deviceRecoveryContract';
import {
  deriveDeviceRecoveryKeys,
  encryptDeviceRecoveryPayload,
  decryptDeviceRecoveryPayload,
  RECOVERY_UPDATE_GUIDANCE,
  recoveryTranscriptHash,
} from '@/renderer/utils/crypto/trustedDeviceRecovery';
import { base64ToArrayBuffer, arrayBufferToBase64 } from '@/renderer/utils/crypto/crypto';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useUserStore } from '@/renderer/stores/auth/userStore';
import { setRuntimeServerBase } from '@/renderer/services/system/runtimeServerBase';
import { _resetRefreshState } from '@/renderer/services/system/apiClient';
import { _resetClientVersionCache } from '@/renderer/utils/runtime/clientVersion';

const server = setupServer();
const originalElectronBridge = globalThis.electron;
const base = vector.context.server_origin;
const fixtureContext: DeviceRecoveryContext = { ...vector.context, protocol_version: 2 };
const beganAt = vector.context.expires_at - 60_000;
const id = vector.context.request_id;
const createURL = `${base}/api/v1/auth/recovery/device-request`;
const pollURL = `${createURL}/${id}`;
const completeURL = `${pollURL}/complete`;
const listURL = `${base}/api/v1/mfa/recovery-requests`;
const respondURL = `${listURL}/${id}/respond`;
const plaintext = () => base64ToArrayBuffer(vector.plaintext);
function recoveryToken(expiresAt: number): string {
  return `synthetic.${btoa(
    JSON.stringify({
      user_id: vector.user_id,
      jti: vector.recovery_token_jti,
      exp: expiresAt / 1000,
    })
  )
    .replaceAll('+', '-')
    .replaceAll('/', '_')
    .replaceAll('=', '')}.synthetic`;
}
const token = recoveryToken(vector.context.expires_at);
const longRecoveryToken = recoveryToken(beganAt + 25 * 60 * 60_000);
const attempts: Array<{ dispose(): void }> = [];
let responderPrivate: CryptoKey;
let requesterPrivate: CryptoKey;

function observe() {
  const views: DeviceRecoveryView[] = [];
  const waiters: Array<{ predicate: (view: DeviceRecoveryView) => boolean; resolve: () => void }> =
    [];
  const onView = (view: DeviceRecoveryView) => {
    views.push(view);
    for (const waiter of [...waiters])
      if (waiter.predicate(view)) {
        waiters.splice(waiters.indexOf(waiter), 1);
        waiter.resolve();
      }
  };
  return {
    views,
    onView,
    latest: () => views[views.length - 1],
    next: (predicate: (view: DeviceRecoveryView) => boolean) =>
      new Promise<void>((resolve) => waiters.push({ predicate, resolve })),
  };
}
function requester(recoveringToken = token) {
  const seen = observe();
  const attempt = new RequesterDeviceRecoveryAttempt(recoveringToken, seen.onView);
  attempts.push(attempt);
  return { attempt, seen };
}
function pending(context: DeviceRecoveryContext = fixtureContext): DeviceRecoveryRequest {
  return { ...context, status: 'pending' };
}
function terminal(status: 'complete' | 'expired' | 'rejected' = 'complete') {
  return { request_id: id, protocol_version: 2, status, expires_at: vector.context.expires_at };
}
function acknowledgement(status: 'complete' | 'offered' | 'approved' | 'rejected') {
  return { request_id: id, protocol_version: 2, status };
}
function installCreate(
  options: {
    token?: string;
    expiresAt?: () => number;
    beforeCreate?: () => Promise<void>;
    dateHeader?: string;
  } = {}
) {
  let context: DeviceRecoveryContext | undefined;
  server.use(
    http.post(createURL, async ({ request }) => {
      const body = (await request.json()) as DeviceRecoveryCreateBody;
      expect(request.headers.get('Authorization')).toBe(`Bearer ${options.token ?? token}`);
      await options.beforeCreate?.();
      context = {
        request_id: id,
        protocol_version: 2,
        server_origin: body.server_origin,
        account_binding: body.account_binding,
        expires_at: options.expiresAt?.() ?? vector.context.expires_at,
        requester_nonce: body.requester_nonce,
        requester_public_key: body.requester_public_key,
        recovery_token_jti_hash: vector.context.recovery_token_jti_hash,
      };
      return HttpResponse.json(pending(context), {
        headers: options.dateHeader ? { Date: options.dateHeader } : undefined,
      });
    })
  );
  return () => {
    if (!context) throw new Error('Create must settle before reading its context.');
    return context;
  };
}
async function approval(context: DeviceRecoveryContext) {
  const material = await deriveDeviceRecoveryKeys(
    'responder',
    responderPrivate,
    context,
    vector.offer
  );
  const offer: DeviceRecoveryOffer = { ...vector.offer, transcript_hash: material.transcriptHash };
  return {
    ...context,
    ...offer,
    status: 'approved' as const,
    encrypted_payload: await encryptDeviceRecoveryPayload(material, plaintext()),
  };
}
async function approvedRequester(corrupt = false) {
  const context = installCreate();
  const result = requester();
  await result.attempt.start();
  expect(result.seen.latest().status).toBe('pending');
  const row = await approval(context());
  if (corrupt) {
    const bytes = new Uint8Array(base64ToArrayBuffer(row.encrypted_payload));
    bytes[bytes.length - 1] ^= 1;
    row.encrypted_payload = arrayBufferToBase64(bytes.buffer);
  }
  server.use(http.get(pollURL, () => HttpResponse.json(row)));
  await result.attempt.poll();
  expect(result.seen.latest().status).toBe('approved-locked');
  return { ...result, row };
}
const invalidations: Array<[string, (attempt: { dispose(): void }) => void]> = [
  [
    'auth lifecycle',
    () => {
      useAuthStore.getState().beginAuthLifecycle('successor-token', null);
    },
  ],
  [
    'local user identity',
    () =>
      useUserStore.setState({
        user: { ...mockUser, id: '22222222-3333-4444-8555-666666666666' },
      }),
  ],
  ['selected server', () => setRuntimeServerBase('https://successor.example.test')],
  [
    'E2EE teardown',
    () => {
      custody.epoch += 1;
    },
  ],
  ['dispose', (attempt) => attempt.dispose()],
];

beforeAll(async () => {
  server.listen({ onUnhandledRequest: 'error' });
  const raw = new Uint8Array(base64ToArrayBuffer(vector.offer.responder_public_key));
  const url64 = (bytes: Uint8Array) =>
    arrayBufferToBase64(bytes.slice().buffer)
      .replaceAll('+', '-')
      .replaceAll('/', '_')
      .replaceAll('=', '');
  requesterPrivate = await crypto.subtle.importKey(
    'jwk',
    {
      kty: 'EC',
      crv: 'P-384',
      x: url64(
        new Uint8Array(base64ToArrayBuffer(vector.context.requester_public_key)).slice(1, 49)
      ),
      y: url64(new Uint8Array(base64ToArrayBuffer(vector.context.requester_public_key)).slice(49)),
      d: vector.requester_private_scalar,
      ext: true,
    },
    { name: 'ECDH', namedCurve: 'P-384' },
    false,
    ['deriveBits']
  );
  responderPrivate = await crypto.subtle.importKey(
    'jwk',
    {
      kty: 'EC',
      crv: 'P-384',
      x: url64(raw.slice(1, 49)),
      y: url64(raw.slice(49)),
      d: vector.responder_private_scalar,
      ext: true,
    },
    { name: 'ECDH', namedCurve: 'P-384' },
    false,
    ['deriveBits']
  );
});
afterAll(() => server.close());
beforeEach(() => {
  resetAllStores();
  _resetRefreshState();
  _resetClientVersionCache();
  vi.useFakeTimers({
    toFake: ['Date', 'setTimeout', 'clearTimeout', 'setInterval', 'clearInterval'],
  });
  vi.setSystemTime(beganAt);
  setRuntimeServerBase(base);
  custody.epoch = 0;
  custody.validate.mockReset().mockResolvedValue(undefined);
  custody.export.mockReset().mockImplementation(async () => plaintext());
  custody.wrapping.mockClear();
  custody.wrapped.mockClear();
  useAuthStore.getState().setAccessToken('synthetic-access-token');
  useUserStore.setState({ user: { ...mockUser, id: vector.user_id } });
});
afterEach(() => {
  attempts.splice(0).forEach((attempt) => attempt.dispose());
  server.resetHandlers();
  (globalThis as unknown as { electron: unknown }).electron = originalElectronBridge;
  _resetClientVersionCache();
  vi.useRealTimers();
});

describe('requester creation with normal recovery-token lifetime', () => {
  it('accepts a full fifteen-minute server lifetime after delayed server creation with a 25-hour JWT', async () => {
    const entered = deferred();
    const release = deferred();
    const context = installCreate({
      token: longRecoveryToken,
      expiresAt: () => Date.now() + 15 * 60_000,
      beforeCreate: async () => {
        entered.resolve();
        await release.promise;
      },
    });
    const { attempt, seen } = requester(longRecoveryToken);
    const starting = attempt.start();
    await entered.promise;
    await vi.advanceTimersByTimeAsync(3000);
    release.resolve();
    await starting;
    expect(context().expires_at).toBe(beganAt + 3000 + 15 * 60_000);
    expect(seen.latest()).toMatchObject({ status: 'pending', error: '' });
    server.use(http.get(pollURL, () => HttpResponse.json(pending(context()))));
    vi.setSystemTime(beganAt + 15 * 60_000);
    await attempt.poll();
    expect(seen.latest()).toMatchObject({ status: 'pending', error: '' });
    vi.setSystemTime(context().expires_at);
    await attempt.poll();
    expect(seen.latest().error).toMatch(/expired/);
  });

  it('accepts a full server lifetime after a held real WebCrypto public-key export with a 25-hour JWT', async () => {
    const entered = deferred();
    const release = deferred();
    const exportKey = crypto.subtle.exportKey.bind(crypto.subtle);
    const spy = vi.spyOn(crypto.subtle, 'exportKey').mockImplementation(async (format, key) => {
      const exported = await exportKey(format, key);
      if (format === 'raw' && key.algorithm.name === 'ECDH') {
        entered.resolve();
        await release.promise;
      }
      return exported;
    });
    try {
      const context = installCreate({
        token: longRecoveryToken,
        expiresAt: () => Date.now() + 15 * 60_000,
      });
      const { attempt, seen } = requester(longRecoveryToken);
      const starting = attempt.start();
      await entered.promise;
      await vi.advanceTimersByTimeAsync(4000);
      release.resolve();
      await starting;
      expect(context().expires_at).toBe(beganAt + 4000 + 15 * 60_000);
      expect(seen.latest()).toMatchObject({ status: 'pending', error: '' });
    } finally {
      spy.mockRestore();
    }
  });

  it('rejects a create expiry beyond the captured JWT even when it is within fifteen minutes', async () => {
    installCreate({ expiresAt: () => vector.context.expires_at + 1 });
    const { attempt, seen } = requester();
    await attempt.start();
    expect(seen.latest()).toMatchObject({ status: 'error', error: RECOVERY_UPDATE_GUIDANCE });
    expect(custody.validate).not.toHaveBeenCalled();
  });

  it('rejects a create expiry beyond fifteen minutes plus the two-minute clock allowance with a 25-hour JWT', async () => {
    installCreate({ token: longRecoveryToken, expiresAt: () => Date.now() + 17 * 60_000 + 1 });
    const { attempt, seen } = requester(longRecoveryToken);
    await attempt.start();
    expect(seen.latest()).toMatchObject({ status: 'error', error: RECOVERY_UPDATE_GUIDANCE });
    expect(custody.validate).not.toHaveBeenCalled();
  });

  it('pins the accepted absolute deadline instead of accepting a later poll extension', async () => {
    const context = installCreate({
      token: longRecoveryToken,
      expiresAt: () => Date.now() + 15 * 60_000,
    });
    const { attempt, seen } = requester(longRecoveryToken);
    await attempt.start();
    expect(seen.latest().status).toBe('pending');
    await vi.advanceTimersByTimeAsync(1000);
    server.use(
      http.get(pollURL, () =>
        HttpResponse.json(pending({ ...context(), expires_at: context().expires_at + 1 }))
      )
    );
    await attempt.poll();
    expect(seen.latest()).toMatchObject({ status: 'error', error: RECOVERY_UPDATE_GUIDANCE });
    expect(custody.validate).not.toHaveBeenCalled();
  });
});

describe('bounded recovery clock skew', () => {
  it.each([-120_000, -60_000, 60_000, 120_000])(
    'accepts an honest create expiry when the desktop clock differs from the server by %i ms',
    async (localOffset) => {
      vi.setSystemTime(beganAt + localOffset);
      const context = installCreate({
        token: longRecoveryToken,
        expiresAt: () => beganAt + 15 * 60_000,
        dateHeader: new Date(beganAt - 24 * 60 * 60_000).toUTCString(),
      });
      const { attempt, seen } = requester(longRecoveryToken);
      await attempt.start();
      expect(seen.latest()).toMatchObject({ status: 'pending', error: '' });
      expect(context().expires_at).toBe(beganAt + 15 * 60_000);
      server.use(http.get(pollURL, () => HttpResponse.json(pending(context()))));
      await attempt.poll();
      expect(seen.latest()).toMatchObject({ status: 'pending', error: '' });
    }
  );

  it.each([-120_000, -60_000, 60_000, 120_000])(
    'lists an honest expiry when the desktop clock differs from the server by %i ms',
    async (localOffset) => {
      vi.setSystemTime(beganAt + localOffset);
      const row = pending({ ...fixtureContext, expires_at: beganAt + 15 * 60_000 });
      server.use(
        http.get(listURL, () =>
          HttpResponse.json(
            { requests: [row] },
            { headers: { Date: new Date(beganAt + 24 * 60 * 60_000).toUTCString() } }
          )
        )
      );
      await expect(listDeviceRecoveryRequests(vector.user_id, () => {})).resolves.toEqual([row]);
    }
  );

  it('keeps the captured JWT cap inside the accepted clock allowance', async () => {
    const shortToken = recoveryToken(beganAt + 16 * 60_000);
    installCreate({ token: shortToken, expiresAt: () => beganAt + 16 * 60_000 + 1 });
    const { attempt, seen } = requester(shortToken);
    await attempt.start();
    expect(seen.latest()).toMatchObject({ status: 'error', error: RECOVERY_UPDATE_GUIDANCE });
    expect(custody.validate).not.toHaveBeenCalled();
  });

  it('cannot enlarge the create allowance using an untrusted future Date header', async () => {
    installCreate({
      token: longRecoveryToken,
      expiresAt: () => beganAt + 17 * 60_000 + 1,
      dateHeader: new Date(beganAt + 24 * 60 * 60_000).toUTCString(),
    });
    const { attempt, seen } = requester(longRecoveryToken);
    await attempt.start();
    expect(seen.latest()).toMatchObject({ status: 'error', error: RECOVERY_UPDATE_GUIDANCE });
  });

  it('cannot enlarge the list allowance using an untrusted future Date header', async () => {
    server.use(
      http.get(listURL, () =>
        HttpResponse.json(
          { requests: [pending({ ...fixtureContext, expires_at: beganAt + 17 * 60_000 + 1 })] },
          { headers: { Date: new Date(beganAt + 24 * 60 * 60_000).toUTCString() } }
        )
      )
    );
    await expect(listDeviceRecoveryRequests(vector.user_id, () => {})).rejects.toThrow(
      RECOVERY_UPDATE_GUIDANCE
    );
  });

  it('uses one list receipt bound even when real public-key validation finishes later', async () => {
    server.use(
      http.get(listURL, () =>
        HttpResponse.json({
          requests: [pending({ ...fixtureContext, expires_at: beganAt + 17 * 60_000 + 1 })],
        })
      )
    );
    const entered = deferred();
    const release = deferred();
    const importKey = crypto.subtle.importKey.bind(crypto.subtle);
    let held = false;
    const spy = vi
      .spyOn(crypto.subtle, 'importKey')
      .mockImplementation(async (format, keyData, algorithm, extractable, usages) => {
        const imported = await importKey(format, keyData, algorithm, extractable, usages);
        if (
          !held &&
          format === 'raw' &&
          typeof algorithm !== 'string' &&
          algorithm.name === 'ECDH'
        ) {
          held = true;
          entered.resolve();
          await release.promise;
        }
        return imported;
      });
    try {
      const listing = listDeviceRecoveryRequests(vector.user_id, () => {});
      const rejected = expect(listing).rejects.toThrow(RECOVERY_UPDATE_GUIDANCE);
      await entered.promise;
      vi.setSystemTime(beganAt + 2000);
      release.resolve();
      await rejected;
    } finally {
      release.resolve();
      spy.mockRestore();
    }
  });

  it('pins requester transcript expiry while repeated polls cannot renew its fifteen-minute local deadline', async () => {
    const context = installCreate({
      token: longRecoveryToken,
      expiresAt: () => beganAt + 17 * 60_000,
    });
    const { attempt, seen } = requester(longRecoveryToken);
    await attempt.start();
    expect(seen.latest().status).toBe('pending');
    const row = await approval(context());
    let polls = 0;
    server.use(
      http.get(pollURL, () => {
        polls += 1;
        return HttpResponse.json(row);
      })
    );
    await attempt.poll();
    expect(seen.latest()).toMatchObject({ status: 'approved-locked', error: '' });
    const fingerprint = seen.latest().fingerprint;
    expect(fingerprint).not.toBe('');
    for (const elapsed of [60_000, 7 * 60_000, 15 * 60_000 - 1]) {
      vi.setSystemTime(beganAt + elapsed);
      await attempt.poll();
      expect(seen.latest()).toMatchObject({ status: 'approved-locked', fingerprint, error: '' });
      expect(row.expires_at).toBe(beganAt + 17 * 60_000);
    }
    expect(polls).toBe(4);
    vi.setSystemTime(beganAt + 15 * 60_000);
    await attempt.poll();
    expect(polls).toBe(4);
    expect(seen.latest()).toMatchObject({ status: 'error', fingerprint: '' });
    expect(seen.latest().error).toMatch(/expired/);
    expect(custody.validate).not.toHaveBeenCalled();
  });

  it.each([
    ['before', 15 * 60_000 - 1, true],
    ['at', 15 * 60_000, false],
  ] as const)(
    'held responder export %s its fifteen-minute local deadline obeys that deadline instead of the later wire expiry',
    async (_name, elapsed, positive) => {
      const context = { ...fixtureContext, expires_at: beganAt + 17 * 60_000 };
      let offered: DeviceRecoveryOffer | undefined;
      let approvals = 0;
      server.use(
        http.post(respondURL, async ({ request }) => {
          const body = (await request.json()) as DeviceRecoveryRespondBody;
          if (body.action === 'offer') {
            offered = {
              responder_public_key: body.responder_public_key,
              responder_nonce: body.responder_nonce,
              transcript_hash: body.transcript_hash,
            };
            return HttpResponse.json(acknowledgement('offered'));
          }
          expect(body.action).toBe('approve');
          approvals += 1;
          return HttpResponse.json(acknowledgement('approved'));
        })
      );
      const seen = observe();
      const attempt = new ResponderDeviceRecoveryAttempt(
        { ...context, status: 'pending' },
        seen.onView
      );
      attempts.push(attempt);
      await attempt.start();
      expect(seen.latest().status).toBe('offered');
      if (!offered) throw new Error('Expected a published offer');
      expect(offered.transcript_hash).toBe(await recoveryTranscriptHash(context, offered));
      const entered = deferred();
      const release = deferred();
      const exported = plaintext();
      custody.export.mockImplementation(async () => {
        entered.resolve();
        await release.promise;
        return exported;
      });
      const confirming = attempt.confirmMatch();
      await entered.promise;
      vi.setSystemTime(beganAt + elapsed);
      release.resolve();
      expect(await confirming).toBe(positive);
      expect(approvals).toBe(positive ? 1 : 0);
      expect(seen.views.some((view) => view.status === 'submitted')).toBe(positive);
      expect(new Uint8Array(exported).every((byte) => byte === 0)).toBe(true);
    }
  );
});

describe('requester completion and fixed lifetime', () => {
  it('hands off decrypted bytes only after exact completion acknowledgement, then wipes on dispose', async () => {
    const { attempt, seen, row } = await approvedRequester();
    const entered = deferred();
    const release = deferred();
    server.use(
      http.post(completeURL, async ({ request }) => {
        expect(request.headers.get('Authorization')).toBe(`Bearer ${token}`);
        expect(await request.json()).toEqual({
          protocol_version: 2,
          transcript_hash: row.transcript_hash,
        });
        entered.resolve();
        await release.promise;
        return HttpResponse.json(acknowledgement('complete'));
      })
    );
    const confirming = attempt.confirmMatch();
    await entered.promise;
    expect(seen.latest().status).toBe('completing');
    expect(() => attempt.recoveredAccountKey()).toThrow(RECOVERY_UPDATE_GUIDANCE);
    const imported = custody.validate.mock.calls[0][0];
    expect(new Uint8Array(imported)).toEqual(new Uint8Array(plaintext()));
    release.resolve();
    await confirming;
    expect(seen.latest().status).toBe('complete');
    expect(attempt.recoveredAccountKey()).toBe(imported);
    attempt.dispose();
    expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
    expect(() => attempt.recoveredAccountKey()).toThrow();
  });
  it.each([503, 409])(
    'reconciles ambiguous %s completion only with this attempt’s already imported key',
    async (status) => {
      const { attempt, seen } = await approvedRequester();
      server.use(http.post(completeURL, () => HttpResponse.json({}, { status })));
      await attempt.confirmMatch();
      expect(seen.latest().status).toBe('completing');
      const imported = custody.validate.mock.calls[0][0];
      expect(() => attempt.recoveredAccountKey()).toThrow();
      server.use(http.get(pollURL, () => HttpResponse.json(terminal())));
      const completed = seen.next((view) => view.status === 'complete');
      await vi.advanceTimersByTimeAsync(3000);
      await completed;
      expect(attempt.recoveredAccountKey()).toBe(imported);
      expect(custody.validate).toHaveBeenCalledTimes(1);
    }
  );
  it('does not redispatch completion while its Retry-After delay is active', async () => {
    const { attempt, seen } = await approvedRequester();
    let completions = 0;
    server.use(
      http.post(completeURL, () => {
        completions += 1;
        return HttpResponse.json({}, { status: 429, headers: { 'Retry-After': '7' } });
      })
    );
    await attempt.confirmMatch();
    expect(seen.latest()).toMatchObject({ status: 'completing', retryAt: beganAt + 7000 });
    await attempt.confirmMatch();
    expect(completions).toBe(1);
    expect(seen.latest().retryAt).toBe(beganAt + 7000);
  });

  it('server complete alone never supplies plaintext or advances this local attempt', async () => {
    installCreate();
    const { attempt, seen } = requester();
    await attempt.start();
    server.use(http.get(pollURL, () => HttpResponse.json(terminal())));
    await attempt.poll();
    expect(seen.latest()).toMatchObject({
      status: 'error',
      error: RECOVERY_UPDATE_GUIDANCE,
      fingerprint: '',
    });
    expect(custody.validate).not.toHaveBeenCalled();
    expect(() => attempt.recoveredAccountKey()).toThrow();
  });
  it('the original absolute deadline forbids plaintext handoff even after completion', async () => {
    const { attempt, seen } = await approvedRequester();
    server.use(http.post(completeURL, () => HttpResponse.json(acknowledgement('complete'))));
    await attempt.confirmMatch();
    const imported = attempt.recoveredAccountKey();
    vi.setSystemTime(vector.context.expires_at);
    expect(() => attempt.recoveredAccountKey()).toThrow(/expired/);
    await vi.advanceTimersByTimeAsync(1000);
    expect(seen.latest()).toMatchObject({ status: 'error', fingerprint: '', confirmed: false });
    expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
  });
  it('expiry during account-key import wipes bytes and prevents completion dispatch', async () => {
    const { attempt, seen } = await approvedRequester();
    const entered = deferred();
    const release = deferred();
    custody.validate.mockImplementation(async () => {
      entered.resolve();
      await release.promise;
    });
    let completions = 0;
    server.use(
      http.post(completeURL, () => {
        completions += 1;
        return HttpResponse.json(acknowledgement('complete'));
      })
    );
    const confirming = attempt.confirmMatch();
    await entered.promise;
    const imported = custody.validate.mock.calls[0][0];
    vi.setSystemTime(vector.context.expires_at);
    release.resolve();
    await confirming;
    expect(seen.latest().error).toMatch(/expired/);
    expect(completions).toBe(0);
    expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
  });
  it('expiry while completion acknowledgement is in flight cannot extend the attempt', async () => {
    const { attempt, seen } = await approvedRequester();
    const entered = deferred();
    const release = deferred();
    server.use(
      http.post(completeURL, async () => {
        entered.resolve();
        await release.promise;
        return HttpResponse.json(acknowledgement('complete'));
      })
    );
    const confirming = attempt.confirmMatch();
    await entered.promise;
    const imported = custody.validate.mock.calls[0][0];
    vi.setSystemTime(vector.context.expires_at);
    release.resolve();
    await confirming;
    expect(seen.latest().error).toMatch(/expired/);
    expect(() => attempt.recoveredAccountKey()).toThrow();
    expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
  });
});

describe('polling and transport recovery', () => {
  it('keeps one HTTP poll active and waits three seconds after it settles', async () => {
    const context = installCreate();
    const { attempt, seen } = requester();
    await attempt.start();
    const entered = deferred();
    const release = deferred();
    let calls = 0;
    server.use(
      http.get(pollURL, async () => {
        calls += 1;
        entered.resolve();
        await release.promise;
        return HttpResponse.json(pending(context()));
      })
    );
    const polling = attempt.poll();
    await entered.promise;
    await attempt.poll();
    await vi.advanceTimersByTimeAsync(6000);
    expect(calls).toBe(1);
    release.resolve();
    await polling;
    await vi.advanceTimersByTimeAsync(2999);
    expect(calls).toBe(1);
    const settled = seen.next((view) => view.status === 'pending' && !view.error);
    await vi.advanceTimersByTimeAsync(1);
    await settled;
    expect(calls).toBe(2);
  });
  it.each([
    ['7', 7000],
    [new Date(beganAt + 8000).toUTCString(), 8000],
  ])(
    'honors Retry-After %s before polling again without abandoning the attempt',
    async (header, delay) => {
      const context = installCreate();
      const { attempt, seen } = requester();
      await attempt.start();
      let calls = 0;
      server.use(
        http.get(pollURL, () => {
          calls += 1;
          return calls === 1
            ? HttpResponse.json({}, { status: 429, headers: { 'Retry-After': header } })
            : HttpResponse.json(pending(context()));
        })
      );
      await attempt.poll();
      expect(seen.latest()).toMatchObject({ status: 'pending', retryAt: beganAt + delay });
      await attempt.poll();
      expect(calls).toBe(1);
      await vi.advanceTimersByTimeAsync(delay - 1);
      expect(calls).toBe(1);
      const settled = seen.next((view) => view.status === 'pending' && !view.error);
      await vi.advanceTimersByTimeAsync(1);
      await settled;
      expect(calls).toBe(2);
      expect(seen.latest().retryAt).toBe(0);
    }
  );
  it('poll-driven completion preserves a seven-second retry deadline instead of polling at three seconds', async () => {
    const context = installCreate();
    const { attempt, seen } = requester();
    await attempt.start();
    const row = await approval(context());
    server.use(
      http.get(pollURL, () =>
        HttpResponse.json({
          ...context(),
          ...vector.offer,
          transcript_hash: row.transcript_hash,
          status: 'offered',
        })
      )
    );
    await attempt.poll();
    await attempt.confirmMatch();
    let polls = 0;
    server.use(
      http.get(pollURL, () => {
        polls += 1;
        return HttpResponse.json(polls === 1 ? row : terminal());
      }),
      http.post(completeURL, () =>
        HttpResponse.json(
          {},
          {
            status: 429,
            headers: { 'Retry-After': '7' },
          }
        )
      )
    );
    await attempt.poll();
    expect(seen.latest()).toMatchObject({ status: 'completing', retryAt: beganAt + 7000 });
    await vi.advanceTimersByTimeAsync(6999);
    expect(polls).toBe(1);
    const completed = seen.next((view) => view.status === 'complete');
    await vi.advanceTimersByTimeAsync(1);
    await completed;
    expect(polls).toBe(2);
    expect(new Uint8Array(attempt.recoveredAccountKey())).toEqual(new Uint8Array(plaintext()));
  });

  it('preserves pinned fingerprint and consent across ordinary poll transport failure', async () => {
    const context = installCreate();
    const { attempt, seen } = requester();
    await attempt.start();
    const row = await approval(context());
    const offered = {
      ...context(),
      ...vector.offer,
      transcript_hash: row.transcript_hash,
      status: 'offered',
    };
    server.use(http.get(pollURL, () => HttpResponse.json(offered)));
    await attempt.poll();
    await attempt.confirmMatch();
    const fingerprint = seen.latest().fingerprint;
    server.use(http.get(pollURL, () => HttpResponse.error()));
    await attempt.poll();
    expect(seen.latest()).toMatchObject({
      status: 'offered',
      fingerprint,
      confirmed: true,
      retryAt: beganAt + 3000,
    });
    server.use(
      http.get(pollURL, () => HttpResponse.json(row)),
      http.post(completeURL, () => HttpResponse.json(acknowledgement('complete')))
    );
    const completed = seen.next((view) => view.status === 'complete');
    await vi.advanceTimersByTimeAsync(3000);
    await completed;
    expect(new Uint8Array(attempt.recoveredAccountKey())).toEqual(new Uint8Array(plaintext()));
  });
  it('a changed remote context clears the pinned fingerprint and consent instead of retaining a retry', async () => {
    const context = installCreate();
    const { attempt, seen } = requester();
    await attempt.start();
    const row = await approval(context());
    const offered = {
      ...context(),
      ...vector.offer,
      transcript_hash: row.transcript_hash,
      status: 'offered',
    };
    server.use(http.get(pollURL, () => HttpResponse.json(offered)));
    await attempt.poll();
    await attempt.confirmMatch();
    expect(seen.latest().confirmed).toBe(true);
    expect(seen.latest().fingerprint).not.toBe('');
    server.use(
      http.get(pollURL, () =>
        HttpResponse.json({
          ...offered,
          account_binding: vector.context.recovery_token_jti_hash,
        })
      )
    );
    await attempt.poll();
    expect(seen.latest()).toMatchObject({
      status: 'error',
      error: RECOVERY_UPDATE_GUIDANCE,
      fingerprint: '',
      confirmed: false,
      retryAt: 0,
    });
    expect(custody.validate).not.toHaveBeenCalled();
    expect(() => attempt.recoveredAccountKey()).toThrow();
  });

  it('real AES-GCM authentication failure clears consent and never imports or acknowledges', async () => {
    const { attempt, seen } = await approvedRequester(true);
    let completions = 0;
    server.use(
      http.post(completeURL, () => {
        completions += 1;
        return HttpResponse.json(acknowledgement('complete'));
      })
    );
    await attempt.confirmMatch();
    expect(seen.latest()).toMatchObject({
      status: 'error',
      error: RECOVERY_UPDATE_GUIDANCE,
      fingerprint: '',
      confirmed: false,
    });
    expect(custody.validate).not.toHaveBeenCalled();
    expect(completions).toBe(0);
  });
});

describe('responder transport completion', () => {
  it('publishes an authenticated envelope that the original requester can decrypt and wipes export bytes', async () => {
    let offer: DeviceRecoveryOffer | undefined;
    let published = '';
    server.use(
      http.post(respondURL, async ({ request }) => {
        const body = (await request.json()) as DeviceRecoveryRespondBody;
        if (body.action === 'offer') {
          offer = {
            responder_public_key: body.responder_public_key,
            responder_nonce: body.responder_nonce,
            transcript_hash: body.transcript_hash,
          };
          return HttpResponse.json(acknowledgement('offered'));
        }
        expect(body.action).toBe('approve');
        if (body.action !== 'approve' || !offer) throw new Error('Offer must precede approval');
        expect(body.transcript_hash).toBe(offer.transcript_hash);
        published = body.encrypted_payload;
        return HttpResponse.json(acknowledgement('approved'));
      })
    );
    const seen = observe();
    const attempt = new ResponderDeviceRecoveryAttempt(
      { ...fixtureContext, status: 'pending' },
      seen.onView
    );
    attempts.push(attempt);
    const exported = plaintext();
    custody.export.mockResolvedValue(exported);
    await attempt.start();
    expect(seen.latest().status).toBe('offered');
    expect(await attempt.confirmMatch()).toBe(true);
    expect(seen.latest()).toMatchObject({ status: 'submitted', fingerprint: '', confirmed: false });
    if (!offer) throw new Error('Expected a published offer');
    const material = await deriveDeviceRecoveryKeys(
      'requester',
      requesterPrivate,
      fixtureContext,
      offer
    );
    expect(new Uint8Array(await decryptDeviceRecoveryPayload(material, published))).toEqual(
      new Uint8Array(plaintext())
    );
    expect(new Uint8Array(exported).every((byte) => byte === 0)).toBe(true);
  });

  it('reconciles a lost offer acknowledgement against the owned immutable offer without replacing its key', async () => {
    let offer: DeviceRecoveryOffer | undefined;
    let posts = 0;
    let lists = 0;
    server.use(
      http.post(respondURL, async ({ request }) => {
        posts += 1;
        const body = (await request.json()) as DeviceRecoveryRespondBody;
        if (body.action !== 'offer') throw new Error('Expected offer only');
        offer = {
          responder_public_key: body.responder_public_key,
          responder_nonce: body.responder_nonce,
          transcript_hash: body.transcript_hash,
        };
        return HttpResponse.json({}, { status: 503 });
      }),
      http.get(listURL, () => {
        lists += 1;
        return HttpResponse.json({
          requests: [{ ...vector.context, ...offer, status: 'offered' }],
        });
      })
    );
    const seen = observe();
    const attempt = new ResponderDeviceRecoveryAttempt(
      { ...fixtureContext, status: 'pending' },
      seen.onView
    );
    attempts.push(attempt);
    await attempt.start();
    expect(seen.latest().retryAt).toBe(beganAt + 3000);
    await attempt.retryOffer();
    expect(lists).toBe(0);
    await vi.advanceTimersByTimeAsync(3000);
    await attempt.retryOffer();
    expect(seen.latest()).toMatchObject({ status: 'offered', retryAt: 0, error: '' });
    expect(posts).toBe(1);
    expect(lists).toBe(1);
    if (!offer) throw new Error('Expected a published offer');
    const material = await deriveDeviceRecoveryKeys(
      'requester',
      requesterPrivate,
      fixtureContext,
      offer
    );
    expect(seen.latest().fingerprint).toBe(material.fingerprint);
  });
});

describe('ambiguous responder approval reconciliation', () => {
  afterEach(() => vi.restoreAllMocks());

  async function ambiguousApproval() {
    const seen = observe();
    const events: string[] = [];
    const exports: ArrayBuffer[] = [];
    const encrypt = crypto.subtle.encrypt.bind(crypto.subtle);
    const encrypted = vi.spyOn(crypto.subtle, 'encrypt').mockImplementation(async (...args) => {
      events.push('encrypt');
      return encrypt(...args);
    });
    custody.export.mockImplementation(async () => {
      events.push('export');
      const bytes = plaintext();
      exports.push(bytes);
      return bytes;
    });
    let offered: DeviceRecoveryOffer | undefined;
    let approvals = 0;
    let allowRetryAcknowledgement = false;
    server.use(
      http.post(respondURL, async ({ request }) => {
        const body = (await request.json()) as DeviceRecoveryRespondBody;
        events.push(body.action);
        if (body.action === 'offer') {
          offered = {
            responder_public_key: body.responder_public_key,
            responder_nonce: body.responder_nonce,
            transcript_hash: body.transcript_hash,
          };
          return HttpResponse.json(acknowledgement('offered'));
        }
        if (body.action !== 'approve') throw new Error('Expected approval after the offer');
        approvals += 1;
        expect(body.transcript_hash).toBe(offered?.transcript_hash);
        if (approvals === 1) return HttpResponse.error();
        if (allowRetryAcknowledgement) return HttpResponse.json(acknowledgement('approved'));
        return HttpResponse.json({}, { status: 409 });
      })
    );
    const attempt = new ResponderDeviceRecoveryAttempt(pending(), seen.onView);
    attempts.push(attempt);
    await attempt.start();
    expect(seen.latest().status).toBe('offered');
    expect(custody.wrapping).not.toHaveBeenCalled();
    expect(await attempt.confirmMatch()).toBe(false);
    expect(seen.latest()).toMatchObject({
      status: 'offered',
      confirmed: false,
      retryAt: beganAt + 3000,
    });
    expect(events).toEqual(['offer', 'export', 'encrypt', 'approve']);
    expect(custody.wrapping).toHaveBeenCalledTimes(1);
    expect(custody.wrapped).toHaveBeenCalledTimes(1);
    expect(custody.export).toHaveBeenCalledTimes(1);
    expect(encrypted).toHaveBeenCalledTimes(1);
    expect(exports.every((bytes) => new Uint8Array(bytes).every((byte) => byte === 0))).toBe(true);
    if (!offered) throw new Error('Expected the retained real ephemeral offer');
    const row = { ...fixtureContext, ...offered, status: 'offered' as const };
    const noFurtherCustody = () => {
      expect(custody.wrapping).toHaveBeenCalledTimes(1);
      expect(custody.wrapped).toHaveBeenCalledTimes(1);
      expect(custody.export).toHaveBeenCalledTimes(1);
      expect(encrypted).toHaveBeenCalledTimes(1);
      expect(approvals).toBe(1);
      expect(seen.views.some((view) => view.status === 'submitted')).toBe(false);
    };
    return {
      attempt,
      seen,
      events,
      row,
      noFurtherCustody,
      exports,
      approvals: () => approvals,
      allowRetry: () => {
        allowRetryAcknowledgement = true;
      },
    };
  }

  it.each(['missing', 'pending', 'different context', 'different offer'] as const)(
    'restarts without further custody when the authoritative row is %s after a lost approval acknowledgement',
    async (change) => {
      const current = await ambiguousApproval();
      let lists = 0;
      let listed: DeviceRecoveryRequest[];
      if (change === 'missing') {
        listed = [];
      } else if (change === 'pending') {
        listed = [pending()];
      } else if (change === 'different context') {
        listed = [{ ...current.row, requester_nonce: vector.context.recovery_token_jti_hash }];
      } else {
        listed = [{ ...current.row, responder_nonce: vector.context.recovery_token_jti_hash }];
      }
      server.use(
        http.get(listURL, () => {
          lists += 1;
          current.events.push('list');
          current.noFurtherCustody();
          return HttpResponse.json({ requests: listed });
        })
      );
      await vi.advanceTimersByTimeAsync(3000);
      expect(await current.attempt.confirmMatch()).toBe(false);
      current.noFurtherCustody();
      expect(lists).toBe(1);
      expect(current.events).toEqual(['offer', 'export', 'encrypt', 'approve', 'list']);
      expect(current.seen.latest()).toMatchObject({
        status: 'error',
        fingerprint: '',
        confirmed: false,
      });
      expect(current.seen.latest().error).toMatch(/restart|new request|update both devices/i);
    }
  );

  it('requires another explicit match, the exact owned offer and its acknowledgement before a legitimate retry', async () => {
    const current = await ambiguousApproval();
    current.allowRetry();
    let lists = 0;
    server.use(
      http.get(listURL, ({ request }) => {
        expect(request.headers.get('Authorization')).toBe('Bearer synthetic-access-token');
        lists += 1;
        current.events.push('list');
        current.noFurtherCustody();
        return HttpResponse.json({ requests: [current.row] });
      })
    );
    expect(await current.attempt.confirmMatch()).toBe(false);
    expect(lists).toBe(0);
    current.noFurtherCustody();
    await vi.advanceTimersByTimeAsync(3000);
    expect(lists).toBe(0);
    current.noFurtherCustody();
    expect(await current.attempt.confirmMatch()).toBe(true);
    expect(lists).toBe(1);
    expect(current.events).toEqual([
      'offer',
      'export',
      'encrypt',
      'approve',
      'list',
      'export',
      'encrypt',
      'approve',
    ]);
    expect(current.approvals()).toBe(2);
    expect(current.seen.latest()).toMatchObject({
      status: 'submitted',
      fingerprint: '',
      confirmed: false,
    });
    expect(
      current.exports.every((bytes) => new Uint8Array(bytes).every((byte) => byte === 0))
    ).toBe(true);
  });

  it('preserves the need to relist after a retryable list failure and honors Retry-After without reading keys', async () => {
    const current = await ambiguousApproval();
    current.allowRetry();
    let lists = 0;
    server.use(
      http.get(listURL, () => {
        lists += 1;
        current.events.push('list');
        current.noFurtherCustody();
        return lists === 1
          ? HttpResponse.json({}, { status: 429, headers: { 'Retry-After': '7' } })
          : HttpResponse.json({ requests: [current.row] });
      })
    );
    await vi.advanceTimersByTimeAsync(3000);
    expect(await current.attempt.confirmMatch()).toBe(false);
    expect(current.seen.latest()).toMatchObject({
      status: 'offered',
      confirmed: false,
      retryAt: beganAt + 10_000,
    });
    current.noFurtherCustody();
    await vi.advanceTimersByTimeAsync(6999);
    expect(await current.attempt.confirmMatch()).toBe(false);
    expect(lists).toBe(1);
    current.noFurtherCustody();
    await vi.advanceTimersByTimeAsync(1);
    expect(await current.attempt.confirmMatch()).toBe(true);
    expect(lists).toBe(2);
    expect(current.events).toEqual([
      'offer',
      'export',
      'encrypt',
      'approve',
      'list',
      'list',
      'export',
      'encrypt',
      'approve',
    ]);
    expect(current.approvals()).toBe(2);
  });

  it.each([
    ['still-current positive control', () => {}],
    ...invalidations,
    ['expiry', () => vi.setSystemTime(vector.context.expires_at)],
  ] as Array<[string, (attempt: { dispose(): void }) => void]>)(
    'held authoritative relisting obeys %s before retry custody and publication',
    async (name, invalidate) => {
      const current = await ambiguousApproval();
      current.allowRetry();
      const entered = deferred();
      const release = deferred();
      let lists = 0;
      server.use(
        http.get(listURL, async () => {
          lists += 1;
          current.events.push('list');
          current.noFurtherCustody();
          entered.resolve();
          await release.promise;
          return HttpResponse.json({ requests: [current.row] });
        })
      );
      await vi.advanceTimersByTimeAsync(3000);
      const confirming = current.attempt.confirmMatch();
      try {
        await entered.promise;
        expect(await current.attempt.confirmMatch()).toBe(false);
        expect(lists).toBe(1);
        current.noFurtherCustody();
        invalidate(current.attempt);
      } finally {
        release.resolve();
      }
      const positive = name === 'still-current positive control';
      expect(await confirming).toBe(positive);
      if (!positive) current.noFurtherCustody();
      expect(current.approvals()).toBe(positive ? 2 : 1);
      expect(current.seen.views.some((view) => view.status === 'submitted')).toBe(positive);
    }
  );
});

describe('real apiFetch approval publication boundary', () => {
  const boundaries = [
    'version IPC',
    'attestation IPC',
    '403 re-attestation IPC',
    '401 refresh IPC',
    '401 then 403 re-attestation IPC',
  ] as const;
  const mutations: Array<[string, (attempt: { dispose(): void }) => void]> = [
    ['still-current positive control', () => {}],
    ['dispose', (attempt) => attempt.dispose()],
    ['expiry', () => vi.setSystemTime(vector.context.expires_at)],
    [
      'E2EE teardown',
      () => {
        custody.epoch += 1;
      },
    ],
    [
      'local user identity',
      () =>
        useUserStore.setState({
          user: {
            ...mockUser,
            id: '22222222-3333-4444-8555-666666666666',
          },
        }),
    ],
  ];

  it.each(
    boundaries.flatMap((boundary) =>
      [
        ...mutations,
        ...(boundary === '401 then 403 re-attestation IPC'
          ? invalidations.filter(
              ([name]) => name === 'auth lifecycle' || name === 'selected server'
            )
          : []),
      ].map(([name, invalidate]) => [boundary, name, invalidate] as const)
    )
  )(
    '%s held after preparing approval obeys %s before HTTP publication',
    async (boundary, name, invalidate) => {
      server.use(http.post(respondURL, () => HttpResponse.json(acknowledgement('offered'))));
      const seen = observe();
      const attempt = new ResponderDeviceRecoveryAttempt(
        { ...fixtureContext, status: 'pending' },
        seen.onView
      );
      attempts.push(attempt);
      const exported = plaintext();
      custody.export.mockResolvedValue(exported);
      await attempt.start();
      expect(seen.latest().status).toBe('offered');
      const entered = deferred();
      const release = deferred();
      const held = async () => {
        entered.resolve();
        await release.promise;
      };
      _resetClientVersionCache();
      (globalThis as unknown as { electron: unknown }).electron = {
        ...globalThis.electron,
        getVersion: async () => {
          if (boundary === 'version IPC') await held();
          return '1.2.3';
        },
        attestation: {
          getToken: async () => {
            if (boundary === 'attestation IPC') await held();
            return 'synthetic-attestation-token';
          },
          clearToken: async () => {
            if (
              boundary === '403 re-attestation IPC' ||
              boundary === '401 then 403 re-attestation IPC'
            )
              await held();
          },
        },
        refreshToken: async () => {
          if (boundary === '401 refresh IPC') await held();
          return { status: 'ok', accessToken: 'synthetic-refreshed-access-token' };
        },
      };
      let approvals = 0;
      server.use(
        http.post(respondURL, async ({ request }) => {
          const body = (await request.json()) as DeviceRecoveryRespondBody;
          expect(body.action).toBe('approve');
          approvals += 1;
          if (boundary === '401 then 403 re-attestation IPC') {
            expect(request.headers.get('Authorization')).toBe(
              `Bearer ${approvals === 1 ? 'synthetic-access-token' : 'synthetic-refreshed-access-token'}`
            );
            if (approvals === 1) return HttpResponse.json({}, { status: 401 });
            if (approvals === 2)
              return HttpResponse.json({ code: 'ATTESTATION_EXPIRED' }, { status: 403 });
          }
          if (approvals === 1 && boundary === '403 re-attestation IPC')
            return HttpResponse.json({ code: 'ATTESTATION_EXPIRED' }, { status: 403 });
          if (approvals === 1 && boundary === '401 refresh IPC')
            return HttpResponse.json({}, { status: 401 });
          return HttpResponse.json(acknowledgement('approved'));
        })
      );
      const confirming = attempt.confirmMatch();
      await entered.promise;
      // The payload is already encrypted and transient PKCS8 wiped at this inner API await.
      expect(custody.export).toHaveBeenCalledTimes(1);
      expect(new Uint8Array(exported).every((byte) => byte === 0)).toBe(true);
      const legitimateInitialPosts =
        boundary === '401 then 403 re-attestation IPC'
          ? 2
          : boundary.startsWith('403') || boundary.startsWith('401')
            ? 1
            : 0;
      expect(approvals).toBe(legitimateInitialPosts);
      expect(seen.views.some((view) => view.status === 'submitted')).toBe(false);
      invalidate(attempt);
      release.resolve();
      const submitted = await confirming;
      const positive = name === 'still-current positive control';
      expect(approvals).toBe(legitimateInitialPosts + (positive ? 1 : 0));
      expect(submitted).toBe(positive);
      expect(seen.views.some((view) => view.status === 'submitted')).toBe(positive);
    }
  );
});

describe('late continuations', () => {
  it('disposing before create returns cannot publish its late response or start polling', async () => {
    const entered = deferred();
    const release = deferred();
    let polls = 0;
    server.use(
      http.post(createURL, async ({ request }) => {
        const body = (await request.json()) as DeviceRecoveryCreateBody;
        entered.resolve();
        await release.promise;
        return HttpResponse.json(
          pending({
            ...vector.context,
            requester_nonce: body.requester_nonce,
            requester_public_key: body.requester_public_key,
          })
        );
      }),
      http.get(pollURL, () => {
        polls += 1;
        return HttpResponse.json(pending());
      })
    );
    const { attempt, seen } = requester();
    const starting = attempt.start();
    await entered.promise;
    attempt.dispose();
    const count = seen.views.length;
    release.resolve();
    await starting;
    await vi.advanceTimersByTimeAsync(10_000);
    expect(seen.views).toHaveLength(count);
    expect(polls).toBe(0);
  });
  it.each(invalidations)(
    '%s during import prevents complete dispatch and wipes late plaintext',
    async (_name, invalidate) => {
      const { attempt, seen } = await approvedRequester();
      const entered = deferred();
      const release = deferred();
      custody.validate.mockImplementation(async () => {
        entered.resolve();
        await release.promise;
      });
      let completions = 0;
      server.use(
        http.post(completeURL, () => {
          completions += 1;
          return HttpResponse.json(acknowledgement('complete'));
        })
      );
      const confirming = attempt.confirmMatch();
      await entered.promise;
      const imported = custody.validate.mock.calls[0][0];
      invalidate(attempt);
      const count = seen.views.length;
      release.resolve();
      await confirming;
      expect(completions).toBe(0);
      expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
      expect(() => attempt.recoveredAccountKey()).toThrow();
      expect(seen.views.slice(count).some((view) => view.status === 'complete')).toBe(false);
    }
  );
  it.each(invalidations)(
    '%s after import blocks a late completion acknowledgement from releasing plaintext',
    async (_name, invalidate) => {
      const { attempt, seen } = await approvedRequester();
      const entered = deferred();
      const release = deferred();
      server.use(
        http.post(completeURL, async () => {
          entered.resolve();
          await release.promise;
          return HttpResponse.json(acknowledgement('complete'));
        })
      );
      const confirming = attempt.confirmMatch();
      await entered.promise;
      const imported = custody.validate.mock.calls[0][0];
      invalidate(attempt);
      const count = seen.views.length;
      release.resolve();
      await confirming;
      expect(new Uint8Array(imported).every((byte) => byte === 0)).toBe(true);
      expect(() => attempt.recoveredAccountKey()).toThrow();
      expect(seen.views.slice(count).some((view) => view.status === 'complete')).toBe(false);
    }
  );
  it.each(invalidations)(
    '%s during responder export wipes bytes and forbids approval publication',
    async (_name, invalidate) => {
      server.use(http.post(respondURL, () => HttpResponse.json(acknowledgement('offered'))));
      const seen = observe();
      const attempt = new ResponderDeviceRecoveryAttempt(
        { ...fixtureContext, status: 'pending' },
        seen.onView
      );
      attempts.push(attempt);
      await attempt.start();
      expect(seen.latest().status).toBe('offered');
      const entered = deferred();
      const release = deferred();
      const exported = plaintext();
      custody.export.mockImplementation(async () => {
        entered.resolve();
        await release.promise;
        return exported;
      });
      let approvals = 0;
      server.use(
        http.post(respondURL, () => {
          approvals += 1;
          return HttpResponse.json(acknowledgement('approved'));
        })
      );
      const confirming = attempt.confirmMatch();
      await entered.promise;
      invalidate(attempt);
      release.resolve();
      expect(await confirming).toBe(false);
      expect(new Uint8Array(exported).every((byte) => byte === 0)).toBe(true);
      expect(approvals).toBe(0);
      expect(seen.views.some((view) => view.status === 'submitted')).toBe(false);
    }
  );
});

describe('validated owned listing and rejection acknowledgements', () => {
  it('returns current validated pending and offered rows', async () => {
    const row = {
      ...vector.context,
      ...vector.offer,
      request_id: 'bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee',
      status: 'offered',
    };
    row.transcript_hash = await recoveryTranscriptHash(
      { ...fixtureContext, request_id: row.request_id },
      row
    );
    server.use(
      http.get(listURL, ({ request }) => {
        expect(request.headers.get('Authorization')).toBe('Bearer synthetic-access-token');
        return HttpResponse.json({ requests: [pending(), row] });
      })
    );
    const rows = await listDeviceRecoveryRequests(vector.user_id, () => {});
    expect(rows.map((request) => request.status)).toEqual(['pending', 'offered']);
    expect(Object.isFrozen(rows[0])).toBe(true);
  });
  it.each([
    [
      'foreign account',
      {
        ...vector.context,
        account_binding: vector.context.recovery_token_jti_hash,
        status: 'pending',
      },
    ],
    [
      'different origin',
      { ...vector.context, server_origin: 'https://other.example.test', status: 'pending' },
    ],
    ['expired row', { ...vector.context, expires_at: beganAt, status: 'pending' }],
    [
      'lifetime beyond the two-minute clock allowance',
      { ...vector.context, expires_at: beganAt + 17 * 60_000 + 1, status: 'pending' },
    ],
    ['legacy version', { ...vector.context, protocol_version: 1, status: 'pending' }],
    ['terminal row', terminal()],
    ['unknown field', { ...vector.context, status: 'pending', user_id: vector.user_id }],
  ])('refuses the list containing a %s', async (_name, row) => {
    server.use(http.get(listURL, () => HttpResponse.json({ requests: [row] })));
    await expect(listDeviceRecoveryRequests(vector.user_id, () => {})).rejects.toThrow(
      RECOVERY_UPDATE_GUIDANCE
    );
  });
  it('refuses duplicated request identities instead of exposing conflicting choices', async () => {
    server.use(http.get(listURL, () => HttpResponse.json({ requests: [pending(), pending()] })));
    await expect(listDeviceRecoveryRequests(vector.user_id, () => {})).rejects.toThrow(
      RECOVERY_UPDATE_GUIDANCE
    );
  });
  it('discarding a delayed list response obeys its caller lifecycle fence', async () => {
    const entered = deferred();
    const release = deferred();
    let current = true;
    server.use(
      http.get(listURL, async () => {
        entered.resolve();
        await release.promise;
        return HttpResponse.json({ requests: [pending()] });
      })
    );
    const listing = listDeviceRecoveryRequests(vector.user_id, () => {
      if (!current) throw new Error('Caller disposed');
    });
    const rejected = expect(listing).rejects.toThrow('Caller disposed');
    await entered.promise;
    current = false;
    release.resolve();
    await rejected;
  });
  it('resolves rejection only after a matching owned request acknowledgement', async () => {
    server.use(
      http.post(respondURL, async ({ request }) => {
        expect(await request.json()).toEqual({ action: 'reject', protocol_version: 2 });
        return HttpResponse.json(acknowledgement('rejected'));
      })
    );
    await expect(
      rejectDeviceRecoveryRequest({ ...fixtureContext, status: 'pending' }, () => {})
    ).resolves.toBeUndefined();
  });
  it.each([
    [
      'wrong request',
      { ...acknowledgement('rejected'), request_id: 'bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee' },
    ],
    ['wrong state', acknowledgement('approved')],
    [
      'extra fields',
      { ...acknowledgement('rejected'), encrypted_payload: vector.encrypted_payload },
    ],
  ])('does not claim rejection from an acknowledgement with %s', async (_name, ack) => {
    server.use(http.post(respondURL, () => HttpResponse.json(ack)));
    await expect(
      rejectDeviceRecoveryRequest({ ...fixtureContext, status: 'pending' }, () => {})
    ).rejects.toThrow(RECOVERY_UPDATE_GUIDANCE);
  });
  it('preserves a retryable rejection failure instead of claiming remote rejection', async () => {
    server.use(
      http.post(respondURL, () =>
        HttpResponse.json({}, { status: 503, headers: { 'Retry-After': '9' } })
      )
    );
    await expect(
      rejectDeviceRecoveryRequest({ ...fixtureContext, status: 'pending' }, () => {})
    ).rejects.toMatchObject({ retryable: true, retryAfterMs: 9000, status: 503 });
  });
});
