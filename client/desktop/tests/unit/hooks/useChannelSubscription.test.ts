import { renderHook } from '@testing-library/react';
import { useChannelSubscription } from '@/renderer/hooks/useChannelSubscription';
import { useChatStore } from '@/renderer/stores/chatStore';
import {
  __resetPendingUnsubscribes,
  UNSUBSCRIBE_DELAY_MS,
} from '@/renderer/hooks/useDMSubscription';

// Mock websocketService
const mockSubscribe = vi.fn();
const mockUnsubscribe = vi.fn();

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    subscribe: mockSubscribe,
    unsubscribe: mockUnsubscribe,
  }),
}));

describe('useChannelSubscription', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    __resetPendingUnsubscribes();
    useChatStore.setState({ isConnected: true });
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it('subscribes to channel when connected and channelId provided', () => {
    renderHook(() => useChannelSubscription('channel-1'));
    expect(mockSubscribe).toHaveBeenCalledWith('channel-1');
  });

  it('returns isSubscribed: true when connected and channelId provided', () => {
    const { result } = renderHook(() => useChannelSubscription('channel-1'));
    expect(result.current.isSubscribed).toBe(true);
  });

  it('does not subscribe when channelId is null', () => {
    renderHook(() => useChannelSubscription(null));
    expect(mockSubscribe).not.toHaveBeenCalled();
  });

  it('returns isSubscribed: false when channelId is null', () => {
    const { result } = renderHook(() => useChannelSubscription(null));
    expect(result.current.isSubscribed).toBe(false);
  });

  it('does not subscribe when disconnected', () => {
    useChatStore.setState({ isConnected: false });
    renderHook(() => useChannelSubscription('channel-1'));
    expect(mockSubscribe).not.toHaveBeenCalled();
  });

  it('returns isSubscribed: false when disconnected', () => {
    useChatStore.setState({ isConnected: false });
    const { result } = renderHook(() => useChannelSubscription('channel-1'));
    expect(result.current.isSubscribed).toBe(false);
  });

  it('unsubscribes on unmount after debounce window', () => {
    const { unmount } = renderHook(() => useChannelSubscription('channel-1'));
    unmount();
    // Unsubscribe is debounced — not called immediately
    expect(mockUnsubscribe).not.toHaveBeenCalled();
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribe).toHaveBeenCalledWith('channel-1');
  });

  it('resubscribes when channelId changes', () => {
    const { rerender } = renderHook(({ channelId }) => useChannelSubscription(channelId), {
      initialProps: { channelId: 'channel-1' as string | null },
    });

    expect(mockSubscribe).toHaveBeenCalledWith('channel-1');

    rerender({ channelId: 'channel-2' });
    // New id subscribes immediately; old id unsubscribe is pending
    expect(mockSubscribe).toHaveBeenCalledWith('channel-2');

    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribe).toHaveBeenCalledWith('channel-1');
  });

  it('uses ch: namespace so resubscribing the same channel cancels its own pending unsubscribe', () => {
    // Verify that two consecutive mount/unmount cycles of the same channelId
    // correctly share the "ch:<id>" key — not a raw UUID that could collide with DMs.
    const { unmount } = renderHook(() => useChannelSubscription('channel-1'));
    unmount();

    // Second mount within the debounce window cancels the pending unsubscribe
    vi.advanceTimersByTime(100);
    renderHook(() => useChannelSubscription('channel-1'));

    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);

    // subscribe fired exactly once (second mount was a no-op cancel)
    expect(mockSubscribe).toHaveBeenCalledTimes(1);
    expect(mockUnsubscribe).not.toHaveBeenCalled();
  });
});
