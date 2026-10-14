import { StrictMode, createElement, useEffect } from 'react';
import { render as bareRender, renderHook, act } from '@testing-library/react';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import {
  isServerUnreadVisible,
  useNotificationPrefsStore,
} from '@/renderer/stores/ui/notificationPrefsStore';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockChannel, mockServer, mockServer2 } from '../../mocks/fixtures';

// Mock websocketService
const mockSubscribeServer = vi.fn();
const mockUnsubscribeServer = vi.fn();
vi.mock('@/renderer/services/messaging/websocketService', () => ({
  getWebSocketService: () => ({
    subscribeServer: mockSubscribeServer,
    unsubscribeServer: mockUnsubscribeServer,
  }),
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import { useServerChannelSubscriptions } from '@/renderer/hooks/messaging/useServerChannelSubscriptions';

function visibleServerUnread(serverId: string): boolean {
  const { serverUnreadSet, serverUnreadPreciseSet } = useUnreadStore.getState();
  return isServerUnreadVisible(
    serverId,
    serverUnreadSet,
    serverUnreadPreciseSet,
    useNotificationPrefsStore.getState().mutedServers
  );
}

function deferred<T>() {
  let resolve!: (value: T) => void;
  const promise = new Promise<T>((fulfill) => {
    resolve = fulfill;
  });
  return { promise, resolve };
}

describe('useServerChannelSubscriptions', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockApiFetch.mockReset();
    // Older cases replace store actions with spies; clearing their call history
    // does not restore the real actions needed by the behavioral regressions.
    useUnreadStore.setState(useUnreadStore.getInitialState(), true);
    resetAllStores();
    mockApiFetch.mockResolvedValue({
      ok: true,
      json: async () => ({ server_ids: [], unreads: [] }),
    });
  });

  it('does nothing when disconnected', () => {
    useChatStore.setState({ isConnected: false });
    useServerStore.setState({ servers: [mockServer] });
    renderHook(() => useServerChannelSubscriptions());
    expect(mockSubscribeServer).not.toHaveBeenCalled();
  });

  it('does nothing when no servers', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [] });
    renderHook(() => useServerChannelSubscriptions());
    expect(mockSubscribeServer).not.toHaveBeenCalled();
  });

  it('subscribes to servers when connected', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer, mockServer2] });
    renderHook(() => useServerChannelSubscriptions());
    expect(mockSubscribeServer).toHaveBeenCalledWith('server-1');
    expect(mockSubscribeServer).toHaveBeenCalledWith('server-2');
  });

  it('fetches server unread status when connected', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer] });
    renderHook(() => useServerChannelSubscriptions());
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/unread-status');
  });

  it('fetches per-channel unreads for active server', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    renderHook(() => useServerChannelSubscriptions());
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/server-1/unread');
  });

  it('does not fetch per-channel unreads when no active server', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: null });
    renderHook(() => useServerChannelSubscriptions());
    // Should fetch server unreads but not per-channel (server-1/unread pattern)
    expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/unread-status');
    expect(mockApiFetch).not.toHaveBeenCalledWith(
      expect.stringMatching(/\/servers\/server-\d+\/unread/)
    );
  });

  it('does not unsubscribe on unmount (subscriptions are long-lived)', () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer] });
    const { unmount } = renderHook(() => useServerChannelSubscriptions());
    unmount();
    // Server subscriptions persist — WS disconnect/reconnect handles lifecycle.
    // Removing cleanup prevents StrictMode double-mount thrashing.
    expect(mockUnsubscribeServer).not.toHaveBeenCalled();
  });

  it('logs error when server unread-status fetch throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    mockApiFetch.mockRejectedValueOnce(new Error('network down'));
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer] });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        'Failed to fetch server unread status:',
        'network down'
      );
    });
    consoleSpy.mockRestore();
  });

  it('logs error when per-channel unread fetch throws', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    // First call (unread-status) succeeds; second call (per-channel) throws
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockRejectedValueOnce(new Error('channel fetch failed'));
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith(
        'Failed to fetch unread counts:',
        'channel fetch failed'
      );
    });
    consoleSpy.mockRestore();
  });

  it('does not light the server dot when every unread channel is muted (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    // Every unread channel is muted at the channel level (channel-wins).
    useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);
    useNotificationPrefsStore.getState().setMute('channel', 'channel-2', true, null);
    // First call: server-level unread-status. Second: per-channel unreads.
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          unreads: [
            { channel_id: 'channel-1', unread_count: 3 },
            { channel_id: 'channel-2', unread_count: 1 },
          ],
        }),
      });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(clearServerUnread).toHaveBeenCalledWith('server-1');
    });
    expect(markServerUnread).not.toHaveBeenCalled();
    // Counts stay seeded either way, so un-muting reveals the badge without a refetch.
    expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(3);
    expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(1);
  });

  it('lights the server dot when at least one unread channel is not muted (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    // channel-1 muted, channel-2 has no opinion (not muted).
    useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          unreads: [
            { channel_id: 'channel-1', unread_count: 3 },
            { channel_id: 'channel-2', unread_count: 1 },
          ],
        }),
      });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(markServerUnread).toHaveBeenCalledWith('server-1', true);
    });
    expect(clearServerUnread).not.toHaveBeenCalled();
    expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(3);
    expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(1);
  });

  it('clears the server dot when the last unmuted unread channel is muted at runtime (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    // The one unread channel starts unmuted, so the fetch lights the dot.
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ unreads: [{ channel_id: 'channel-1', unread_count: 3 }] }),
      });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(markServerUnread).toHaveBeenCalledWith('server-1', true);
    });
    markServerUnread.mockClear();
    clearServerUnread.mockClear();

    // Muting the channel must recompute the dot without waiting for a refetch.
    act(() => {
      useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);
    });

    expect(clearServerUnread).toHaveBeenCalledWith('server-1');
    expect(markServerUnread).not.toHaveBeenCalled();
    // Count stays seeded so un-muting can reveal the dot again with no refetch.
    expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(3);
  });

  it('reveals the server dot when a muted unread channel is un-muted at runtime (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    // The only unread channel starts muted, so the fetch leaves the dot dark.
    useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ unreads: [{ channel_id: 'channel-1', unread_count: 3 }] }),
      });

    renderHook(() => useServerChannelSubscriptions());

    await vi.waitFor(() => {
      expect(clearServerUnread).toHaveBeenCalledWith('server-1');
    });
    markServerUnread.mockClear();
    clearServerUnread.mockClear();

    // Un-muting recomputes and brings the dot back — the seeded count is reused.
    act(() => {
      useNotificationPrefsStore.getState().setMute('channel', 'channel-1', false, null);
    });

    expect(markServerUnread).toHaveBeenCalledWith('server-1', true);
    expect(clearServerUnread).not.toHaveBeenCalled();
    expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(3);
  });

  // The recompute can only inspect the ACTIVE server's per-channel data, so a
  // BACKGROUND server that was marked precise (channel-wins) and is then
  // server-muted would keep a stale precise dot forever. Muting it must demote
  // its precise flag so it falls back to the server-level mute gate and goes
  // dark (#84 / epic #1029 close audit, P2 follow-up).
  it('demotes a background muted server’s precise flag on mute change', async () => {
    const demoteServerPrecise = vi.fn();
    useUnreadStore.setState({
      demoteServerPrecise,
      serverUnreadSet: new Set(['server-2']),
      serverUnreadPreciseSet: new Set(['server-2']),
    });
    useChatStore.setState({ isConnected: true });
    // server-1 active, server-2 in the background.
    useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });

    renderHook(() => useServerChannelSubscriptions());

    // server-2 is precise but not yet muted, so mount does not demote it.
    expect(demoteServerPrecise).not.toHaveBeenCalled();

    // Muting the background server reconciles its now-unverifiable precise flag.
    act(() => {
      useNotificationPrefsStore.getState().setMute('server', 'server-2', true, null);
    });

    expect(demoteServerPrecise).toHaveBeenCalledWith('server-2');
  });

  // 3562576306: a background server's precise mark that came from a channel-wins
  // override (a channel explicitly unmuted under a muted server) is exactly what
  // isServerUnreadVisible honors, so the background sweep must NOT demote it when
  // a server mute is toggled; otherwise the dot vanishes until the server is
  // reopened or a new unread lands.
  it('does not demote a background channel-wins precise flag on mute change', async () => {
    const demoteServerPrecise = vi.fn();
    useUnreadStore.setState({
      demoteServerPrecise,
      serverUnreadSet: new Set(['server-2']),
      serverUnreadPreciseSet: new Set(['server-2']),
      serverUnreadChannelWinsSet: new Set(['server-2']),
    });
    useChatStore.setState({ isConnected: true });
    // server-1 active, server-2 in the background with a channel-wins unread.
    useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });

    renderHook(() => useServerChannelSubscriptions());

    // Muting the background server (or, since this effect reruns on any mute
    // toggle, an unrelated one) must leave the channel-wins precise flag intact.
    act(() => {
      useNotificationPrefsStore.getState().setMute('server', 'server-2', true, null);
    });

    expect(demoteServerPrecise).not.toHaveBeenCalled();
  });

  // The active server is reconciled by the per-channel recompute (which honors
  // channel-wins), so it must NOT be blindly demoted by the background sweep.
  it('does not demote the active server’s precise flag on mute change', async () => {
    const demoteServerPrecise = vi.fn();
    useUnreadStore.setState({
      demoteServerPrecise,
      serverUnreadSet: new Set(['server-1']),
      serverUnreadPreciseSet: new Set(['server-1']),
    });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });

    renderHook(() => useServerChannelSubscriptions());

    act(() => {
      useNotificationPrefsStore.getState().setMute('server', 'server-1', true, null);
    });

    expect(demoteServerPrecise).not.toHaveBeenCalled();
  });

  // #4 (3562576311): the global unreadCounts is not cleared on server switch, so
  // in the window before the newly opened server's /unread lands it still holds
  // the PREVIOUS server's channel IDs. A mute change in that window must not
  // pair those stale IDs with the new server's mute state and toggle the wrong
  // dot: the recompute is gated on unreadCountsServerId === activeServerId.
  it('does not recompute the active dot from another server’s stale counts (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });

    // server-1 lands with one unmuted unread (owner becomes 'server-1');
    // server-2's per-channel fetch hangs so ownership never advances to it.
    mockApiFetch
      .mockResolvedValueOnce({ ok: true, json: async () => ({ server_ids: [] }) })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ unreads: [{ channel_id: 'channel-1', unread_count: 3 }] }),
      })
      .mockImplementationOnce(() => new Promise(() => {}));

    renderHook(() => useServerChannelSubscriptions());
    await vi.waitFor(() => {
      expect(markServerUnread).toHaveBeenCalledWith('server-1', true);
    });
    markServerUnread.mockClear();
    clearServerUnread.mockClear();

    // Open server-2; its fetch is still pending, so unreadCounts (and its owner)
    // still describe server-1.
    act(() => {
      useServerStore.setState({ activeServerId: 'server-2' });
    });

    // Mute server-2 during that window. The recompute must skip: server-1's
    // channel-1 count must not mark or clear server-2's dot.
    act(() => {
      useNotificationPrefsStore.getState().setMute('server', 'server-2', true, null);
    });

    expect(markServerUnread).not.toHaveBeenCalledWith('server-2', expect.anything());
    expect(clearServerUnread).not.toHaveBeenCalledWith('server-2');
  });

  // #1 (3562576303): the mute-UNAWARE bulk /servers/unread-status seed races the
  // active server's per-channel fetch. If it lands AFTER that fetch cleared a
  // muted-only active server, it re-adds the server as an approximate unread
  // that isServerUnreadVisible shows permanently. The bulk handler reconciles
  // the active server from its own per-channel counts so the dot goes back dark.
  it('reconciles a muted-only active server re-added by a late bulk seed (#84 / epic #1029 close audit)', async () => {
    const markServerUnread = vi.fn();
    const clearServerUnread = vi.fn();
    useUnreadStore.setState({ markServerUnread, clearServerUnread });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: 'server-1' });
    // The only unread channel is muted, so the per-channel fetch clears the dot.
    useNotificationPrefsStore.getState().setMute('channel', 'channel-1', true, null);

    // Bulk seed (call #1) is held open so it resolves AFTER the per-channel
    // fetch (call #2), reproducing the late-seed race.
    let resolveBulk!: (value: unknown) => void;
    mockApiFetch
      .mockImplementationOnce(() => new Promise((r) => (resolveBulk = r)))
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ unreads: [{ channel_id: 'channel-1', unread_count: 3 }] }),
      });

    renderHook(() => useServerChannelSubscriptions());

    // Per-channel fetch lands first: muted-only server → dot cleared.
    await vi.waitFor(() => {
      expect(clearServerUnread).toHaveBeenCalledWith('server-1');
    });
    markServerUnread.mockClear();
    clearServerUnread.mockClear();

    // Now the late bulk seed lands and re-adds server-1 as approximate.
    await act(async () => {
      resolveBulk({ ok: true, json: async () => ({ server_ids: ['server-1'] }) });
      await new Promise((r) => setTimeout(r, 0));
    });

    // Reconcile re-derives from the seeded per-channel counts: only muted
    // unreads remain, so the dot is cleared again rather than left lit.
    expect(clearServerUnread).toHaveBeenCalledWith('server-1');
    expect(markServerUnread).not.toHaveBeenCalled();
  });
  // --- The bulk seed's commit fence (#2403, CodeRabbit follow-up) ---------

  it('does not commit a bulk seed whose connection ended while it was in flight', async () => {
    const setInitialChannelUnreads = vi.fn();
    useUnreadStore.setState({ setInitialChannelUnreads });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: null });

    let resolveBulk!: (value: unknown) => void;
    mockApiFetch.mockImplementationOnce(() => new Promise((r) => (resolveBulk = r)));

    const { rerender } = renderHook(() => useServerChannelSubscriptions());

    // The connection drops while the request is still open. That invalidates the
    // seed: its response describes a session that has ended.
    act(() => {
      useChatStore.setState({ isConnected: false });
    });
    rerender();

    await act(async () => {
      resolveBulk({
        ok: true,
        json: async () => ({
          server_ids: ['server-1'],
          channels: [{ channel_id: 'channel-1', server_id: 'server-1', unread_count: 4 }],
        }),
      });
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(setInitialChannelUnreads).not.toHaveBeenCalled();
  });

  // The fence is checked TWICE, after each await, and the two cover different
  // windows. The case above drops the connection while `apiFetch` is open, where
  // either check catches it — so it proves the seed is fenced, but not which
  // check did it. This one drops the connection while `res.json()` is open,
  // which ONLY the second check can see.
  it('does not commit when the connection ends while the body is still parsing', async () => {
    const setInitialChannelUnreads = vi.fn();
    useUnreadStore.setState({ setInitialChannelUnreads });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: null });

    let resolveJson!: (value: unknown) => void;
    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      // Resolves the response but holds the BODY open, so the continuation is
      // parked between the two fences.
      json: () => new Promise((r) => (resolveJson = r)),
    });

    const { rerender } = renderHook(() => useServerChannelSubscriptions());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    act(() => {
      useChatStore.setState({ isConnected: false });
    });
    rerender();

    await act(async () => {
      resolveJson({
        server_ids: ['server-1'],
        channels: [{ channel_id: 'channel-1', server_id: 'server-1', unread_count: 4 }],
      });
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(setInitialChannelUnreads).not.toHaveBeenCalled();
  });

  // Fence 1's UNIQUE branch. Fence 2 sits after `res.json()`, which the non-ok
  // arm never reaches, so the only thing standing between a stale FAILURE and a
  // current request's latch is the check right after `apiFetch`.
  //
  // Note what is NOT the defect: a separate, pre-existing effect deliberately
  // clears `unreadsFetchedRef` on disconnect so the seed refetches on reconnect.
  // The second request below is therefore correct. The defect is the THIRD — a
  // stale failure releasing the latch that the live second request is holding.
  it('does not let a stale failure release the live request’s latch', async () => {
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: null });

    let failStale!: (value: unknown) => void;
    mockApiFetch
      .mockImplementationOnce(() => new Promise((r) => (failStale = r))) // #1
      .mockImplementationOnce(() => new Promise(() => {})); // #2 — stays open

    const { rerender } = renderHook(() => useServerChannelSubscriptions());
    expect(mockApiFetch).toHaveBeenCalledTimes(1);

    // Disconnect clears the latch (by design) and invalidates seed #1.
    act(() => {
      useChatStore.setState({ isConnected: false });
    });
    rerender();
    // Reconnect starts seed #2, which is now the live one and holds the latch.
    act(() => {
      useChatStore.setState({ isConnected: true });
    });
    rerender();
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });
    expect(mockApiFetch).toHaveBeenCalledTimes(2);

    // Seed #1 finally fails. Its 503 says nothing about seed #2.
    await act(async () => {
      failStale({ ok: false, status: 503 });
      await new Promise((r) => setTimeout(r, 0));
    });

    // Any later effect run — here a membership change — would start seed #3 if
    // that stale failure had released the latch, putting three requests in
    // flight and widening the overlap the fence exists to close.
    act(() => {
      useServerStore.setState({ servers: [mockServer, mockServer2] });
    });
    rerender();
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    expect(mockApiFetch).toHaveBeenCalledTimes(2);
  });

  it('refuses the whole seed when any channel row is malformed', async () => {
    const setInitialChannelUnreads = vi.fn();
    useUnreadStore.setState({ setInitialChannelUnreads });
    useChatStore.setState({ isConnected: true });
    useServerStore.setState({ servers: [mockServer], activeServerId: null });

    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        server_ids: ['server-1'],
        channels: [
          { channel_id: 'channel-1', server_id: 'server-1', unread_count: 4 },
          // unread_count is not a number — a contract break, not a transient.
          { channel_id: 'channel-2', server_id: 'server-1', unread_count: null },
        ],
      }),
    });

    renderHook(() => useServerChannelSubscriptions());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    // Not "commits channel-1 and drops channel-2" — setInitialChannelUnreads
    // REPLACES the map, so a partial commit would delete whatever count
    // channel-2 already had. Nothing is committed at all.
    expect(setInitialChannelUnreads).not.toHaveBeenCalled();
  });

  it('drops seed rows for a server the user is no longer in', async () => {
    const setInitialChannelUnreads = vi.fn();
    useUnreadStore.setState({ setInitialChannelUnreads });
    useChatStore.setState({ isConnected: true });
    // Member of server-1 only; the response also names server-2, which the user
    // left while the request was in flight.
    useServerStore.setState({ servers: [mockServer], activeServerId: null });

    mockApiFetch.mockResolvedValueOnce({
      ok: true,
      json: async () => ({
        server_ids: ['server-1', 'server-2'],
        channels: [
          { channel_id: 'channel-1', server_id: 'server-1', unread_count: 4 },
          { channel_id: 'channel-9', server_id: 'server-2', unread_count: 7 },
        ],
      }),
    });

    renderHook(() => useServerChannelSubscriptions());
    await act(async () => {
      await new Promise((r) => setTimeout(r, 0));
    });

    // A departed server is an ordinary race, NOT malformed — so the payload is
    // committed, minus that server's rows. Restoring them would put back counts
    // removeServer already dropped, with nothing able to clear them again.
    expect(setInitialChannelUnreads).toHaveBeenCalledTimes(1);
    const [rows] = setInitialChannelUnreads.mock.calls[0];
    expect(rows).toEqual([{ channelId: 'channel-1', serverId: 'server-1', count: 4 }]);
  });

  describe('Phase-8 review fixes (#3773)', () => {
    const staleSeed = {
      server_ids: ['server-1'],
      channels: [
        { channel_id: 'channel-1', server_id: 'server-1', unread_count: 9 },
        { channel_id: 'channel-ended-only', server_id: 'server-1', unread_count: 7 },
      ],
    };
    const currentSeed = {
      server_ids: ['server-1', 'server-2'],
      channels: [
        { channel_id: 'channel-1', server_id: 'server-1', unread_count: 1 },
        { channel_id: 'channel-2', server_id: 'server-2', unread_count: 2 },
        { channel_id: 'channel-current-control', server_id: 'server-2', unread_count: 4 },
      ],
    };

    function seedLocalUnreads(withSnapshot = true) {
      if (withSnapshot) {
        useUnreadStore.getState().setInitialUnreads(new Map([['channel-1', 1]]), 'server-1');
      }
      useUnreadStore.getState().setInitialChannelUnreads([
        { channelId: 'channel-1', serverId: 'server-1', count: 1 },
        { channelId: 'channel-2', serverId: 'server-2', count: 2 },
      ]);
      useUnreadStore.getState().markServerUnread('server-1', true);
      useUnreadStore.getState().markServerUnread('server-2', true);
    }

    it('clears A’s last eligible unread after a complete bulk seed while B’s snapshot is retained', async () => {
      useChatStore.setState({ isConnected: true });
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-2' });
      useChannelStore.setState({
        activeChannelId: 'channel-viewed-2',
        channels: [
          { ...mockChannel, id: 'channel-2', server_id: 'server-2' },
          { ...mockChannel, id: 'channel-viewed-2', server_id: 'server-2' },
        ],
      });
      const bulk = deferred<unknown>();
      const firstServer = deferred<unknown>();
      const firstServerBody = vi.fn(async () => ({ unreads: [] }));
      mockApiFetch.mockImplementation((path: string) => {
        if (path === '/api/v1/servers/unread-status') return bulk.promise;
        if (path === '/api/v1/servers/server-1/unread') return firstServer.promise;
        if (path === '/api/v1/servers/server-2/unread') {
          return Promise.resolve({
            ok: true,
            json: async () => ({ unreads: [{ channel_id: 'channel-2', unread_count: 2 }] }),
          });
        }
        throw new Error(`Unexpected unread endpoint: ${path}`);
      });
      const { unmount } = renderHook(() => useServerChannelSubscriptions());
      try {
        await vi.waitFor(() => {
          expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
          expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
        });
        await act(async () => {
          bulk.resolve({ ok: true, json: async () => currentSeed });
        });
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-1')).toEqual({
          serverId: 'server-1',
          count: 1,
        });
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-current-control')).toEqual({
          serverId: 'server-2',
          count: 4,
        });
        expect(visibleServerUnread('server-1')).toBe(true);
        expect(visibleServerUnread('server-2')).toBe(true);

        act(() => {
          useUnreadStore.getState().incrementMention('channel-1');
          useUnreadStore.getState().markServerMention('server-1');
          useUnreadStore.getState().incrementMention('channel-2');
          useUnreadStore.getState().markServerMention('server-2');
          useServerStore.setState({ activeServerId: 'server-1' });
          useChannelStore.setState({
            activeChannelId: 'channel-viewed-1',
            channels: [mockChannel, { ...mockChannel, id: 'channel-viewed-1' }],
          });
        });
        expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/server-1/unread');
        expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
        expect(firstServerBody).not.toHaveBeenCalled();
        act(() => {
          useUnreadStore.getState().clearUnread('channel-1');
        });

        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        expect(useUnreadStore.getState().mentionCounts.has('channel-1')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-2')).toEqual({
          serverId: 'server-2',
          count: 2,
        });
        expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
        expect(useUnreadStore.getState().mentionCounts.get('channel-2')).toBe(1);
        expect(visibleServerUnread('server-2')).toBe(true);
        expect(useUnreadStore.getState().serverMentionSet.has('server-2')).toBe(true);
        expect
          .soft(
            visibleServerUnread('server-1'),
            'reading A’s last eligible unread after a complete seed must clear A’s dot while B owns the snapshot'
          )
          .toBe(false);
        expect
          .soft(
            useUnreadStore.getState().serverMentionSet.has('server-1'),
            'reading A’s last eligible unread after a complete seed must clear A’s mention while B owns the snapshot'
          )
          .toBe(false);
      } finally {
        unmount();
        await act(async () => {
          firstServer.resolve({ ok: true, json: firstServerBody });
        });
      }
    });

    it.each(['response', 'JSON body'])(
      'retires a bulk continuation on unmount during its %s without a successor request',
      async (awaitBoundary) => {
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
        seedLocalUnreads();
        const response = deferred<unknown>();
        const body = deferred<unknown>();
        const parseBody = vi.fn(() => body.promise);
        mockApiFetch.mockReturnValue(response.promise);
        const { unmount } = renderHook(() => useServerChannelSubscriptions());
        if (awaitBoundary === 'JSON body') {
          await act(async () => {
            response.resolve({ ok: true, json: parseBody });
          });
          expect(parseBody).toHaveBeenCalledTimes(1);
        }
        unmount();
        expect(mockUnsubscribeServer).not.toHaveBeenCalled();

        await act(async () => {
          response.resolve({ ok: true, json: async () => staleSeed });
          body.resolve(staleSeed);
        });

        expect(mockApiFetch).toHaveBeenCalledTimes(1);
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.has('channel-ended-only'),
            'an unmounted bulk continuation must not write a channel seed without a successor request'
          )
          .toBe(false);
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.get('channel-2'),
            'an unmounted bulk continuation must preserve B’s genuine unread count'
          )
          .toEqual({ serverId: 'server-2', count: 2 });
        expect
          .soft(
            visibleServerUnread('server-2'),
            'an unmounted bulk continuation must not replace the surviving server seed'
          )
          .toBe(true);
      }
    );

    it.each(['ended-first', 'current-first'])(
      'preserves the current bulk request’s local-read fence across remount (%s)',
      async (responseOrder) => {
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
        seedLocalUnreads();
        const ended = deferred<unknown>();
        const current = deferred<unknown>();
        mockApiFetch.mockReturnValueOnce(ended.promise).mockReturnValueOnce(current.promise);
        const oldHook = renderHook(() => useServerChannelSubscriptions());
        oldHook.unmount();
        expect(mockUnsubscribeServer).not.toHaveBeenCalled();
        const currentHook = renderHook(() => useServerChannelSubscriptions());
        expect(mockApiFetch).toHaveBeenCalledTimes(2);
        act(() => {
          useUnreadStore.getState().clearUnread('channel-1');
        });
        expect(visibleServerUnread('server-1')).toBe(false);
        expect(visibleServerUnread('server-2')).toBe(true);
        try {
          for (const request of responseOrder === 'ended-first'
            ? ['ended', 'current']
            : ['current', 'ended']) {
            await act(async () => {
              if (request === 'ended') ended.resolve({ ok: true, json: async () => staleSeed });
              else current.resolve({ ok: true, json: async () => currentSeed });
            });
            if (request === 'current') {
              expect(
                useUnreadStore.getState().allUnreadCounts.get('channel-current-control'),
                'the current bulk response must commit before checking for a stale overwrite'
              ).toEqual({ serverId: 'server-2', count: 4 });
            }
            expect
              .soft(
                useUnreadStore.getState().allUnreadCounts.has('channel-ended-only'),
                'a remounted bulk owner must reject the ended instance’s channel seed in either response order'
              )
              .toBe(false);
            expect
              .soft(
                visibleServerUnread('server-2'),
                'a remounted bulk owner must reject the ended instance’s server seed in either response order'
              )
              .toBe(true);
          }
          expect
            .soft(
              useUnreadStore.getState().allUnreadCounts.has('channel-1'),
              'the ended request must not consume the surviving request’s read fence or resurrect A1'
            )
            .toBe(false);
          expect
            .soft(
              visibleServerUnread('server-1'),
              'the ended request must not consume the surviving request’s read fence or resurrect A’s dot'
            )
            .toBe(false);
          expect
            .soft(
              useUnreadStore.getState().allUnreadCounts.get('channel-current-control'),
              'the current request must commit its positive channel control after remount'
            )
            .toEqual({ serverId: 'server-2', count: 4 });
          expect
            .soft(useUnreadStore.getState().allUnreadCounts.get('channel-2'))
            .toEqual({ serverId: 'server-2', count: 2 });
        } finally {
          currentHook.unmount();
          await act(async () => {
            ended.resolve({ ok: true, json: async () => staleSeed });
            current.resolve({ ok: true, json: async () => currentSeed });
          });
        }
      }
    );

    it.each(['unread reset while mounted', 'account transition after unmount'])(
      'rejects pre-reset unread metadata after %s',
      async (transition) => {
        useAuthStore.getState().setAccessToken('test-account-a-token');
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
        seedLocalUnreads();
        const ended = deferred<unknown>();
        const current = deferred<unknown>();
        mockApiFetch.mockReturnValueOnce(ended.promise).mockReturnValueOnce(current.promise);
        const oldHook = renderHook(() => useServerChannelSubscriptions());
        act(() => {
          useUnreadStore.getState().clearAll();
        });
        if (transition === 'account transition after unmount') {
          oldHook.unmount();
          act(() => {
            useAuthStore.getState().setAccessToken('test-account-b-token');
          });
        }
        // No connection or membership change and no successor request: clearAll
        // itself must retire the old request, including while this hook is mounted.
        expect(mockApiFetch).toHaveBeenCalledTimes(1);
        expect(useUnreadStore.getState().allUnreadCounts.size).toBe(0);
        await act(async () => {
          ended.resolve({ ok: true, json: async () => staleSeed });
        });
        expect
          .soft(
            useUnreadStore.getState().allUnreadCounts.has('channel-ended-only'),
            'a pre-reset bulk request must not restore prior-account channel metadata after clearAll'
          )
          .toBe(false);
        expect
          .soft(
            visibleServerUnread('server-1'),
            'a pre-reset bulk request must not restore prior-account server unread metadata after clearAll'
          )
          .toBe(false);

        oldHook.unmount();
        const currentHook = renderHook(() => useServerChannelSubscriptions());
        try {
          await act(async () => {
            current.resolve({ ok: true, json: async () => currentSeed });
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-current-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
          expect(visibleServerUnread('server-1')).toBe(true);
          expect(visibleServerUnread('server-2')).toBe(true);
          expect(mockUnsubscribeServer).not.toHaveBeenCalled();
        } finally {
          currentHook.unmount();
        }
      }
    );

    it('commits the surviving bulk response under actual StrictMode replay and retains subscriptions', async () => {
      useChatStore.setState({ isConnected: true });
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
      seedLocalUnreads();
      const requests: Array<ReturnType<typeof deferred>> = [];
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/servers/unread-status') {
          throw new Error(`Unexpected unread endpoint: ${path}`);
        }
        const request = deferred<unknown>();
        requests.push(request);
        return request.promise;
      });
      const replay: string[] = [];
      function Probe() {
        useServerChannelSubscriptions();
        useEffect(() => {
          replay.push('setup');
          return () => {
            replay.push('cleanup');
          };
        }, []);
        return null;
      }
      const { unmount } = bareRender(createElement(StrictMode, null, createElement(Probe)));
      try {
        expect(
          replay,
          'the fixture must execute setup, cleanup, setup under top-level StrictMode'
        ).toEqual(['setup', 'cleanup', 'setup']);
        expect(requests.length).toBeGreaterThan(0);
        act(() => {
          useUnreadStore.getState().clearUnread('channel-1');
        });
        await act(async () => {
          for (const request of requests.slice(0, -1)) {
            request.resolve({ ok: true, json: async () => staleSeed });
          }
          requests[requests.length - 1].resolve({ ok: true, json: async () => currentSeed });
        });
        expect(
          useUnreadStore.getState().allUnreadCounts.get('channel-current-control'),
          'StrictMode cleanup must leave a surviving bulk response able to commit'
        ).toEqual({ serverId: 'server-2', count: 4 });
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-ended-only')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        expect(visibleServerUnread('server-1')).toBe(false);
        expect(visibleServerUnread('server-2')).toBe(true);
        expect(mockSubscribeServer).toHaveBeenCalledWith('server-1');
        expect(mockSubscribeServer).toHaveBeenCalledWith('server-2');
        expect(mockUnsubscribeServer).not.toHaveBeenCalled();
      } finally {
        unmount();
      }
      expect(mockUnsubscribeServer).not.toHaveBeenCalled();
    });

    it.each([
      { mute: 'unmuted', expectedVisible: true },
      { mute: 'muted-only', expectedVisible: false },
      { mute: 'explicitly unmuted under server mute', expectedVisible: true },
    ])(
      'shows untouched A2 discovered after reading A1 and switching to B ($mute)',
      async ({ mute, expectedVisible }) => {
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });
        useChannelStore.setState({
          activeChannelId: 'channel-viewed-1',
          channels: [mockChannel, { ...mockChannel, id: 'channel-viewed-1' }],
        });
        seedLocalUnreads(false);
        if (mute === 'muted-only') {
          useNotificationPrefsStore.getState().setMute('channel', 'channel-new-a2', true, null);
        } else if (mute === 'explicitly unmuted under server mute') {
          useNotificationPrefsStore.getState().setMute('server', 'server-1', true, null);
          useNotificationPrefsStore.getState().setMute('channel', 'channel-1', false, null);
          useNotificationPrefsStore.getState().setMute('channel', 'channel-new-a2', false, null);
        }
        const bulk = deferred<unknown>();
        mockApiFetch.mockImplementation((path: string) => {
          if (path === '/api/v1/servers/unread-status') return bulk.promise;
          if (path === '/api/v1/servers/server-1/unread') {
            return Promise.resolve({
              ok: true,
              json: async () => ({ unreads: [{ channel_id: 'channel-1', unread_count: 1 }] }),
            });
          }
          if (path === '/api/v1/servers/server-2/unread') {
            return Promise.resolve({
              ok: true,
              json: async () => ({ unreads: [{ channel_id: 'channel-2', unread_count: 2 }] }),
            });
          }
          throw new Error(`Unexpected unread endpoint: ${path}`);
        });
        const { unmount } = renderHook(() => useServerChannelSubscriptions());
        try {
          await vi.waitFor(() => {
            expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
          });
          expect(visibleServerUnread('server-1')).toBe(true);
          expect(useUnreadStore.getState().allUnreadCounts.has('channel-new-a2')).toBe(false);
          act(() => {
            useUnreadStore.getState().clearUnread('channel-1');
            useServerStore.setState({ activeServerId: 'server-2' });
            useChannelStore.setState({
              activeChannelId: 'channel-viewed-2',
              channels: [
                { ...mockChannel, id: 'channel-2', server_id: 'server-2' },
                { ...mockChannel, id: 'channel-viewed-2', server_id: 'server-2' },
              ],
            });
          });
          expect(visibleServerUnread('server-1')).toBe(false);
          await vi.waitFor(() => {
            expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
            expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
          });
          await act(async () => {
            bulk.resolve({
              ok: true,
              json: async () => ({
                server_ids: ['server-1', 'server-2'],
                channels: [
                  ...currentSeed.channels,
                  { channel_id: 'channel-new-a2', server_id: 'server-1', unread_count: 3 },
                ],
              }),
            });
          });
          expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-new-a2')).toEqual({
            serverId: 'server-1',
            count: 3,
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-current-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-2')).toEqual({
            serverId: 'server-2',
            count: 2,
          });
          expect(visibleServerUnread('server-2')).toBe(true);
          expect(
            visibleServerUnread('server-1'),
            'a local read of A1 must preserve A2’s eligible unread indicator when valid detail first discovers A2'
          ).toBe(expectedVisible);
        } finally {
          unmount();
          await act(async () => {
            bulk.resolve({ ok: true, json: async () => currentSeed });
          });
        }
      }
    );
  });

  describe('late bulk snapshots after a local read (#3773)', () => {
    it.each([
      {
        scenario:
          'keeps the read server clear when its retained map contains a foreign live unread',
        initialUnreads: [{ channel_id: 'channel-1', unread_count: 1 }],
        arrivalChannel: 'channel-2',
        arrivalServer: 'server-2',
        expectedVisible: false,
      },
      {
        scenario: 'keeps a background arrival visible when its retained owned snapshot is empty',
        initialUnreads: [],
        arrivalChannel: 'channel-new-1',
        arrivalServer: 'server-1',
        expectedVisible: true,
      },
    ])(
      '$scenario while the returned-to server fetch is pending',
      async ({ initialUnreads, arrivalChannel, arrivalServer, expectedVisible }) => {
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });
        useChannelStore.setState({
          activeChannelId: 'channel-viewed-1',
          channels: [mockChannel, { ...mockChannel, id: 'channel-viewed-1' }],
        });
        if (initialUnreads.length > 0) {
          useUnreadStore
            .getState()
            .setInitialChannelUnreads([{ channelId: 'channel-1', serverId: 'server-1', count: 1 }]);
        }

        let resolveBulk!: (response: unknown) => void;
        let resolveSecondServer!: (response: unknown) => void;
        let resolveReturnedServer!: (response: unknown) => void;
        const bulk = new Promise((resolve) => {
          resolveBulk = resolve;
        });
        const secondServer = new Promise((resolve) => {
          resolveSecondServer = resolve;
        });
        const returnedServer = new Promise((resolve) => {
          resolveReturnedServer = resolve;
        });
        const returnedBody = vi.fn(async () => ({ unreads: [] }));
        let firstServerFetches = 0;
        mockApiFetch.mockImplementation((path: string) => {
          if (path === '/api/v1/servers/unread-status') return bulk;
          if (path === '/api/v1/servers/server-2/unread') return secondServer;
          if (path === '/api/v1/servers/server-1/unread') {
            firstServerFetches += 1;
            return firstServerFetches === 1
              ? Promise.resolve({ ok: true, json: async () => ({ unreads: initialUnreads }) })
              : returnedServer;
          }
          throw new Error(`Unexpected unread endpoint: ${path}`);
        });

        const { unmount } = renderHook(() => useServerChannelSubscriptions());
        try {
          await vi.waitFor(() => {
            expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
          });
          expect(visibleServerUnread('server-1')).toBe(!expectedVisible);
          if (!expectedVisible) {
            act(() => {
              useUnreadStore.getState().clearUnread('channel-1');
            });
            expect(visibleServerUnread('server-1')).toBe(false);
          }

          act(() => {
            useServerStore.setState({ activeServerId: 'server-2' });
            useChannelStore.setState({
              activeChannelId: 'channel-viewed-2',
              channels: [
                { ...mockChannel, id: 'channel-2', server_id: 'server-2' },
                { ...mockChannel, id: 'channel-viewed-2', server_id: 'server-2' },
              ],
            });
          });
          expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/server-2/unread');
          act(() => {
            // Mirror the real live actions: only an active-server arrival also
            // enters the generic map, which still retains server-1 ownership.
            if (arrivalServer === 'server-2') {
              useUnreadStore.getState().incrementUnread(arrivalChannel);
            }
            useUnreadStore.getState().bumpChannelUnread(arrivalChannel, arrivalServer);
            useUnreadStore.getState().markServerUnread(arrivalServer, true);
            useServerStore.setState({ activeServerId: 'server-1' });
            useChannelStore.setState({
              activeChannelId: 'channel-viewed-1',
              channels: [
                mockChannel,
                { ...mockChannel, id: 'channel-new-1' },
                { ...mockChannel, id: 'channel-viewed-1' },
              ],
            });
          });
          expect(firstServerFetches).toBe(2);
          expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
          expect([...useUnreadStore.getState().unreadCounts]).toEqual(
            expectedVisible ? [] : [['channel-2', 1]]
          );
          expect(visibleServerUnread('server-1')).toBe(expectedVisible);
          expect(visibleServerUnread(arrivalServer)).toBe(true);

          await act(async () => {
            resolveBulk({
              ok: true,
              json: async () => ({
                server_ids: expectedVisible ? ['server-2'] : ['server-1', 'server-2'],
                channels: [
                  ...(!expectedVisible
                    ? [{ channel_id: 'channel-1', server_id: 'server-1', unread_count: 1 }]
                    : []),
                  { channel_id: 'channel-bulk-control', server_id: 'server-2', unread_count: 4 },
                ],
              }),
            });
          });

          // These controls prove the bulk committed while the fresh owning
          // response could not have repaired either stale-snapshot decision.
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-bulk-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
          expect(returnedBody).not.toHaveBeenCalled();
          expect(useUnreadStore.getState().allUnreadCounts.get(arrivalChannel)).toEqual({
            serverId: arrivalServer,
            count: 1,
          });
          expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
          expect(visibleServerUnread('server-2')).toBe(true);
          expect(
            visibleServerUnread('server-1'),
            'the bulk active correction must preserve the fresher owned read or background arrival'
          ).toBe(expectedVisible);
        } finally {
          unmount();
          await act(async () => {
            resolveBulk({ ok: true, json: async () => ({ server_ids: [], channels: [] }) });
            resolveSecondServer({ ok: true, json: async () => ({ unreads: [] }) });
            resolveReturnedServer({ ok: true, json: returnedBody });
          });
        }
      }
    );

    it.each([
      { destination: 'server-2', channelDetail: true },
      { destination: 'server-2-then-server-1', channelDetail: true },
      { destination: 'direct-messages', channelDetail: true },
      { destination: 'server-2', channelDetail: false },
    ])(
      'keeps the previously read server clear after switching to $destination (channels: $channelDetail)',
      async ({ destination, channelDetail }) => {
        useChatStore.setState({ isConnected: true });
        useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: 'server-1' });
        useChannelStore.setState({
          activeChannelId: 'channel-viewed-1',
          channels: [mockChannel, { ...mockChannel, id: 'channel-viewed-1' }],
        });
        useUnreadStore.getState().setInitialChannelUnreads([
          { channelId: 'channel-1', serverId: 'server-1', count: 1 },
          { channelId: 'channel-2', serverId: 'server-2', count: 2 },
        ]);
        useUnreadStore.getState().markServerUnread('server-1', true);
        useUnreadStore.getState().markServerUnread('server-2', true);

        let resolveBulk!: (response: unknown) => void;
        let hasReadFirstChannel = false;
        const bulk = new Promise((resolve) => {
          resolveBulk = resolve;
        });
        mockApiFetch.mockImplementation((path: string) => {
          if (path === '/api/v1/servers/unread-status') return bulk;
          if (path === '/api/v1/servers/server-1/unread') {
            return Promise.resolve({
              ok: true,
              json: async () => ({
                unreads: hasReadFirstChannel ? [] : [{ channel_id: 'channel-1', unread_count: 1 }],
              }),
            });
          }
          if (path === '/api/v1/servers/server-2/unread') {
            return Promise.resolve({
              ok: true,
              json: async () => ({ unreads: [{ channel_id: 'channel-2', unread_count: 2 }] }),
            });
          }
          throw new Error(`Unexpected unread endpoint: ${path}`);
        });

        renderHook(() => useServerChannelSubscriptions());
        await vi.waitFor(() => {
          expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
          expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
        });
        expect(visibleServerUnread('server-1')).toBe(true);
        expect(mockApiFetch).toHaveBeenCalledWith('/api/v1/servers/unread-status');

        act(() => {
          useUnreadStore.getState().clearUnread('channel-1');
          hasReadFirstChannel = true;
          // Existing channel-list read reconciliation clears the dot separately.
          // Use its real action so this case isolates resurrection by the seed.
          useUnreadStore.getState().clearServerUnread('server-1');
          useServerStore.setState({
            activeServerId: destination === 'direct-messages' ? null : 'server-2',
          });
          useChannelStore.setState({
            activeChannelId: destination === 'direct-messages' ? null : 'channel-viewed-2',
            channels:
              destination === 'direct-messages'
                ? []
                : [
                    { ...mockChannel, id: 'channel-2', server_id: 'server-2' },
                    { ...mockChannel, id: 'channel-viewed-2', server_id: 'server-2' },
                  ],
          });
        });
        expect(visibleServerUnread('server-1')).toBe(false);
        if (destination !== 'direct-messages') {
          await vi.waitFor(() => {
            expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-2');
            expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(2);
          });
        }
        if (destination === 'server-2-then-server-1') {
          act(() => {
            useServerStore.setState({ activeServerId: 'server-1' });
            useChannelStore.setState({ activeChannelId: 'channel-1', channels: [mockChannel] });
          });
          // Positive navigation control: the new owning response after the read
          // is empty; it must not clear B while reconciling the returned-to A.
          await vi.waitFor(() => {
            expect(useUnreadStore.getState().unreadCountsServerId).toBe('server-1');
            expect(useUnreadStore.getState().unreadCounts.size).toBe(0);
          });
          expect(visibleServerUnread('server-2')).toBe(true);
        }

        // The extra row is a positive control proving the bulk continuation committed.
        const data = {
          server_ids: ['server-1', 'server-2'],
          ...(channelDetail
            ? {
                channels: [
                  { channel_id: 'channel-1', server_id: 'server-1', unread_count: 1 },
                  { channel_id: 'channel-2', server_id: 'server-2', unread_count: 2 },
                  { channel_id: 'channel-bulk-control', server_id: 'server-2', unread_count: 4 },
                ],
              }
            : {}),
        };
        const readBody = vi.fn(async () => data);
        await act(async () => {
          resolveBulk({ ok: true, json: readBody });
        });

        expect(readBody).toHaveBeenCalledTimes(1);
        expect(useUnreadStore.getState().allUnreadCounts.has('channel-1')).toBe(false);
        expect(useUnreadStore.getState().allUnreadCounts.get('channel-2')).toEqual({
          serverId: 'server-2',
          count: 2,
        });
        if (channelDetail) {
          expect(useUnreadStore.getState().allUnreadCounts.get('channel-bulk-control')).toEqual({
            serverId: 'server-2',
            count: 4,
          });
        }
        expect(visibleServerUnread('server-2')).toBe(true);
        expect(
          visibleServerUnread('server-1'),
          'the previously read server unread indicator must not be resurrected by a late bulk snapshot'
        ).toBe(false);
      }
    );

    it('keeps a new live unread visible when the older snapshot omits its server', async () => {
      useChatStore.setState({ isConnected: true });
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
      let resolveBulk!: (response: unknown) => void;
      mockApiFetch.mockImplementation((path: string) => {
        if (path !== '/api/v1/servers/unread-status') {
          throw new Error(`Unexpected unread endpoint: ${path}`);
        }
        return new Promise((resolve) => {
          resolveBulk = resolve;
        });
      });
      renderHook(() => useServerChannelSubscriptions());

      act(() => {
        useUnreadStore.getState().bumpChannelUnread('channel-2', 'server-2');
        useUnreadStore.getState().markServerUnread('server-2', true);
      });
      expect(visibleServerUnread('server-2')).toBe(true);

      await act(async () => {
        resolveBulk({
          ok: true,
          json: async () => ({
            server_ids: ['server-1'],
            channels: [{ channel_id: 'channel-1', server_id: 'server-1', unread_count: 3 }],
          }),
        });
      });

      expect(useUnreadStore.getState().allUnreadCounts.get('channel-1')).toEqual({
        serverId: 'server-1',
        count: 3,
      });
      expect(useUnreadStore.getState().allUnreadCounts.get('channel-2')).toEqual({
        serverId: 'server-2',
        count: 1,
      });
      expect(visibleServerUnread('server-1')).toBe(true);
      expect(
        visibleServerUnread('server-2'),
        'a live unread arriving during the bulk request must keep its server indicator visible'
      ).toBe(true);
    });

    it('preserves the approximate fallback when an older snapshot omits channel detail', async () => {
      useChatStore.setState({ isConnected: true });
      useServerStore.setState({ servers: [mockServer, mockServer2], activeServerId: null });
      mockApiFetch.mockResolvedValue({
        ok: true,
        json: async () => ({ server_ids: ['server-2'] }),
      });
      renderHook(() => useServerChannelSubscriptions());
      await vi.waitFor(() => {
        expect(visibleServerUnread('server-2')).toBe(true);
      });

      expect(useUnreadStore.getState().serverUnreadPreciseSet.has('server-2')).toBe(false);
      expect(useUnreadStore.getState().allUnreadCounts.size).toBe(0);
    });
  });
});
