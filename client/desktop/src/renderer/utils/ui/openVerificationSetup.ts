import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
} from '../../services/system/requestContext';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { hasPendingDrafts, useDraftSettingsStore } from '../../stores/ui/draftSettingsStore';
import { useSettingsNavStore } from '../../stores/ui/settingsNavStore';
import {
  useSettingsOverlayStore,
  type VerificationReturn,
} from '../../stores/ui/settingsOverlayStore';

/**
 * The control `requestFocus` lands on: the Multi-Factor Authentication
 * `<details>` in Privacy & Security. `SettingsPage` focuses its `<summary>` and
 * expands it, as `openProfilePage` does for `section-profile`.
 */
const MFA_SECTION_ID = 'section-mfa';

export interface OpenVerificationSetupOptions {
  /** Where the "Back to …" button goes once setup is done. */
  returnTo: VerificationReturn;
  /**
   * For a Server Settings host with unsaved edits: resolves false to stay
   * where it is. Leaving Server Settings unmounts it, and its edits with it.
   */
  confirmDiscard?: () => boolean | Promise<boolean>;
  /**
   * Closes the caller's step-up dialog, abandoning the action. Called only
   * once the discard guard has passed (D-4): a declined discard must find the
   * dialog, and everything in it, exactly as it was.
   */
  closeHost?: () => void;
}

/**
 * The "Set up verification" route (#3456 §3.6a): opens App Settings at the
 * MFA section and records where to come back to.
 *
 * The discard guard runs first, while the caller's dialog is still open; only
 * then is the dialog closed through `closeHost` (D-4). The gated action is
 * abandoned, never retried on return: a frozen destructive request must not
 * outlive a Settings excursion, so the user repeats it themselves.
 *
 * Resolves true when Settings opened. It does not open after a declined
 * discard, nor when the account or server changed while the discard was asked:
 * the return would then name the previous one's chat or server.
 */
export async function openVerificationSetup({
  returnTo,
  confirmDiscard,
  closeHost,
}: OpenVerificationSetupOptions): Promise<boolean> {
  const context = captureApiRequestContext();
  if (confirmDiscard !== undefined && !(await confirmDiscard())) return false;
  if (!apiRequestContextIsCurrent(context)) return false;
  closeHost?.();
  const overlay = useSettingsOverlayStore.getState();
  overlay.openSettings('app');
  overlay.setVerificationReturn(returnTo);
  useSettingsNavStore.getState().requestFocus('privacy', MFA_SECTION_ID);
  return true;
}

/** The "Back to …" button's label for a pending return. */
export function verificationReturnLabel(target: VerificationReturn): string {
  switch (target.kind) {
    case 'chat':
      return 'Back to chat';
    case 'serverSettings':
      return 'Back to Server Settings';
  }
}

/**
 * The "Back to …" button's action: takes the pending return and goes there.
 * "Back to chat" closes the overlay, since the chat underneath never moved.
 * "Back to Server Settings" first refetches that server's permissions (D-6):
 * the permission events refresh only the active server, and Server Settings
 * may be open for another one, which would otherwise come back on the masked
 * snapshot taken before setup. The fetcher's own fences guard the write; a
 * failed refetch still reopens it, since the server stays authoritative. An
 * account or server change during the refetch reopens nothing. Nothing is
 * pending, nothing happens.
 *
 * Either way App Settings goes, and its draft layer with it, so a change the
 * user has not applied holds the return exactly as "Back to app" holds its
 * close: nothing is taken and nothing closes until it is applied or reverted.
 *
 * Resolves true when it left App Settings.
 */
export async function returnFromVerificationSetup(): Promise<boolean> {
  if (hasPendingDrafts(useDraftSettingsStore.getState().drafts)) return false;
  const overlay = useSettingsOverlayStore.getState();
  const target = overlay.takeVerificationReturn();
  if (target === null) return false;
  if (target.kind === 'chat') {
    overlay.close();
    return true;
  }
  const context = captureApiRequestContext();
  await usePermissionStore.getState().fetchServerPermissions(target.serverId);
  if (!apiRequestContextIsCurrent(context)) return false;
  // The refetch is a wait: a change made during it holds the return as well.
  if (hasPendingDrafts(useDraftSettingsStore.getState().drafts)) {
    useSettingsOverlayStore.getState().setVerificationReturn(target);
    return false;
  }
  useSettingsOverlayStore
    .getState()
    .openSettings('server', { serverId: target.serverId, section: target.section });
  return true;
}
