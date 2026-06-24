/**
 * Token Manager — Main process secure token storage and refresh
 *
 * Uses Electron's safeStorage API (macOS Keychain, Windows DPAPI, Linux libsecret)
 * to encrypt the refresh token at rest. The refresh token never enters the
 * renderer process — only the short-lived access token is returned via IPC.
 *
 * Architecture:
 * - Refresh token: encrypted on disk + held in main process memory
 * - Access token: returned to renderer via IPC, memory-only (never persisted)
 * - Token refresh: main process makes HTTP calls via net.fetch()
 * - Tamper detection: safeStorage.decryptString() throws on corrupted ciphertext
 */

import { app, safeStorage, net } from 'electron';
import path from 'node:path';
import fs from 'node:fs';
import { getMachineId } from './machineId';
import type { RefreshResult } from './ipcContract';

// ─── Module State (never leaves this process) ────────────────────────

type E2EEKeyMaterial = {
  wrappingKeyBase64: string;
  preferencesKeyBase64: string;
  wrappedPrivateKeyBase64: string;
};

let inMemoryRefreshToken: string | null = null;
let inMemoryRememberMe = true;
let inMemoryApiBase = '';
let cachedAccessToken: string | null = null;
// Session-only (rememberMe=false) E2EE key material lives here and ONLY here —
// never on disk — so it survives a renderer soft reload (the main process
// persists across the reload) while honoring the "no session-only key material
// on disk/localStorage" invariant (#1870). Mirrors inMemoryRefreshToken.
let inMemoryE2EEKeys: E2EEKeyMaterial | null = null;
let refreshPromise: Promise<RefreshResult> | null = null;

export function getCachedAccessToken(): string | null {
  return cachedAccessToken;
}

export function getApiBaseOrigin(): string | null {
  if (!inMemoryApiBase) return null;
  try {
    return new URL(inMemoryApiBase).origin;
  } catch {
    return null;
  }
}

// ─── Proactive Refresh State (#254) ──────────────────────────────────
// Main process timer — immune to Chromium's renderer throttling during
// minimize/background/sleep. The renderer's own proactive timer remains
// as a secondary layer.

const PROACTIVE_BUFFER_SECONDS = 60; // Refresh 60s before JWT expiry
const MIN_PROACTIVE_INTERVAL_MS = 10_000; // Rate limit: max 1 proactive refresh per 10s

let proactiveTimer: ReturnType<typeof setTimeout> | null = null;
let proactiveRefreshCallback: ((accessToken: string, sessionId?: string) => void) | null = null;
let lastProactiveRefreshTimestamp = 0;

const TOKEN_FILE = path.join(app.getPath('userData'), 'secure-token.dat');
const META_FILE = path.join(app.getPath('userData'), 'token-meta.json');
const E2EE_FILE = path.join(app.getPath('userData'), 'secure-e2ee.dat');

// ─── Helpers ─────────────────────────────────────────────────────────

function canPersist(): boolean {
  return safeStorage.isEncryptionAvailable();
}

function writeMeta(apiBase: string, rememberMe: boolean): void {
  try {
    fs.writeFileSync(META_FILE, JSON.stringify({ apiBase, rememberMe }), 'utf-8');
  } catch (err) {
    console.error('[TokenManager] Failed to write meta file:', (err as Error).message);
  }
}

function readMeta(): { apiBase: string; rememberMe: boolean } | null {
  try {
    const raw = fs.readFileSync(META_FILE, 'utf-8');
    return JSON.parse(raw);
  } catch {
    return null;
  }
}

function deleteFiles(): void {
  try {
    fs.unlinkSync(TOKEN_FILE);
  } catch {
    /* no-op */
  }
  try {
    fs.unlinkSync(META_FILE);
  } catch {
    /* no-op */
  }
  try {
    fs.unlinkSync(E2EE_FILE);
  } catch {
    /* no-op */
  }
}

// ─── Proactive Refresh (#254) ────────────────────────────────────────

/**
 * Decode the `exp` claim from a JWT access token.
 * JWTs are base64url-encoded — no secret needed to read the payload.
 */
function decodeJwtExp(token: string): number | null {
  try {
    const parts = token.split('.');
    if (parts.length !== 3) return null;
    const payload = Buffer.from(parts[1], 'base64url').toString('utf-8');
    const claims = JSON.parse(payload) as { exp?: number };
    return typeof claims.exp === 'number' ? claims.exp : null;
  } catch {
    return null;
  }
}

/**
 * Schedule a proactive token refresh based on the JWT's exp claim.
 * Called after every successful refresh (renderer-initiated or proactive).
 */
function scheduleProactiveTimer(delayMs: number): void {
  proactiveTimer = setTimeout(() => {
    proactiveTimer = null;
    void doProactiveRefresh();
  }, delayMs);
}

function scheduleProactiveRefresh(accessToken: string): void {
  if (proactiveTimer) {
    clearTimeout(proactiveTimer);
    proactiveTimer = null;
  }

  const exp = decodeJwtExp(accessToken);
  if (!exp) return;

  const nowSeconds = Math.floor(Date.now() / 1000);
  const delaySeconds = exp - nowSeconds - PROACTIVE_BUFFER_SECONDS;

  if (delaySeconds > 0) {
    console.debug(
      `[TokenManager] Proactive refresh scheduled in ${Math.round(delaySeconds / 60)}m ${delaySeconds % 60}s`
    );
    scheduleProactiveTimer(delaySeconds * 1000);
    return;
  }

  // Token already near expiry — refresh immediately (rate-limited)
  const sinceLastRefresh = Date.now() - lastProactiveRefreshTimestamp;
  if (sinceLastRefresh < MIN_PROACTIVE_INTERVAL_MS) {
    // Recently refreshed — schedule after cooldown to avoid tight loop
    const retryMs = MIN_PROACTIVE_INTERVAL_MS - sinceLastRefresh;
    console.debug(`[TokenManager] Token near expiry, retrying in ${retryMs}ms (rate-limited)`);
    scheduleProactiveTimer(retryMs);
    return;
  }

  console.debug('[TokenManager] Token near expiry, refreshing immediately');
  void doProactiveRefresh();
}

/**
 * Execute a proactive refresh from the main process timer or powerMonitor resume.
 * On success, notifies the renderer via the registered callback.
 * On failure, schedules a retry after the cooldown window so the main process
 * layer doesn't go silent while the renderer may be throttled.
 */
async function doProactiveRefresh(): Promise<void> {
  lastProactiveRefreshTimestamp = Date.now();
  const result = await performRefresh();
  if (result.status === 'ok' && result.accessToken) {
    console.debug('[TokenManager] Proactive refresh succeeded');
    proactiveRefreshCallback?.(result.accessToken, result.sessionId);
  } else if (inMemoryRefreshToken && inMemoryApiBase) {
    // Refresh failed but we still have credentials — schedule a retry
    console.warn(`[TokenManager] Proactive refresh failed (${result.status}), retrying in 10s`);
    scheduleProactiveTimer(MIN_PROACTIVE_INTERVAL_MS);
  }
}

/**
 * Register a callback to notify the renderer when a proactive refresh
 * (timer or sleep/wake) produces a new access token.
 * Renderer-initiated refreshes return the token via IPC response instead.
 */
export function setProactiveRefreshCallback(
  cb: (accessToken: string, sessionId?: string) => void
): void {
  proactiveRefreshCallback = cb;
}

/**
 * Cancel the proactive refresh timer.
 */
export function stopProactiveRefresh(): void {
  if (proactiveTimer) {
    clearTimeout(proactiveTimer);
    proactiveTimer = null;
  }
}

/**
 * Handle system resume from sleep — cancel stale timer and refresh with
 * rate-limit awareness. During sleep, the timer may have drifted past
 * the token's expiry window.
 */
export function onSystemResume(): void {
  if (!inMemoryRefreshToken || !inMemoryApiBase) return;

  if (proactiveTimer) {
    clearTimeout(proactiveTimer);
    proactiveTimer = null;
  }

  // Respect rate limit — if we just refreshed, defer to cooldown
  const sinceLastRefresh = Date.now() - lastProactiveRefreshTimestamp;
  if (sinceLastRefresh < MIN_PROACTIVE_INTERVAL_MS) {
    const retryMs = MIN_PROACTIVE_INTERVAL_MS - sinceLastRefresh;
    console.debug(`[TokenManager] System resumed, refreshing in ${retryMs}ms (rate-limited)`);
    scheduleProactiveTimer(retryMs);
    return;
  }

  console.debug('[TokenManager] System resumed from sleep, refreshing token');
  void doProactiveRefresh();
}

// ─── Public API ──────────────────────────────────────────────────────

/**
 * Store the refresh token securely.
 * - If rememberMe=true and safeStorage is available: encrypt to disk + hold in memory
 * - If rememberMe=false: memory-only, delete any disk files
 * - If safeStorage unavailable: memory-only (user re-logins on restart)
 */
export function storeRefreshToken(data: {
  refreshToken: string;
  rememberMe: boolean;
  apiBase: string;
  accessToken?: string;
}): void {
  inMemoryRefreshToken = data.refreshToken;
  inMemoryRememberMe = data.rememberMe;
  inMemoryApiBase = data.apiBase;
  cachedAccessToken = data.accessToken ?? null;

  if (!data.rememberMe) {
    // Session-only: clear any persisted token.
    // Key-material audit: previously logged the token's last-8 chars + a
    // sha256 fingerprint — removed to keep refresh-token bytes off stdout.
    deleteFiles();
    return;
  }

  if (!canPersist()) {
    // safeStorage unavailable (rare Linux without keyring) — memory-only
    console.warn('[TokenManager] safeStorage unavailable, token will not persist across restarts');
    return;
  }

  try {
    const encrypted = safeStorage.encryptString(data.refreshToken);
    fs.writeFileSync(TOKEN_FILE, encrypted);
    writeMeta(data.apiBase, data.rememberMe);

    // Verify disk round-trip: read back and compare.
    // Key-material audit (#667): no token bytes in any log output, even rare-
    // path diagnostics. A mismatch on the decrypted-vs-intended comparison is
    // the signal on its own; emitting suffix bytes would give an operator no
    // actionable information while creating a session-correlation primitive
    // for anyone with access to stdout, crash dumps, or any other log sink.
    // See [internal]rules/e2ee.md "Private keys, channel keys, and session keys
    // NEVER logged" — refresh tokens are in scope.
    const readBack = fs.readFileSync(TOKEN_FILE);
    const decrypted = safeStorage.decryptString(readBack);
    if (decrypted !== data.refreshToken) {
      console.error(
        '[TokenManager] DISK ROUND-TRIP MISMATCH on refresh token (safeStorage integrity failure)'
      );
    }
  } catch (err) {
    console.error('[TokenManager] Failed to encrypt/write token:', (err as Error).message);
  }
}

/**
 * Restore the refresh token from disk on app startup.
 * Returns the token or an error status.
 */
export function restoreRefreshToken():
  | { status: 'ok'; token: string; apiBase: string; rememberMe: boolean }
  | { status: 'no_session' | 'tampered' | 'unavailable' } {
  if (inMemoryRefreshToken && inMemoryApiBase) {
    return {
      status: 'ok',
      token: inMemoryRefreshToken,
      apiBase: inMemoryApiBase,
      rememberMe: inMemoryRememberMe,
    };
  }

  if (!canPersist()) {
    console.debug('[TokenManager] restoreRefreshToken: safeStorage unavailable');
    return { status: 'unavailable' };
  }

  const meta = readMeta();
  if (!meta) {
    const tokenFileExists = fs.existsSync(TOKEN_FILE);
    console.debug(
      `[TokenManager] restoreRefreshToken: no meta file (token file exists: ${tokenFileExists})`
    );
    return { status: 'no_session' };
  }

  try {
    const encrypted = fs.readFileSync(TOKEN_FILE);
    const token = safeStorage.decryptString(encrypted);
    // Key-material audit: previously logged the token's last-8 chars + a
    // sha256 fingerprint plus rememberMe + apiBase — removed to keep
    // refresh-token bytes off stdout.
    inMemoryRefreshToken = token;
    inMemoryRememberMe = meta.rememberMe;
    inMemoryApiBase = meta.apiBase;
    return { status: 'ok', token, apiBase: meta.apiBase, rememberMe: meta.rememberMe };
  } catch (err) {
    // decryptString throws on tampered ciphertext (AES-GCM auth tag failure)
    console.error('[TokenManager] Token decryption failed (tampered?):', (err as Error).message);
    deleteFiles();
    return { status: 'tampered' };
  }
}

async function tryParseMfaChallenge(response: Response): Promise<RefreshResult | null> {
  if (response.status !== 403) return null;

  try {
    const errData = (await response.json()) as {
      error?: string;
      mfa_challenge_token?: string;
      methods?: string[];
      recovery_only_methods?: string[];
    };
    if (
      (errData.error === 'suspicious_session_mfa' || errData.error === 'mfa_upgrade_required') &&
      errData.mfa_challenge_token
    ) {
      console.warn(`[TokenManager] ${errData.error} — MFA required`);
      return {
        status: 'mfa_required',
        mfaChallengeToken: errData.mfa_challenge_token,
        mfaMethods: errData.methods || [],
        mfaRecoveryOnlyMethods: errData.recovery_only_methods || [],
      };
    }
  } catch {
    // Not JSON or no MFA data — fall through to generic failure
  }

  return null;
}

function persistRotatedToken(newRefreshToken: string): void {
  if (!inMemoryRememberMe || !canPersist()) {
    console.debug(
      `[TokenManager] Rotated token NOT persisted (rememberMe=${inMemoryRememberMe}, canPersist=${canPersist()})`
    );
    return;
  }

  try {
    const encrypted = safeStorage.encryptString(newRefreshToken);
    fs.writeFileSync(TOKEN_FILE, encrypted);
    // Key-material audit: previously logged the new refresh token's last-8
    // chars — removed to keep token bytes off stdout.
  } catch (err) {
    console.error('[TokenManager] Failed to re-encrypt rotated token:', (err as Error).message);
  }
}

/**
 * Perform a token refresh via the main process.
 * Deduplicates concurrent calls. Returns only the access token.
 */
export function performRefresh(): Promise<RefreshResult> {
  if (refreshPromise) {
    return refreshPromise;
  }

  refreshPromise = (async () => {
    if (!inMemoryRefreshToken || !inMemoryApiBase) {
      return { status: 'no_token' };
    }

    try {
      // Key-material audit (#667): no token bytes in any log output, including
      // failure-path console.warn. HTTP status + error classification are
      // sufficient diagnostics; deriving a suffix correlation handle from
      // inMemoryRefreshToken violates [internal]rules/e2ee.md.
      const response = await net.fetch(`${inMemoryApiBase}/api/v1/auth/refresh`, {
        method: 'POST',
        headers: {
          'X-Refresh-Token': inMemoryRefreshToken,
          'X-Machine-Id': getMachineId(),
        },
        // Omit cookies — Chromium's persistent cookie store may contain a stale
        // refresh_token cookie from a previous session/login.  The server reads
        // cookies before the X-Refresh-Token header, so a stale cookie would
        // shadow the correct header value and cause a 401.
        credentials: 'omit',
      });

      if (!response.ok) {
        const mfaResult = await tryParseMfaChallenge(response);
        if (mfaResult) return mfaResult;
        console.warn(`[TokenManager] Refresh failed: HTTP ${response.status}`);
        return { status: 'refresh_failed' };
      }

      const data = (await response.json()) as {
        access_token?: string;
        refresh_token?: string;
        session_id?: string;
      };
      const newAccessToken = data.access_token;
      const newRefreshToken = data.refresh_token;
      const newSessionId = data.session_id;

      if (!newAccessToken) {
        return { status: 'refresh_failed' };
      }

      // Rotate: update in-memory refresh token and re-encrypt to disk.
      // Key-material audit: previously logged suffix of both old and new
      // refresh tokens — removed to keep token bytes off stdout.
      if (newRefreshToken) {
        inMemoryRefreshToken = newRefreshToken;
        persistRotatedToken(newRefreshToken);
      }

      // Schedule next proactive refresh based on new token's exp (#254)
      scheduleProactiveRefresh(newAccessToken);

      cachedAccessToken = newAccessToken;
      return { status: 'ok', accessToken: newAccessToken, sessionId: newSessionId };
    } catch (err) {
      console.error('[TokenManager] Refresh request failed:', (err as Error).message);
      return { status: 'refresh_failed' };
    } finally {
      refreshPromise = null;
    }
  })();

  return refreshPromise;
}

/**
 * Perform logout — call server endpoint from main process, then clear everything.
 */
export async function performLogout(accessToken?: string): Promise<void> {
  if (!inMemoryApiBase) return;

  try {
    const headers: Record<string, string> = {};
    if (accessToken) {
      headers['Authorization'] = `Bearer ${accessToken}`;
    }
    if (inMemoryRefreshToken) {
      headers['X-Refresh-Token'] = inMemoryRefreshToken;
    }

    await net.fetch(`${inMemoryApiBase}/api/v1/auth/logout`, {
      method: 'POST',
      headers,
      credentials: 'omit',
    });
  } catch (err) {
    console.error('[TokenManager] Logout request failed:', (err as Error).message);
  }

  clearTokens();
}

/**
 * Clear all token state — in-memory and on disk.
 */
export function clearTokens(): void {
  stopProactiveRefresh();
  inMemoryRefreshToken = null;
  inMemoryRememberMe = true;
  inMemoryApiBase = '';
  cachedAccessToken = null;
  // Drop session-only E2EE key custody on logout/clear — the in-memory keys
  // must not outlive the session (CWE-212). performLogout() flows through here.
  inMemoryE2EEKeys = null;
  deleteFiles();
}

// ─── E2EE Key Persistence (safeStorage) ──────────────────────────────

/**
 * Store E2EE session keys encrypted via safeStorage.
 * Called after login/registration when E2EE service has been initialized.
 */
export function storeE2EEKeys(data: E2EEKeyMaterial): void {
  // Always hold the key material in main-process memory, regardless of
  // rememberMe, so a session-only user's E2EE keys survive a renderer soft
  // reload. This is memory only — the disk write below stays gated on
  // rememberMe, so session-only key material never touches disk (#1870).
  inMemoryE2EEKeys = data;

  if (!canPersist() || !inMemoryRememberMe) {
    return; // Only persist to disk if safeStorage available and rememberMe is on
  }

  try {
    const json = JSON.stringify(data);
    const encrypted = safeStorage.encryptString(json);
    fs.writeFileSync(E2EE_FILE, encrypted);
  } catch (err) {
    console.error('[TokenManager] Failed to encrypt/write E2EE keys:', (err as Error).message);
  }
}

/**
 * Restore E2EE session keys from safeStorage.
 * Returns the key material or null if unavailable.
 */
export function restoreE2EEKeys(): E2EEKeyMaterial | null {
  // Prefer the in-memory copy (set by storeE2EEKeys) so a session-only soft
  // reload restores keys that were never written to disk. Mirrors the
  // memory-first branch in restoreRefreshToken().
  if (inMemoryE2EEKeys) return inMemoryE2EEKeys;

  if (!canPersist()) return null;

  try {
    const encrypted = fs.readFileSync(E2EE_FILE);
    const json = safeStorage.decryptString(encrypted);
    return JSON.parse(json);
  } catch {
    return null;
  }
}

/**
 * Read the persisted API base URL from the token metadata file.
 * Returns the URL if available, null otherwise.
 * Used by the SPA loader to fetch client config before the renderer loads.
 */
export function getPersistedApiBase(): string | null {
  if (inMemoryApiBase) return inMemoryApiBase;
  const meta = readMeta();
  return meta?.apiBase || null;
}

export function getCapabilities(): { persistAvailable: boolean } {
  return { persistAvailable: canPersist() };
}

// ─── Test Helpers ────────────────────────────────────────────────────

/**
 * Reset all module-private mutable state for test isolation.
 * Follows the same pattern as apiClient._resetRefreshState().
 */
export function _resetForTesting(): void {
  stopProactiveRefresh();
  inMemoryRefreshToken = null;
  inMemoryRememberMe = true;
  inMemoryApiBase = '';
  cachedAccessToken = null;
  inMemoryE2EEKeys = null;
  refreshPromise = null;
  proactiveRefreshCallback = null;
  lastProactiveRefreshTimestamp = 0;
}
