// @vitest-environment node
//
// secureStorage — the gate that keeps safeStorage persistence on Linux's
// keyring-backed keys only (v11, v12): never the hardcoded PosixKeyProvider
// fallback (v10), and never a tag nobody has verified at source.

import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

const { mockIsAsyncAvailable, mockEncrypt, mockBackend } = vi.hoisted(() => ({
  mockIsAsyncAvailable: vi.fn(async () => true),
  mockEncrypt: vi.fn(async (s: string) => Buffer.from(`v11${s}`)),
  mockBackend: vi.fn(() => 'gnome_libsecret'),
}));

vi.mock('electron', () => ({
  safeStorage: {
    isAsyncEncryptionAvailable: mockIsAsyncAvailable,
    encryptStringAsync: mockEncrypt,
    getSelectedStorageBackend: mockBackend,
  },
}));

import {
  encryptForDisk,
  isSecureStorageAvailable,
  isUnprotectedCiphertext,
} from '@/main/secureStorage';

const originalPlatform = process.platform;

function setPlatform(platform: NodeJS.Platform) {
  Object.defineProperty(process, 'platform', { value: platform, configurable: true });
}

describe('secureStorage', () => {
  beforeEach(() => {
    mockIsAsyncAvailable.mockReset();
    mockIsAsyncAvailable.mockResolvedValue(true);
    mockEncrypt.mockReset();
    mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v11${s}`));
    mockBackend.mockReset();
    mockBackend.mockReturnValue('gnome_libsecret');
  });

  afterEach(() => {
    setPlatform(originalPlatform);
  });

  describe('encryptForDisk', () => {
    it('returns keyring ciphertext on Linux', async () => {
      setPlatform('linux');
      const encrypted = await encryptForDisk('secret');
      expect(encrypted?.toString('latin1')).toBe('v11secret');
    });

    it('refuses the basic_text backend without encrypting', async () => {
      setPlatform('linux');
      mockBackend.mockReturnValue('basic_text');
      await expect(encryptForDisk('secret')).resolves.toBeNull();
      expect(mockEncrypt).not.toHaveBeenCalled();
    });

    it('refuses v10 ciphertext on Linux (keyring failed, hardcoded key encrypted)', async () => {
      setPlatform('linux');
      mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v10${s}`));
      await expect(encryptForDisk('secret')).resolves.toBeNull();
    });

    it('accepts v12 (Secret Portal) ciphertext on Linux', async () => {
      setPlatform('linux');
      mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v12${s}`));
      const encrypted = await encryptForDisk('secret');
      expect(encrypted?.toString('latin1')).toBe('v12secret');
    });

    it('refuses a tag nobody has verified on Linux, failing closed', async () => {
      setPlatform('linux');
      mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v13${s}`));
      await expect(encryptForDisk('secret')).resolves.toBeNull();
    });

    it.each(['darwin', 'win32'] as const)(
      'keeps v10 ciphertext on %s, where v10 is an OS-protected key',
      async (platform) => {
        setPlatform(platform);
        mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v10${s}`));
        const encrypted = await encryptForDisk('secret');
        expect(encrypted?.toString('latin1')).toBe('v10secret');
        // getSelectedStorageBackend is Linux-only in Electron; never called elsewhere.
        expect(mockBackend).not.toHaveBeenCalled();
      }
    );

    it('returns null when the async encryptor is unavailable', async () => {
      setPlatform('linux');
      mockIsAsyncAvailable.mockResolvedValue(false);
      await expect(encryptForDisk('secret')).resolves.toBeNull();
      expect(mockEncrypt).not.toHaveBeenCalled();
    });

    it('rejects when the encryptor fails', async () => {
      setPlatform('darwin');
      mockEncrypt.mockRejectedValue(new Error('Error while encrypting'));
      await expect(encryptForDisk('secret')).rejects.toThrow('Error while encrypting');
    });
  });

  describe('isUnprotectedCiphertext', () => {
    it('accepts only the Linux keyring tags on Linux', () => {
      setPlatform('linux');
      expect(isUnprotectedCiphertext(Buffer.from('v11real'))).toBe(false);
      expect(isUnprotectedCiphertext(Buffer.from('v12real'))).toBe(false);
      expect(isUnprotectedCiphertext(Buffer.from('v10forged'))).toBe(true);
      expect(isUnprotectedCiphertext(Buffer.from('v13unknown'))).toBe(true);
      expect(isUnprotectedCiphertext(Buffer.from(''))).toBe(true);
    });

    it.each(['darwin', 'win32'] as const)('flags nothing on %s', (platform) => {
      setPlatform(platform);
      expect(isUnprotectedCiphertext(Buffer.from('v10keychain'))).toBe(false);
      expect(isUnprotectedCiphertext(Buffer.from('v13unknown'))).toBe(false);
    });
  });

  describe('isSecureStorageAvailable', () => {
    it('is true when the probe encrypts under an OS-protected key', async () => {
      setPlatform('linux');
      await expect(isSecureStorageAvailable()).resolves.toBe(true);
    });

    it('is false under basic_text', async () => {
      setPlatform('linux');
      mockBackend.mockReturnValue('basic_text');
      await expect(isSecureStorageAvailable()).resolves.toBe(false);
    });

    it('is false under the Linux hardcoded-key fallback', async () => {
      setPlatform('linux');
      mockEncrypt.mockImplementation(async (s: string) => Buffer.from(`v10${s}`));
      await expect(isSecureStorageAvailable()).resolves.toBe(false);
    });

    it('is false, not a rejection, when the encryptor fails', async () => {
      setPlatform('darwin');
      mockEncrypt.mockRejectedValue(new Error('Error while encrypting'));
      await expect(isSecureStorageAvailable()).resolves.toBe(false);
    });
  });
});
