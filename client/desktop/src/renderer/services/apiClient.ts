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
import { apiUrl, getApiBase } from './runtimeServerBase';
import type { TerminalAttestationCode } from '../stores/attestationFailureStore';

export { API_BASE } from '../config';

// ─── Machine ID cache (for X-Machine-Id header, #89) ─────────────────
const cachedMachineIds = new Map<string, string>();

export async function ensureMachineId(): Promise<string> {
  const apiBase = getApiBase();
  const cachedMachineId = cachedMachineIds.get(apiBase);
  if (cachedMachineId) return cachedMachineId;
  if (globalThis.electron?.getMachineId) {
    const machineId = await globalThis.electron.getMachineId(apiBase);
    cachedMachineIds.set(apiBase, machineId);
    return machineId;
  }
  return '';
}

/** Synchronous accessor — returns '' until ensureMachineId() resolves */
export function getMachineIdSync(): string {
  return cachedMachineIds.get(getApiBase()) ?? '';
}

// ─── Proactive Token Refresh (#240-A) ────────────────────────────────
// Decode JWT exp claim and schedule refresh ~60s before expiry.
// This prevents 401s in normal operation — the reactive 401 handler below
// is belt-and-suspenders for clock skew, server restarts, etc.

const REFRESH_BUFFER_SECONDS = 60; // Refresh 60s before expiry
const MIN_REFRESH_INTERVAL_MS = 10_000; // Rate limit: max 1 refresh per 10s (#240-D)

let proactiveRefreshTimer: ReturnType<typeof setTimeout> | null = null;

export interface AuthLifecycleSnapshot {
  accessToken: string | null;
  sessionId: string | null;
}

interface RefreshCooldown {
  lifecycle: AuthLifecycleSnapshot;
  startedAt: number;
}

let refreshCooldown: RefreshCooldown | null = null;

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
  const active = rendererRefreshOperation;
  if (active !== null) {
    // Proactive refresh never owns logout/reset behavior, so it is safe to
    // observe any already-running refresh and let its normal timer reschedule.
    return active.promise;
  }
  const lifecycle = captureAuthLifecycle();
  if (refreshCooldownIsActive(lifecycle)) {
    return null;
  }
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
  refreshCooldown = null;
  cachedMachineIds.clear();
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
interface RendererRefreshOperation {
  lifecycle: AuthLifecycleSnapshot;
  promise: Promise<string | null>;
}

let rendererRefreshOperation: RendererRefreshOperation | null = null;

function captureAuthLifecycle(): AuthLifecycleSnapshot {
  const { accessToken, sessionId } = useAuthStore.getState();
  return { accessToken, sessionId };
}

function authLifecyclesMatch(left: AuthLifecycleSnapshot, right: AuthLifecycleSnapshot): boolean {
  if (left.sessionId !== null || right.sessionId !== null) {
    return left.sessionId !== null && left.sessionId === right.sessionId;
  }
  return left.accessToken === right.accessToken;
}

function refreshCooldownIsActive(lifecycle: AuthLifecycleSnapshot, now = Date.now()): boolean {
  return (
    refreshCooldown !== null &&
    authLifecyclesMatch(refreshCooldown.lifecycle, lifecycle) &&
    now - refreshCooldown.startedAt < MIN_REFRESH_INTERVAL_MS
  );
}

function updateRefreshCooldownOwner(origin: AuthLifecycleSnapshot): void {
  if (refreshCooldown === null || !authLifecyclesMatch(refreshCooldown.lifecycle, origin)) return;
  refreshCooldown = { ...refreshCooldown, lifecycle: captureAuthLifecycle() };
}

function authLifecycleIsCurrent(snapshot: AuthLifecycleSnapshot): boolean {
  const current = useAuthStore.getState();
  if (snapshot.sessionId !== null) {
    return current.accessToken !== null && current.sessionId === snapshot.sessionId;
  }
  return current.sessionId === null && current.accessToken === snapshot.accessToken;
}

function refreshedAuthLifecycleIsCurrent(
  snapshot: AuthLifecycleSnapshot,
  refreshedToken: string
): boolean {
  const current = useAuthStore.getState();
  return (
    current.accessToken === refreshedToken &&
    (snapshot.sessionId === null || current.sessionId === snapshot.sessionId)
  );
}

function requestLifecycleIsCurrent(
  snapshot: AuthLifecycleSnapshot,
  signal?: AbortSignal | null
): boolean {
  return signal?.aborted !== true && authLifecycleIsCurrent(snapshot);
}

function refreshedRequestLifecycleIsCurrent(
  snapshot: AuthLifecycleSnapshot,
  refreshedToken: string,
  signal?: AbortSignal | null
): boolean {
  return signal?.aborted !== true && refreshedAuthLifecycleIsCurrent(snapshot, refreshedToken);
}

/**
 * Handle MFA challenge during token refresh if the server flags a suspicious session.
 * Returns the new access token if MFA verification + retry succeeds, null otherwise.
 */
async function handleMfaChallengeIfNeeded(
  result: import('../../main/ipcContract').RefreshResult,
  lifecycle: AuthLifecycleSnapshot
): Promise<string | null> {
  if (result.status !== 'mfa_required' || !result.mfaChallengeToken) return null;
  if (!authLifecycleIsCurrent(lifecycle)) return null;

  const { useMFAChallengeStore } = await import('../stores/mfaChallengeStore');
  if (!authLifecycleIsCurrent(lifecycle)) return null;
  const mfaResult = await useMFAChallengeStore
    .getState()
    .showChallenge(
      result.mfaChallengeToken,
      result.mfaMethods || [],
      'suspicious_refresh',
      result.mfaRecoveryOnlyMethods || []
    );
  if (!mfaResult.verified || !authLifecycleIsCurrent(lifecycle)) return null;

  // MFA verified — retry the refresh. The cookie-path token from
  // electron.refreshToken() is authoritative (see spec §6.3 / §9). The
  // body's access_token from the verify response is a duplicate; we log a
  // warning if they diverge so future incidents are debuggable, but use the
  // IPC token regardless. Token VALUES are never logged — only the divergence
  // fact, per [internal]rules/observability.md.
  const retryResult = await globalThis.electron.refreshToken();
  if (!authLifecycleIsCurrent(lifecycle)) return null;
  if (retryResult.status === 'ok' && retryResult.accessToken) {
    if (
      lifecycle.sessionId !== null &&
      retryResult.sessionId !== undefined &&
      retryResult.sessionId !== lifecycle.sessionId
    ) {
      return null;
    }
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
async function performTokenRefresh(lifecycle: AuthLifecycleSnapshot): Promise<string | null> {
  if (!globalThis.electron?.refreshToken) return null;

  const result = await globalThis.electron.refreshToken();
  if (!authLifecycleIsCurrent(lifecycle)) return null;
  if (result.status === 'ok' && result.accessToken) {
    if (
      lifecycle.sessionId !== null &&
      result.sessionId !== undefined &&
      result.sessionId !== lifecycle.sessionId
    ) {
      return null;
    }
    useAuthStore.getState().setAccessToken(result.accessToken);
    if (result.sessionId) useAuthStore.getState().setSessionId(result.sessionId);
    return result.accessToken;
  }

  // Handle suspicious session MFA challenge
  return handleMfaChallengeIfNeeded(result, lifecycle);
}

export async function refreshAccessToken(): Promise<string | null> {
  const lifecycle = captureAuthLifecycle();
  const active = rendererRefreshOperation;
  if (active !== null) {
    if (authLifecyclesMatch(active.lifecycle, lifecycle)) return active.promise;
    await active.promise;
    if (!authLifecycleIsCurrent(lifecycle)) return null;
    return refreshAccessToken();
  }

  const promise = performTokenRefresh(lifecycle)
    .then((token) => {
      if (token !== null) updateRefreshCooldownOwner(lifecycle);
      return token;
    })
    .finally(() => {
      if (rendererRefreshOperation?.promise === promise) rendererRefreshOperation = null;
    });
  const operation: RendererRefreshOperation = { lifecycle, promise };
  rendererRefreshOperation = operation;
  refreshCooldown = { lifecycle, startedAt: Date.now() };
  return promise;
}

/**
 * Authenticated fetch wrapper.
 *
 * Usage: `apiFetch('/api/v1/messages/123', { method: 'PATCH', body: ... })`
 *
 * - Automatically adds Authorization header
 * - On 401, refreshes token via main process IPC and retries once
 * - Paths are relative to the active runtime API base (pass full path starting with /)
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
async function handleRefreshFailure(
  lifecycle: AuthLifecycleSnapshot,
  signal?: AbortSignal | null
): Promise<void> {
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return;

  // If recovery system is already handling this disconnect, don't double-reset
  const { useConnectionStore } = await import('../stores/connectionStore');
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return;
  const phase = useConnectionStore.getState().phase;
  if (phase !== 'stable') {
    if (requestLifecycleIsCurrent(lifecycle, signal)) {
      useAuthStore.getState().clearAccessToken();
    }
    return;
  }

  const { gracefulReset, nuclearReset } = await import('./resetService');
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return;
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
 * URL construction is centralized — `apiUrl(path)` happens exactly here
 * and nowhere else. `path` is the relative API route supplied by internal
 * callers (always `/api/v1/...` shaped). The function is the only place that
 * combines the active runtime API base with caller-supplied path.
 *
 * `credentials: 'include'` is non-negotiable for the auth cookie path.
 */
function apiFetchRaw(
  path: string,
  init: RequestInit | undefined,
  headers: Headers
): Promise<Response> {
  return fetch(apiUrl(path), {
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
  mid: string | null,
  lifecycle: AuthLifecycleSnapshot
): Promise<Response> {
  if (!requestLifecycleIsCurrent(lifecycle, init?.signal)) return response;
  // Both IPC calls route through the *Safe wrappers so a frame-validation or
  // other IPC failure degrades to "no fresh token → original 403" rather than
  // throwing out of the recovery path (no-rot consistency with apiFetch, #1527).
  await clearAttestationTokenSafe();
  if (!requestLifecycleIsCurrent(lifecycle, init?.signal)) return response;
  const fresh = await getAttestationTokenSafe();
  if (!fresh || !requestLifecycleIsCurrent(lifecycle, init?.signal)) return response;

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
  response: Response,
  lifecycle: AuthLifecycleSnapshot,
  signal?: AbortSignal | null
): Promise<Response> {
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return response;
  await globalThis.electron?.updater?.forceCheckForUpdates('attestation_required');
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return response;

  const { useAttestationFailureStore } = await import('../stores/attestationFailureStore');
  if (!requestLifecycleIsCurrent(lifecycle, signal)) return response;
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
  mid: string | null,
  lifecycle: AuthLifecycleSnapshot
): Promise<Response> {
  const body = await parseAttestationBody(response);
  if (!requestLifecycleIsCurrent(lifecycle, init?.signal)) return response;

  if (body.code !== null && ATTESTATION_REATTEST_CODES.has(body.code)) {
    return handleReattestPath(path, init, response, mid, lifecycle);
  }

  if (body.code !== null && isTerminalAttestationCode(body.code)) {
    return handleTerminalAttestationPath(body.code, body, response, lifecycle, init?.signal);
  }

  // Non-attestation 403 (RBAC denial, unknown code, non-JSON body, etc.).
  // Return untouched — existing callers depend on seeing the raw 403.
  return response;
}

interface TokenRefreshAttempt {
  token: string | null;
  rateLimited: boolean;
}

async function attempt401TokenRefresh(
  lifecycle: AuthLifecycleSnapshot
): Promise<TokenRefreshAttempt> {
  const active = rendererRefreshOperation;
  if (active !== null && authLifecyclesMatch(active.lifecycle, lifecycle)) {
    return { token: await active.promise, rateLimited: false };
  }

  if (refreshCooldownIsActive(lifecycle)) {
    return { token: null, rateLimited: true };
  }

  return { token: await refreshAccessToken(), rateLimited: false };
}

async function build401RetryHeaders(
  init: RequestInit | undefined,
  newToken: string,
  mid: string | null
): Promise<Headers> {
  const headers = new Headers(init?.headers);
  headers.set('Authorization', `Bearer ${newToken}`);
  const sessionId = useAuthStore.getState().sessionId;
  if (sessionId) headers.set('X-Session-ID', sessionId);
  if (mid) headers.set('X-Machine-Id', mid);
  const attToken = await getAttestationTokenSafe();
  if (attToken) headers.set('X-Attestation-Token', attToken);
  return headers;
}

/**
 * Attempt to recover from a 401 response by refreshing the token and retrying.
 * Returns the retried response on success, or the original 401 on failure.
 */
async function handle401Recovery(
  path: string,
  init: RequestInit | undefined,
  response: Response,
  mid: string | null,
  authoritative: boolean,
  lifecycle: AuthLifecycleSnapshot
): Promise<Response> {
  // The response and any recovery work belong to the auth lifecycle that
  // issued the original request. Never let a held 401 adopt a later account.
  if (lifecycle.accessToken === null) return response;
  if (!requestLifecycleIsCurrent(lifecycle, init?.signal)) return response;

  const refreshAttempt = await attempt401TokenRefresh(lifecycle);
  if (refreshAttempt.rateLimited) {
    // Recently refreshed and no in-flight refresh — likely token revocation.
    // Only an authoritative request may act on that signal and tear down the
    // session; background sync returns the original 401 and degrades quietly.
    if (authoritative) await handleRefreshFailure(lifecycle, init?.signal);
    return response;
  }

  const newToken = refreshAttempt.token;
  if (!newToken) {
    // Refresh failed. Same authority rule as the cooldown branch above: only an
    // authoritative Concord API call may tear down the session on a 401.
    // Non-authoritative surfaces (content proxies #1957, encrypted preferences
    // sync #1956) return the raw 401 and never log the user out.
    if (authoritative && requestLifecycleIsCurrent(lifecycle, init?.signal)) {
      await handleRefreshFailure(lifecycle, init?.signal);
    }
    return response;
  }

  if (!refreshedRequestLifecycleIsCurrent(lifecycle, newToken, init?.signal)) {
    return response;
  }

  // Retry with new token. The retry rebuilds headers from `init?.headers`,
  // which drops the Authorization / X-Session-ID / X-Machine-Id / X-Attestation-Token
  // values set by the original apiFetch call (those were attached to the request
  // Headers, not echoed back into `init`). Re-attach all four so the retried
  // request matches the original surface — otherwise an attestation-enabled
  // server will 403 the retry because it cannot locate the per-session token
  // record without X-Session-ID, and would also reject a missing X-Attestation-Token.
  // Pull the current cached attestation token and rebuild all transient auth
  // headers. The lifecycle is checked again after this asynchronous lookup.
  const retryHeaders = await build401RetryHeaders(init, newToken, mid);
  if (!refreshedRequestLifecycleIsCurrent(lifecycle, newToken, init?.signal)) {
    return response;
  }
  return apiFetchRaw(path, init, retryHeaders);
}

/**
 * @param opts.authoritative Whether a 401 from this request may tear down the
 *   session (default `true`). User-action Concord API calls are authoritative.
 *   Pass `false` for non-authoritative surfaces whose 401 is not proof the
 *   session is dead: third-party content proxies (KLIPY, #1957) and best-effort
 *   background first-party sync (encrypted preferences, #1956). These still
 *   attempt one refresh+retry but never call handleRefreshFailure.
 */
export async function apiFetch(
  path: string,
  init?: RequestInit,
  opts?: { authoritative?: boolean }
): Promise<Response> {
  const authLifecycle = captureAuthLifecycle();
  const token = authLifecycle.accessToken;

  const headers = new Headers(init?.headers);
  if (token) {
    headers.set('Authorization', `Bearer ${token}`);
  }
  // X-Session-ID is required by the attestation middleware to look up the
  // per-session token record keyed by (session_id, machine_id). Omitting it
  // when attestation is enabled produces 403 ATTESTATION_MISSING / EXPIRED.
  // Read from authStore (populated by /auth/login and /auth/refresh responses).
  const sessionId = authLifecycle.sessionId;
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
    return handle403Attestation(path, init, response, mid, authLifecycle);
  }

  // If not 401, return as-is
  if (response.status !== 401) {
    return response;
  }

  return handle401Recovery(path, init, response, mid, opts?.authoritative ?? true, authLifecycle);
}

/**
 * Best-effort revoke of a server session whose client-side login/SSO flow
 * aborted on a logout-class teardown (E2EEInitTeardownError / wasTornDownSince).
 *
 * The teardown clears the RENDERER access token and the main-process/disk token
 * halves, but a server response that resolved *after* the teardown (e.g. a late
 * completeSSORegistration / login) has already minted a live refresh-token row
 * and set the HttpOnly `refresh_token` cookie — clearing local state does not
 * revoke that. This POSTs `/auth/logout` from the RENDERER as a DIRECT fetch
 * with `credentials: 'include'` so the browser jar carries the HttpOnly cookie;
 * the server revokes the row and clears the cookie via Set-Cookie
 * (extractRefreshToken reads the cookie, so no client-held refresh token is
 * needed — the main-process `electron.logout()` path can't help here because
 * it uses `credentials:'omit'` + an `X-Refresh-Token` the renderer-jar SSO
 * cookie never populates).
 *
 * Deliberately NOT apiFetch — see the inline comment at the fetch call for the
 * TOCTOU + cross-origin + bearer rationale. A 401 here can never recurse into
 * another teardown because the response never enters the auth-recovery path.
 * Failure is swallowed — the row expires on its own if the network is down.
 * See #2337 (Codex P1).
 */
/**
 * An aborted flow's session reference: the auth lifecycle snapshot plus the
 * runtime API base it authenticated against. The base matters on self-hosted
 * switches (Codex P1, #2337): HttpOnly cookies are per-origin, so an abort
 * continuation that runs after the user switched servers must revoke against
 * the ABORTED origin — the current origin's jar never held that cookie.
 */
export interface AbortedSessionRef extends AuthLifecycleSnapshot {
  apiBase?: string;
}

export async function revokeAbortedSession(aborted: AbortedSessionRef): Promise<void> {
  const currentBase = getApiBase();
  const abortedBase = aborted.apiBase ?? currentBase;
  if (abortedBase === currentBase) {
    // Bind the revoke to the aborted session (Codex P1, #2337). If a NEWER
    // live session now owns the HttpOnly cookie — the user retried sign-in
    // while this flow was unwinding and the second login succeeded — revoking
    // would log the good session out, and the aborted cookie was already
    // overwritten anyway. Decline. A torn-down/cleared store (the common
    // abort case) is NOT "newer": the aborted cookie is still the only one in
    // the jar, so proceed. The second clause deliberately does NOT require the
    // aborted session ID to be null: a successor SSO login installs a token
    // with NO session ID (useSSOFlow), so a mixed shape — aborted {A, S1},
    // current {B, null} — must still decline (Codex P1, #2337).
    //
    // Cross-origin aborts (self-hosted switch) skip this check entirely:
    // cookie jars are per-origin, so the current session cannot own the
    // aborted origin's cookie.
    const current = captureAuthLifecycle();
    const newerSessionOwnsCookie =
      (current.sessionId !== null && current.sessionId !== aborted.sessionId) ||
      (current.sessionId === null &&
        current.accessToken !== null &&
        current.accessToken !== aborted.accessToken);
    if (newerSessionOwnsCookie) return;
  }
  try {
    // Direct fetch, deliberately NOT apiFetch (Codex P1 pair, #2337):
    // (1) apiFetch awaits the attestation-token IPC before dispatch — a
    //     successor login could install its cookie during that await, and the
    //     logout would then revoke the successor's cookie (TOCTOU on the
    //     ownership check above, which is now synchronous with dispatch);
    // (2) apiFetch always targets the CURRENT runtime base, but a cross-origin
    //     abort must revoke against the ABORTED origin;
    // (3) the bearer must be the ABORTED session's own — the common abort case
    //     runs after the teardown cleared authStore, and without it the
    //     server's blacklistAccessToken no-ops, leaving the aborted 15-min
    //     access JWT accepted until expiry.
    // A 401 here can never recurse into teardown because nothing routes this
    // response through the auth-recovery path.
    await fetch(`${abortedBase}/api/v1/auth/logout`, {
      method: 'POST',
      credentials: 'include',
      ...(aborted.accessToken
        ? { headers: { Authorization: `Bearer ${aborted.accessToken}` } }
        : {}),
    });
  } catch {
    // best-effort — a failed revoke leaves the server row to expire naturally.
  }
}

/** Snapshot the current auth session, for binding a later revokeAbortedSession. */
export function captureAuthSession(): AbortedSessionRef {
  // Carry the runtime API base so a later revoke targets the origin this
  // session actually belongs to (self-hosted switch hazard — see AbortedSessionRef).
  return { ...captureAuthLifecycle(), apiBase: getApiBase() };
}
