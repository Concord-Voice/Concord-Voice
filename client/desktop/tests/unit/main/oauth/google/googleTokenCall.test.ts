// @vitest-environment node
import { describe, it, expect, vi } from 'vitest';
import {
  googleTokenCall,
  GOOGLE_TOKEN_ENDPOINT,
} from '../../../../../src/main/oauth/google/googleTokenCall';

function res(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'content-type': 'application/json' },
  });
}
const base = {
  code: 'c',
  codeVerifier: 'v',
  clientId: 'id',
  clientSecret: 'sek',
  redirectUri: 'http://127.0.0.1:5000/oauth/callback',
  signal: new AbortController().signal,
};

describe('googleTokenCall', () => {
  it('POSTs form-encoded params and returns id_token', async () => {
    const fetchFn = vi.fn(async (_u: unknown, init?: RequestInit) => {
      const body = new URLSearchParams(init!.body as string);
      expect(body.get('grant_type')).toBe('authorization_code');
      expect(body.get('code_verifier')).toBe('v');
      expect(body.get('client_secret')).toBe('sek');
      return res(200, { id_token: 'ID' });
    }) as unknown as typeof fetch;
    const out = await googleTokenCall({ ...base, fetchFn });
    expect(out.idToken).toBe('ID');
    expect(fetchFn).toHaveBeenCalledWith(
      GOOGLE_TOKEN_ENDPOINT,
      expect.objectContaining({ method: 'POST' })
    );
  });
  it('maps 4xx to google_exchange_rejected', async () => {
    const fetchFn = (async () => res(400, { error: 'invalid_grant' })) as unknown as typeof fetch;
    await expect(googleTokenCall({ ...base, fetchFn })).rejects.toMatchObject({
      code: 'google_exchange_rejected',
    });
  });
  it('maps 5xx to google_unavailable', async () => {
    const fetchFn = (async () => res(503, {})) as unknown as typeof fetch;
    await expect(googleTokenCall({ ...base, fetchFn })).rejects.toMatchObject({
      code: 'google_unavailable',
    });
  });
  it('maps network error to google_unavailable', async () => {
    const fetchFn = (async () => {
      throw new TypeError('network');
    }) as unknown as typeof fetch;
    await expect(googleTokenCall({ ...base, fetchFn })).rejects.toMatchObject({
      code: 'google_unavailable',
    });
  });
  it('maps abort to sso_cancelled', async () => {
    const ac = new AbortController();
    ac.abort();
    const fetchFn = (async () => {
      const e = new Error('aborted');
      e.name = 'AbortError';
      throw e;
    }) as unknown as typeof fetch;
    await expect(googleTokenCall({ ...base, signal: ac.signal, fetchFn })).rejects.toMatchObject({
      code: 'sso_cancelled',
    });
  });
});
