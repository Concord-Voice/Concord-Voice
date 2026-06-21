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

    it('removes ALL persisted localStorage keys', () => {
      localStorage.setItem('concord-layout', 'data');
      localStorage.setItem('concord-settings', 'data');
      localStorage.setItem('concord:audio-advanced', 'data');
      localStorage.setItem('concord:video-settings', 'data');
      localStorage.setItem('concord:tts-settings', 'data');

      nuclearReset();

      expect(localStorage.getItem('concord-layout')).toBeNull();
      expect(localStorage.getItem('concord-settings')).toBeNull();
      expect(localStorage.getItem('concord:audio-advanced')).toBeNull();
      expect(localStorage.getItem('concord:video-settings')).toBeNull();
      expect(localStorage.getItem('concord:tts-settings')).toBeNull();
    });

    it('calls electron clearTokens', () => {
      const clearTokens = vi.fn();
      window.electron.clearTokens = clearTokens;

      nuclearReset();

      expect(clearTokens).toHaveBeenCalled();
    });
  });
});
