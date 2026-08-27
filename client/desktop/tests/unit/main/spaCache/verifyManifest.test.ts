import { describe, it, expect } from 'vitest';
import { createPublicKey } from 'node:crypto';
import { verifyManifest } from '@/main/spaCache/verifyManifest';
import { SPA_CACHE_MAX_STALENESS_MS, SPA_MANIFEST_MAX_BYTES } from '@/main/spaCache/manifestSchema';
// REAL public-key module (NO vi.mock) — this file never mocks it, so importing
// the genuine values here asserts the committed trust-root state.
import {
  SPA_MANIFEST_PUBLIC_KEYS_PEM,
  SPA_MANIFEST_PUBLIC_KEY_V1_PEM,
  SPA_MANIFEST_PUBLIC_KEY_V2_PEM,
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [verifier.publicKeyPem],
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
      publicKeyPems: [''],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
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
      publicKeyPems: [kp.publicKeyPem],
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/empty signature|signature/);
  });
});

// ── Shipped trust-root state (#1870 / activation #1907 / rotation #2958) ────
// Asserts the COMMITTED state of the public-key module. After the operator key
// ceremony (#1907) a real SPKI public key is configured and the verifier treats
// it as the trust anchor; #2958 added a SECOND (incoming) key for the rotation
// window. Still the genuine module values — no vi.mock — so any future change to
// the committed trust root (adding a key, retiring V1, or reverting to dormant)
// is a DELIBERATE, reviewed edit to this test; a silent trust-root flip cannot
// land green.
describe('spaManifestPublicKey shipped trust root (#1870, dual-trust #2958)', () => {
  it('ships exactly two trusted keys during the rotation window', () => {
    // The COUNT is asserted, not just ">= 1". Retiring V1 (#2958 step 3) or
    // adding a third signer must come here first and be argued for.
    expect(SPA_MANIFEST_PUBLIC_KEYS_PEM).toHaveLength(2);
    for (const pem of SPA_MANIFEST_PUBLIC_KEYS_PEM) {
      expect(pem).toContain('BEGIN PUBLIC KEY');
    }
  });

  it('ships DISTINCT keys — asserted over the ARRAY, not the named constants', () => {
    // The single highest-probability failure of the dual-trust change is a
    // copy-paste that puts the same key in both slots: the build looks
    // dual-trusted, every other test passes, and the rotation silently bricks
    // every client at switchover.
    //
    // Assert over SPA_MANIFEST_PUBLIC_KEYS_PEM, because THAT is what the
    // verifier consumes. An earlier draft compared only V1 against V2 and was
    // proven vacuous by mutation: duplicating V1 inside the array left both
    // named constants distinct and the test green on a build that is
    // single-trust in practice. Do not narrow this back to the two constants.
    //
    // Compare canonical DER, not PEM text, so whitespace or line-ending
    // differences cannot fake distinctness.
    const der = (pem: string) =>
      createPublicKey(pem).export({ type: 'spki', format: 'der' }).toString('hex');
    const shipped = SPA_MANIFEST_PUBLIC_KEYS_PEM.map(der);
    expect(new Set(shipped).size).toBe(shipped.length);

    // ...and the array really is built from the two named constants, so the
    // pairwise check above cannot be satisfied by an array of unrelated keys.
    expect(new Set(shipped)).toEqual(
      new Set([der(SPA_MANIFEST_PUBLIC_KEY_V1_PEM), der(SPA_MANIFEST_PUBLIC_KEY_V2_PEM)])
    );
  });

  it('ships both keys as parseable RSA-4096', () => {
    for (const pem of [SPA_MANIFEST_PUBLIC_KEY_V1_PEM, SPA_MANIFEST_PUBLIC_KEY_V2_PEM]) {
      const key = createPublicKey(pem);
      expect(key.asymmetricKeyType).toBe('rsa');
      expect(key.asymmetricKeyDetails?.modulusLength).toBe(4096);
    }
  });

  it('reports the trust list as configured', () => {
    expect(isSpaManifestKeyConfigured()).toBe(true);
  });

  it('verifyManifest rejects a manifest signed by a key other than the committed ones', () => {
    // End-to-end: a manifest signed by some OTHER (ephemeral) key cannot verify
    // against EITHER committed key — confirms the shipped list is the anchor and
    // that a second entry did not turn the verifier permissive.
    const kp = makeKeypair();
    const { manifestBytes, signatureBase64 } = buildSignedManifest(kp, { entry: entryFixture() });
    const result = verifyManifest({
      manifestBytes,
      signatureBase64,
      publicKeyPems: SPA_MANIFEST_PUBLIC_KEYS_PEM,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.now(),
    });
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature does not verify/);
  });
});

// ── Dual-trust verification behavior (#2958) ────────────────────────────────
// The shipped keys' PRIVATE halves are not available to a test (V1 is a GitHub
// secret, V2 is in Infisical, and neither may be committed), so the behavior of
// the multi-key loop is proven with EPHEMERAL keypairs. The suite above covers
// the committed keys' structure; this one covers the logic that consumes them.
describe('verifyManifest dual trust (#2958)', () => {
  const build = (kp: ReturnType<typeof makeKeypair>) =>
    buildSignedManifest(kp, { entry: entryFixture() });

  const verify = (
    signed: ReturnType<typeof buildSignedManifest>,
    publicKeyPems: readonly string[]
  ) =>
    verifyManifest({
      manifestBytes: signed.manifestBytes,
      signatureBase64: signed.signatureBase64,
      publicKeyPems,
      shellIpcContract: SHELL_IPC,
      nowMs: Date.parse(signed.manifestObject.generatedAt as string),
    });

  it('POSITIVE CONTROL: a single-key list still verifies its own signer', () => {
    // If this fails, every rejection below is vacuous — the harness is broken,
    // not the code under test.
    const kp = makeKeypair();
    expect(verify(build(kp), [kp.publicKeyPem]).ok).toBe(true);
  });

  it('accepts a manifest signed by the FIRST key in the list', () => {
    const outgoing = makeKeypair();
    const incoming = makeKeypair();
    expect(verify(build(incoming), [incoming.publicKeyPem, outgoing.publicKeyPem]).ok).toBe(true);
  });

  it('accepts a manifest signed by the SECOND key in the list', () => {
    // The rotation case that matters: the client ships incoming-first, but the
    // deploy is still signing with the outgoing key.
    const outgoing = makeKeypair();
    const incoming = makeKeypair();
    expect(verify(build(outgoing), [incoming.publicKeyPem, outgoing.publicKeyPem]).ok).toBe(true);
  });

  it('is order-independent', () => {
    const a = makeKeypair();
    const b = makeKeypair();
    const signed = build(a);
    expect(verify(signed, [a.publicKeyPem, b.publicKeyPem]).ok).toBe(true);
    expect(verify(signed, [b.publicKeyPem, a.publicKeyPem]).ok).toBe(true);
  });

  it('FALSIFICATION: rejects a signer that is in neither slot', () => {
    // Without this the accept-tests are satisfiable by a verifier that accepts
    // everything the moment the list is non-empty.
    const stranger = makeKeypair();
    const result = verify(build(stranger), [
      makeKeypair().publicKeyPem,
      makeKeypair().publicKeyPem,
    ]);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature does not verify/);
  });

  it('an EMPTY list is dormant, not a signature mismatch', () => {
    const result = verify(build(makeKeypair()), []);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/no verification key configured/);
  });

  it('an ALL-PLACEHOLDER list is dormant, not a signature mismatch', () => {
    // The dormancy regression this guards: filtering blanks AFTER the emptiness
    // test would fall through to the crypto loop and report "signature does not
    // verify" for a cache that was never activated. Both fail closed, but only
    // one of them is diagnosable.
    const result = verify(build(makeKeypair()), ['', '   ', '\n']);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/no verification key configured/);
  });

  it('a blank entry alongside a real key does not disable the real key', () => {
    const kp = makeKeypair();
    expect(verify(build(kp), ['', kp.publicKeyPem]).ok).toBe(true);
  });

  it('a MALFORMED key does not strand a valid key later in the list', () => {
    // Per-key try/catch: a single unparseable PEM must not abort the scan. A
    // whole-loop catch would reject here.
    const kp = makeKeypair();
    expect(
      verify(build(kp), [
        '-----BEGIN PUBLIC KEY-----\nnope\n-----END PUBLIC KEY-----',
        kp.publicKeyPem,
      ]).ok
    ).toBe(true);
  });

  it('an ALL-MALFORMED list reports a key error, not a signature mismatch', () => {
    const result = verify(build(makeKeypair()), [
      '-----BEGIN PUBLIC KEY-----\nnope\n-----END PUBLIC KEY-----',
    ]);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature verification error/);
  });

  it('a tampered manifest is rejected under EVERY trusted key', () => {
    // Adding a second signer must not create a "some key will accept it" path.
    const a = makeKeypair();
    const b = makeKeypair();
    const signed = build(a);
    const tampered = {
      ...signed,
      manifestBytes: Buffer.concat([signed.manifestBytes, Buffer.from(' ')]),
    };
    const result = verify(tampered, [a.publicKeyPem, b.publicKeyPem]);
    expect(result.ok).toBe(false);
    if (!result.ok) expect(result.reason).toMatch(/signature does not verify/);
  });
});
