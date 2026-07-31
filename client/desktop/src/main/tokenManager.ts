/**
 * Token Manager — Main process secure token storage and refresh
 *
 * Uses Electron's safeStorage API (macOS Keychain, Windows DPAPI, Linux libsecret)
 * to encrypt the refresh token at rest. The refresh token never enters the
 * renderer process — IPC exposes only the short-lived access token, session
 * lineage, and an opaque credential owner for compare-and-clear operations.
 *
 * Architecture:
 * - Refresh token: encrypted on disk + held in main process memory
 * - Access token: returned to renderer via IPC, memory-only (never persisted)
 * - Token refresh: main process makes HTTP calls via net.fetch()
 * - Tamper detection: safeStorage.decryptString() throws on corrupted ciphertext
 */

import { safeStorage, net } from 'electron';
import path from 'node:path';
import fs from 'node:fs';
import { getMachineId } from './machineId';
import type { CredentialOwner, RefreshResult } from './ipcContract';
import {
  profileIdForApiBase,
  profilePathsForApiBase,
  type ProfilePaths,
} from './selfHostedProfile';

// ─── Module State (never leaves this process) ────────────────────────

// Main-process-local shape of the persisted E2EE key material. Structural mirror
// of the renderer's `E2EESessionKeys` (renderer/services/e2eeService.ts); the two
// meet at the `auth:storeE2EEKeys` IPC boundary by structural (JSON) compatibility,
// so the type is intentionally NOT shared — keeping main and renderer type domains
// decoupled. Keep the two shapes in sync if either gains a field.
type E2EEKeyMaterial = {
  wrappingKeyBase64: string;
  preferencesKeyBase64: string;
  wrappedPrivateKeyBase64: string;
};

type E2EEPersistenceState = 'pending' | 'ready';

interface TokenMeta {
  apiBase: string;
  rememberMe: boolean;
  credentialOwner?: CredentialOwner;
  e2eeState?: E2EEPersistenceState;
}

interface PersistedE2EEKeys {
  credentialOwner: CredentialOwner;
  keys: E2EEKeyMaterial;
}

interface StagedE2EEKeys {
  generation: number;
  keys: E2EEKeyMaterial;
}

interface RefreshOwnerSnapshot {
  generation: number;
  refreshToken: string;
  apiBase: string;
}

interface OwnedRefreshResult {
  result: RefreshResult;
  owner: RefreshOwnerSnapshot | null;
}

interface RefreshOperation {
  owner: RefreshOwnerSnapshot | null;
  promise: Promise<OwnedRefreshResult>;
}

let inMemoryRefreshToken: string | null = null;
let inMemoryRememberMe = true;
let inMemoryApiBase = '';
let cachedAccessToken: string | null = null;
// Session-only (rememberMe=false) E2EE key material lives here and ONLY here —
// never on disk — so it survives a renderer soft reload (the main process
// persists across the reload) while honoring the "no session-only key material
// on disk/localStorage" invariant (#1870). Mirrors inMemoryRefreshToken.
let inMemoryE2EEKeys: E2EEKeyMaterial | null = null;
let inMemoryE2EEOwner: CredentialOwner | null = null;
let inMemoryE2EEState: E2EEPersistenceState = 'pending';
let stagedE2EEKeys: StagedE2EEKeys | null = null;
let reservedCredentialOwner: CredentialOwner | null = null;
let allowLegacyE2EEMigration = false;
let credentialGeneration = 0;
let refreshOperation: RefreshOperation | null = null;

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
let proactiveRefreshCallback:
  ((accessToken: string, sessionId?: string, previousSessionId?: string) => void) | null = null;
let lastProactiveRefreshTimestamp = 0;

const DEFAULT_PROFILE_API_BASE = 'https://api.concordvoice.chat';

// ─── Helpers ─────────────────────────────────────────────────────────

function canPersist(): boolean {
  return safeStorage.isEncryptionAvailable();
}

function pathsForApiBase(apiBase: string): ProfilePaths {
  return profilePathsForApiBase(apiBase || DEFAULT_PROFILE_API_BASE);
}

function activePaths(): ProfilePaths {
  return pathsForApiBase(inMemoryApiBase || DEFAULT_PROFILE_API_BASE);
}

function snapshotCredentialOwner(): RefreshOwnerSnapshot | null {
  if (!inMemoryRefreshToken || !inMemoryApiBase) return null;
  return {
    generation: credentialGeneration,
    refreshToken: inMemoryRefreshToken,
    apiBase: inMemoryApiBase,
  };
}

function isCredentialOwnerCurrent(owner: RefreshOwnerSnapshot | null): boolean {
  return (
    owner !== null &&
    owner.generation === credentialGeneration &&
    owner.refreshToken === inMemoryRefreshToken &&
    owner.apiBase === inMemoryApiBase
  );
}

function credentialOwnersMatch(
  left: RefreshOwnerSnapshot | null,
  right: RefreshOwnerSnapshot | null
): boolean {
  if (left === null || right === null) return left === right;
  return (
    left.generation === right.generation &&
    left.refreshToken === right.refreshToken &&
    left.apiBase === right.apiBase
  );
}

function activeProfileFile(): string {
  return path.join(
    path.dirname(pathsForApiBase(DEFAULT_PROFILE_API_BASE).metaFile),
    'active-profile.json'
  );
}

function ensureParentDir(filePath: string): void {
  fs.mkdirSync(path.dirname(filePath), { recursive: true });
}

function writeMeta(
  apiBase: string,
  rememberMe: boolean,
  credentialOwner: CredentialOwner,
  e2eeState: E2EEPersistenceState
): boolean {
  const paths = pathsForApiBase(apiBase);
  try {
    ensureParentDir(paths.metaFile);
    fs.writeFileSync(
      paths.metaFile,
      JSON.stringify({
        apiBase,
        rememberMe,
        profileId: profileIdForApiBase(apiBase),
        credentialOwner,
        e2eeState,
      }),
      'utf-8'
    );
    fs.writeFileSync(activeProfileFile(), JSON.stringify({ apiBase }), 'utf-8');
    return true;
  } catch (err) {
    console.error('[TokenManager] Failed to write meta file:', (err as Error).message);
    return false;
  }
}

function readActiveApiBase(): string | null {
  try {
    const raw = fs.readFileSync(activeProfileFile(), 'utf-8');
    const parsed = JSON.parse(raw) as { apiBase?: unknown };
    return typeof parsed.apiBase === 'string' ? parsed.apiBase : null;
  } catch {
    return null;
  }
}

function readMeta(metaFile = pathsForApiBase(DEFAULT_PROFILE_API_BASE).metaFile): TokenMeta | null {
  try {
    const raw = fs.readFileSync(metaFile, 'utf-8');
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (typeof parsed.apiBase !== 'string' || typeof parsed.rememberMe !== 'boolean') return null;
    const credentialOwner = parsed.credentialOwner;
    const e2eeState = parsed.e2eeState;
    return {
      apiBase: parsed.apiBase,
      rememberMe: parsed.rememberMe,
      ...(typeof credentialOwner === 'number' &&
      Number.isSafeInteger(credentialOwner) &&
      credentialOwner > 0
        ? { credentialOwner }
        : {}),
      ...(e2eeState === 'pending' || e2eeState === 'ready' ? { e2eeState } : {}),
    };
  } catch {
    return null;
  }
}

function readActiveMeta(): TokenMeta | null {
  const activeApiBase = readActiveApiBase();
  if (activeApiBase) {
    const activeMeta = readMeta(pathsForApiBase(activeApiBase).metaFile);
    if (activeMeta) return activeMeta;
  }
  return readMeta();
}

function deleteE2EEFile(apiBase: string): void {
  try {
    fs.unlinkSync(pathsForApiBase(apiBase).e2eeFile);
  } catch {
    /* no-op */
  }
}

function nextCredentialOwner(apiBase?: string): CredentialOwner {
  const persistedOwner = apiBase
    ? (readMeta(pathsForApiBase(apiBase).metaFile)?.credentialOwner ?? 0)
    : 0;
  credentialGeneration = Math.max(credentialGeneration, persistedOwner);
  if (credentialGeneration >= Number.MAX_SAFE_INTEGER) {
    throw new Error('Credential owner space exhausted');
  }
  credentialGeneration += 1;
  return credentialGeneration;
}

function isE2EEKeyMaterial(value: unknown): value is E2EEKeyMaterial {
  if (typeof value !== 'object' || value === null || Array.isArray(value)) return false;
  const candidate = value as Record<string, unknown>;
  return (
    typeof candidate.wrappingKeyBase64 === 'string' &&
    candidate.wrappingKeyBase64.length > 0 &&
    typeof candidate.preferencesKeyBase64 === 'string' &&
    candidate.preferencesKeyBase64.length > 0 &&
    typeof candidate.wrappedPrivateKeyBase64 === 'string' && // pragma: allowlist secret
    candidate.wrappedPrivateKeyBase64.length > 0
  );
}

function deleteFiles(apiBase = inMemoryApiBase || DEFAULT_PROFILE_API_BASE): void {
  const paths = pathsForApiBase(apiBase);
  try {
    fs.unlinkSync(paths.tokenFile);
  } catch {
    /* no-op */
  }
  try {
    fs.unlinkSync(paths.metaFile);
  } catch {
    /* no-op */
  }
  try {
    fs.unlinkSync(paths.e2eeFile);
  } catch {
    /* no-op */
  }
  try {
    fs.unlinkSync(activeProfileFile());
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
  const outcome = await performOwnedRefresh();
  if (!isCredentialOwnerCurrent(outcome.owner)) return;

  const { result } = outcome;
  if (result.status === 'ok' && result.accessToken) {
    console.debug('[TokenManager] Proactive refresh succeeded');
    proactiveRefreshCallback?.(result.accessToken, result.sessionId, result.previousSessionId);
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
  cb: (accessToken: string, sessionId?: string, previousSessionId?: string) => void
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

interface StoreRefreshTokenInput {
  refreshToken: string;
  rememberMe: boolean;
  apiBase: string;
  accessToken?: string;
}

/** Publish one refresh credential under an already-current owner. */
function publishRefreshToken(
  data: StoreRefreshTokenInput,
  owner: CredentialOwner,
  stagedKeys: E2EEKeyMaterial | null = null
): CredentialOwner {
  // Invalidate predecessor key custody before publishing the successor token.
  // Disk state is marked pending before the token file is replaced, so a crash
  // at any later instruction can never pair the new credential with old keys.
  inMemoryE2EEKeys = null;
  inMemoryE2EEOwner = null;
  inMemoryE2EEState = 'pending';
  allowLegacyE2EEMigration = false;
  stagedE2EEKeys = null;
  reservedCredentialOwner = null;

  inMemoryRefreshToken = data.refreshToken;
  inMemoryRememberMe = data.rememberMe;
  inMemoryApiBase = data.apiBase;
  cachedAccessToken = data.accessToken ?? null;
  lastProactiveRefreshTimestamp = 0;
  if (data.accessToken) {
    scheduleProactiveRefresh(data.accessToken);
  } else {
    stopProactiveRefresh();
  }

  if (!data.rememberMe) {
    // Session-only: clear any persisted token and predecessor E2EE blob.
    // Key-material audit: previously logged the token's last-8 chars + a
    // sha256 fingerprint — removed to keep refresh-token bytes off stdout.
    deleteFiles(data.apiBase);
  } else if (!canPersist()) {
    // safeStorage unavailable (rare Linux without keyring) — memory-only
    console.warn('[TokenManager] safeStorage unavailable, token will not persist across restarts');
    deleteFiles(data.apiBase);
  } else if (writeMeta(data.apiBase, data.rememberMe, owner, 'pending')) {
    deleteE2EEFile(data.apiBase);
    try {
      const encrypted = safeStorage.encryptString(data.refreshToken);
      const paths = pathsForApiBase(data.apiBase);
      ensureParentDir(paths.tokenFile);
      fs.writeFileSync(paths.tokenFile, encrypted);

      // Verify disk round-trip without logging credential bytes.
      const readBack = fs.readFileSync(paths.tokenFile);
      const decrypted = safeStorage.decryptString(readBack);
      if (decrypted !== data.refreshToken) {
        console.error(
          '[TokenManager] DISK ROUND-TRIP MISMATCH on refresh token (safeStorage integrity failure)'
        );
      }
    } catch (err) {
      console.error('[TokenManager] Failed to encrypt/write token:', (err as Error).message);
      // Do not leave a predecessor token behind under the successor's pending
      // owner marker. The current process can continue memory-only, but a
      // restart must fail closed instead of resurrecting the prior credential.
      deleteFiles(data.apiBase);
    }
  } else {
    // writeMeta may have failed before or after a partial write. Remove every
    // profile artifact so restart cannot combine that partial successor state
    // with a predecessor token/E2EE blob.
    deleteFiles(data.apiBase);
  }

  // Registration can stage keys before its email-confirmation response mints
  // credentials. Adopt only a stage from the immediately preceding empty
  // generation; all credential-bearing flows must use the owner-scoped writer.
  if (stagedKeys) {
    void storeE2EEKeysIfOwner(stagedKeys, owner);
  }
  return owner;
}

/**
 * Store a new renderer-issued refresh credential securely.
 * Each call creates a new global owner and invalidates predecessor E2EE state.
 */
export function storeRefreshToken(data: StoreRefreshTokenInput): CredentialOwner {
  const stagedKeys =
    stagedE2EEKeys?.generation === credentialGeneration ? stagedE2EEKeys.keys : null;
  const owner = nextCredentialOwner(data.apiBase);
  return publishRefreshToken(data, owner, stagedKeys);
}

/** Reserve the global credential owner before an asynchronous SSO exchange. */
export function reserveCredentialOwner(apiBase: string): CredentialOwner {
  clearTokens();
  const owner = nextCredentialOwner(apiBase);
  reservedCredentialOwner = owner;
  return owner;
}

/** True only while no newer credential lifecycle has superseded `owner`. */
export function credentialOwnerIsCurrent(owner: CredentialOwner): boolean {
  return owner === credentialGeneration;
}

/** Compare-and-store for SSO completions that began under a reserved owner. */
export function storeRefreshTokenIfOwner(
  data: StoreRefreshTokenInput,
  owner: CredentialOwner
): CredentialOwner | null {
  if (
    owner !== credentialGeneration ||
    reservedCredentialOwner !== owner ||
    inMemoryRefreshToken !== null
  ) {
    return null;
  }
  return publishRefreshToken(data, owner);
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

  const meta = readActiveMeta();
  if (!meta) {
    const paths = pathsForApiBase(DEFAULT_PROFILE_API_BASE);
    const tokenFileExists = fs.existsSync(paths.tokenFile);
    console.debug(
      `[TokenManager] restoreRefreshToken: no meta file (token file exists: ${tokenFileExists})`
    );
    return { status: 'no_session' };
  }

  try {
    const encrypted = fs.readFileSync(pathsForApiBase(meta.apiBase).tokenFile);
    const token = safeStorage.decryptString(encrypted);
    // Key-material audit: previously logged the token's last-8 chars + a
    // sha256 fingerprint plus rememberMe + apiBase — removed to keep
    // refresh-token bytes off stdout.
    const legacyMeta = meta.credentialOwner === undefined || meta.e2eeState === undefined;
    const owner = meta.credentialOwner ?? nextCredentialOwner(meta.apiBase);
    credentialGeneration = owner;
    inMemoryRefreshToken = token;
    inMemoryRememberMe = meta.rememberMe;
    inMemoryApiBase = meta.apiBase;
    inMemoryE2EEKeys = null;
    inMemoryE2EEOwner = null;
    inMemoryE2EEState = meta.e2eeState ?? 'pending';
    stagedE2EEKeys = null;
    reservedCredentialOwner = null;
    allowLegacyE2EEMigration = legacyMeta;
    if (legacyMeta) {
      // Persist the owner + fail-closed marker before attempting the one-time
      // legacy E2EE migration. A crash now prompts for unlock instead of ever
      // pairing this credential with an unowned blob.
      writeMeta(meta.apiBase, meta.rememberMe, owner, 'pending');
    }
    return { status: 'ok', token, apiBase: meta.apiBase, rememberMe: meta.rememberMe };
  } catch (err) {
    // decryptString throws on tampered ciphertext (AES-GCM auth tag failure)
    console.error('[TokenManager] Token decryption failed (tampered?):', (err as Error).message);
    deleteFiles(meta.apiBase);
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

function persistRotatedToken(newRefreshToken: string, apiBase: string): void {
  if (!inMemoryRememberMe || !canPersist()) {
    console.debug(
      `[TokenManager] Rotated token NOT persisted (rememberMe=${inMemoryRememberMe}, canPersist=${canPersist()})`
    );
    return;
  }

  try {
    const encrypted = safeStorage.encryptString(newRefreshToken);
    const paths = pathsForApiBase(apiBase);
    ensureParentDir(paths.tokenFile);
    fs.writeFileSync(paths.tokenFile, encrypted);
    // Key-material audit: previously logged the new refresh token's last-8
    // chars — removed to keep token bytes off stdout.
  } catch (err) {
    console.error('[TokenManager] Failed to re-encrypt rotated token:', (err as Error).message);
  }
}

/**
 * Perform a token refresh via the main process while retaining the credential
 * owner that started it. Response-side state may only commit to that owner.
 */
function performOwnedRefresh(): Promise<OwnedRefreshResult> {
  const owner = snapshotCredentialOwner();
  if (refreshOperation && credentialOwnersMatch(refreshOperation.owner, owner)) {
    return refreshOperation.promise;
  }

  const promise = (async (): Promise<OwnedRefreshResult> => {
    if (!owner) {
      return { result: { status: 'no_token' }, owner };
    }

    try {
      // Key-material audit (#667): no token bytes in any log output, including
      // failure-path console.warn. HTTP status + error classification are
      // sufficient diagnostics; deriving a suffix correlation handle from
      // refresh-token bytes violates [internal]rules/e2ee.md.
      const response = await net.fetch(`${owner.apiBase}/api/v1/auth/refresh`, {
        method: 'POST',
        headers: {
          'X-Refresh-Token': owner.refreshToken,
          'X-Machine-Id': getMachineId(owner.apiBase),
        },
        // Omit cookies — Chromium's persistent cookie store may contain a stale
        // refresh_token cookie from a previous session/login.  The server reads
        // cookies before the X-Refresh-Token header, so a stale cookie would
        // shadow the correct header value and cause a 401.
        credentials: 'omit',
      });

      if (!response.ok) {
        const mfaResult = await tryParseMfaChallenge(response);
        if (mfaResult) return { result: mfaResult, owner };
        console.warn(`[TokenManager] Refresh failed: HTTP ${response.status}`);
        return { result: { status: 'refresh_failed' }, owner };
      }

      const data = (await response.json()) as {
        access_token?: string;
        refresh_token?: string;
        session_id?: string;
        previous_session_id?: string;
      };
      const newAccessToken = data.access_token;
      const newRefreshToken = data.refresh_token;
      const newSessionId = data.session_id;
      const previousSessionId = data.previous_session_id;

      if (!newAccessToken) {
        return { result: { status: 'refresh_failed' }, owner };
      }

      // A login/restore/logout may have replaced this operation's credentials
      // while the fetch or response parse was in flight. Fail closed before
      // touching memory, disk, profile-scoped paths, or the proactive timer.
      if (!isCredentialOwnerCurrent(owner)) {
        return { result: { status: 'refresh_failed' }, owner };
      }

      // Rotate: update in-memory refresh token and re-encrypt to disk.
      // Key-material audit: previously logged suffix of both old and new
      // refresh tokens — removed to keep token bytes off stdout.
      if (newRefreshToken) {
        inMemoryRefreshToken = newRefreshToken;
        persistRotatedToken(newRefreshToken, owner.apiBase);
      }

      // Schedule next proactive refresh based on new token's exp (#254)
      lastProactiveRefreshTimestamp = Date.now();
      scheduleProactiveRefresh(newAccessToken);

      cachedAccessToken = newAccessToken;
      return {
        result: {
          status: 'ok',
          accessToken: newAccessToken,
          sessionId: newSessionId,
          previousSessionId,
        },
        owner: snapshotCredentialOwner(),
      };
    } catch (err) {
      console.error('[TokenManager] Refresh request failed:', (err as Error).message);
      return { result: { status: 'refresh_failed' }, owner };
    }
  })();

  const operation = { owner, promise };
  refreshOperation = operation;
  void promise.then(
    () => {
      if (refreshOperation === operation) refreshOperation = null;
    },
    () => {
      if (refreshOperation === operation) refreshOperation = null;
    }
  );
  return promise;
}

/** Deduplicate refresh requests and expose only the renderer-safe result. */
export async function performRefresh(): Promise<RefreshResult> {
  return (await performOwnedRefresh()).result;
}

/**
 * Perform logout — clear local ownership first, then notify the server best-effort.
 */
export async function performLogout(accessToken?: string): Promise<void> {
  const apiBase = inMemoryApiBase;
  const refreshToken = inMemoryRefreshToken;
  clearTokens();
  if (!apiBase) return;

  try {
    const headers: Record<string, string> = {};
    if (accessToken) {
      headers['Authorization'] = `Bearer ${accessToken}`;
    }
    if (refreshToken) {
      headers['X-Refresh-Token'] = refreshToken;
    }

    await net.fetch(`${apiBase}/api/v1/auth/logout`, {
      method: 'POST',
      headers,
      credentials: 'omit',
    });
  } catch (err) {
    console.error('[TokenManager] Logout request failed:', (err as Error).message);
  }
}

/**
 * Clear all token state — in-memory and on disk.
 */
export function clearTokens(): void {
  const apiBaseToClear = inMemoryApiBase || readActiveApiBase() || DEFAULT_PROFILE_API_BASE;
  credentialGeneration += 1;
  stopProactiveRefresh();
  inMemoryRefreshToken = null;
  inMemoryRememberMe = true;
  inMemoryApiBase = '';
  cachedAccessToken = null;
  // Drop session-only E2EE key custody on logout/clear — the in-memory keys
  // must not outlive the session (CWE-212). performLogout() flows through here.
  inMemoryE2EEKeys = null;
  inMemoryE2EEOwner = null;
  inMemoryE2EEState = 'pending';
  stagedE2EEKeys = null;
  reservedCredentialOwner = null;
  allowLegacyE2EEMigration = false;
  deleteFiles(apiBaseToClear);
}

/**
 * Clear credentials only when the caller still owns the stored lifecycle.
 * Refresh-token rotation deliberately preserves this owner.
 */
export function clearTokensIfOwner(owner: CredentialOwner): boolean {
  if (owner !== credentialGeneration) return false;
  clearTokens();
  return true;
}

/**
 * Release an ORPHANED SSO credential reservation so the pre-credential staging
 * lane (`storeE2EEKeys`, below) reopens for password registration (#2394).
 *
 * A reservation exists to say "an SSO flow with a live continuation in main
 * holds the exclusive right to mint the next credential." When the user
 * abandons that flow, nothing retired the reservation, so a later password
 * registration silently lost restart-survival of its E2EE keys.
 *
 * Deliberately NOT `clearTokensIfOwner`: that primitive CAS-checks the
 * generation ONLY, and `publishRefreshToken` preserves the generation across a
 * rotation, so a renderer-timed release routed through it could pass the CAS
 * and wipe a just-published live credential. This guard additionally requires
 * the slot to be RESERVED and UNFILLED — the same triple
 * `storeRefreshTokenIfOwner` checks — which makes that case structurally
 * unreachable.
 *
 * Which clause actually closes it, precisely: `publishRefreshToken` nulls
 * `reservedCredentialOwner` in the same synchronous block that sets the token,
 * so after a publish the FIRST clause already short-circuits. The
 * `inMemoryRefreshToken` clause is therefore defense-in-depth against a future
 * writer that sets a token without clearing the reservation — a state no
 * current code path can reach, and consequently one no test can construct
 * through the public API. Do not read the published-credential test as a lock
 * on that clause; it exercises clause one. Do not "simplify" this back to
 * `clearTokensIfOwner` — the reasoning above, not a test, is what stops you.
 *
 * Delegates the wipe to `clearTokens()`: one wipe implementation, and the
 * generation bump is DESIRED — every in-flight SSO continuation then fails its
 * next `credentialOwnerIsCurrent` check and revokes its own server session.
 */
export function releaseCredentialReservation(): boolean {
  if (
    reservedCredentialOwner === null ||
    reservedCredentialOwner !== credentialGeneration ||
    inMemoryRefreshToken !== null
  ) {
    return false;
  }
  clearTokens();
  return true;
}

// ─── E2EE Key Persistence (safeStorage) ──────────────────────────────

/**
 * Store E2EE session keys encrypted via safeStorage.
 * Called after login/registration when E2EE service has been initialized.
 *
 * Returns `true` when disk persistence is in its expected state — either the
 * write succeeded, or it was intentionally skipped (session-only / no
 * safeStorage). Returns `false` ONLY when a disk write was attempted and
 * genuinely failed (keychain locked, disk full). The renderer uses this to
 * decide whether restart-survival was actually set up; a `false` is the signal
 * that used to be swallowed (#1288). In-memory key custody is preserved in all
 * cases — a persistence failure never drops the usable in-session keys (#1278).
 */
export function storeE2EEKeys(data: E2EEKeyMaterial): boolean {
  // The generic writer exists only for pre-credential registration staging.
  // Once a credential or SSO reservation exists, accepting an unowned write
  // could let a stale renderer continuation overwrite its successor's keys.
  if (inMemoryRefreshToken !== null || reservedCredentialOwner !== null) {
    // #2394: make "the staging lane is held" distinguishable from a keychain
    // failure in the main-process log. The renderer sees only `false` and
    // cannot tell these apart, and before #2394 the surviving cause of a held
    // lane was an orphaned SSO reservation — a bug, not an expected state.
    // Never log the owner value or any key material (observability.md #1).
    console.warn(
      '[TokenManager] storeE2EEKeys refused — a credential or SSO reservation already holds the staging lane'
    );
    return false;
  }
  stagedE2EEKeys = { generation: credentialGeneration, keys: data };
  return true;
}

/** Store E2EE keys only if the caller still owns the credential lifecycle. */
export function storeE2EEKeysIfOwner(data: E2EEKeyMaterial, owner: CredentialOwner): boolean {
  if (owner !== credentialGeneration || inMemoryRefreshToken === null || inMemoryApiBase === '') {
    return false;
  }

  // Publish the memory copy first: session-only users and keychain-write
  // failures still retain a usable soft-reload session. The durable marker is
  // flipped to ready only after the owner-tagged E2EE blob is on disk.
  inMemoryE2EEKeys = data;
  inMemoryE2EEOwner = owner;
  inMemoryE2EEState = 'ready';
  allowLegacyE2EEMigration = false;

  if (!canPersist() || !inMemoryRememberMe) return true;

  const meta = readMeta(activePaths().metaFile);
  if (meta?.credentialOwner !== owner) return false;

  try {
    const persisted: PersistedE2EEKeys = { credentialOwner: owner, keys: data };
    const encrypted = safeStorage.encryptString(JSON.stringify(persisted));
    const paths = activePaths();
    ensureParentDir(paths.e2eeFile);
    fs.writeFileSync(paths.e2eeFile, encrypted);
    return writeMeta(inMemoryApiBase, inMemoryRememberMe, owner, 'ready');
  } catch (err) {
    console.error('[TokenManager] Failed to encrypt/write E2EE keys:', (err as Error).message);
    return false;
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
  if (inMemoryE2EEKeys && inMemoryE2EEOwner === credentialGeneration) {
    return inMemoryE2EEKeys;
  }

  if (
    !canPersist() ||
    !inMemoryRefreshToken ||
    !inMemoryApiBase ||
    (inMemoryE2EEState !== 'ready' && !allowLegacyE2EEMigration)
  ) {
    return null;
  }

  try {
    const encrypted = fs.readFileSync(activePaths().e2eeFile);
    const json = safeStorage.decryptString(encrypted);
    const parsed = JSON.parse(json) as unknown;
    if (
      typeof parsed === 'object' &&
      parsed !== null &&
      !Array.isArray(parsed) &&
      (parsed as Record<string, unknown>).credentialOwner === credentialGeneration &&
      isE2EEKeyMaterial((parsed as Record<string, unknown>).keys)
    ) {
      const keys = (parsed as PersistedE2EEKeys).keys;
      inMemoryE2EEKeys = keys;
      inMemoryE2EEOwner = credentialGeneration;
      inMemoryE2EEState = 'ready';
      return keys;
    }

    if (allowLegacyE2EEMigration && isE2EEKeyMaterial(parsed)) {
      const keys = parsed;
      void storeE2EEKeysIfOwner(keys, credentialGeneration);
      return keys;
    }
    return null;
  } catch {
    return null;
  }
}

/** Renderer-safe owner + fail-closed E2EE restore state for the active credential. */
export function getCredentialCustodyState(): {
  credentialOwner: CredentialOwner | null;
  pendingE2EEUnlock: boolean;
} {
  const hasCredential = inMemoryRefreshToken !== null && inMemoryApiBase !== '';
  if (!hasCredential) return { credentialOwner: null, pendingE2EEUnlock: false };
  const hasOwnedKeys =
    inMemoryE2EEKeys !== null &&
    inMemoryE2EEOwner === credentialGeneration &&
    inMemoryE2EEState === 'ready';
  return { credentialOwner: credentialGeneration, pendingE2EEUnlock: !hasOwnedKeys };
}

/**
 * Read the persisted API base URL from the token metadata file.
 * Returns the URL if available, null otherwise.
 * Used by the SPA loader to fetch client config before the renderer loads.
 */
export function getPersistedApiBase(): string | null {
  if (inMemoryApiBase) return inMemoryApiBase;
  const meta = readActiveMeta();
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
  credentialGeneration += 1;
  inMemoryRefreshToken = null;
  inMemoryRememberMe = true;
  inMemoryApiBase = '';
  cachedAccessToken = null;
  inMemoryE2EEKeys = null;
  inMemoryE2EEOwner = null;
  inMemoryE2EEState = 'pending';
  stagedE2EEKeys = null;
  reservedCredentialOwner = null;
  allowLegacyE2EEMigration = false;
  refreshOperation = null;
  proactiveRefreshCallback = null;
  lastProactiveRefreshTimestamp = 0;
}
