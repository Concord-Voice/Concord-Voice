/**
 * Apple /auth/token exchange (#974, spec step 4). Form-POSTs the
 * authorization code + PKCE verifier + broker-signed client_secret and
 * returns the id_token.
 *
 * SECURITY: request and response bodies are never logged and never attached
 * to thrown errors. The access/refresh tokens Apple returns alongside the
 * id_token are deliberately dropped — Concord consumes OIDC identity only
 * (parity with the retired server-side exchangeCode).
 *
 * Status mapping (spec §Failure taxonomy):
 *   4xx → apple_exchange_rejected (new flow — the code/verifier pair is burned)
 *   5xx / network / bad body → apple_unavailable (orchestrator retries once)
 *   abort → sso_cancelled
 */
import { AppleFlowError } from './errors';

export const APPLE_TOKEN_ENDPOINT = 'https://appleid.apple.com/auth/token';

export async function appleTokenCall(opts: {
  code: string;
  codeVerifier: string;
  clientId: string;
  clientSecret: string;
  redirectUri: string;
  fetchFn: typeof fetch;
  signal: AbortSignal;
  /** Test seam — production callers omit. */
  endpoint?: string;
}): Promise<{ idToken: string }> {
  const form = new URLSearchParams({
    grant_type: 'authorization_code',
    code: opts.code,
    code_verifier: opts.codeVerifier,
    client_id: opts.clientId,
    client_secret: opts.clientSecret,
    redirect_uri: opts.redirectUri,
  });

  let res: Response;
  try {
    res = await opts.fetchFn(opts.endpoint ?? APPLE_TOKEN_ENDPOINT, {
      method: 'POST',
      headers: {
        'Content-Type': 'application/x-www-form-urlencoded',
        Accept: 'application/json',
      },
      body: form.toString(),
      signal: opts.signal,
    });
  } catch (err) {
    if ((err as Error).name === 'AbortError') {
      throw new AppleFlowError('sso_cancelled', 'apple_token');
    }
    throw new AppleFlowError('apple_unavailable', 'apple_token');
  }

  if (res.status >= 400 && res.status < 500) {
    throw new AppleFlowError('apple_exchange_rejected', 'apple_token');
  }
  if (!res.ok) {
    throw new AppleFlowError('apple_unavailable', 'apple_token');
  }

  const body = (await res.json().catch(() => null)) as { id_token?: unknown } | null;
  if (!body || typeof body.id_token !== 'string' || body.id_token.length === 0) {
    throw new AppleFlowError('apple_exchange_rejected', 'apple_token');
  }
  return { idToken: body.id_token };
}
