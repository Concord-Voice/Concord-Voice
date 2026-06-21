/**
 * Shared transport helpers for E2EE-encrypted JSON blobs (preferences, saved GIFs, etc.).
 *
 * These functions encapsulate the encrypt/decrypt + HTTP push/fetch + debounce dance
 * so individual sync services (preferencesSync, savedGifsSync, ...) only need to define
 * how to read/apply their own blob shape.
 */

import { e2eeService } from './e2eeService';
import { apiFetch } from './apiClient';

export interface E2EEBlobEnvelope<T> {
  v: 1;
  data: T;
}

/** Read an opaque encrypted blob from the server, decrypt it, and return the parsed payload. */
export async function fetchEncryptedBlob<T>(
  endpoint: string,
  responseKey: string
): Promise<{ blob: T | null; pushBootstrap: boolean }> {
  if (!e2eeService.isInitialized) {
    return { blob: null, pushBootstrap: false };
  }

  try {
    const res = await apiFetch(endpoint);
    if (!res.ok) {
      return { blob: null, pushBootstrap: false };
    }

    const data = await res.json();
    const wrapper = data[responseKey] as { encrypted_data: string } | null;
    if (!wrapper) {
      return { blob: null, pushBootstrap: true };
    }

    try {
      const decrypted = await e2eeService.decryptPreferences<T>(wrapper.encrypted_data);
      return { blob: decrypted, pushBootstrap: false };
    } catch {
      // Stale/incompatible ciphertext — caller should re-push local state.
      return { blob: null, pushBootstrap: true };
    }
  } catch {
    return { blob: null, pushBootstrap: false };
  }
}

/** Encrypt a blob and PUT it to the given endpoint. Errors are swallowed (logged). */
export async function pushEncryptedBlob<T>(endpoint: string, blob: T): Promise<void> {
  if (!e2eeService.isInitialized) return;

  try {
    const encrypted = await e2eeService.encryptPreferences(blob);
    await apiFetch(endpoint, {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ encrypted_data: encrypted }),
    });
  } catch {
    // Push errors are non-fatal — the next change will retry.
  }
}
