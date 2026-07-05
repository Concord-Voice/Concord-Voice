import type { E2EESessionKeys } from '../services/e2eeService';
import { errorMessage } from './redactError';

/**
 * Persist E2EE session keys to the OS keychain via the main process.
 *
 * Surfaces a non-blocking `console.warn` on either failure mode:
 *   - a genuine keychain/disk write failure, where `storeE2EEKeys` resolves
 *     `false` (it no longer throws for that — #1288); or
 *   - an IPC-transport failure, where `ipcRenderer.invoke` rejects.
 *
 * It NEVER clears the in-memory session: a persistence failure only costs
 * restart-survival (App.tsx session-restore rehydrates from stored keys); the
 * current in-session E2EE keys stay valid, and the user can re-login on next
 * launch to restore persistence (#1278 — a persist failure must not
 * `clearKeys()` a valid session).
 *
 * No-op when `sessionKeys` is null (e.g. a prior E2EE-init failure already
 * cleared them, so `getSessionKeys()` returns null) or when the Electron bridge
 * is unavailable (web/dev shells without the preload API).
 *
 * Single source of truth for the login / registration / SSO-setup call-sites so
 * the warn-and-never-clear policy is defined once, not copy-pasted (which also
 * kept those handlers under the S3776 cognitive-complexity ceiling).
 */
export async function persistE2EESessionKeys(sessionKeys: E2EESessionKeys | null): Promise<void> {
  if (!sessionKeys || !globalThis.electron?.storeE2EEKeys) return;

  try {
    const persisted = await globalThis.electron.storeE2EEKeys(sessionKeys);
    if (persisted === false) {
      console.warn(
        'E2EE session keys did not persist to the keychain (E2EE active for this session only; re-login on next launch to restore restart-survival).'
      );
    }
  } catch (storeError) {
    console.warn(
      'Failed to invoke E2EE key persistence (E2EE active for this session only):',
      errorMessage(storeError)
    );
  }
}
