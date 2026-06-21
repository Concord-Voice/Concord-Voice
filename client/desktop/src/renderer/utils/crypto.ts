/**
 * Client-side cryptography utilities for E2EE
 * Uses Web Crypto API for secure key generation and management
 */

import { argon2id } from 'hash-wasm';

// Constants for PBKDF2 key derivation (legacy — kept for migration)
const PBKDF2_ITERATIONS = 600000; // 600k iterations (OWASP 2023)
const SALT_LENGTH = 16; // 16 bytes
const DERIVED_KEY_LENGTH = 32; // 32 bytes (256 bits)

// Argon2id parameters — matches server-side (auth/password.go DefaultParams)
const ARGON2_MEMORY = 64 * 1024; // 64 MB in KiB
const ARGON2_ITERATIONS = 3;
const ARGON2_PARALLELISM = 4;
const ARGON2_HASH_LENGTH = 32; // 256 bits

export type KeyDerivationAlgorithm = 'pbkdf2' | 'argon2id';

const PREFERENCES_DOMAIN_SEPARATOR = 'concord-preferences-v1';

// Constants for RSA key pair
const RSA_KEY_SIZE = 4096; // 4096-bit RSA keys (NIST secure through 2040+)
const RSA_PUBLIC_EXPONENT = new Uint8Array([1, 0, 1]); // 65537

/**
 * Generate a random salt for key derivation
 */
export function generateSalt(): Uint8Array {
  return crypto.getRandomValues(new Uint8Array(SALT_LENGTH));
}

/**
 * Derive an encryption key from a password using PBKDF2
 * This key is used to wrap/unwrap the user's private key
 *
 * @param password - User's password
 * @param salt - Salt for key derivation
 * @returns Promise<CryptoKey> - Derived AES-GCM key
 */
export async function deriveKeyFromPassword(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  // Import password as key material
  const encoder = new TextEncoder();
  const passwordKey = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    'PBKDF2',
    false,
    ['deriveKey']
  );

  // Derive AES-GCM key from password
  return crypto.subtle.deriveKey(
    {
      name: 'PBKDF2',
      salt: salt as BufferSource,
      iterations: PBKDF2_ITERATIONS,
      hash: 'SHA-256',
    },
    passwordKey,
    {
      name: 'AES-GCM',
      length: DERIVED_KEY_LENGTH * 8, // bits
    },
    false, // not extractable
    ['wrapKey', 'unwrapKey']
  );
}

/**
 * Derive an encryption key from a password using Argon2id (WASM).
 * Matches server-side parameters (m=64MB, t=3, p=4).
 * Returns a non-extractable AES-GCM key for wrapping/unwrapping.
 */
export async function deriveKeyArgon2id(password: string, salt: Uint8Array): Promise<CryptoKey> {
  const hash = await argon2id({
    password,
    salt,
    iterations: ARGON2_ITERATIONS,
    parallelism: ARGON2_PARALLELISM,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: 'binary',
  });

  return crypto.subtle.importKey(
    'raw',
    hash as BufferSource,
    { name: 'AES-GCM', length: 256 },
    false,
    ['wrapKey', 'unwrapKey']
  );
}

/**
 * Derive an EXTRACTABLE wrapping key from password using Argon2id.
 * Used once during login/registration to export raw bytes for safeStorage.
 */
export async function deriveKeyArgon2idExportable(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  const hash = await argon2id({
    password,
    salt,
    iterations: ARGON2_ITERATIONS,
    parallelism: ARGON2_PARALLELISM,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: 'binary',
  });

  return crypto.subtle.importKey(
    'raw',
    hash as BufferSource,
    { name: 'AES-GCM', length: 256 },
    true,
    ['wrapKey', 'unwrapKey']
  );
}

/**
 * Derive a domain-separated preferences key using Argon2id.
 * Non-extractable for runtime use.
 */
export async function derivePreferencesKeyArgon2id(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  const encoder = new TextEncoder();
  const separator = encoder.encode(PREFERENCES_DOMAIN_SEPARATOR);
  const domainSalt = new Uint8Array(salt.length + separator.length);
  domainSalt.set(salt, 0);
  domainSalt.set(separator, salt.length);

  const hash = await argon2id({
    password,
    salt: domainSalt,
    iterations: ARGON2_ITERATIONS,
    parallelism: ARGON2_PARALLELISM,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: 'binary',
  });

  return crypto.subtle.importKey(
    'raw',
    hash as BufferSource,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt']
  );
}

/**
 * Derive an EXTRACTABLE domain-separated preferences key using Argon2id.
 * Used once during login/registration to export raw bytes for safeStorage.
 */
export async function derivePreferencesKeyArgon2idExportable(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  const encoder = new TextEncoder();
  const separator = encoder.encode(PREFERENCES_DOMAIN_SEPARATOR);
  const domainSalt = new Uint8Array(salt.length + separator.length);
  domainSalt.set(salt, 0);
  domainSalt.set(separator, salt.length);

  const hash = await argon2id({
    password,
    salt: domainSalt,
    iterations: ARGON2_ITERATIONS,
    parallelism: ARGON2_PARALLELISM,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: 'binary',
  });

  return crypto.subtle.importKey(
    'raw',
    hash as BufferSource,
    { name: 'AES-GCM', length: 256 },
    true,
    ['encrypt', 'decrypt']
  );
}

/**
 * Generate an RSA key pair for E2EE
 *
 * @returns Promise<CryptoKeyPair> - RSA-OAEP key pair
 */
export async function generateKeyPair(): Promise<CryptoKeyPair> {
  return crypto.subtle.generateKey(
    {
      name: 'RSA-OAEP',
      modulusLength: RSA_KEY_SIZE,
      publicExponent: RSA_PUBLIC_EXPONENT,
      hash: 'SHA-256',
    },
    true, // extractable
    ['encrypt', 'decrypt']
  );
}

/**
 * Wrap (encrypt) a private key using a password-derived key
 *
 * @param privateKey - RSA private key to wrap
 * @param wrappingKey - AES-GCM key derived from password
 * @returns Promise<ArrayBuffer> - Wrapped (encrypted) private key
 */
export async function wrapPrivateKey(
  privateKey: CryptoKey,
  wrappingKey: CryptoKey
): Promise<ArrayBuffer> {
  // Generate a random IV for AES-GCM
  const iv = crypto.getRandomValues(new Uint8Array(12));

  // Wrap the private key
  const wrappedKey = await crypto.subtle.wrapKey('pkcs8', privateKey, wrappingKey, {
    name: 'AES-GCM',
    iv: iv,
  });

  // Prepend IV to wrapped key (we'll need it for unwrapping)
  const result = new Uint8Array(iv.length + wrappedKey.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(wrappedKey), iv.length);

  return result.buffer;
}

/**
 * Unwrap (decrypt) a private key using a password-derived key
 *
 * @param wrappedKey - Wrapped private key (with IV prepended)
 * @param unwrappingKey - AES-GCM key derived from password
 * @returns Promise<CryptoKey> - Unwrapped RSA private key
 */
export async function unwrapPrivateKey(
  wrappedKey: ArrayBuffer,
  unwrappingKey: CryptoKey
): Promise<CryptoKey> {
  // Extract IV from the beginning of wrapped key
  const data = new Uint8Array(wrappedKey);
  const iv = data.slice(0, 12);
  const actualWrappedKey = data.slice(12);

  // Unwrap the private key
  return crypto.subtle.unwrapKey(
    'pkcs8',
    actualWrappedKey,
    unwrappingKey,
    {
      name: 'AES-GCM',
      iv: iv,
    },
    {
      name: 'RSA-OAEP',
      hash: 'SHA-256',
    },
    false, // non-extractable: prevent key export via XSS
    ['decrypt']
  );
}

/**
 * Unwrap the device private key as a non-extractable RSA-PSS *signing* handle (#1624).
 *
 * Identical to unwrapPrivateKey except the unwrapped key is tagged RSA-PSS / ['sign']
 * instead of RSA-OAEP / ['decrypt']. The wrapped PKCS8 is algorithm-agnostic RSA key
 * material — WebCrypto fixes the unwrapped key's algorithm + usages from these params,
 * so the SAME wrapped device key yields an OAEP-decrypt key OR a PSS-sign key. Raw
 * PKCS8 is never exposed; the result is non-extractable. Used to sign age-claim
 * canonical bytes (RSA-PSS / SHA-256 / saltLength 32) for the #272 age system.
 *
 * @param wrappedKey - Wrapped private key (12-byte IV prepended); same blob as unwrapPrivateKey
 * @param unwrappingKey - AES-GCM key derived from password
 * @returns Promise<CryptoKey> - Non-extractable RSA-PSS private key, usage ['sign']
 */
export async function unwrapSigningKey(
  wrappedKey: ArrayBuffer,
  unwrappingKey: CryptoKey
): Promise<CryptoKey> {
  // Extract IV from the beginning of wrapped key
  const data = new Uint8Array(wrappedKey);
  const iv = data.slice(0, 12);
  const actualWrappedKey = data.slice(12);

  // Unwrap the SAME wrapped PKCS8 as an RSA-PSS sign key (not OAEP decrypt)
  return crypto.subtle.unwrapKey(
    'pkcs8',
    actualWrappedKey,
    unwrappingKey,
    {
      name: 'AES-GCM',
      iv: iv,
    },
    {
      name: 'RSA-PSS',
      hash: 'SHA-256',
    },
    false, // non-extractable: prevent key export via XSS
    ['sign']
  );
}

/**
 * Export a public key to base64 format (for storage/transmission)
 *
 * @param publicKey - RSA public key
 * @returns Promise<string> - Base64-encoded public key
 */
export async function exportPublicKey(publicKey: CryptoKey): Promise<string> {
  const exported = await crypto.subtle.exportKey('spki', publicKey);
  return arrayBufferToBase64(exported);
}

/**
 * Import a public key from base64 format
 *
 * @param base64Key - Base64-encoded public key
 * @returns Promise<CryptoKey> - RSA public key
 */
export async function importPublicKey(base64Key: string): Promise<CryptoKey> {
  const keyData = base64ToArrayBuffer(base64Key);
  return crypto.subtle.importKey(
    'spki',
    keyData,
    {
      name: 'RSA-OAEP',
      hash: 'SHA-256',
    },
    true,
    ['encrypt']
  );
}

/**
 * Convert ArrayBuffer to base64 string
 */
export function arrayBufferToBase64(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  for (let i = 0; i < bytes.byteLength; i++) {
    binary += String.fromCodePoint(bytes[i]);
  }
  return btoa(binary);
}

/**
 * Convert base64 string to ArrayBuffer.
 *
 * atob() produces a binary string where every character is a single code
 * unit in the range 0x00..0xFF. Since `i` is strictly bounded by
 * `binary.length`, `codePointAt(i)` is always defined — but the type
 * signature returns `number | undefined`, so we explicitly narrow via
 * `?? 0` (unreachable fallback) instead of using a non-null assertion.
 */
export function base64ToArrayBuffer(base64: string): ArrayBuffer {
  const binary = atob(base64);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.codePointAt(i) ?? 0;
  }
  return bytes.buffer;
}

/**
 * Complete registration flow: generate keys and wrap private key
 *
 * @param password - User's password
 * @returns Promise<RegistrationKeys> - Keys ready for registration
 */
export interface RegistrationKeys {
  publicKey: CryptoKey;
  privateKey: CryptoKey;
  wrappedPrivateKey: string; // base64
  keyDerivationSalt: string; // base64
  keyDerivationAlg: KeyDerivationAlgorithm;
}

export async function generateRegistrationKeys(password: string): Promise<RegistrationKeys> {
  const salt = generateSalt();

  // Use Argon2id for all new registrations
  const wrappingKey = await deriveKeyArgon2id(password, salt);

  const keyPair = await generateKeyPair();
  const wrappedKey = await wrapPrivateKey(keyPair.privateKey, wrappingKey);

  return {
    publicKey: keyPair.publicKey,
    privateKey: keyPair.privateKey,
    wrappedPrivateKey: arrayBufferToBase64(wrappedKey),
    keyDerivationSalt: arrayBufferToBase64(salt.buffer as ArrayBuffer),
    keyDerivationAlg: 'argon2id',
  };
}

/**
 * Complete login flow: unwrap private key
 *
 * @param password - User's password
 * @param wrappedPrivateKeyBase64 - Wrapped private key from server
 * @param saltBase64 - Salt from server
 * @returns Promise<CryptoKey> - Unwrapped private key
 */
export async function unwrapLoginKeys(
  password: string,
  wrappedPrivateKeyBase64: string,
  saltBase64: string,
  alg: KeyDerivationAlgorithm = 'pbkdf2'
): Promise<CryptoKey> {
  const salt = new Uint8Array(base64ToArrayBuffer(saltBase64));
  const wrappedKey = base64ToArrayBuffer(wrappedPrivateKeyBase64);

  const unwrappingKey =
    alg === 'argon2id'
      ? await deriveKeyArgon2id(password, salt)
      : await deriveKeyFromPassword(password, salt);

  return unwrapPrivateKey(wrappedKey, unwrappingKey);
}

// ========================================
// Exportable key derivation (for session persistence via safeStorage)
// ========================================

/**
 * Derive an EXTRACTABLE wrapping key from password.
 * Used once during login/registration to export raw bytes for safeStorage.
 * The exported bytes are then re-imported as non-extractable for runtime use.
 */
export async function deriveKeyFromPasswordExportable(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  const encoder = new TextEncoder();
  const passwordKey = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    'PBKDF2',
    false,
    ['deriveKey']
  );

  return crypto.subtle.deriveKey(
    {
      name: 'PBKDF2',
      salt: salt as BufferSource,
      iterations: PBKDF2_ITERATIONS,
      hash: 'SHA-256',
    },
    passwordKey,
    {
      name: 'AES-GCM',
      length: DERIVED_KEY_LENGTH * 8,
    },
    true, // extractable — for one-time export only
    ['wrapKey', 'unwrapKey']
  );
}

/**
 * Derive an EXTRACTABLE preferences key from password.
 * Same domain-separation as derivePreferencesKey, but extractable for storage.
 */
export async function derivePreferencesKeyExportable(
  password: string,
  salt: Uint8Array
): Promise<CryptoKey> {
  const encoder = new TextEncoder();
  const separator = encoder.encode(PREFERENCES_DOMAIN_SEPARATOR);
  const domainSalt = new Uint8Array(salt.length + separator.length);
  domainSalt.set(salt, 0);
  domainSalt.set(separator, salt.length);

  const passwordKey = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    'PBKDF2',
    false,
    ['deriveKey']
  );

  return crypto.subtle.deriveKey(
    {
      name: 'PBKDF2',
      salt: domainSalt,
      iterations: PBKDF2_ITERATIONS,
      hash: 'SHA-256',
    },
    passwordKey,
    {
      name: 'AES-GCM',
      length: DERIVED_KEY_LENGTH * 8,
    },
    true, // extractable — for one-time export only
    ['encrypt', 'decrypt']
  );
}

// ========================================
// Channel Symmetric Key (CSK) operations
// ========================================

const AES_GCM_IV_LENGTH = 12; // 12-byte IV for AES-GCM

/**
 * Generate a new channel symmetric key (AES-256-GCM)
 */
export async function generateChannelKey(): Promise<CryptoKey> {
  return crypto.subtle.generateKey(
    { name: 'AES-GCM', length: 256 },
    true, // extractable (needed for wrapping)
    ['encrypt', 'decrypt']
  );
}

/**
 * Export a channel key as raw bytes
 */
export async function exportChannelKey(key: CryptoKey): Promise<ArrayBuffer> {
  return crypto.subtle.exportKey('raw', key);
}

/**
 * Import a channel key from raw bytes
 */
export async function importChannelKey(raw: ArrayBuffer): Promise<CryptoKey> {
  return crypto.subtle.importKey('raw', raw, { name: 'AES-GCM', length: 256 }, true, [
    'encrypt',
    'decrypt',
  ]);
}

/**
 * Wrap a channel key with a member's RSA-OAEP public key.
 * Returns base64-encoded wrapped key.
 */
export async function wrapChannelKey(channelKey: CryptoKey, publicKey: CryptoKey): Promise<string> {
  // Export the channel key as raw, then encrypt with RSA-OAEP
  const rawKey = await exportChannelKey(channelKey);
  const encrypted = await crypto.subtle.encrypt({ name: 'RSA-OAEP' }, publicKey, rawKey);
  return arrayBufferToBase64(encrypted);
}

/**
 * Unwrap a channel key using the user's RSA-OAEP private key.
 * Takes base64-encoded wrapped key, returns CryptoKey.
 */
export async function unwrapChannelKey(
  wrappedKeyBase64: string,
  privateKey: CryptoKey
): Promise<CryptoKey> {
  const wrappedData = base64ToArrayBuffer(wrappedKeyBase64);
  const rawKey = await crypto.subtle.decrypt({ name: 'RSA-OAEP' }, privateKey, wrappedData);
  return importChannelKey(rawKey);
}

/**
 * Encrypt a message with a channel key (AES-256-GCM).
 * Prepends 12-byte random IV to ciphertext. Returns base64.
 */
export async function encryptMessage(plaintext: string, channelKey: CryptoKey): Promise<string> {
  const iv = crypto.getRandomValues(new Uint8Array(AES_GCM_IV_LENGTH));
  const encoded = new TextEncoder().encode(plaintext);

  const ciphertext = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, channelKey, encoded);

  // Prepend IV to ciphertext
  const result = new Uint8Array(iv.length + ciphertext.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(ciphertext), iv.length);

  return arrayBufferToBase64(result.buffer);
}

/**
 * Decrypt a message with a channel key (AES-256-GCM).
 * Expects base64 input with 12-byte IV prepended to ciphertext.
 */
export async function decryptMessage(
  ciphertextBase64: string,
  channelKey: CryptoKey
): Promise<string> {
  const data = new Uint8Array(base64ToArrayBuffer(ciphertextBase64));
  const iv = data.slice(0, AES_GCM_IV_LENGTH);
  const ciphertext = data.slice(AES_GCM_IV_LENGTH);

  const plaintext = await crypto.subtle.decrypt({ name: 'AES-GCM', iv }, channelKey, ciphertext);

  return new TextDecoder().decode(plaintext);
}

// ========================================
// Encrypted User Preferences (PBKDF2 — legacy, kept for migration)
// ========================================

/**
 * Derive an AES-256-GCM key for encrypting user preferences.
 * Uses the same PBKDF2 parameters as deriveKeyFromPassword but with a
 * domain-separated salt to ensure cryptographic independence from the wrapping key.
 */
export async function derivePreferencesKey(password: string, salt: Uint8Array): Promise<CryptoKey> {
  const encoder = new TextEncoder();

  // Domain-separate: append a fixed suffix to the salt
  const separator = encoder.encode(PREFERENCES_DOMAIN_SEPARATOR);
  const domainSalt = new Uint8Array(salt.length + separator.length);
  domainSalt.set(salt, 0);
  domainSalt.set(separator, salt.length);

  const passwordKey = await crypto.subtle.importKey(
    'raw',
    encoder.encode(password),
    'PBKDF2',
    false,
    ['deriveKey']
  );

  return crypto.subtle.deriveKey(
    {
      name: 'PBKDF2',
      salt: domainSalt,
      iterations: PBKDF2_ITERATIONS,
      hash: 'SHA-256',
    },
    passwordKey,
    {
      name: 'AES-GCM',
      length: DERIVED_KEY_LENGTH * 8,
    },
    false,
    ['encrypt', 'decrypt']
  );
}

/**
 * Encrypt a JSON-serializable object with AES-256-GCM.
 * Prepends 12-byte random IV. Returns base64.
 */
export async function encryptBlob<T>(data: T, key: CryptoKey): Promise<string> {
  const iv = crypto.getRandomValues(new Uint8Array(AES_GCM_IV_LENGTH));
  const encoded = new TextEncoder().encode(JSON.stringify(data));

  const ciphertext = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, key, encoded);

  const result = new Uint8Array(iv.length + ciphertext.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(ciphertext), iv.length);

  return arrayBufferToBase64(result.buffer);
}

/**
 * Decrypt a base64 blob (12-byte IV + AES-256-GCM ciphertext) back to a typed object.
 */
export async function decryptBlob<T>(ciphertextBase64: string, key: CryptoKey): Promise<T> {
  const data = new Uint8Array(base64ToArrayBuffer(ciphertextBase64));
  const iv = data.slice(0, AES_GCM_IV_LENGTH);
  const ciphertext = data.slice(AES_GCM_IV_LENGTH);

  const plaintext = await crypto.subtle.decrypt({ name: 'AES-GCM', iv }, key, ciphertext);

  return JSON.parse(new TextDecoder().decode(plaintext)) as T;
}

// ========================================
// Recovery Key (Account Recovery System — Issue #200)
// ========================================

const BASE58_ALPHABET = '123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz';

/**
 * Encode a Uint8Array as a base58 string (Bitcoin-style alphabet).
 * Pure math — no external dependencies.
 */
export function base58Encode(bytes: Uint8Array): string {
  // Count leading zero bytes (they map to '1' in base58)
  let leadingZeros = 0;
  for (let i = 0; i < bytes.length && bytes[i] === 0; i++) {
    leadingZeros++;
  }

  // Convert byte array to a big integer (big-endian)
  let num = 0n;
  for (const byte of bytes) {
    num = num * 256n + BigInt(byte);
  }

  // Convert big integer to base58
  let result = '';
  while (num > 0n) {
    const remainder = Number(num % 58n);
    num = num / 58n;
    result = BASE58_ALPHABET[remainder] + result;
  }

  // Prepend '1' for each leading zero byte
  return BASE58_ALPHABET[0].repeat(leadingZeros) + result;
}

/**
 * Decode a base58 string back to a Uint8Array.
 * Throws on invalid characters.
 */
export function base58Decode(str: string): Uint8Array {
  // Count leading '1' characters (they map to 0x00 bytes)
  let leadingOnes = 0;
  for (let i = 0; i < str.length && str[i] === '1'; i++) {
    leadingOnes++;
  }

  // Convert base58 string to a big integer
  let num = 0n;
  for (const char of str) {
    const index = BASE58_ALPHABET.indexOf(char);
    if (index === -1) {
      throw new Error(`Invalid base58 character: ${char}`);
    }
    num = num * 58n + BigInt(index);
  }

  // Convert big integer to byte array (big-endian)
  const tempBytes: number[] = [];
  while (num > 0n) {
    tempBytes.unshift(Number(num % 256n));
    num = num / 256n;
  }

  // Prepend zero bytes for leading '1' characters
  const result = new Uint8Array(leadingOnes + tempBytes.length);
  // Leading zeros are already 0 in a new Uint8Array
  result.set(tempBytes, leadingOnes);

  return result;
}

/**
 * Generate a human-readable recovery key.
 * 32 random bytes → base58 → grouped in chunks of 5 separated by dashes.
 */
export function generateRecoveryKey(): string {
  const bytes = crypto.getRandomValues(new Uint8Array(32));
  const encoded = base58Encode(bytes);
  // Group into chunks of 5 separated by dashes for readability
  return encoded.match(/.{1,5}/g)?.join('-') ?? encoded;
}

/**
 * Parse a recovery key string back to raw bytes.
 * Trims whitespace, strips dashes/spaces, decodes base58,
 * and validates the result is exactly 32 bytes.
 */
export function parseRecoveryKey(key: string): Uint8Array {
  const cleaned = key.trim().replaceAll(/[\s-]/g, '');
  const bytes = base58Decode(cleaned);
  if (bytes.length !== 32) {
    throw new Error(`Invalid recovery key: expected 32 bytes after decoding, got ${bytes.length}`);
  }
  return bytes;
}

/**
 * Derive an AES-GCM key from raw recovery key bytes using Argon2id.
 * Same parameters as password derivation but accepts raw bytes instead of a string.
 */
async function deriveKeyFromRecoveryBytes(
  keyBytes: Uint8Array,
  salt: Uint8Array
): Promise<CryptoKey> {
  const hash = await argon2id({
    password: keyBytes,
    salt,
    parallelism: ARGON2_PARALLELISM,
    iterations: ARGON2_ITERATIONS,
    memorySize: ARGON2_MEMORY,
    hashLength: ARGON2_HASH_LENGTH,
    outputType: 'binary',
  });

  return crypto.subtle.importKey(
    'raw',
    hash as BufferSource,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt']
  );
}

/**
 * Wrap the user's private key with a recovery-key-derived key.
 *
 * Since the in-memory private key is non-extractable, we cannot use
 * crypto.subtle.wrapKey(). Instead we decrypt the password-wrapped blob
 * to get raw PKCS8 bytes, then re-encrypt with the recovery-derived key.
 *
 * @param wrappedPrivateKeyBase64 - The password-wrapped private key blob (IV + ciphertext)
 * @param passwordWrappingKey - The password-derived AES-GCM wrapping key
 * @param recoveryKeyBase58 - The base58 recovery key (with or without dashes)
 * @returns The recovery-wrapped key blob and its salt (both base64)
 */
export async function wrapWithRecoveryKey(
  wrappedPrivateKeyBase64: string,
  passwordWrappingKey: CryptoKey,
  recoveryKeyBase58: string
): Promise<{ wrappedKey: string; salt: string }> {
  // 1. Decrypt the password-wrapped blob to get raw PKCS8 bytes
  const wrappedData = base64ToArrayBuffer(wrappedPrivateKeyBase64);
  const wrappedBytes = new Uint8Array(wrappedData);
  const iv = wrappedBytes.slice(0, 12);
  const ciphertext = wrappedBytes.slice(12);

  // Use unwrapKey (which the wrapping key supports) instead of decrypt
  const privateKeyForExport = await crypto.subtle.unwrapKey(
    'pkcs8',
    ciphertext,
    passwordWrappingKey,
    { name: 'AES-GCM', iv },
    { name: 'RSA-OAEP', hash: 'SHA-256' },
    true, // extractable so we can re-export
    ['decrypt']
  );
  const rawPkcs8 = await crypto.subtle.exportKey('pkcs8', privateKeyForExport);

  // 2. Derive recovery wrapping key
  const recoveryBytes = parseRecoveryKey(recoveryKeyBase58);
  const salt = generateSalt(); // 16 bytes
  const recoveryKey = await deriveKeyFromRecoveryBytes(recoveryBytes, salt);

  // 3. Encrypt PKCS8 bytes with recovery key
  const recoveryIv = crypto.getRandomValues(new Uint8Array(12));
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv: recoveryIv },
    recoveryKey,
    rawPkcs8
  );

  // 4. Prepend IV (same format as password wrapping)
  const result = new Uint8Array(12 + encrypted.byteLength);
  result.set(recoveryIv, 0);
  result.set(new Uint8Array(encrypted), 12);

  return {
    wrappedKey: arrayBufferToBase64(result.buffer),
    salt: arrayBufferToBase64(salt.buffer as ArrayBuffer),
  };
}

/**
 * Unwrap a private key using a recovery key.
 * Reverse of wrapWithRecoveryKey — decrypts the recovery-wrapped blob
 * back to raw PKCS8 bytes.
 *
 * @param recoveryWrappedKeyBase64 - The recovery-wrapped key blob (IV + ciphertext)
 * @param saltBase64 - The salt used during recovery key wrapping
 * @param recoveryKeyBase58 - The base58 recovery key (with or without dashes)
 * @returns Raw PKCS8 bytes of the private key
 */
export async function unwrapWithRecoveryKey(
  recoveryWrappedKeyBase64: string,
  saltBase64: string,
  recoveryKeyBase58: string
): Promise<ArrayBuffer> {
  const recoveryBytes = parseRecoveryKey(recoveryKeyBase58);
  const salt = new Uint8Array(base64ToArrayBuffer(saltBase64));
  const recoveryKey = await deriveKeyFromRecoveryBytes(recoveryBytes, salt);

  const wrappedData = base64ToArrayBuffer(recoveryWrappedKeyBase64);
  const wrappedBytes = new Uint8Array(wrappedData);
  const iv = wrappedBytes.slice(0, 12);
  const ciphertext = wrappedBytes.slice(12);

  return crypto.subtle.decrypt({ name: 'AES-GCM', iv }, recoveryKey, ciphertext);
}

/**
 * Wrap the preferences key with a recovery-key-derived key.
 * Uses a SEPARATE salt from the private key wrapping to maintain
 * cryptographic independence between the two wrapped keys.
 *
 * @param prefsKeyBase64 - The preferences key exported as base64 raw bytes
 * @param recoveryKeyBase58 - The base58 recovery key (with or without dashes)
 * @returns The recovery-wrapped preferences key and its salt (both base64)
 */
export async function wrapPrefsKeyWithRecoveryKey(
  prefsKeyBase64: string,
  recoveryKeyBase58: string
): Promise<{ wrappedKey: string; salt: string }> {
  const prefsKeyBytes = base64ToArrayBuffer(prefsKeyBase64);
  const recoveryBytes = parseRecoveryKey(recoveryKeyBase58);
  const salt = generateSalt();
  const recoveryKey = await deriveKeyFromRecoveryBytes(recoveryBytes, salt);

  const iv = crypto.getRandomValues(new Uint8Array(12));
  const encrypted = await crypto.subtle.encrypt(
    { name: 'AES-GCM', iv },
    recoveryKey,
    prefsKeyBytes
  );

  const result = new Uint8Array(12 + encrypted.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(encrypted), 12);

  return {
    wrappedKey: arrayBufferToBase64(result.buffer),
    salt: arrayBufferToBase64(salt.buffer as ArrayBuffer),
  };
}

/**
 * Unwrap the preferences key using a recovery key.
 * Reverse of wrapPrefsKeyWithRecoveryKey — decrypts and re-imports
 * the preferences key as a non-extractable CryptoKey.
 *
 * @param recoveryWrappedPrefsBase64 - The recovery-wrapped preferences key blob
 * @param saltBase64 - The salt used during recovery wrapping
 * @param recoveryKeyBase58 - The base58 recovery key (with or without dashes)
 * @returns Non-extractable AES-GCM CryptoKey for encrypt/decrypt
 */
export async function unwrapPrefsKeyWithRecoveryKey(
  recoveryWrappedPrefsBase64: string,
  saltBase64: string,
  recoveryKeyBase58: string
): Promise<CryptoKey> {
  const recoveryBytes = parseRecoveryKey(recoveryKeyBase58);
  const salt = new Uint8Array(base64ToArrayBuffer(saltBase64));
  const recoveryKey = await deriveKeyFromRecoveryBytes(recoveryBytes, salt);

  const wrappedData = base64ToArrayBuffer(recoveryWrappedPrefsBase64);
  const wrappedBytes = new Uint8Array(wrappedData);
  const iv = wrappedBytes.slice(0, 12);
  const ciphertext = wrappedBytes.slice(12);

  const rawPrefsKey = await crypto.subtle.decrypt({ name: 'AES-GCM', iv }, recoveryKey, ciphertext);

  return crypto.subtle.importKey('raw', rawPrefsKey, { name: 'AES-GCM', length: 256 }, false, [
    'encrypt',
    'decrypt',
  ]);
}

// ── ECDH Key Exchange (Phase B: Trusted Device Recovery) ──────────────────

export async function generateECDHKeyPair(): Promise<CryptoKeyPair> {
  return crypto.subtle.generateKey(
    { name: 'ECDH', namedCurve: 'P-256' },
    true, // extractable for transport
    ['deriveKey']
  );
}

export async function exportECDHPublicKey(key: CryptoKey): Promise<string> {
  const raw = await crypto.subtle.exportKey('raw', key);
  return arrayBufferToBase64(raw);
}

export async function importECDHPublicKey(base64: string): Promise<CryptoKey> {
  const raw = base64ToArrayBuffer(base64);
  return crypto.subtle.importKey('raw', raw, { name: 'ECDH', namedCurve: 'P-256' }, false, []);
}

export async function deriveSharedSecret(
  privateKey: CryptoKey,
  peerPublicKey: CryptoKey
): Promise<CryptoKey> {
  return crypto.subtle.deriveKey(
    { name: 'ECDH', public: peerPublicKey },
    privateKey,
    { name: 'AES-GCM', length: 256 },
    false,
    ['encrypt', 'decrypt']
  );
}

export async function encryptWithSharedSecret(
  sharedKey: CryptoKey,
  data: ArrayBuffer
): Promise<string> {
  const iv = crypto.getRandomValues(new Uint8Array(12));
  const encrypted = await crypto.subtle.encrypt({ name: 'AES-GCM', iv }, sharedKey, data);
  const result = new Uint8Array(12 + encrypted.byteLength);
  result.set(iv, 0);
  result.set(new Uint8Array(encrypted), 12);
  return arrayBufferToBase64(result.buffer);
}

export async function decryptWithSharedSecret(
  sharedKey: CryptoKey,
  encryptedBase64: string
): Promise<ArrayBuffer> {
  const data = new Uint8Array(base64ToArrayBuffer(encryptedBase64));
  const iv = data.slice(0, 12);
  const ciphertext = data.slice(12);
  return crypto.subtle.decrypt({ name: 'AES-GCM', iv }, sharedKey, ciphertext);
}
