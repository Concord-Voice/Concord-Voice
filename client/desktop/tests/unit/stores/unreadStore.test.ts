import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import {
  isServerUnreadVisible,
  useNotificationPrefsStore,
} from '@/renderer/stores/ui/notificationPrefsStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockServer, mockServer2 } from '../../mocks/fixtures';

function visibleServerUnread(serverId: string): boolean {
  const { serverUnreadSet, serverUnreadPreciseSet } = useUnreadStore.getState();
  return isServerUnreadVisible(
    serverId,
    serverUnreadSet,
    serverUnreadPreciseSet,
    useNotificationPrefsStore.getState().mutedServers
  );
}

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

    it('clears the owning server indicator when its last eligible channel is read (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 3]]), 'server-1');
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerUnread('server-2', true);
      expect(visibleServerUnread('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-1');

      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
      expect(visibleServerUnread('server-2')).toBe(true);
      expect(
        visibleServerUnread('server-1'),
        'reading the last eligible channel must clear its owning server unread indicator'
      ).toBe(false);
    });

    it('keeps the owning indicator when another eligible channel is unread (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['channel-1', 3],
          ['channel-2', 2],
        ]),
        'server-1'
      );
      useUnreadStore.getState().markServerUnread('server-1', true);

      useUnreadStore.getState().clearUnread('channel-1');

      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
      expect(visibleServerUnread('server-1')).toBe(true);
    });

    it('retains muted residual counts without lighting the owning indicator (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['channel-1', 3],
          ['channel-2', 2],
        ]),
        'server-1'
      );
      useNotificationPrefsStore.getState().setMute('channel', 'channel-2', true, null);
      useUnreadStore.getState().markServerUnread('server-1', true);

      useUnreadStore.getState().clearUnread('channel-1');

      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
      expect(
        visibleServerUnread('server-1'),
        'muted-only residual counts must not keep the read server unread indicator lit'
      ).toBe(false);
    });

    it('keeps an explicitly unmuted residual channel visible under server mute (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(
        new Map([
          ['channel-1', 3],
          ['channel-2', 2],
        ]),
        'server-1'
      );
      useNotificationPrefsStore.getState().setMute('server', 'server-1', true, null);
      useNotificationPrefsStore.getState().setMute('channel', 'channel-2', false, null);
      useUnreadStore.getState().markServerUnread('server-1', true, true);

      useUnreadStore.getState().clearUnread('channel-1');

      expect(visibleServerUnread('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverUnreadChannelWinsSet.has('server-1')).toBe(true);
      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
    });

    it('does not clear either server from a channel snapshot with unknown ownership (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-unknown', 1]]));
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerUnread('server-2', true);

      useUnreadStore.getState().clearUnread('channel-unknown');

      expect(visibleServerUnread('server-1')).toBe(true);
      expect(visibleServerUnread('server-2')).toBe(true);
    });

    describe('Phase-8 review fixes (#3773)', () => {
      it.each([false, true])(
        'keeps a positively owned read conservative without a completed detailed seed (pending: %s)',
        (seedPending) => {
          useServerStore.setState({
            servers: [mockServer, mockServer2],
            activeServerId: 'server-1',
          });
          useUnreadStore.getState().setInitialUnreads(new Map([['channel-b1', 2]]), 'server-2');
          if (seedPending) useUnreadStore.getState().beginChannelUnreadSeed();
          // A live positive row establishes A1's owner, but says nothing about
          // other unread channels in A. B's complete snapshot cannot fill that gap.
          useUnreadStore.getState().bumpChannelUnread('channel-a1', 'server-1');
          useUnreadStore.getState().markServerUnread('server-1', true);
          useUnreadStore.getState().markServerMention('server-1');
          useUnreadStore.getState().markServerUnread('server-2', true);
          useUnreadStore.getState().markServerMention('server-2');
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-a1')).toEqual({
            serverId: 'server-1',
            count: 1,
          });
          expect(visibleServerUnread('server-1')).toBe(true);

          useUnreadStore.getState().clearUnread('channel-a1');

          expect(useUnreadStore.getState().allUnreadCounts.has('channel-a1')).toBe(false);
          expect(
            visibleServerUnread('server-1'),
            'a positive owner without complete A data must not clear A’s unread indicator'
          ).toBe(true);
          expect(
            useUnreadStore.getState().serverMentionSet.has('server-1'),
            'a positive owner without complete A data must not clear A’s mention indicator'
          ).toBe(true);
          expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(2);
          expect(visibleServerUnread('server-2')).toBe(true);
          expect(useUnreadStore.getState().serverMentionSet.has('server-2')).toBe(true);
        }
      );
    });

    it('does not use a departing channel to clear another server owning the active snapshot (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-2', 2]]), 'server-2');
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'channel-1', serverId: 'server-1', count: 1 },
        { channelId: 'channel-2', serverId: 'server-2', count: 2 },
      ]);
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerUnread('server-2', true);

      useUnreadStore.getState().clearUnread('channel-1');

      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
      expect(visibleServerUnread('server-2')).toBe(true);
    });

    it('preserves the retained snapshot owner when a newly active server channel is read (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map(), 'server-1');
      // B is active while its snapshot is pending. A's older snapshot remains,
      // and the live active-channel increment does not change that ownership.
      useServerStore.setState({ activeServerId: 'server-2' });
      useUnreadStore.getState().bumpChannelUnread('channel-a2', 'server-1');
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerMention('server-1');
      useUnreadStore.getState().bumpChannelUnread('channel-b2', 'server-2');
      useUnreadStore.getState().incrementUnread('channel-b2');
      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-b2')).toBe(1);
      expect(visibleServerUnread('server-1')).toBe(true);
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-b2');

      expect(useUnreadStore.getState().allUnreadCounts.has('channel-b2')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
        serverId: 'server-1',
        count: 1,
      });
      expect
        .soft(
          visibleServerUnread('server-1'),
          'reading newly active B must not erase the retained snapshot owner A unread indicator'
        )
        .toBe(true);
      expect
        .soft(
          useUnreadStore.getState().serverMentionSet.has('server-1'),
          'reading newly active B must not erase unread mentions in the retained snapshot owner A'
        )
        .toBe(true);
    });

    it('does not assign an unscoped count to the retained snapshot owner when it is read (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map(), 'server-1');
      // This generic writer has no server argument, and the old snapshot never
      // contained the channel. Its later presence supplies no ownership proof.
      useUnreadStore.getState().setUnreadCount('channel-unknown', 1);
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerMention('server-1');
      expect(useUnreadStore.getState().allUnreadCounts.has('channel-unknown')).toBe(false);
      expect(visibleServerUnread('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-unknown');

      expect(useUnreadStore.getState().unreadCounts.has('channel-unknown')).toBe(false);
      expect
        .soft(
          visibleServerUnread('server-1'),
          'reading an unscoped channel must not erase the retained snapshot owner unread indicator'
        )
        .toBe(true);
      expect
        .soft(
          useUnreadStore.getState().serverMentionSet.has('server-1'),
          'reading an unscoped channel must not erase the retained snapshot owner unread mentions'
        )
        .toBe(true);
    });

    it('keeps newer background unread in the same server when its older snapshot channel is read (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-a1', 1]]), 'server-1');
      useUnreadStore
        .getState()
        .setInitialChannelUnreads([{ channelId: 'channel-a1', serverId: 'server-1', count: 1 }]);
      useServerStore.setState({ activeServerId: 'server-2' });
      useUnreadStore.getState().bumpChannelUnread('channel-a2', 'server-1');
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerMention('server-1');
      expect(useUnreadStore.getState().unreadCounts.has('channel-a2')).toBe(false);
      expect(visibleServerUnread('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-a1');

      expect(useUnreadStore.getState().unreadCounts.size).toBe(0);
      expect(useUnreadStore.getState().allUnreadCounts.has('channel-a1')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
        serverId: 'server-1',
        count: 1,
      });
      expect
        .soft(
          visibleServerUnread('server-1'),
          'reading an older snapshot channel must retain newer same-server background unread visibility'
        )
        .toBe(true);
      expect
        .soft(
          useUnreadStore.getState().serverMentionSet.has('server-1'),
          'reading an older snapshot channel must retain newer same-server background unread mentions'
        )
        .toBe(true);
    });

    it('clears the owned server when its last unread exists only in cross-server counts (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map(), 'server-1');
      // A's authoritative empty snapshot remains while B's fetch is pending.
      // A's background arrival is positively tagged, without a generic count.
      useServerStore.setState({ activeServerId: 'server-2' });
      useUnreadStore.getState().bumpChannelUnread('channel-a2', 'server-1');
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerMention('server-1');
      useUnreadStore.getState().bumpChannelUnread('channel-b1', 'server-2');
      useUnreadStore.getState().incrementUnread('channel-b1');
      useUnreadStore.getState().markServerUnread('server-2', true);
      useUnreadStore.getState().incrementMention('channel-b1');
      useUnreadStore.getState().markServerMention('server-2');
      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
      expect(useUnreadStore.getState().unreadCounts.has('channel-a2')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-a2')).toEqual({
        serverId: 'server-1',
        count: 1,
      });
      expect(visibleServerUnread('server-1')).toBe(true);
      expect(visibleServerUnread('server-2')).toBe(true);
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-a2');

      expect(useUnreadStore.getState().allUnreadCounts.has('channel-a2')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
        serverId: 'server-2',
        count: 1,
      });
      expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(1);
      expect(useUnreadStore.getState().mentionCounts.get('channel-b1')).toBe(1);
      expect(visibleServerUnread('server-2')).toBe(true);
      expect(useUnreadStore.getState().serverMentionSet.has('server-2')).toBe(true);
      expect
        .soft(
          visibleServerUnread('server-1'),
          'reading the last positively owned cross-server count must clear its server indicator'
        )
        .toBe(false);
      expect
        .soft(
          useUnreadStore.getState().serverMentionSet.has('server-1'),
          'reading the last positively owned cross-server count must clear its server mention indicator'
        )
        .toBe(false);
    });

    it('clears the read server despite a foreign live channel in its retained snapshot map (#3773)', () => {
      useUnreadStore.getState().setInitialUnreads(new Map([['channel-a1', 1]]), 'server-1');
      useUnreadStore
        .getState()
        .setInitialChannelUnreads([{ channelId: 'channel-a1', serverId: 'server-1', count: 1 }]);
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().incrementMention('channel-a1');
      useUnreadStore.getState().markServerMention('server-1');

      // B's live channel enters the generic map before B's owning fetch lands.
      // Its cross-server entry positively identifies it as foreign to A.
      useServerStore.setState({ activeServerId: 'server-2' });
      useUnreadStore.getState().bumpChannelUnread('channel-b1', 'server-2');
      useUnreadStore.getState().incrementUnread('channel-b1');
      useUnreadStore.getState().markServerUnread('server-2', true);
      expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(1);
      expect(visibleServerUnread('server-1')).toBe(true);
      expect(visibleServerUnread('server-2')).toBe(true);
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(true);

      useUnreadStore.getState().clearUnread('channel-a1');

      expect(useUnreadStore.getState().allUnreadCounts.has('channel-a1')).toBe(false);
      expect(useUnreadStore.getState().mentionCounts.has('channel-a1')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-b1')).toEqual({
        serverId: 'server-2',
        count: 1,
      });
      expect(useUnreadStore.getState().unreadCounts.get('channel-b1')).toBe(1);
      expect(visibleServerUnread('server-2')).toBe(true);
      expect
        .soft(
          visibleServerUnread('server-1'),
          'a positively identified foreign unread channel must not keep the read server indicator lit'
        )
        .toBe(false);
      expect
        .soft(
          useUnreadStore.getState().serverMentionSet.has('server-1'),
          'a positively identified foreign unread channel must not retain the read server mention indicator'
        )
        .toBe(false);
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

  describe('allUnreadCounts (cross-server, #2403)', () => {
    beforeEach(() => {
      useUnreadStore.getState().clearAll();
    });

    it('seeds cross-server counts and keeps them addressable by channel', () => {
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'c1', serverId: 's1', count: 3 },
        { channelId: 'c2', serverId: 's2', count: 5 },
      ]);

      const all = useUnreadStore.getState().allUnreadCounts;
      expect(all.get('c1')).toEqual({ serverId: 's1', count: 3 });
      expect(all.get('c2')).toEqual({ serverId: 's2', count: 5 });
    });

    it('bumps a BACKGROUND server channel — the case unreadCounts cannot represent', () => {
      useUnreadStore.getState().bumpChannelUnread('c9', 's9');
      useUnreadStore.getState().bumpChannelUnread('c9', 's9');

      expect(useUnreadStore.getState().allUnreadCounts.get('c9')).toEqual({
        serverId: 's9',
        count: 2,
      });
      // unreadCounts is active-server-only and must NOT have been touched.
      expect(useUnreadStore.getState().unreadCounts.has('c9')).toBe(false);
    });

    it('clearUnread removes the channel from the cross-server map too', () => {
      useUnreadStore
        .getState()
        .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 3 }]);
      useUnreadStore.getState().clearUnread('c1');

      expect(useUnreadStore.getState().allUnreadCounts.has('c1')).toBe(false);
    });

    it('clearServerChannelUnreads drops only the departed server, leaving others intact', () => {
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'c1', serverId: 's1', count: 3 },
        { channelId: 'c2', serverId: 's1', count: 1 },
        { channelId: 'c3', serverId: 's2', count: 5 },
      ]);

      useUnreadStore.getState().clearServerChannelUnreads('s1');

      const all = useUnreadStore.getState().allUnreadCounts;
      expect(all.has('c1')).toBe(false);
      expect(all.has('c2')).toBe(false);
      // A different server's counts are untouched.
      expect(all.get('c3')).toEqual({ serverId: 's2', count: 5 });
    });

    it('clearServerChannelUnreads does NOT fire on the muted-only path', () => {
      // clearServerUnread has nine call sites and seven of them mean "every
      // unread channel here is muted, dim the dot" rather than "the server is
      // gone". Folding the channel-count purge into it would delete real unread
      // data on a mute — and require it to reappear when the mute expired. The
      // two must stay separate; this pins that.
      useUnreadStore
        .getState()
        .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 3 }]);

      useUnreadStore.getState().clearServerUnread('s1');

      expect(useUnreadStore.getState().allUnreadCounts.get('c1')).toEqual({
        serverId: 's1',
        count: 3,
      });
    });
  });
});
