import type { E2EESessionKeys } from '../services/e2eeService';
import type { CredentialOwner } from '../../main/ipcContract';
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
export async function persistE2EESessionKeys(
  sessionKeys: E2EESessionKeys | null,
  credentialOwner?: CredentialOwner
): Promise<boolean> {
  if (!sessionKeys) return true;

  const electron = globalThis.electron;
  if (!electron) return true;

  // A login continuation that owns main-process credentials must keep key
  // persistence in that same ownership domain. Never fall back to the generic
  // writer: a stale continuation could otherwise overwrite a successor's
  // keychain material after its renderer-side generation check.
  if (credentialOwner !== undefined && !electron.storeE2EEKeysIfOwner) {
    console.warn('Owner-scoped E2EE key persistence is unavailable in this desktop shell.');
    return false;
  }
  if (credentialOwner === undefined && !electron.storeE2EEKeys) return true;

  try {
    const persisted =
      credentialOwner === undefined
        ? await electron.storeE2EEKeys(sessionKeys)
        : await electron.storeE2EEKeysIfOwner(sessionKeys, credentialOwner);
    if (persisted === false || typeof persisted !== 'boolean') {
      // #2394: deliberately NOT attributed to the keychain. A `false` can also
      // mean the main-process staging lane was held by a credential or an SSO
      // reservation, which the renderer cannot distinguish. The main process
      // logs which cause fired; here we state only the consequence.
      console.warn(
        'E2EE session keys did not persist (E2EE active for this session only; re-login on next launch to restore restart-survival).'
      );
      return false;
    }
    return true;
  } catch (storeError) {
    console.warn(
      'Failed to invoke E2EE key persistence (E2EE active for this session only):',
      errorMessage(storeError)
    );
    return false;
  }
}
