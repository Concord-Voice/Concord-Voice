/**
 * E2EE Service — JIT key management with zero-persistence of raw secrets.
 *
 * Design: Keys are derived/unwrapped at the moment of use and cleared after.
 * WebCrypto CryptoKey objects are opaque (browser keeps material in protected memory).
 *
 * Cached state:
 * - wrappingKey: CryptoKey (derived, opaque, session lifetime)
 * - wrappedPrivateKey: string (encrypted blob, safe in memory)
 * - channelKeyCache: Map of wrapped keys (encrypted, safe in memory)
 *
 * NOT cached: unwrapped private key, unwrapped CSK — JIT only
 */

import {
  type KeyDerivationAlgorithm,
  deriveKeyArgon2idExportable,
  derivePreferencesKeyArgon2idExportable,
  encryptBlob,
  decryptBlob,
  unwrapPrivateKey,
  unwrapSigningKey,
  unwrapChannelKey,
  wrapChannelKey,
  encryptMessage,
  decryptMessage,
  generateChannelKey,
  exportChannelKey,
  importPublicKey,
  arrayBufferToBase64,
  base64ToArrayBuffer,
} from '../utils/crypto';
import { apiFetch, safeJson } from './apiClient';
import {
  E2EEInitTeardownError,
  E2EEKeyUnavailableError,
  type E2EEKeyErrorCode,
} from './e2eeErrors';
import { useE2EEStore } from '../stores/e2eeStore';

// Session lifetime — keys cached until explicit invalidation (rotation) or logout/close.
// Lazy fetch + hold: each channel's key is fetched on first visit and held for the session.
const CHANNEL_KEY_CACHE_TTL = Number.MAX_SAFE_INTEGER;
const PENDING_KEY_RETRY_DELAY_MS = 60_000;

interface CachedWrappedKey {
  wrappedKey: string;
  keyVersion: number;
  lastUsed: number;
  /**
   * Cache-poison refetch counter. Bounded at 1 — if a malformed wrap lands in
   * cache and the refetch also fails shape validation, throw MALFORMED_PAYLOAD
   * as terminal. Prevents infinite loops when the server persistently serves
   * corrupt data.
   *
   * Reset to 0 on successful unwrap (cache-write in happy path) or on explicit
   * invalidateChannelKey. See spec §6.4.
   */
  refetchAfterMalformed: number;
}

export interface ChannelKeyMaterial {
  channelKey: CryptoKey;
  keyVersion: number;
}

interface ErrorResponseShape {
  error?: string;
  code?: E2EEKeyErrorCode;
  kind?: 'channel' | 'dm' | 'unknown';
  pending?: boolean;
}

interface KeyResponseShape {
  key: { wrapped_key: string; key_version?: number };
  kind?: 'channel' | 'dm' | 'unknown';
}

interface PendingKeyRequest {
  user_id: string;
  channel_id: string;
  key_version?: number;
}

interface PendingRecipientPublicKey {
  public_key: string;
  key_version?: number;
}

interface WrappedPendingKey {
  wrappedKey: string;
  keyFingerprint: string;
}

type PendingKeyStepResult<T> = { action: 'process'; data: T } | { action: 'continue' | 'stop' };

function pendingKeyResponseIsRetryable(res: Response): boolean {
  return res.status === 429 || res.status >= 500;
}

/** Exported key material for safeStorage persistence */
export interface E2EESessionKeys {
  wrappingKeyBase64: string;
  preferencesKeyBase64: string;
  wrappedPrivateKeyBase64: string;
}

/**
 * Opaque proof of the exact initialize() commit currently held by the service.
 * The attempt number distinguishes that commit from a newer initialization
 * that has started but has not published its keys yet.
 */
export interface E2EEInitializationReceipt {
  readonly sessionKeys: E2EESessionKeys;
  readonly attempt: number;
}

/** Prevent an async key derivation owned by an old account from committing globally. */
export interface E2EEInitializationGuard {
  signal: AbortSignal;
  isCurrent: () => boolean;
}

/** Binds pre-fetched key work to one account session and channel-key generation. */
export interface E2EEChannelOperationGuard {
  assertCurrent: () => void;
}

function initializationIsCurrent(guard?: E2EEInitializationGuard): boolean {
  return guard === undefined || (!guard.signal.aborted && guard.isCurrent());
}

class E2EEService {
  /**
   * Expected byte length of a wrapped channel key after base64 decode.
   *
   * RSA-OAEP with a 4096-bit modulus produces ciphertext of exactly
   * `modulusLength / 8 = 512` bytes, irrespective of plaintext length. The
   * assertion is STRICT (not a range) so any future change to the wrap format
   * fails loudly rather than silently. See spec §6.4.
   */
  private static readonly EXPECTED_WRAP_BYTES = 512;

  private wrappingKey: CryptoKey | null = null;
  private preferencesKey: CryptoKey | null = null;
  private wrappedPrivateKey: string = '';
  private readonly channelKeyCache: Map<string, CachedWrappedKey> = new Map();
  private readonly versionedKeyCache: Map<string, Map<number, CachedWrappedKey>> = new Map();
  private readonly pendingKeyFetches: Map<string, Promise<ChannelKeyMaterial>> = new Map();
  private readonly pendingVersionedKeyFetches: Map<string, Promise<CryptoKey>> = new Map();
  private readonly channelKeyGenerations: Map<string, number> = new Map();
  private readonly channelAccessRevocationGenerations: Map<string, number> = new Map();
  private pendingKeyRetryTimer: ReturnType<typeof setTimeout> | null = null;
  private pendingKeyRequestProcessor: Promise<void> | null = null;
  private pendingKeyRequestProcessorGeneration: number | null = null;
  private pendingKeyRequestRerun = false;
  private keySessionGeneration: number = 0;
  // Bumped ONLY by clearKeys() (actual key destruction: logout, nuclearReset/
  // gracefulReset teardown, account switch). The init-commit fence keys on THIS
  // counter, not keySessionGeneration: recoveryReset()/fencePendingOperations()
  // bump keySessionGeneration to reject stale decrypt CONTINUATIONS while
  // deliberately preserving key custody — a same-session Recovery-A landing
  // mid-derivation must NOT abort a guarded re-init commit (the password-change
  // flow would otherwise push preferences with the OLD keyset after the server
  // committed the new password — undecryptable after relogin; Codex P1, PR #2337).
  private keyClearGeneration: number = 0;
  // Monotonic initialize()/initializeFromStoredKeys() attempt counter
  // (Codex P1, PR #2337). Concurrent auth flows can initialize under the same
  // keyClearGeneration: a rapid successor may start while an earlier Argon2id
  // derivation is still pending, and last-writer-wins would let the STALE
  // keyset overwrite the successor's committed keys (cross-session key
  // confusion). The commit gate admits only the NEWEST attempt; a superseded attempt throws
  // E2EEInitTeardownError (its session is gone from the flow's perspective —
  // same abort contract as destruction). Token-lifecycle ownership itself
  // stays at the auth surfaces (their pre-admit gates), keeping this service
  // auth-store-agnostic.
  private initAttemptSequence = 0;
  private rateLimitedUntil: number = 0; // timestamp when rate limit expires

  // #1878: authoritative CSK-rotation signal. Fires when a strictly-higher
  // channel key version is observed/cached — the sender re-base trigger. The
  // first-ever observation for a channel does NOT fire (no prior baseline to
  // rotate from); only an increase from a previously-seen version does.
  private readonly keyRotationListeners = new Set<
    (e: { channelId: string; keyVersion: number }) => void
  >();
  private readonly highestSeenVersion = new Map<string, number>();

  /** Cached exported keys for safeStorage persistence (set during initialize) */
  private sessionKeys: E2EESessionKeys | null = null;
  private sessionKeysInitAttempt: number | null = null;

  /**
   * Initialize the E2EE service after login/registration.
   * Derives keys using Argon2id, exports raw bytes for safeStorage,
   * then re-imports as non-extractable for runtime security.
   *
   * The `keyDerivationAlg` parameter is retained for API/forward-compat but
   * currently only Argon2id is supported. PBKDF2 was removed after a server-side
   * DB purge that eliminated all legacy PBKDF2-wrapped keys; the previous
   * silent-downgrade fallback could mask network failures.
   */
  async initialize(
    password: string,
    wrappedPrivateKeyBase64: string,
    saltBase64: string,
    _keyDerivationAlg: KeyDerivationAlgorithm = 'argon2id',
    guard?: E2EEInitializationGuard,
    sinceEpoch?: number
  ): Promise<E2EEInitializationReceipt | null> {
    if (!initializationIsCurrent(guard)) return null;
    // Snapshot the key-CLEAR generation BEFORE the (slow) derivation begins —
    // or adopt the caller-captured epoch (captureTeardownEpoch) when provided.
    // The fresh-login callers (Login/Register/SSOEagerUnlock/SSOPassphraseSetup)
    // pass no `guard`, so this internal fence is the ONLY thing that aborts the
    // commit when a 401 -> nuclearReset() -> gracefulReset() -> clearKeys()
    // (#2327) tears the session down mid-Argon2id: clearKeys() bumps
    // keyClearGeneration, and the commit gate then throws E2EEInitTeardownError
    // for a keyset derived under a stale generation (a silent void-success let
    // Login continue past the teardown — Codex P1-A). The caller-captured
    // epoch closes the sibling window where the teardown lands BEFORE
    // initialize() is even called (Codex P1-B): an entry-time snapshot would
    // postdate the clearKeys() bump and never trip. (CWE-212, #2199.)
    // Deliberately NOT keySessionGeneration: recoveryReset() bumps that one
    // while PRESERVING the session — see the keyClearGeneration field comment.
    const startGeneration = sinceEpoch ?? this.keyClearGeneration;
    // Pre-sequence staleness gate (Codex P1, PR #2337): a continuation whose
    // caller epoch is ALREADY stale (a teardown fired before it resumed) must
    // abort WITHOUT advancing initAttemptSequence. If it bumped the sequence
    // first, it would invalidate a valid newer sign-in that started earlier and
    // already captured a lower attempt number — cancelling a good login (whose
    // abort path would then revoke its own freshly-issued session). Validate
    // the epoch BEFORE claiming the newest-attempt slot.
    if (this.keyClearGeneration !== startGeneration) throw new E2EEInitTeardownError();
    // This call is now the newest initialization attempt — any still-pending
    // earlier attempt is superseded and must not commit (see the
    // initAttemptSequence field comment).
    const attempt = ++this.initAttemptSequence;
    // Entry gate: re-validate (guard + supersession) before any derivation work.
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;
    const saltBytes = new Uint8Array(base64ToArrayBuffer(saltBase64));
    // Return the invocation-owned receipt (or null on a guard-declined commit) so
    // auth flows use THIS call's committed keys, never ambient singleton state (#2423).
    return this.initializeWithArgon2id(
      password,
      wrappedPrivateKeyBase64,
      saltBytes,
      startGeneration,
      attempt,
      guard
    );
    // No periodic cleanup needed — session-scoped cache (keys stay until rotation or logout)
  }

  /**
   * The current teardown epoch. Callers that begin auth-flow work BEFORE
   * invoking initialize() (e.g. Login publishes the access token and unwraps
   * keys first) capture this up front and pass it as initialize()'s
   * `sinceEpoch`, so a teardown landing anywhere in the flow — not just during
   * derivation — aborts the commit (Codex P1-B, PR #2337). Monotonic counter,
   * bumped only by clearKeys(); carries no key material.
   */
  captureTeardownEpoch(): number {
    return this.keyClearGeneration;
  }

  /**
   * True if clearKeys() (logout / 401 teardown / account switch) ran since the
   * captured epoch. Auth flows call this immediately before EVERY admit action
   * (onSuccess / onUnlock / SSO phase-idle, and before persisting a refresh
   * token) — initialize()'s own fence covers only the span up to the key
   * commit, and a teardown landing during the later token-store / persist /
   * hydrate awaits must not be steamrolled into an admit (Codex P1,
   * PR #2337).
   */
  wasTornDownSince(epoch: number): boolean {
    return this.keyClearGeneration !== epoch;
  }

  /**
   * Commit gate for the async initialize() path, with two distinct outcomes:
   * - Caller-owned guard abort (account switch / explicit cancel) → returns
   *   false; callers bail SILENTLY (the guard's owner initiated it and handles
   *   it via its own signal — existing contract, unchanged).
   * - Key DESTRUCTION since the snapshot/epoch (clearKeys bumped
   *   keyClearGeneration: logout, 401 teardown, account switch) → THROWS
   *   E2EEInitTeardownError so `await initialize()` cannot resolve as success
   *   and the login/restore flow cannot continue past the teardown
   *   (Codex P1-A, PR #2337; CWE-212, #2199).
   * - Supersession (a NEWER initialize()/initializeFromStoredKeys() attempt
   *   started since this one) → ALSO THROWS E2EEInitTeardownError: a stale
   *   pending attempt must never commit over — or after — the newest one
   *   (last-writer-wins would let a superseded auth flow's keyset overwrite
   *   a successor's; Codex P1, PR #2337).
   * The fresh-login callers pass no guard, so the generation arm is what
   * fences a 401 -> nuclearReset landing mid-Argon2id. A same-session
   * recoveryReset() (continuation fence only, keys preserved) intentionally
   * trips NEITHER arm (Codex P1, PR #2337).
   */
  private assertInitCommitCurrent(
    startGeneration: number,
    attempt: number,
    guard?: E2EEInitializationGuard
  ): boolean {
    if (!initializationIsCurrent(guard)) return false;
    if (this.keyClearGeneration !== startGeneration) throw new E2EEInitTeardownError();
    if (this.initAttemptSequence !== attempt) throw new E2EEInitTeardownError();
    return true;
  }

  /**
   * Standard Argon2id initialization path.
   */
  private async initializeWithArgon2id(
    password: string,
    wrappedPrivateKeyBase64: string,
    salt: Uint8Array,
    startGeneration: number,
    attempt: number,
    guard?: E2EEInitializationGuard
  ): Promise<E2EEInitializationReceipt | null> {
    const exportableWrapping = await deriveKeyArgon2idExportable(password, salt);
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;
    const exportablePrefs = await derivePreferencesKeyArgon2idExportable(password, salt);
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;

    return this.finalizeKeys(
      exportableWrapping,
      exportablePrefs,
      wrappedPrivateKeyBase64,
      startGeneration,
      attempt,
      guard
    );
  }

  /**
   * Shared finalization: export raw bytes for safeStorage, re-import as non-extractable.
   */
  private async finalizeKeys(
    exportableWrapping: CryptoKey,
    exportablePrefs: CryptoKey,
    wrappedPrivateKeyBase64: string,
    startGeneration: number,
    attempt: number,
    guard?: E2EEInitializationGuard
  ): Promise<E2EEInitializationReceipt | null> {
    const wrappingRaw = await crypto.subtle.exportKey('raw', exportableWrapping);
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;
    const prefsRaw = await crypto.subtle.exportKey('raw', exportablePrefs);
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;

    // Re-import into locals first. No singleton field changes until every
    // asynchronous step is complete and the initiating account is still current.
    const wrappingKey = await crypto.subtle.importKey(
      'raw',
      wrappingRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['wrapKey', 'unwrapKey']
    );
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;
    const preferencesKey = await crypto.subtle.importKey(
      'raw',
      prefsRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt', 'decrypt']
    );
    if (!this.assertInitCommitCurrent(startGeneration, attempt, guard)) return null;

    const sessionKeys: E2EESessionKeys = {
      wrappingKeyBase64: arrayBufferToBase64(wrappingRaw),
      preferencesKeyBase64: arrayBufferToBase64(prefsRaw),
      wrappedPrivateKeyBase64,
    };

    // Commit the complete keyset synchronously so cancellation or an account
    // switch can never expose a half-initialized singleton.
    this.wrappingKey = wrappingKey;
    this.preferencesKey = preferencesKey;
    this.wrappedPrivateKey = wrappedPrivateKeyBase64;
    this.sessionKeys = sessionKeys;
    this.sessionKeysInitAttempt = attempt;

    // Mark the renderer-side E2EE store ready so the post-auth gate
    // (#270 Task 21b) can transition past SSOEagerUnlock. Source of truth
    // remains this service — the store is a downstream subscription point.
    useE2EEStore.getState().setReady(true);

    // #2423: construct the receipt from THIS invocation's local sessionKeys +
    // attempt at the synchronous commit point, so the caller receives provenance
    // for exactly the keys it committed — never a successor's ambient state.
    const receipt: E2EEInitializationReceipt = { sessionKeys, attempt };
    return receipt;
  }

  /**
   * Initialize from stored session keys (restored from safeStorage).
   * Imports raw key bytes as non-extractable CryptoKey objects.
   */
  async initializeFromStoredKeys(keys: E2EESessionKeys): Promise<void> {
    // Same commit fence as initialize()/finalizeKeys(): snapshot the generation
    // before the importKey awaits, re-import into LOCALS, and only publish the
    // singleton fields once no teardown fired. Restore has no Argon2id window
    // (only two fast importKey ops), but it is the same publish surface — a
    // 401 -> nuclearReset landing between the two awaits would otherwise leave a
    // ready=true / wrappingKey=null split-brain and resurrect sessionKeys for
    // the torn-down account (CWE-212, #2199). This mirrors finalizeKeys' commit
    // discipline; this path takes no guard, so the generation check is the fence.
    // keyClearGeneration (destruction-only), NOT keySessionGeneration — see the
    // field comment (same-session recoveryReset must not abort a commit).
    const startGeneration = this.keyClearGeneration;
    // This restore is now the newest initialization attempt — a still-pending
    // earlier attempt (or one that starts later) supersedes/aborts per the
    // initAttemptSequence contract (Codex P1, PR #2337).
    const attempt = ++this.initAttemptSequence;
    const wrappingRaw = base64ToArrayBuffer(keys.wrappingKeyBase64);
    const prefsRaw = base64ToArrayBuffer(keys.preferencesKeyBase64);

    const wrappingKey = await crypto.subtle.importKey(
      'raw',
      wrappingRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['wrapKey', 'unwrapKey']
    );
    if (!this.assertInitCommitCurrent(startGeneration, attempt)) return;
    const preferencesKey = await crypto.subtle.importKey(
      'raw',
      prefsRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt', 'decrypt']
    );
    if (!this.assertInitCommitCurrent(startGeneration, attempt)) return;

    // Synchronous commit — no await between the gate above and setReady().
    this.wrappingKey = wrappingKey;
    this.preferencesKey = preferencesKey;
    this.wrappedPrivateKey = keys.wrappedPrivateKeyBase64;
    this.sessionKeys = keys;
    this.sessionKeysInitAttempt = attempt;

    // Mirror initialize(): mark the renderer-side E2EE store ready so the
    // post-auth gate (#270 Task 21b) can fall through to MainApp. Used on
    // session-restore from safeStorage at app launch.
    useE2EEStore.getState().setReady(true);

    // No periodic cleanup needed — session-scoped cache (keys stay until rotation or logout)
  }

  /**
   * Get the exported session keys for safeStorage persistence.
   * Only available after initialize() has been called.
   */
  getSessionKeys(): E2EESessionKeys | null {
    return this.sessionKeys;
  }

  /**
   * Clear only the initialization represented by `receipt`.
   *
   * Both checks are required: session-key identity protects a newer committed
   * account, while initAttemptSequence protects a newer initialization that is
   * still deriving and would otherwise be aborted by clearKeys().
   */
  clearKeysIfInitializationCurrent(receipt: E2EEInitializationReceipt | null): boolean {
    if (
      receipt === null ||
      this.sessionKeys !== receipt.sessionKeys ||
      this.sessionKeysInitAttempt !== receipt.attempt ||
      this.initAttemptSequence !== receipt.attempt
    ) {
      return false;
    }
    this.clearKeys();
    return true;
  }

  /**
   * Check if the service is initialized (user has logged in with E2EE)
   */
  get isInitialized(): boolean {
    return this.wrappingKey !== null;
  }

  /**
   * JIT: Derive the private key from the stored wrapped key.
   * Caller uses it for a single operation, then lets it fall out of scope.
   */
  private async derivePrivateKey(): Promise<CryptoKey> {
    if (!this.wrappingKey || !this.wrappedPrivateKey) {
      throw new Error('E2EE service not initialized');
    }
    const wrappedKeyBuffer = base64ToArrayBuffer(this.wrappedPrivateKey);
    return unwrapPrivateKey(wrappedKeyBuffer, this.wrappingKey);
  }

  /**
   * Derive the device key as a non-extractable RSA-PSS *signing* handle (#1624).
   *
   * Re-unwraps the SAME wrapped device key as derivePrivateKey, but tagged for
   * RSA-PSS / ['sign'] instead of RSA-OAEP / ['decrypt']. `signAgeClaim` (below) is
   * the production entry point ageClaimService uses; this lower-level handle accessor
   * is exposed mainly for handle-level testing (asserting non-extractability / sign
   * usage) and symmetry with derivePrivateKey. Mirrors derivePrivateKey's shape.
   *
   * Modulus-reuse caveat: this signature shares the E2EE key's modulus, so it is
   * exactly as trustworthy as device-key custody — NOT an independent second
   * factor. Downstream consumers must not market it as one. See
   * docs/age-claim-canonical-form.md and [internal]rules/e2ee.md.
   */
  async deriveSigningKey(): Promise<CryptoKey> {
    if (!this.wrappingKey || !this.wrappedPrivateKey) {
      throw new Error('E2EE service not initialized');
    }
    const wrappedKeyBuffer = base64ToArrayBuffer(this.wrappedPrivateKey);
    return unwrapSigningKey(wrappedKeyBuffer, this.wrappingKey);
  }

  /**
   * Sign age-claim canonical bytes with the device key; returns base64 (#1624).
   *
   * Derives the non-extractable RSA-PSS sign handle and signs under
   * RSA-PSS / SHA-256 / saltLength 32 — the salt the server's verifier requires
   * (PSSSaltLengthAuto is rejected). Keeping crypto.subtle.sign here (a designated
   * crypto module per the concord-crypto-outside-module rule) lets ageClaimService
   * orchestrate without ever touching crypto.subtle or a CryptoKey.
   */
  async signAgeClaim(canonicalBytes: Uint8Array): Promise<string> {
    const signKey = await this.deriveSigningKey();
    const signature = await crypto.subtle.sign(
      { name: 'RSA-PSS', saltLength: 32 },
      signKey,
      canonicalBytes as BufferSource
    );
    return arrayBufferToBase64(signature);
  }

  /**
   * Cache-poison defense: assert the wrapped key is exactly 512 bytes after
   * base64 decode — the only length RSA-OAEP-4096 can produce.
   *
   * On mismatch, throw `MALFORMED_PAYLOAD`. The caller (fetch path) is
   * responsible for the bounded refetch-once dance. A strict equality check
   * (not a range) means any future wrap-format change fails loudly.
   */
  private validateWrapShape(wrappedKeyBase64: string): void {
    let bytes: ArrayBuffer;
    try {
      bytes = base64ToArrayBuffer(wrappedKeyBase64);
    } catch {
      // Malformed base64 (e.g., DOMException from atob) — classify as
      // MALFORMED_PAYLOAD so the cache-poison defense can refetch once.
      throw new E2EEKeyUnavailableError('MALFORMED_PAYLOAD', false);
    }
    if (bytes.byteLength !== E2EEService.EXPECTED_WRAP_BYTES) {
      throw new E2EEKeyUnavailableError('MALFORMED_PAYLOAD', false);
    }
  }

  /**
   * Parse a non-OK key-fetch response: update rate-limit state on 429 and
   * throw a typed `E2EEKeyUnavailableError` carrying the server's code+pending.
   * Shared by `fetchAndUnwrapChannelKey` and `getChannelKeyByVersion` so both
   * paths stay byte-identical in behavior.
   *
   * When the server response is NO_KEY_YET + pending:true, fire-and-forget a
   * peer-rewrap enrollment trigger for the channel (#1023). The server's
   * GetUnifiedKeys handler already auto-enrolls on 404; this explicit POST is
   * defense-in-depth and is idempotent on the server (ON CONFLICT DO NOTHING).
   */
  private async throwKeyFetchError(
    res: Response,
    sessionGeneration: number,
    channelId: string,
    channelGeneration: number
  ): Promise<never> {
    const body = await safeJson<ErrorResponseShape>(res).catch((): ErrorResponseShape => ({}));
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    if (res.status === 429) {
      // Number.isFinite is load-bearing, not defensive dressing. Retry-After
      // is spec-legal as an HTTP-date, which parseInt turns into NaN — and
      // `Date.now() + NaN` is NaN, for which `Date.now() < NaN` is always
      // false. The guard would then be silently INERT rather than falling back
      // to the default beside it. (#1218 review; dmStore's copy of this read
      // carries the same check.)
      const parsed = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
      const retryAfter = Number.isFinite(parsed) && parsed >= 0 ? parsed : 30;
      this.rateLimitedUntil = Date.now() + retryAfter * 1000;
    }
    const code = body.code ?? 'NO_KEY_YET';
    const pending = body.pending ?? false;

    // Fire-and-forget rewrap enrollment for the pending:true missing-key case (#1023).
    if (code === 'NO_KEY_YET' && pending) {
      this.requestRewrap(channelId).catch(() => {
        // Intentionally swallowed — enrollment is best-effort; the existing
        // pending:true classifier (retryable=true) drives the actual retry loop.
      });
    }

    throw new E2EEKeyUnavailableError(code, pending);
  }

  /**
   * Cache-poison refetch for the versioned-fetch path (`getChannelKeyByVersion`).
   * Evicts the main-cache marker + the versioned entry, consults the shared
   * refetch counter on the main cache, and either marks a new refetch-in-flight
   * slot (caller should recurse) or re-throws (terminal).
   *
   * Returns `true` if the caller should refetch once more, `false` if the
   * original error must be re-thrown (non-MALFORMED_PAYLOAD). Throws the
   * original error when the refetch budget is exhausted.
   */
  private handleMalformedVersionedWrap(channelId: string, version: number, err: unknown): boolean {
    if (!(err instanceof E2EEKeyUnavailableError) || err.code !== 'MALFORMED_PAYLOAD') {
      return false;
    }
    const existing = this.channelKeyCache.get(channelId);
    // Treat the counter as fresh (0) if the existing entry is a real cached
    // key, not a marker slot. Only a marker slot (empty wrappedKey) indicates
    // an in-flight refetch cycle.
    const alreadyRefetched = existing && !existing.wrappedKey ? existing.refetchAfterMalformed : 0;
    // Evict both caches so the retry starts from a clean slate.
    this.channelKeyCache.delete(channelId);
    this.versionedKeyCache.get(channelId)?.delete(version);
    if (alreadyRefetched >= 1) {
      throw err; // terminal — do not loop
    }
    // Mark the main-cache slot so the recursive call sees the counter.
    this.channelKeyCache.set(channelId, {
      wrappedKey: '',
      keyVersion: 0,
      lastUsed: 0,
      refetchAfterMalformed: 1,
    });
    return true;
  }

  private assertCurrentKeySession(generation: number): void {
    if (generation !== this.keySessionGeneration) {
      throw new Error('E2EE key session changed');
    }
  }

  private getChannelKeyGeneration(channelId: string): number {
    return this.channelKeyGenerations.get(channelId) ?? 0;
  }

  private assertCurrentKeyContext(
    sessionGeneration: number,
    channelId: string,
    channelGeneration: number
  ): void {
    this.assertCurrentKeySession(sessionGeneration);
    const accessRevocationGeneration = this.channelAccessRevocationGenerations.get(channelId) ?? -1;
    if (channelGeneration < accessRevocationGeneration) {
      // Access loss is terminal for work captured before the revocation. Check
      // this before the ordinary key-generation mismatch so callers never
      // misclassify a removal as a retryable rotation.
      throw new E2EEKeyUnavailableError('NOT_MEMBER', false);
    }
    if (channelGeneration !== this.getChannelKeyGeneration(channelId)) {
      // Rotation/retry invalidation is transient for an otherwise-live
      // account session. Surface the existing pending-key shape so callers
      // retry instead of permanently marking messages undecryptable.
      throw new E2EEKeyUnavailableError('NO_KEY_YET', true);
    }
  }

  createChannelOperationGuard(channelId: string): E2EEChannelOperationGuard {
    const sessionGeneration = this.keySessionGeneration;
    const channelGeneration = this.getChannelKeyGeneration(channelId);
    return {
      assertCurrent: () =>
        this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration),
    };
  }

  /**
   * Rotation sends `key_delivered` to its own holder before the upload response.
   * That expected cache invalidation must not abort later rotation batches, but
   * session changes and terminal access revocation still fence the operation.
   */
  createChannelRotationGuard(channelId: string): E2EEChannelOperationGuard {
    const sessionGeneration = this.keySessionGeneration;
    const accessRevocationGeneration = this.channelAccessRevocationGenerations.get(channelId) ?? -1;
    return {
      assertCurrent: () => {
        this.assertCurrentKeySession(sessionGeneration);
        if (
          (this.channelAccessRevocationGenerations.get(channelId) ?? -1) !==
          accessRevocationGeneration
        ) {
          throw new E2EEKeyUnavailableError('NOT_MEMBER', false);
        }
      },
    };
  }

  /**
   * Get the unwrapped channel key for a channel (JIT).
   * Fetches the wrapped key from cache or server, unwraps with private key.
   * Uses pendingKeyFetches to prevent cache stampeding (concurrent fetches for the same channel).
   */
  async getChannelKey(channelId: string): Promise<CryptoKey> {
    const material = await this.getChannelKeyMaterial(channelId);
    return material.channelKey;
  }

  /**
   * Get the current channel key together with the exact epoch selected for it.
   * The wrapped key and version are captured before any await so cache rotation
   * cannot pair key material from one epoch with metadata from another.
   */
  async getChannelKeyMaterial(channelId: string): Promise<ChannelKeyMaterial> {
    if (!channelId) {
      throw new E2EEKeyUnavailableError('INVALID_REQUEST', false);
    }
    const sessionGeneration = this.keySessionGeneration;
    const channelGeneration = this.getChannelKeyGeneration(channelId);

    // Check cache for wrapped key. Guard against refetch-marker slots whose
    // `wrappedKey` is empty (see fetchAndUnwrapChannelKey cache-poison path).
    const cached = this.channelKeyCache.get(channelId);
    if (cached?.wrappedKey && Date.now() - cached.lastUsed < CHANNEL_KEY_CACHE_TTL) {
      const wrappedKey = cached.wrappedKey;
      const keyVersion = cached.keyVersion;
      cached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      // #1878: a cache hit re-observes the cached version (idempotent — only a
      // strictly-higher version fires the rotation emitter).
      this.noteChannelVersion(channelId, keyVersion);
      const channelKey = await unwrapChannelKey(wrappedKey, privateKey);
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      return { channelKey, keyVersion };
    }

    // If we're rate-limited, don't fire another request
    if (Date.now() < this.rateLimitedUntil) {
      throw new E2EEKeyUnavailableError('NO_KEY_YET', false);
    }

    // Deduplicate concurrent fetches for the same channel
    const pending = this.pendingKeyFetches.get(channelId);
    if (pending) {
      return pending;
    }

    const fetchPromise = this.fetchAndUnwrapChannelKey(
      channelId,
      sessionGeneration,
      channelGeneration
    );
    this.pendingKeyFetches.set(channelId, fetchPromise);

    try {
      return await fetchPromise;
    } finally {
      if (this.pendingKeyFetches.get(channelId) === fetchPromise) {
        this.pendingKeyFetches.delete(channelId);
      }
    }
  }

  /**
   * Internal: fetch wrapped key from server, cache it, and unwrap.
   */
  private async fetchAndUnwrapChannelKey(
    channelId: string,
    sessionGeneration: number,
    channelGeneration: number
  ): Promise<ChannelKeyMaterial> {
    const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}`);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    if (!res.ok) {
      await this.throwKeyFetchError(res, sessionGeneration, channelId, channelGeneration);
    }
    const data = await safeJson<KeyResponseShape>(res);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const wrappedKey: string = data.key.wrapped_key;
    const keyVersion: number = data.key.key_version || 1;

    // Cache-poison defense: validate wrap shape before trusting the cache.
    // Strict 512-byte check (RSA-OAEP-4096 output). On failure, refetch once
    // and if the refetch is also malformed, surface MALFORMED_PAYLOAD as
    // terminal — we cannot remediate a server that persistently serves
    // corrupt data, but we MUST not loop forever.
    try {
      this.validateWrapShape(wrappedKey);
    } catch (err) {
      if (err instanceof E2EEKeyUnavailableError && err.code === 'MALFORMED_PAYLOAD') {
        const existing = this.channelKeyCache.get(channelId);
        // Treat the counter as fresh (0) if the existing entry is a real
        // cached key, not a marker slot. Only a marker slot (empty
        // wrappedKey) indicates an in-flight refetch cycle.
        const alreadyRefetched =
          existing && !existing.wrappedKey ? existing.refetchAfterMalformed : 0;
        this.channelKeyCache.delete(channelId);
        if (alreadyRefetched >= 1) {
          throw err; // terminal — do not loop
        }
        // Mark the slot with a refetch counter so the recursive call sees it.
        // Empty wrappedKey is guarded by the getChannelKey cache-read branch.
        this.channelKeyCache.set(channelId, {
          wrappedKey: '',
          keyVersion: 0,
          lastUsed: 0,
          refetchAfterMalformed: 1,
        });
        return this.fetchAndUnwrapChannelKey(channelId, sessionGeneration, channelGeneration);
      }
      throw err;
    }

    // Cache the wrapped key (counter resets on successful validation)
    this.channelKeyCache.set(channelId, {
      wrappedKey,
      keyVersion,
      lastUsed: Date.now(),
      refetchAfterMalformed: 0,
    });

    // #1878: a fresh current-key fetch observes its version — fires the rotation
    // emitter if the channel's current version advanced.
    this.noteChannelVersion(channelId, keyVersion);

    // JIT unwrap: derive private key, unwrap channel key, private key falls out of scope
    const privateKey = await this.derivePrivateKey();
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const channelKey = await unwrapChannelKey(wrappedKey, privateKey);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    return { channelKey, keyVersion };
  }

  /**
   * Request peer-fulfilled rewrap for a missing key (#1023).
   *
   * Fire-and-forget enrollment trigger. The server INSERTs into
   * pending_key_requests / dm_pending_key_requests; a peer client polling
   * GetPendingKeyRequests fulfills by wrapping the CSK for this user.
   *
   * Idempotent: server uses ON CONFLICT DO NOTHING.
   *
   * Honors rateLimitedUntil per the existing pattern in getChannelKey
   * (line 323-326) — no-op if rate-limited.
   *
   * Throws on non-2xx, non-429 responses; callers in the
   * fetchAndUnwrapChannelKey path use `.catch(() => {})` to ignore.
   */
  async requestRewrap(contextId: string): Promise<void> {
    if (Date.now() < this.rateLimitedUntil) {
      return;
    }

    const res = await apiFetch(`/api/v1/e2ee/keys/${contextId}/rewrap`, {
      method: 'POST',
    });

    if (res.status === 429) {
      // See throwKeyFetchError above for why the finite check matters.
      const parsed = Number.parseInt(res.headers.get('Retry-After') ?? '', 10);
      const retryAfter = Number.isFinite(parsed) && parsed >= 0 ? parsed : 60;
      this.rateLimitedUntil = Date.now() + retryAfter * 1000;
      return;
    }

    if (!res.ok) {
      throw new Error(`requestRewrap failed: ${res.status}`);
    }
  }

  /**
   * Encrypt a message for a channel.
   * JIT: gets channel key, encrypts, key falls out of scope.
   */
  async encryptForChannel(channelId: string, plaintext: string): Promise<string> {
    const operationGuard = this.createChannelOperationGuard(channelId);
    const channelKey = await this.getChannelKey(channelId);
    operationGuard.assertCurrent();
    const ciphertext = await encryptMessage(plaintext, channelKey);
    operationGuard.assertCurrent();
    return ciphertext;
  }

  /**
   * Encrypt a message and bind it to the version of the selected channel key.
   * The version is captured before WebCrypto yields so a concurrent cache
   * invalidation cannot stamp the ciphertext with a different key epoch.
   */
  async encryptForChannelWithVersion(
    channelId: string,
    plaintext: string
  ): Promise<{ ciphertext: string; keyVersion: number }> {
    const operationGuard = this.createChannelOperationGuard(channelId);
    const { channelKey, keyVersion } = await this.getChannelKeyMaterial(channelId);
    operationGuard.assertCurrent();
    const ciphertext = await encryptMessage(plaintext, channelKey);
    operationGuard.assertCurrent();
    return { ciphertext, keyVersion };
  }

  /**
   * Decrypt a message from a channel.
   * JIT: gets channel key, decrypts, key falls out of scope.
   */
  async decryptForChannel(channelId: string, ciphertext: string): Promise<string> {
    const sessionGeneration = this.keySessionGeneration;
    const channelGeneration = this.getChannelKeyGeneration(channelId);
    const channelKey = await this.getChannelKey(channelId);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const plaintext = await decryptMessage(ciphertext, channelKey);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    return plaintext;
  }

  /**
   * Decrypt a message using a pre-fetched channel key.
   * Avoids redundant getChannelKey() calls when batch-decrypting.
   */
  async decryptWithKey(
    ciphertext: string,
    channelKey: CryptoKey,
    operationGuard: E2EEChannelOperationGuard
  ): Promise<string> {
    operationGuard.assertCurrent();
    const plaintext = await decryptMessage(ciphertext, channelKey);
    operationGuard.assertCurrent();
    return plaintext;
  }

  /**
   * Create channel keys for all members when creating an E2EE channel.
   * Generates a new CSK, wraps it for each member's public key.
   *
   * @param memberPublicKeys - Map of user_id → base64 public key
   * @returns Map of user_id → base64 wrapped CSK
   */
  async createChannelKeys(memberPublicKeys: Map<string, string>): Promise<Map<string, string>> {
    const channelKey = await generateChannelKey();
    const wrappedKeys = new Map<string, string>();

    for (const [userId, publicKeyBase64] of memberPublicKeys) {
      const publicKey = await importPublicKey(publicKeyBase64);
      const wrapped = await wrapChannelKey(channelKey, publicKey);
      wrappedKeys.set(userId, wrapped);
    }

    return wrappedKeys;
  }

  /**
   * Wrap the channel key for a new member (key distribution).
   * Used when an existing member distributes keys to a joining member.
   */
  async wrapKeyForMember(channelId: string, memberPublicKeyBase64: string): Promise<string> {
    return (await this.wrapKeyForMemberWithFingerprint(channelId, memberPublicKeyBase64))
      .wrappedKey;
  }

  private async wrapKeyForMemberWithFingerprint(
    channelId: string,
    memberPublicKeyBase64: string,
    keyVersion?: number
  ): Promise<{ wrappedKey: string; keyFingerprint: string }> {
    // Get the unwrapped channel key
    const channelKey =
      keyVersion !== undefined && keyVersion > 0
        ? await this.getChannelKeyByVersion(channelId, keyVersion)
        : await this.getChannelKey(channelId);
    const memberPublicKey = await importPublicKey(memberPublicKeyBase64);

    // Export and re-wrap for the new member
    const rawKey = await exportChannelKey(channelKey);
    const tempKey = await crypto.subtle.importKey(
      'raw',
      rawKey,
      { name: 'AES-GCM', length: 256 },
      true,
      ['encrypt', 'decrypt']
    );
    return {
      wrappedKey: await wrapChannelKey(tempKey, memberPublicKey),
      keyFingerprint: arrayBufferToBase64(await crypto.subtle.digest('SHA-256', rawKey)),
    };
  }

  /**
   * Process pending key requests — auto-wrap keys for new members.
   */
  processPendingKeyRequests(): Promise<void> {
    if (!this.isInitialized) return Promise.resolve();
    const sessionGeneration = this.keySessionGeneration;

    if (
      this.pendingKeyRequestProcessor !== null &&
      this.pendingKeyRequestProcessorGeneration === sessionGeneration
    ) {
      this.pendingKeyRequestRerun = true;
      return this.pendingKeyRequestProcessor;
    }
    this.pendingKeyRequestProcessor = null;
    this.pendingKeyRequestProcessorGeneration = null;
    this.pendingKeyRequestRerun = false;

    const processor = (async () => {
      do {
        this.pendingKeyRequestRerun = false;
        await this.processPendingKeyRequestsOnce(sessionGeneration);
      } while (
        this.pendingKeyRequestRerun &&
        this.isPendingKeyRequestSessionCurrent(sessionGeneration)
      );
    })();
    this.pendingKeyRequestProcessor = processor;
    this.pendingKeyRequestProcessorGeneration = sessionGeneration;
    return processor.finally(() => {
      if (
        this.pendingKeyRequestProcessor === processor &&
        this.pendingKeyRequestProcessorGeneration === sessionGeneration
      ) {
        this.pendingKeyRequestProcessor = null;
        this.pendingKeyRequestProcessorGeneration = null;
      }
    });
  }

  private isPendingKeyRequestSessionCurrent(sessionGeneration: number): boolean {
    return this.isInitialized && this.keySessionGeneration === sessionGeneration;
  }

  private async processPendingKeyRequestsOnce(sessionGeneration: number): Promise<void> {
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return;

    try {
      const requests = await this.fetchPendingKeyRequests(sessionGeneration);
      if (requests === null) return;
      console.debug('[E2EE] Pending key requests:', requests.length);

      for (const request of requests) {
        const action = await this.processPendingKeyRequest(request, sessionGeneration);
        if (action === 'stop') break;
      }
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return;
      console.warn('[E2EE] processPendingKeyRequests fatal', {
        error: (err as Error).message,
      });
      this.schedulePendingKeyRetry(sessionGeneration);
    }
  }

  private async fetchPendingKeyRequests(
    sessionGeneration: number
  ): Promise<PendingKeyRequest[] | null> {
    let res: Response;
    try {
      res = await apiFetch('/api/v1/e2ee/pending-keys');
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return null;
      console.warn('[E2EE] pending-keys request failed', { error: (err as Error).message });
      this.schedulePendingKeyRetry(sessionGeneration);
      return null;
    }
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return null;
    if (!res.ok) {
      console.debug('[E2EE] pending-keys request failed:', res.status, res.statusText);
      if (pendingKeyResponseIsRetryable(res)) this.schedulePendingKeyRetry(sessionGeneration);
      return null;
    }

    let data: { pending_requests?: PendingKeyRequest[] };
    try {
      data = await safeJson(res);
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return null;
      console.warn('[E2EE] pending-keys response was invalid', { error: (err as Error).message });
      this.schedulePendingKeyRetry(sessionGeneration);
      return null;
    }
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return null;
    return data.pending_requests || [];
  }

  private async processPendingKeyRequest(
    request: PendingKeyRequest,
    sessionGeneration: number
  ): Promise<'continue' | 'stop'> {
    const recipientKey = await this.fetchPendingRecipientPublicKey(request, sessionGeneration);
    if (recipientKey.action !== 'process') return recipientKey.action;

    const wrappedKey = await this.wrapPendingKey(request, recipientKey.data, sessionGeneration);
    if (wrappedKey.action !== 'process') return wrappedKey.action;

    return this.uploadPendingKey(request, recipientKey.data, wrappedKey.data, sessionGeneration);
  }

  private async fetchPendingRecipientPublicKey(
    request: PendingKeyRequest,
    sessionGeneration: number
  ): Promise<PendingKeyStepResult<PendingRecipientPublicKey>> {
    let res: Response;
    try {
      res = await apiFetch(`/api/v1/users/${request.user_id}/public-key`);
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
      console.warn('[E2EE] Failed to fetch public key for pending key distribution', {
        userId: request.user_id,
        error: (err as Error).message,
      });
      this.schedulePendingKeyRetry(sessionGeneration);
      return { action: 'stop' };
    }
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
    if (!res.ok) {
      console.warn('[E2EE] Failed to fetch public key for pending key distribution', {
        userId: request.user_id,
        status: res.status,
      });
      if (pendingKeyResponseIsRetryable(res)) {
        this.schedulePendingKeyRetry(sessionGeneration);
        return { action: 'stop' };
      }
      return { action: 'continue' };
    }

    try {
      const data = await safeJson<PendingRecipientPublicKey>(res);
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
      return { action: 'process', data };
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
      console.warn('[E2EE] pending public key response was invalid', {
        userId: request.user_id,
        error: (err as Error).message,
      });
      this.schedulePendingKeyRetry(sessionGeneration);
      return { action: 'stop' };
    }
  }

  private async wrapPendingKey(
    request: PendingKeyRequest,
    recipientKey: PendingRecipientPublicKey,
    sessionGeneration: number
  ): Promise<PendingKeyStepResult<WrappedPendingKey>> {
    try {
      const data = await this.wrapKeyForMemberWithFingerprint(
        request.channel_id,
        recipientKey.public_key,
        request.key_version
      );
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
      return { action: 'process', data };
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return { action: 'stop' };
      console.warn('[E2EE] Failed to process pending key request', {
        channelId: request.channel_id,
        userId: request.user_id,
        error: (err as Error).message,
      });
      if (
        err instanceof TypeError ||
        (err instanceof E2EEKeyUnavailableError &&
          (err.code === 'INTERNAL_ERROR' || err.code === 'NO_KEY_YET'))
      ) {
        this.schedulePendingKeyRetry(sessionGeneration);
        return { action: 'stop' };
      }
      return { action: 'continue' };
    }
  }

  private async uploadPendingKey(
    request: PendingKeyRequest,
    recipientKey: PendingRecipientPublicKey,
    wrappedKey: WrappedPendingKey,
    sessionGeneration: number
  ): Promise<'continue' | 'stop'> {
    // Echo the recipient's public-key version the CSK was wrapped against (#2420)
    // so the server can skip + self-heal a wrap that raced a concurrent key reset.
    const uploadBody: {
      wrapped_keys: Record<string, string>;
      key_fingerprint: string;
      key_version?: number;
      wrapped_key_versions?: Record<string, number>;
    } = {
      wrapped_keys: { [request.user_id]: wrappedKey.wrappedKey },
      key_fingerprint: wrappedKey.keyFingerprint,
    };
    if (recipientKey.key_version !== undefined) {
      uploadBody.wrapped_key_versions = { [request.user_id]: recipientKey.key_version };
    }
    if (request.key_version !== undefined && request.key_version > 0) {
      uploadBody.key_version = request.key_version;
    }

    let res: Response;
    try {
      res = await apiFetch(`/api/v1/e2ee/keys/${request.channel_id}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(uploadBody),
      });
    } catch (err) {
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return 'stop';
      console.warn('[E2EE] Failed to upload pending key distribution', {
        channelId: request.channel_id,
        userId: request.user_id,
        error: (err as Error).message,
      });
      this.schedulePendingKeyRetry(sessionGeneration);
      return 'stop';
    }
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return 'stop';
    console.debug(
      '[E2EE] Key distributed for',
      request.user_id,
      'channel',
      request.channel_id,
      res.status
    );
    if (!res.ok && pendingKeyResponseIsRetryable(res)) {
      this.schedulePendingKeyRetry(sessionGeneration);
      return 'stop';
    }
    return 'continue';
  }

  private schedulePendingKeyRetry(sessionGeneration: number): void {
    if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return;
    if (this.pendingKeyRetryTimer !== null) return;
    this.pendingKeyRetryTimer = setTimeout(() => {
      this.pendingKeyRetryTimer = null;
      if (!this.isPendingKeyRequestSessionCurrent(sessionGeneration)) return;
      void this.processPendingKeyRequests();
    }, PENDING_KEY_RETRY_DELAY_MS);
  }

  /**
   * Encrypt a preferences blob using the domain-separated preferences key.
   */
  async encryptPreferences<T>(data: T): Promise<string> {
    if (!this.preferencesKey) {
      throw new Error('E2EE service not initialized');
    }
    return encryptBlob(data, this.preferencesKey);
  }

  /**
   * Decrypt a preferences blob using the domain-separated preferences key.
   */
  async decryptPreferences<T>(ciphertextBase64: string): Promise<T> {
    if (!this.preferencesKey) {
      throw new Error('E2EE service not initialized');
    }
    return decryptBlob<T>(ciphertextBase64, this.preferencesKey);
  }

  /**
   * Get the current (latest) key version for a channel from cache.
   * Returns 1 if no key is cached yet.
   */
  getCurrentKeyVersion(channelId: string): number {
    const cached = this.channelKeyCache.get(channelId);
    return cached?.keyVersion ?? 1;
  }

  /**
   * #1878: the authoritative channel-key version currently cached for a channel,
   * or 0 when no key is cached yet. Used by voiceService to bind the media
   * encrypt key's version at E2EE init (the value stamped into the versioned frame
   * trailer). Distinct from `getCurrentKeyVersion` (defaults to 1) — here the
   * floor is 0 so an unbound channel maps to the "no version known yet" value
   * rather than silently claiming v1.
   */
  getChannelKeyVersion(channelId: string): number {
    return this.channelKeyCache.get(channelId)?.keyVersion ?? 0;
  }

  /**
   * Highest channel-key version observed through either the current-key or a
   * by-version fetch. This intentionally differs from getChannelKeyVersion():
   * the current-key cache can still hold vN after a decrypt-side history fetch
   * has confirmed vN+1. Media initialization reads this after subscribing so it
   * can reconcile an edge-triggered rotation that happened before its live
   * sender subscription was committed.
   */
  getHighestSeenKeyVersion(channelId: string): number {
    return this.highestSeenVersion.get(channelId) ?? 0;
  }

  /**
   * Fetch and unwrap a specific key version for a channel (for decrypting old messages).
   * Results are cached in the versioned key cache.
   *
   * Cache hits return BEFORE allocating an in-flight dedup promise. Concurrent
   * cache-miss callers for the same `(channelId, version)` share one network
   * fetch via `pendingVersionedKeyFetches` under a compound key
   * `${channelId}:v${version}` — isolated from the current-key material fetches,
   * so the two paths never collide (#1878).
   */
  async getChannelKeyByVersion(channelId: string, version: number): Promise<CryptoKey> {
    if (!channelId) {
      throw new E2EEKeyUnavailableError('INVALID_REQUEST', false);
    }
    const sessionGeneration = this.keySessionGeneration;
    const channelGeneration = this.getChannelKeyGeneration(channelId);

    // Check if this is the current version — use main cache.
    // Guard against empty wrappedKey (cache-poison marker slot).
    const mainCached = this.channelKeyCache.get(channelId);
    if (mainCached?.wrappedKey && mainCached.keyVersion === version) {
      const wrappedKey = mainCached.wrappedKey;
      mainCached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      this.noteChannelVersion(channelId, version); // #1878
      const channelKey = await unwrapChannelKey(wrappedKey, privateKey);
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      return channelKey;
    }

    // Check versioned cache
    const versionCached = this.versionedKeyCache.get(channelId)?.get(version);
    if (versionCached?.wrappedKey && Date.now() - versionCached.lastUsed < CHANNEL_KEY_CACHE_TTL) {
      const wrappedKey = versionCached.wrappedKey;
      versionCached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      this.noteChannelVersion(channelId, version); // #1878
      const channelKey = await unwrapChannelKey(wrappedKey, privateKey);
      this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
      return channelKey;
    }

    // If we're rate-limited, don't fire another request
    if (Date.now() < this.rateLimitedUntil) {
      throw new E2EEKeyUnavailableError('NO_KEY_YET', false);
    }

    // #1878: by-version in-flight dedup lives in a dedicated map isolated from
    // current-key material fetches. Allocate only AFTER the cache misses above,
    // so a cache hit never creates a pending promise.
    const dedupKey = `${channelId}:v${version}`;
    const inflight = this.pendingVersionedKeyFetches.get(dedupKey);
    if (inflight) {
      return inflight;
    }
    const fetchPromise = this.fetchChannelKeyByVersion(
      channelId,
      version,
      sessionGeneration,
      channelGeneration
    );
    this.pendingVersionedKeyFetches.set(dedupKey, fetchPromise);
    try {
      return await fetchPromise;
    } finally {
      if (this.pendingVersionedKeyFetches.get(dedupKey) === fetchPromise) {
        this.pendingVersionedKeyFetches.delete(dedupKey);
      }
    }
  }

  /**
   * Internal: fetch a specific channel-key version from the server, validate the
   * wrap shape (bounded cache-poison refetch), cache it in the versioned cache,
   * note the observed version (rotation emitter), and unwrap. Extracted from
   * `getChannelKeyByVersion` so the in-flight dedup wraps exactly one body and
   * the malformed-wrap retry recurses on the body (never re-allocating a dedup
   * promise). See #1878.
   */
  private async fetchChannelKeyByVersion(
    channelId: string,
    version: number,
    sessionGeneration: number,
    channelGeneration: number
  ): Promise<CryptoKey> {
    // Fetch specific version from server
    const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}?version=${version}`);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    if (!res.ok) {
      await this.throwKeyFetchError(res, sessionGeneration, channelId, channelGeneration);
    }
    const data = await safeJson<KeyResponseShape>(res);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const wrappedKey: string = data.key.wrapped_key;

    // Cache-poison defense: shares the refetch counter on the main cache so
    // "one refetch per channel" holds across both fetch paths. On shape-fail,
    // evict the main-cache marker + versioned entry for this version, then
    // refetch once (bounded). `handleMalformedVersionedWrap` throws on
    // non-MALFORMED_PAYLOAD or exhausted budget; returns true to signal the
    // caller should recurse.
    try {
      this.validateWrapShape(wrappedKey);
    } catch (err) {
      if (this.handleMalformedVersionedWrap(channelId, version, err)) {
        return this.fetchChannelKeyByVersion(
          channelId,
          version,
          sessionGeneration,
          channelGeneration
        );
      }
      throw err;
    }

    // SERVER-AUTHORITATIVE EPOCH -- record what was SERVED, never what was
    // ASKED FOR.
    //
    // `version` is attacker-reachable. It arrives from a client-attested
    // key_version that no upload path validates: as `message.key_version` on
    // the message path, and (since #2157 PR 2) as the X-File-Key-Version header
    // an attachment download reflects back, which fires on passive scroll.
    // Recording the request rather than the response let a fabricated epoch
    // install the current CSK under a number no rotation ever produced, and
    // drive the MONOTONIC rotation watermark below to an arbitrary value --
    // after which every genuine rotation compares <= and is dropped, so the
    // sender keeps encrypting under a CSK a revocation was supposed to retire.
    // A PoC drove it to media teardown on the DM path.
    //
    // The DM branch is now exact-match (fetchDMKey), so a mismatch should no
    // longer be reachable from our own server. This is what makes that
    // unnecessary rather than what depends on it.
    const servedVersion =
      typeof data.key.key_version === 'number' && Number.isSafeInteger(data.key.key_version)
        ? data.key.key_version
        : version;

    // Cache in versioned cache (refetchAfterMalformed: 0 — validation passed)
    let versionMapForChannel = this.versionedKeyCache.get(channelId);
    if (!versionMapForChannel) {
      versionMapForChannel = new Map();
      this.versionedKeyCache.set(channelId, versionMapForChannel);
    }
    versionMapForChannel.set(servedVersion, {
      wrappedKey,
      keyVersion: servedVersion,
      lastUsed: Date.now(),
      refetchAfterMalformed: 0,
    });

    // Clear any stale marker slot left on the main cache by a prior refetch
    // cycle — a successful versioned fetch means the refetch counter is resolved.
    const staleMarker = this.channelKeyCache.get(channelId);
    if (staleMarker && !staleMarker.wrappedKey) {
      this.channelKeyCache.delete(channelId);
    }

    // #1878: observe the fetched version — fires the rotation emitter on
    // increase. servedVersion, not version: see above. This is the watermark
    // the poison targeted.
    this.noteChannelVersion(channelId, servedVersion);

    const privateKey = await this.derivePrivateKey();
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const channelKey = await unwrapChannelKey(wrappedKey, privateKey);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    return channelKey;
  }

  /**
   * Decrypt a message using a specific key version.
   * Falls back to current key if version is 0 or 1 (legacy).
   */
  async decryptForChannelWithVersion(
    channelId: string,
    ciphertext: string,
    version: number
  ): Promise<string> {
    // Version 0 or 1 = use current key (legacy/default)
    if (!version || version <= 1) {
      return this.decryptForChannel(channelId, ciphertext);
    }

    // Check if the requested version matches current cached version
    const currentVersion = this.getCurrentKeyVersion(channelId);
    if (version === currentVersion) {
      return this.decryptForChannel(channelId, ciphertext);
    }

    // Fetch specific version
    const sessionGeneration = this.keySessionGeneration;
    const channelGeneration = this.getChannelKeyGeneration(channelId);
    const channelKey = await this.getChannelKeyByVersion(channelId, version);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    const plaintext = await decryptMessage(ciphertext, channelKey);
    this.assertCurrentKeyContext(sessionGeneration, channelId, channelGeneration);
    return plaintext;
  }

  /**
   * Rotate the channel key: generate a new CSK, wrap for all members, and distribute.
   * Used after member removal or theft detection.
   *
   * @param channelId - The channel to rotate keys for
   * @param newKeyVersion - The new epoch/version number
   * @param memberPublicKeys - Map of user_id → base64 public key for remaining members
   * @param wrappedKeyVersions - Public-key versions for recipient freshness checks
   * @param operationGuard - Lifecycle guard captured by the rotation caller
   */
  async rotateChannelKey(
    channelId: string,
    newKeyVersion: number,
    memberPublicKeys: Map<string, string>,
    wrappedKeyVersions?: Record<string, number>,
    operationGuard: E2EEChannelOperationGuard = this.createChannelOperationGuard(channelId)
  ): Promise<void> {
    operationGuard.assertCurrent();
    const channelKey = await generateChannelKey();
    operationGuard.assertCurrent();
    const exportedChannelKey = await exportChannelKey(channelKey);
    operationGuard.assertCurrent();
    const keyFingerprint = arrayBufferToBase64(
      await crypto.subtle.digest('SHA-256', exportedChannelKey)
    );
    operationGuard.assertCurrent();
    const wrappedMembers: Array<[string, string]> = [];
    for (const [userId, publicKeyBase64] of memberPublicKeys) {
      operationGuard.assertCurrent();
      try {
        const publicKey = await importPublicKey(publicKeyBase64);
        operationGuard.assertCurrent();
        wrappedMembers.push([userId, await wrapChannelKey(channelKey, publicKey)]);
      } catch (err) {
        console.warn('[E2EE] Skipping member with invalid public key during rotation', {
          userId,
          error: (err as Error).message,
        });
      }
      operationGuard.assertCurrent();
    }
    if (wrappedMembers.length === 0) {
      throw new Error('channel key rotation has no valid recipients');
    }
    const wrappedBatches: Array<Record<string, string>> = [];
    for (let start = 0; start < wrappedMembers.length; start += 500) {
      wrappedBatches.push(Object.fromEntries(wrappedMembers.slice(start, start + 500)));
    }

    for (const wrappedKeys of wrappedBatches) {
      const batchKeyVersions: Record<string, number> = {};
      for (const userId of Object.keys(wrappedKeys)) {
        const keyVersion = wrappedKeyVersions?.[userId];
        if (keyVersion !== undefined) batchKeyVersions[userId] = keyVersion;
      }
      const body: {
        wrapped_keys: Record<string, string>;
        key_version: number;
        key_fingerprint: string;
        wrapped_key_versions?: Record<string, number>;
      } = {
        wrapped_keys: wrappedKeys,
        key_version: newKeyVersion,
        key_fingerprint: keyFingerprint,
      };
      if (Object.keys(batchKeyVersions).length > 0) body.wrapped_key_versions = batchKeyVersions;
      operationGuard.assertCurrent();
      const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(body),
      });
      operationGuard.assertCurrent();
      if (!res.ok) {
        const data = await safeJson(res).catch(() => ({}));
        operationGuard.assertCurrent();
        console.debug('[E2EE] Key rotation distribution failed:', res.status, data);
        throw new Error('channel key rotation distribution failed');
      }
    }

    // Invalidate current cache so next encrypt/decrypt fetches the new key
    operationGuard.assertCurrent();
    this.invalidateChannelKey(channelId);
  }

  /**
   * Get the password-derived wrapping key (for recovery key wrapping).
   * Returns null if the service is not initialized.
   */
  getWrappingKey(): CryptoKey | null {
    return this.wrappingKey;
  }

  /**
   * Get the wrapped (encrypted) private key blob (base64).
   * This is the password-wrapped private key stored on the server.
   */
  getWrappedPrivateKey(): string {
    return this.wrappedPrivateKey;
  }

  /**
   * Get the preferences key as base64 (for recovery key wrapping).
   * Returns the exported raw key bytes from the session keys cache.
   * Returns null if the service is not initialized or session keys are unavailable.
   */
  getPreferencesKeyBase64(): string | null {
    const keys = this.getSessionKeys();
    return keys?.preferencesKeyBase64 ?? null;
  }

  /**
   * Invalidates in-flight key work without dropping initialized keys or caches.
   * Same-session resets use this fence so late decrypts cannot outlive content cleanup.
   */
  fencePendingOperations(): void {
    if (this.pendingKeyRetryTimer !== null) {
      clearTimeout(this.pendingKeyRetryTimer);
      this.pendingKeyRetryTimer = null;
    }
    this.keySessionGeneration += 1;
    this.pendingKeyFetches.clear();
    this.pendingVersionedKeyFetches.clear();
  }

  /**
   * Clear all keys on logout.
   */
  clearKeys(): void {
    this.pendingKeyRequestProcessor = null;
    this.pendingKeyRequestProcessorGeneration = null;
    this.pendingKeyRequestRerun = false;
    this.fencePendingOperations();
    // Destruction marker for the init-commit fence (assertInitCommitCurrent):
    // bumped ONLY here, never in fencePendingOperations — see the field comment.
    this.keyClearGeneration += 1;
    this.rateLimitedUntil = 0;
    this.wrappingKey = null;
    this.preferencesKey = null;
    this.wrappedPrivateKey = '';
    this.sessionKeys = null;
    this.sessionKeysInitAttempt = null;
    this.channelKeyCache.clear();
    this.versionedKeyCache.clear();
    this.channelKeyGenerations.clear();
    this.channelAccessRevocationGenerations.clear();
    // #1878: drop all rotation baselines on logout — a new session must not
    // inherit a stale highest-seen version. Listeners are NOT cleared here:
    // subscribers (voiceService) own their unsubscribe lifecycle.
    this.highestSeenVersion.clear();

    // Reset the reactive flag so the post-auth gate (#270 Task 21b) goes
    // back to its pre-init state. We deliberately do NOT touch
    // `needsSSOUnlock` here — that one-shot signal belongs to useSSOFlow.
    useE2EEStore.getState().setReady(false);
  }

  /**
   * Invalidate cached key for a specific channel (e.g., on key rotation).
   * Also clears historical versioned keys for the channel.
   */
  invalidateChannelKey(channelId: string): void {
    this.channelKeyGenerations.set(channelId, this.getChannelKeyGeneration(channelId) + 1);
    this.channelKeyCache.delete(channelId);
    this.versionedKeyCache.delete(channelId);
    this.pendingKeyFetches.delete(channelId);
    const versionedPrefix = `${channelId}:v`;
    for (const dedupKey of this.pendingVersionedKeyFetches.keys()) {
      if (dedupKey.startsWith(versionedPrefix)) {
        this.pendingVersionedKeyFetches.delete(dedupKey);
      }
    }
    // #1878: DO NOT clear highestSeenVersion here. invalidateChannelKey is the
    // primary CSK-rotation trigger path (WS revocation / performKeyRotation,
    // requestRecovery self-heal, key_delivered, channel_access_revoked) — it
    // runs BEFORE the subsequent getChannelKey refetch re-observes the new
    // version. If we deleted the baseline, that refetch would see prev=-1
    // (first-ever) and only re-seed, so noteChannelVersion would NOT fire the
    // rotation emitter — the sender would never re-base and would keep stamping
    // the OLD keyVersion, defeating the entire #1878 fix (caught by Gitar +
    // e2ee-reviewer on PR #1885). Keeping the baseline is safe: noteChannelVersion
    // only fires on a STRICTLY-higher version, so a cache-bust followed by a
    // same-version refetch produces no fire, while a genuine rotation (higher
    // version) correctly fires. Logout drops all baselines via clearKeys().
  }

  /**
   * Fence work captured before authoritative channel/DM access loss.
   * Unlike an ordinary key rotation, an access revocation is terminal and
   * must not trigger history refetch retries that could republish stale data.
   */
  revokeChannelAccess(channelId: string): void {
    this.invalidateChannelKey(channelId);
    this.channelAccessRevocationGenerations.set(channelId, this.getChannelKeyGeneration(channelId));
  }

  /**
   * #1878: Subscribe to confirmed CSK rotations. The listener receives
   * `{ channelId, keyVersion }` exactly once per strictly-higher version that
   * the service observes/caches for a channel. Returns an unsubscribe fn.
   */
  onKeyRotation(listener: (e: { channelId: string; keyVersion: number }) => void): () => void {
    this.keyRotationListeners.add(listener);
    return () => this.keyRotationListeners.delete(listener);
  }

  /**
   * #1878: Record an observed channel-key version. Fires the rotation emitter
   * once when `keyVersion` is strictly higher than the previously-seen version
   * for the channel. The first-ever observation only seeds the baseline (no
   * fire). Listener errors are swallowed so a misbehaving subscriber can never
   * break the key-fetch flow.
   */
  private noteChannelVersion(channelId: string, keyVersion: number): void {
    const prev = this.highestSeenVersion.get(channelId) ?? -1;
    if (keyVersion <= prev) {
      return;
    }
    this.highestSeenVersion.set(channelId, keyVersion);
    if (prev < 0) {
      return; // first-ever observation — seed baseline only, do not fire
    }
    for (const listener of this.keyRotationListeners) {
      try {
        listener({ channelId, keyVersion });
      } catch {
        /* listener errors never break the key flow (#1878) */
      }
    }
  }
}

// Singleton instance
export const e2eeService = new E2EEService();
