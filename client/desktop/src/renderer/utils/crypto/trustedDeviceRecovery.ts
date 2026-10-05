import {
  arrayBufferToBase64,
  base64ToArrayBuffer,
  importECDHPublicKey,
  sha256,
  deriveDeviceRecoveryKeyMaterial,
  encryptRecoveryEnvelope,
  decryptRecoveryEnvelope,
  type DeviceRecoveryKeyMaterial,
} from './crypto';
import { isValidUUID } from '../runtime/uuid';
import type {
  DeviceRecoveryContext,
  DeviceRecoveryOffer,
} from '../../services/system/deviceRecoveryContract';

export { generateDeviceRecoveryKeyPair } from './crypto';
export const RECOVERY_UPDATE_GUIDANCE =
  'Could not authenticate recovery. Update both devices and start a new request.';

export interface RecoveryTokenContext {
  readonly userId: string;
  readonly accountBinding: string;
  readonly jtiHash: string;
  readonly expiresAt: number;
}

export function canonicalUUID(value: string): string {
  if (!isValidUUID(value)) throw new Error(RECOVERY_UPDATE_GUIDANCE);
  return value.toLowerCase();
}

export function decodeCanonicalBase64(value: string, length: number): ArrayBuffer {
  try {
    const bytes = base64ToArrayBuffer(value);
    if (bytes.byteLength !== length || arrayBufferToBase64(bytes) !== value)
      throw new Error(RECOVERY_UPDATE_GUIDANCE);
    return bytes;
  } catch {
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  }
}

export function validateRecoveryOrigin(origin: string): void {
  const url = new URL(origin);
  if (
    url.origin !== origin ||
    !['http:', 'https:'].includes(url.protocol) ||
    url.username ||
    url.password ||
    new TextEncoder().encode(origin).length > 512
  ) {
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  }
}

export async function deriveRecoveryAccountBinding(userId: string): Promise<string> {
  return arrayBufferToBase64(
    await sha256(
      new TextEncoder().encode(
        `concord-trusted-device-recovery/account/v2\0${canonicalUUID(userId)}`
      )
    )
  );
}

/** JWT decoding captures local context only; server authentication still validates its signature. */
export async function captureRecoveryTokenContext(token: string): Promise<RecoveryTokenContext> {
  if (token.length > 4096 || token.split('.').length !== 3)
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  const encoded = token.split('.')[1];
  const padded = encoded
    .replaceAll('-', '+')
    .replaceAll('_', '/')
    .padEnd(Math.ceil(encoded.length / 4) * 4, '=');
  const claims: unknown = JSON.parse(
    new TextDecoder('utf-8', { fatal: true }).decode(base64ToArrayBuffer(padded))
  );
  if (!claims || typeof claims !== 'object' || Array.isArray(claims))
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  const fields = claims as Record<string, unknown>;
  if (
    typeof fields.user_id !== 'string' ||
    typeof fields.jti !== 'string' ||
    !fields.jti ||
    typeof fields.exp !== 'number' ||
    !Number.isSafeInteger(fields.exp) ||
    !Number.isSafeInteger(fields.exp * 1000) ||
    fields.exp * 1000 <= Date.now()
  ) {
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  }
  const userId = canonicalUUID(fields.user_id);
  const accountBinding = await deriveRecoveryAccountBinding(userId);
  const jtiHash = arrayBufferToBase64(await sha256(new TextEncoder().encode(fields.jti)));
  return Object.freeze({ userId, accountBinding, jtiHash, expiresAt: fields.exp * 1000 });
}

export async function validateRecoveryPublicKey(value: string): Promise<void> {
  if (new Uint8Array(decodeCanonicalBase64(value, 97))[0] !== 4)
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  await importECDHPublicKey(value); // WebCrypto validates P-384 curve membership.
}

export function validateRecoveryContext(context: DeviceRecoveryContext): void {
  if (
    context.protocol_version !== 2 ||
    canonicalUUID(context.request_id) !== context.request_id ||
    !Number.isSafeInteger(context.expires_at) ||
    context.expires_at <= 0
  ) {
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  }
  validateRecoveryOrigin(context.server_origin);
  decodeCanonicalBase64(context.account_binding, 32);
  decodeCanonicalBase64(context.requester_nonce, 32);
  decodeCanonicalBase64(context.recovery_token_jti_hash, 32);
  if (new Uint8Array(decodeCanonicalBase64(context.requester_public_key, 97))[0] !== 4)
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
}

export function canonicalRecoveryTranscript(
  context: DeviceRecoveryContext,
  offer: Pick<DeviceRecoveryOffer, 'responder_public_key' | 'responder_nonce'>
): Uint8Array<ArrayBuffer> {
  validateRecoveryContext(context);
  decodeCanonicalBase64(offer.responder_nonce, 32);
  if (new Uint8Array(decodeCanonicalBase64(offer.responder_public_key, 97))[0] !== 4)
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  return new TextEncoder().encode(
    JSON.stringify([
      'concord-trusted-device-recovery',
      2,
      context.server_origin,
      context.request_id,
      context.account_binding,
      context.expires_at,
      context.requester_nonce,
      offer.responder_nonce,
      context.recovery_token_jti_hash,
      'requester',
      context.requester_public_key,
      'responder',
      offer.responder_public_key,
    ])
  );
}

export async function recoveryTranscriptHash(
  context: DeviceRecoveryContext,
  offer: Pick<DeviceRecoveryOffer, 'responder_public_key' | 'responder_nonce'>
): Promise<string> {
  return arrayBufferToBase64(await sha256(canonicalRecoveryTranscript(context, offer)));
}

/** Construct with this role's retained local fields, never a relay-supplied replacement. */
export async function deriveDeviceRecoveryKeys(
  role: 'requester' | 'responder',
  privateKey: CryptoKey,
  context: DeviceRecoveryContext,
  offer: Pick<DeviceRecoveryOffer, 'responder_public_key' | 'responder_nonce'>,
  assertCurrent: () => void = () => {}
): Promise<DeviceRecoveryKeyMaterial> {
  assertCurrent();
  const peer = await importECDHPublicKey(
    role === 'requester' ? offer.responder_public_key : context.requester_public_key
  );
  assertCurrent();
  return deriveDeviceRecoveryKeyMaterial(
    privateKey,
    peer,
    canonicalRecoveryTranscript(context, offer),
    assertCurrent
  );
}

export async function encryptDeviceRecoveryPayload(
  material: DeviceRecoveryKeyMaterial,
  pkcs8: ArrayBuffer
): Promise<string> {
  return encryptRecoveryEnvelope(
    material.encryptionKey,
    pkcs8,
    decodeCanonicalBase64(material.transcriptHash, 32)
  );
}

export function validateRecoveryPayload(payload: string): ArrayBuffer {
  if (payload.length > 10924) throw new Error(RECOVERY_UPDATE_GUIDANCE);
  const bytes = base64ToArrayBuffer(payload);
  if (
    bytes.byteLength < 30 ||
    bytes.byteLength > 8192 ||
    arrayBufferToBase64(bytes) !== payload ||
    new Uint8Array(bytes)[0] !== 2
  ) {
    throw new Error(RECOVERY_UPDATE_GUIDANCE);
  }
  return bytes;
}

export async function decryptDeviceRecoveryPayload(
  material: DeviceRecoveryKeyMaterial,
  payload: string
): Promise<ArrayBuffer> {
  return decryptRecoveryEnvelope(
    material.encryptionKey,
    validateRecoveryPayload(payload),
    decodeCanonicalBase64(material.transcriptHash, 32)
  );
}
