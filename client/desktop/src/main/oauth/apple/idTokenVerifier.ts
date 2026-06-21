/**
 * Local Apple id_token verification (#974, spec step 5 / R3).
 *
 * jose.jwtVerify against Apple's JWKS via createRemoteJWKSet — built-in
 * cache, kid-miss refetch, and fetch cooldown. Verification pins:
 *   - algorithms: ['RS256']   (A04 algorithm-confusion allowlist)
 *   - issuer:   https://appleid.apple.com
 *   - audience: the Services ID parsed from the authorize URL
 * plus the claim checks jose cannot express:
 *   - nonce === the /initiate-issued nonce (CSRF/replay binding)
 *   - nonce_supported !== false (Apple string-bool quirk tolerated — a token
 *     that disavows nonce binding is rejected on its own terms)
 *   - sub present (the identity the server will classify on)
 *
 * The server re-verifies everything at /session (CWE-345 defense in depth);
 * this local gate exists so a tampered token never leaves the device.
 *
 * Error split (spec §Failure taxonomy):
 *   - JWKS transport/kid-miss → apple_verification_unavailable (retry once;
 *     jose's cooldown is respected because the JWKS set object is cached)
 *   - signature/claim failure → apple_id_token_invalid (never retried with
 *     the same token)
 */
import * as jose from 'jose';

import { AppleFlowError } from './errors';

export const APPLE_ISSUER = 'https://appleid.apple.com';
export const APPLE_JWKS_URL = 'https://appleid.apple.com/auth/keys';

export interface AppleIDTokenClaims {
  sub: string;
  email?: string;
  emailVerified?: boolean;
}

/**
 * Module-cached remote JWKS — one instance so jose's cooldown applies. The
 * flow's AbortSignal is read through a mutable ref at fetch time rather than
 * baked into the cached instance: the cache outlives any single flow, so a
 * superseded flow's aborted signal must never poison later verifications.
 * Supersession aborts the prior flow WITHOUT awaiting its teardown, so a
 * late unwind can overlap the next flow — writes to the ref are therefore
 * ownership-guarded (see the finally below).
 */
let remoteJwks: jose.JWTVerifyGetKey | null = null;
let currentFlowSignal: AbortSignal | undefined;
function defaultJwks(): jose.JWTVerifyGetKey {
  remoteJwks ??= jose.createRemoteJWKSet(new URL(APPLE_JWKS_URL), {
    [jose.customFetch]: (url, options) => {
      // Merge only defined operands — AbortSignal.any throws a TypeError on
      // non-signal entries, and jose's own options.signal (its timeout) is
      // jose's contract to supply, not ours to assume.
      const signals = [options.signal, currentFlowSignal].filter(
        (s): s is AbortSignal => s instanceof AbortSignal
      );
      return fetch(url, {
        ...options,
        signal: signals.length > 0 ? AbortSignal.any(signals) : undefined,
      });
    },
  });
  return remoteJwks;
}

export async function verifyAppleIDToken(opts: {
  idToken: string;
  clientId: string;
  expectedNonce: string;
  /**
   * Flow AbortSignal — cancels an in-flight JWKS fetch on sso:appleCancel /
   * window close / deadline, completing the single-AbortController teardown
   * contract (review finding on #1486).
   */
  signal?: AbortSignal;
  /** Test seam: a local JWKS resolver. Production callers omit. */
  jwks?: jose.JWTVerifyGetKey;
}): Promise<AppleIDTokenClaims> {
  let payload: jose.JWTPayload;
  currentFlowSignal = opts.signal;
  try {
    ({ payload } = await jose.jwtVerify(opts.idToken, opts.jwks ?? defaultJwks(), {
      algorithms: ['RS256'],
      issuer: APPLE_ISSUER,
      audience: opts.clientId,
    }));
  } catch (err) {
    // Cancellation first: an aborted JWKS fetch surfaces as AbortError (and
    // teardown races can reshape it), so the signal state is authoritative —
    // mirrors the sso_cancelled mapping in appleFlow's other network stages.
    if ((err as Error).name === 'AbortError' || opts.signal?.aborted) {
      throw new AppleFlowError('sso_cancelled', 'verify');
    }
    // Transport-class failures are retryable; everything else is terminal
    // for this token. createRemoteJWKSet surfaces network failure as the
    // underlying fetch TypeError.
    if (
      err instanceof jose.errors.JWKSNoMatchingKey ||
      err instanceof jose.errors.JWKSTimeout ||
      (err as Error).name === 'TypeError'
    ) {
      throw new AppleFlowError('apple_verification_unavailable', 'verify');
    }
    throw new AppleFlowError('apple_id_token_invalid', 'verify');
  } finally {
    // Only clear the ref this call owns — supersession aborts the prior
    // flow without awaiting teardown, so a superseded flow's late unwind
    // must not null out the newer flow's signal.
    if (currentFlowSignal === opts.signal) {
      currentFlowSignal = undefined;
    }
  }

  if (payload.nonce !== opts.expectedNonce) {
    throw new AppleFlowError('apple_id_token_invalid', 'verify');
  }
  if (payload.nonce_supported === false || payload.nonce_supported === 'false') {
    throw new AppleFlowError('apple_id_token_invalid', 'verify');
  }
  if (typeof payload.sub !== 'string' || payload.sub.length === 0) {
    throw new AppleFlowError('apple_id_token_invalid', 'verify');
  }

  return {
    sub: payload.sub,
    email: typeof payload.email === 'string' ? payload.email : undefined,
    emailVerified: payload.email_verified === true || payload.email_verified === 'true',
  };
}
