import { render, screen, fireEvent, waitFor, act } from '../../../test-utils';
import { vi, beforeAll, afterAll, beforeEach, afterEach } from 'vitest';
import { http, HttpResponse } from 'msw';
import { setupServer } from 'msw/node';
import vectors from '../../../../../../docs/design/trusted-recovery-v2-vectors.json';
import AccountRecovery from '@/renderer/components/Auth/AccountRecovery';
import {
  apiUrl,
  getApiBase,
  resetRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';
import { resetAllStores } from '../../../helpers/store-helpers';
import { deferred } from '../../../helpers/deferred';
import {
  arrayBufferToBase64,
  base64ToArrayBuffer,
  exportECDHPublicKey,
  generateKeyPair,
  deriveKeyArgon2id,
  unwrapPrivateKey,
  generateDeviceRecoveryKeyPair,
  randomRecoveryNonce,
  generateRecoveryKey,
  wrapPrivateKey,
  wrapWithRecoveryKey,
} from '@/renderer/utils/crypto/crypto';
import {
  deriveDeviceRecoveryKeys,
  encryptDeviceRecoveryPayload,
  captureRecoveryTokenContext,
} from '@/renderer/utils/crypto/trustedDeviceRecovery';
import type {
  DeviceRecoveryContext,
  DeviceRecoveryRequest,
} from '@/renderer/services/system/deviceRecoveryContract';
import { RequesterDeviceRecoveryAttempt } from '@/renderer/services/system/deviceRecoveryService';

const server = setupServer();
let accountKeys: CryptoKeyPair;
let pkcs8: ArrayBuffer;
let context: DeviceRecoveryContext | null;
let pollResponse: DeviceRecoveryRequest | Record<string, unknown> | null;
let requests: Array<{ path: string; body: Record<string, unknown> }>;
let token: string;
let completionStatus = 200;
const onComplete = vi.fn();
beforeAll(async () => {
  server.listen({ onUnhandledRequest: 'error' });
  accountKeys = await generateKeyPair();
  pkcs8 = await crypto.subtle.exportKey('pkcs8', accountKeys.privateKey);
});
afterAll(() => server.close());
beforeEach(async () => {
  resetAllStores();
  resetRuntimeServerBase();
  context = null;
  pollResponse = null;
  requests = [];
  completionStatus = 200;
  onComplete.mockReset();
  const expiry = Math.floor((Date.now() + 60_000) / 1000);
  token = `context.${arrayBufferToBase64(
    new TextEncoder().encode(
      JSON.stringify({ user_id: vectors.user_id, jti: vectors.recovery_token_jti, exp: expiry })
    ).buffer
  )
    .replaceAll('=', '')
    .replaceAll('+', '-')
    .replaceAll('/', '_')}.unsigned`;
  const captured = await captureRecoveryTokenContext(token);
  server.use(
    http.post(apiUrl('/api/v1/auth/recovery/begin'), () => HttpResponse.json({})),
    http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
      HttpResponse.json({
        recovery_token: token,
        has_trusted_devices: true,
        has_recovery_key: false,
      })
    ),
    http.post(apiUrl('/api/v1/auth/recovery/device-request'), async ({ request }) => {
      const body = (await request.json()) as Record<string, unknown>;
      requests.push({ path: 'device-request', body });
      context = {
        request_id: vectors.context.request_id,
        protocol_version: 2,
        server_origin: String(body.server_origin),
        account_binding: String(body.account_binding),
        expires_at: expiry * 1000,
        requester_nonce: String(body.requester_nonce),
        requester_public_key: String(body.requester_public_key),
        recovery_token_jti_hash: captured.jtiHash,
      };
      return HttpResponse.json({ ...context, status: 'pending' });
    }),
    http.get(apiUrl(`/api/v1/auth/recovery/device-request/${vectors.context.request_id}`), () =>
      HttpResponse.json(pollResponse ?? { ...context, status: 'pending' })
    ),
    http.post(
      apiUrl(`/api/v1/auth/recovery/device-request/${vectors.context.request_id}/complete`),
      async ({ request }) => {
        requests.push({
          path: 'complete',
          body: (await request.json()) as Record<string, unknown>,
        });
        expect(request.headers.get('Authorization')).toBe(`Bearer ${token}`);
        return HttpResponse.json(
          { request_id: vectors.context.request_id, protocol_version: 2, status: 'complete' },
          { status: completionStatus }
        );
      }
    ),
    http.post(apiUrl('/api/v1/auth/recovery/reset-password'), async ({ request }) => {
      requests.push({
        path: 'reset-password',
        body: (await request.json()) as Record<string, unknown>,
      });
      return HttpResponse.json({});
    })
  );
});
afterEach(() => {
  server.resetHandlers();
  vi.restoreAllMocks();
});
function retainedContext(): DeviceRecoveryContext {
  if (!context) throw new Error('Missing create context');
  return context;
}
async function start() {
  const rendered = render(<AccountRecovery onBack={vi.fn()} onComplete={onComplete} />);
  fireEvent.change(screen.getByRole('textbox', { name: 'Email' }), {
    target: { value: 'test@example.com' },
  });
  fireEvent.click(screen.getByRole('button', { name: 'Send Recovery Code' }));
  await screen.findByPlaceholderText('000000');
  fireEvent.change(screen.getByPlaceholderText('000000'), { target: { value: '123456' } });
  fireEvent.click(screen.getByRole('button', { name: 'Verify Code' }));
  await screen.findByRole('button', { name: 'Recover from trusted device instead' });
  fireEvent.click(screen.getByRole('button', { name: 'Recover from trusted device instead' }));
  await waitFor(() => expect(context?.requester_public_key).toEqual(expect.any(String)));
  return rendered;
}
async function approval() {
  const captured = retainedContext();
  const responder = await generateDeviceRecoveryKeyPair();
  const proposed = {
    responder_public_key: await exportECDHPublicKey(responder.publicKey),
    responder_nonce: randomRecoveryNonce(),
  };
  const material = await deriveDeviceRecoveryKeys(
    'responder',
    responder.privateKey,
    captured,
    proposed
  );
  return {
    ...captured,
    ...proposed,
    transcript_hash: material.transcriptHash,
    status: 'approved' as const,
    encrypted_payload: await encryptDeviceRecoveryPayload(material, pkcs8),
  };
}
async function fingerprint() {
  await screen.findByRole('group', { name: 'Recovery fingerprint' }, { timeout: 5000 });
}
function confirm() {
  fireEvent.click(screen.getByRole('button', { name: 'These fingerprints match' }));
}

it('locks early approval until requester consent then acknowledges completion and preserves the RSA account identity', async () => {
  const decrypt = vi.spyOn(crypto.subtle, 'decrypt');
  await start();
  pollResponse = await approval();
  await fingerprint();
  expect(screen.getByRole('status')).toHaveTextContent('Approval received');
  expect(decrypt).not.toHaveBeenCalled();
  expect(requests.some((request) => request.path === 'complete')).toBe(false);
  expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
  confirm();
  await screen.findByLabelText('New Password');
  expect(requests.map((request) => request.path)).toEqual(['device-request', 'complete']);
  const password = 'new-local-test-password';
  fireEvent.change(screen.getByLabelText('New Password'), { target: { value: password } });
  fireEvent.change(screen.getByLabelText('Confirm Password'), { target: { value: password } });
  fireEvent.click(screen.getByRole('button', { name: 'Reset Password' }));
  await screen.findByText('Password Reset Complete', undefined, { timeout: 6000 });
  const reset = requests.find((request) => request.path === 'reset-password')?.body;
  if (!reset) throw new Error('Missing password reset');
  expect(reset.key_derivation_alg).toBe('argon2id');
  const wrapping = await deriveKeyArgon2id(
    password,
    new Uint8Array(base64ToArrayBuffer(String(reset.key_derivation_salt)))
  );
  const recovered = await unwrapPrivateKey(
    base64ToArrayBuffer(String(reset.wrapped_private_key)),
    wrapping
  );
  const challenge = new Uint8Array([1, 2, 3, 4]);
  const encrypted = await crypto.subtle.encrypt(
    { name: 'RSA-OAEP' },
    accountKeys.publicKey,
    challenge
  );
  expect(
    new Uint8Array(await crypto.subtle.decrypt({ name: 'RSA-OAEP' }, recovered, encrypted))
  ).toEqual(challenge);
  fireEvent.click(screen.getByRole('button', { name: 'Sign In' }));
  expect(onComplete).toHaveBeenCalledOnce();
});
it.each(['transcript', 'requester', 'ciphertext', 'version'] as const)(
  'refuses changed %s before password reset',
  async (field) => {
    await start();
    const approved = await approval();
    if (field === 'transcript')
      approved.transcript_hash = arrayBufferToBase64(new Uint8Array(32).buffer);
    if (field === 'requester') approved.requester_nonce = randomRecoveryNonce();
    if (field === 'ciphertext') {
      const bytes = new Uint8Array(base64ToArrayBuffer(approved.encrypted_payload));
      bytes[bytes.length - 1] ^= 1;
      approved.encrypted_payload = arrayBufferToBase64(bytes.buffer);
    }
    if (field === 'version') {
      const bytes = new Uint8Array(base64ToArrayBuffer(approved.encrypted_payload));
      bytes[0] = 1;
      approved.encrypted_payload = arrayBufferToBase64(bytes.buffer);
    }
    pollResponse = approved;
    if (field === 'ciphertext') {
      await fingerprint();
      confirm();
    }
    await screen.findByText(
      'Could not authenticate recovery. Update both devices and start a new request.',
      undefined,
      { timeout: 5000 }
    );
    expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Continue with Account Reset' })).toBeDisabled();
    expect(requests.some((request) => request.path === 'complete')).toBe(false);
  }
);
it('keeps imported approval locked behind a completion acknowledgement', async () => {
  const decrypt = vi.spyOn(crypto.subtle, 'decrypt');
  const rendered = await start();
  completionStatus = 500;
  pollResponse = await approval();
  await fingerprint();
  const retryStarted = deferred();
  const acknowledgement = deferred();
  const confirmation = vi.spyOn(RequesterDeviceRecoveryAttempt.prototype, 'confirmMatch');
  const retryPoll = vi.spyOn(RequesterDeviceRecoveryAttempt.prototype, 'poll');
  // Control only the retry phase; preserve real HTTP and WebCrypto work. Clear the
  // earlier native poll timer when the completion failure schedules its retry.
  vi.useFakeTimers({
    toFake: ['Date', 'setTimeout', 'clearTimeout'],
    shouldClearNativeTimers: true,
  });
  try {
    await act(async () => {
      confirm();
      const result = confirmation.mock.results[0];
      if (!result || result.type !== 'return') throw new Error('Missing requester confirmation');
      await result.value;
    });
    expect(
      screen.getByText('Checking whether recovery completion was acknowledged.')
    ).toBeInTheDocument();
    expect(decrypt).toHaveBeenCalledOnce();
    expect(requests.filter((request) => request.path === 'complete')).toHaveLength(1);
    expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
    server.use(
      http.post(
        apiUrl(`/api/v1/auth/recovery/device-request/${vectors.context.request_id}/complete`),
        async ({ request }) => {
          requests.push({
            path: 'complete',
            body: (await request.json()) as Record<string, unknown>,
          });
          expect(request.headers.get('Authorization')).toBe(`Bearer ${token}`);
          retryStarted.resolve();
          await acknowledgement.promise;
          return HttpResponse.json({
            request_id: vectors.context.request_id,
            protocol_version: 2,
            status: 'complete',
          });
        }
      )
    );
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2999);
    });
    expect(requests.filter((request) => request.path === 'complete')).toHaveLength(1);
    expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1);
      await retryStarted.promise;
    });
    expect(requests.map((request) => request.path)).toEqual([
      'device-request',
      'complete',
      'complete',
    ]);
    expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
    expect(decrypt).toHaveBeenCalledOnce();
    await act(async () => {
      acknowledgement.resolve();
      const result = retryPoll.mock.results[0];
      if (!result || result.type !== 'return') throw new Error('Missing scheduled retry poll');
      await result.value;
    });
    expect(screen.getByLabelText('New Password')).toBeInTheDocument();
    expect(decrypt).toHaveBeenCalledOnce();
  } finally {
    acknowledgement.resolve();
    rendered.unmount();
    vi.useRealTimers();
  }
});
it('rejects mixed-client create responses with update/restart guidance', async () => {
  server.use(
    http.post(apiUrl('/api/v1/auth/recovery/device-request'), () =>
      HttpResponse.json({ request_id: vectors.context.request_id })
    )
  );
  await start().catch(() => {});
  expect(await screen.findByRole('alert')).toHaveTextContent(
    'Could not authenticate recovery. Update both devices and start a new request.'
  );
  expect(screen.queryByLabelText('New Password')).not.toBeInTheDocument();
});
it.each(['rejected', 'expired'] as const)(
  'preserves %s outcome and explicit data-loss consent',
  async (status) => {
    await start();
    const captured = retainedContext();
    pollResponse = {
      request_id: captured.request_id,
      protocol_version: 2,
      expires_at: captured.expires_at,
      status,
    };
    await screen.findByRole('button', { name: 'Continue with Account Reset' }, { timeout: 5000 });
    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(screen.getByRole('alert')).toHaveTextContent(
      status === 'rejected' ? 'rejected' : 'expired'
    );
    expect(screen.getByRole('button', { name: 'Continue with Account Reset' })).toBeDisabled();
    expect(requests).toHaveLength(1);
  }
);
it('erases a current completed plaintext handoff on unmount', async () => {
  const decrypted: ArrayBuffer[] = [];
  const originalDecrypt = crypto.subtle.decrypt.bind(crypto.subtle);
  vi.spyOn(crypto.subtle, 'decrypt').mockImplementation(async (...args) => {
    const bytes = await originalDecrypt(...args);
    decrypted.push(bytes);
    return bytes;
  });
  const rendered = await start();
  pollResponse = await approval();
  await fingerprint();
  confirm();
  await screen.findByLabelText('New Password');
  expect(decrypted).toHaveLength(1);
  expect(new Uint8Array(decrypted[0]).some((byte) => byte !== 0)).toBe(true);
  await act(async () => rendered.unmount());
  expect(new Uint8Array(decrypted[0]).every((byte) => byte === 0)).toBe(true);
});

it.each(['recovery-key', 'account-reset'] as const)(
  'allows the %s alternative to finish after a terminal device failure',
  async (alternative) => {
    let recoveryKey = '';
    if (alternative === 'recovery-key') {
      recoveryKey = generateRecoveryKey();
      const wrapping = await deriveKeyArgon2id('synthetic-original-password', new Uint8Array(16));
      const wrapped = await wrapWithRecoveryKey(
        arrayBufferToBase64(await wrapPrivateKey(accountKeys.privateKey, wrapping)),
        wrapping,
        recoveryKey
      );
      server.use(
        http.post(apiUrl('/api/v1/auth/recovery/verify-code'), () =>
          HttpResponse.json({
            recovery_token: token,
            has_trusted_devices: true,
            has_recovery_key: true,
            recovery_wrapped_private_key: wrapped.wrappedKey,
            recovery_key_salt: wrapped.salt,
          })
        )
      );
    }
    server.use(
      http.post(apiUrl('/api/v1/auth/recovery/reset-account'), async ({ request }) => {
        requests.push({
          path: 'reset-account',
          body: (await request.json()) as Record<string, unknown>,
        });
        return HttpResponse.json({});
      })
    );
    await start();
    const captured = retainedContext();
    pollResponse = {
      request_id: captured.request_id,
      protocol_version: 2,
      expires_at: captured.expires_at,
      status: 'rejected',
    };
    if (alternative === 'recovery-key') {
      await screen.findByRole('textbox', { name: 'Recovery Key' }, { timeout: 5000 });
      expect(screen.getAllByRole('alert')).toHaveLength(1);
      expect(screen.getByRole('alert')).toHaveTextContent('rejected');
      fireEvent.change(screen.getByRole('textbox', { name: 'Recovery Key' }), {
        target: { value: recoveryKey },
      });
      fireEvent.click(screen.getByRole('button', { name: 'Recover Account' }));
    } else {
      await screen.findByRole('button', { name: 'Continue with Account Reset' }, { timeout: 5000 });
      expect(screen.getAllByRole('alert')).toHaveLength(1);
      expect(screen.getByRole('alert')).toHaveTextContent('rejected');
      expect(screen.getByRole('button', { name: 'Continue with Account Reset' })).toBeDisabled();
      fireEvent.click(screen.getByRole('checkbox'));
      fireEvent.click(screen.getByRole('button', { name: 'Continue with Account Reset' }));
    }
    await screen.findByLabelText('New Password', undefined, { timeout: 6000 });
    const password = 'synthetic-alternative-password';
    fireEvent.change(screen.getByLabelText('New Password'), { target: { value: password } });
    fireEvent.change(screen.getByLabelText('Confirm Password'), { target: { value: password } });
    fireEvent.click(screen.getByRole('button', { name: 'Reset Password' }));
    await screen.findByText('Password Reset Complete', undefined, { timeout: 6000 });
    expect(requests.some((request) => request.path === 'complete')).toBe(false);
    const final = requests.find(
      (request) =>
        request.path === (alternative === 'recovery-key' ? 'reset-password' : 'reset-account')
    );
    expect(final?.body.key_derivation_alg).toBe('argon2id');
    if (alternative === 'account-reset') expect(final?.body.acknowledge_data_loss).toBe(true);
  },
  20_000
);
