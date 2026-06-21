import { renderHook, act, waitFor } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { mockMessage, mockMessage2, mockPendingMessage } from '../../mocks/fixtures';
import type { MessageWithStatus } from '@/renderer/types/chat';

// Mock apiFetch and safeJson
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

// Mock e2eeService
const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();
const mockInvalidateChannelKey = vi.fn();

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
  },
}));

import { useMessageFetch } from '@/renderer/hooks/useMessageFetch';

// Helper: build a mock API response
function mockFetchResponse(messages: MessageWithStatus[], ok = true) {
  const response = { ok, status: ok ? 200 : 500 };
  mockApiFetch.mockResolvedValueOnce(response);
  if (ok) {
    mockSafeJson.mockResolvedValueOnce({ messages });
  } else {
    mockSafeJson.mockResolvedValueOnce({ error: 'Server error' });
  }
}

describe('useMessageFetch', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    useChatStore.setState({
      messagesByChannel: new Map(),
    });
  });

  // --- Basic fetch ---

  it('fetches messages on mount and stores them in chatStore', async () => {
    const msgs = [
      { ...mockMessage2, id: 'msg-2', created_at: '2025-01-01T12:01:00Z' },
      { ...mockMessage, id: 'msg-1', created_at: '2025-01-01T12:00:00Z' },
    ];
    mockFetchResponse(msgs);

    const { result } = renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    expect(result.current.isLoading).toBe(true);

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
    });

    // Messages should be stored (server returns DESC, hook reverses to ASC)
    const stored = useChatStore.getState().messagesByChannel.get('channel-1');
    expect(stored).toBeDefined();
    expect(stored![0].id).toBe('msg-1');
    expect(stored![1].id).toBe('msg-2');
  });

  it('does not route call_event rows through e2ee decryption', async () => {
    // call_event rows carry plaintext server metadata in call_event_payload
    // and empty content; they must bypass the E2EE decrypt pass (#1219) so
    // decryptContent on '' doesn't set decryptFailed. getChannelKey is left
    // unconfigured (resolves undefined) so the default path WOULD route to
    // decryptForChannel if the guard were absent.
    const callEventRow = {
      ...mockMessage,
      id: 'ce1',
      content: '',
      type: 'call_event',
      call_event_payload: {
        status: 'completed' as const,
        started_at: '2026-06-15T00:00:00Z',
        duration_seconds: 42,
      },
    } as unknown as MessageWithStatus;
    mockFetchResponse([callEventRow]);

    renderHook(() => useMessageFetch('conv-1', { type: 'dm' }));

    await waitFor(() => {
      const stored = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(stored).toBeDefined();
      expect(stored![0].id).toBe('ce1');
    });

    expect(mockDecryptForChannel).not.toHaveBeenCalled();
    expect(mockDecryptWithKey).not.toHaveBeenCalled();
  });

  it('uses the correct endpoint for DM type', async () => {
    mockFetchResponse([]);

    renderHook(() => useMessageFetch('conv-1', { type: 'dm' }));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/dm/conversations/conv-1/messages')
      );
    });
  });

  it('uses the correct endpoint for channel type', async () => {
    mockFetchResponse([]);

    renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('/api/v1/channels/channel-1/messages')
      );
    });
  });

  // --- DESC → ASC ordering ---

  it('reverses server DESC order to ASC for chronological display', async () => {
    // Server returns newest first (DESC)
    const serverOrder = [
      { ...mockMessage, id: 'msg-3', created_at: '2025-01-01T12:02:00Z' },
      { ...mockMessage, id: 'msg-2', created_at: '2025-01-01T12:01:00Z' },
      { ...mockMessage, id: 'msg-1', created_at: '2025-01-01T12:00:00Z' },
    ];
    mockFetchResponse(serverOrder);

    renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    await waitFor(() => {
      const stored = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(stored).toHaveLength(3);
      // Reversed to ASC (oldest first)
      expect(stored![0].id).toBe('msg-1');
      expect(stored![1].id).toBe('msg-2');
      expect(stored![2].id).toBe('msg-3');
    });
  });

  // --- Optimistic merge ---

  it('preserves optimistic messages not yet confirmed by server', async () => {
    // Pre-populate store with an optimistic message
    const optimistic: MessageWithStatus = {
      ...mockPendingMessage,
      id: 'client-msg-1',
      clientMessageId: 'client-msg-1',
      status: 'pending',
      channel_id: 'channel-1',
    };
    useChatStore.getState().setMessages('channel-1', [optimistic]);

    // Server returns one message that does NOT include the optimistic one
    mockFetchResponse([{ ...mockMessage, id: 'msg-1' }]);

    renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    await waitFor(() => {
      const stored = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(stored).toBeDefined();
      // Should have both: the server message and the optimistic one
      expect(stored!.length).toBe(2);
      expect(stored!.some((m) => m.clientMessageId === 'client-msg-1')).toBe(true);
      expect(stored!.some((m) => m.id === 'msg-1')).toBe(true);
    });
  });

  it('deduplicates optimistic messages that appear in server response', async () => {
    const optimistic: MessageWithStatus = {
      ...mockPendingMessage,
      id: 'client-msg-1',
      clientMessageId: 'client-msg-1',
      status: 'sent',
      channel_id: 'channel-1',
    };
    useChatStore.getState().setMessages('channel-1', [optimistic]);

    // Server returns the same message with the server-assigned ID
    // AND the clientMessageId matches
    mockFetchResponse([{ ...mockMessage, id: 'server-msg-1', clientMessageId: 'client-msg-1' }]);

    renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    await waitFor(() => {
      const stored = useChatStore.getState().messagesByChannel.get('channel-1');
      // Optimistic should be deduped — only the server message remains
      expect(stored!.length).toBe(1);
    });
  });

  // --- Error handling ---

  it('sets error state on fetch failure', async () => {
    mockFetchResponse([], false);

    const { result } = renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

    await waitFor(() => {
      expect(result.current.isLoading).toBe(false);
      expect(result.current.error).toBe('Server error');
    });
  });

  // --- hasMore / pagination ---

  it('sets hasMore=true when response contains exactly limit messages', async () => {
    const msgs = Array.from({ length: 50 }, (_, i) => ({
      ...mockMessage,
      id: `msg-${i}`,
      created_at: `2025-01-01T${String(i).padStart(2, '0')}:00:00Z`,
    }));
    mockFetchResponse(msgs);

    const { result } = renderHook(() =>
      useMessageFetch('channel-1', { type: 'channel', limit: 50 })
    );

    await waitFor(() => {
      expect(result.current.hasMore).toBe(true);
    });
  });

  it('sets hasMore=false when response contains fewer than limit messages', async () => {
    mockFetchResponse([mockMessage]);

    const { result } = renderHook(() =>
      useMessageFetch('channel-1', { type: 'channel', limit: 50 })
    );

    await waitFor(() => {
      expect(result.current.hasMore).toBe(false);
    });
  });

  // --- Pagination (loadMore) ---

  it('handleLoadMore fetches older messages with before cursor', async () => {
    // Initial fetch
    const initialMsgs = Array.from({ length: 50 }, (_, i) => ({
      ...mockMessage,
      id: `msg-${i}`,
      created_at: `2025-01-01T${String(i).padStart(2, '0')}:00:00Z`,
    }));
    mockFetchResponse(initialMsgs);

    const { result } = renderHook(() =>
      useMessageFetch('channel-1', { type: 'channel', limit: 50 })
    );

    await waitFor(() => {
      expect(result.current.hasMore).toBe(true);
    });

    // Load more
    const olderMsgs = [{ ...mockMessage, id: 'old-msg-1', created_at: '2024-12-31T23:00:00Z' }];
    mockFetchResponse(olderMsgs);

    await act(async () => {
      await result.current.handleLoadMore();
    });

    // Should have called with before= parameter
    expect(mockApiFetch).toHaveBeenLastCalledWith(expect.stringContaining('before='));
  });

  // --- onFetchComplete callback ---

  it('calls onFetchComplete after successful initial fetch', async () => {
    mockFetchResponse([mockMessage]);
    const onFetchComplete = vi.fn();

    renderHook(() => useMessageFetch('channel-1', { type: 'channel', onFetchComplete }));

    await waitFor(() => {
      expect(onFetchComplete).toHaveBeenCalledTimes(1);
    });
  });

  // --- Null channelId ---

  it('does not fetch when channelId is null', async () => {
    const { result } = renderHook(() => useMessageFetch(null, { type: 'channel' }));

    expect(result.current.messages).toEqual([]);
    expect(result.current.isLoading).toBe(false);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  // --- Abort on channel change ---

  it('aborts in-flight fetch when channelId changes', async () => {
    // First channel: slow response
    let resolveFirst: (value: unknown) => void;
    mockApiFetch.mockReturnValueOnce(
      new Promise((resolve) => {
        resolveFirst = resolve;
      })
    );

    const { rerender } = renderHook(
      ({ channelId }) => useMessageFetch(channelId, { type: 'channel' }),
      { initialProps: { channelId: 'channel-1' as string | null } }
    );

    // Switch to channel-2 before first resolves
    mockFetchResponse([mockMessage2]);
    rerender({ channelId: 'channel-2' });

    // Resolve the first (stale) request
    resolveFirst!({ ok: true });
    mockSafeJson.mockResolvedValueOnce({ messages: [mockMessage] });

    await waitFor(() => {
      // Only channel-2 messages should be stored
      const ch2 = useChatStore.getState().messagesByChannel.get('channel-2');
      expect(ch2).toBeDefined();
    });

    // channel-1 should NOT have messages from the aborted fetch
    const ch1 = useChatStore.getState().messagesByChannel.get('channel-1');
    expect(ch1).toBeUndefined();
  });
});
