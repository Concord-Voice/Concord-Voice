// @vitest-environment node
/**
 * appleFlow orchestration tests (#974): every collaborator mocked via deps.
 */
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import {
  APPLE_FLOW_TIMEOUT_MS,
  cancelActiveAppleFlow,
  mapSessionResponse,
  runAppleSignIn,
  type AppleFlowDeps,
} from '@/main/oauth/apple/appleFlow';
import { AppleFlowError } from '@/main/oauth/apple/errors';
import type { LoopbackHandle, LoopbackResult } from '@/main/ssoLoopback';

const AUTH_URL =
  'https://appleid.apple.com/auth/authorize?client_id=chat.concordvoice.signin' +
  '&redirect_uri=http%3A%2F%2F127.0.0.1%3A51620%2Foauth%2Fcallback' +
  '&response_type=code&state=state-1&nonce=nonce-1&code_challenge=x&code_challenge_method=S256';

interface FakeLoopback {
  handle: LoopbackHandle;
  resolve: (r: LoopbackResult) => void;
  reject: (e: Error) => void;
  closeCalls: () => number;
}

function makeLoopback(): FakeLoopback {
  let resolveFn!: (r: LoopbackResult) => void;
  let rejectFn!: (e: Error) => void;
  let closes = 0;
  const promise = new Promise<LoopbackResult>((res, rej) => {
    resolveFn = res;
    rejectFn = rej;
  });
  promise.catch(() => undefined);
  const handle: LoopbackHandle = {
    port: 51620,
    redirectURI: 'http://127.0.0.1:51620/oauth/callback',
    bindAddress: '127.0.0.1',
    promise,
    close: () => {
      closes += 1;
      rejectFn(new Error('oauth_cancelled'));
    },
  };
  return { handle, resolve: resolveFn, reject: rejectFn, closeCalls: () => closes };
}

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

interface RigOverrides {
  initiateBody?: unknown;
  initiateStatus?: number;
  sessionBody?: unknown;
  sessionStatus?: number;
  flowTimeoutMs?: number;
}

function makeDeps(lo: FakeLoopback, o: RigOverrides = {}) {
  const controlPlaneFetch = vi.fn(async (url: RequestInfo | URL) => {
    const u = String(url);
    if (u.includes('/api/v1/auth/sso/apple?')) {
      return jsonResponse(
        o.initiateStatus ?? 200,
        o.initiateBody ?? { auth_url: AUTH_URL, state: 'state-1', nonce: 'nonce-1' }
      );
    }
    if (u.endsWith('/api/v1/auth/sso/apple/session')) {
      return jsonResponse(o.sessionStatus ?? 200, o.sessionBody ?? { access_token: 'at-1' });
    }
    throw new Error(`unexpected control-plane url: ${u}`);
  });

  const deps: AppleFlowDeps = {
    apiBase: 'https://api.test.example',
    controlPlaneFetch: controlPlaneFetch as unknown as typeof fetch,
    appleFetch: vi.fn() as unknown as typeof fetch,
    openExternal: vi.fn(async () => undefined),
    startLoopback: vi.fn(async () => lo.handle),
    signClientSecret: vi.fn(async () => 'broker-jwt'),
    appleTokenCall: vi.fn(async () => ({ idToken: 'idt-1' })),
    verifyIdToken: vi.fn(async () => ({ sub: 's', email: 'e@x.test', emailVerified: true })),
    flowTimeoutMs: o.flowTimeoutMs,
  };
  return { deps, controlPlaneFetch };
}

/** Drives the happy callback after the flow has opened the browser. */
function deliverCallback(lo: FakeLoopback, overrides: Partial<LoopbackResult> = {}) {
  lo.resolve({ code: 'code-1', state: 'state-1', ...overrides });
}

beforeEach(() => {
  vi.clearAllMocks();
});

afterEach(() => {
  cancelActiveAppleFlow(); // never leak an active flow between tests
});

describe('runAppleSignIn — happy path', () => {
  it('chains pkce → loopback → initiate → browser → broker → token → verify → session', async () => {
    const lo = makeLoopback();
    const { deps, controlPlaneFetch } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalledWith(AUTH_URL));
    deliverCallback(lo, { appleUserData: '{"name":{"firstName":"Jane"}}' });

    await expect(p).resolves.toEqual({ kind: 'tokens', accessToken: 'at-1' });

    // initiate carried the loopback redirect AND a shaped S256 challenge
    const initiateUrl = String(controlPlaneFetch.mock.calls[0][0]);
    expect(initiateUrl).toContain('redirect_uri=http%3A%2F%2F127.0.0.1%3A51620%2Foauth%2Fcallback');
    expect(initiateUrl).toMatch(/code_challenge=[A-Za-z0-9_-]{43}/);

    // token call got client_id + redirect_uri parsed from the auth_url, the
    // loopback code, and a verifier whose S256 equals the sent challenge
    const tokenArgs = (deps.appleTokenCall as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(tokenArgs.clientId).toBe('chat.concordvoice.signin');
    expect(tokenArgs.redirectUri).toBe('http://127.0.0.1:51620/oauth/callback');
    expect(tokenArgs.code).toBe('code-1');
    expect(tokenArgs.clientSecret).toBe('broker-jwt');

    // verify bound the initiate nonce AND the flow AbortSignal (so JWKS
    // fetches die with the flow — #1486 review fix)
    const verifyArgs = (deps.verifyIdToken as ReturnType<typeof vi.fn>).mock.calls[0][0];
    expect(verifyArgs.expectedNonce).toBe('nonce-1');
    expect(verifyArgs.idToken).toBe('idt-1');
    expect(verifyArgs.signal).toBeInstanceOf(AbortSignal);

    // session POST threaded id_token + state + apple_user_data
    const sessionInit = controlPlaneFetch.mock.calls[1][1] as RequestInit;
    expect(JSON.parse(String(sessionInit.body))).toEqual({
      id_token: 'idt-1',
      state: 'state-1',
      apple_user_data: '{"name":{"firstName":"Jane"}}',
    });

    // loopback gets the FLOW deadline, not the 60s default (spec R2)
    expect(deps.startLoopback).toHaveBeenCalledWith({ timeoutMs: APPLE_FLOW_TIMEOUT_MS });
  });

  it('omits apple_user_data when the loopback returned none', async () => {
    const lo = makeLoopback();
    const { deps, controlPlaneFetch } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await p;
    const sessionInit = controlPlaneFetch.mock.calls[1][1] as RequestInit;
    expect(JSON.parse(String(sessionInit.body))).not.toHaveProperty('apple_user_data');
  });
});

describe('runAppleSignIn — failure taxonomy', () => {
  it('initiate non-OK → sso_initiate_failed', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo, { initiateStatus: 503 });
    await expect(runAppleSignIn(deps)).resolves.toEqual({
      kind: 'error',
      code: 'sso_initiate_failed',
    });
  });

  it('initiate malformed body (missing nonce) → sso_initiate_failed', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo, { initiateBody: { auth_url: AUTH_URL, state: 'state-1' } });
    await expect(runAppleSignIn(deps)).resolves.toEqual({
      kind: 'error',
      code: 'sso_initiate_failed',
    });
  });

  it('loopback timeout → sso_cancelled', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    lo.reject(new Error('oauth_timeout'));
    await expect(p).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
  });

  it('provider error passes through the oauth_provider_error family', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    lo.reject(new Error('oauth_provider_error:user_cancelled_authorize'));
    await expect(p).resolves.toEqual({
      kind: 'error',
      code: 'oauth_provider_error:user_cancelled_authorize',
    });
  });

  it('callback state mismatch → oauth_provider_error:state_mismatch (constant-time compare)', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo, { state: 'tampered-state' });
    await expect(p).resolves.toEqual({
      kind: 'error',
      code: 'oauth_provider_error:state_mismatch',
    });
    expect(deps.signClientSecret).not.toHaveBeenCalled();
  });

  it('broker failure surfaces its taxonomy code', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    (deps.signClientSecret as ReturnType<typeof vi.fn>).mockRejectedValue(
      new AppleFlowError('sso_state_expired', 'broker')
    );
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'error', code: 'sso_state_expired' });
  });

  it('apple 5xx retries ONCE then surfaces apple_unavailable', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    (deps.appleTokenCall as ReturnType<typeof vi.fn>).mockRejectedValue(
      new AppleFlowError('apple_unavailable', 'apple_token')
    );
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'error', code: 'apple_unavailable' });
    expect(deps.appleTokenCall).toHaveBeenCalledTimes(2);
  });

  it('apple 400 is NOT retried', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    (deps.appleTokenCall as ReturnType<typeof vi.fn>).mockRejectedValue(
      new AppleFlowError('apple_exchange_rejected', 'apple_token')
    );
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'error', code: 'apple_exchange_rejected' });
    expect(deps.appleTokenCall).toHaveBeenCalledTimes(1);
  });

  it('JWKS-unavailable retries once and can SUCCEED on the second attempt', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    (deps.verifyIdToken as ReturnType<typeof vi.fn>)
      .mockRejectedValueOnce(new AppleFlowError('apple_verification_unavailable', 'verify'))
      .mockResolvedValueOnce({ sub: 's', emailVerified: true });
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'tokens', accessToken: 'at-1' });
    expect(deps.verifyIdToken).toHaveBeenCalledTimes(2);
  });

  it('invalid id_token is NEVER retried', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    (deps.verifyIdToken as ReturnType<typeof vi.fn>).mockRejectedValue(
      new AppleFlowError('apple_id_token_invalid', 'verify')
    );
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'error', code: 'apple_id_token_invalid' });
    expect(deps.verifyIdToken).toHaveBeenCalledTimes(1);
  });

  it('session non-OK → sso_session_rejected', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo, {
      sessionStatus: 401,
      sessionBody: { error_code: 'invalid_state' },
    });
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    deliverCallback(lo);
    await expect(p).resolves.toEqual({ kind: 'error', code: 'sso_session_rejected' });
  });
});

describe('runAppleSignIn — lifecycle', () => {
  it('cancelActiveAppleFlow mid-loopback → sso_cancelled + loopback closed', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo);
    const p = runAppleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    cancelActiveAppleFlow();
    await expect(p).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
    expect(lo.closeCalls()).toBeGreaterThanOrEqual(1);
  });

  it('the flow deadline tears the flow down → sso_cancelled', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo, { flowTimeoutMs: 20 });
    const p = runAppleSignIn(deps); // never deliver the callback
    await expect(p).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
    expect(lo.closeCalls()).toBeGreaterThanOrEqual(1);
  });

  it('a second invocation supersedes (cancels) the first', async () => {
    const lo1 = makeLoopback();
    const { deps: deps1 } = makeDeps(lo1);
    const p1 = runAppleSignIn(deps1);
    await vi.waitFor(() => expect(deps1.openExternal).toHaveBeenCalled());

    const lo2 = makeLoopback();
    const { deps: deps2 } = makeDeps(lo2);
    const p2 = runAppleSignIn(deps2);

    await expect(p1).resolves.toEqual({ kind: 'error', code: 'sso_cancelled' });
    await vi.waitFor(() => expect(deps2.openExternal).toHaveBeenCalled());
    deliverCallback(lo2);
    await expect(p2).resolves.toEqual({ kind: 'tokens', accessToken: 'at-1' });
  });

  it('rejects a non-https auth_url without opening the browser', async () => {
    const lo = makeLoopback();
    const { deps } = makeDeps(lo, {
      initiateBody: {
        auth_url: 'http://appleid.apple.com/auth/authorize?client_id=x&redirect_uri=y',
        state: 'state-1',
        nonce: 'nonce-1',
      },
    });
    await expect(runAppleSignIn(deps)).resolves.toEqual({
      kind: 'error',
      code: 'sso_initiate_failed',
    });
    expect(deps.openExternal).not.toHaveBeenCalled();
  });
});

describe('mapSessionResponse', () => {
  it('maps access_token → tokens', () => {
    expect(mapSessionResponse({ access_token: 'a' })).toEqual({ kind: 'tokens', accessToken: 'a' });
  });

  it('maps mfa_challenge_token → mfa_challenge with methods passthrough', () => {
    expect(
      mapSessionResponse({
        mfa_challenge_token: 'm',
        methods: ['totp', 'webauthn'],
        recovery_only_methods: ['backup_code'],
        webauthn_options: { rpId: 'x' },
      })
    ).toEqual({
      kind: 'mfa_challenge',
      mfaChallengeToken: 'm',
      methods: ['totp', 'webauthn'],
      recoveryOnlyMethods: ['backup_code'],
      webauthnOptions: { rpId: 'x' },
    });
  });

  it('maps sso_registration_required → sso_token/new_user', () => {
    expect(
      mapSessionResponse({
        sso_registration_required: true,
        sso_token: 't',
        email: 'e@x.test',
        name: 'Jane Doe',
      })
    ).toEqual({
      kind: 'sso_token',
      branch: 'new_user',
      ssoToken: 't',
      email: 'e@x.test',
      name: 'Jane Doe',
    });
  });

  it('maps account_link_available → sso_token/account_link', () => {
    expect(
      mapSessionResponse({
        account_link_available: true,
        sso_token: 't',
        masked_email: 'j***@x.test',
      })
    ).toEqual({
      kind: 'sso_token',
      branch: 'account_link',
      ssoToken: 't',
      maskedEmail: 'j***@x.test',
    });
  });

  it('maps an unrecognized shape → error sso_session_rejected', () => {
    expect(mapSessionResponse({ surprise: true })).toEqual({
      kind: 'error',
      code: 'sso_session_rejected',
    });
  });
});
