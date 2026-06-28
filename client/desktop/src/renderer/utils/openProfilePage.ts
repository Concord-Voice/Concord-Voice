import { useSettingsNavStore } from '../stores/settingsNavStore';
import { useSettingsOverlayStore } from '../stores/settingsOverlayStore';

/**
 * The single navigation route from the user-popover "My Profile" quick-link to the
 * profile editor's new home, Settings ▸ Account ▸ My Profile (#1773).
 *
 * Unlike `openSubscriptionPage` (which fires from inside an already-open Settings
 * overlay and so needs only the focus request), this fires from the user popover
 * with the overlay CLOSED — so it must BOTH open the app-settings overlay AND
 * request the cross-section focus that scrolls/expands `section-profile`.
 */
export function openProfilePage(): void {
  useSettingsOverlayStore.getState().openSettings('app');
  useSettingsNavStore.getState().requestFocus('account', 'section-profile');
}
