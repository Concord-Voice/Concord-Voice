import { vi, describe, it, expect, beforeEach } from 'vitest';

const resetOrder = vi.hoisted(() => [] as string[]);

// Mock apiClient to prevent real HTTP calls
vi.mock('@/renderer/services/apiClient', () => ({
  stopProactiveRefresh: vi.fn(),
  refreshAccessToken: vi.fn(),
}));

vi.mock('@/renderer/services/preferencesSync', () => ({
  preferencesSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('preferences-sync-stop')),
    pushPreferences: vi.fn(),
  },
}));

vi.mock('@/renderer/services/savedGifsSync', () => ({
  savedGifsSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('saved-gifs-sync-stop')),
  },
}));

vi.mock('@/renderer/services/friendOrgSync', () => ({
  friendOrgSyncService: {
    stopWatching: vi.fn(() => resetOrder.push('friend-sync-stop')),
  },
}));

vi.mock('@/renderer/services/presenceOverrideSync', () => ({
  presenceOverrideSyncService: {
    reset: vi.fn(),
  },
}));

vi.mock('@/renderer/services/notificationPrefsService', () => ({
  stopExpirySweep: vi.fn(() => resetOrder.push('notification-sweep-stop')),
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
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';
import { usePresenceOverrideStore } from '@/renderer/stores/presenceOverrideStore';
import { useSavedGifsStore } from '@/renderer/stores/savedGifsStore';
import { useNotificationPrefsStore } from '@/renderer/stores/notificationPrefsStore';
import { preferencesSyncService } from '@/renderer/services/preferencesSync';
import { savedGifsSyncService } from '@/renderer/services/savedGifsSync';
import { friendOrgSyncService } from '@/renderer/services/friendOrgSync';
import { presenceOverrideSyncService } from '@/renderer/services/presenceOverrideSync';
import { stopExpirySweep } from '@/renderer/services/notificationPrefsService';
import { stopProactiveRefresh } from '@/renderer/services/apiClient';
import { mockServer } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';

beforeEach(() => {
  resetAllStores();
  resetOrder.length = 0;
  vi.mocked(friendOrgSyncService.stopWatching).mockClear();
  vi.mocked(preferencesSyncService.stopWatching).mockClear();
  vi.mocked(savedGifsSyncService.stopWatching).mockClear();
  vi.mocked(stopExpirySweep).mockClear();
  vi.mocked(stopProactiveRefresh).mockClear();
  vi.mocked(presenceOverrideSyncService.reset)
    .mockReset()
    .mockImplementation(() => {
      resetOrder.push('presence-sync-reset');
      usePresenceOverrideStore.getState().reset();
    });
  // Set up some state to verify it gets cleared
  useAuthStore.getState().setAccessToken('test-token');
  useServerStore.getState().addServer(mockServer);
  useUserStore.setState({
    user: { id: 'u1', username: 'test', email: 'test@test.com' },
  } as never);
});

describe('resetService', () => {
  describe('gracefulReset', () => {
    it('cancels encrypted-social sync before clearing either decrypted store', () => {
      useFriendOrgStore.getState()._hydrate({
        v: 1,
        categories: [
          {
            id: 'cat_prior',
            name: 'Prior account',
            emoji: '',
            color: null,
            memberIds: ['prior-user'],
          },
        ],
        sectionOrder: ['cat_prior'],
      });
      usePresenceOverrideStore.getState().apply(['11111111-1111-4111-8111-111111111111'], 7);
      useSavedGifsStore.getState().saveGif('prior-account-gif');
      useNotificationPrefsStore
        .getState()
        .setMute('server', '22222222-2222-4222-8222-222222222222', true, null);

      const unsubscribeFriend = useFriendOrgStore.subscribe((state, previous) => {
        if (previous.categories.length > 0 && state.categories.length === 0) {
          resetOrder.push('friend-store-reset');
        }
      });
      const unsubscribePresence = usePresenceOverrideStore.subscribe((state, previous) => {
        if (previous.excludedUserIds.length > 0 && state.excludedUserIds.length === 0) {
          resetOrder.push('presence-store-reset');
        }
      });

      try {
        gracefulReset();
      } finally {
        unsubscribeFriend();
        unsubscribePresence();
      }

      expect(resetOrder).toEqual([
        'preferences-sync-stop',
        'saved-gifs-sync-stop',
        'friend-sync-stop',
        'notification-sweep-stop',
        'presence-sync-reset',
        'presence-store-reset',
        'friend-store-reset',
      ]);
      expect(useFriendOrgStore.getState().categories).toEqual([]);
      expect(useFriendOrgStore.getState().sectionOrder).toEqual([]);
      expect(useSavedGifsStore.getState().gifs).toEqual([]);
      expect(useNotificationPrefsStore.getState().mutedServers.size).toBe(0);
      expect(usePresenceOverrideStore.getState()).toMatchObject({
        excludedUserIds: [],
        appliedVersion: 0,
        loading: false,
        saving: false,
        conflict: false,
        error: null,
      });
      expect(useAuthStore.getState().accessToken).toBe('test-token');
    });

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

    it('preserves proactive token refresh for a same-account reset', () => {
      gracefulReset();

      expect(stopProactiveRefresh).not.toHaveBeenCalled();
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
    it('inherits encrypted-social cancellation before clearing auth and tokens', () => {
      useFriendOrgStore.getState()._hydrate({
        v: 1,
        categories: [
          { id: 'cat_prior', name: 'Prior', emoji: '', color: null, memberIds: ['prior-user'] },
        ],
        sectionOrder: ['cat_prior'],
      });
      usePresenceOverrideStore.getState().apply(['11111111-1111-4111-8111-111111111111'], 3);
      const clearTokens = vi.fn(() => resetOrder.push('token-clear'));
      window.electron.clearTokens = clearTokens;

      nuclearReset();

      expect(resetOrder.slice(0, 5)).toEqual([
        'preferences-sync-stop',
        'saved-gifs-sync-stop',
        'friend-sync-stop',
        'notification-sweep-stop',
        'presence-sync-reset',
      ]);
      expect(resetOrder.at(-1)).toBe('token-clear');
      expect(useFriendOrgStore.getState().categories).toEqual([]);
      expect(usePresenceOverrideStore.getState().excludedUserIds).toEqual([]);
      expect(useAuthStore.getState().accessToken).toBeNull();
    });

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
