import { useAuthStore } from '../../src/renderer/stores/authStore';
import { useUserStore } from '../../src/renderer/stores/userStore';
import { useServerStore } from '../../src/renderer/stores/serverStore';
import { useChannelStore } from '../../src/renderer/stores/channelStore';
import { useChatStore } from '../../src/renderer/stores/chatStore';
import { useMemberStore } from '../../src/renderer/stores/memberStore';
import { useUnreadStore } from '../../src/renderer/stores/unreadStore';
import { useInviteStore } from '../../src/renderer/stores/inviteStore';
import { useDMStore } from '../../src/renderer/stores/dmStore';
import { useFriendStore } from '../../src/renderer/stores/friendStore';
import { useConnectionStore } from '../../src/renderer/stores/connectionStore';
import { usePrivacyStore } from '../../src/renderer/stores/privacyStore';
import { useSubscriptionStore } from '../../src/renderer/stores/subscriptionStore';
import { useMFAChallengeStore } from '../../src/renderer/stores/mfaChallengeStore';
import { useSavedGifsStore } from '../../src/renderer/stores/savedGifsStore';
import { useChannelScrollStore } from '../../src/renderer/stores/channelScrollStore';
import { useUpdateStatusStore } from '../../src/renderer/stores/updateStatusStore';
import { useVoiceStore } from '../../src/renderer/stores/voiceStore';
import { useSSOStore } from '../../src/renderer/stores/ssoStore';
import { useE2EEStore } from '../../src/renderer/stores/e2eeStore';
import { usePendingRegistrationStore } from '../../src/renderer/stores/pendingRegistrationStore';
import { useNotificationPrefsStore } from '../../src/renderer/stores/notificationPrefsStore';
import { useAttestationFailureStore } from '../../src/renderer/stores/attestationFailureStore';
import { useRichPresenceStore } from '../../src/renderer/stores/richPresenceStore';
import { useChangelogStore } from '../../src/renderer/stores/changelogStore';
import { useFriendOrgStore } from '../../src/renderer/stores/friendOrgStore';
import { usePresenceOverrideStore } from '../../src/renderer/stores/presenceOverrideStore';
import { useSettingsStore } from '../../src/renderer/stores/settingsStore';
import { useAudioSettingsStore } from '../../src/renderer/stores/audioSettingsStore';
import { useClientConfigStore } from '../../src/renderer/stores/clientConfigStore';
import { useDraftMessageStore } from '../../src/renderer/stores/draftMessageStore';
import { useDraftSettingsStore } from '../../src/renderer/stores/draftSettingsStore';
import { useKeyboardShortcutStore } from '../../src/renderer/stores/keyboardShortcutStore';
import { useLayoutStore } from '../../src/renderer/stores/layoutStore';
import { useNotificationNavigationStore } from '../../src/renderer/stores/notificationNavigationStore';
import { useNotificationStore } from '../../src/renderer/stores/notificationStore';
import { useOsPermissionStore } from '../../src/renderer/stores/osPermissionStore';
import { usePermissionStore } from '../../src/renderer/stores/permissionStore';
import { useSettingsNavStore } from '../../src/renderer/stores/settingsNavStore';
import { useSettingsOverlayStore } from '../../src/renderer/stores/settingsOverlayStore';
import { useTTSSettingsStore } from '../../src/renderer/stores/ttsSettingsStore';
import { useVideoSettingsStore } from '../../src/renderer/stores/videoSettingsStore';

interface InitialStateStore<State> {
  getInitialState(): State;
  setState(state: State, replace: true): void;
}

function resetToInitialState<State>(store: InitialStateStore<State>): void {
  const runtimeStore = store as Partial<InitialStateStore<State>> | undefined;
  if (
    !runtimeStore ||
    typeof runtimeStore.getInitialState !== 'function' ||
    typeof runtimeStore.setState !== 'function'
  ) {
    return;
  }

  runtimeStore.setState(runtimeStore.getInitialState(), true);
}

/**
 * Resets all Zustand stores to their initial state.
 * Call this in beforeEach() or afterEach() to prevent test leakage.
 */
export function resetAllStores(): void {
  // Stores with explicit clear/reset methods
  useAuthStore.getState().clearAccessToken();
  useAuthStore.getState().setRememberMe(true); // Reset to default
  useAuthStore.getState().setLoginNotice(null); // Not covered by clearAccessToken (by design)
  useUserStore.getState().clearUser();
  useServerStore.getState().clearServers();
  useChannelStore.getState().clearChannels();
  useChatStore.getState().reset();
  useMemberStore.getState().clearMembers();
  useUnreadStore.getState().clearAll();
  useInviteStore.getState().clearInvites();
  useDMStore.getState().clearDMs();
  useFriendStore.getState().clearFriends();
  useConnectionStore.getState().reset();
  usePrivacyStore.getState().clearPrivacy();
  useSubscriptionStore.getState().reset();
  useSettingsStore.getState().setAllowNsfwContent(false);
  useMFAChallengeStore.getState().clearChallenge();
  useSavedGifsStore.setState({ gifs: [] });
  useChannelScrollStore.setState({ positions: {}, latestMessageIds: {} });
  useVoiceStore.getState().reset();
  // reset() intentionally preserves layout prefs (incl. voiceViewMode) — pin
  // the view-mode default here so tests never order-depend on a prior toggle.
  useVoiceStore.setState({
    channelVoiceMembers: {},
    serverVoiceCounts: {},
    voiceViewMode: 'front-center',
  });
  useUpdateStatusStore.getState().reset();
  useSSOStore.getState().reset();
  useE2EEStore.getState().reset();
  usePendingRegistrationStore.getState().clearPending();
  useNotificationPrefsStore.getState().clearAll();
  useAttestationFailureStore.getState().dismiss();
  useRichPresenceStore.getState().reset();
  useFriendOrgStore.getState().reset();
  usePresenceOverrideStore.getState().reset();
  useChangelogStore.setState({ lastSeenVersion: null });
  resetToInitialState(useAudioSettingsStore);
  resetToInitialState(useClientConfigStore);
  resetToInitialState(useDraftMessageStore);
  useDraftSettingsStore.getState().teardown();
  resetToInitialState(useKeyboardShortcutStore);
  resetToInitialState(useLayoutStore);
  resetToInitialState(useNotificationNavigationStore);
  resetToInitialState(useNotificationStore);
  resetToInitialState(useOsPermissionStore);
  resetToInitialState(usePermissionStore);
  resetToInitialState(useSettingsNavStore);
  resetToInitialState(useSettingsOverlayStore);
  resetToInitialState(useTTSSettingsStore);
  resetToInitialState(useVideoSettingsStore);

  // Clear persisted state from localStorage AND sessionStorage
  // (pendingRegistrationStore persists to sessionStorage)
  localStorage.clear();
  sessionStorage.clear();
}
