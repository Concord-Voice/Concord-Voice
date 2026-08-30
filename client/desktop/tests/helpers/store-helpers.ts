import { useAuthStore } from '../../src/renderer/stores/auth/authStore';
import { useUserStore } from '../../src/renderer/stores/auth/userStore';
import { useServerStore } from '../../src/renderer/stores/chat/serverStore';
import { useChannelStore } from '../../src/renderer/stores/chat/channelStore';
import { useChatStore } from '../../src/renderer/stores/chat/chatStore';
import { useMemberStore } from '../../src/renderer/stores/chat/memberStore';
import { useUnreadStore } from '../../src/renderer/stores/chat/unreadStore';
import { useInviteStore } from '../../src/renderer/stores/chat/inviteStore';
import { useDMStore } from '../../src/renderer/stores/chat/dmStore';
import { useFriendStore } from '../../src/renderer/stores/chat/friendStore';
import { useConnectionStore } from '../../src/renderer/stores/ui/connectionStore';
import { usePrivacyStore } from '../../src/renderer/stores/ui/privacyStore';
import { useSubscriptionStore } from '../../src/renderer/stores/auth/subscriptionStore';
import { useMFAChallengeStore } from '../../src/renderer/stores/auth/mfaChallengeStore';
import { useSavedGifsStore } from '../../src/renderer/stores/chat/savedGifsStore';
import { useChannelScrollStore } from '../../src/renderer/stores/chat/channelScrollStore';
import { useUpdateStatusStore } from '../../src/renderer/stores/ui/updateStatusStore';
import { useVoiceStore } from '../../src/renderer/stores/voice/voiceStore';
import { useSSOStore } from '../../src/renderer/stores/auth/ssoStore';
import { useE2EEStore } from '../../src/renderer/stores/auth/e2eeStore';
import { usePendingRegistrationStore } from '../../src/renderer/stores/auth/pendingRegistrationStore';
import { useNotificationPrefsStore } from '../../src/renderer/stores/ui/notificationPrefsStore';
import { useAttestationFailureStore } from '../../src/renderer/stores/auth/attestationFailureStore';
import { useRichPresenceStore } from '../../src/renderer/stores/ui/richPresenceStore';
import { useChangelogStore } from '../../src/renderer/stores/ui/changelogStore';
import { useFriendOrgStore } from '../../src/renderer/stores/chat/friendOrgStore';
import { usePresenceOverrideStore } from '../../src/renderer/stores/ui/presenceOverrideStore';
import { useSettingsStore } from '../../src/renderer/stores/ui/settingsStore';
import { useAudioSettingsStore } from '../../src/renderer/stores/audio/audioSettingsStore';
import { useClientConfigStore } from '../../src/renderer/stores/ui/clientConfigStore';
import { useDraftMessageStore } from '../../src/renderer/stores/chat/draftMessageStore';
import { useDraftSettingsStore } from '../../src/renderer/stores/ui/draftSettingsStore';
import { useKeyboardShortcutStore } from '../../src/renderer/stores/ui/keyboardShortcutStore';
import { useLayoutStore } from '../../src/renderer/stores/ui/layoutStore';
import { useNotificationNavigationStore } from '../../src/renderer/stores/ui/notificationNavigationStore';
import { useNotificationStore } from '../../src/renderer/stores/ui/notificationStore';
import { useOsPermissionStore } from '../../src/renderer/stores/voice/osPermissionStore';
import { usePermissionStore } from '../../src/renderer/stores/chat/permissionStore';
import { useSettingsNavStore } from '../../src/renderer/stores/ui/settingsNavStore';
import { useSettingsOverlayStore } from '../../src/renderer/stores/ui/settingsOverlayStore';
import { useTTSSettingsStore } from '../../src/renderer/stores/audio/ttsSettingsStore';
import { useVideoSettingsStore } from '../../src/renderer/stores/voice/videoSettingsStore';

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
