import { describe, it, expect, beforeEach, vi } from 'vitest';
import { useUnreadStore } from '../../../src/renderer/stores/chat/unreadStore';
import { useDMStore } from '../../../src/renderer/stores/chat/dmStore';
import { useNotificationPrefsStore } from '../../../src/renderer/stores/ui/notificationPrefsStore';
import { useServerStore } from '../../../src/renderer/stores/chat/serverStore';

const mockSetBadgeCount = vi.fn();
// `tests/setup.ts` defines `window.electron` for every jsdom test, and in jsdom
// `window` IS `globalThis` — so the bridge is there. This used to be an `if`,
// which is the wrong shape for a precondition: were that ever to stop holding,
// the mock would silently not be installed, every assertion below would run
// against a spy the production code never reaches, and the suite would go GREEN
// while testing nothing. A missing bridge is a broken harness, not a case to
// skip.
if (!globalThis.electron) {
  throw new Error(
    'badgeSync tests require the preload bridge from tests/setup.ts — without it ' +
      'the setBadgeCount mock is never installed and every assertion here is vacuous'
  );
}
(globalThis.electron as Record<string, unknown>).setBadgeCount = mockSetBadgeCount;

const { computeBadgeTotal, startBadgeSync } =
  await import('../../../src/renderer/services/system/badgeSync');

describe('badgeSync', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useUnreadStore.getState().clearAll();
    useNotificationPrefsStore.setState({
      mutedChannels: new Map(),
      mutedServers: new Map(),
      mutedDMs: new Map(),
    });
    useDMStore.setState({ conversations: [] });
    useServerStore.setState({ activeServerId: null } as never);
  });

  it('sums unmuted channel unread and DM unread', () => {
    useUnreadStore.getState().setInitialChannelUnreads([
      { channelId: 'c1', serverId: 's1', count: 3 },
      { channelId: 'c2', serverId: 's2', count: 2 },
    ]);
    useDMStore.setState({
      conversations: [{ id: 'd1', unreadCount: 4 }],
    } as never);

    expect(computeBadgeTotal()).toBe(9);
  });

  it('excludes a muted channel', () => {
    useUnreadStore.getState().setInitialChannelUnreads([
      { channelId: 'c1', serverId: 's1', count: 3 },
      { channelId: 'c2', serverId: 's2', count: 2 },
    ]);
    useNotificationPrefsStore.setState({
      mutedChannels: new Map([['c1', { muted: true, mutedUntil: null }]]),
    } as never);

    expect(computeBadgeTotal()).toBe(2);
  });

  it('excludes a muted DM — the other half of the sum', () => {
    useDMStore.setState({
      conversations: [
        { id: 'd1', unreadCount: 4 },
        { id: 'd2', unreadCount: 1 },
      ],
    } as never);
    useNotificationPrefsStore.setState({
      mutedDMs: new Map([['d1', { muted: true, mutedUntil: null }]]),
    } as never);

    expect(computeBadgeTotal()).toBe(1);
  });

  // THE REGRESSION TEST FOR #2403. This is the assertion that fails today.
  it('clears the badge when the messages are read', () => {
    const stop = startBadgeSync();

    useUnreadStore
      .getState()
      .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 2 }]);
    expect(mockSetBadgeCount).toHaveBeenLastCalledWith(2);

    // Read it the ordinary way — no notification click anywhere.
    useUnreadStore.getState().clearUnread('c1');
    expect(mockSetBadgeCount).toHaveBeenLastCalledWith(0);

    stop();
  });

  it('asserts the badge on start, so a reload cannot strand a stale value', () => {
    useUnreadStore
      .getState()
      .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 7 }]);
    vi.clearAllMocks();

    const stop = startBadgeSync();
    expect(mockSetBadgeCount).toHaveBeenCalledWith(7);

    stop();
  });

  it('does not re-push an unchanged total', () => {
    const stop = startBadgeSync();
    vi.clearAllMocks();

    useDMStore.setState({ conversations: [] } as never);
    expect(mockSetBadgeCount).not.toHaveBeenCalled();

    stop();
  });
  // The core of #2403's fix: ONE channel's count comes from exactly one map.
  it('sources the active server from unreadCounts and never double-counts it', () => {
    useServerStore.setState({ activeServerId: 's1' } as never);
    useUnreadStore.getState().setInitialChannelUnreads([
      { channelId: 'c1', serverId: 's1', count: 2 }, // stale cross-server copy
      { channelId: 'c9', serverId: 's2', count: 4 }, // background server
    ]);
    // The authoritative active-server value for the same channel.
    useUnreadStore.getState().setInitialUnreads(new Map([['c1', 3]]), 's1');

    // s1 from unreadCounts (3) + s2 from allUnreadCounts (4). Summing both maps
    // would give 9 and put a number on the Dock for a conversation on screen.
    expect(computeBadgeTotal()).toBe(7);
  });

  it('falls back to allUnreadCounts when unreadCounts belongs to the previous server', () => {
    // `unreadCounts` is replaced wholesale on a server switch and is NOT cleared
    // on the way out, so immediately after a switch it still holds the PREVIOUS
    // server's counts. Reading it as the new server's would be wrong; reading
    // nothing would drop the server entirely. A slightly stale count is better.
    useServerStore.setState({ activeServerId: 's2' } as never);
    useUnreadStore
      .getState()
      .setInitialChannelUnreads([{ channelId: 'c9', serverId: 's2', count: 4 }]);
    useUnreadStore.getState().setInitialUnreads(new Map([['c1', 3]]), 's1'); // stale

    expect(computeBadgeTotal()).toBe(4);
  });

  // The third mute map. mutedChannels and mutedDMs are covered above; this one
  // suppresses every channel in a server at once and had no case of its own.
  it('excludes every channel in a muted server', () => {
    useUnreadStore.getState().setInitialChannelUnreads([
      { channelId: 'c1', serverId: 's1', count: 3 },
      { channelId: 'c2', serverId: 's1', count: 5 },
      { channelId: 'c9', serverId: 's2', count: 2 },
    ]);
    useNotificationPrefsStore.setState({
      mutedServers: new Map([['s1', { muted: true, mutedUntil: null }]]),
    } as never);

    expect(computeBadgeTotal()).toBe(2);
  });

  it('stops pushing once the returned unsubscribe runs', () => {
    const stop = startBadgeSync();
    useUnreadStore
      .getState()
      .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 2 }]);
    expect(mockSetBadgeCount).toHaveBeenLastCalledWith(2);

    stop();
    vi.clearAllMocks();

    // Every subscribed store, including serverStore — a listener left attached
    // survives a logout and pushes another account's numbers to the OS.
    useUnreadStore
      .getState()
      .setInitialChannelUnreads([{ channelId: 'c1', serverId: 's1', count: 99 }]);
    useDMStore.setState({ conversations: [{ id: 'd1', unreadCount: 5 }] } as never);
    useNotificationPrefsStore.setState({ mutedChannels: new Map() } as never);
    useServerStore.setState({ activeServerId: 's7' } as never);

    expect(mockSetBadgeCount).not.toHaveBeenCalled();
  });
});
