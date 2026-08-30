import { renderHook, act } from '@testing-library/react';
import { useServerStore } from '@/renderer/stores/chat/serverStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelStore } from '@/renderer/stores/chat/channelStore';
import { useUnreadStore } from '@/renderer/stores/chat/unreadStore';
import { useNotificationPrefsStore } from '@/renderer/stores/ui/notificationPrefsStore';
import { mockServer, mockServer2 } from '../../mocks/fixtures';

// Mock websocketService
const mockSubscribeServer = vi.fn();
const mockUnsubscribeServer = vi.fn();
vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    subscribeServer: mockSubscribeServer,
    unsubscribeServer: mockUnsubscribeServer,
  }),
}));

// Mock apiFetch
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
}));

import { useServerChannelSubscriptions } from '@/renderer/hooks/messaging/useServerChannelSubscriptions';

describe('useServerChannelSubscriptions', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useServerStore.setState({ servers: [], activeServerId: null });
    useChatStore.setState({ isConnected: false });
    useChannelStore.setState({ activeChannelId: null });
    useUnreadStore.setState({
      unreadCounts: new Map(),
      unreadCountsServerId: null,
      serverUnreadSet: new Set(),
      serverUnreadPreciseSet: new Set(),
      serverUnreadChannelWinsSet: new Set(),
    });
    useNotificationPrefsStore.getState().clearAll();
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
});
