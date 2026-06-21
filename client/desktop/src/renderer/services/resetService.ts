/**
 * Reset Service — Centralized state cleanup for auth lifecycle events
 *
 * Three tiers:
 * - softRestart: Page reload preserving auth tokens. Used by Recovery B when
 *   the client has issues but the session is valid.
 * - gracefulReset: Clears content stores, preserves device settings (theme, audio, etc.)
 *   Used when the user is being "remembered" but needs a fresh content slate.
 * - nuclearReset: Wipes everything. Used when the login screen appears — no ghost profiles.
 *
 * Core principle: if the login screen appears, go nuclear.
 */

import { useAuthStore } from '../stores/authStore';
import { stopProactiveRefresh } from './apiClient';
import { getMessageQueue } from './messageQueue';
import { useServerStore } from '../stores/serverStore';
import { useChannelStore } from '../stores/channelStore';
import { useDMStore } from '../stores/dmStore';
import { useFriendStore } from '../stores/friendStore';
import { usePrivacyStore } from '../stores/privacyStore';
import { useMemberStore } from '../stores/memberStore';
import { useUnreadStore } from '../stores/unreadStore';
import { useVoiceStore } from '../stores/voiceStore';
import { useChatStore } from '../stores/chatStore';
import { useLayoutStore } from '../stores/layoutStore';
import { useUserStore } from '../stores/userStore';
import { useDraftMessageStore } from '../stores/draftMessageStore';
import { useSSOStore } from '../stores/ssoStore';
import { useE2EEStore } from '../stores/e2eeStore';
import { useRichPresenceStore } from '../stores/richPresenceStore';
import { useSubscriptionStore } from '../stores/subscriptionStore';

/**
 * Clears content stores while preserving device settings.
 * Appropriate when rememberMe=true and the session can be restored,
 * but user-specific content must be wiped to prevent ghost artifacts.
 */
export function gracefulReset(): void {
  // Stop proactive token refresh timer (#240)
  stopProactiveRefresh();

  // Clear content stores (in-memory state)
  useServerStore.getState().clearServers();
  useChannelStore.getState().clearChannels();
  useDMStore.getState().clearDMs();
  useFriendStore.getState().clearFriends();
  usePrivacyStore.getState().clearPrivacy();
  useMemberStore.getState().clearMembers();
  useUnreadStore.getState().clearAll();
  useVoiceStore.getState().reset();
  useChatStore.getState().reset();
  // Clear rich-presence (custom-text) cache — other users' statuses + self —
  // so signing into a different account never surfaces the prior user's
  // statuses (#1233/Gitar; risk: privacy cross-account leak).
  useRichPresenceStore.getState().reset();
  // Reset the entitlement capability set to the least-privilege free floor so a
  // prior (e.g. premium) user's in-memory caps never leak into the next session
  // (#1297; risk: authorization cross-account leak — same class as rich-presence
  // above). subscriptionStore has no persist, but logout without app restart
  // would otherwise keep the stale set in memory.
  useSubscriptionStore.getState().reset();
  useUserStore.getState().clearUser();

  // Clear user-specific layout content (serverFolders, serverOrder)
  // but preserve UI preferences (panel widths/modes)
  useLayoutStore.getState().clearUserContent();

  // Clear draft messages (user content, not device settings)
  useDraftMessageStore.getState().clearAllDrafts();

  // Clear SSO ephemeral state — an in-flight SSO callback that's interrupted
  // by a logout / soft-restart must not leave a stale `register_required` or
  // `mfa_required` phase, which would re-mount SSOPassphraseSetup or the MFA
  // modal at the next login screen with stolen-token-equivalent data.
  useSSOStore.getState().reset();

  // Clear E2EE store flags — needsSSOUnlock and ready must reset on every
  // logout-class transition. Otherwise an SSO user logging back in via the
  // password path would see the eager-unlock gate from the previous session.
  useE2EEStore.getState().reset();

  // Clear the in-memory MessageQueue — its singleton survives the renderer
  // logout flow, and any encrypted-flagged plaintext queued by the prior
  // user would otherwise be re-encrypted with the next user's keys after
  // the #918 gate releases on relogin (cross-account identity confusion).
  getMessageQueue().clear();

  // Remove persisted content store data from localStorage
  // (settings stores are NOT touched — theme, audio, video, TTS persist)
  localStorage.removeItem('concord:dm-store');
  localStorage.removeItem('concord-servers');
  localStorage.removeItem('concord-channels');
  localStorage.removeItem('concord:draft-messages');
}

/**
 * Soft restart — reloads the page while preserving auth tokens.
 * Used by Recovery B when the client has issues but the session is valid.
 * Auth tokens persist in zustand/persist localStorage, and disk tokens
 * remain untouched for Remember Me users.
 */
export function softRestart(): void {
  gracefulReset();
  globalThis.location.reload();
}

/**
 * Wipes all state — in-memory stores, localStorage, and main process tokens.
 * Used when the login screen appears (any reason), ensuring zero ghost state.
 */
export function nuclearReset(): void {
  // Start with everything gracefulReset does
  gracefulReset();

  // Additionally clear auth store
  useAuthStore.getState().clearAccessToken();

  // Remove ALL persisted localStorage keys (settings + layout)
  localStorage.removeItem('concord-layout');
  localStorage.removeItem('concord-settings');
  localStorage.removeItem('concord:audio-advanced');
  localStorage.removeItem('concord:video-settings');
  localStorage.removeItem('concord:tts-settings');

  // Clear main process tokens (disk files + in-memory)
  globalThis.electron?.clearTokens?.();
}
