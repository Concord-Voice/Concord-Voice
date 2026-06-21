/**
 * Google sign-in orchestrator (#975) — spec flow steps 2–7, main-process only.
 *
 *   pkce → loopback (flow-deadline timeout) → GET /initiate (challenge + nonce
 *   out, {auth_url, state, nonce} back) → system browser → loopback callback →
 *   constant-time state check → POST Google /token (client-driven PKCE, embedded
 *   non-confidential client_secret) → local jose verification (nonce binding) →
 *   POST /session → discriminated SSOSignInResult.
 *
 * Google is simpler than Apple: RFC 8252 loopback (no Worker bridge), plain-string
 * client_secret (no broker), no appleUserData, dual-issuer verification.
 *
 * Lifecycle: ONE AbortController + the loopback handle per flow, torn down by
 * (a) sso:googleCancel, (b) main-window 'closed', (c) the 5-minute overall
 * deadline, or (d) a superseding invocation. Single-flight by design.
 *
 * SECURITY:
 *   - The verifier, id_token, and token response exist only inside
 *     runGoogleSignIn's scope — never logged, never IPC'd (spec §Secrets lifetime).
 *   - This function NEVER rejects: every failure folds to
 *     { kind: 'error', code } so no raw Error (with a potential cause chain)
 *     crosses the IPC boundary ([internal]rules/observability.md).
 */
import type { SSOSignInErrorCode, SSOSignInResult } from '../../../shared/sso';
import type { startLoopback as startLoopbackFn, LoopbackHandle } from '../../ssoLoopback';

import { GoogleFlowError } from './errors';
import type { googleTokenCall as googleTokenCallFn } from './googleTokenCall';
import type { verifyGoogleIDToken as verifyGoogleIDTokenFn } from './idTokenVerifier';
import { codeChallengeS256, generateCodeVerifier } from '../pkce';
import {
  constantTimeEquals,
  mapSessionResponse as mapSessionResponseShared,
  toErrorCode as toErrorCodeShared,
} from '../ssoFlowShared';

/** Overall flow deadline (spec R2). */
export const GOOGLE_FLOW_TIMEOUT_MS = 5 * 60 * 1000;

export interface GoogleFlowDeps {
  apiBase: string;
  /**
   * The Google client_id is NOT a dep — it is parsed from the server-built
   * authorize URL (mirrors appleFlow), sourced from the control-plane's
   * GOOGLE_CLIENT_ID config. There is no client-side client_id constant.
   */
  clientSecret: string;
  /**
   * fetch for control-plane calls. MUST be Electron net.fetch bound with
   * credentials:'include' so /session's Set-Cookie (the refresh token) lands
   * in the default-session jar the renderer reads at /auth/refresh.
   */
  controlPlaneFetch: typeof fetch;
  /** fetch for Google endpoints (cookie-less by design). */
  googleFetch: typeof fetch;
  openExternal: (url: string) => Promise<void>;
  startLoopback: typeof startLoopbackFn;
  googleTokenCall: typeof googleTokenCallFn;
  verifyIdToken: typeof verifyGoogleIDTokenFn;
  /** Test seam — production callers omit (defaults to GOOGLE_FLOW_TIMEOUT_MS). */
  flowTimeoutMs?: number;
}

interface ActiveFlow {
  abort: AbortController;
  loopback: LoopbackHandle | null;
}

let activeFlow: ActiveFlow | null = null;

/**
 * Tears down the in-flight Google flow: renderer cancel (sso:googleCancel),
 * window close, deadline, or supersession. Idempotent — safe to call with
 * no flow active.
 */
export function cancelActiveGoogleFlow(): void {
  if (!activeFlow) return;
  const flow = activeFlow;
  activeFlow = null;
  flow.loopback?.close(); // rejects the loopback promise with oauth_cancelled
  flow.abort.abort(); // aborts any in-flight fetch
}

interface InitiateResponse {
  auth_url: string;
  state: string;
  nonce: string;
}

async function initiate(
  deps: GoogleFlowDeps,
  redirectURI: string,
  codeChallenge: string,
  signal: AbortSignal
): Promise<InitiateResponse> {
  let res: Response;
  try {
    const qs = new URLSearchParams({ redirect_uri: redirectURI, code_challenge: codeChallenge });
    res = await deps.controlPlaneFetch(`${deps.apiBase}/api/v1/auth/sso/google?${qs.toString()}`, {
      signal,
    });
  } catch (err) {
    if ((err as Error).name === 'AbortError')
      throw new GoogleFlowError('sso_cancelled', 'initiate');
    throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  }
  if (!res.ok) throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  const body = (await res.json().catch(() => null)) as Partial<InitiateResponse> | null;
  if (
    !body ||
    typeof body.auth_url !== 'string' ||
    typeof body.state !== 'string' ||
    typeof body.nonce !== 'string'
  ) {
    throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  }
  return body as InitiateResponse;
}

/**
 * Parses client_id out of the server-built Google authorize URL (mirrors
 * appleFlow.parseAuthUrl). The control-plane embeds GOOGLE_CLIENT_ID in the
 * authorize URL at /initiate time; the desktop never holds a client_id constant.
 */
function parseGoogleAuthUrl(authUrl: string): { clientId: string } {
  let parsed: URL;
  try {
    parsed = new URL(authUrl);
  } catch {
    throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  }
  if (parsed.protocol !== 'https:') throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  const clientId = parsed.searchParams.get('client_id') ?? '';
  if (!clientId) throw new GoogleFlowError('sso_initiate_failed', 'initiate');
  return { clientId };
}

async function postSession(
  deps: GoogleFlowDeps,
  args: { idToken: string; state: string },
  signal: AbortSignal
): Promise<Record<string, unknown>> {
  const payload: Record<string, string> = { id_token: args.idToken, state: args.state };
  let res: Response;
  try {
    res = await deps.controlPlaneFetch(`${deps.apiBase}/api/v1/auth/sso/google/session`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
      signal,
    });
  } catch (err) {
    if ((err as Error).name === 'AbortError') throw new GoogleFlowError('sso_cancelled', 'session');
    throw new GoogleFlowError('sso_session_rejected', 'session');
  }
  if (!res.ok) throw new GoogleFlowError('sso_session_rejected', 'session');
  const body = (await res.json().catch(() => null)) as Record<string, unknown> | null;
  if (!body) throw new GoogleFlowError('sso_session_rejected', 'session');
  return body;
}

/**
 * Maps the /session response into the IPC-safe discriminated union.
 * Exported for direct unit coverage. Delegates to the shared implementation
 * in ssoFlowShared.ts (response shapes are identical across providers).
 */
export function mapSessionResponse(body: Record<string, unknown>): SSOSignInResult {
  return mapSessionResponseShared(body);
}

function toErrorCode(err: unknown): SSOSignInErrorCode {
  return toErrorCodeShared(
    err,
    (e): e is GoogleFlowError => e instanceof GoogleFlowError,
    'googleFlow'
  );
}

export async function runGoogleSignIn(deps: GoogleFlowDeps): Promise<SSOSignInResult> {
  // Single-flight: a new invocation supersedes any in-flight flow.
  cancelActiveGoogleFlow();

  const abort = new AbortController();
  const flow: ActiveFlow = { abort, loopback: null };
  activeFlow = flow;

  const timeoutMs = deps.flowTimeoutMs ?? GOOGLE_FLOW_TIMEOUT_MS;
  const deadline = setTimeout(() => {
    if (activeFlow === flow) cancelActiveGoogleFlow();
  }, timeoutMs);

  try {
    const verifier = generateCodeVerifier();
    const challenge = codeChallengeS256(verifier);

    const loopback = await deps.startLoopback({ timeoutMs });
    flow.loopback = loopback;
    if (abort.signal.aborted) {
      loopback.close();
      throw new GoogleFlowError('sso_cancelled', 'loopback');
    }

    const init = await initiate(deps, loopback.redirectURI, challenge, abort.signal);
    const { clientId } = parseGoogleAuthUrl(init.auth_url);

    await deps.openExternal(init.auth_url);

    // Rejects with oauth_cancelled / oauth_timeout / oauth_provider_error:<x>.
    const cb = await loopback.promise;

    if (!constantTimeEquals(cb.state, init.state)) {
      // State mismatch — folded into the provider-error family (mirrors appleFlow D3).
      throw new GoogleFlowError('oauth_provider_error:state_mismatch', 'state');
    }

    const { idToken } = await deps.googleTokenCall({
      code: cb.code,
      codeVerifier: verifier,
      clientId,
      clientSecret: deps.clientSecret,
      redirectUri: loopback.redirectURI,
      fetchFn: deps.googleFetch,
      signal: abort.signal,
    });

    // Local gate only: a tampered token never leaves the device.
    await deps.verifyIdToken({
      idToken,
      clientId,
      expectedNonce: init.nonce,
      signal: abort.signal,
    });

    const sessionBody = await postSession(deps, { idToken, state: init.state }, abort.signal);
    return mapSessionResponse(sessionBody);
  } catch (err) {
    return { kind: 'error', code: toErrorCode(err) };
  } finally {
    clearTimeout(deadline);
    if (activeFlow === flow) {
      activeFlow = null;
      flow.loopback?.close();
    }
  }
}
