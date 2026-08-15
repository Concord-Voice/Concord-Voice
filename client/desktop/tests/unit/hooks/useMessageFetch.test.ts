import { renderHook, act, waitFor } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { mockMessage, mockMessage2, mockPendingMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
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
const mockOperationGuard = { assertCurrent: vi.fn() };
const mockCreateChannelOperationGuard = vi.fn(() => mockOperationGuard);

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    createChannelOperationGuard: (...args: unknown[]) => mockCreateChannelOperationGuard(...args),
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
    invalidateChannelKey: (...args: unknown[]) => mockInvalidateChannelKey(...args),
  },
}));

import { reconcileFetchedMessages, useMessageFetch } from '@/renderer/hooks/useMessageFetch';

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

/** A promise whose resolution the test controls, for in-flight-window cases. */
function deferred<T>() {
  let resolvePromise: ((value: T) => void) | undefined;
  const promise = new Promise<T>((resolve) => {
    resolvePromise = resolve;
  });
  return {
    promise,
    resolve(value: T) {
      if (!resolvePromise) throw new Error('deferred promise was not initialized');
      resolvePromise(value);
    },
  };
}

describe('useMessageFetch', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockOperationGuard.assertCurrent.mockImplementation(() => undefined);
    useChatStore.setState({
      messagesByChannel: new Map(),
    });
  });

  // --- Basic fetch ---

  it('keeps a newer live edit using the latest of edited_at and updated_at', () => {
    const fetched = {
      ...mockMessage,
      id: 'edited-message',
      content: 'stale plaintext',
      updated_at: '2025-01-01T12:00:00Z',
      edited_at: '2025-01-01T12:01:00Z',
    };
    const live = {
      ...fetched,
      content: 'newer plaintext',
      // DM edit responses may leave updated_at unchanged.
      edited_at: '2025-01-01T12:02:00Z',
    };

    const reconciled = reconcileFetchedMessages([fetched], [live], new Set(['edited-message']));

    expect(reconciled.fetched).toEqual([live]);
  });

  it('accepts a later REST edit despite a skewed synthetic live updated_at', () => {
    const fetched = {
      ...mockMessage,
      id: 'clock-skewed-edit',
      content: 'authoritative later edit',
      edited_at: '2025-01-01T12:03:00Z',
      updated_at: '2025-01-01T12:03:00Z',
    };
    const live = {
      ...fetched,
      content: 'stale earlier edit',
      edited_at: '2025-01-01T12:02:00Z',
      updated_at: '2099-01-01T00:00:00Z',
    };

    const reconciled = reconcileFetchedMessages([fetched], [live], new Set(['clock-skewed-edit']));

    expect(reconciled.fetched).toEqual([fetched]);
  });

  it('keeps a loaded row invalidated by an equal-timestamp live edit', () => {
    const fetched = {
      ...mockMessage,
      id: 'equal-timestamp-edit',
      content: 'authoritative REST plaintext',
      edited_at: '2025-01-01T12:04:00Z',
    };
    const live = {
      ...fetched,
      content: 'live decrypted plaintext',
    };

    const reconciled = reconcileFetchedMessages(
      [fetched],
      [live],
      new Set(['equal-timestamp-edit']),
      new Set(['equal-timestamp-edit'])
    );

    expect(reconciled.fetched).toEqual([fetched]);
    expect(reconciled.preserved).toEqual([]);
  });

  it('drops an unloaded REST row invalidated by an edit or delete during decryption', () => {
    const fetched = {
      ...mockMessage,
      id: 'unloaded-message',
      content: 'stale plaintext',
    };

    const reconciled = reconcileFetchedMessages(
      [fetched],
      [],
      new Set(),
      new Set(['unloaded-message'])
    );

    expect(reconciled.fetched).toEqual([]);
  });

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

  it('uses the conversation id as channel_id for DM history rows without one', async () => {
    mockFetchResponse([
      {
        ...mockMessage,
        id: 'dm-msg-1',
        channel_id: undefined as unknown as string,
      },
    ]);

    renderHook(() => useMessageFetch('conv-1', { type: 'dm' }));

    await waitFor(() => {
      const stored = useChatStore.getState().messagesByChannel.get('conv-1');
      expect(stored?.[0]?.channel_id).toBe('conv-1');
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

  // --- Purge refetch seam (#1354) ---

  // A purge clears the scope wholesale, so the server is the only source of
  // truth for what survived. One seam covers channels and DMs alike.
  describe('messages-purged', () => {
    beforeEach(() => {
      resetAllStores();
    });

    it('refetches the mounted scope on messages-purged', async () => {
      mockFetchResponse([mockMessage]);
      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

      mockFetchResponse([mockMessage2]);
      act(() => {
        globalThis.dispatchEvent(
          new CustomEvent('messages-purged', { detail: { scopeId: 'channel-1' } })
        );
      });

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
    });

    it('ignores messages-purged for a scope that is not mounted', async () => {
      mockFetchResponse([mockMessage]);
      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

      await act(async () => {
        globalThis.dispatchEvent(
          new CustomEvent('messages-purged', { detail: { scopeId: 'some-other-scope' } })
        );
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    // A server-wide purge names a server id, which can never equal a channel
    // id, so the modal dispatches a null scope meaning "refetch what is
    // mounted". Before this, server purges left every mounted view stale.
    it('treats a null scopeId as a match for the mounted scope', async () => {
      mockFetchResponse([mockMessage]);
      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(1));

      mockFetchResponse([mockMessage2]);
      act(() => {
        globalThis.dispatchEvent(new CustomEvent('messages-purged', { detail: { scopeId: null } }));
      });

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
    });

    // #1741 content invariant: `aborted` alone cannot close this window. It
    // flips in the effect cleanup, which runs only after React commits the
    // setFetchTrigger bump the purge queues — a response resolving in between
    // reaches indexDecryptedMessages + setMessages with purged plaintext.
    it('does not publish a response that resolves inside the purge window', async () => {
      // vi.clearAllMocks() keeps queued once-implementations, so drain them:
      // this case depends on THIS test's request being the deferred one.
      mockApiFetch.mockReset();
      mockSafeJson.mockReset();

      const response = { ok: true, status: 200 };
      const purgedPage = deferred<{ messages: MessageWithStatus[] }>();
      mockApiFetch.mockResolvedValueOnce(response);
      mockSafeJson.mockReturnValueOnce(purgedPage.promise);

      renderHook(() => useMessageFetch('channel-purge-window', { type: 'channel' }));
      await waitFor(() => expect(mockSafeJson).toHaveBeenCalledTimes(1));

      // The refetch the purge queues never resolves, so the store's final
      // state reflects only whether the in-flight request published.
      mockApiFetch.mockResolvedValueOnce(response);
      mockSafeJson.mockReturnValueOnce(new Promise<never>(() => {}));

      await act(async () => {
        globalThis.dispatchEvent(
          new CustomEvent('messages-purged', { detail: { scopeId: 'channel-purge-window' } })
        );
        // Still inside act(): the listener has run synchronously (bumping the
        // purge generation) but React has not committed, so `aborted` is false
        // — exactly the window this fence exists for.
        purgedPage.resolve({
          messages: [
            { ...mockMessage, channel_id: 'channel-purge-window', content: 'purged plaintext' },
          ],
        });
        // Drain the request's remaining microtasks WITHOUT returning from
        // act(), which is what defers React's commit. The fetch therefore
        // reaches its publication point with `aborted` still false: only the
        // purge generation can stop it here.
        for (let tick = 0; tick < 50; tick += 1) {
          // eslint-disable-next-line no-await-in-loop -- sequential microtask drain is the point
          await Promise.resolve();
        }
      });

      // Zero rows, not an empty string in a row: the fence stops publication
      // outright. Contrast with the positive control below, which publishes one
      // row from the same mechanics minus the purge.
      const stored = useChatStore.getState().messagesByChannel.get('channel-purge-window') ?? [];
      expect(stored).toEqual([]);
    });

    // Positive control for the fence above. An empty store is also what you get
    // when the response never reached publication for some unrelated reason —
    // a mock that resolved wrongly, a hook that bailed early — so the empty
    // assertion alone cannot distinguish "the fence stopped it" from "nothing
    // ever arrived". This runs the SAME mechanics with the purge dispatch
    // removed and asserts the content DOES land, which is what makes the
    // difference attributable to the fence.
    it('publishes that same response when no purge intervenes', async () => {
      mockApiFetch.mockReset();
      mockSafeJson.mockReset();

      const response = { ok: true, status: 200 };
      const page = deferred<{ messages: MessageWithStatus[] }>();
      mockApiFetch.mockResolvedValueOnce(response);
      mockSafeJson.mockReturnValueOnce(page.promise);

      renderHook(() => useMessageFetch('channel-purge-control', { type: 'channel' }));
      await waitFor(() => expect(mockSafeJson).toHaveBeenCalledTimes(1));

      await act(async () => {
        page.resolve({
          messages: [
            { ...mockMessage, channel_id: 'channel-purge-control', content: 'published plaintext' },
          ],
        });
        for (let tick = 0; tick < 50; tick += 1) {
          // eslint-disable-next-line no-await-in-loop -- sequential microtask drain is the point
          await Promise.resolve();
        }
      });

      // The ROW is the signal, not its text: this suite stubs the decrypt seam,
      // so the published row carries the fail-closed empty placeholder rather
      // than the plaintext. That is precisely the contrast the control needs —
      // one row published here, zero rows above, from identical mechanics.
      const stored = useChatStore.getState().messagesByChannel.get('channel-purge-control') ?? [];
      expect(stored).toHaveLength(1);
      expect(stored[0].id).toBe(mockMessage.id);
    });
  });
});
