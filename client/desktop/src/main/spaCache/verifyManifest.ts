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
 *   - empty/placeholder public key            → reject (cache dormant)
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
  | { ok: true; manifest: SpaManifest }
  | { ok: false; reason: string };

/**
 * Verify a fetched SPA manifest. `nowMs` is injected (not read from the clock)
 * so callers/tests are deterministic.
 */
export function verifyManifest(params: {
  manifestBytes: Buffer;
  signatureBase64: string;
  publicKeyPem: string;
  shellIpcContract: number;
  nowMs: number;
}): ManifestVerifyResult {
  const { manifestBytes, signatureBase64, publicKeyPem, shellIpcContract, nowMs } = params;

  // 1. Fail closed if no real key is configured (pre-activation placeholder).
  if (!publicKeyPem || publicKeyPem.trim().length === 0) {
    return { ok: false, reason: 'no verification key configured (cache disabled)' };
  }

  // 2. DoS guard: bound the manifest size before any parsing/crypto work.
  if (manifestBytes.length > SPA_MANIFEST_MAX_BYTES) {
    return { ok: false, reason: `manifest exceeds ${SPA_MANIFEST_MAX_BYTES} bytes` };
  }

  // 3. Verify the detached signature over the RAW manifest bytes.
  let signatureValid: boolean;
  try {
    const signature = Buffer.from(signatureBase64, 'base64');
    if (signature.length === 0) {
      return { ok: false, reason: 'empty signature' };
    }
    const key = createPublicKey(publicKeyPem);
    signatureValid = cryptoVerify(
      SPA_MANIFEST_SIGN_ALGORITHM,
      manifestBytes,
      {
        key,
        padding: cryptoConstants.RSA_PKCS1_PSS_PADDING,
        saltLength: SPA_MANIFEST_SIGN_SALT_LENGTH,
      },
      signature
    );
  } catch (err) {
    // Malformed key, malformed signature base64, etc. — never trust.
    return { ok: false, reason: `signature verification error: ${(err as Error).message}` };
  }
  if (!signatureValid) {
    return { ok: false, reason: 'signature does not verify' };
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
