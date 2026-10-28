import { NativeModules } from 'react-native';

/**
 * Opaque native key handle. Key bytes never cross into JavaScript, and the identity key
 * never leaves native code. JS can still ask native code to wrap a channel key to any
 * 4096-bit public key it passes, so the JS bundle is part of the trust boundary.
 */
export type KeyHandle = string & { readonly __brand: 'ConcordKeyHandle' };

export type CreatedIdentity = {
  privateKeyHandle: KeyHandle;
  publicKeySpkiBase64: string;
  wrappedPrivateKeyBase64: string;
  keyDerivationSalt: string;
  keyDerivationAlg: 'argon2id';
};

type NativeCrypto = {
  unlockIdentity(
    password: string,
    saltBase64: string,
    wrapped: string,
  ): Promise<unknown>;
  createIdentity(password: string): Promise<unknown>;
  generateChannelKey(): Promise<unknown>;
  unwrapChannelKey(wrapped: string, privateKeyHandle: string): Promise<unknown>;
  wrapChannelKey(
    channelKeyHandle: string,
    publicKeySpkiBase64: string,
  ): Promise<unknown>;
  encryptMessage(plaintext: string, channelKeyHandle: string): Promise<unknown>;
  decryptMessage(
    ciphertextBase64: string,
    channelKeyHandle: string,
  ): Promise<unknown>;
  releaseKey(handle: string): Promise<unknown>;
  releaseAll(): Promise<unknown>;
  persistIdentity(privateKeyHandle: string, account: string): Promise<unknown>;
  loadIdentity(
    account: string,
    reason: string,
    allowUI: boolean,
  ): Promise<unknown>;
  deleteIdentity(account: string): Promise<unknown>;
};

const HANDLE = /^[0-9A-F]{8}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{4}-[0-9A-F]{12}$/;
// Non-empty: native crypto output is never empty.
const BASE64 =
  /^(?=.)(?:[A-Za-z0-9+/]{4})*(?:[A-Za-z0-9+/]{2}==|[A-Za-z0-9+/]{3}=)?$/;

function native(): NativeCrypto {
  const module = (NativeModules as { ConcordCryptoModule?: NativeCrypto })
    .ConcordCryptoModule;
  // No JavaScript fallback: without the native module there is no crypto at all.
  if (!module) throw new Error('Concord native crypto is unavailable');
  return module;
}

function handle(value: unknown): KeyHandle {
  if (typeof value !== 'string' || !HANDLE.test(value)) {
    throw new Error('Invalid native key handle');
  }
  return value as KeyHandle;
}

function base64(value: unknown): string {
  if (typeof value !== 'string' || !BASE64.test(value)) {
    throw new Error('Invalid native crypto output');
  }
  return value;
}

export async function unlockIdentity(
  password: string,
  saltBase64: string,
  wrappedPrivateKeyBase64: string,
  keyDerivationAlg: string,
): Promise<KeyHandle> {
  // Only Argon2id is implemented natively. Say so, rather than letting a legacy
  // account fail as a wrong password.
  if (keyDerivationAlg !== 'argon2id') {
    throw new Error(
      `Unsupported key derivation: ${keyDerivationAlg || 'none'}`,
    );
  }
  return handle(
    await native().unlockIdentity(
      password,
      saltBase64,
      wrappedPrivateKeyBase64,
    ),
  );
}

export async function createIdentity(
  password: string,
): Promise<CreatedIdentity> {
  const value = await native().createIdentity(password);
  if (typeof value !== 'object' || value === null) {
    throw new Error('Invalid native crypto output');
  }
  const record = value as Record<string, unknown>;
  if (record.keyDerivationAlg !== 'argon2id') {
    throw new Error('Invalid native crypto output');
  }
  return {
    privateKeyHandle: handle(record.privateKeyHandle),
    publicKeySpkiBase64: base64(record.publicKeySpkiBase64),
    wrappedPrivateKeyBase64: base64(record.wrappedPrivateKeyBase64),
    keyDerivationSalt: base64(record.keyDerivationSalt),
    keyDerivationAlg: 'argon2id',
  };
}

export async function generateChannelKey(): Promise<KeyHandle> {
  return handle(await native().generateChannelKey());
}

export async function unwrapChannelKey(
  wrappedKeyBase64: string,
  privateKey: KeyHandle,
): Promise<KeyHandle> {
  return handle(await native().unwrapChannelKey(wrappedKeyBase64, privateKey));
}

export async function wrapChannelKey(
  channelKey: KeyHandle,
  publicKeySpkiBase64: string,
): Promise<string> {
  return base64(await native().wrapChannelKey(channelKey, publicKeySpkiBase64));
}

export async function encryptMessage(
  plaintext: string,
  channelKey: KeyHandle,
): Promise<string> {
  return base64(await native().encryptMessage(plaintext, channelKey));
}

export async function decryptMessage(
  ciphertextBase64: string,
  channelKey: KeyHandle,
): Promise<string> {
  const value = await native().decryptMessage(ciphertextBase64, channelKey);
  if (typeof value !== 'string')
    throw new Error('Invalid native crypto output');
  return value;
}

/** Stores an unlocked identity behind Face ID / Touch ID (device-only, current biometric set). */
export async function persistIdentity(
  privateKey: KeyHandle,
  account: string,
): Promise<void> {
  await native().persistIdentity(privateKey, account);
}

/** Reads the identity back; allowUI false must fail rather than show the biometric prompt. */
export async function loadIdentity(
  account: string,
  reason: string,
  allowUI = true,
): Promise<KeyHandle> {
  return handle(await native().loadIdentity(account, reason, allowUI));
}

export async function deleteIdentity(account: string): Promise<void> {
  await native().deleteIdentity(account);
}

/** The stable native rejection code, e.g. E_CONCORD_CRYPTO_NOTFOUND, or null. */
export function nativeErrorCode(error: unknown): string | null {
  const code = (error as { code?: unknown } | null)?.code;
  return typeof code === 'string' && code.startsWith('E_CONCORD_CRYPTO')
    ? code
    : null;
}

export async function releaseKey(key: KeyHandle): Promise<void> {
  await native().releaseKey(key);
}

export async function releaseAll(): Promise<void> {
  await native().releaseAll();
}
