import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { resetAllStores } from '../../helpers/store-helpers';
import vectors from '../../../../../docs/design/trusted-recovery-v2-vectors.json';
import {
  arrayBufferToBase64,
  base64ToArrayBuffer,
  exportECDHPublicKey,
  generateDeviceRecoveryKeyPair,
  validateRecoveryAccountKey,
} from '@/renderer/utils/crypto/crypto';
import {
  canonicalRecoveryTranscript,
  recoveryTranscriptHash,
  deriveDeviceRecoveryKeys,
  deriveRecoveryAccountBinding,
  captureRecoveryTokenContext,
  encryptDeviceRecoveryPayload,
  decryptDeviceRecoveryPayload,
  validateRecoveryPublicKey,
  validateRecoveryPayload,
} from '@/renderer/utils/crypto/trustedDeviceRecovery';
import type { DeviceRecoveryContext } from '@/renderer/services/system/deviceRecoveryContract';

const context: DeviceRecoveryContext = { ...vectors.context, protocol_version: 2 };
const offer = vectors.offer;
function toUrlBase64(bytes: ArrayBuffer): string {
  return arrayBufferToBase64(bytes).replaceAll('+', '-').replaceAll('/', '_').replaceAll('=', '');
}
async function privateKey(role: 'requester' | 'responder'): Promise<CryptoKey> {
  const point = new Uint8Array(
    base64ToArrayBuffer(
      role === 'requester' ? context.requester_public_key : offer.responder_public_key
    )
  );
  return crypto.subtle.importKey(
    'jwk',
    {
      kty: 'EC',
      crv: 'P-384',
      x: toUrlBase64(point.slice(1, 49).buffer),
      y: toUrlBase64(point.slice(49).buffer),
      d: vectors[`${role}_private_scalar`]
        .replaceAll('+', '-')
        .replaceAll('/', '_')
        .replaceAll('=', ''),
    },
    { name: 'ECDH', namedCurve: 'P-384' },
    false,
    ['deriveBits']
  );
}
async function fixture() {
  return {
    sender: await deriveDeviceRecoveryKeys(
      'responder',
      await privateKey('responder'),
      context,
      offer
    ),
    receiver: await deriveDeviceRecoveryKeys(
      'requester',
      await privateKey('requester'),
      context,
      offer
    ),
  };
}
beforeEach(() => resetAllStores());
afterEach(() => {
  vi.restoreAllMocks();
  vi.useRealTimers();
});

describe('trusted recovery v2 shared contract', () => {
  it('pins both roles to cross-language transcript, fingerprint and AES-GCM/AAD vectors', async () => {
    expect(new TextDecoder().decode(canonicalRecoveryTranscript(context, offer))).toBe(
      vectors.canonical_transcript
    );
    expect(arrayBufferToBase64(canonicalRecoveryTranscript(context, offer).buffer)).toBe(
      vectors.canonical_transcript_base64
    );
    expect(await recoveryTranscriptHash(context, offer)).toBe(vectors.transcript_hash);
    expect(await deriveRecoveryAccountBinding(vectors.user_id)).toBe(context.account_binding);
    const f = await fixture();
    expect(f.sender.fingerprint).toBe(vectors.fingerprint);
    expect(f.receiver.fingerprint).toBe(vectors.fingerprint);
    expect(f.sender.transcriptHash).toBe(vectors.transcript_hash);
    expect(f.receiver.encryptionKey.extractable).toBe(false);
    expect(f.receiver.encryptionKey.algorithm).toEqual({ name: 'AES-GCM', length: 256 });
    expect(
      arrayBufferToBase64(await decryptDeviceRecoveryPayload(f.receiver, vectors.encrypted_payload))
    ).toBe(vectors.plaintext);
    vi.spyOn(crypto, 'getRandomValues').mockImplementation((array) => {
      if (!(array instanceof Uint8Array)) throw new Error('Unexpected random array');
      array.set(new Uint8Array(base64ToArrayBuffer(vectors.iv)));
      return array;
    });
    expect(
      await encryptDeviceRecoveryPayload(f.sender, base64ToArrayBuffer(vectors.plaintext))
    ).toBe(vectors.encrypted_payload);
  });
  it('captures immutable token UUID/JTI context without treating decoding as signature validation', async () => {
    vi.useFakeTimers();
    vi.setSystemTime(context.expires_at - 1000);
    const jwt = `local.${toUrlBase64(new TextEncoder().encode(JSON.stringify({ user_id: vectors.user_id.toUpperCase(), jti: vectors.recovery_token_jti, exp: context.expires_at / 1000 })).buffer)}.unsigned`;
    const captured = await captureRecoveryTokenContext(jwt);
    expect(captured).toEqual({
      userId: vectors.user_id,
      accountBinding: context.account_binding,
      jtiHash: context.recovery_token_jti_hash,
      expiresAt: context.expires_at,
    });
    expect(Object.isFrozen(captured)).toBe(true);
    vi.setSystemTime(context.expires_at);
    await expect(captureRecoveryTokenContext(jwt)).rejects.toThrow();
    await expect(captureRecoveryTokenContext('old-token')).rejects.toThrow();
  });
  it('generates fresh P-384 ephemeral private keys that cannot be exported', async () => {
    const first = await generateDeviceRecoveryKeyPair();
    const second = await generateDeviceRecoveryKeyPair();
    expect(first.privateKey.extractable).toBe(false);
    expect(first.privateKey.usages).toEqual(['deriveBits']);
    await expect(crypto.subtle.exportKey('pkcs8', first.privateKey)).rejects.toThrow();
    expect(await exportECDHPublicKey(first.publicKey)).not.toBe(
      await exportECDHPublicKey(second.publicKey)
    );
  });
  it.each([
    'server_origin',
    'request_id',
    'account_binding',
    'expires_at',
    'requester_nonce',
    'requester_public_key',
    'recovery_token_jti_hash',
    'responder_nonce',
    'responder_public_key',
  ] as const)('separates keys and refuses ciphertext after a %s substitution', async (field) => {
    const other = await generateDeviceRecoveryKeyPair();
    const otherPublic = await exportECDHPublicKey(other.publicKey);
    const changes = {
      server_origin: 'http://localhost:8080',
      request_id: 'bbbbbbbb-bbbb-4ccc-8ddd-eeeeeeeeeeee',
      account_binding: arrayBufferToBase64(new Uint8Array(32).fill(1).buffer),
      expires_at: context.expires_at + 1,
      requester_nonce: arrayBufferToBase64(new Uint8Array(32).fill(2).buffer),
      requester_public_key: otherPublic,
      recovery_token_jti_hash: arrayBufferToBase64(new Uint8Array(32).fill(3).buffer),
      responder_nonce: arrayBufferToBase64(new Uint8Array(32).fill(4).buffer),
      responder_public_key: otherPublic,
    };
    const modifiedContext = {
      ...context,
      ...(field in context ? { [field]: changes[field] } : {}),
    };
    const modifiedOffer = { ...offer, ...(field in offer ? { [field]: changes[field] } : {}) };
    const key = await deriveDeviceRecoveryKeys(
      'requester',
      await privateKey('requester'),
      modifiedContext,
      modifiedOffer
    );
    expect(key.fingerprint).not.toBe(vectors.fingerprint);
    await expect(decryptDeviceRecoveryPayload(key, vectors.encrypted_payload)).rejects.toThrow();
  });
  it('authenticates AAD separately from the encryption key and refuses tampered bytes', async () => {
    const { receiver } = await fixture();
    await expect(
      decryptDeviceRecoveryPayload(
        { ...receiver, transcriptHash: arrayBufferToBase64(new Uint8Array(32).buffer) },
        vectors.encrypted_payload
      )
    ).rejects.toThrow();
    const bytes = new Uint8Array(base64ToArrayBuffer(vectors.encrypted_payload));
    bytes[bytes.length - 1] ^= 1;
    await expect(
      decryptDeviceRecoveryPayload(receiver, arrayBufferToBase64(bytes.buffer))
    ).rejects.toThrow();
  });
  it.each([0, 1, 3])('refuses envelope version %s without legacy fallback', async (version) => {
    const { receiver } = await fixture();
    const bytes = new Uint8Array(base64ToArrayBuffer(vectors.encrypted_payload));
    bytes[0] = version;
    await expect(
      decryptDeviceRecoveryPayload(receiver, arrayBufferToBase64(bytes.buffer))
    ).rejects.toThrow('Update both devices');
  });
  it('rejects noncanonical binary, wrong curves, invalid points, versions and origin aliases', async () => {
    for (const payload of [
      '',
      '!!!!',
      vectors.encrypted_payload.replaceAll('=', ''),
      arrayBufferToBase64(new Uint8Array(8193).buffer),
    ])
      expect(() => validateRecoveryPayload(payload)).toThrow();
    for (const key of [
      'AAAA',
      context.requester_public_key.replaceAll('=', ''),
      arrayBufferToBase64(new Uint8Array(97).fill(4).buffer),
    ])
      await expect(validateRecoveryPublicKey(key)).rejects.toThrow();
    for (const change of [
      { protocol_version: 1 },
      { request_id: context.request_id.toUpperCase() },
      { server_origin: `${context.server_origin}/` },
      { server_origin: 'https://user:pass@recovery.example.test' },
      { requester_nonce: context.requester_nonce.replaceAll('=', '') },
    ]) {
      expect(() =>
        canonicalRecoveryTranscript({ ...context, ...change } as DeviceRecoveryContext, offer)
      ).toThrow();
    }
    const p256 = await crypto.subtle.generateKey({ name: 'ECDH', namedCurve: 'P-256' }, false, [
      'deriveBits',
    ]);
    await expect(
      validateRecoveryPublicKey(await exportECDHPublicKey(p256.publicKey))
    ).rejects.toThrow();
  });
  it('wipes raw ECDH bits even if HKDF fails and fences each continuation', async () => {
    const bits = new Uint8Array(48).fill(9).buffer;
    vi.spyOn(crypto.subtle, 'deriveBits').mockResolvedValue(bits);
    vi.spyOn(crypto.subtle, 'deriveKey').mockRejectedValue(new Error('WebCrypto unavailable'));
    await expect(
      deriveDeviceRecoveryKeys('requester', await privateKey('requester'), context, offer)
    ).rejects.toThrow('WebCrypto unavailable');
    expect(new Uint8Array(bits)).toEqual(new Uint8Array(48));
    vi.restoreAllMocks();
    const guard = vi.fn(() => {
      if (guard.mock.calls.length >= 4) throw new Error('changed');
    });
    await expect(
      deriveDeviceRecoveryKeys('requester', await privateKey('requester'), context, offer, guard)
    ).rejects.toThrow('changed');
  });
  it('rejects invalid PKCS8 and recovered RSA keys below the account-key minimum', async () => {
    await expect(validateRecoveryAccountKey(new Uint8Array([1, 2, 3]).buffer)).rejects.toThrow();
    const undersized = await crypto.subtle.generateKey(
      {
        name: 'RSA-OAEP',
        modulusLength: 2048,
        publicExponent: new Uint8Array([1, 0, 1]),
        hash: 'SHA-256',
      },
      true,
      ['encrypt', 'decrypt']
    );
    const bytes = await crypto.subtle.exportKey('pkcs8', undersized.privateKey);
    try {
      await expect(validateRecoveryAccountKey(bytes)).rejects.toThrow(
        'Invalid recovered account key'
      );
    } finally {
      new Uint8Array(bytes).fill(0);
    }
  });
  it.each(['digest', 'importKey', 'deriveKey'] as const)(
    'fails closed when WebCrypto %s is unavailable',
    async (primitive) => {
      const local = await privateKey('requester');
      vi.spyOn(crypto.subtle, primitive).mockRejectedValue(new Error('unavailable'));
      await expect(deriveDeviceRecoveryKeys('requester', local, context, offer)).rejects.toThrow(
        'unavailable'
      );
    }
  );
  it('fails closed when primitive generation or derivation is unavailable', async () => {
    vi.spyOn(crypto.subtle, 'generateKey').mockRejectedValue(new Error('unavailable'));
    await expect(generateDeviceRecoveryKeyPair()).rejects.toThrow('unavailable');
    vi.restoreAllMocks();
    vi.spyOn(crypto.subtle, 'deriveBits').mockRejectedValue(new Error('unavailable'));
    await expect(
      deriveDeviceRecoveryKeys('requester', await privateKey('requester'), context, offer)
    ).rejects.toThrow('unavailable');
  });
});
