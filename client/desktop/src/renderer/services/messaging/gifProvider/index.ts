/**
 * Active GIF provider singleton.
 *
 * The picker, the chat embed, and the message composer import `gifProvider`
 * from this file. To swap vendors (e.g. fall back to a different API if KLIPY
 * ever revokes our key), change the import + assignment below — that's the
 * entire vendor swap. The rest of the codebase imports against the abstract
 * `GifProvider` interface from `./types.ts`.
 *
 * KLIPY ToS Section 1 reserves the right to revoke API access at any time, so
 * this single point of swap is our bus-factor mitigation.
 *
 * This module also subscribes to the privacy store and pushes proxy /
 * personalization preference changes through to the active provider so the
 * picker and embed automatically respect the user's settings without each
 * caller having to remember to wire them up.
 */

import type { GifProvider } from './types';
import { klipyProvider } from './klipyProvider';
import { klipyClient } from './klipyClient';
import { usePrivacyStore } from '../../../stores/ui/privacyStore';

export const gifProvider: GifProvider = klipyProvider;
export const subscribeGifIdentity = (listener: () => void): (() => void) =>
  klipyClient.subscribeCustomerId(listener);
export const getGifIdentityRevision = (): number => klipyClient.getCustomerIdRevision();
export type {
  GifProvider,
  GifResolved,
  GifSearchResult,
  GifCategory,
  GifCategoryPreview,
} from './types';

// Apply current settings immediately, then subscribe to future changes.
// Note: KLIPY traffic is ALWAYS proxied through the control-plane now —
// the legacy "Privacy Mode" toggle no longer has any effect on routing.
const applySettings = (state: ReturnType<typeof usePrivacyStore.getState>): void => {
  // The initial ON value is only a placeholder until the server confirms the
  // account's preference. Do not mint or send a stable ID from that placeholder.
  gifProvider.setPersonalizationEnabled(
    state.loaded && state.settings.sharePersonalizationWithGifProvider
  );
};
applySettings(usePrivacyStore.getState());
usePrivacyStore.subscribe(applySettings);
