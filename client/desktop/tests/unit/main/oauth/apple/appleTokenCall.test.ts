// @vitest-environment node
/**
 * Apple /auth/token call tests (#974): form encoding + status→taxonomy
 * mapping. fetch is injected — no network.
 */
import { describe, expect, it, vi } from 'vitest';

import { APPLE_TOKEN_ENDPOINT, appleTokenCall } from '@/main/oauth/apple/appleTokenCall';
import { AppleFlowError } from '@/main/oauth/apple/errors';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

const baseArgs = {
  code: 'auth-code-1',
  codeVerifier: 'verifier-1',
  clientId: 'chat.concordvoice.signin',
  clientSecret: 'broker-jwt',
  redirectUri: 'http://127.0.0.1:51620/oauth/callback',
};

function call(fetchFn: typeof fetch) {
  return appleTokenCall({ ...baseArgs, fetchFn, signal: new AbortController().signal });
}

async function expectCode(p: Promise<unknown>, code: string) {
  const err = (await p.then(
    () => null,
    (e) => e
  )) as AppleFlowError | null;
  expect(err).toBeInstanceOf(AppleFlowError);
  expect(err?.code).toBe(code);
}

describe('appleTokenCall', () => {
  it('form-POSTs the RFC 6749 fields to the Apple token endpoint', async () => {
    const fetchFn = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      expect(String(url)).toBe(APPLE_TOKEN_ENDPOINT);
      expect(init?.method).toBe('POST');
      expect((init?.headers as Record<string, string>)['Content-Type']).toBe(
        'application/x-www-form-urlencoded'
      );
      const form = new URLSearchParams(String(init?.body));
      expect(form.get('grant_type')).toBe('authorization_code');
      expect(form.get('code')).toBe('auth-code-1');
      expect(form.get('code_verifier')).toBe('verifier-1');
      expect(form.get('client_id')).toBe('chat.concordvoice.signin');
      expect(form.get('client_secret')).toBe('broker-jwt');
      expect(form.get('redirect_uri')).toBe('http://127.0.0.1:51620/oauth/callback');
      return jsonResponse(200, { id_token: 'idt-1', access_token: 'dropped' });
    }) as unknown as typeof fetch;

    await expect(call(fetchFn)).resolves.toEqual({ idToken: 'idt-1' });
  });

  it('maps 400 (invalid_grant et al.) to apple_exchange_rejected', async () => {
    const fetchFn = (async () => jsonResponse(400, { error: 'invalid_grant' })) as typeof fetch;
    await expectCode(call(fetchFn), 'apple_exchange_rejected');
  });

  it('maps 5xx to apple_unavailable', async () => {
    const fetchFn = (async () => jsonResponse(503, { error: 'server_error' })) as typeof fetch;
    await expectCode(call(fetchFn), 'apple_unavailable');
  });

  it('maps network failure to apple_unavailable without leaking the cause', async () => {
    const fetchFn = (async () => {
      throw new TypeError('fetch failed: ETIMEDOUT');
    }) as typeof fetch;
    const err = (await call(fetchFn).then(
      () => null,
      (e) => e
    )) as AppleFlowError;
    expect(err.code).toBe('apple_unavailable');
    expect(err.message).toBe('apple_unavailable');
    expect((err as { cause?: unknown }).cause).toBeUndefined();
  });

  it('maps an aborted call to sso_cancelled', async () => {
    const controller = new AbortController();
    controller.abort();
    const fetchFn = (async (_u: RequestInfo | URL, init?: RequestInit) => {
      init?.signal?.throwIfAborted();
      return jsonResponse(200, { id_token: 'never' });
    }) as typeof fetch;
    const p = appleTokenCall({ ...baseArgs, fetchFn, signal: controller.signal });
    await expectCode(p, 'sso_cancelled');
  });

  it('rejects a 200 without id_token as apple_exchange_rejected', async () => {
    const fetchFn = (async () => jsonResponse(200, { access_token: 'only' })) as typeof fetch;
    await expectCode(call(fetchFn), 'apple_exchange_rejected');
  });
});
