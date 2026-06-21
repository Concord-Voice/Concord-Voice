import { describe, it, expect, beforeEach, vi, afterEach } from 'vitest';
import {
  useNotificationPrefsStore,
  isChannelMuted,
  isDMMuted,
  type PreferenceWire,
} from '@/renderer/stores/notificationPrefsStore';
import { resetAllStores } from '../../helpers/store-helpers';

const SERVER_ID = '11111111-1111-1111-1111-111111111111';
const CHANNEL_ID = '22222222-2222-2222-2222-222222222222';
const DM_ID = '33333333-3333-3333-3333-333333333333';

// Build a wire-shape preference row with sensible defaults; tests override
// only the fields they care about.
function wire(overrides: Partial<PreferenceWire>): PreferenceWire {
  return {
    target_type: 'server',
    target_id: SERVER_ID,
    muted: true,
    muted_until: null,
    updated_at: new Date().toISOString(),
    ...overrides,
  };
}

describe('notificationPrefsStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  describe('setInitialPreferences', () => {
    it('splits wire rows into the three target-typed maps', () => {
      useNotificationPrefsStore
        .getState()
        .setInitialPreferences([
          wire({ target_type: 'server', target_id: SERVER_ID }),
          wire({ target_type: 'channel', target_id: CHANNEL_ID }),
          wire({ target_type: 'dm', target_id: DM_ID }),
        ]);
      const s = useNotificationPrefsStore.getState();
      expect(s.mutedServers.size).toBe(1);
      expect(s.mutedChannels.size).toBe(1);
      expect(s.mutedDMs.size).toBe(1);
      expect(s.mutedServers.get(SERVER_ID)?.muted).toBe(true);
    });

    it('parses muted_until from RFC3339 into a Date', () => {
      const future = new Date(Date.now() + 60_000).toISOString();
      useNotificationPrefsStore.getState().setInitialPreferences([wire({ muted_until: future })]);
      const entry = useNotificationPrefsStore.getState().mutedServers.get(SERVER_ID);
      expect(entry?.mutedUntil).toBeInstanceOf(Date);
      expect(entry?.mutedUntil?.toISOString()).toBe(future);
    });

    it('replaces existing state on a subsequent hydration', () => {
      const store = useNotificationPrefsStore.getState();
      store.setInitialPreferences([wire({ target_id: SERVER_ID })]);
      // A second hydration with an empty payload must wipe the prior entry —
      // a stale row would mean the user sees a mute they have already removed
      // on another device.
      store.setInitialPreferences([]);
      expect(useNotificationPrefsStore.getState().mutedServers.size).toBe(0);
    });
  });

  describe('setMute', () => {
    it('upserts a server mute', () => {
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      expect(useNotificationPrefsStore.getState().mutedServers.get(SERVER_ID)?.muted).toBe(true);
    });

    it('upserts an explicit unmute (muted=false stays in the map)', () => {
      // The Discord-style override: a channel set to muted=false MUST persist
      // as a row so it can defeat its server's mute. Removing the row would
      // collapse "explicitly unmuted" into "no opinion" — wrong outcome.
      useNotificationPrefsStore.getState().setMute('channel', CHANNEL_ID, false, null);
      const entry = useNotificationPrefsStore.getState().mutedChannels.get(CHANNEL_ID);
      expect(entry).toBeDefined();
      expect(entry?.muted).toBe(false);
    });

    it('routes to the correct map based on targetType', () => {
      const store = useNotificationPrefsStore.getState();
      store.setMute('server', SERVER_ID, true, null);
      store.setMute('channel', CHANNEL_ID, true, null);
      store.setMute('dm', DM_ID, true, null);
      const s = useNotificationPrefsStore.getState();
      expect(s.mutedServers.has(SERVER_ID)).toBe(true);
      expect(s.mutedChannels.has(CHANNEL_ID)).toBe(true);
      expect(s.mutedDMs.has(DM_ID)).toBe(true);
    });
  });

  describe('removeMute', () => {
    it('removes only the targeted row', () => {
      const store = useNotificationPrefsStore.getState();
      store.setMute('server', SERVER_ID, true, null);
      store.setMute('channel', CHANNEL_ID, true, null);
      store.removeMute('server', SERVER_ID);
      const s = useNotificationPrefsStore.getState();
      expect(s.mutedServers.has(SERVER_ID)).toBe(false);
      expect(s.mutedChannels.has(CHANNEL_ID)).toBe(true);
    });

    it('is a no-op when the row does not exist', () => {
      // Returning early preserves referential equality of the maps, which
      // avoids unnecessary re-renders for subscribers using shallow equality.
      const before = useNotificationPrefsStore.getState().mutedServers;
      useNotificationPrefsStore.getState().removeMute('server', SERVER_ID);
      const after = useNotificationPrefsStore.getState().mutedServers;
      expect(after).toBe(before);
    });
  });

  describe('clearExpiredMutes', () => {
    it('drops entries whose mutedUntil is in the past', () => {
      const past = new Date(Date.now() - 10_000);
      const future = new Date(Date.now() + 60_000);
      const store = useNotificationPrefsStore.getState();
      store.setMute('server', SERVER_ID, true, past);
      store.setMute('channel', CHANNEL_ID, true, future);
      store.clearExpiredMutes();
      const s = useNotificationPrefsStore.getState();
      expect(s.mutedServers.has(SERVER_ID)).toBe(false);
      expect(s.mutedChannels.has(CHANNEL_ID)).toBe(true);
    });

    it('keeps indefinite mutes (mutedUntil=null)', () => {
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      useNotificationPrefsStore.getState().clearExpiredMutes();
      expect(useNotificationPrefsStore.getState().mutedServers.has(SERVER_ID)).toBe(true);
    });

    it('preserves referential equality when nothing expired (no spurious re-renders)', () => {
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      const before = useNotificationPrefsStore.getState().mutedServers;
      useNotificationPrefsStore.getState().clearExpiredMutes();
      const after = useNotificationPrefsStore.getState().mutedServers;
      expect(after).toBe(before);
    });
  });
});

describe('notificationPrefsStore selectors', () => {
  beforeEach(() => {
    resetAllStores();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  describe('isChannelMuted resolution order', () => {
    it('uses the channel pref outright when present (mute)', () => {
      useNotificationPrefsStore.getState().setMute('channel', CHANNEL_ID, true, null);
      expect(isChannelMuted(CHANNEL_ID, SERVER_ID)).toBe(true);
    });

    it('uses the channel pref outright when present (explicit unmute beats server mute)', () => {
      // This is THE invariant from the issue spec. Verify directly.
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      useNotificationPrefsStore.getState().setMute('channel', CHANNEL_ID, false, null);
      expect(isChannelMuted(CHANNEL_ID, SERVER_ID)).toBe(false);
    });

    it('falls back to the server pref when the channel has no opinion', () => {
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      expect(isChannelMuted(CHANNEL_ID, SERVER_ID)).toBe(true);
    });

    it('returns false when neither channel nor server has an entry', () => {
      expect(isChannelMuted(CHANNEL_ID, SERVER_ID)).toBe(false);
    });

    it('ignores server fallback when serverId is null', () => {
      useNotificationPrefsStore.getState().setMute('server', SERVER_ID, true, null);
      // serverId=null is the DM-style caller — they don't know about a server,
      // so a server mute must not leak into the DM mute decision.
      expect(isChannelMuted(CHANNEL_ID, null)).toBe(false);
    });
  });

  describe('isDMMuted', () => {
    it('returns false when no entry exists', () => {
      expect(isDMMuted(DM_ID)).toBe(false);
    });

    it('returns true for an active mute', () => {
      useNotificationPrefsStore.getState().setMute('dm', DM_ID, true, null);
      expect(isDMMuted(DM_ID)).toBe(true);
    });

    it('honors expiry', () => {
      useNotificationPrefsStore.getState().setMute('dm', DM_ID, true, new Date(Date.now() - 1_000));
      expect(isDMMuted(DM_ID)).toBe(false);
    });
  });
});
