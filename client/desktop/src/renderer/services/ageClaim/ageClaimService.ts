// Age-claim orchestrator (#1624, child B of epic #272).
//
// submitSignedAgeClaim is the single entry point the DOB path (#1625) and the SSO
// path (#1626) call. It evaluates an age signal LOCALLY, assembles the canonical
// claim, signs it with the device key (RSA-PSS / SHA-256 / saltLength 32), and PUTs
// it to the #1623 server endpoint. The raw birthdate/age never leaves this module:
// only the two derived booleans (and fields derived from them) are assembled into
// the claim — nothing is written to any store, localStorage, safeStorage, or disk.
import { evaluateAge, type AgeSignal } from './evaluateAge';
import { buildCanonicalBytes, validateAgeClaim, type AgeClaim } from './canonicalAgeClaim';
import { e2eeService } from '../e2eeService';
import { apiFetch } from '../apiClient';
import { useUserStore } from '../../stores/userStore';

/**
 * Result of a claim submission. On failure, `code` is the server's `error_code`
 * (e.g. `account_disabled`, `stale_key_version`, `invalid_signature`,
 * `replayed_nonce`, `stale_timestamp`, `no_signing_key`, `malformed`) OR a
 * client-side precondition code (`unavailable` — not logged in, E2EE not ready,
 * key-version fetch failed, signing failed, or network error). `account_disabled`
 * is the terminal `valid_age=false` path the caller surfaces in the disable UX.
 *
 * Consumer retry note (#1625 DOB / #1626 SSO): an `invalid_signature` code most
 * often means the local device key no longer matches the server's current
 * `key_version` — e.g. a key rotation/recovery happened on another device and this
 * session still holds the old wrapped key (a state that has also broken E2EE and
 * will force a re-login). Callers should treat `invalid_signature` like
 * `stale_key_version`: re-initialize E2EE / re-fetch key material and re-sign ONCE,
 * not as a terminal failure. It is NOT terminal at the server either — disable
 * fires only on a *verified* `valid_age=false` claim, never on a signature reject.
 * This module deliberately does NOT collapse `invalid_signature` into
 * `stale_key_version` (that would mask genuine tampering/corruption — the server's
 * single non-oracle failure code); the distinction must reach the caller intact.
 */
export type AgeClaimResult =
  | { ok: true; validAge: boolean; nsfwAuth: boolean }
  | { ok: false; code: string };

export interface SubmitAgeClaimInput {
  /** Birthdate or coarse age-band. Consumed only by evaluateAge; never persisted. */
  signal: AgeSignal;
  /** Caller-supplied jurisdiction obligation (#1627 supplies a real value); default 0. */
  jurisdictionObligation?: 0 | 1 | 2;
}

const CLIENT_VERSION_CHARSET = /[^0-9A-Za-z.+-]/g;

/** Current authenticated user id (matches the JWT identity the server reconstructs). */
function getCurrentUserId(): string | null {
  return useUserStore.getState().user?.id ?? null;
}

/** App version string, clamped to the contract charset `[0-9A-Za-z.+-]`, <=32 chars. */
async function getClientVersion(): Promise<string> {
  let raw = '';
  try {
    raw = await window.electron.getVersion();
  } catch {
    raw = '';
  }
  const clean = raw.replace(CLIENT_VERSION_CHARSET, '').slice(0, 32);
  return clean.length > 0 ? clean : '0.0.0';
}

/**
 * Fetch the user's CURRENT device key_version from the server (latest public_keys
 * row — the same row the server's LoadCurrentKey checks against). Just-in-time so
 * the claim is signed under the version the verifier will require; a rotation in
 * the sub-second window yields a retryable `stale_key_version`. Throws on any
 * non-OK / unparseable response (caller maps to `unavailable`).
 */
async function fetchCurrentKeyVersion(userId: string): Promise<number> {
  const res = await apiFetch(`/api/v1/users/${userId}/public-key`);
  if (!res.ok) {
    throw new Error('age-claim: key-version fetch failed');
  }
  const data = (await res.json()) as { key_version?: number };
  if (typeof data.key_version !== 'number' || !Number.isInteger(data.key_version)) {
    throw new Error('age-claim: malformed key-version response');
  }
  return data.key_version;
}

/** Parse the server's `{ error_code }`; default `unavailable` if unparseable. */
async function extractErrorCode(res: Response): Promise<string> {
  try {
    const data = (await res.json()) as { error_code?: string };
    return typeof data.error_code === 'string' && data.error_code.length > 0
      ? data.error_code
      : 'unavailable';
  } catch {
    return 'unavailable';
  }
}

export async function submitSignedAgeClaim(input: SubmitAgeClaimInput): Promise<AgeClaimResult> {
  const userId = getCurrentUserId();
  if (!userId) {
    return { ok: false, code: 'unavailable' };
  }
  if (!e2eeService.isInitialized) {
    return { ok: false, code: 'unavailable' };
  }

  // Evaluate locally. `input.signal` (the raw DOB/age) is read here and nowhere else;
  // only validAge/nsfwAuth escape this function.
  const { validAge, nsfwAuth } = evaluateAge(input.signal, new Date());

  let keyVersion: number;
  try {
    keyVersion = await fetchCurrentKeyVersion(userId);
  } catch {
    return { ok: false, code: 'unavailable' };
  }

  const clientVersion = await getClientVersion();

  const nonceBytes = crypto.getRandomValues(new Uint8Array(32));
  const nonce = Array.from(nonceBytes, (b) => b.toString(16).padStart(2, '0')).join('');

  const claim: AgeClaim = {
    canonicalVersion: 1,
    userId: userId.toLowerCase(),
    validAge,
    nsfwAuth,
    jurisdictionObligation: input.jurisdictionObligation ?? 0,
    nonce,
    timestamp: Math.floor(Date.now() / 1000),
    keyVersion,
    clientVersion,
  };

  try {
    validateAgeClaim(claim);
  } catch {
    return { ok: false, code: 'malformed' };
  }

  const bytes = buildCanonicalBytes(claim);

  // Signing lives in e2eeService (a designated crypto module): derive the device
  // RSA-PSS handle + sign (saltLength 32) + base64, all without exposing crypto.subtle
  // or a CryptoKey here.
  let signatureB64: string;
  try {
    signatureB64 = await e2eeService.signAgeClaim(bytes);
  } catch {
    return { ok: false, code: 'unavailable' };
  }

  // Body carries the 8 signed-core fields MINUS user_id (server reconstructs it
  // from the JWT) PLUS the base64 signature.
  const body = {
    canonical_version: claim.canonicalVersion,
    valid_age: claim.validAge,
    nsfw_auth: claim.nsfwAuth,
    jurisdiction_obligation: claim.jurisdictionObligation,
    nonce: claim.nonce,
    timestamp: claim.timestamp,
    key_version: claim.keyVersion,
    client_version: claim.clientVersion,
    signature: signatureB64,
  };

  let res: Response;
  try {
    res = await apiFetch('/api/v1/age/claim', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    });
  } catch {
    return { ok: false, code: 'unavailable' };
  }

  if (res.ok) {
    // Return the verdict the client SIGNED + submitted so the caller renders exactly that,
    // never a second client recomputation that could disagree with the signed claim at a
    // birthday boundary across the round-trip (#1625). The server enforces the disable on
    // this signed valid_age; it does not echo a verdict back (the 200 body is unused).
    return { ok: true, validAge, nsfwAuth };
  }
  return { ok: false, code: await extractErrorCode(res) };
}
