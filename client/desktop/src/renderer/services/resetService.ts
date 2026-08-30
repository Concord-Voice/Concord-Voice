/**
 * Reset Service — Centralized state cleanup for auth lifecycle events
 *
 * Four tiers, ordered narrowest to widest:
 * - recoveryReset: Same-account transport recovery. Fences in-flight E2EE work and
 *   clears NOTHING. Recovery-A reconnects the same session with the same access
 *   token — it is a refresh, not a reset (#2199).
 * - softRestart: Page reload preserving auth tokens. Used by Recovery B when
 *   the client has issues but the session is valid.
 * - gracefulReset: Clears content stores, preserves device settings (theme, audio, etc.)
 *   Used when the user is being "remembered" but needs a fresh content slate.
 * - nuclearReset: Wipes all user/content/auth state, including renderer E2EE key
 *   custody. Used when the login screen appears — no ghost profiles. Device-LOCAL
 *   settings (theme/appearance, audio, video, TTS, layout UI prefs) deliberately
 *   survive: wiping them reverted every logout-class transition to defaults (#1603,
 *   public feedback #26). User-scoped fields nested inside those stores
 *   (per-participant volumes) are still cleared.
 *
 * Core principles:
 * - If the login screen appears, go nuclear — on identity and content, never on
 *   device preferences.
 * - If the account did not change, do not use a primitive that clears
 *   account-scoped state (#2199).
 */

import { useAuthStore } from '../stores/auth/authStore';
import { stopProactiveRefresh } from './apiClient';
import { getMessageQueue } from './messageQueue';
import { useServerStore } from '../stores/chat/serverStore';
import { useChannelStore } from '../stores/chat/channelStore';
import { useDMStore } from '../stores/chat/dmStore';
import { useFriendStore } from '../stores/chat/friendStore';
import { usePrivacyStore } from '../stores/ui/privacyStore';
import { useMemberStore } from '../stores/chat/memberStore';
import { useUnreadStore } from '../stores/chat/unreadStore';
import { useVoiceStore } from '../stores/voice/voiceStore';
import { useChatStore } from '../stores/chat/chatStore';
import { useLayoutStore } from '../stores/ui/layoutStore';
import { useUserStore } from '../stores/auth/userStore';
import { useDraftMessageStore } from '../stores/chat/draftMessageStore';
import { useSSOStore } from '../stores/auth/ssoStore';
import { useE2EEStore } from '../stores/auth/e2eeStore';
import { useRichPresenceStore } from '../stores/ui/richPresenceStore';
import { useSubscriptionStore } from '../stores/auth/subscriptionStore';
import { useSettingsStore } from '../stores/ui/settingsStore';
import { useAudioSettingsStore } from '../stores/audio/audioSettingsStore';
import { preferencesSyncService } from './preferencesSync';
import { savedGifsSyncService } from './savedGifsSync';
import { friendOrgSyncService } from './friendOrgSync';
import { presenceOverrideSyncService } from './presenceOverrideSync';
import { clearFriendEligibilityCache } from './friendEligibility';
import { stopExpirySweep } from './notificationPrefsService';
import { useFriendOrgStore } from '../stores/chat/friendOrgStore';
import { useSavedGifsStore } from '../stores/chat/savedGifsStore';
import { useNotificationPrefsStore } from '../stores/ui/notificationPrefsStore';
import { resetPostLoginHydrationLifecycle } from './postLoginHydrationLifecycle';
import { clearIndex } from './searchService';
import { e2eeService } from './e2eeService';

/**
 * Same-account recovery fence — the narrowest tier.
 *
 * Recovery-A reconnects the SAME session with the SAME access token; only the
 * transport blipped. It must NOT clear account-scoped state: no store is stale
 * merely because the socket dropped, and `draftMessageStore` / MessageQueue hold
 * client-authored state with no server copy to restore from. Using gracefulReset
 * here destroyed drafts, the outbound queue, and the user's activeServerId on
 * every transient blip — hydratePostLogin restores none of them, and the nine
 * renderer effects that hydrate off `accessToken` never re-fire because Recovery-A's
 * defining property is that the token does not change (#2199).
 *
 * The only genuine need is rejecting in-flight decrypt continuations that belong
 * to the pre-drop key generation. Key material is preserved deliberately:
 * `handleReconnected` gates `validateEpochsOnReconnect` and
 * `processPendingKeyRequests` on `e2eeService.isInitialized`, and clearing it
 * would force a full key re-derivation on every reconnect.
 *
 * `resetPostLoginHydrationLifecycle()` is deliberately NOT called here — the
 * caller's `beginPostLoginHydrationGuard()` already invokes it, and a second call
 * would abort the guard it is about to create.
 *
 * Do NOT call gracefulReset() from a same-account recovery path.
 */
export function recoveryReset(): void {
  e2eeService.fencePendingOperations();
}

/**
 * Clears content stores while preserving device settings.
 * Appropriate when rememberMe=true and the session can be restored,
 * but user-specific content must be wiped to prevent ghost artifacts.
 *
 * NOT for same-account transport recovery — see recoveryReset() (#2199).
 */
export function gracefulReset(opts?: { keepDeepLinks?: boolean }): void {
  resetPostLoginHydrationLifecycle();

  // Stop every account-bound watcher/timer before clearing its store. This also
  // invalidates in-flight encrypt/decrypt work so a prior account cannot finish
  // against the next account's token after a fast logout/login transition.
  preferencesSyncService.stopWatching();
  savedGifsSyncService.stopWatching();
  friendOrgSyncService.stopWatching();
  stopExpirySweep();
  // Clear renderer E2EE key custody. EVERY gracefulReset caller reaches the login
  // screen (rememberMe=true refresh failure → apiClient.ts; session-restore failure
  // → App.tsx) or a full renderer reload (softRestart), and nuclearReset calls this
  // too — none is a live same-session continuation. So the prior account's
  // wrappingKey / raw sessionKeys must NOT stay resident (CWE-212, #2199). This is
  // renderer-only: it does not touch the safeStorage secure-e2ee.dat or the
  // main-process in-memory copy, so a rememberMe=true next-launch restore and a
  // softRestart soft-reload both re-derive keys normally. clearKeys() fences
  // pending decrypts internally.
  //
  // Same-account transport recovery does NOT flow through here — it uses
  // recoveryReset(), which fences WITHOUT clearing keys precisely because that path
  // continues the SAME session and must keep decrypting. Do NOT call gracefulReset()
  // from a same-account recovery path (that was the #2199 bug), and do NOT downgrade
  // this back to a bare fencePendingOperations() — that re-opens the CWE-212 leak.
  e2eeService.clearKeys();
  presenceOverrideSyncService.reset();
  useFriendOrgStore.getState().reset();
  useSavedGifsStore.getState().reset();
  useNotificationPrefsStore.getState().clearAll();

  // Clear content stores (in-memory state)
  useServerStore.getState().clearServers();
  useChannelStore.getState().clearChannels();
  useDMStore.getState().clearDMs();
  useFriendStore.getState().clearFriends();
  usePrivacyStore.getState().clearPrivacy();
  // #1241: module-scope eligibility cache. Surviving logout would serve the
  // previous account's verdicts to the next user on a shared device.
  clearFriendEligibilityCache();
  useMemberStore.getState().clearMembers();
  useUnreadStore.getState().clearAll();
  useVoiceStore.getState().reset();
  useChatStore.getState().reset();
  // Search indexes decrypted message content outside the chat stores. Purge it
  // explicitly so no plaintext survives an account lifecycle transition.
  clearIndex();
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

  // A deep link held in App's own state belongs to the session that is ending
  // (#2363). App replays it when `accessToken && emailVerified` next become true,
  // and that predicate is account-BLIND — so user A's invite, held across a
  // rememberMe refresh failure, opens for whoever signs in next.
  //
  // Announced from gracefulReset rather than nuclearReset because the leak's
  // reachable path is the REMEMBERED one: nuclearReset already ends the lifecycle
  // and forgets in main, while gracefulReset deliberately preserves the session so
  // it can resume — and preserving a session is not the same as preserving it for
  // whoever signs in next. nuclearReset calls through here, so both are covered.
  //
  // GATED on there actually having been a session. The first version of this
  // announced unconditionally on the reasoning that click-invite-then-sign-in has
  // no prior session so the reset never runs for it. That was wrong in both of the
  // ways it could be: a cold start whose `restoreSession()` fails calls
  // gracefulReset (App.tsx's restore fallback), and so does the ownerless-credential
  // path — which explicitly preserves MAIN's copy with `{ keepDeepLinks: true }` and
  // would then have erased the renderer's, the two halves working against each
  // other. Both run with `accessToken === null`, because nobody ever signed in.
  //
  // `handleRefreshFailure` calls this BEFORE `clearAccessToken()`, so a genuinely
  // ended session still has its token here. Token present = a session existed and
  // its codes must not cross; token absent = login-side, and the queued invite is
  // exactly what the user is signing in to accept.
  //
  // The token answers "did a session exist", NOT "is this session ending", and
  // ONE caller separates them: softRestart() reloads a still-valid same-account
  // session on Recovery B, where preflight found the renderer unstable and the
  // token fine. It opts out; every other caller is a real teardown and the
  // DEFAULT forgets, so a future caller that ends a session lands on the safe
  // side without knowing this flag exists — the same reasoning that inverted
  // `auth:clearTokens`'s default in v24. Naming matches that channel on purpose.
  if (opts?.keepDeepLinks !== true && useAuthStore.getState().accessToken !== null) {
    globalThis.dispatchEvent(new CustomEvent('deep-link-session-ended'));
    // The renderer event clears the copy App holds; main holds its own, and on a
    // rememberMe teardown nothing else tells it the session ended — `clearTokens`
    // is not called there, by design. Forget-only, so the disk tokens that let the
    // next launch retry are untouched (#2363).
    void globalThis.electron?.forgetDeepLinks?.();
  }
}

/**
 * Soft restart — reloads the page while preserving auth tokens.
 * Used by Recovery B when the client has issues but the session is valid.
 * Auth tokens persist in zustand/persist localStorage, and disk tokens
 * remain untouched for Remember Me users.
 */
export function softRestart(): void {
  // Same account, same token, same session — only the DOCUMENT is being replaced.
  // Main keeps its deliverable codes and resetDeepLinkDelivery re-queues them for
  // the successor renderer, which is the behaviour a transport blip must not
  // change. #2199 is the same lesson on NSFW intent; do not let a teardown fence
  // reach this path (#2363).
  gracefulReset({ keepDeepLinks: true });
  globalThis.location.reload();
}

/**
 * Wipes all user/content/auth state — in-memory stores, user-scoped
 * localStorage, and main process tokens. Used when the login screen appears
 * (any reason), ensuring zero ghost state.
 *
 * Device-local settings (concord-settings, concord:audio-advanced,
 * concord:video-settings, concord:tts-settings, concord-layout) are NOT
 * removed: they are machine preferences, not profile state, and removing
 * them here silently reverted the user's theme and A/V settings on every
 * logout / session-only 401 (#1603). Do not re-add removeItem calls for
 * settings keys — clear user-scoped fields inside those stores instead.
 */
export function nuclearReset(): void {
  // Nuclear reset ends the authenticated lifecycle. A same-account graceful
  // reset must keep this timer alive because its token value does not change and
  // therefore cannot retrigger apiClient's auth-store subscription.
  stopProactiveRefresh();

  // Renderer E2EE key custody is cleared by gracefulReset() below (every
  // login-screen path clears keys since #2199); no separate clearKeys() call is
  // needed here. tokenManager.clearTokens() at the end wipes the main-process +
  // disk halves. Together: a login-screen transition leaves NO renderer, main, or
  // disk key material for the prior account (CWE-212).
  gracefulReset();

  // Account-scoped, NOT device-local — so it is cleared here rather than in
  // gracefulReset(). softRestart() calls gracefulReset() on a same-account
  // Recovery-B reload (valid session, unchanged token), and clearing there would
  // silently revoke the user's NSFW opt-in and persist it to disk on any
  // transport blip — the exact shape #2199 exists to prevent. Every
  // login-screen transition routes through nuclearReset(), so cross-account
  // hygiene is unchanged.
  useSettingsStore.getState().setAllowNsfwContent(false);

  // Additionally clear auth store
  useAuthStore.getState().clearAccessToken();

  // Clear user-scoped data nested inside the (preserved) device-settings
  // stores: per-participant volume overrides are keyed by other users' IDs
  // (#1233 cross-account discipline).
  useAudioSettingsStore.getState().clearAllParticipantVolumes();
  useAudioSettingsStore.getState().clearAllScreenShareVolumes();

  // Clear main process tokens (disk files + in-memory). NO `keepDeepLinks` opt-out:
  // this reset ends the authenticated lifecycle, so main's default — forget — is
  // the wanted behaviour, and a code delivered to this user must not survive into
  // whoever signs in next (#2363).
  //
  // Exactly ONE call, and that matters more than it looks. An earlier shape kept
  // deep links here and issued a SECOND, unflagged clear to do the forgetting.
  // Main's `clearTokens()` resolves its target as
  // `inMemoryApiBase || readActiveApiBase() || DEFAULT_PROFILE_API_BASE`, and the
  // first call empties the in-memory base and deletes the active-profile pointer —
  // so the second falls through to the DEFAULT profile and deletes a remembered
  // SaaS session's credentials this logout never touched. Doubling the call is not
  // idempotent; it changes WHICH profile is cleared (CODEX P2).
  globalThis.electron?.clearTokens?.();
}
