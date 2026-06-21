// @vitest-environment node
/**
 * Broker-call tests (#974): status→taxonomy mapping per the spec table.
 * fetch is injected — no network, no MSW (main-process module).
 */
import { describe, expect, it, vi } from 'vitest';

import { AppleFlowError } from '@/main/oauth/apple/errors';
import { signClientSecret } from '@/main/oauth/apple/signClientSecret';

const API = 'https://api.test.example';

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function call(fetchFn: typeof fetch, signal?: AbortSignal) {
  return signClientSecret({
    apiBase: API,
    state: 'state-1',
    fetchFn,
    signal: signal ?? new AbortController().signal,
  });
}

async function expectCode(p: Promise<unknown>, code: string) {
  const err = (await p.then(
    () => null,
    (e) => e
  )) as AppleFlowError | null;
  expect(err).toBeInstanceOf(AppleFlowError);
  expect(err?.code).toBe(code);
}

describe('signClientSecret', () => {
  it('POSTs the state to the broker and returns the client_secret', async () => {
    const fetchFn = vi.fn(async (url: RequestInfo | URL, init?: RequestInit) => {
      expect(String(url)).toBe(`${API}/api/v1/auth/sso/apple/sign-client-secret`);
      expect(init?.method).toBe('POST');
      expect(JSON.parse(String(init?.body))).toEqual({ state: 'state-1' });
      return jsonResponse(200, { client_secret: 'jwt-abc', expires_in: 60 });
    }) as unknown as typeof fetch;

    await expect(call(fetchFn)).resolves.toBe('jwt-abc');
  });

  it('maps 401 to sso_state_expired', async () => {
    const fetchFn = (async () =>
      jsonResponse(401, { error_code: 'invalid_state' })) as typeof fetch;
    await expectCode(call(fetchFn), 'sso_state_expired');
  });

  it('maps 429 to sso_rate_limited', async () => {
    const fetchFn = (async () => jsonResponse(429, {})) as typeof fetch;
    await expectCode(call(fetchFn), 'sso_rate_limited');
  });

  it('maps 5xx to sso_initiate_failed (plan deviation D3)', async () => {
    const fetchFn = (async () =>
      jsonResponse(500, { error_code: 'internal_error' })) as typeof fetch;
    await expectCode(call(fetchFn), 'sso_initiate_failed');
  });

  it('maps network failure to sso_initiate_failed without leaking the cause', async () => {
    const fetchFn = (async () => {
      throw new TypeError('fetch failed: ECONNREFUSED 127.0.0.1:443');
    }) as typeof fetch;
    const err = (await call(fetchFn).then(
      () => null,
      (e) => e
    )) as AppleFlowError;
    expect(err.code).toBe('sso_initiate_failed');
    expect(err.message).toBe('sso_initiate_failed');
    expect((err as { cause?: unknown }).cause).toBeUndefined();
  });

  it('maps an aborted call to sso_cancelled', async () => {
    const controller = new AbortController();
    controller.abort();
    const fetchFn = (async (_u: RequestInfo | URL, init?: RequestInit) => {
      init?.signal?.throwIfAborted();
      return jsonResponse(200, {});
    }) as typeof fetch;
    await expectCode(call(fetchFn, controller.signal), 'sso_cancelled');
  });

  it('rejects a 200 with a malformed body as sso_initiate_failed', async () => {
    const fetchFn = (async () => jsonResponse(200, { nope: true })) as typeof fetch;
    await expectCode(call(fetchFn), 'sso_initiate_failed');
  });
});
