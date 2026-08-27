/**
 * SPA LKG cache — manifest verification (the trust root, #1870).
 *
 * Pure (no I/O): given the fetched manifest bytes, the detached base64
 * signature, the verification public key, the shell's IPC contract version, and
 * the current time, decide whether the manifest may be trusted. The caller
 * (populateCache / resolveCachedSpa) is responsible for the actual fetch and
 * for downloading + hash-verifying the asset bytes the manifest enumerates.
 *
 * Security posture — FAIL CLOSED at every step:
 *   - no non-placeholder key in the trust list → reject (cache dormant)
 *   - manifest larger than the byte cap        → reject (DoS guard)
 *   - signature does not verify over RAW bytes → reject
 *   - JSON parse / zod schema failure          → reject
 *   - schemaVersion mismatch                   → reject
 *   - spaIpcContract > shell contract          → reject (needs a binary update)
 *   - generatedAt older than the staleness cap → reject (do not run ancient bytes)
 *   - totalSize != sum(entry,assets) or > cap  → reject
 *
 * The signature is verified over the EXACT bytes passed in (never a
 * re-serialization), so there is no canonicalization to drift between the
 * Node/CI signer and this verifier. See manifestSchema.ts.
 *
 * The caller supplies a LIST of trusted public keys (#2958): during a signing-key
 * rotation the client must accept both the outgoing and the incoming signer, or
 * the switchover strands every binary already in the field. A longer list only
 * ever adds an accepted signer — it can never weaken or bypass the signature
 * check itself, and an empty/all-placeholder list still means "dormant".
 */

import { constants as cryptoConstants, createPublicKey, verify as cryptoVerify } from 'node:crypto';
import {
  SPA_MANIFEST_MAX_BYTES,
  SPA_MANIFEST_SCHEMA_VERSION,
  SPA_MANIFEST_SIGN_ALGORITHM,
  SPA_MANIFEST_SIGN_SALT_LENGTH,
  SPA_CACHE_MAX_STALENESS_MS,
  SpaManifestSchema,
  type SpaManifest,
} from './manifestSchema';

export type ManifestVerifyResult =
  { ok: true; manifest: SpaManifest } | { ok: false; reason: string };

/**
 * Try the detached signature against each trusted key until one verifies.
 *
 * Extracted from `verifyManifest` so that function stays under the cognitive
 * complexity ceiling — the multi-key scan nests two try/catch blocks and a
 * conditional break, which pushed the caller to 20 against a limit of 15.
 *
 * FAIL-CLOSED RULES, all three load-bearing:
 *
 *   1. Each key gets its OWN try/catch, so one malformed PEM cannot abort the
 *      scan and strand a valid key later in the list.
 *   2. `verified` is only ever ASSIGNED `true`, and only on the branch where
 *      cryptoVerify() itself returned true. There is deliberately no assignment
 *      path by which a thrown exception, an unusable key, or an exhausted list
 *      can leave it set.
 *   3. A list whose every entry fails to PARSE is reported as a key error
 *      rather than a signature mismatch, so a bad committed PEM is diagnosable.
 *      A key that parsed but did not verify is an honest "does not verify".
 *
 * `trustedKeys` must already be blank-filtered and non-empty; the caller owns
 * the dormant-cache case, which is a different answer entirely.
 */
function verifyDetachedSignature(params: {
  manifestBytes: Buffer;
  signature: Buffer;
  trustedKeys: readonly string[];
}): { verified: true } | { verified: false; reason: string } {
  const { manifestBytes, signature, trustedKeys } = params;

  let anyKeyUsable = false;
  let lastKeyError: string | undefined;

  for (const pem of trustedKeys) {
    let key;
    try {
      key = createPublicKey(pem);
    } catch (err) {
      // Malformed PEM in the trust list — skip it, try the next key.
      lastKeyError = (err as Error).message;
      continue;
    }
    anyKeyUsable = true;
    try {
      const ok = cryptoVerify(
        SPA_MANIFEST_SIGN_ALGORITHM,
        manifestBytes,
        {
          key,
          padding: cryptoConstants.RSA_PKCS1_PSS_PADDING,
          saltLength: SPA_MANIFEST_SIGN_SALT_LENGTH,
        },
        signature
      );
      if (ok) {
        return { verified: true };
      }
    } catch {
      // Wrong key type for the algorithm, a malformed signature — this key does
      // not verify these bytes. Try the next one; never trust.
      //
      // Deliberately does NOT record `lastKeyError`: reaching here means the key
      // PARSED, so `anyKeyUsable` is already true and that field can never be
      // read afterward. A dead store would read like an error path that reports
      // something, and the honest answer for this case IS
      // "signature does not verify".
    }
  }

  if (!anyKeyUsable) {
    return {
      verified: false,
      reason: `signature verification error: ${lastKeyError ?? 'no usable verification key'}`,
    };
  }
  return { verified: false, reason: 'signature does not verify' };
}

/**
 * Verify a fetched SPA manifest. `nowMs` is injected (not read from the clock)
 * so callers/tests are deterministic.
 */
export function verifyManifest(params: {
  manifestBytes: Buffer;
  signatureBase64: string;
  publicKeyPems: readonly string[];
  shellIpcContract: number;
  nowMs: number;
}): ManifestVerifyResult {
  const { manifestBytes, signatureBase64, publicKeyPems, shellIpcContract, nowMs } = params;

  // 1. Fail closed if no real key is configured (pre-activation placeholder).
  //    Blanks are filtered BEFORE the emptiness test: a list of only
  //    placeholders is a DORMANT cache, not a configured key that happens to
  //    reject everything. Those two states fail closed alike but read very
  //    differently in a log, and conflating them is how a dormancy regression
  //    hides as an ordinary rejection.
  const trustedKeys = publicKeyPems.filter((pem) => pem && pem.trim().length > 0);
  if (trustedKeys.length === 0) {
    return { ok: false, reason: 'no verification key configured (cache disabled)' };
  }

  // 2. DoS guard: bound the manifest size before any parsing/crypto work.
  if (manifestBytes.length > SPA_MANIFEST_MAX_BYTES) {
    return { ok: false, reason: `manifest exceeds ${SPA_MANIFEST_MAX_BYTES} bytes` };
  }

  // 3. Verify the detached signature over the RAW manifest bytes against the
  //    trust list (see verifyDetachedSignature below for the fail-closed rules).
  const signature = Buffer.from(signatureBase64, 'base64');
  if (signature.length === 0) {
    return { ok: false, reason: 'empty signature' };
  }
  const check = verifyDetachedSignature({ manifestBytes, signature, trustedKeys });
  if (!check.verified) {
    return { ok: false, reason: check.reason };
  }

  // 4. Only AFTER the signature verifies do we parse the (now-trusted-bytes) JSON.
  let parsed: unknown;
  try {
    parsed = JSON.parse(manifestBytes.toString('utf8'));
  } catch {
    return { ok: false, reason: 'manifest is not valid JSON' };
  }

  const result = SpaManifestSchema.safeParse(parsed);
  if (!result.success) {
    return {
      ok: false,
      reason: `manifest schema invalid: ${result.error.issues[0]?.message ?? 'unknown'}`,
    };
  }
  const manifest = result.data;

  // 5. Schema version must match exactly (defense in depth; zod literal already enforces).
  if (manifest.schemaVersion !== SPA_MANIFEST_SCHEMA_VERSION) {
    return { ok: false, reason: `unsupported schemaVersion ${manifest.schemaVersion}` };
  }

  // 6. IPC contract: the cache must NOT bypass a required binary update.
  if (manifest.spaIpcContract > shellIpcContract) {
    return {
      ok: false,
      reason: `manifest IPC contract ${manifest.spaIpcContract} > shell ${shellIpcContract} (binary update required)`,
    };
  }

  // 7. Bounded staleness: do not run ancient cached bytes.
  const generatedMs = Date.parse(manifest.generatedAt);
  if (Number.isNaN(generatedMs)) {
    return { ok: false, reason: 'generatedAt is not parseable' };
  }
  if (generatedMs > nowMs + 24 * 60 * 60 * 1000) {
    // More than a day in the future — clock skew or forged timestamp.
    return { ok: false, reason: 'generatedAt is implausibly in the future' };
  }
  if (nowMs - generatedMs > SPA_CACHE_MAX_STALENESS_MS) {
    return { ok: false, reason: 'manifest is stale beyond the freshness window' };
  }

  // 8. totalSize must equal the actual sum and stay within the cap (zod bounded the cap).
  const computedTotal = manifest.entry.size + manifest.assets.reduce((acc, a) => acc + a.size, 0);
  if (computedTotal !== manifest.totalSize) {
    return { ok: false, reason: 'totalSize does not match entry+assets sum' };
  }

  return { ok: true, manifest };
}
