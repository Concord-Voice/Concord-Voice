import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

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
import {
  getApiBase,
  resetRuntimeServerBase,
  setRuntimeServerBase,
} from '@/renderer/services/system/runtimeServerBase';

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

afterEach(() => {
  resetRuntimeServerBase();
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

    const options = await beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext());

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

    const options = await beginWebAuthnInlineVerification('dm.clear', captureApiRequestContext());

    expect(options.allowCredentials).toBeUndefined();
  });

  it('sends no signal when the caller passes none', async () => {
    mockApiFetch.mockResolvedValueOnce(json(BEGIN_OPTIONS));

    await beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext());

    expect('signal' in mockApiFetch.mock.calls[0][1]).toBe(false);
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

    const err = await thrown(
      beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext())
    );

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

    const err = await thrown(
      beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext())
    );

    expect(err.serverError).toBeNull();
    expect(err.message).toBe('Failed to start verification');
    expect(err.status).toBe(500);
  });

  it('propagates a transport rejection untouched', async () => {
    const failure = new TypeError('Failed to fetch');
    mockApiFetch.mockRejectedValueOnce(failure);

    await expect(
      beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext())
    ).rejects.toBe(failure);
  });

  // Mutant: `res.json()` without the `.catch`, so an HTML error body rejects
  // with a SyntaxError and the 401 loses its session-expired meaning.
  it.each([401, 502])(
    'keeps the real status %i when the error body is not JSON',
    async (status) => {
      mockApiFetch.mockResolvedValueOnce(html(status));

      const err = await thrown(
        beginWebAuthnInlineVerification('dm.purge', captureApiRequestContext())
      );

      expect(err.step).toBe('begin');
      expect(err.status).toBe(status);
      expect(err.serverError).toBeNull();
      expect(err.message).toBe('Failed to start verification');
    }
  );
});

// Every server shares one renderer origin, so the browser's own rpId check
// cannot tell them apart. A server could name another server's relying party
// in begin and relay the assertion the user's key signs for it. Oracle: the
// browser ceremony runs only for the relying party of the server the proof is
// sent to, and with no option begin does not use.
describe("step-up rpId relay: the relying party is bound to the proof's server", () => {
  const EVIL = 'https://evil-selfhost.example';

  function stubCeremony() {
    const get = vi.fn().mockResolvedValue(credential());
    Object.defineProperty(navigator, 'credentials', {
      value: { get },
      writable: true,
      configurable: true,
    });
    return get;
  }

  function beginAnswers(publicKey: Record<string, unknown>) {
    mockApiFetch.mockImplementation(async (path: string) =>
      path === WEBAUTHN_INLINE_BEGIN_PATH
        ? json({ publicKey })
        : json({ mfa_token: 'inline-token' })
    );
  }

  /** Begin, ceremony, finish: the order both callers run them in. */
  async function stepUp(context = captureApiRequestContext()): Promise<string> {
    const { signal } = new AbortController();
    const options = await beginWebAuthnInlineVerification('dm.purge', context, signal);
    const cred = await performWebAuthnAssertion(options, signal);
    return finishWebAuthnVerification(cred, context, signal);
  }

  it("refuses a server naming another server's relying party, before the ceremony", async () => {
    setRuntimeServerBase(EVIL);
    beginAnswers({
      challenge: 'AQID',
      rpId: 'concordvoice.chat',
      allowCredentials: [{ type: 'public-key', id: 'BAUG' }],
    });
    const get = stubCeremony();

    await expect(stepUp(captureApiRequestContext())).rejects.toThrow(
      'Server returned WebAuthn options for another server.'
    );

    expect(get).not.toHaveBeenCalled();
    expect(mockApiFetch.mock.calls.map(([path]) => path)).toEqual([WEBAUTHN_INLINE_BEGIN_PATH]);
  });

  it("runs the ceremony for the server's own relying party (control)", async () => {
    expect(getApiBase()).toBe('http://localhost:8080');
    beginAnswers({ challenge: 'AQID', rpId: 'localhost' });
    const get = stubCeremony();

    await expect(stepUp()).resolves.toBe('inline-token');

    expect(get).toHaveBeenCalledTimes(1);
    expect(get.mock.calls[0][0].publicKey.rpId).toBe('localhost');
  });

  it("accepts the official service's relying party for its API host", async () => {
    setRuntimeServerBase('https://api.concordvoice.chat');
    beginAnswers({ challenge: 'AQID', rpId: 'concordvoice.chat' });
    const get = stubCeremony();

    await expect(stepUp(captureApiRequestContext())).resolves.toBe('inline-token');

    expect(get).toHaveBeenCalledTimes(1);
  });

  // Mutant: binding to the current server rather than the captured one. With a
  // context, apiFetch sends begin and finish to the captured server only.
  it('binds to the captured server when the caller passes a context', async () => {
    setRuntimeServerBase(EVIL);
    const context = captureApiRequestContext();
    resetRuntimeServerBase();
    beginAnswers({ challenge: 'AQID', rpId: 'localhost' });
    const get = stubCeremony();

    await expect(stepUp(context)).rejects.toThrow(
      'Server returned WebAuthn options for another server.'
    );

    expect(get).not.toHaveBeenCalled();
  });

  // Mutant: a parse failure caught and the raw options passed on (fail-open),
  // or the whole body parsed in place of its publicKey.
  it.each([
    ['an empty body', {}],
    ['a null publicKey', { publicKey: null }],
    ['options outside publicKey', { challenge: 'AQID', rpId: 'localhost' }],
    ['a publicKey with no rpId', { publicKey: { challenge: 'AQID' } }],
  ])('refuses %s before the ceremony', async (_name, body) => {
    mockApiFetch.mockResolvedValue(json(body));
    const get = stubCeremony();

    await expect(stepUp()).rejects.toThrow('Server returned invalid WebAuthn options.');

    expect(get).not.toHaveBeenCalled();
  });

  it('refuses a 2xx begin whose body is not JSON, before the ceremony', async () => {
    mockApiFetch.mockResolvedValue(html(200));
    const get = stubCeremony();

    await expect(stepUp()).rejects.toThrow('Server returned invalid WebAuthn options.');

    expect(get).not.toHaveBeenCalled();
  });

  // Mutant: checking the rpId but keeping the spread decode.
  it('passes the browser no option begin does not use', async () => {
    beginAnswers({
      challenge: 'AQID',
      rpId: 'localhost',
      userVerification: 'required',
      allowCredentials: [{ type: 'public-key', id: 'BAUG', transports: ['usb'], extra: 'x' }],
      extensions: { appid: 'https://concordvoice.chat' },
      hints: ['hybrid'],
    });
    const get = stubCeremony();

    await stepUp();

    const publicKey = get.mock.calls[0][0].publicKey;
    expect(publicKey).not.toHaveProperty('extensions');
    expect(publicKey).not.toHaveProperty('hints');
    expect(publicKey.allowCredentials[0]).not.toHaveProperty('extra');
    expect(publicKey.allowCredentials[0].transports).toEqual(['usb']);
    expect(publicKey.userVerification).toBe('required');
  });
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

    const token = await finishWebAuthnVerification(credential(), captureApiRequestContext());

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

    await finishWebAuthnVerification(credential(null), captureApiRequestContext());

    expect(JSON.parse(mockApiFetch.mock.calls[0][1].body).response).not.toHaveProperty(
      'userHandle'
    );
  });

  it('sends no signal when the caller passes none', async () => {
    mockApiFetch.mockResolvedValueOnce(json({ mfa_token: 'inline-token' }));

    await finishWebAuthnVerification(credential(), captureApiRequestContext());

    expect('signal' in mockApiFetch.mock.calls[0][1]).toBe(false);
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

    const err = await thrown(finishWebAuthnVerification(credential(), captureApiRequestContext()));

    expect(err.step).toBe('finish');
    expect(err.status).toBe(400);
    expect(err.serverError).toBe('No verification session found. Start a new verification.');
  });

  it('falls back to generic copy when a non-2xx names no error', async () => {
    mockApiFetch.mockResolvedValueOnce(json({}, 401));

    const err = await thrown(finishWebAuthnVerification(credential(), captureApiRequestContext()));

    expect(err.status).toBe(401);
    expect(err.serverError).toBeNull();
    expect(err.message).toBe('Verification failed');
  });

  // Mutant: `res.json()` without the `.catch`, as for begin.
  it.each([401, 502])(
    'keeps the real status %i when the error body is not JSON',
    async (status) => {
      mockApiFetch.mockResolvedValueOnce(html(status));

      const err = await thrown(
        finishWebAuthnVerification(credential(), captureApiRequestContext())
      );

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

    const err = await thrown(finishWebAuthnVerification(credential(), captureApiRequestContext()));

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

    const err = await thrown(finishWebAuthnVerification(credential(), captureApiRequestContext()));

    expect(err.step).toBe('finish');
    expect(err.status).toBe(200);
    expect(err.message).toBe('Verification failed');
  });
});
