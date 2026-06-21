// @vitest-environment node
//
// Signing-path tests for #1624 (RCI-critical). Two layers of parity:
//   1. The committed A↔B fixture's signature verifies against our canonical bytes
//      under RSA-PSS / SHA-256 / saltLength 32 — proves our verify path (and the
//      canonical serializer + PSS params) match the Go server's signer.
//   2. e2eeService.deriveSigningKey() re-unwraps the SAME device key as a
//      non-extractable RSA-PSS sign handle; its signatures verify against the
//      device public key. This is the production round-trip.
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, it, expect, beforeEach, afterEach } from 'vitest';
import { e2eeService } from '@/renderer/services/e2eeService';
import { generateRegistrationKeys, base64ToArrayBuffer } from '@/renderer/utils/crypto';
import { useE2EEStore } from '@/renderer/stores/e2eeStore';
import { buildCanonicalBytes, type AgeClaim } from '@/renderer/services/ageClaim/canonicalAgeClaim';

// Server-owned single source of truth (#1623). Read via node:fs, not import.
const FIXTURE = JSON.parse(
  readFileSync(
    resolve(
      __dirname,
      '../../../../../../services/control-plane/internal/age/testdata/age-claim-canonical-v1.json'
    ),
    'utf8'
  )
);

function fixtureClaim(): AgeClaim {
  const c = FIXTURE.claim;
  return {
    canonicalVersion: c.canonical_version,
    userId: c.user_id,
    validAge: c.valid_age,
    nsfwAuth: c.nsfw_auth,
    jurisdictionObligation: c.jurisdiction_obligation,
    nonce: c.nonce,
    timestamp: c.timestamp,
    keyVersion: c.key_version,
    clientVersion: c.client_version,
  };
}

const PSS = { name: 'RSA-PSS', hash: 'SHA-256' } as const;
const PSS_SIGN = { name: 'RSA-PSS', saltLength: 32 } as const;
const testPassword = 'TestPassword123!';

describe('age-claim signing', () => {
  beforeEach(() => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
  });
  afterEach(() => {
    e2eeService.clearKeys();
    useE2EEStore.getState().reset();
  });

  it('fixture signature verifies against canonical bytes (RSA-PSS, saltLength 32)', async () => {
    const pub = await crypto.subtle.importKey(
      'spki',
      base64ToArrayBuffer(FIXTURE.public_key_spki_b64),
      PSS,
      false,
      ['verify']
    );
    const bytes = buildCanonicalBytes(fixtureClaim());
    const ok = await crypto.subtle.verify(
      PSS_SIGN,
      pub,
      base64ToArrayBuffer(FIXTURE.signature_b64),
      bytes
    );
    expect(ok).toBe(true);
  });

  it('fixture signature does NOT verify against tampered canonical bytes', async () => {
    const pub = await crypto.subtle.importKey(
      'spki',
      base64ToArrayBuffer(FIXTURE.public_key_spki_b64),
      PSS,
      false,
      ['verify']
    );
    const tampered = buildCanonicalBytes({ ...fixtureClaim(), validAge: !fixtureClaim().validAge });
    const ok = await crypto.subtle.verify(
      PSS_SIGN,
      pub,
      base64ToArrayBuffer(FIXTURE.signature_b64),
      tampered
    );
    expect(ok).toBe(false);
  });

  it('deriveSigningKey signs with the device key; signature verifies against the device public key', async () => {
    const regKeys = await generateRegistrationKeys(testPassword);
    await e2eeService.initialize(
      testPassword,
      regKeys.wrappedPrivateKey,
      regKeys.keyDerivationSalt
    );

    const signKey = await e2eeService.deriveSigningKey();
    expect(signKey.type).toBe('private');
    expect(signKey.extractable).toBe(false);
    expect(signKey.usages).toContain('sign');

    const bytes = buildCanonicalBytes(fixtureClaim());
    const sig = await crypto.subtle.sign(PSS_SIGN, signKey, bytes);

    // The device key pair shares modulus across OAEP/PSS usages; re-import the
    // device public key material as an RSA-PSS verify key to confirm the round-trip.
    const spki = await crypto.subtle.exportKey('spki', regKeys.publicKey);
    const pssPub = await crypto.subtle.importKey('spki', spki, PSS, false, ['verify']);
    const ok = await crypto.subtle.verify(PSS_SIGN, pssPub, sig, bytes);
    expect(ok).toBe(true);
  });

  it('deriveSigningKey throws when the service is not initialized', async () => {
    await expect(e2eeService.deriveSigningKey()).rejects.toThrow('E2EE service not initialized');
  });

  it('signAgeClaim returns a base64 signature that verifies against the device key', async () => {
    const regKeys = await generateRegistrationKeys(testPassword);
    await e2eeService.initialize(
      testPassword,
      regKeys.wrappedPrivateKey,
      regKeys.keyDerivationSalt
    );

    const bytes = buildCanonicalBytes(fixtureClaim());
    const sigB64 = await e2eeService.signAgeClaim(bytes);
    expect(typeof sigB64).toBe('string');
    expect(sigB64.length).toBeGreaterThan(0);

    const spki = await crypto.subtle.exportKey('spki', regKeys.publicKey);
    const pssPub = await crypto.subtle.importKey('spki', spki, PSS, false, ['verify']);
    const ok = await crypto.subtle.verify(PSS_SIGN, pssPub, base64ToArrayBuffer(sigB64), bytes);
    expect(ok).toBe(true);
  });

  it('signAgeClaim throws when the service is not initialized', async () => {
    await expect(e2eeService.signAgeClaim(new Uint8Array([1, 2, 3]))).rejects.toThrow(
      'E2EE service not initialized'
    );
  });
});
