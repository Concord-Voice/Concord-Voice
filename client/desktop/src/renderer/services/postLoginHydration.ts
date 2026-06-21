/**
 * postLoginHydration (#1297)
 *
 * Single home for the post-login user-state hydration cluster that was
 * previously duplicated inline in Login.completeLoginFromResponse and
 * App.tsx's session-restore path. Extracting it here lets EVERY login path —
 * password/MFA/WebAuthn, SSO (useSSOFlow), and session-restore — hydrate the
 * same set of state uniformly, so none can silently skip a step.
 */

import { preferencesSyncService, type PreferencesSyncDeps } from './preferencesSync';
import { savedGifsSyncService } from './savedGifsSync';
import { friendOrgSyncService } from './friendOrgSync';
import { tryHydrateNotificationPrefs } from './notificationPrefsService';
import { useSubscriptionStore } from '../stores/subscriptionStore';
import { useSettingsStore } from '../stores/settingsStore';
import { useLayoutStore } from '../stores/layoutStore';

/** Build the dependency bag for preferencesSyncService — extracted to reduce nesting depth. */
export function buildPreferencesSyncDeps(): PreferencesSyncDeps {
  return {
    getAppearance: () => useSettingsStore.getState().appearance,
    setAppearance: (patch) =>
      useSettingsStore.setState((s) => ({ appearance: { ...s.appearance, ...patch } })),
    getLayout: () => {
      const s = useLayoutStore.getState();
      return {
        channelPanelPinned: s.channelPanelPinned,
        channelPanelWidth: s.channelPanelWidth,
        memberPanelMode: s.memberPanelMode,
        memberPanelWidth: s.memberPanelWidth,
        serverBarHeight: s.serverBarHeight,
        folderBarHeight: s.folderBarHeight,
        serverFolders: s.serverFolders,
        serverOrder: s.serverOrder,
      };
    },
    setLayout: (patch) => useLayoutStore.setState(patch),
  };
}

/**
 * Hydrate all post-login user state — preferences, saved GIFs, notification mute
 * prefs, and the entitlement capability set — in one place so EVERY login path
 * (password/MFA/WebAuthn via Login.completeLoginFromResponse, SSO via useSSOFlow,
 * and session-restore in App.tsx) hydrates uniformly and none can silently skip it.
 * Extracted verbatim from the prior completeLoginFromResponse cluster (#1297).
 */
export async function hydratePostLogin(): Promise<void> {
  preferencesSyncService.init(buildPreferencesSyncDeps());
  preferencesSyncService.startWatching();
  await preferencesSyncService.fetchAndApply();
  savedGifsSyncService.startWatching();
  await savedGifsSyncService.fetchAndApply();
  friendOrgSyncService.startWatching();
  await friendOrgSyncService.fetchAndApply();
  await tryHydrateNotificationPrefs();
  await useSubscriptionStore.getState().hydrate();
}
