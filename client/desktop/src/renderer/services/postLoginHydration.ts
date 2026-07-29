/**
 * postLoginHydration (#1297)
 *
 * Single home for the post-login user-state hydration cluster that was
 * previously duplicated inline in Login.completeLoginFromResponse and
 * App.tsx's session-restore path. Every login path uses this same cluster:
 * password/MFA/WebAuthn and session restore invoke it directly, while SSO
 * deliberately defers it until App's eager passphrase-unlock callback has
 * initialized E2EE.
 */

import { preferencesSyncService, type PreferencesSyncDeps } from './preferencesSync';
import { savedGifsSyncService } from './savedGifsSync';
import { friendOrgSyncService } from './friendOrgSync';
import { presenceOverrideSyncService } from './presenceOverrideSync';
import { tryHydrateNotificationPrefs } from './notificationPrefsService';
import { useSubscriptionStore } from '../stores/subscriptionStore';
import { useSettingsStore } from '../stores/settingsStore';
import { useLayoutStore } from '../stores/layoutStore';
import {
  beginPostLoginHydrationGuard,
  type HydrationLifecycleGuard,
} from './postLoginHydrationLifecycle';

/** Build the dependency bag for preferencesSyncService — extracted to reduce nesting depth. */
export function buildPreferencesSyncDeps(): PreferencesSyncDeps {
  return {
    getAppearance: () => useSettingsStore.getState().appearance,
    setAppearance: (patch) =>
      useSettingsStore.setState((s) => ({ appearance: { ...s.appearance, ...patch } })),
    getLayout: () => {
      const s = useLayoutStore.getState();
      return {
        sidebarProfiles: s.sidebarProfiles,
        sidebarLayoutsDecoupled: s.sidebarLayoutsDecoupled,
        serverBarHeight: s.serverBarHeight,
        folderBarHeight: s.folderBarHeight,
        serverFolders: s.serverFolders,
        serverOrder: s.serverOrder,
      };
    },
    setLayout: (patch) => useLayoutStore.setState(patch),
    applySidebarPreferences: (profiles, decoupled) =>
      useLayoutStore.getState().applySidebarPreferences(profiles, decoupled),
  };
}

/**
 * Hydrate all post-login user state — preferences, saved GIFs, notification mute
 * prefs, and the entitlement capability set — in one place so every login path
 * hydrates the same state. Password/MFA/WebAuthn invoke it through
 * Login.completeLoginFromResponse, session restore invokes it in App.tsx, and
 * SSO invokes it from App only after SSOEagerUnlock initializes E2EE.
 * Extracted verbatim from the prior completeLoginFromResponse cluster (#1297).
 */
export async function hydratePostLogin(existingGuard?: HydrationLifecycleGuard): Promise<void> {
  const guard = existingGuard ?? beginPostLoginHydrationGuard();
  const isCurrent = guard.isCurrent;

  if (!isCurrent()) return;
  preferencesSyncService.init(buildPreferencesSyncDeps());
  if (!isCurrent()) return;
  preferencesSyncService.startWatching();
  if (!isCurrent()) return;
  await preferencesSyncService.fetchAndApply(guard);

  if (!isCurrent()) return;
  savedGifsSyncService.startWatching();
  if (!isCurrent()) return;
  await savedGifsSyncService.fetchAndApply(guard);

  if (!isCurrent()) return;
  friendOrgSyncService.startWatching();
  if (!isCurrent()) return;
  const friendOrgHydrationIsCurrent = await friendOrgSyncService.fetchAndApply();
  if (!isCurrent() || !friendOrgHydrationIsCurrent) return;

  const presenceOverrideHydrationIsCurrent = await presenceOverrideSyncService.fetchAndApply();
  if (!isCurrent() || !presenceOverrideHydrationIsCurrent) return;

  await tryHydrateNotificationPrefs(guard);
  if (!isCurrent()) return;
  await useSubscriptionStore.getState().hydrate(guard);
}
