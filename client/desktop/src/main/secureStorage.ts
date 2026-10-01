/**
 * Secure-storage gate for everything the main process persists through
 * Electron `safeStorage` (Remember-Me refresh tokens, E2EE session keys).
 *
 * Electron's async encryptor reports itself available even when the only key
 * it holds is Chromium's Linux fallback: `PosixKeyProvider`, a hardcoded
 * PBKDF2("peanuts") AES-128 key tagged `v10`. It becomes the encryption key
 * under `--password-store=basic` (`basic_text`) and whenever the keyring
 * provider fails to return a key. Anything encrypted with it is readable by
 * anyone who can read the file, so it counts as "secure storage unavailable":
 * Login and Register refuse to start (#197), and a write from a live session
 * leaves its credential in memory only.
 *
 * The sync API this replaced did not have the gap: under `basic_text` its
 * `isEncryptionAvailable()` returned false unless the app called
 * `setUsePlainTextEncryption(true)`, which Concord never does.
 *
 * Verified at source, Electron 44.4.3 / Chromium 152.0.7977.130:
 * `shell/browser/api/electron_api_safe_storage.cc` (availability resolves true
 * once the encryptor exists), `shell/browser/browser_process_impl.cc`
 * `CreateOSCryptAsync` (PosixKeyProvider registered at precedence 5 under
 * `IS_POSIX && !IS_MAC`; macOS registers only KeychainKeyProvider and Windows
 * only DPAPIKeyProvider, so neither has a hardcoded fallback),
 * `components/os_crypt/async/browser/os_crypt_async.cc` (the highest-precedence
 * provider that returned a key encrypts, so a failed keyring leaves PosixKeyProvider),
 * `components/os_crypt/async/browser/posix_key_provider.cc` (the fixed key, `v10`),
 * `freedesktop_secret_key_provider.cc` (`v11`), `secret_portal_key_provider.h` (`v12`).
 */

import { safeStorage } from 'electron';

// Linux keyring-backed providers' tags. Everything else is refused: the
// hardcoded `v10`, and any tag a future Chromium adds, so a new weak provider
// fails closed until someone verifies it at source and adds it here.
// Linux only: on macOS the Keychain provider also tags its (real) key `v10`.
const LINUX_KEYRING_TAGS: ReadonlySet<string> = new Set(['v11', 'v12']);

function isLinux(): boolean {
  return process.platform === 'linux';
}

/**
 * True for Linux ciphertext that is not under a keyring key. Never trust it on
 * read either: anyone who can write the file can forge a hardcoded-key blob,
 * and decrypting it would let `shouldReEncrypt` re-save the forgery under the
 * real keyring key.
 */
export function isUnprotectedCiphertext(encrypted: Buffer): boolean {
  return isLinux() && !LINUX_KEYRING_TAGS.has(encrypted.subarray(0, 3).toString('latin1'));
}

/**
 * Encrypt `plainText` for disk, or resolve `null` when no OS-protected key is
 * available. Rejects only if the encryptor itself fails.
 */
export async function encryptForDisk(plainText: string): Promise<Buffer | null> {
  if (!(await safeStorage.isAsyncEncryptionAvailable())) return null;
  // Electron's documented signal. getSelectedStorageBackend exists on Linux only.
  if (isLinux() && safeStorage.getSelectedStorageBackend() === 'basic_text') return null;
  const encrypted = await safeStorage.encryptStringAsync(plainText);
  // Catches the undocumented case too: a desktop keyring that failed to unlock.
  return isUnprotectedCiphertext(encrypted) ? null : encrypted;
}

/** True only when `encryptForDisk` would produce OS-protected ciphertext. */
export async function isSecureStorageAvailable(): Promise<boolean> {
  try {
    return (await encryptForDisk('secure-storage-probe')) !== null;
  } catch {
    return false;
  }
}
