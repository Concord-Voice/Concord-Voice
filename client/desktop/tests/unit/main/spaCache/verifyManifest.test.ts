import { describe, it, expect } from 'vitest';
import { verifyManifest } from '@/main/spaCache/verifyManifest';
import { SPA_CACHE_MAX_STALENESS_MS, SPA_MANIFEST_MAX_BYTES } from '@/main/spaCache/manifestSchema';
// REAL public-key module (NO vi.mock) — this file never mocks it, so importing
// the genuine values here asserts the committed trust-root state.
import {
  SPA_MANIFEST_PUBLIC_KEY_PEM,
  isSpaManifestKeyConfigured,
} from '@/main/spaCache/spaManifestPublicKey';
import { buildSignedManifest, makeKeypair, signBytes, type FileFixture } from './manifestTestUtils';

const SHELL_IPC = 16;

function entryFixture(): FileFixture {
  return { path: 'index.html', bytes: Buffer.from('<!doctype html><html></html>') };
}

describe('verifyManifest (#1870 trust root)', () => {
  it('accepts a valid manifest + signature', () => {
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, { entry: entryFixture() });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(true);
    if (result.ok) {
      expect(result.manifest.entry.path).toBe('index.html');
    }
  });

  it('rejects a tampered manifest byte (signature no longer verifies)', () => {
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, { entry: entryFixture() });
    // Flip one byte AFTER signing.
    const tampered = Buffer.from(manifestBytes);
    tampered[tampered.length - 2] ^= 0xff;
    const result = verifyManifest({
      manifestBytes: tampered,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature does not verify/);
  });

  it('rejects a signature made by a DIFFERENT key', () => {
    const signer = makeKeypair();
    const verifier = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(signer, {
      entry: entryFixture(),
    });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: verifier.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature does not verify/);
  });

  it('fails closed when the public key is empty (cache dormant)', () => {
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, { entry: entryFixture() });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: '',
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/no verification key configured/);
  });

  it('rejects a manifest with schemaVersion != 1', () => {
    const kp = makeKeypair();
    const built = buildSignedManifest(kp, { entry: entryFixture() });
    const obj = { ...built.manifestObject, schemaVersion: 2 };
    const bytes = Buffer.from(JSON.stringify(obj), 'utf8');
    // Re-sign the mutated bytes so we reach the schema check, not the sig check.
    const sig = signBytes(bytes, kp.privateKeyPem);
    const result = verifyManifest({
      manifestBytes: bytes,
      signatureBase64: sig,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/schema invalid|schemaVersion/);
  });

  it('rejects when spaIpcContract > shell contract (binary update required)', () => {
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, {
      entry: entryFixture(),
      spaIpcContract: SHELL_IPC + 5,
    });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/binary update required/);
  });

  it('rejects a manifest stale beyond the freshness window', () => {
    const kp = makeKeypair();
    const generatedAt = new Date(Date.now() - SPA_CACHE_MAX_STALENESS_MS - 60_000).toISOString();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, {
      entry: entryFixture(),
      generatedAt,
    });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/stale/);
  });

  it('rejects a generatedAt implausibly in the future', () => {
    const kp = makeKeypair();
    const generatedAt = new Date(Date.now() + 5 * 24 * 60 * 60 * 1000).toISOString();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, {
      entry: entryFixture(),
      generatedAt,
    });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/future/);
  });

  it('rejects when totalSize does not equal entry+assets sum', () => {
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, {
      entry: entryFixture(),
      totalSizeOverride: 999_999,
    });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/totalSize/);
  });

  it('rejects manifest bytes larger than the size cap before any crypto', () => {
    const kp = makeKeypair();
    const oversized = Buffer.alloc(SPA_MANIFEST_MAX_BYTES + 1, 0x20);
    const result = verifyManifest({
      manifestBytes: oversized,
      signatureBase64: 'AAAA',
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/exceeds/);
  });

  it('rejects a manifest whose asset path is a traversal path (zod rejects)', () => {
    const kp = makeKeypair();
    // Hand-build an object with a malicious asset path, then sign the bytes so
    // we reach the zod schema check (not the signature check).
    const goodEntry = entryFixture();
    const malicious = {
      schemaVersion: 1,
      buildId: 'test-build-1',
      spaIpcContract: 1,
      generatedAt: new Date().toISOString(),
      entry: {
        path: 'index.html',
        sha256: 'a'.repeat(64),
        size: goodEntry.bytes.length,
      },
      assets: [
        {
          path: '../../../etc/passwd',
          sha256: 'b'.repeat(64),
          size: 10,
        },
      ],
      totalSize: goodEntry.bytes.length + 10,
    };
    const bytes = Buffer.from(JSON.stringify(malicious), 'utf8');
    const sig = signBytes(bytes, kp.privateKeyPem);
    const result = verifyManifest({
      manifestBytes: bytes,
      signatureBase64: sig,
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/schema invalid|traversal/);
  });

  it('rejects an empty signature', () => {
    const kp = makeKeypair();
    const { manifestBytes } = buildSignedManifest(kp, { entry: entryFixture() });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64: '',
      publicKeyPem: kp.publicKeyPem,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/empty signature|signature/);
  });
});

// ── Shipped trust-root is dormant / fail-closed (Finding E3) ────────────────
// Asserts the COMMITTED default state of the public-key module: the placeholder
// is empty and `isSpaManifestKeyConfigured()` is false, so the verifier refuses
// ALL manifests until the operator runs the key ceremony. This makes any future
// key-ceremony commit a DELIBERATE, reviewed change to this test — a silent
// trust-root flip cannot land green. (No vi.mock here — the REAL module values.)
describe('spaManifestPublicKey shipped default (#1870 Finding E3)', () => {
  it('ships with an EMPTY placeholder public key (cache dormant)', () => {
    expect(SPA_MANIFEST_PUBLIC_KEY_PEM).toBe('');
  });

  it('reports the key as NOT configured (fail-closed)', () => {
    expect(isSpaManifestKeyConfigured()).toBe(false);
  });

  it('verifyManifest fails closed under the shipped (empty) public key', () => {
    // End-to-end: a manifest signed by a real ephemeral key still cannot verify
    // against the committed empty trust root — confirms dormancy is enforced.
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, { entry: entryFixture() });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPem: SPA_MANIFEST_PUBLIC_KEY_PEM,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/no verification key configured/);
  });
});
