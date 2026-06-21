/**
 * Broker call for the client-driven Apple exchange (#974, consuming #972).
 * POSTs the flow's state to the control-plane client-secret broker and
 * returns the 60-second ES256 client_secret JWT Apple's /auth/token demands.
 *
 * SECURITY: the returned secret lives only in the appleFlow closure — never
 * logged, never IPC'd. Failures map to stable taxonomy codes; response
 * bodies are never attached to thrown errors (no Error.cause chains —
 * [internal]rules/observability.md).
 *
 * Status mapping (spec §Failure taxonomy + plan deviation D3):
 *   401 → sso_state_expired   (state consumed/expired — new flow)
 *   429 → sso_rate_limited    (backoff)
 *   5xx / network / bad body → sso_initiate_failed (new flow)
 *   abort → sso_cancelled
 */
import { AppleFlowError } from './errors';

export async function signClientSecret(opts: {
  apiBase: string;
  state: string;
  fetchFn: typeof fetch;
  signal: AbortSignal;
}): Promise<string> {
  let res: Response;
  try {
    res = await opts.fetchFn(`${opts.apiBase}/api/v1/auth/sso/apple/sign-client-secret`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ state: opts.state }),
      signal: opts.signal,
    });
  } catch (err) {
    if ((err as Error).name === 'AbortError') {
      throw new AppleFlowError('sso_cancelled', 'broker');
    }
    throw new AppleFlowError('sso_initiate_failed', 'broker');
  }
  if (res.status === 401) throw new AppleFlowError('sso_state_expired', 'broker');
  if (res.status === 429) throw new AppleFlowError('sso_rate_limited', 'broker');
  if (!res.ok) throw new AppleFlowError('sso_initiate_failed', 'broker');

  const body = (await res.json().catch(() => null)) as { client_secret?: unknown } | null;
  if (!body || typeof body.client_secret !== 'string' || body.client_secret.length === 0) {
    throw new AppleFlowError('sso_initiate_failed', 'broker');
  }
  return body.client_secret;
}
