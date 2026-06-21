import { renderHook } from '@testing-library/react';
import { useServerStore } from '@/renderer/stores/serverStore';
import { useChatStore } from '@/renderer/stores/chatStore';
import { useChannelStore } from '@/renderer/stores/channelStore';
import { useUnreadStore } from '@/renderer/stores/unreadStore';
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

import { useServerChannelSubscriptions } from '@/renderer/hooks/useServerChannelSubscriptions';

describe('useServerChannelSubscriptions', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useServerStore.setState({ servers: [], activeServerId: null });
    useChatStore.setState({ isConnected: false });
    useChannelStore.setState({ activeChannelId: null });
    useUnreadStore.setState({
      unreadCounts: new Map(),
      serverUnreadSet: new Set(),
    });
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
});
