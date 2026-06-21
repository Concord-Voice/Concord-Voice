// @vitest-environment node
//
// Integration test for update-feed cert pinning (#658).
//
// Satisfies the issue's acceptance criterion:
//   "Update feed connection fails when MITM'd with a non-pinned-but-valid CA
//    cert (integration test)"
//
// Unlike the unit tests which use pre-committed PEM fixtures, this test
// generates a fresh self-signed cert at runtime via `selfsigned` (devDep)
// and constructs a synthetic VerifyProcRequest. The full PEM-parsing →
// SPKI-extraction → pin-comparison chain is exercised against a real
// (rogue) certificate; only the Electron session harness is synthesized.
//
// An end-to-end test that spins up Electron + a live rogue-TLS server is
// deferred to manual verification (spec §8.9).
import { describe, it, expect, vi, beforeAll } from 'vitest';
import selfsigned from 'selfsigned';
import {
  computeSpkiSha256,
  createPinningVerifyProc,
  type PinConfig,
  type VerifyProcRequest,
} from '../../src/main/updatePinning';

const PINNED_HOST = 'api.example.com';

describe('updatePinning — freshly generated rogue cert rejection', () => {
  let roguePem: string;
  let rogueSpki: string;
  let goodPem: string;
  let goodSpki: string;

  beforeAll(async () => {
    // Rogue cert impersonating the pinned hostname; "known-good" cert to pin
    // as the legitimate primary. Both keygens run in parallel via Promise.all
    // over selfsigned 5.x's async generate() API.
    const [roguePems, goodPems] = await Promise.all([
      selfsigned.generate([{ name: 'commonName', value: PINNED_HOST }], {
        days: 1,
        keySize: 2048,
        algorithm: 'sha256',
      }),
      selfsigned.generate([{ name: 'commonName', value: 'legitimate-origin' }], {
        days: 1,
        keySize: 2048,
        algorithm: 'sha256',
      }),
    ]);
    roguePem = roguePems.cert;
    rogueSpki = computeSpkiSha256(roguePem);
    goodPem = goodPems.cert;
    goodSpki = computeSpkiSha256(goodPem);
  });

  it('rogue SPKI differs from the pinned SPKI (sanity check)', () => {
    expect(rogueSpki).not.toBe(goodSpki);
  });

  it('rejects a self-signed cert impersonating the feed domain with callback(-2)', () => {
    const logger = { warn: vi.fn(), error: vi.fn() };
    const config: PinConfig = {
      pinnedHosts: [PINNED_HOST],
      primaryPins: [goodSpki],
      fallbackPins: [goodSpki],
    };
    const proc = createPinningVerifyProc(config, logger);
    const callback = vi.fn();

    // Simulate Chromium having accepted the rogue cert's chain (the threat:
    // attacker obtained a valid cert from a different CA). Pinning must
    // reject regardless.
    const request: VerifyProcRequest = {
      hostname: PINNED_HOST,
      certificate: { data: roguePem },
      errorCode: 0,
      verificationResult: 'net::OK',
    };

    proc(request, callback);

    expect(callback).toHaveBeenCalledWith(-2);
    expect(callback).toHaveBeenCalledTimes(1);
    expect(logger.error).toHaveBeenCalledWith(expect.stringMatching(/SECURITY.*pin miss/));
  });

  it('accepts the same rogue cert if its SPKI IS pinned (round-trip sanity)', () => {
    // Confirms the mechanism is bidirectional: pinning is deterministic,
    // not cert-content-specific beyond the SPKI.
    const logger = { warn: vi.fn(), error: vi.fn() };
    const config: PinConfig = {
      pinnedHosts: [PINNED_HOST],
      primaryPins: [rogueSpki],
      fallbackPins: [goodSpki],
    };
    const proc = createPinningVerifyProc(config, logger);
    const callback = vi.fn();

    proc(
      {
        hostname: PINNED_HOST,
        certificate: { data: roguePem },
        errorCode: 0,
        verificationResult: 'net::OK',
      },
      callback
    );

    expect(callback).toHaveBeenCalledWith(0);
  });

  it('rejects the rogue cert for an unrelated but pinned hostname', () => {
    // Edge case: hostname is pinned but the cert presented is not the one we pinned.
    const logger = { warn: vi.fn(), error: vi.fn() };
    const config: PinConfig = {
      pinnedHosts: [PINNED_HOST, 'other.example.com'],
      primaryPins: [goodSpki],
      fallbackPins: [goodSpki],
    };
    const proc = createPinningVerifyProc(config, logger);
    const callback = vi.fn();

    proc(
      {
        hostname: 'other.example.com',
        certificate: { data: roguePem },
        errorCode: 0,
        verificationResult: 'net::OK',
      },
      callback
    );

    expect(callback).toHaveBeenCalledWith(-2);
  });
});
