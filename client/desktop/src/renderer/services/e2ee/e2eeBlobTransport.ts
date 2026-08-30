/**
 * Shared transport helpers for E2EE-encrypted JSON blobs (preferences, saved GIFs, etc.).
 *
 * These functions encapsulate the encrypt/decrypt + HTTP push/fetch + debounce dance
 * so individual sync services (preferencesSync, savedGifsSync, ...) only need to define
 * how to read/apply their own blob shape.
 */

import { e2eeService } from './e2eeService';
import { apiFetch } from '../system/apiClient';

export interface E2EEBlobEnvelope<T> {
  v: 1;
  data: T;
}

/** Read an opaque encrypted blob from the server, decrypt it, and return the parsed payload. */
export async function fetchEncryptedBlob<T>(
  endpoint: string,
  responseKey: string,
  signal?: AbortSignal,
  isCurrent?: () => boolean
): Promise<{ blob: T | null; pushBootstrap: boolean }> {
  const operationIsCurrent = (): boolean =>
    signal?.aborted !== true && (isCurrent === undefined || isCurrent());
  if (!e2eeService.isInitialized || !operationIsCurrent()) {
    return { blob: null, pushBootstrap: false };
  }

  try {
    const res = await apiFetch(endpoint, signal ? { signal } : undefined, {
      authoritative: false,
    });
    if (!operationIsCurrent() || !res.ok) {
      return { blob: null, pushBootstrap: false };
    }

    const data: unknown = await res.json();
    if (!operationIsCurrent()) return { blob: null, pushBootstrap: false };
    if (typeof data !== 'object' || data === null || Array.isArray(data)) {
      return { blob: null, pushBootstrap: false };
    }
    const wrapper = (data as Record<string, unknown>)[responseKey];
    if (wrapper === null) {
      return { blob: null, pushBootstrap: true };
    }
    if (typeof wrapper !== 'object' || Array.isArray(wrapper)) {
      return { blob: null, pushBootstrap: false };
    }
    const encryptedData = (wrapper as { encrypted_data?: unknown }).encrypted_data;
    if (typeof encryptedData !== 'string' || encryptedData.length === 0) {
      return { blob: null, pushBootstrap: false };
    }

    try {
      const decrypted = await e2eeService.decryptPreferences<T>(encryptedData);
      if (!operationIsCurrent()) return { blob: null, pushBootstrap: false };
      return { blob: decrypted, pushBootstrap: false };
    } catch {
      if (!operationIsCurrent()) return { blob: null, pushBootstrap: false };
      // A present row that cannot decrypt is not authoritative absence. Fail
      // closed so an empty or not-yet-hydrated local store cannot overwrite
      // old-key/corrupt ciphertext; only an explicit null row may bootstrap.
      return { blob: null, pushBootstrap: false };
    }
  } catch {
    return { blob: null, pushBootstrap: false };
  }
}

/** Outcome of an authoritative row fetch for password-change rotation (#2200). */
export type RotationFetchResult =
  | { kind: 'absent' }
  | { kind: 'present'; version: number; plaintext: unknown }
  | { kind: 'undecryptable'; version: number }
  | { kind: 'error' };

/**
 * Authoritative fetch of one password-derived domain row for atomic rotation.
 * Unlike fetchEncryptedBlob this surfaces the row VERSION (the CAS
 * precondition) and distinguishes undecryptable-present (preserve marker) from
 * absence — the local store is never consulted (#2200 criterion 4). Decrypts
 * with the HELD (old-password) preferences key, so it must run BEFORE the
 * password POST re-initializes e2eeService. Uses the default authoritative
 * apiFetch mode: this is a user-action fetch whose failure aborts the change,
 * not a best-effort background sync.
 */
export async function fetchBlobRowForRotation(
  endpoint: string,
  responseKey: string,
  signal?: AbortSignal
): Promise<RotationFetchResult> {
  if (!e2eeService.isInitialized || signal?.aborted === true) return { kind: 'error' };
  try {
    const res = await apiFetch(endpoint, signal ? { signal } : undefined);
    if (!res.ok) return { kind: 'error' };
    const data: unknown = await res.json();
    if (typeof data !== 'object' || data === null || Array.isArray(data)) return { kind: 'error' };
    const wrapper = (data as Record<string, unknown>)[responseKey];
    if (wrapper === null) return { kind: 'absent' };
    if (typeof wrapper !== 'object' || Array.isArray(wrapper)) return { kind: 'error' };
    const { encrypted_data: encryptedData, version } = wrapper as {
      encrypted_data?: unknown;
      version?: unknown;
    };
    if (
      typeof encryptedData !== 'string' ||
      encryptedData.length === 0 ||
      typeof version !== 'number' ||
      !Number.isInteger(version) ||
      version < 1
    ) {
      return { kind: 'error' };
    }
    try {
      const plaintext = await e2eeService.decryptPreferences<unknown>(encryptedData);
      return { kind: 'present', version, plaintext };
    } catch {
      // Present-but-undecryptable is NOT absence (#2191 posture): the caller
      // submits a verify+preserve marker instead of new ciphertext.
      return { kind: 'undecryptable', version };
    }
  } catch {
    return { kind: 'error' };
  }
}

/** Encrypt a blob and PUT it to the given endpoint. Errors are swallowed (logged). */
export async function pushEncryptedBlob<T>(
  endpoint: string,
  blob: T,
  signal?: AbortSignal,
  isCurrent?: () => boolean
): Promise<void> {
  const operationIsCurrent = (): boolean =>
    signal?.aborted !== true && (isCurrent === undefined || isCurrent());
  if (!e2eeService.isInitialized || !operationIsCurrent()) return;

  try {
    const encrypted = await e2eeService.encryptPreferences(blob);
    // Encryption cannot be aborted. Re-check before apiFetch captures the
    // current account token so a stopped prior-account push cannot cross over.
    if (!operationIsCurrent()) return;
    await apiFetch(
      endpoint,
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ encrypted_data: encrypted }),
        signal,
      },
      { authoritative: false }
    );
  } catch {
    // Push errors are non-fatal — the next change will retry.
  }
}
