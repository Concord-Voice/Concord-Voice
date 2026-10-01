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
 * - Tamper detection: safeStorage.decryptStringAsync() rejects on corrupted
 *   ciphertext; Linux files not under a keyring key (the hardcoded key is
 *   unauthenticated and forgeable) are refused before decryption the same way
 * - Disk persistence: only through secureStorage.ts, which accepts only Linux's
 *   keyring-backed keys. In-memory state is published synchronously; the
 *   async disk writes after it are queued and re-check ownership after each await.
 */

import { safeStorage, net } from 'electron';
import path from 'node:path';
import fs from 'node:fs';
import { getMachineId } from './machineId';
import { encryptForDisk, isSecureStorageAvailable, isUnprotectedCiphertext } from './secureStorage';
import type { CredentialOwner, RefreshResult } from './ipcContract';
import {
  profileIdForApiBase,
  profilePathsForApiBase,
  type ProfilePaths,
} from './selfHostedProfile';

// ─── Module State (never leaves this process) ────────────────────────

// Main-process-local shape of the persisted E2EE key material. Structural mirror
// of the renderer's `E2EESessionKeys` (renderer/services/e2ee/e2eeService.ts); the two
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
// Owner numbers are not unique. Restore adopts a profile's persisted owner,
// which can move this counter backwards, and two profiles can persist the same
// number. So every queued write re-checks the API base and the stored value
// (token, key set or meta owner) as well as the owner: never reduce a fence to
// an owner-only check.
let credentialGeneration = 0;
let refreshOperation: RefreshOperation | null = null;
// Tail of the safeStorage disk-write queue (see enqueueDiskWrite).
let diskWrites: Promise<unknown> = Promise.resolve();

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

/**
 * Run safeStorage disk writes one at a time, in call order, so a credential's
 * meta file is on disk before its E2EE blob. Ordering is all the queue gives:
 * every write still re-checks ownership after its own awaits.
 */
function enqueueDiskWrite<T>(write: () => Promise<T>): Promise<T> {
  const result = diskWrites.then(write);
  diskWrites = result.catch(() => undefined);
  return result;
}

/** True while `token` is still the live credential of `owner` for `apiBase`. */
function refreshTokenIsCurrent(token: string, apiBase: string, owner: CredentialOwner): boolean {
  return (
    owner === credentialGeneration && token === inMemoryRefreshToken && apiBase === inMemoryApiBase
  );
}

function writeTokenFile(apiBase: string, encrypted: Buffer): void {
  const paths = pathsForApiBase(apiBase);
  ensureParentDir(paths.tokenFile);
  writeFileAtomic(paths.tokenFile, encrypted);
}

function pathsForApiBase(apiBase: string): ProfilePaths {
  return profilePathsForApiBase(apiBase || DEFAULT_PROFILE_API_BASE);
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

// A fixed fallback, never String(err): a non-Error rejection could carry anything.
function errorMessage(err: unknown): string {
  return err instanceof Error ? err.message : 'non-Error rejection';
}

/** Write through a 0o600 temp file and a same-directory rename: a crash leaves no torn file. */
function writeFileAtomic(dest: string, data: string | Buffer): void {
  const tmp = `${dest}.${process.pid}.tmp`;
  try {
    fs.writeFileSync(tmp, data, { mode: 0o600 });
    fs.renameSync(tmp, dest);
  } catch (err) {
    fs.rmSync(tmp, { force: true });
    throw err;
  }
}

function writeActiveProfile(apiBase: string): void {
  try {
    ensureParentDir(activeProfileFile());
    writeFileAtomic(activeProfileFile(), JSON.stringify({ apiBase }));
  } catch (err) {
    console.error('[TokenManager] Failed to write active profile:', errorMessage(err));
  }
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
    writeFileAtomic(
      paths.metaFile,
      JSON.stringify({
        apiBase,
        rememberMe,
        profileId: profileIdForApiBase(apiBase),
        credentialOwner,
        e2eeState,
      })
    );
    writeFileAtomic(activeProfileFile(), JSON.stringify({ apiBase }));
    return true;
  } catch (err) {
    console.error('[TokenManager] Failed to write meta file:', errorMessage(err));
    return false;
  }
}

function readActiveApiBase(): string | null {
  try {
    const raw = fs.readFileSync(activeProfileFile(), 'utf-8');
    const parsed = JSON.parse(raw) as { apiBase?: unknown };
    return typeof parsed.apiBase === 'string' && URL.canParse(parsed.apiBase)
      ? parsed.apiBase
      : null;
  } catch {
    return null;
  }
}

function readMeta(metaFile = pathsForApiBase(DEFAULT_PROFILE_API_BASE).metaFile): TokenMeta | null {
  try {
    const raw = fs.readFileSync(metaFile, 'utf-8');
    const parsed = JSON.parse(raw) as Record<string, unknown>;
    if (
      typeof parsed.apiBase !== 'string' ||
      !URL.canParse(parsed.apiBase) ||
      typeof parsed.rememberMe !== 'boolean'
    ) {
      return null;
    }
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
  // A pointer without its meta is a login interrupted before its write landed:
  // no session, never a fallback to another profile's older credential. So is a
  // pointer that exists but names no valid API base.
  if (activeApiBase) return readMeta(pathsForApiBase(activeApiBase).metaFile);
  if (fs.existsSync(activeProfileFile())) return null;
  return readMeta();
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

function sameE2EEKeys(a: E2EEKeyMaterial | null, b: E2EEKeyMaterial): boolean {
  return (
    a !== null &&
    a.wrappingKeyBase64 === b.wrappingKeyBase64 &&
    a.preferencesKeyBase64 === b.preferencesKeyBase64 &&
    a.wrappedPrivateKeyBase64 === b.wrappedPrivateKeyBase64
  );
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

/**
 * Write a newly published Remember-Me credential. Without OS-protected storage
 * it stays memory-only (session-only), the same as rememberMe=false.
 */
function persistPublishedToken(
  token: string,
  apiBase: string,
  owner: CredentialOwner
): Promise<void> {
  return enqueueDiskWrite(async () => {
    // A logout or a newer login owns the profile files now. Checked before the
    // encrypt too, so a burst of logins encrypts only the newest.
    const superseded = () => owner !== credentialGeneration || apiBase !== inMemoryApiBase;
    if (superseded()) return;
    let encrypted: Buffer | null;
    try {
      encrypted = await encryptForDisk(token);
    } catch (err) {
      console.error('[TokenManager] Failed to encrypt token:', errorMessage(err));
      return;
    }
    if (superseded()) return;
    if (encrypted === null) {
      console.warn(
        '[TokenManager] Secure storage unavailable, token will not persist across restarts'
      );
      return;
    }
    if (!writeMeta(apiBase, true, owner, 'pending')) {
      // writeMeta may have failed after a partial write; leave nothing behind.
      deleteFiles(apiBase);
      return;
    }
    // A rotation during the encrypt queued its own write of the newer token.
    if (token !== inMemoryRefreshToken) return;
    try {
      writeTokenFile(apiBase, encrypted);
    } catch (err) {
      console.error('[TokenManager] Failed to write token:', errorMessage(err));
      deleteFiles(apiBase);
    }
  });
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

  // Every predecessor artifact goes before anything is written. The disk write
  // below awaits an encrypt, so a crash or a logout in that window must find no
  // session on disk, never a predecessor token or E2EE blob under this owner.
  // Key-material audit: previously logged the token's last-8 chars + a sha256
  // fingerprint — removed to keep refresh-token bytes off stdout.
  deleteFiles(data.apiBase);
  if (data.rememberMe) {
    // Point a restart at this profile before the await, so a crash there finds
    // no session. If this write fails, a restart falls back to the default
    // profile, as it does after a login here without Remember Me.
    writeActiveProfile(data.apiBase);
    void persistPublishedToken(data.refreshToken, data.apiBase, owner);
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

type RestoreRefreshTokenResult =
  | { status: 'ok'; token: string; apiBase: string; rememberMe: boolean }
  | { status: 'no_session' | 'tampered' | 'unavailable' };

// Electron's only signal for an os_crypt_async "temporarily unavailable" key
// (e.g. a keyring not yet unlocked) is this rejection message. Such a token is
// intact, so it must not be deleted as tampered.
const DECRYPT_TEMPORARILY_UNAVAILABLE = /temporarily unavailable/i;

function memoryRestoreResult(): RestoreRefreshTokenResult {
  if (!inMemoryRefreshToken || !inMemoryApiBase) return { status: 'no_session' };
  return {
    status: 'ok',
    token: inMemoryRefreshToken,
    apiBase: inMemoryApiBase,
    rememberMe: inMemoryRememberMe,
  };
}

/**
 * Restore the refresh token on app startup — main-process memory first, then
 * disk. Returns the token or an error status.
 */
export async function restoreRefreshToken(): Promise<RestoreRefreshTokenResult> {
  const fromMemory = memoryRestoreResult();
  if (fromMemory.status === 'ok') return fromMemory;

  // Any login, logout or registration key staging during an await below owns
  // custody; never overwrite its state or delete its files with this stale read.
  const generation = credentialGeneration;
  const staged = stagedE2EEKeys;
  const superseded = () => credentialGeneration !== generation || stagedE2EEKeys !== staged;
  const available = await isSecureStorageAvailable();
  if (superseded()) return memoryRestoreResult();
  if (!available) {
    console.debug('[TokenManager] restoreRefreshToken: secure storage unavailable');
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

  let token: string;
  let shouldReEncrypt: boolean;
  try {
    const encrypted = fs.readFileSync(pathsForApiBase(meta.apiBase).tokenFile);
    if (isUnprotectedCiphertext(encrypted)) {
      throw new Error('Token file is not under a keyring key');
    }
    ({ result: token, shouldReEncrypt } = await safeStorage.decryptStringAsync(encrypted));
  } catch (err) {
    if (superseded()) return memoryRestoreResult();
    if (DECRYPT_TEMPORARILY_UNAVAILABLE.test(errorMessage(err))) {
      console.warn('[TokenManager] Token decryption temporarily unavailable; not deleting it');
      return { status: 'unavailable' };
    }
    // decryptStringAsync rejects on tampered ciphertext (AES-GCM auth tag failure)
    console.error('[TokenManager] Token decryption failed (tampered?):', errorMessage(err));
    deleteFiles(meta.apiBase);
    return { status: 'tampered' };
  }
  if (superseded()) return memoryRestoreResult();

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
  // The key that decrypted this file is no longer the one that encrypts.
  if (shouldReEncrypt) persistRotatedToken(token, meta.apiBase);
  return { status: 'ok', token, apiBase: meta.apiBase, rememberMe: meta.rememberMe };
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

/**
 * Best-effort overwrite of the token file with a rotated or re-keyed token. On
 * failure the previous file stays, which the server's refresh grace can still
 * recover from.
 */
function persistRotatedToken(newRefreshToken: string, apiBase: string): void {
  if (!inMemoryRememberMe) {
    console.debug('[TokenManager] Rotated token NOT persisted (rememberMe=false)');
    return;
  }
  const owner = credentialGeneration;
  void enqueueDiskWrite(async () => {
    try {
      const encrypted = await encryptForDisk(newRefreshToken);
      // Superseded tokens and memory-only sessions never reach the disk.
      if (encrypted === null || !refreshTokenIsCurrent(newRefreshToken, apiBase, owner)) return;
      writeTokenFile(apiBase, encrypted);
      // Key-material audit: previously logged the new refresh token's last-8
      // chars — removed to keep token bytes off stdout.
    } catch (err) {
      console.error('[TokenManager] Failed to re-encrypt rotated token:', errorMessage(err));
    }
  });
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
      console.error('[TokenManager] Refresh request failed:', errorMessage(err));
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
    console.error('[TokenManager] Logout request failed:', errorMessage(err));
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
 * Stage E2EE session keys for the credential a registration is about to mint;
 * publishRefreshToken adopts them through storeE2EEKeysIfOwner, which persists.
 *
 * storeE2EEKeysIfOwner returns `true` when disk persistence is in its expected
 * state — either the write succeeded, or it was intentionally skipped
 * (session-only / no OS-protected storage). It returns `false` when a disk
 * write was attempted and genuinely failed (keychain locked, disk full), or the
 * write was superseded. The renderer uses this to
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
export async function storeE2EEKeysIfOwner(
  data: E2EEKeyMaterial,
  owner: CredentialOwner
): Promise<boolean> {
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

  if (!inMemoryRememberMe) return true;

  const apiBase = inMemoryApiBase;
  return enqueueDiskWrite(async () => {
    // A logout, a newer credential, or different keys replaced this write.
    // Checked before the encrypt too, so a burst of key writes encrypts only
    // the newest. An identical repeat is not superseded and still writes.
    const superseded = () =>
      owner !== credentialGeneration ||
      apiBase !== inMemoryApiBase ||
      !sameE2EEKeys(inMemoryE2EEKeys, data);
    if (superseded()) return false;
    let encrypted: Buffer | null;
    try {
      const persisted: PersistedE2EEKeys = { credentialOwner: owner, keys: data };
      encrypted = await encryptForDisk(JSON.stringify(persisted));
    } catch (err) {
      console.error('[TokenManager] Failed to encrypt E2EE keys:', errorMessage(err));
      return false;
    }
    if (superseded()) return false;
    // No OS-protected storage: memory-only by design, like rememberMe=false.
    if (encrypted === null) return true;
    if (readMeta(pathsForApiBase(apiBase).metaFile)?.credentialOwner !== owner) return false;
    try {
      const paths = pathsForApiBase(apiBase);
      ensureParentDir(paths.e2eeFile);
      writeFileAtomic(paths.e2eeFile, encrypted);
      return writeMeta(apiBase, true, owner, 'ready');
    } catch (err) {
      console.error('[TokenManager] Failed to write E2EE keys:', errorMessage(err));
      return false;
    }
  });
}

/** The decrypted E2EE file; null when absent, unreadable or not under a keyring key. */
async function decryptE2EEFile(
  apiBase: string
): Promise<{ result: string; shouldReEncrypt: boolean } | null> {
  try {
    const encrypted = fs.readFileSync(pathsForApiBase(apiBase).e2eeFile);
    // Forgeable; the next unlock overwrites it with a keyring-encrypted blob.
    if (isUnprotectedCiphertext(encrypted)) return null;
    // `return await`, so a rejected decrypt lands in the catch.
    return await safeStorage.decryptStringAsync(encrypted);
  } catch {
    return null;
  }
}

/**
 * Restore E2EE session keys from safeStorage.
 * Returns the key material or null if unavailable.
 */
export async function restoreE2EEKeys(): Promise<E2EEKeyMaterial | null> {
  // Prefer the in-memory copy (set by storeE2EEKeysIfOwner) so a session-only
  // soft reload restores keys that were never written to disk. Mirrors the
  // memory-first branch in restoreRefreshToken().
  if (inMemoryE2EEKeys && inMemoryE2EEOwner === credentialGeneration) {
    return inMemoryE2EEKeys;
  }

  if (
    !inMemoryRefreshToken ||
    !inMemoryApiBase ||
    (inMemoryE2EEState !== 'ready' && !allowLegacyE2EEMigration)
  ) {
    return null;
  }

  const owner = credentialGeneration;
  const apiBase = inMemoryApiBase;
  const decrypted = await decryptE2EEFile(apiBase);
  // No readable file, or a logout or newer credential during the decrypt.
  if (
    !decrypted ||
    owner !== credentialGeneration ||
    apiBase !== inMemoryApiBase ||
    !inMemoryRefreshToken
  ) {
    return null;
  }
  // A key write for this owner landed during the decrypt and wins over disk.
  if (inMemoryE2EEKeys && inMemoryE2EEOwner === owner) return inMemoryE2EEKeys;
  const { result: json, shouldReEncrypt } = decrypted;

  let parsed: unknown;
  try {
    parsed = JSON.parse(json);
  } catch {
    return null;
  }
  if (
    typeof parsed === 'object' &&
    parsed !== null &&
    !Array.isArray(parsed) &&
    (parsed as Record<string, unknown>).credentialOwner === owner &&
    isE2EEKeyMaterial((parsed as Record<string, unknown>).keys)
  ) {
    const keys = (parsed as PersistedE2EEKeys).keys;
    inMemoryE2EEKeys = keys;
    inMemoryE2EEOwner = owner;
    inMemoryE2EEState = 'ready';
    // The key that decrypted this blob is no longer the one that encrypts.
    if (shouldReEncrypt) void storeE2EEKeysIfOwner(keys, owner);
    return keys;
  }

  if (allowLegacyE2EEMigration && isE2EEKeyMaterial(parsed)) {
    const keys = parsed;
    void storeE2EEKeysIfOwner(keys, owner);
    return keys;
  }
  return null;
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

export async function getCapabilities(): Promise<{ persistAvailable: boolean }> {
  return { persistAvailable: await isSecureStorageAvailable() };
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
  diskWrites = Promise.resolve();
}

/** Resolve once every safeStorage disk write queued so far has settled. */
export function _flushDiskWritesForTesting(): Promise<unknown> {
  return diskWrites;
}
