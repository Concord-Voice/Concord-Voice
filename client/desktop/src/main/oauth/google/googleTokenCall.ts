import { GoogleFlowError } from './errors';

export const GOOGLE_TOKEN_ENDPOINT = 'https://oauth2.googleapis.com/token';

export async function googleTokenCall(opts: {
  code: string;
  codeVerifier: string;
  clientId: string;
  clientSecret: string;
  redirectUri: string;
  fetchFn: typeof fetch;
  signal: AbortSignal;
  endpoint?: string; // test seam
}): Promise<{ idToken: string }> {
  const form = new URLSearchParams({
    grant_type: 'authorization_code',
    code: opts.code,
    code_verifier: opts.codeVerifier,
    client_id: opts.clientId,
    client_secret: opts.clientSecret,
    redirect_uri: opts.redirectUri,
  });
  let resp: Response;
  try {
    resp = await opts.fetchFn(opts.endpoint ?? GOOGLE_TOKEN_ENDPOINT, {
      method: 'POST',
      headers: { 'content-type': 'application/x-www-form-urlencoded' },
      body: form.toString(),
      signal: opts.signal,
    });
  } catch (err) {
    if (opts.signal.aborted || (err as Error)?.name === 'AbortError') {
      throw new GoogleFlowError('sso_cancelled', 'token');
    }
    throw new GoogleFlowError('google_unavailable', 'token');
  }
  if (resp.status >= 500) throw new GoogleFlowError('google_unavailable', 'token');
  if (resp.status >= 400) throw new GoogleFlowError('google_exchange_rejected', 'token');
  let body: { id_token?: unknown };
  try {
    body = (await resp.json()) as { id_token?: unknown };
  } catch {
    throw new GoogleFlowError('google_exchange_rejected', 'token-parse');
  }
  if (typeof body.id_token !== 'string' || body.id_token.length === 0) {
    throw new GoogleFlowError('google_exchange_rejected', 'token-shape');
  }
  return { idToken: body.id_token };
}
