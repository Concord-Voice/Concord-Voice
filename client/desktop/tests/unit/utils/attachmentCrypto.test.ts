import { describe, it, expect } from 'vitest';
import {
  encryptFile,
  decryptFile,
  classifyFileType,
  formatFileSize,
  isImageType,
  MAX_FILE_SIZE,
  MAX_ATTACHMENTS,
} from '@/renderer/utils/attachmentCrypto';

describe('attachmentCrypto', () => {
  // Generate a test AES-256-GCM key
  async function generateTestKey(): Promise<CryptoKey> {
    return crypto.subtle.generateKey({ name: 'AES-GCM', length: 256 }, true, [
      'encrypt',
      'decrypt',
    ]);
  }

  describe('encryptFile / decryptFile', () => {
    it('encrypts and decrypts a file roundtrip', async () => {
      const key = await generateTestKey();
      const originalData = new TextEncoder().encode('Hello, file content!').buffer;

      const encrypted = await encryptFile(originalData, key);
      expect(encrypted.byteLength).toBeGreaterThan(originalData.byteLength);

      const decrypted = await decryptFile(encrypted, key);
      const decryptedText = new TextDecoder().decode(decrypted);
      expect(decryptedText).toBe('Hello, file content!');
    });

    it('prepends 12-byte IV to ciphertext', async () => {
      const key = await generateTestKey();
      const data = new Uint8Array(10).buffer;

      const encrypted = await encryptFile(data, key);
      // IV (12 bytes) + ciphertext (10 bytes data + 16 bytes GCM tag)
      expect(encrypted.byteLength).toBe(12 + 10 + 16);
    });

    it('works with empty data', async () => {
      const key = await generateTestKey();
      const data = new ArrayBuffer(0);

      const encrypted = await encryptFile(data, key);
      // IV (12) + GCM tag (16) = 28 bytes minimum
      expect(encrypted.byteLength).toBe(28);

      const decrypted = await decryptFile(encrypted, key);
      expect(decrypted.byteLength).toBe(0);
    });

    it('works with large data', async () => {
      const key = await generateTestKey();
      const data = new Uint8Array(1024 * 1024).buffer; // 1 MB

      const encrypted = await encryptFile(data, key);
      const decrypted = await decryptFile(encrypted, key);
      expect(decrypted.byteLength).toBe(1024 * 1024);
    });

    it('fails decryption with wrong key', async () => {
      const key1 = await generateTestKey();
      const key2 = await generateTestKey();
      const data = new TextEncoder().encode('secret').buffer;

      const encrypted = await encryptFile(data, key1);

      await expect(decryptFile(encrypted, key2)).rejects.toThrow();
    });

    it('produces different ciphertext for same plaintext (random IV)', async () => {
      const key = await generateTestKey();
      const data = new TextEncoder().encode('same content').buffer;

      const encrypted1 = await encryptFile(data, key);
      const encrypted2 = await encryptFile(data, key);

      // IVs should differ (first 12 bytes)
      const iv1 = new Uint8Array(encrypted1).slice(0, 12);
      const iv2 = new Uint8Array(encrypted2).slice(0, 12);
      expect(iv1).not.toEqual(iv2);
    });
  });

  describe('classifyFileType', () => {
    it('classifies image types as photo', () => {
      expect(classifyFileType('image/png')).toBe('photo');
      expect(classifyFileType('image/jpeg')).toBe('photo');
      expect(classifyFileType('image/webp')).toBe('photo');
      expect(classifyFileType('image/heic')).toBe('photo');
    });

    it('classifies animated types', () => {
      expect(classifyFileType('image/gif')).toBe('animated');
      expect(classifyFileType('image/apng')).toBe('animated');
    });

    it('classifies video types', () => {
      expect(classifyFileType('video/mp4')).toBe('video');
      expect(classifyFileType('video/webm')).toBe('video');
      expect(classifyFileType('video/quicktime')).toBe('video');
    });

    it('classifies audio types', () => {
      expect(classifyFileType('audio/mpeg')).toBe('audio');
      expect(classifyFileType('audio/ogg')).toBe('audio');
      expect(classifyFileType('audio/wav')).toBe('audio');
    });

    it('classifies unknown types as file', () => {
      expect(classifyFileType('application/pdf')).toBe('file');
      expect(classifyFileType('text/plain')).toBe('file');
      expect(classifyFileType('application/zip')).toBe('file');
    });

    it('handles case-insensitive matching', () => {
      expect(classifyFileType('Image/PNG')).toBe('photo');
      expect(classifyFileType('VIDEO/MP4')).toBe('video');
    });
  });

  describe('formatFileSize', () => {
    it('formats bytes', () => {
      expect(formatFileSize(500)).toBe('500 B');
    });

    it('formats kilobytes', () => {
      expect(formatFileSize(1024)).toBe('1.0 KB');
      expect(formatFileSize(5120)).toBe('5.0 KB');
    });

    it('formats megabytes', () => {
      expect(formatFileSize(1024 * 1024)).toBe('1.0 MB');
      expect(formatFileSize(25 * 1024 * 1024)).toBe('25.0 MB');
    });
  });

  describe('isImageType', () => {
    it('returns true for image types', () => {
      expect(isImageType('image/png')).toBe(true);
      expect(isImageType('image/jpeg')).toBe(true);
      expect(isImageType('image/gif')).toBe(true);
    });

    it('returns false for non-image types', () => {
      expect(isImageType('video/mp4')).toBe(false);
      expect(isImageType('application/pdf')).toBe(false);
    });
  });

  describe('constants', () => {
    it('MAX_FILE_SIZE is 25 MiB', () => {
      expect(MAX_FILE_SIZE).toBe(26_214_400);
    });

    it('MAX_ATTACHMENTS is 5', () => {
      expect(MAX_ATTACHMENTS).toBe(5);
    });
  });
});
