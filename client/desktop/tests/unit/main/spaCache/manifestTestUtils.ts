/**
 * Test-only helpers for the SPA LKG cache suites (#1870).
 *
 * Generates an EPHEMERAL RSA-4096 keypair per call and signs fixture manifests
 * with RSA-PSS / SHA-256 / saltLength 32 — matching the production verifier. No
 * private key is ever committed; each test run mints a fresh pair in-memory.
 */

import {
  constants as cryptoConstants,
  createHash,
  generateKeyPairSync,
  sign as cryptoSign,
} from 'node:crypto';
import {
  SPA_MANIFEST_SCHEMA_VERSION,
  SPA_MANIFEST_SIGN_ALGORITHM,
  SPA_MANIFEST_SIGN_SALT_LENGTH,
} from '@/main/spaCache/manifestSchema';

export interface EphemeralKeypair {
  privateKeyPem: string;
  publicKeyPem: string;
}

/** Mint a fresh RSA-4096 keypair (PEM SPKI public / PKCS8 private). */
export function makeKeypair(): EphemeralKeypair {
  const { privateKey, publicKey } = generateKeyPairSync('rsa', { modulusLength: 4096 });
  return {
    privateKeyPem: privateKey.export({ type: 'pkcs8', format: 'pem' }).toString(),
    publicKeyPem: publicKey.export({ type: 'spki', format: 'pem' }).toString(),
  };
}

/** RSA-PSS sign raw bytes with the test private key; returns base64. */
export function signBytes(bytes: Buffer, privateKeyPem: string): string {
  const sig = cryptoSign(SPA_MANIFEST_SIGN_ALGORITHM, bytes, {
    key: privateKeyPem,
    padding: cryptoConstants.RSA_PKCS1_PSS_PADDING,
    saltLength: SPA_MANIFEST_SIGN_SALT_LENGTH,
  });
  return sig.toString('base64');
}

export function sha256Hex(bytes: Buffer): string {
  return createHash('sha256').update(bytes).digest('hex');
}

export interface FileFixture {
  path: string;
  bytes: Buffer;
}

export interface ManifestFixtureOptions {
  entry: FileFixture;
  assets?: FileFixture[];
  generatedAt?: string;
  spaIpcContract?: number;
  buildId?: string;
  /** Override totalSize to a wrong value (to exercise the sum mismatch path). */
  totalSizeOverride?: number;
}

export interface BuiltManifest {
  manifestObject: Record<string, unknown>;
  manifestBytes: Buffer;
  signatureBase64: string;
}

/**
 * Build a manifest object + its RAW JSON bytes + a valid detached signature over
 * those bytes. The signature is over the verbatim bytes returned, so callers can
 * tamper the bytes afterward to exercise the signature-rejection path.
 */
export function buildSignedManifest(
  keypair: EphemeralKeypair,
  opts: ManifestFixtureOptions
): BuiltManifest {
  const assets = opts.assets ?? [];
  const entrySize = opts.entry.bytes.length;
  const assetsTotal = assets.reduce((acc, a) => acc + a.bytes.length, 0);
  const totalSize = opts.totalSizeOverride ?? entrySize + assetsTotal;

  const manifestObject: Record<string, unknown> = {
    schemaVersion: SPA_MANIFEST_SCHEMA_VERSION,
    buildId: opts.buildId ?? 'test-build-1',
    spaIpcContract: opts.spaIpcContract ?? 1,
    generatedAt: opts.generatedAt ?? new Date().toISOString(),
    entry: {
      path: opts.entry.path,
      sha256: sha256Hex(opts.entry.bytes),
      size: entrySize,
    },
    assets: assets.map((a) => ({
      path: a.path,
      sha256: sha256Hex(a.bytes),
      size: a.bytes.length,
    })),
    totalSize,
  };

  const manifestBytes = Buffer.from(JSON.stringify(manifestObject), 'utf8');
  const signatureBase64 = signBytes(manifestBytes, keypair.privateKeyPem);
  return { manifestObject, manifestBytes, signatureBase64 };
}
