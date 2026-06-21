/**
 * API Client - Authenticated fetch wrapper with automatic token refresh
 *
 * Wraps native fetch to:
 * - Auto-inject Authorization header from authStore
 * - Proactively refresh the access token ~60s before JWT expiry (#240)
 * - On 401, attempt token refresh via main process IPC and retry once
 * - If refresh fails, clear tokens (triggers logout redirect)
 * - Rate-limit refresh calls to max 1 per 10s to prevent hammering
 *
 * The refresh token never enters the renderer process — the main process
 * handles refresh via safeStorage-encrypted token + net.fetch().
 */

import { useAuthStore } from '../stores/authStore';
import { API_BASE } from '../config';
import type { TerminalAttestationCode } from '../stores/attestationFailureStore';

export { API_BASE } from '../config';

// ─── Machine ID cache (for X-Machine-Id header, #89) ─────────────────
let cachedMachineId: string | null = null;

export async function ensureMachineId(): Promise<string> {
  if (cachedMachineId) return cachedMachineId;
  if (globalThis.electron?.getMachineId) {
    cachedMachineId = await globalThis.electron.getMachineId();
  }
  return cachedMachineId ?? '';
}

/** Synchronous accessor — returns '' until ensureMachineId() resolves */
export function getMachineIdSync(): string {
  return cachedMachineId ?? '';
}

// ─── Proactive Token Refresh (#240-A) ────────────────────────────────
// Decode JWT exp claim and schedule refresh ~60s before expiry.
// This prevents 401s in normal operation — the reactive 401 handler below
// is belt-and-suspenders for clock skew, server restarts, etc.

const REFRESH_BUFFER_SECONDS = 60; // Refresh 60s before expiry
const MIN_REFRESH_INTERVAL_MS = 10_000; // Rate limit: max 1 refresh per 10s (#240-D)

let proactiveRefreshTimer: ReturnType<typeof setTimeout> | null = null;
let lastRefreshTimestamp = 0;

/**
 * Decode the `exp` claim from a JWT access token without a library.
 * JWTs are base64url-encoded — no secret needed to read the payload.
 * Returns the Unix timestamp (seconds) or null if parsing fails.
 */
function decodeJwtExp(token: string): number | null {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return null;
    // base64url → base64 → decode (with padding)
    let payload = parts[1].replaceAll('-', '+').replaceAll('_', '/');
    // Pad to multiple of 4 — atob() requires proper padding
    const pad = payload.length % 4;
    if (pad === 2) payload += '==';
    else if (pad === 3) payload += '=';
    const json = atob(payload);
    const claims = JSON.parse(json) as { exp?: number };
    return typeof claims.exp === 'number' ? claims.exp : null;
  } catch {
    return null;
  }
}

/**
 * Schedule a proactive token refresh based on the JWT's exp claim.
 * Called automatically whenever a new access token is set in authStore.
 */
function scheduleProactiveRefresh(token: string | null): void {
  // Clear any existing timer
  if (proactiveRefreshTimer) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }

  if (!token) return;

  const exp = decodeJwtExp(token);
  if (!exp) return;

  const nowSeconds = Math.floor(Date.now() / 1000);
  const delaySeconds = exp - nowSeconds - REFRESH_BUFFER_SECONDS;

  if (delaySeconds <= 0) {
    // Token already expired or about to — refresh immediately (rate-limited).
    // If rate-limited, schedule a retry after the cooldown window.
    rateLimitedRefresh().then((result) => {
      if (result === null && useAuthStore.getState().accessToken === token) {
        // Rate-limited and token unchanged — retry after cooldown
        proactiveRefreshTimer = setTimeout(() => {
          proactiveRefreshTimer = null;
          void rateLimitedRefresh();
        }, MIN_REFRESH_INTERVAL_MS);
      }
    });
    return;
  }

  proactiveRefreshTimer = setTimeout(() => {
    proactiveRefreshTimer = null;
    void rateLimitedRefresh();
  }, delaySeconds * 1000);
}

/**
 * Perform a refresh with rate limiting (max 1 per 10s).
 * Prevents hammering the server if tokens are very short-lived or clock is skewed.
 */
async function rateLimitedRefresh(): Promise<string | null> {
  const now = Date.now();
  if (now - lastRefreshTimestamp < MIN_REFRESH_INTERVAL_MS) {
    return null;
  }
  lastRefreshTimestamp = now;
  return refreshAccessToken();
}

/** Stop proactive refresh (called on logout/cleanup). */
export function stopProactiveRefresh(): void {
  if (proactiveRefreshTimer) {
    clearTimeout(proactiveRefreshTimer);
    proactiveRefreshTimer = null;
  }
}

/** Reset rate limiter state (for tests only). */
export function _resetRefreshState(): void {
  lastRefreshTimestamp = 0;
  stopProactiveRefresh();
}

// Subscribe to authStore: whenever accessToken changes, reschedule proactive refresh.
// This fires on login, token refresh, and logout (null).
let _prevToken: string | null = null;
const _unsubscribeAuthStore = useAuthStore.subscribe((state) => {
  if (state.accessToken !== _prevToken) {
    _prevToken = state.accessToken;
    scheduleProactiveRefresh(state.accessToken);
  }
});

// ─── Main Process Proactive Refresh (#254) ───────────────────────────
// The main process schedules its own proactive timer (immune to Chromium
// throttling). When it refreshes, it pushes the new token here via IPC.
let _unsubscribeTokenRefreshed: (() => void) | undefined;
if (globalThis.electron?.onTokenRefreshed) {
  _unsubscribeTokenRefreshed = globalThis.electron.onTokenRefreshed((data) => {
    useAuthStore.getState().setAccessToken(data.accessToken);
    if (data.sessionId) useAuthStore.getState().setSessionId(data.sessionId);
  });
}

// Clean up subscription on Vite HMR to prevent duplicate subscriptions/timers
if (import.meta.hot) {
  import.meta.hot.dispose(() => {
    _unsubscribeAuthStore();
    _unsubscribeTokenRefreshed?.();
    stopProactiveRefresh();
  });
}

// ─── Renderer-level refresh deduplication ────────────────────────────
/**
 * Renderer-level refresh deduplication.
 *
 * When multiple apiFetch calls get 401 simultaneously (e.g. channel read +
 * StrictMode double-mount message fetches), they all try to refresh. Without
 * dedup, even though the main process deduplicates the actual HTTP call, the
 * "refresh failed → logout" path can fire multiple times or a second call can
 * race with the first's token rotation.
 *
 * This promise is shared: concurrent callers await the same in-flight refresh.
 */
let rendererRefreshPromise: Promise<string | null> | null = null;

/**
 * Handle MFA challenge during token refresh if the server flags a suspicious session.
 * Returns the new access token if MFA verification + retry succeeds, null otherwise.
 */
async function handleMfaChallengeIfNeeded(
  result: import('../../main/ipcContract').RefreshResult
): Promise<string | null> {
  if (result.status !== 'mfa_required' || !result.mfaChallengeToken) return null;

  const { useMFAChallengeStore } = await import('../stores/mfaChallengeStore');
  const mfaResult = await useMFAChallengeStore
    .getState()
    .showChallenge(
      result.mfaChallengeToken,
      result.mfaMethods || [],
      'suspicious_refresh',
      result.mfaRecoveryOnlyMethods || []
    );
  if (!mfaResult.verified) return null;

  // MFA verified — retry the refresh. The cookie-path token from
  // electron.refreshToken() is authoritative (see spec §6.3 / §9). The
  // body's access_token from the verify response is a duplicate; we log a
  // warning if they diverge so future incidents are debuggable, but use the
  // IPC token regardless. Token VALUES are never logged — only the divergence
  // fact, per [internal]rules/observability.md.
  const retryResult = await globalThis.electron.refreshToken();
  if (retryResult.status === 'ok' && retryResult.accessToken) {
    if (
      mfaResult.payload?.access_token &&
      mfaResult.payload.access_token !== retryResult.accessToken
    ) {
      console.warn('MFA verify token divergence: IPC token used, body token discarded');
    }
    useAuthStore.getState().setAccessToken(retryResult.accessToken);
    if (retryResult.sessionId) useAuthStore.getState().setSessionId(retryResult.sessionId);
    return retryResult.accessToken;
  }
  return null;
}

/**
 * Attempt to refresh the access token via the main process IPC.
 * The main process holds the refresh token securely and makes the HTTP call.
 * Concurrent calls from the renderer are deduplicated.
 */
export async function refreshAccessToken(): Promise<string | null> {
  if (rendererRefreshPromise) {
    return rendererRefreshPromise;
  }

  rendererRefreshPromise = (async () => {
    if (!globalThis.electron?.refreshToken) return null;

    const result = await globalThis.electron.refreshToken();
    if (result.status === 'ok' && result.accessToken) {
      useAuthStore.getState().setAccessToken(result.accessToken);
      if (result.sessionId) useAuthStore.getState().setSessionId(result.sessionId);
      return result.accessToken;
    }

    // Handle suspicious session MFA challenge
    return handleMfaChallengeIfNeeded(result);
  })();

  try {
    return await rendererRefreshPromise;
  } finally {
    rendererRefreshPromise = null;
  }
}

/**
 * Authenticated fetch wrapper.
 *
 * Usage: `apiFetch('/api/v1/messages/123', { method: 'PATCH', body: ... })`
 *
 * - Automatically adds Authorization header
 * - On 401, refreshes token via main process IPC and retries once
 * - Paths are relative to API_BASE (pass full path starting with /)
 */
/**
 * Safely parse a JSON response, handling non-JSON responses (e.g. Cloudflare HTML pages).
 * Throws a descriptive error instead of a cryptic "Unexpected token '<'" SyntaxError.
 */
export async function safeJson<T = unknown>(res: Response): Promise<T> {
  const contentType = res.headers.get('Content-Type') || '';
  // Accept application/json, application/problem+json, application/vnd.api+json, etc.
  if (!contentType.includes('json')) {
    const text = await res.text().catch(() => '');
    const preview = text.slice(0, 120);
    throw new Error(
      `Expected JSON but got ${contentType || 'unknown'} (HTTP ${res.status}): ${preview}`
    );
  }
  try {
    return await res.json();
  } catch {
    throw new Error(`Invalid JSON in response (HTTP ${res.status}, Content-Type: ${contentType})`);
  }
}

/**
 * Handle refresh failure by clearing auth state and optionally resetting stores.
 * Only triggers logout once — safe to call from concurrent 401 handlers.
 */
async function handleRefreshFailure(): Promise<void> {
  if (!useAuthStore.getState().accessToken) return;

  // If recovery system is already handling this disconnect, don't double-reset
  const { useConnectionStore } = await import('../stores/connectionStore');
  const phase = useConnectionStore.getState().phase;
  if (phase !== 'stable') {
    useAuthStore.getState().clearAccessToken();
    return;
  }

  const { gracefulReset, nuclearReset } = await import('./resetService');
  if (useAuthStore.getState().rememberMe) {
    gracefulReset();
    // DO NOT clear disk tokens — session can be restored on next launch
  } else {
    nuclearReset(); // already calls clearTokens() internally
  }
  useAuthStore.getState().clearAccessToken();
}

/**
 * Internal raw-fetch helper.
 *
 * Every API request in this module funnels through this single function so
 * URL construction is centralized — `${API_BASE}${path}` happens exactly here
 * and nowhere else. `path` is the relative API route supplied by internal
 * callers (always `/api/v1/...` shaped). The function is the only place that
 * concatenates the constant API_BASE with caller-supplied path.
 *
 * `credentials: 'include'` is non-negotiable for the auth cookie path.
 */
function apiFetchRaw(
  path: string,
  init: RequestInit | undefined,
  headers: Headers
): Promise<Response> {
  return fetch(`${API_BASE}${path}`, {
    ...init,
    headers,
    credentials: 'include',
  });
}

/**
 * Attestation error codes that trigger the silent re-attest + retry path.
 * These indicate a stale/invalid token that might succeed after a cache
 * clear and fresh fetch — the underlying build identity is still valid.
 */
const ATTESTATION_REATTEST_CODES = new Set([
  'ATTESTATION_EXPIRED',
  'ATTESTATION_INVALID',
  'ATTESTATION_MISSING',
]);

/**
 * Attestation error codes that indicate this build is permanently rejected.
 * No retry; surface the update modal and return the original 403 to the caller.
 *
 * Typed as `Set<TerminalAttestationCode>` so the Set acts as a structural
 * type-guard: `ATTESTATION_TERMINAL_CODES.has(code)` does not narrow `code`
 * (it's still `string`), but iterating against the typed Set keeps the
 * source-of-truth aligned with the store's TerminalAttestationCode union.
 * Narrowing happens at the use site via `isTerminalAttestationCode()`.
 */
const ATTESTATION_TERMINAL_CODES: ReadonlySet<TerminalAttestationCode> =
  new Set<TerminalAttestationCode>([
    'ATTESTATION_UNKNOWN_RELEASE',
    'CLIENT_VERSION_TOO_OLD',
    'ATTESTATION_REVOKED',
  ]);

/**
 * Type-narrow a string code to TerminalAttestationCode. Use at boundaries
 * where we know the code passed ATTESTATION_TERMINAL_CODES.has() but TS
 * cannot infer the narrowing from `Set<X>.has(string)`.
 */
function isTerminalAttestationCode(code: string): code is TerminalAttestationCode {
  return (ATTESTATION_TERMINAL_CODES as ReadonlySet<string>).has(code);
}

/**
 * Parsed shape of a 403 attestation failure body. Fields are all optional —
 * the server may omit any of them and a non-JSON body yields all-undefined.
 */
interface AttestationFailureBody {
  code: string | null;
  requiredMinVersion: string | undefined;
  downloadHelpUrl: string | undefined;
}

/**
 * Parse a 403 response body into the AttestationFailureBody shape.
 * Uses response.clone() so the original response body remains readable by
 * callers if we fall through and return the original response unchanged.
 */
async function parseAttestationBody(response: Response): Promise<AttestationFailureBody> {
  const body = (await response
    .clone()
    .json()
    .catch(() => ({}))) as Record<string, unknown>;
  return {
    code: typeof body.code === 'string' ? body.code : null,
    requiredMinVersion:
      typeof body.requiredMinVersion === 'string' ? body.requiredMinVersion : undefined,
    downloadHelpUrl: typeof body.downloadHelpUrl === 'string' ? body.downloadHelpUrl : undefined,
  };
}

/**
 * Build request headers for an attestation retry: bearer token (if present),
 * session ID (if present), machine ID (if present), and the fresh attestation
 * token. X-Session-ID is required for the server to locate the per-session
 * attestation token record (keyed by session_id + machine_id) — omitting it on
 * the retry guarantees a second 403 even with the fresh token.
 */
function buildAttestationRetryHeaders(
  init: RequestInit | undefined,
  mid: string | null,
  freshAttToken: string
): Headers {
  const headers = new Headers(init?.headers);
  const currentToken = useAuthStore.getState().accessToken;
  if (currentToken) headers.set('Authorization', `Bearer ${currentToken}`);
  const sessionId = useAuthStore.getState().sessionId;
  if (sessionId) headers.set('X-Session-ID', sessionId);
  if (mid) headers.set('X-Machine-Id', mid);
  headers.set('X-Attestation-Token', freshAttToken);
  return headers;
}

/**
 * Fetch the cached attestation token WITHOUT letting an IPC failure brick the
 * request. `X-Attestation-Token` is an OPTIONAL header — the server is the gate
 * that enforces attestation and returns 403 (handled separately) when it is
 * required. A rejected `attestation:get-token` IPC must therefore degrade to
 * "no token attached", never propagate up and fail the whole request.
 *
 * Defense-in-depth for the bundled-build outage: a sender-frame regression made
 * this IPC throw on every call, and the unguarded `await` cascaded an optional
 * header into total connectivity loss. The optional-chain still skips the call
 * entirely on the web/test path where `globalThis.electron` is undefined.
 */
async function getAttestationTokenSafe(): Promise<string | null> {
  try {
    return (await globalThis.electron?.attestation?.getToken()) ?? null;
  } catch (err) {
    console.warn(
      'Attestation token fetch failed; proceeding without X-Attestation-Token:',
      err instanceof Error ? err.message : 'unknown error'
    );
    return null;
  }
}

/**
 * Clear the cached attestation token WITHOUT letting an IPC failure propagate.
 * Same defense-in-depth rationale as getAttestationTokenSafe: clearing is a
 * best-effort side effect on the 403 re-attest path; if the IPC rejects (e.g. a
 * sender-frame regression), the caller falls through to "no fresh token" and
 * returns the original 403 unchanged. Optional-chained for the web/test path.
 */
async function clearAttestationTokenSafe(): Promise<void> {
  try {
    await globalThis.electron?.attestation?.clearToken();
  } catch (err) {
    console.warn(
      'Attestation token clear failed; continuing without re-attest:',
      err instanceof Error ? err.message : 'unknown error'
    );
  }
}

/**
 * Re-attest path: clear cached attestation token, fetch a fresh one, and
 * retry the request ONCE with the new token. If the mint returns null/empty
 * (inert mint milestone), return the original 403 unchanged.
 */
async function handleReattestPath(
  path: string,
  init: RequestInit | undefined,
  response: Response,
  mid: string | null
): Promise<Response> {
  // Both IPC calls route through the *Safe wrappers so a frame-validation or
  // other IPC failure degrades to "no fresh token → original 403" rather than
  // throwing out of the recovery path (no-rot consistency with apiFetch, #1527).
  await clearAttestationTokenSafe();
  const fresh = await getAttestationTokenSafe();
  if (!fresh) return response;

  // Retry ONCE with the fresh attestation token. Raw fetch — no recursion.
  const retryHeaders = buildAttestationRetryHeaders(init, mid, fresh);
  return apiFetchRaw(path, init, retryHeaders);
}

/**
 * Terminal path: this build is permanently rejected. Trigger an update check
 * and surface the failure modal via the store. Returns the original 403 so
 * callers see the unmodified server response.
 *
 * Accepts a pre-narrowed TerminalAttestationCode so the modal cannot be
 * opened with an unrecognized code that would render inappropriate UX.
 */
async function handleTerminalAttestationPath(
  code: TerminalAttestationCode,
  body: AttestationFailureBody,
  response: Response
): Promise<Response> {
  await globalThis.electron?.updater?.forceCheckForUpdates('attestation_required');

  const { useAttestationFailureStore } = await import('../stores/attestationFailureStore');
  useAttestationFailureStore.getState().showFailure({
    code,
    requiredMinVersion: body.requiredMinVersion,
    downloadHelpUrl: body.downloadHelpUrl,
  });

  return response;
}

/**
 * Handle a 403 response that may carry an attestation failure code.
 *
 * Routing:
 * - Re-attest codes (EXPIRED/INVALID/MISSING): clear cached token, fetch
 *   fresh token, retry ONCE. If fresh token is null/empty (mint is inert in
 *   this milestone), return original 403 without retry.
 * - Terminal codes (UNKNOWN_RELEASE/VERSION_TOO_OLD/REVOKED): trigger
 *   forceCheckForUpdates + surface failure modal; return original 403.
 * - Any other 403 (RBAC denial, non-attestation error, non-JSON body):
 *   return original 403 untouched — existing callers must keep working.
 *
 * Never loops back into apiFetch. Retry uses raw fetch exactly once.
 */
async function handle403Attestation(
  path: string,
  init: RequestInit | undefined,
  response: Response,
  mid: string | null
): Promise<Response> {
  const body = await parseAttestationBody(response);

  if (body.code !== null && ATTESTATION_REATTEST_CODES.has(body.code)) {
    return handleReattestPath(path, init, response, mid);
  }

  if (body.code !== null && isTerminalAttestationCode(body.code)) {
    return handleTerminalAttestationPath(body.code, body, response);
  }

  // Non-attestation 403 (RBAC denial, unknown code, non-JSON body, etc.).
  // Return untouched — existing callers depend on seeing the raw 403.
  return response;
}

/**
 * Attempt to recover from a 401 response by refreshing the token and retrying.
 * Returns the retried response on success, or the original 401 on failure.
 */
async function handle401Recovery(
  path: string,
  init: RequestInit | undefined,
  response: Response,
  mid: string | null
): Promise<Response> {
  // If auth already cleared, return original 401
  if (!useAuthStore.getState().accessToken) return response;

  // Attempt token refresh (deduplicated + rate-limited)
  let newToken: string | null = null;
  if (rendererRefreshPromise) {
    // Another 401 handler already started a refresh — piggyback on it
    newToken = await rendererRefreshPromise;
  } else {
    const now = Date.now();
    if (now - lastRefreshTimestamp < MIN_REFRESH_INTERVAL_MS) {
      // Recently refreshed and no in-flight refresh — likely token revocation
      return response;
    }
    lastRefreshTimestamp = now;
    newToken = await refreshAccessToken();
  }

  if (!newToken) {
    await handleRefreshFailure();
    return response;
  }

  // Retry with new token. The retry rebuilds headers from `init?.headers`,
  // which drops the Authorization / X-Session-ID / X-Machine-Id / X-Attestation-Token
  // values set by the original apiFetch call (those were attached to the request
  // Headers, not echoed back into `init`). Re-attach all four so the retried
  // request matches the original surface — otherwise an attestation-enabled
  // server will 403 the retry because it cannot locate the per-session token
  // record without X-Session-ID, and would also reject a missing X-Attestation-Token.
  const retryHeaders = new Headers(init?.headers);
  retryHeaders.set('Authorization', `Bearer ${newToken}`);
  const sessionId = useAuthStore.getState().sessionId;
  if (sessionId) retryHeaders.set('X-Session-ID', sessionId);
  if (mid) retryHeaders.set('X-Machine-Id', mid);
  // Pull the CURRENT cached attestation token; same source as apiFetch's initial
  // injection. getAttestationTokenSafe never throws (web/test path → null, IPC
  // failure → null+logged), so the 401-recovery retry can't be bricked by an
  // optional header — no-rot consistency with apiFetch (#1527).
  const attToken = await getAttestationTokenSafe();
  if (attToken) retryHeaders.set('X-Attestation-Token', attToken);
  return apiFetchRaw(path, init, retryHeaders);
}

export async function apiFetch(path: string, init?: RequestInit): Promise<Response> {
  const token = useAuthStore.getState().accessToken;

  const headers = new Headers(init?.headers);
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }
  // X-Session-ID is required by the attestation middleware to look up the
  // per-session token record keyed by (session_id, machine_id). Omitting it
  // when attestation is enabled produces 403 ATTESTATION_MISSING / EXPIRED.
  // Read from authStore (populated by /auth/login and /auth/refresh responses).
  const sessionId = useAuthStore.getState().sessionId;
  if (sessionId) {
    headers.set('X-Session-ID', sessionId);
  }
  const mid = getMachineIdSync();
  if (mid) {
    headers.set('X-Machine-Id', mid);
  }

  // Attach attestation token if present. getAttestationTokenSafe never throws —
  // it returns null on the web/test path (no electron bridge) AND on any IPC
  // failure, so an optional header can never brick the whole request.
  const attToken = await getAttestationTokenSafe();
  if (attToken) {
    headers.set('X-Attestation-Token', attToken);
  }

  const response = await apiFetchRaw(path, init, headers);

  // Intercept 403 attestation failures before the 401 path.
  if (response.status === 403) {
    return handle403Attestation(path, init, response, mid);
  }

  // If not 401, return as-is
  if (response.status !== 401) {
    return response;
  }

  return handle401Recovery(path, init, response, mid);
}
