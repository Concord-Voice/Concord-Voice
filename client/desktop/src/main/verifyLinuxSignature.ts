import { createPublicKey, verify as cryptoVerify } from 'node:crypto';
import { readFile } from 'node:fs/promises';
import { LINUX_UPDATE_PUBLIC_KEY_PEM } from './linuxUpdatePublicKey';

/** Ed25519 / RFC 8032 detached signatures are exactly 64 bytes. */
const ED25519_SIGNATURE_BYTES = 64;

/**
 * Minimal structural shape of the Electron `net.fetch` response this module
 * consumes. Kept structural (no `electron` import) so the module is pure and
 * unit-testable; the production caller passes the real `net.fetch`.
 */
export interface SigFetchResponse {
  readonly ok: boolean;
  readonly status: number;
  arrayBuffer(): Promise<ArrayBuffer>;
}
export type SigFetch = (url: string, options?: { cache?: 'no-store' }) => Promise<SigFetchResponse>;

/**
 * Result of a Linux update-artifact signature check.
 *
 * `verified: false` ALWAYS refuses the install (fail-closed). `kind`
 * distinguishes the failure for USER MESSAGING ONLY — it never affects the
 * install decision (the install boundary is identical for both kinds):
 *   - `tampered`    — the bytes did NOT match a valid signature
 *                     (`crypto.verify -> false`). The one outcome that is genuine
 *                     evidence of a forged/altered artifact → tamper warning.
 *   - `unavailable` — the signature could not be checked at all (non-2xx / IO /
 *                     network error, missing or malformed `.sig`, no feed
 *                     configured). A transient/availability condition → a
 *                     retryable "couldn't verify right now" message, NOT a
 *                     tamper warning. An attacker who strips/blocks the `.sig`
 *                     lands here and STILL cannot install (refused either way),
 *                     so distinguishing the message does not weaken the gate.
 */
export type LinuxVerifyResult =
  | { verified: true }
  | { verified: false; reason: string; kind: 'tampered' | 'unavailable' };

/**
 * Verify a downloaded Linux update artifact against its detached Ed25519
 * signature, anchored to the BUNDLED public key (the sole trust anchor — the
 * guarantee holds even with no TLS pinning).
 *
 * FAIL-CLOSED: the only `verified: true` path is `crypto.verify -> true`. Every
 * other outcome returns `verified: false` so the caller refuses the install;
 * `kind` is `'tampered'` ONLY for a cryptographic verify-false, `'unavailable'`
 * for all fetch / IO / format failures (see LinuxVerifyResult).
 *
 * The production caller MUST pass Electron `net.fetch` bound to the DEFAULT
 * session, where the `api.concordvoice.chat` TLS pin lives (see `main.ts`'s
 * `setCertificateVerifyProc`). A custom session or a raw HTTP client would
 * silently bypass the pin. The `fetchFn` seam exists for tests.
 *
 * PII-safe: callers log only the artifact basename + outcome — never key
 * material, never the signature bytes.
 */
export async function verifyLinuxArtifact(
  filePath: string,
  sigUrl: string,
  fetchFn: SigFetch
): Promise<LinuxVerifyResult> {
  let sigBytes: Buffer;
  try {
    const res = await fetchFn(sigUrl, { cache: 'no-store' });
    if (!res.ok) {
      return {
        verified: false,
        reason: `signature fetch failed: HTTP ${res.status}`,
        kind: 'unavailable',
      };
    }
    sigBytes = Buffer.from(await res.arrayBuffer());
  } catch (err) {
    return {
      verified: false,
      reason: `signature fetch error: ${(err as Error).message}`,
      kind: 'unavailable',
    };
  }

  if (sigBytes.length !== ED25519_SIGNATURE_BYTES) {
    return {
      verified: false,
      reason: `signature wrong length: ${sigBytes.length} (expected ${ED25519_SIGNATURE_BYTES})`,
      kind: 'unavailable',
    };
  }

  let fileBytes: Buffer;
  try {
    fileBytes = await readFile(filePath);
  } catch (err) {
    return {
      verified: false,
      reason: `artifact read error: ${(err as Error).message}`,
      kind: 'unavailable',
    };
  }

  try {
    const key = createPublicKey(LINUX_UPDATE_PUBLIC_KEY_PEM);
    if (cryptoVerify(null, fileBytes, key, sigBytes)) {
      return { verified: true };
    }
    // The signature is well-formed but does NOT match the bytes — the only
    // outcome that is genuine evidence of tampering.
    return { verified: false, reason: 'signature does not verify', kind: 'tampered' };
  } catch (err) {
    return {
      verified: false,
      reason: `signature verification error: ${(err as Error).message}`,
      kind: 'unavailable',
    };
  }
}
