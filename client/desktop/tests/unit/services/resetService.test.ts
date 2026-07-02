import { vi, describe, it, expect, beforeEach } from 'vitest';

// Mock apiClient to prevent real HTTP calls
vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
}));

import { gracefulReset, nuclearReset } from '@/renderer/services/resetService';
import { useAuthStore } from '@/renderer/stores/authStore';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useFriendStore } from '@/renderer/stores/friendStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useUserStore } from '@/renderer/stores/userStore';
import { useRichPresenceStore } from '@/renderer/stores/richPresenceStore';
import { useSubscriptionStore, FREE_ENTITLEMENT } from '@/renderer/stores/subscriptionStore';
import { useAudioSettingsStore } from '@/renderer/stores/audioSettingsStore';
import { mockServer } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  // Set up some state to verify it gets cleared
  useAuthStore.getState().setAccessToken('test-token');
  useServerStore.getState().addServer(mockServer);
  useUserStore.setState({
    user: { id: 'u1', username: 'test', email: 'test@test.com' },
  } as never);
});

describe('resetService', () => {
  describe('gracefulReset', () => {
    it('clears content stores', () => {
      gracefulReset();

      expect(useServerStore.getState().servers).toHaveLength(0);
      expect(useChannelStore.getState().channels).toHaveLength(0);
      expect(useDMStore.getState().conversations).toHaveLength(0);
      expect(useFriendStore.getState().friends).toHaveLength(0);
      expect(useChatStore.getState().messagesByChannel.size).toBe(0);
    });

    it('clears the rich-presence custom-text cache (#1233 cross-account leak fix)', () => {
      useRichPresenceStore.getState().setCustomText('other-user', { text: 'busy', emoji: '🔴' });
      useRichPresenceStore.getState().setSelfPresence({ tier: 2, customText: 'mine' });

      gracefulReset();

      expect(Object.keys(useRichPresenceStore.getState().customTextByUser)).toHaveLength(0);
      expect(useRichPresenceStore.getState().self.tier).toBe(0);
      expect(useRichPresenceStore.getState().self.customText).toBeUndefined();
    });

    it('clears the entitlement set to the free floor (#1297 cross-account leak fix)', () => {
      useSubscriptionStore
        .getState()
        .setEntitlement({ ...FREE_ENTITLEMENT, tier: 'premium', allowMusicMode: true });

      gracefulReset();

      expect(useSubscriptionStore.getState().entitlement).toEqual(FREE_ENTITLEMENT);
      expect(useSubscriptionStore.getState().entitlement.tier).toBe('free');
      expect(useSubscriptionStore.getState().degraded).toBe(false);
    });

    it('preserves auth tokens', () => {
      gracefulReset();
      expect(useAuthStore.getState().accessToken).toBe('test-token');
    });

    it('removes specific localStorage keys', () => {
      localStorage.setItem('concord:dm-store', 'data');
      localStorage.setItem('concord-servers', 'data');
      localStorage.setItem('concord-channels', 'data');
      localStorage.setItem('concord:audio-advanced', 'should-stay');

      gracefulReset();

      expect(localStorage.getItem('concord:dm-store')).toBeNull();
      expect(localStorage.getItem('concord-servers')).toBeNull();
      expect(localStorage.getItem('concord-channels')).toBeNull();
      // Settings stores should NOT be touched
      expect(localStorage.getItem('concord:audio-advanced')).toBe('should-stay');
    });
  });

  describe('nuclearReset', () => {
    it('clears everything including auth', () => {
      nuclearReset();

      expect(useAuthStore.getState().accessToken).toBeNull();
      expect(useServerStore.getState().servers).toHaveLength(0);
    });

    it('preserves device-settings localStorage keys (#1603)', () => {
      // Device-local preferences survive logout-class resets; the old
      // wipe-everything contract reverted theme + A/V settings on every
      // logout. Detailed per-store coverage lives in
      // resetService.deviceSettings.test.ts.
      localStorage.setItem('concord-settings', 'device-pref');
      localStorage.setItem('concord:audio-advanced', 'device-pref');
      localStorage.setItem('concord:video-settings', 'device-pref');
      localStorage.setItem('concord:tts-settings', 'device-pref');

      nuclearReset();

      // The audio key is re-serialized by clearAllParticipantVolumes()'s
      // persist write-back, so assert presence, not raw-string equality.
      expect(localStorage.getItem('concord-settings')).toBe('device-pref');
      expect(localStorage.getItem('concord:audio-advanced')).not.toBeNull();
      expect(localStorage.getItem('concord:video-settings')).toBe('device-pref');
      expect(localStorage.getItem('concord:tts-settings')).toBe('device-pref');
    });

    it('clears user-scoped per-participant volume overrides (#1233 discipline)', () => {
      useAudioSettingsStore.getState().setParticipantVolume('other-user-id', 40);

      nuclearReset();

      expect(useAudioSettingsStore.getState().perParticipantVolume).toEqual({});
    });

    it('calls electron clearTokens', () => {
      const clearTokens = vi.fn();
      window.electron.clearTokens = clearTokens;

      nuclearReset();

      expect(clearTokens).toHaveBeenCalled();
    });
  });
});
