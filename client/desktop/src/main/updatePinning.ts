import { X509Certificate, createHash } from 'node:crypto';

/**
 * Data-only configuration for the pinning verify proc. Lives in the separate
 * module `updatePinningConfig.ts` so rotation PRs produce surgical diffs.
 * See spec §14/§15 and the rotation runbook.
 */
export interface PinConfig {
  readonly pinnedHosts: readonly string[];
  readonly primaryPins: readonly string[];
  readonly fallbackPins: readonly string[];
}

/**
 * Structural type mirroring Electron's `Request` shape passed to a
 * `Session.setCertificateVerifyProc` callback. Declared locally so the pure
 * module stays Electron-free for unit testing.
 */
export interface VerifyProcRequest {
  readonly hostname: string;
  readonly certificate: { readonly data: string };
  readonly errorCode: number;
  readonly verificationResult: string;
}

type Logger = Pick<Console, 'warn' | 'error'>;
type VerifyProcCallback = (verificationResult: number) => void;
export type VerifyProc = (request: VerifyProcRequest, callback: VerifyProcCallback) => void;

function normalizeHex(s: string): string {
  return s.toLowerCase().replaceAll(/[^0-9a-f]/g, '');
}

/**
 * Compute the SPKI SHA-256 fingerprint (RFC 7469 spki-sha256 equivalent) of a
 * PEM-encoded X.509 certificate. Used to identify a cert's public key
 * independent of its subject/issuer/serial — so cert renewals that reuse the
 * same keypair (LE with `--reuse-key`) yield the same fingerprint.
 *
 * See spec §6 Invariant I6 (leaf cert only) and [internal]specs/2026-04-20-658-updater-feed-cert-pin-design.md.
 *
 * @param pemCertData PEM-encoded certificate (the value of Electron's
 *                    VerifyProcRequest.certificate.data)
 * @returns lowercase hex SHA-256 of the DER-encoded SubjectPublicKeyInfo (64 chars)
 */
export function computeSpkiSha256(pemCertData: string): string {
  const cert = new X509Certificate(pemCertData);
  const spkiDer = cert.publicKey.export({ type: 'spki', format: 'der' });
  return createHash('sha256').update(spkiDer).digest('hex');
}

/**
 * Case-insensitive exact match of `hostname` against a pinned-hosts list.
 * NO wildcard support — subdomains are not matched. Design decision per spec
 * §6 Invariants I5 and I7: explicit enumeration only, to avoid pinning
 * `staging.*` by accident when the production list has `api.*`.
 */
export function isHostnamePinned(hostname: string, pinnedHosts: readonly string[]): boolean {
  const h = hostname.toLowerCase();
  return pinnedHosts.some((p) => p.toLowerCase() === h);
}

/**
 * Create a `Session.setCertificateVerifyProc` handler that enforces SPKI
 * pinning on pinned SaaS hostnames. Non-pinned hosts pass through to
 * Chromium's default cert validation.
 *
 * Enforces the ten security invariants in spec §6:
 *
 *   I1  fail-closed on any exception                          → callback(-2)
 *   I2  never widen Chromium trust                            → defer on errorCode != 0
 *   I3  ≥ 1 primary and ≥ 1 fallback pin required at init     → throws
 *   I4  pin comparison is case-insensitive hex
 *   I5  hostname match is case-insensitive exact (no wildcards)
 *   I6  SPKI computed from the leaf cert only
 *   I7  non-pinned hosts fall through to Chromium default     → callback(-3)
 *   I8  fallback pin hits emit a warning log
 *   I9  log lines include hostname + SPKI last-8; never full SPKI or subject DN
 *   I10 callback invoked exactly once per code path (no await, no async)
 *
 * @throws Error if config.primaryPins or config.fallbackPins is empty (I3)
 */
export function createPinningVerifyProc(config: PinConfig, logger: Logger): VerifyProc {
  if (config.primaryPins.length === 0) {
    throw new Error('updatePinning: primary pin list is empty — refusing to start');
  }
  if (config.fallbackPins.length === 0) {
    throw new Error('updatePinning: fallback pin list is empty — refusing to start');
  }

  const primarySet = new Set(config.primaryPins.map(normalizeHex));
  const fallbackSet = new Set(config.fallbackPins.map(normalizeHex));

  return (request, callback) => {
    try {
      // I7 — non-pinned hosts defer to Chromium's default.
      if (!isHostnamePinned(request.hostname, config.pinnedHosts)) {
        return callback(-3);
      }

      // I2 — Chromium already rejected the chain (expired, revoked, untrusted
      // root). Defer so the client sees the real diagnostic error; pinning
      // never widens trust.
      if (request.errorCode !== 0) {
        return callback(-3);
      }

      // I6 — leaf cert only (request.certificate; NOT request.validatedCertificate).
      const spki = normalizeHex(computeSpkiSha256(request.certificate.data));
      const last8 = spki.slice(-8);

      if (primarySet.has(spki)) {
        return callback(0);
      }

      if (fallbackSet.has(spki)) {
        // I8 — rotation-window indicator; fleet ops should see this in logs.
        logger.warn(
          `Update feed pin: fallback pin matched for ${request.hostname} (SPKI:…${last8})`
        );
        return callback(0);
      }

      // I9 — hostname + last-8 only; never full SPKI, never subject fields.
      logger.error(`SECURITY: Update feed pin miss for ${request.hostname}; got SPKI:…${last8}`);
      return callback(-2);
    } catch (err) {
      // I1 — any failure → fail-closed reject. Never accept on exception.
      const host = request?.hostname ?? '<unknown>';
      const msg = err instanceof Error ? err.message : String(err);
      logger.error(`SECURITY: verifyProc error for ${host}: ${msg}`);
      return callback(-2);
    }
  };
}
