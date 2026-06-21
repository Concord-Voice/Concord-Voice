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

/**
 * Resets all Zustand stores to their initial state.
 * Call this in beforeEach() or afterEach() to prevent test leakage.
 */
export function resetAllStores(): void {
  // Stores with explicit clear/reset methods
  useAuthStore.getState().clearAccessToken();
  useAuthStore.getState().setRememberMe(true); // Reset to default
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
  useMFAChallengeStore.getState().clearChallenge();
  useSavedGifsStore.setState({ gifs: [] });
  useChannelScrollStore.setState({ positions: {} });
  useVoiceStore.getState().reset();
  useVoiceStore.setState({ channelVoiceMembers: {}, serverVoiceCounts: {} });
  useUpdateStatusStore.getState().reset();
  useSSOStore.getState().reset();
  useE2EEStore.getState().reset();
  usePendingRegistrationStore.getState().clearPending();
  useNotificationPrefsStore.getState().clearAll();
  useAttestationFailureStore.getState().dismiss();
  useRichPresenceStore.getState().reset();

  // Clear persisted state from localStorage AND sessionStorage
  // (pendingRegistrationStore persists to sessionStorage)
  localStorage.clear();
  sessionStorage.clear();
}
