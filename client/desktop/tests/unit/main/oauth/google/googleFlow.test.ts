// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { runGoogleSignIn, cancelActiveGoogleFlow, GOOGLE_FLOW_TIMEOUT_MS } from '../../../../../src/main/oauth/google/googleFlow';
import type { GoogleFlowDeps } from '../../../../../src/main/oauth/google/googleFlow';

function makeLoopback() {
  let resolveCb!: (v: { code: string; state: string }) => void;
  let rejectCb!: (e: Error) => void;
  const promise = new Promise<{ code: string; state: string }>((res, rej) => { resolveCb = res; rejectCb = rej; });
  promise.catch(() => undefined);
  return { handle: { port: 5123, redirectURI: 'http://127.0.0.1:5123/oauth/callback', bindAddress: '127.0.0.1', promise, close: vi.fn(() => rejectCb(new Error('oauth_cancelled'))) }, resolveCb, rejectCb };
}

function makeDeps(lo: ReturnType<typeof makeLoopback>, over: Partial<GoogleFlowDeps> = {}): GoogleFlowDeps {
  return {
    apiBase: 'https://api.test',
    clientSecret: 'sek',
    controlPlaneFetch: vi.fn(async (input: unknown) => {
      const url = String(input);
      if (url.includes('/auth/sso/google?')) return new Response(JSON.stringify({ auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?client_id=gid&x=1', state: 'STATE', nonce: 'NONCE' }), { status: 200 });
      if (url.endsWith('/auth/sso/google/session')) return new Response(JSON.stringify({ status: 'authenticated', access_token: 'AT' }), { status: 200 });
      return new Response('{}', { status: 404 });
    }) as unknown as typeof fetch,
    googleFetch: vi.fn(async () => new Response('{}', { status: 200 })) as unknown as typeof fetch,
    openExternal: vi.fn(async () => {}),
    startLoopback: vi.fn(async () => lo.handle) as unknown as GoogleFlowDeps['startLoopback'],
    googleTokenCall: vi.fn(async () => ({ idToken: 'IDT' })),
    verifyIdToken: vi.fn(async () => ({ sub: 'g-sub', email: 'a@b.com', email_verified: true })),
    flowTimeoutMs: 1000,
    ...over,
  };
}

beforeEach(() => vi.clearAllMocks());
afterEach(() => cancelActiveGoogleFlow());

describe('runGoogleSignIn', () => {
  it('completes the happy path → tokens', async () => {
    const lo = makeLoopback();
    const deps = makeDeps(lo);
    const p = runGoogleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    lo.resolveCb({ code: 'CODE', state: 'STATE' });
    const result = await p;
    expect(result).toEqual({ kind: 'tokens', accessToken: 'AT' });
    expect(deps.googleTokenCall).toHaveBeenCalledWith(expect.objectContaining({ clientSecret: 'sek', clientId: 'gid' }));
  });

  it('fails when the server auth_url lacks client_id', async () => {
    const lo = makeLoopback();
    const deps = makeDeps(lo, {
      controlPlaneFetch: vi.fn(async (input: unknown) => {
        const url = String(input);
        if (url.includes('/auth/sso/google?')) return new Response(JSON.stringify({ auth_url: 'https://accounts.google.com/o/oauth2/v2/auth?x=1', state: 'STATE', nonce: 'NONCE' }), { status: 200 });
        return new Response('{}', { status: 404 });
      }) as unknown as typeof fetch,
    });
    const result = await runGoogleSignIn(deps);
    expect(result).toEqual({ kind: 'error', code: 'sso_initiate_failed' });
    expect(deps.googleTokenCall).not.toHaveBeenCalled();
  });

  it('rejects a state mismatch with oauth_provider_error:state_mismatch (mirrors appleFlow)', async () => {
    const lo = makeLoopback();
    const deps = makeDeps(lo);
    const p = runGoogleSignIn(deps);
    await vi.waitFor(() => expect(deps.openExternal).toHaveBeenCalled());
    lo.resolveCb({ code: 'CODE', state: 'WRONG' });
    const result = await p;
    expect(result).toEqual({ kind: 'error', code: 'oauth_provider_error:state_mismatch' });
  });

  it('exports a 5-minute default flow timeout constant', () => {
    expect(GOOGLE_FLOW_TIMEOUT_MS).toBe(5 * 60 * 1000);
  });
});
