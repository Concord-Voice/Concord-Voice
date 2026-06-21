import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';

const mockSubscribeDM = vi.fn();
const mockUnsubscribeDM = vi.fn();
const mockSubscribeChannel = vi.fn();
const mockUnsubscribeChannel = vi.fn();

vi.mock('@/renderer/services/websocketService', () => ({
  getWebSocketService: () => ({
    subscribeDM: mockSubscribeDM,
    unsubscribeDM: mockUnsubscribeDM,
    subscribe: mockSubscribeChannel,
    unsubscribe: mockUnsubscribeChannel,
  }),
}));

import {
  useDMSubscription,
  __resetPendingUnsubscribes,
  UNSUBSCRIBE_DELAY_MS,
} from '@/renderer/hooks/useDMSubscription';
import { useChannelSubscription } from '@/renderer/hooks/useChannelSubscription';

beforeEach(() => {
  vi.clearAllMocks();
  vi.useFakeTimers();
  __resetPendingUnsubscribes();
  useChatStore.setState({ isConnected: true });
});

afterEach(() => {
  vi.useRealTimers();
});

describe('useDMSubscription', () => {
  it('subscribes to DM conversation on mount when connected', () => {
    renderHook(() => useDMSubscription('conv-1'));
    expect(mockSubscribeDM).toHaveBeenCalledWith('conv-1');
  });

  it('does not unsubscribe immediately on unmount (debounced)', () => {
    const { unmount } = renderHook(() => useDMSubscription('conv-1'));
    unmount();
    expect(mockUnsubscribeDM).not.toHaveBeenCalled();
  });

  it('fires real unsubscribe after the debounce window', () => {
    const { unmount } = renderHook(() => useDMSubscription('conv-1'));
    unmount();
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribeDM).toHaveBeenCalledWith('conv-1');
  });

  it('unsubscribe + resubscribe within debounce window is a no-op round-trip', () => {
    const { unmount } = renderHook(() => useDMSubscription('conv-1'));
    expect(mockSubscribeDM).toHaveBeenCalledTimes(1);

    unmount();
    // Still within debounce window
    vi.advanceTimersByTime(500);

    // Re-mount with the same ID
    renderHook(() => useDMSubscription('conv-1'));

    // Let the original timer expire
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);

    // The re-mount should have cancelled the pending unsubscribe,
    // so neither a real unsubscribe nor a duplicate subscribe fires.
    expect(mockUnsubscribeDM).not.toHaveBeenCalled();
    expect(mockSubscribeDM).toHaveBeenCalledTimes(1);
  });

  it('does not subscribe when disconnected', () => {
    useChatStore.setState({ isConnected: false });
    renderHook(() => useDMSubscription('conv-1'));
    expect(mockSubscribeDM).not.toHaveBeenCalled();
  });

  it('does not subscribe when conversationId is null', () => {
    renderHook(() => useDMSubscription(null));
    expect(mockSubscribeDM).not.toHaveBeenCalled();
  });

  it('returns isSubscribed=true when active', () => {
    const { result } = renderHook(() => useDMSubscription('conv-1'));
    expect(result.current.isSubscribed).toBe(true);
  });

  it('returns isSubscribed=false when disconnected', () => {
    useChatStore.setState({ isConnected: false });
    const { result } = renderHook(() => useDMSubscription('conv-1'));
    expect(result.current.isSubscribed).toBe(false);
  });

  it('re-subscribes to a different conversation id after debounce flushes', () => {
    const { rerender } = renderHook(({ convId }) => useDMSubscription(convId), {
      initialProps: { convId: 'conv-1' as string | null },
    });
    expect(mockSubscribeDM).toHaveBeenCalledWith('conv-1');

    rerender({ convId: 'conv-2' });
    // conv-2 subscribes immediately, conv-1 unsubscribe is pending
    expect(mockSubscribeDM).toHaveBeenCalledWith('conv-2');

    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribeDM).toHaveBeenCalledWith('conv-1');
  });
});

describe('useChannelSubscription (debounce integration)', () => {
  it('debounces unsubscribe and cancels on immediate resubscribe', () => {
    const { unmount } = renderHook(() => useChannelSubscription('chan-1'));
    expect(mockSubscribeChannel).toHaveBeenCalledWith('chan-1');
    expect(mockSubscribeChannel).toHaveBeenCalledTimes(1);

    unmount();
    vi.advanceTimersByTime(500);
    renderHook(() => useChannelSubscription('chan-1'));

    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);

    expect(mockUnsubscribeChannel).not.toHaveBeenCalled();
    expect(mockSubscribeChannel).toHaveBeenCalledTimes(1);
  });

  it('fires real unsubscribe after debounce when no resubscribe arrives', () => {
    const { unmount } = renderHook(() => useChannelSubscription('chan-1'));
    unmount();
    expect(mockUnsubscribeChannel).not.toHaveBeenCalled();
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribeChannel).toHaveBeenCalledWith('chan-1');
  });
});

describe('namespace isolation — same UUID used as DM and channel ID', () => {
  it('pending DM unsubscribe does not cancel a channel subscribe for the same UUID', () => {
    const sharedId = 'aaaaaaaa-0000-0000-0000-000000000001';

    // Mount DM, then unmount — puts "dm:<sharedId>" timer in the map
    const { unmount: unmountDM } = renderHook(() => useDMSubscription(sharedId));
    expect(mockSubscribeDM).toHaveBeenCalledWith(sharedId);
    unmountDM();

    // Still within debounce window — mount as channel
    vi.advanceTimersByTime(500);
    renderHook(() => useChannelSubscription(sharedId));

    // Channel subscribe should fire (different namespace key)
    expect(mockSubscribeChannel).toHaveBeenCalledWith(sharedId);

    // After debounce, DM unsubscribe fires; channel is unaffected
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribeDM).toHaveBeenCalledWith(sharedId);
    expect(mockUnsubscribeChannel).not.toHaveBeenCalled();
  });

  it('pending channel unsubscribe does not cancel a DM subscribe for the same UUID', () => {
    const sharedId = 'aaaaaaaa-0000-0000-0000-000000000002';

    // Mount channel, then unmount
    const { unmount: unmountCh } = renderHook(() => useChannelSubscription(sharedId));
    expect(mockSubscribeChannel).toHaveBeenCalledWith(sharedId);
    unmountCh();

    // Still within debounce window — mount as DM
    vi.advanceTimersByTime(500);
    renderHook(() => useDMSubscription(sharedId));

    // DM subscribe should fire (different namespace key)
    expect(mockSubscribeDM).toHaveBeenCalledWith(sharedId);

    // After debounce, channel unsubscribe fires; DM is unaffected
    vi.advanceTimersByTime(UNSUBSCRIBE_DELAY_MS + 10);
    expect(mockUnsubscribeChannel).toHaveBeenCalledWith(sharedId);
    expect(mockUnsubscribeDM).not.toHaveBeenCalled();
  });
});
