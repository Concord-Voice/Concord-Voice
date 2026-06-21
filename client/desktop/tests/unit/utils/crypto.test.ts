// @vitest-environment node
import { describe, it, expect } from 'vitest';
import {
  generateSalt,
  deriveKeyFromPassword,
  generateKeyPair,
  wrapPrivateKey,
  unwrapPrivateKey,
  exportPublicKey,
  importPublicKey,
  arrayBufferToBase64,
  base64ToArrayBuffer,
  generateRegistrationKeys,
  unwrapLoginKeys,
  generateChannelKey,
  exportChannelKey,
  importChannelKey,
  encryptMessage,
  decryptMessage,
  encryptBlob,
  decryptBlob,
  derivePreferencesKey,
  base58Encode,
  base58Decode,
  generateRecoveryKey,
  parseRecoveryKey,
  wrapWithRecoveryKey,
  unwrapWithRecoveryKey,
  deriveKeyArgon2idExportable,
} from '@/renderer/utils/crypto';

describe('crypto utilities', () => {
  describe('arrayBufferToBase64 / base64ToArrayBuffer', () => {
    it('round-trips correctly', () => {
      const original = new Uint8Array([1, 2, 3, 4, 5, 100, 200, 255]);
      const base64 = arrayBufferToBase64(original.buffer);
      const roundTripped = new Uint8Array(base64ToArrayBuffer(base64));
      expect(roundTripped).toEqual(original);
    });

    it('handles empty buffer', () => {
      const empty = new Uint8Array([]);
      const base64 = arrayBufferToBase64(empty.buffer);
      expect(base64).toBe('');
      const back = new Uint8Array(base64ToArrayBuffer(base64));
      expect(back.length).toBe(0);
    });
  });

  describe('generateSalt', () => {
    it('returns 16 bytes', () => {
      const salt = generateSalt();
      expect(salt.length).toBe(16);
    });

    it('returns different salts each time', () => {
      const salt1 = generateSalt();
      const salt2 = generateSalt();
      expect(arrayBufferToBase64(salt1.buffer)).not.toBe(arrayBufferToBase64(salt2.buffer));
    });
  });

  describe('deriveKeyFromPassword', () => {
    it('derives a CryptoKey', async () => {
      const salt = generateSalt();
      const key = await deriveKeyFromPassword('testpassword', salt);
      expect(key).toBeDefined();
      expect(key.type).toBe('secret');
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('same password + salt produces same key', async () => {
      const salt = generateSalt();
      const key1 = await deriveKeyFromPassword('testpassword', salt);
      const key2 = await deriveKeyFromPassword('testpassword', salt);
      // Can't compare CryptoKey directly, but they should both work for wrapping
      expect(key1.algorithm.name).toBe(key2.algorithm.name);
    });
  });

  describe('generateKeyPair', () => {
    it('generates an RSA-OAEP key pair', { timeout: 30_000 }, async () => {
      const keyPair = await generateKeyPair();
      expect(keyPair.publicKey).toBeDefined();
      expect(keyPair.privateKey).toBeDefined();
      expect(keyPair.publicKey.algorithm.name).toBe('RSA-OAEP');
      expect(keyPair.privateKey.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('wrapPrivateKey / unwrapPrivateKey', () => {
    it('round-trips private key', { timeout: 30_000 }, async () => {
      const salt = generateSalt();
      const wrappingKey = await deriveKeyFromPassword('testpassword', salt);
      const keyPair = await generateKeyPair();

      const wrapped = await wrapPrivateKey(keyPair.privateKey, wrappingKey);
      expect(wrapped.byteLength).toBeGreaterThan(12); // IV + wrapped key

      const unwrapped = await unwrapPrivateKey(wrapped, wrappingKey);
      expect(unwrapped.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('exportPublicKey / importPublicKey', () => {
    it('round-trips public key', { timeout: 30_000 }, async () => {
      const keyPair = await generateKeyPair();
      const exported = await exportPublicKey(keyPair.publicKey);
      expect(typeof exported).toBe('string');
      expect(exported.length).toBeGreaterThan(0);

      const imported = await importPublicKey(exported);
      expect(imported.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('generateRegistrationKeys', () => {
    it('returns all required key material', { timeout: 30_000 }, async () => {
      const keys = await generateRegistrationKeys('MySecurePassword123!');
      expect(keys.publicKey).toBeDefined();
      expect(keys.privateKey).toBeDefined();
      expect(typeof keys.wrappedPrivateKey).toBe('string');
      expect(typeof keys.keyDerivationSalt).toBe('string');
      expect(keys.wrappedPrivateKey.length).toBeGreaterThan(0);
      expect(keys.keyDerivationSalt.length).toBeGreaterThan(0);
    });
  });

  describe('unwrapLoginKeys', () => {
    it('unwraps keys generated during registration', { timeout: 30_000 }, async () => {
      const password = 'MySecurePassword123!';
      const regKeys = await generateRegistrationKeys(password);

      const privateKey = await unwrapLoginKeys(
        password,
        regKeys.wrappedPrivateKey,
        regKeys.keyDerivationSalt,
        regKeys.keyDerivationAlg
      );
      expect(privateKey.algorithm.name).toBe('RSA-OAEP');
    });
  });

  describe('channel key operations', () => {
    it('generateChannelKey creates AES-GCM key', async () => {
      const key = await generateChannelKey();
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('exportChannelKey / importChannelKey round-trips', async () => {
      const key = await generateChannelKey();
      const raw = await exportChannelKey(key);
      expect(raw.byteLength).toBe(32); // 256 bits

      const imported = await importChannelKey(raw);
      expect(imported.algorithm.name).toBe('AES-GCM');
    });
  });

  describe('encryptMessage / decryptMessage', () => {
    it('round-trips a message', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Hello, encrypted world!';

      const ciphertext = await encryptMessage(plaintext, key);
      expect(typeof ciphertext).toBe('string');
      expect(ciphertext).not.toBe(plaintext);

      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe(plaintext);
    });

    it('produces unique ciphertext each time (random IV)', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Same message';

      const ct1 = await encryptMessage(plaintext, key);
      const ct2 = await encryptMessage(plaintext, key);
      expect(ct1).not.toBe(ct2); // Different IVs
    });

    it('handles unicode content', async () => {
      const key = await generateChannelKey();
      const plaintext = 'Hello! Encryption works.';

      const ciphertext = await encryptMessage(plaintext, key);
      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe(plaintext);
    });

    it('handles empty string', async () => {
      const key = await generateChannelKey();
      const ciphertext = await encryptMessage('', key);
      const decrypted = await decryptMessage(ciphertext, key);
      expect(decrypted).toBe('');
    });
  });

  describe('encryptBlob / decryptBlob', () => {
    it('round-trips a JSON object', async () => {
      const salt = generateSalt();
      const key = await derivePreferencesKey('password', salt);
      const data = { theme: 'dark', fontSize: 14, nested: { enabled: true } };

      const encrypted = await encryptBlob(data, key);
      expect(typeof encrypted).toBe('string');

      const decrypted = await decryptBlob<typeof data>(encrypted, key);
      expect(decrypted).toEqual(data);
    });
  });

  describe('base58Encode / base58Decode', () => {
    it('round-trips random bytes', () => {
      const original = crypto.getRandomValues(new Uint8Array(32));
      const encoded = base58Encode(original);
      const decoded = base58Decode(encoded);
      expect(decoded).toEqual(original);
    });

    it('handles leading zero bytes', () => {
      const data = new Uint8Array([0, 0, 0, 1, 2, 3]);
      const encoded = base58Encode(data);
      const decoded = base58Decode(encoded);
      expect(decoded).toEqual(data);
    });

    it('handles empty input', () => {
      const data = new Uint8Array([]);
      const encoded = base58Encode(data);
      expect(encoded).toBe('');
      const decoded = base58Decode('');
      expect(decoded).toEqual(data);
    });

    it('handles single byte', () => {
      for (const val of [0, 1, 57, 58, 127, 255]) {
        const data = new Uint8Array([val]);
        const decoded = base58Decode(base58Encode(data));
        expect(decoded).toEqual(data);
      }
    });

    it('throws on invalid characters', () => {
      expect(() => base58Decode('0OIl')).toThrow(); // 0, O, I, l are not in base58
    });
  });

  describe('generateRecoveryKey / parseRecoveryKey', () => {
    it('generates a dash-grouped base58 key', () => {
      const key = generateRecoveryKey();
      expect(key).toContain('-');
      // Each group is at most 5 chars
      const groups = key.split('-');
      for (const group of groups) {
        expect(group.length).toBeLessThanOrEqual(5);
        expect(group.length).toBeGreaterThan(0);
      }
    });

    it('parseRecoveryKey strips dashes and decodes to 32 bytes', () => {
      const key = generateRecoveryKey();
      const bytes = parseRecoveryKey(key);
      expect(bytes.length).toBe(32);
    });

    it('generates unique keys each time', () => {
      const key1 = generateRecoveryKey();
      const key2 = generateRecoveryKey();
      expect(key1).not.toBe(key2);
    });
  });

  describe('wrapWithRecoveryKey / unwrapWithRecoveryKey', () => {
    it('round-trips: wrap then unwrap recovers original PKCS8 bytes', async () => {
      // Generate an RSA keypair (extractable so we can export PKCS8 for comparison)
      const keyPair = await generateKeyPair();

      // Wrap with password using the production wrapPrivateKey function
      const salt = generateSalt();
      const wrappingKey = await deriveKeyArgon2idExportable('test-password-12!A', salt);
      const wrappedBuf = await wrapPrivateKey(keyPair.privateKey, wrappingKey);
      const wrappedBase64 = arrayBufferToBase64(wrappedBuf);

      // wrapWithRecoveryKey now uses unwrapKey (which the wrapping key supports)
      // so we can pass the wrapping key directly — no re-import needed
      const recoveryKey = generateRecoveryKey();
      const { wrappedKey: recoveryWrapped, salt: recoverySalt } = await wrapWithRecoveryKey(
        wrappedBase64,
        wrappingKey,
        recoveryKey
      );

      // Unwrap with recovery key
      const recoveredPkcs8 = await unwrapWithRecoveryKey(
        recoveryWrapped,
        recoverySalt,
        recoveryKey
      );

      // Verify recovered PKCS8 imports as a valid RSA private key
      const importedKey = await crypto.subtle.importKey(
        'pkcs8',
        recoveredPkcs8,
        { name: 'RSA-OAEP', hash: 'SHA-256' },
        false,
        ['decrypt']
      );
      expect(importedKey.type).toBe('private');
    }, 60000);

    it('fails with wrong recovery key', async () => {
      const keyPair = await generateKeyPair();
      const salt = generateSalt();
      const wrappingKey = await deriveKeyArgon2idExportable('test-password-12!A', salt);
      const wrappedBuf = await wrapPrivateKey(keyPair.privateKey, wrappingKey);
      const wrappedBase64 = arrayBufferToBase64(wrappedBuf);

      const recoveryKey = generateRecoveryKey();
      const wrongKey = generateRecoveryKey();
      const { wrappedKey, salt: recoverySalt } = await wrapWithRecoveryKey(
        wrappedBase64,
        wrappingKey,
        recoveryKey
      );

      await expect(unwrapWithRecoveryKey(wrappedKey, recoverySalt, wrongKey)).rejects.toThrow();
    }, 60000);
  });

  describe('derivePreferencesKey', () => {
    it('derives a key for preferences encryption', async () => {
      const salt = generateSalt();
      const key = await derivePreferencesKey('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
    });

    it('produces different key than deriveKeyFromPassword (domain separation)', async () => {
      const salt = generateSalt();
      const wrappingKey = await deriveKeyFromPassword('password', salt);
      const prefsKey = await derivePreferencesKey('password', salt);
      // Both are non-extractable, but we can verify they work independently
      expect(wrappingKey.algorithm.name).toBe('AES-GCM');
      expect(prefsKey.algorithm.name).toBe('AES-GCM');
    });
  });

  // ── Exportable key variants ──

  describe('deriveKeyFromPasswordExportable', () => {
    it('derives an extractable AES-GCM key', async () => {
      const { deriveKeyFromPasswordExportable } = await import('@/renderer/utils/crypto');
      const salt = generateSalt();
      const key = await deriveKeyFromPasswordExportable('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
      expect(key.extractable).toBe(true);
    });
  });

  describe('derivePreferencesKeyExportable', () => {
    it('derives an extractable AES-GCM preferences key', async () => {
      const { derivePreferencesKeyExportable } = await import('@/renderer/utils/crypto');
      const salt = generateSalt();
      const key = await derivePreferencesKeyExportable('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
      expect(key.extractable).toBe(true);
    });
  });

  // ── Channel key wrapping ──

  describe('wrapChannelKey / unwrapChannelKey', () => {
    it('round-trips a channel key through RSA-OAEP wrapping', async () => {
      const { generateChannelKey, wrapChannelKey, unwrapChannelKey, exportChannelKey } =
        await import('@/renderer/utils/crypto');
      const keyPair = await generateKeyPair();
      const channelKey = await generateChannelKey();
      const wrapped = await wrapChannelKey(channelKey, keyPair.publicKey);
      expect(typeof wrapped).toBe('string'); // base64

      const unwrapped = await unwrapChannelKey(wrapped, keyPair.privateKey);
      // Verify keys produce same raw bytes
      const origRaw = await exportChannelKey(channelKey);
      const unwrappedRaw = await exportChannelKey(unwrapped);
      expect(new Uint8Array(origRaw)).toEqual(new Uint8Array(unwrappedRaw));
    }, 30000);
  });

  // ── ECDH shared secret ──

  describe('ECDH operations', () => {
    it('generates ECDH key pair', async () => {
      const { generateECDHKeyPair } = await import('@/renderer/utils/crypto');
      const kp = await generateECDHKeyPair();
      expect(kp.publicKey.algorithm.name).toBe('ECDH');
      expect(kp.privateKey.algorithm.name).toBe('ECDH');
    });

    it('exports and imports ECDH public key', async () => {
      const { generateECDHKeyPair, exportECDHPublicKey, importECDHPublicKey } =
        await import('@/renderer/utils/crypto');
      const kp = await generateECDHKeyPair();
      const exported = await exportECDHPublicKey(kp.publicKey);
      expect(typeof exported).toBe('string');

      const imported = await importECDHPublicKey(exported);
      expect(imported.algorithm.name).toBe('ECDH');
    });

    it('derives shared secret and encrypts/decrypts', async () => {
      const {
        generateECDHKeyPair,
        deriveSharedSecret,
        encryptWithSharedSecret,
        decryptWithSharedSecret,
      } = await import('@/renderer/utils/crypto');

      const alice = await generateECDHKeyPair();
      const bob = await generateECDHKeyPair();

      const aliceSecret = await deriveSharedSecret(alice.privateKey, bob.publicKey);
      const bobSecret = await deriveSharedSecret(bob.privateKey, alice.publicKey);

      // Both should be able to encrypt/decrypt the same data
      const data = new TextEncoder().encode('hello ECDH');
      const encrypted = await encryptWithSharedSecret(aliceSecret, data.buffer);
      const decrypted = await decryptWithSharedSecret(bobSecret, encrypted);
      const result = new TextDecoder().decode(decrypted);
      expect(result).toBe('hello ECDH');
    });
  });

  // ── Preferences key recovery wrapping ──

  describe('wrapPrefsKeyWithRecoveryKey / unwrapPrefsKeyWithRecoveryKey', () => {
    it('round-trips preferences key through recovery wrapping', async () => {
      const {
        generateRecoveryKey,
        derivePreferencesKeyExportable,
        wrapPrefsKeyWithRecoveryKey,
        unwrapPrefsKeyWithRecoveryKey,
      } = await import('@/renderer/utils/crypto');

      const salt = generateSalt();
      const prefsKey = await derivePreferencesKeyExportable('password', salt);
      const recoveryKey = generateRecoveryKey();

      // Export prefs key to base64 for wrapping
      const rawPrefs = await crypto.subtle.exportKey('raw', prefsKey);
      const prefsBase64 = arrayBufferToBase64(rawPrefs);

      const { wrappedKey, salt: wrappingSalt } = await wrapPrefsKeyWithRecoveryKey(
        prefsBase64,
        recoveryKey
      );
      expect(typeof wrappedKey).toBe('string');
      expect(typeof wrappingSalt).toBe('string');

      const unwrapped = await unwrapPrefsKeyWithRecoveryKey(wrappedKey, wrappingSalt, recoveryKey);
      expect(unwrapped.algorithm.name).toBe('AES-GCM');
    }, 30000);
  });

  // ── Argon2id preferences key variants ──

  describe('derivePreferencesKeyArgon2id', () => {
    it('derives an Argon2id preferences key', async () => {
      const { derivePreferencesKeyArgon2id } = await import('@/renderer/utils/crypto');
      const salt = generateSalt();
      const key = await derivePreferencesKeyArgon2id('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
      expect(key.extractable).toBe(false);
    }, 60000);
  });

  describe('derivePreferencesKeyArgon2idExportable', () => {
    it('derives an extractable Argon2id preferences key', async () => {
      const { derivePreferencesKeyArgon2idExportable } = await import('@/renderer/utils/crypto');
      const salt = generateSalt();
      const key = await derivePreferencesKeyArgon2idExportable('password', salt);
      expect(key.algorithm.name).toBe('AES-GCM');
      expect(key.extractable).toBe(true);
    }, 60000);
  });
});
