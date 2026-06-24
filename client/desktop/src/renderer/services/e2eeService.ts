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
import { E2EEKeyUnavailableError, type E2EEKeyErrorCode } from './e2eeErrors';
import { useE2EEStore } from '../stores/e2eeStore';

// Session lifetime — keys cached until explicit invalidation (rotation) or logout/close.
// Lazy fetch + hold: each channel's key is fetched on first visit and held for the session.
const CHANNEL_KEY_CACHE_TTL = Number.MAX_SAFE_INTEGER;

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

/** Exported key material for safeStorage persistence */
export interface E2EESessionKeys {
  wrappingKeyBase64: string;
  preferencesKeyBase64: string;
  wrappedPrivateKeyBase64: string;
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
  private readonly pendingKeyFetches: Map<string, Promise<CryptoKey>> = new Map();
  private rateLimitedUntil: number = 0; // timestamp when rate limit expires

  /** Cached exported keys for safeStorage persistence (set during initialize) */
  private sessionKeys: E2EESessionKeys | null = null;

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
    _keyDerivationAlg: KeyDerivationAlgorithm = 'argon2id'
  ): Promise<void> {
    const saltBytes = new Uint8Array(base64ToArrayBuffer(saltBase64));
    await this.initializeWithArgon2id(password, wrappedPrivateKeyBase64, saltBytes);

    // No periodic cleanup needed — session-scoped cache (keys stay until rotation or logout)
  }

  /**
   * Standard Argon2id initialization path.
   */
  private async initializeWithArgon2id(
    password: string,
    wrappedPrivateKeyBase64: string,
    salt: Uint8Array
  ): Promise<void> {
    const exportableWrapping = await deriveKeyArgon2idExportable(password, salt);
    const exportablePrefs = await derivePreferencesKeyArgon2idExportable(password, salt);

    await this.finalizeKeys(exportableWrapping, exportablePrefs, wrappedPrivateKeyBase64);
  }

  /**
   * Shared finalization: export raw bytes for safeStorage, re-import as non-extractable.
   */
  private async finalizeKeys(
    exportableWrapping: CryptoKey,
    exportablePrefs: CryptoKey,
    wrappedPrivateKeyBase64: string
  ): Promise<void> {
    const wrappingRaw = await crypto.subtle.exportKey('raw', exportableWrapping);
    const prefsRaw = await crypto.subtle.exportKey('raw', exportablePrefs);

    // Re-import as non-extractable for runtime use (XSS protection)
    this.wrappingKey = await crypto.subtle.importKey(
      'raw',
      wrappingRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['wrapKey', 'unwrapKey']
    );
    this.preferencesKey = await crypto.subtle.importKey(
      'raw',
      prefsRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt', 'decrypt']
    );

    this.wrappedPrivateKey = wrappedPrivateKeyBase64;

    // Cache exported keys for safeStorage (base64 for JSON serialization)
    this.sessionKeys = {
      wrappingKeyBase64: arrayBufferToBase64(wrappingRaw),
      preferencesKeyBase64: arrayBufferToBase64(prefsRaw),
      wrappedPrivateKeyBase64,
    };

    // Mark the renderer-side E2EE store ready so the post-auth gate
    // (#270 Task 21b) can transition past SSOEagerUnlock. Source of truth
    // remains this service — the store is a downstream subscription point.
    useE2EEStore.getState().setReady(true);
  }

  /**
   * Initialize from stored session keys (restored from safeStorage).
   * Imports raw key bytes as non-extractable CryptoKey objects.
   */
  async initializeFromStoredKeys(keys: E2EESessionKeys): Promise<void> {
    const wrappingRaw = base64ToArrayBuffer(keys.wrappingKeyBase64);
    const prefsRaw = base64ToArrayBuffer(keys.preferencesKeyBase64);

    this.wrappingKey = await crypto.subtle.importKey(
      'raw',
      wrappingRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['wrapKey', 'unwrapKey']
    );
    this.preferencesKey = await crypto.subtle.importKey(
      'raw',
      prefsRaw,
      { name: 'AES-GCM', length: 256 },
      false,
      ['encrypt', 'decrypt']
    );

    this.wrappedPrivateKey = keys.wrappedPrivateKeyBase64;
    this.sessionKeys = keys;

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
   * When `contextId` is supplied and the server response is NO_KEY_YET + pending:true,
   * fire-and-forget a peer-rewrap enrollment trigger (#1023). The server's
   * GetUnifiedKeys handler already auto-enrolls on 404; this explicit POST is
   * defense-in-depth and is idempotent on the server (ON CONFLICT DO NOTHING).
   */
  private async throwKeyFetchError(res: Response, contextId?: string): Promise<never> {
    if (res.status === 429) {
      const retryAfter = Number.parseInt(res.headers.get('Retry-After') || '30', 10);
      this.rateLimitedUntil = Date.now() + retryAfter * 1000;
    }
    const body = await safeJson<ErrorResponseShape>(res).catch((): ErrorResponseShape => ({}));
    const code = body.code ?? 'NO_KEY_YET';
    const pending = body.pending ?? false;

    // Fire-and-forget rewrap enrollment for the pending:true missing-key case (#1023).
    if (contextId && code === 'NO_KEY_YET' && pending) {
      this.requestRewrap(contextId).catch(() => {
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

  /**
   * Get the unwrapped channel key for a channel (JIT).
   * Fetches the wrapped key from cache or server, unwraps with private key.
   * Uses pendingKeyFetches to prevent cache stampeding (concurrent fetches for the same channel).
   */
  async getChannelKey(channelId: string): Promise<CryptoKey> {
    if (!channelId) {
      throw new E2EEKeyUnavailableError('INVALID_REQUEST', false);
    }

    // Check cache for wrapped key. Guard against refetch-marker slots whose
    // `wrappedKey` is empty (see fetchAndUnwrapChannelKey cache-poison path).
    const cached = this.channelKeyCache.get(channelId);
    if (cached?.wrappedKey && Date.now() - cached.lastUsed < CHANNEL_KEY_CACHE_TTL) {
      cached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      return unwrapChannelKey(cached.wrappedKey, privateKey);
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

    const fetchPromise = this.fetchAndUnwrapChannelKey(channelId);
    this.pendingKeyFetches.set(channelId, fetchPromise);

    try {
      return await fetchPromise;
    } finally {
      this.pendingKeyFetches.delete(channelId);
    }
  }

  /**
   * Internal: fetch wrapped key from server, cache it, and unwrap.
   */
  private async fetchAndUnwrapChannelKey(channelId: string): Promise<CryptoKey> {
    const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}`);
    if (!res.ok) {
      await this.throwKeyFetchError(res, channelId);
    }
    const data = await safeJson<KeyResponseShape>(res);
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
        return this.fetchAndUnwrapChannelKey(channelId);
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

    // JIT unwrap: derive private key, unwrap channel key, private key falls out of scope
    const privateKey = await this.derivePrivateKey();
    return unwrapChannelKey(wrappedKey, privateKey);
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
      const retryAfter = Number.parseInt(res.headers.get('Retry-After') ?? '60', 10);
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
    const channelKey = await this.getChannelKey(channelId);
    return encryptMessage(plaintext, channelKey);
  }

  /**
   * Decrypt a message from a channel.
   * JIT: gets channel key, decrypts, key falls out of scope.
   */
  async decryptForChannel(channelId: string, ciphertext: string): Promise<string> {
    const channelKey = await this.getChannelKey(channelId);
    return decryptMessage(ciphertext, channelKey);
  }

  /**
   * Decrypt a message using a pre-fetched channel key.
   * Avoids redundant getChannelKey() calls when batch-decrypting.
   */
  async decryptWithKey(ciphertext: string, channelKey: CryptoKey): Promise<string> {
    return decryptMessage(ciphertext, channelKey);
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
    // Get the unwrapped channel key
    const channelKey = await this.getChannelKey(channelId);
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
    return wrapChannelKey(tempKey, memberPublicKey);
  }

  /**
   * Process pending key requests — auto-wrap keys for new members.
   */
  async processPendingKeyRequests(): Promise<void> {
    if (!this.isInitialized) return;

    try {
      const res = await apiFetch('/api/v1/e2ee/pending-keys');
      if (!res.ok) {
        console.debug('[E2EE] pending-keys request failed:', res.status, res.statusText);
        return;
      }

      const data = await safeJson<{
        pending_requests?: Array<{ user_id: string; channel_id: string }>;
      }>(res);
      const requests = data.pending_requests || [];
      console.debug('[E2EE] Pending key requests:', requests.length);

      for (const req of requests) {
        try {
          // Fetch new member's public key
          const pkRes = await apiFetch(`/api/v1/users/${req.user_id}/public-key`);
          if (!pkRes.ok) {
            console.warn('[E2EE] Failed to fetch public key for pending key distribution', {
              userId: req.user_id,
              status: pkRes.status,
            });
            continue;
          }

          const pkData = await safeJson<{ public_key: string }>(pkRes);
          const wrappedKey = await this.wrapKeyForMember(req.channel_id, pkData.public_key);

          // Upload the wrapped key
          const uploadRes = await apiFetch(`/api/v1/e2ee/keys/${req.channel_id}`, {
            method: 'POST',
            headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({
              wrapped_keys: { [req.user_id]: wrappedKey },
            }),
          });
          console.debug(
            '[E2EE] Key distributed for',
            req.user_id,
            'channel',
            req.channel_id,
            uploadRes.status
          );
        } catch (err) {
          console.warn('[E2EE] Failed to process pending key request', {
            channelId: req.channel_id,
            userId: req.user_id,
            error: (err as Error).message,
          });
        }
      }
    } catch (err) {
      console.warn('[E2EE] processPendingKeyRequests fatal', {
        error: (err as Error).message,
      });
    }
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
   * Fetch and unwrap a specific key version for a channel (for decrypting old messages).
   * Results are cached in the versioned key cache.
   */
  async getChannelKeyByVersion(channelId: string, version: number): Promise<CryptoKey> {
    if (!channelId) {
      throw new E2EEKeyUnavailableError('INVALID_REQUEST', false);
    }

    // Check if this is the current version — use main cache.
    // Guard against empty wrappedKey (cache-poison marker slot).
    const mainCached = this.channelKeyCache.get(channelId);
    if (mainCached?.wrappedKey && mainCached.keyVersion === version) {
      mainCached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      return unwrapChannelKey(mainCached.wrappedKey, privateKey);
    }

    // Check versioned cache
    const versionCached = this.versionedKeyCache.get(channelId)?.get(version);
    if (versionCached?.wrappedKey && Date.now() - versionCached.lastUsed < CHANNEL_KEY_CACHE_TTL) {
      versionCached.lastUsed = Date.now();
      const privateKey = await this.derivePrivateKey();
      return unwrapChannelKey(versionCached.wrappedKey, privateKey);
    }

    // If we're rate-limited, don't fire another request
    if (Date.now() < this.rateLimitedUntil) {
      throw new E2EEKeyUnavailableError('NO_KEY_YET', false);
    }

    // Fetch specific version from server
    const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}?version=${version}`);
    if (!res.ok) {
      await this.throwKeyFetchError(res, channelId);
    }
    const data = await safeJson<KeyResponseShape>(res);
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
        return this.getChannelKeyByVersion(channelId, version);
      }
      throw err;
    }

    // Cache in versioned cache (refetchAfterMalformed: 0 — validation passed)
    let versionMapForChannel = this.versionedKeyCache.get(channelId);
    if (!versionMapForChannel) {
      versionMapForChannel = new Map();
      this.versionedKeyCache.set(channelId, versionMapForChannel);
    }
    versionMapForChannel.set(version, {
      wrappedKey,
      keyVersion: version,
      lastUsed: Date.now(),
      refetchAfterMalformed: 0,
    });

    // Clear any stale marker slot left on the main cache by a prior refetch
    // cycle — a successful versioned fetch means the refetch counter is resolved.
    const staleMarker = this.channelKeyCache.get(channelId);
    if (staleMarker && !staleMarker.wrappedKey) {
      this.channelKeyCache.delete(channelId);
    }

    const privateKey = await this.derivePrivateKey();
    return unwrapChannelKey(wrappedKey, privateKey);
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
    const channelKey = await this.getChannelKeyByVersion(channelId, version);
    return decryptMessage(ciphertext, channelKey);
  }

  /**
   * Rotate the channel key: generate a new CSK, wrap for all members, and distribute.
   * Used after member removal or theft detection.
   *
   * @param channelId - The channel to rotate keys for
   * @param newKeyVersion - The new epoch/version number
   * @param memberPublicKeys - Map of user_id → base64 public key for remaining members
   */
  async rotateChannelKey(
    channelId: string,
    newKeyVersion: number,
    memberPublicKeys: Map<string, string>
  ): Promise<void> {
    const channelKey = await generateChannelKey();
    const wrappedKeys: Record<string, string> = {};

    for (const [userId, publicKeyBase64] of memberPublicKeys) {
      const publicKey = await importPublicKey(publicKeyBase64);
      const wrapped = await wrapChannelKey(channelKey, publicKey);
      wrappedKeys[userId] = wrapped;
    }

    const res = await apiFetch(`/api/v1/e2ee/keys/${channelId}`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        wrapped_keys: wrappedKeys,
        key_version: newKeyVersion,
      }),
    });

    if (!res.ok) {
      const data = await safeJson(res).catch(() => ({}));
      console.debug('[E2EE] Key rotation distribution failed:', res.status, data);
      // Non-fatal: another client may have already distributed (first-response-wins)
    }

    // Invalidate current cache so next encrypt/decrypt fetches the new key
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
   * Clear all keys on logout.
   */
  clearKeys(): void {
    this.wrappingKey = null;
    this.preferencesKey = null;
    this.wrappedPrivateKey = '';
    this.sessionKeys = null;
    this.channelKeyCache.clear();
    this.versionedKeyCache.clear();
    this.pendingKeyFetches.clear();

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
    this.channelKeyCache.delete(channelId);
    this.versionedKeyCache.delete(channelId);
  }
}

// Singleton instance
export const e2eeService = new E2EEService();
