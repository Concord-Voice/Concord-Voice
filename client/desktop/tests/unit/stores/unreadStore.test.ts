import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('unreadStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  describe('setInitialUnreads', () => {
    it('sets initial counts from a map', () => {
      const counts = new Map([
        ['channel-1', 5],
        ['channel-2', 3],
      ]);
      useUnreadStore.getState().setInitialUnreads(counts);
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(5);
      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(3);
    });

    it('records the owning server id so the recompute can verify freshness', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]), 'server-1');
      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
    });

    it('defaults the owner to null when no server id is supplied', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]));
      expect(useUnreadStore.getState().unreadCountsServerId).toBeNull();
    });

    it('replaces the owner when a different server seeds its counts', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]), 'server-1');
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-9', 2]]), 'server-2');
      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });
  });

  describe('incrementUnread', () => {
    it('increments from zero', () => {
      useUnreadStore.getState().incrementUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
    });

    it('increments existing count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().incrementUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(6);
    });
  });

  describe('clearUnread', () => {
    it('removes a channel count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().clearUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });
  });

  describe('setUnreadCount', () => {
    it('removes entry when count is zero', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-1', 0);
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });
  });

  describe('serverUnreadSet', () => {
    it('tracks servers with unread messages', () => {
      useUnreadStore.getState().setInitialServerUnreads(['server-1', 'server-2']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
    });

    it('marks and clears server unreads', () => {
      useUnreadStore.getState().markServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      useUnreadStore.getState().clearServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
    });
  });

  describe('serverUnreadPreciseSet', () => {
    it('the bulk seed marks entries as approximate (not precise)', () => {
      useUnreadStore.getState().setInitialServerUnreads(['server-1']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
    });

    it('an approximate mark stays out of the precise set', () => {
      useUnreadStore.getState().markServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
    });

    it('a mute-aware mark records the server as precise', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
    });

    it('promotes an approximate entry to precise on a later mute-aware mark', () => {
      useUnreadStore.getState().markServerUnread('server-1'); // bulk-style approximate
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
      useUnreadStore.getState().markServerUnread('server-1', true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
    });

    it('clearServerUnread drops the precise flag too', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().clearServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
    });

    it('re-seeding preserves the precise flag for a server still unread in the seed', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
      // A concurrent/late bulk seed must not demote a mute-aware precise mark
      // for a server it still reports unread, or a channel-wins dot under a
      // muted server would drop until the next event.
      useUnreadStore.getState().setInitialServerUnreads(['server-1']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
    });

    it('re-seeding drops the precise flag for a server no longer in the seed', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      // server-1 is absent from the new seed: its dot is gone, so its precise
      // flag must go with it.
      useUnreadStore.getState().setInitialServerUnreads(['server-2']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
    });

    it('demoteServerPrecise drops only the precise flag, keeping the dot', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().demoteServerPrecise('server-1');
      // Still unread (the dot survives), but now approximate so it falls back
      // to the server-level mute gate.
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
    });

    it('demoteServerPrecise is a no-op for a server that was never precise', () => {
      useUnreadStore.getState().markServerUnread('server-1'); // approximate
      const before = useUnreadStore.getState().serverUnreadPreciseSet;
      useUnreadStore.getState().demoteServerPrecise('server-1');
      // Same Set identity: no state churn for a server that had no precise flag.
      expect(useUnreadStore.getState().serverUnreadPreciseSet).toBe(before);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
    });
  });

  // A channel-wins precise mark (from a channel explicitly unmuted under a muted
  // server) must survive a later server mute, so the background demote sweep can
  // tell it apart from a demotable server-fallback precise mark (3562576306).
  describe('serverUnreadChannelWinsSet', () => {
    it('records a channel-wins mark alongside the precise flag', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(true);
    });

    it('leaves a plain precise mark out of the channel-wins set', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
    });

    it('never records channel-wins for an approximate (non-precise) mark', () => {
      // channelWins only rides along with precise; an approximate mark can never
      // gain demote-immunity it did not earn through channel-wins resolution.
      useUnreadStore.getState().markServerUnread('server-1', false, true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
    });

    it('promotes an existing precise mark to channel-wins on a later mark', () => {
      useUnreadStore.getState().markServerUnread('server-1', true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(true);
    });

    it('demoteServerPrecise drops the channel-wins flag in lockstep', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      useUnreadStore.getState().demoteServerPrecise('server-1');
      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-1')).toBe(false);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
    });

    it('clearServerUnread drops the channel-wins flag too', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      useUnreadStore.getState().clearServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
    });

    it('re-seeding preserves the channel-wins flag for a server still unread', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      useUnreadStore.getState().setInitialServerUnreads(['server-1', 'server-2']);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(true);
    });

    it('re-seeding drops the channel-wins flag for a server no longer in the seed', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      useUnreadStore.getState().setInitialServerUnreads(['server-2']);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(false);
    });
  });

  describe('clearAll', () => {
    it('clears all channel unreads', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
      useUnreadStore.getState().clearAll();
      expect(useUnreadStore.getState().unreadCounts.size).toBe(0);
    });

    it('resets the counts owner', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 5]]), 'server-1');
      useUnreadStore.getState().clearAll();
      expect(useUnreadStore.getState().unreadCountsServerId).toBeNull();
    });

    it('resets the precise and channel-wins server sets', () => {
      useUnreadStore.getState().markServerUnread('server-1', true, true);
      useUnreadStore.getState().clearAll();
      expect(useUnreadStore.getState().serverUnreadPreciseSet.size).toBe(0);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.size).toBe(0);
    });
  });
});
