import { beforeEach, describe, expect, it, vi } from 'vitest';

// The inline WebAuthn step-up helpers behind the factor picker's hook
// (`useStepUpFactor`; design 2026-09-26-mfa-factor-picker D14). Oracle: what is put on the
// wire (path, body, the caller's context and signal) and what the helpers
// throw for each answer the server can give.

const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', async (importOriginal) => ({
  ...(await importOriginal<typeof import('@/renderer/services/system/apiClient')>()),
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import {
  beginWebAuthnInlineVerification,
  finishWebAuthnVerification,
  NO_INLINE_SESSION,
  NO_WEBAUTHN_CREDENTIALS,
  performWebAuthnAssertion,
  WEBAUTHN_INLINE_BEGIN_PATH,
  WEBAUTHN_INLINE_FINISH_PATH,
  WebAuthnInlineError,
} from '@/renderer/services/system/webauthnInlineStepUp';
import { captureApiRequestContext } from '@/renderer/services/system/requestContext';

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

/** What a proxy answers with when it, not the server, fails: HTML, not JSON. */
function html(status: number): Response {
  return new Response('<html><body>Bad gateway</body></html>', {
    status,
    headers: { 'Content-Type': 'text/html' },
  });
}

const BEGIN_OPTIONS = {
  publicKey: {
    // 'AQID' is [1, 2, 3]; 'BAUG' is [4, 5, 6].
    challenge: 'AQID',
    rpId: 'localhost',
    timeout: 60000,
    allowCredentials: [{ type: 'public-key', id: 'BAUG' }],
  },
};

function credential(userHandle: ArrayBuffer | null = new Uint8Array([70, 80]).buffer) {
  return {
    id: 'credential-id',
    rawId: new Uint8Array([1, 2, 3]).buffer,
    type: 'public-key',
    response: {
      authenticatorData: new Uint8Array([10, 20]).buffer,
      clientDataJSON: new Uint8Array([30, 40]).buffer,
      signature: new Uint8Array([50, 60]).buffer,
      userHandle,
    },
  } as unknown as PublicKeyCredential;
}

/** The thrown WebAuthnInlineError, or fails the test if the promise resolved or threw another type. */
async function thrown(promise: Promise<unknown>): Promise<WebAuthnInlineError> {
  const err = await promise.then(
    () => null,
    (e: unknown) => e
  );
  expect(err).toBeInstanceOf(WebAuthnInlineError);
  return err as WebAuthnInlineError;
}

beforeEach(() => {
  mockApiFetch.mockReset();
});

describe('the frozen server strings', () => {
  it('match the control plane exactly', () => {
    expect(NO_WEBAUTHN_CREDENTIALS).toBe('No WebAuthn credentials registered');
    expect(NO_INLINE_SESSION).toBe('No verification session found. Start a new verification.');
    expect(WEBAUTHN_INLINE_BEGIN_PATH).toBe('/api/v1/mfa/webauthn/verify-inline/begin');
    expect(WEBAUTHN_INLINE_FINISH_PATH).toBe('/api/v1/mfa/webauthn/verify-inline/finish');
  });
});

describe('beginWebAuthnInlineVerification', () => {
  it('posts the purpose and decodes the challenge and credential ids', async () => {
    mockApiFetch.mockResolvedValueOnce(json(BEGIN_OPTIONS));

    const options = await beginWebAuthnInlineVerification('dm.purge');

    const [path, init] = mockApiFetch.mock.calls[0];
    expect(path).toBe(WEBAUTHN_INLINE_BEGIN_PATH);
    expect(init.method).toBe('POST');
    expect(init.headers).toEqual({ 'Content-Type': 'application/json' });
    expect(init.body).toBe(JSON.stringify({ purpose: 'dm.purge' }));
    expect(Array.from(new Uint8Array(options.challenge as ArrayBuffer))).toEqual([1, 2, 3]);
    expect(options.rpId).toBe('localhost');
    expect(options.timeout).toBe(60000);
    expect(options.allowCredentials).toHaveLength(1);
    expect(Array.from(new Uint8Array(options.allowCredentials![0].id as ArrayBuffer))).toEqual([
      4, 5, 6,
    ]);
  });

  it('accepts options without allowCredentials', async () => {
    mockApiFetch.mockResolvedValueOnce(
      json({ publicKey: { challenge: 'AQID', rpId: 'localhost' } })
    );

    const options = await beginWebAuthnInlineVerification('dm.clear');

    expect(options.allowCredentials).toBeUndefined();
  });

  it('is its own operation without a context or signal (a caller that passes neither)', async () => {
    mockApiFetch.mockResolvedValueOnce(json(BEGIN_OPTIONS));

    await beginWebAuthnInlineVerification('dm.purge');

    const call = mockApiFetch.mock.calls[0];
    expect(call).toHaveLength(2);
    expect('signal' in call[1]).toBe(false);
  });

  it('forwards the caller context and signal', async () => {
    mockApiFetch.mockResolvedValueOnce(json(BEGIN_OPTIONS));
    const context = captureApiRequestContext();
    const { signal } = new AbortController();

    await beginWebAuthnInlineVerification('dm.purge', context, signal);

    const [, init, opts] = mockApiFetch.mock.calls[0];
    expect(opts.context).toBe(context);
    expect(init.signal).toBe(signal);
  });

  it('throws the server error text with the step and status on a non-2xx', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ error: NO_WEBAUTHN_CREDENTIALS }, 400));

    const err = await thrown(beginWebAuthnInlineVerification('dm.purge'));

    expect(err.step).toBe('begin');
    expect(err.status).toBe(400);
    expect(err.serverError).toBe('No WebAuthn credentials registered');
    expect(err.message).toBe('No WebAuthn credentials registered');
    expect(err.name).toBe('WebAuthnInlineError');
  });

  it.each([
    ['no error field', {}],
    ['an empty error', { error: '' }],
    ['a non-string error', { error: 42 }],
    ['a non-object body', 'oops'],
    ['a null body', null],
  ])('falls back to generic copy and a null serverError for %s', async (_name, body) => {
    mockApiFetch.mockResolvedValueOnce(json(body, 500));

    const err = await thrown(beginWebAuthnInlineVerification('dm.purge'));

    expect(err.serverError).toBeNull();
    expect(err.message).toBe('Failed to start verification');
    expect(err.status).toBe(500);
  });

  it('propagates a transport rejection untouched', async () => {
    const failure = new TypeError('Failed to fetch');
    mockApiFetch.mockRejectedValueOnce(failure);

    await expect(beginWebAuthnInlineVerification('dm.purge')).rejects.toBe(failure);
  });

  // Mutant: `res.json()` without the `.catch`, so an HTML error body rejects
  // with a SyntaxError and the 401 loses its session-expired meaning.
  it.each([401, 502])(
    'keeps the real status %i when the error body is not JSON',
    async (status) => {
      mockApiFetch.mockResolvedValueOnce(html(status));

      const err = await thrown(beginWebAuthnInlineVerification('dm.purge'));

      expect(err.step).toBe('begin');
      expect(err.status).toBe(status);
      expect(err.serverError).toBeNull();
      expect(err.message).toBe('Failed to start verification');
    }
  );
});

describe('performWebAuthnAssertion', () => {
  it('asks the browser with the options and the signal', async () => {
    const get = vi.fn().mockResolvedValue(credential());
    Object.defineProperty(navigator, 'credentials', {
      value: { get },
      writable: true,
      configurable: true,
    });
    const options = { challenge: new Uint8Array([1]).buffer } as PublicKeyCredentialRequestOptions;
    const { signal } = new AbortController();

    const result = await performWebAuthnAssertion(options, signal);

    expect(get).toHaveBeenCalledWith({ publicKey: options, signal });
    expect(result.id).toBe('credential-id');
  });

  it('throws when the browser returns no credential', async () => {
    Object.defineProperty(navigator, 'credentials', {
      value: { get: vi.fn().mockResolvedValue(null) },
      writable: true,
      configurable: true,
    });

    await expect(
      performWebAuthnAssertion(
        {} as PublicKeyCredentialRequestOptions,
        new AbortController().signal
      )
    ).rejects.toThrow('No credential returned');
  });
});

describe('finishWebAuthnVerification', () => {
  it('posts the base64url-encoded assertion and returns the token', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ mfa_token: 'inline-token' }));

    const token = await finishWebAuthnVerification(credential());

    expect(token).toBe('inline-token');
    const [path, init] = mockApiFetch.mock.calls[0];
    expect(path).toBe(WEBAUTHN_INLINE_FINISH_PATH);
    expect(init.method).toBe('POST');
    expect(JSON.parse(init.body)).toEqual({
      id: 'credential-id',
      rawId: 'AQID',
      type: 'public-key',
      response: {
        authenticatorData: 'ChQ',
        clientDataJSON: 'Hig',
        signature: 'Mjw',
        userHandle: 'RlA',
      },
    });
  });

  it('omits userHandle when the authenticator sent none', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ mfa_token: 'inline-token' }));

    await finishWebAuthnVerification(credential(null));

    expect(JSON.parse(mockApiFetch.mock.calls[0][1].body).response).not.toHaveProperty(
      'userHandle'
    );
  });

  it('is its own operation without a context or signal', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ mfa_token: 'inline-token' }));

    await finishWebAuthnVerification(credential());

    const call = mockApiFetch.mock.calls[0];
    expect(call).toHaveLength(2);
    expect('signal' in call[1]).toBe(false);
  });

  it('forwards the caller context and signal', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ mfa_token: 'inline-token' }));
    const context = captureApiRequestContext();
    const { signal } = new AbortController();

    await finishWebAuthnVerification(credential(), context, signal);

    const [, init, opts] = mockApiFetch.mock.calls[0];
    expect(opts.context).toBe(context);
    expect(init.signal).toBe(signal);
  });

  it('throws the server error text on a non-2xx, including the expired-session string', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ error: NO_INLINE_SESSION }, 400));

    const err = await thrown(finishWebAuthnVerification(credential()));

    expect(err.step).toBe('finish');
    expect(err.status).toBe(400);
    expect(err.serverError).toBe('No verification session found. Start a new verification.');
  });

  it('falls back to generic copy when a non-2xx names no error', async () => {
    mockApiFetch.mockResolvedValueOnce(json({}, 401));

    const err = await thrown(finishWebAuthnVerification(credential()));

    expect(err.status).toBe(401);
    expect(err.serverError).toBeNull();
    expect(err.message).toBe('Verification failed');
  });

  // Mutant: `res.json()` without the `.catch`, as for begin.
  it.each([401, 502])(
    'keeps the real status %i when the error body is not JSON',
    async (status) => {
      mockApiFetch.mockResolvedValueOnce(html(status));

      const err = await thrown(finishWebAuthnVerification(credential()));

      expect(err.step).toBe('finish');
      expect(err.status).toBe(status);
      expect(err.serverError).toBeNull();
      expect(err.message).toBe('Verification failed');
    }
  );

  // Mutant: `(data as {...}).mfa_token` without the null-safe read, so an
  // unreadable 2xx body throws a TypeError instead of failing closed.
  it('fails closed, as a WebAuthnInlineError, on a 2xx whose body is not JSON', async () => {
    mockApiFetch.mockResolvedValueOnce(html(200));

    const err = await thrown(finishWebAuthnVerification(credential()));

    expect(err.step).toBe('finish');
    expect(err.status).toBe(200);
    expect(err.message).toBe('Verification failed');
  });

  it.each([
    ['no mfa_token', {}],
    ['an empty mfa_token', { mfa_token: '' }],
    ['a non-string mfa_token', { mfa_token: 123 }],
    ['a null mfa_token', { mfa_token: null }],
  ])('fails closed on a 2xx with %s', async (_name, body) => {
    mockApiFetch.mockResolvedValueOnce(json(body, 200));

    const err = await thrown(finishWebAuthnVerification(credential()));

    expect(err.step).toBe('finish');
    expect(err.status).toBe(200);
    expect(err.message).toBe('Verification failed');
  });
});
