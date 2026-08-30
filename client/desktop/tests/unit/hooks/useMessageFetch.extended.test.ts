import { renderHook, waitFor, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { MessageWithStatus } from '@/renderer/types/chat';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';
import { removeMessage } from '@/renderer/services/messaging/searchService';

// Mock apiFetch and safeJson
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/system/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

// Mock e2eeService — NOT initialized by default for some tests
let mockIsInitialized = true;
const mockGetChannelKey = vi.fn();
const mockGetChannelKeyByVersion = vi.fn();
const mockDecryptWithKey = vi.fn();
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();
const mockOperationGuard = { assertCurrent: vi.fn() };
const mockCreateChannelOperationGuard = vi.fn(() => mockOperationGuard);

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return mockIsInitialized;
    },
    createChannelOperationGuard: (...args: unknown[]) => mockCreateChannelOperationGuard(...args),
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

import { useMessageFetch } from '@/renderer/hooks/messaging/useMessageFetch';

function mockFetchResponse(messages: MessageWithStatus[], ok = true) {
  const response = { ok, status: ok ? 200 : 500 };
  mockApiFetch.mockResolvedValueOnce(response);
  if (ok) {
    mockSafeJson.mockResolvedValueOnce({ messages });
  } else {
    mockSafeJson.mockResolvedValueOnce({ error: 'Server error' });
  }
}

function guardExpiringAfterInternalBatch(
  expirationError: Error = new Error('channel access revoked before publication')
) {
  let assertions = 0;
  let expired = false;
  return {
    assertCurrent: vi.fn(() => {
      if (expired) throw expirationError;
      assertions += 1;
      // One encrypted row reaches the batch's final assertion fifth. Queue
      // revocation ahead of the awaiting caller's continuation to exercise
      // the outer publication fence.
      if (assertions === 5) {
        queueMicrotask(() => {
          expired = true;
        });
      }
    }),
  };
}

describe('useMessageFetch — extended coverage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockOperationGuard.assertCurrent.mockImplementation(() => undefined);
    resetAllStores();
    mockIsInitialized = true;
  });

  // --- E2EE decryption ---

  describe('E2EE decryption', () => {
    it('decrypts encrypted messages using channel key', async () => {
      const encryptedMsg: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-1',
        content: 'encrypted-content',
      };

      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      mockDecryptWithKey.mockResolvedValue('decrypted text');
      mockFetchResponse([encryptedMsg]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('decrypted text');
      });
    });

    it('handles messages with versioned keys', async () => {
      const versionedMsg: MessageWithStatus = {
        ...mockMessage,
        id: 'ver-1',
        content: 'versioned-encrypted',
        key_version: 2,
      };

      const mockVersionKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(null);
      mockGetChannelKeyByVersion.mockResolvedValue(mockVersionKey);
      mockDecryptWithKey.mockResolvedValue('versioned decrypted');
      mockFetchResponse([versionedMsg]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('versioned decrypted');
      });
    });

    it('marks messages as pendingKeys when E2EE is not initialized', async () => {
      mockIsInitialized = false;

      const encryptedMsg: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-1',
        content: 'encrypted',
      };
      mockFetchResponse([encryptedMsg]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('');
        expect(stored![0].pendingKeys).toBe(true);
      });
    });

    it('marks messages as decryptFailed on decryption error', async () => {
      const encryptedMsg: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-1',
        content: 'encrypted',
      };

      mockGetChannelKey.mockRejectedValue(new Error('key not found'));
      mockDecryptForChannel.mockRejectedValue(new Error('decryption failed'));
      mockFetchResponse([encryptedMsg]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('');
        expect(stored![0].decryptFailed).toBe(true);
      });
    });

    it('decrypts replied_to content alongside the message', async () => {
      const msgWithReply: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-reply-1',
        content: 'encrypted-parent',
        replied_to: {
          id: 'original-1',
          user_id: 'user-1',
          username: 'testuser',
          content: 'encrypted-reply-content',
          key_version: 1,
        },
      };

      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      // replied_to is decrypted first, then the parent message
      mockDecryptWithKey
        .mockResolvedValueOnce('decrypted reply')
        .mockResolvedValueOnce('decrypted parent');
      mockFetchResponse([msgWithReply]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('decrypted parent');
        expect(stored![0].replied_to?.content).toBe('decrypted reply');
      });
    });

    it('decrypts replied_to with a different key version than the parent', async () => {
      const msgWithVersionedReply: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-vreply-1',
        content: 'encrypted-parent-v1',
        key_version: 1,
        replied_to: {
          id: 'original-2',
          user_id: 'user-1',
          username: 'testuser',
          content: 'encrypted-reply-v3',
          key_version: 3,
        },
      };

      const mockCurrentKey = {} as CryptoKey;
      const mockV3Key = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockCurrentKey);
      mockGetChannelKeyByVersion.mockResolvedValue(mockV3Key);
      // Parent uses current key (version 1), replied_to uses version 3
      mockDecryptWithKey.mockImplementation(async (content: string, key: CryptoKey) => {
        if (key === mockCurrentKey) return 'decrypted parent v1';
        if (key === mockV3Key) return 'decrypted reply v3';
        throw new Error('unexpected key');
      });
      mockFetchResponse([msgWithVersionedReply]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('decrypted parent v1');
        expect(stored![0].replied_to?.content).toBe('decrypted reply v3');
      });

      // Should have pre-fetched version 3 key
      expect(mockGetChannelKeyByVersion).toHaveBeenCalledWith('channel-1', 3);
    });

    it('decrypts replied_to content alongside parent message', async () => {
      const msgWithPlainReply: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-plainreply-1',
        content: 'encrypted-parent',
        replied_to: {
          id: 'original-3',
          user_id: 'user-1',
          username: 'testuser',
          content: 'encrypted-reply',
        },
      };

      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      // replied_to decrypted first, then parent
      mockDecryptWithKey
        .mockResolvedValueOnce('plaintext reply')
        .mockResolvedValueOnce('decrypted parent');
      mockFetchResponse([msgWithPlainReply]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('decrypted parent');
        expect(stored![0].replied_to?.content).toBe('plaintext reply');
      });

      // decryptWithKey should be called for both replied_to and parent
      expect(mockDecryptWithKey).toHaveBeenCalledTimes(2);
    });

    it('blanks replied_to ciphertext on decryption failure', async () => {
      const msgWithBadReply: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-badreply-1',
        content: 'encrypted-parent',
        replied_to: {
          id: 'original-4',
          user_id: 'user-1',
          username: 'testuser',
          content: 'undecryptable-ciphertext',
          key_version: 1,
        },
      };

      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      // replied_to decryption fails first, then parent succeeds
      mockDecryptWithKey
        .mockRejectedValueOnce(new Error('decryption failed'))
        .mockResolvedValueOnce('decrypted parent');
      mockFetchResponse([msgWithBadReply]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        expect(stored![0].content).toBe('decrypted parent');
        // Reply previews stay fail-closed and never render ciphertext.
        expect(stored![0].replied_to?.content).toBe('');
      });
    });

    it('blanks both message and replied_to content when E2EE is not initialized', async () => {
      mockIsInitialized = false;

      const msgWithReply: MessageWithStatus = {
        ...mockMessage,
        id: 'enc-noinit-1',
        content: 'encrypted',
        replied_to: {
          id: 'original-5',
          user_id: 'user-1',
          username: 'testuser',
          content: 'encrypted-reply',
        },
      };
      mockFetchResponse([msgWithReply]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toBeDefined();
        // Fail-closed: message content blanked, pendingKeys set
        expect(stored![0].content).toBe('');
        expect(stored![0].pendingKeys).toBe(true);
        // replied_to content also blanked to prevent ciphertext leak
        expect(stored![0].replied_to?.content).toBe('');
      });
    });

    it('decrypts messages via channel key', async () => {
      const mockKey = {} as CryptoKey;
      mockGetChannelKey.mockResolvedValue(mockKey);
      mockDecryptWithKey.mockResolvedValueOnce('hello world');

      const plainMsg: MessageWithStatus = {
        ...mockMessage,
        id: 'plain-1',
        content: 'hello world',
      };
      mockFetchResponse([plainMsg]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored![0].content).toBe('hello world');
      });
    });

    it('does not publish an initial batch revoked after its internal final assertion', async () => {
      const expiringGuard = guardExpiringAfterInternalBatch();
      mockCreateChannelOperationGuard.mockReturnValueOnce(expiringGuard);
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey.mockResolvedValue('revoked plaintext');
      mockFetchResponse([
        {
          ...mockMessage,
          id: 'revoked-initial',
          content: 'revoked-ciphertext',
        },
      ]);

      const { result } = renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => expect(result.current.isLoading).toBe(false));
      expect(useChatStore.getState().messagesByChannel.get('channel-1')).toBeUndefined();
      expect(result.current.error).toBe('channel access revoked before publication');
    });

    it('refetches an initial batch fenced by an ordinary key rotation', async () => {
      const expiringGuard = guardExpiringAfterInternalBatch(
        new E2EEKeyUnavailableError('NO_KEY_YET', true)
      );
      mockCreateChannelOperationGuard
        .mockReturnValueOnce(expiringGuard)
        .mockReturnValueOnce(mockOperationGuard);
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey
        .mockResolvedValueOnce('stale plaintext')
        .mockResolvedValueOnce('fresh plaintext');
      mockFetchResponse([{ ...mockMessage, id: 'stale-initial', content: 'stale-ciphertext' }]);
      mockFetchResponse([{ ...mockMessage, id: 'fresh-initial', content: 'fresh-ciphertext' }]);

      const { result } = renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toHaveLength(1);
        expect(stored?.[0]).toMatchObject({ id: 'fresh-initial', content: 'fresh plaintext' });
      });
      expect(result.current.error).toBeNull();
    });

    it('refetches an unloaded row invalidated while its REST snapshot decrypts', async () => {
      let resolveStaleDecrypt!: (plaintext: string) => void;
      const staleDecrypt = new Promise<string>((resolve) => {
        resolveStaleDecrypt = resolve;
      });
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey
        .mockReturnValueOnce(staleDecrypt)
        .mockResolvedValueOnce('authoritative edited plaintext');
      mockFetchResponse([{ ...mockMessage, id: 'inflight-edit', content: 'stale-ciphertext' }]);
      mockFetchResponse([{ ...mockMessage, id: 'inflight-edit', content: 'edited-ciphertext' }]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockDecryptWithKey).toHaveBeenCalledTimes(1));

      act(() => {
        removeMessage('inflight-edit');
        resolveStaleDecrypt('stale plaintext');
      });

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(2));
      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toHaveLength(1);
        expect(stored?.[0]).toMatchObject({
          id: 'inflight-edit',
          content: 'authoritative edited plaintext',
        });
      });
    });

    it('does not refetch for an unrelated search invalidation during decrypt', async () => {
      let resolveDecrypt!: (plaintext: string) => void;
      const decrypt = new Promise<string>((resolve) => {
        resolveDecrypt = resolve;
      });
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey.mockReturnValueOnce(decrypt);
      mockFetchResponse([
        { ...mockMessage, id: 'current-snapshot-row', content: 'current-ciphertext' },
      ]);

      const { result } = renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockDecryptWithKey).toHaveBeenCalledTimes(1));

      act(() => {
        removeMessage('different-channel-message');
        resolveDecrypt('current plaintext');
      });

      await waitFor(() => expect(result.current.isLoading).toBe(false));
      await act(async () => {
        await Promise.resolve();
      });
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
    });

    it('never drops a loaded row when an equal-timestamp live edit wins during decrypt', async () => {
      const editedAt = '2025-01-01T12:05:00Z';
      const loadedMessage = {
        ...mockMessage,
        id: 'loaded-inflight-edit',
        content: 'previous plaintext',
        edited_at: editedAt,
      };
      useChatStore.setState({
        messagesByChannel: new Map([['channel-1', [loadedMessage]]]),
      });
      let observedEmpty = false;
      const unsubscribe = useChatStore.subscribe((state) => {
        if ((state.messagesByChannel.get('channel-1')?.length ?? 0) === 0) observedEmpty = true;
      });
      let resolveFirstDecrypt!: (plaintext: string) => void;
      const firstDecrypt = new Promise<string>((resolve) => {
        resolveFirstDecrypt = resolve;
      });
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      mockDecryptWithKey
        .mockReturnValueOnce(firstDecrypt)
        .mockResolvedValueOnce('authoritative edited plaintext');
      const fetchedEdit = {
        ...mockMessage,
        id: 'loaded-inflight-edit',
        content: 'edited-ciphertext',
        edited_at: editedAt,
      };
      mockFetchResponse([fetchedEdit]);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel' }));
      await waitFor(() => expect(mockDecryptWithKey).toHaveBeenCalledTimes(1));

      act(() => {
        useChatStore.getState().updateMessage('channel-1', 'loaded-inflight-edit', {
          content: 'live edited plaintext',
          edited_at: editedAt,
        });
        removeMessage('loaded-inflight-edit');
        resolveFirstDecrypt('authoritative edited plaintext');
      });

      await waitFor(() => {
        expect(
          useChatStore
            .getState()
            .messagesByChannel.get('channel-1')
            ?.map((message) => message.id)
        ).toEqual(['loaded-inflight-edit']);
      });
      unsubscribe();
      expect(mockApiFetch).toHaveBeenCalledTimes(1);
      expect(observedEmpty).toBe(false);
    });
  });

  // --- handleLoadMore channel-change guard ---

  describe('handleLoadMore', () => {
    it('does not update state when channel changed during pagination request', async () => {
      // Initial fetch for channel-1
      const initialMsgs = Array.from({ length: 50 }, (_, i) => ({
        ...mockMessage,
        id: `msg-${i}`,
        created_at: `2025-01-01T${String(i).padStart(2, '0')}:00:00Z`,
      }));
      mockFetchResponse(initialMsgs);

      const { result, rerender } = renderHook(
        ({ channelId }) => useMessageFetch(channelId, { type: 'channel', limit: 50 }),
        { initialProps: { channelId: 'channel-1' as string | null } }
      );

      await waitFor(() => {
        expect(result.current.hasMore).toBe(true);
      });

      // Start pagination but switch channels during the request
      let resolvePagination!: (value: unknown) => void;
      mockApiFetch.mockReturnValueOnce(
        new Promise((resolve) => {
          resolvePagination = resolve;
        })
      );

      const loadMorePromise = act(async () => {
        await result.current.handleLoadMore();
      });

      // Switch to channel-2 before pagination resolves
      mockFetchResponse([{ ...mockMessage, id: 'ch2-msg-1' }]);
      rerender({ channelId: 'channel-2' });

      // Resolve the stale pagination
      resolvePagination({ ok: true });
      mockSafeJson.mockResolvedValueOnce({ messages: [{ ...mockMessage, id: 'old-msg' }] });

      await loadMorePromise;

      // channel-1 should still have only the initial messages (stale result discarded)
      const ch1 = useChatStore.getState().messagesByChannel.get('channel-1');
      // If the stale result was properly discarded, ch1 might be the initial set
      expect(ch1?.find((m) => m.id === 'old-msg')).toBeUndefined();
    });

    it('does not prepend a page revoked after its internal final assertion', async () => {
      const initialGuard = { assertCurrent: vi.fn() };
      const expiringPaginationGuard = guardExpiringAfterInternalBatch();
      mockCreateChannelOperationGuard
        .mockReturnValueOnce(initialGuard)
        .mockReturnValueOnce(expiringPaginationGuard);
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      const callEvent = {
        ...mockMessage,
        id: 'current-call-event',
        content: '',
        type: 'call_event',
      } as MessageWithStatus;
      mockFetchResponse([callEvent]);

      const { result } = renderHook(() =>
        useMessageFetch('channel-1', { type: 'channel', limit: 1 })
      );
      await waitFor(() => expect(result.current.hasMore).toBe(true));

      mockDecryptWithKey.mockResolvedValueOnce('revoked older plaintext');
      mockFetchResponse([
        {
          ...mockMessage,
          id: 'revoked-pagination',
          content: 'revoked-older-ciphertext',
        },
      ]);

      await act(async () => {
        await result.current.handleLoadMore();
      });

      const stored = useChatStore.getState().messagesByChannel.get('channel-1');
      expect(stored?.some((message) => message.id === 'revoked-pagination')).toBe(false);
      expect(result.current.error).toBe('channel access revoked before publication');
    });

    it('refetches history when pagination crosses an ordinary key rotation', async () => {
      const initialGuard = { assertCurrent: vi.fn() };
      const expiringPaginationGuard = guardExpiringAfterInternalBatch(
        new E2EEKeyUnavailableError('NO_KEY_YET', true)
      );
      mockCreateChannelOperationGuard
        .mockReturnValueOnce(initialGuard)
        .mockReturnValueOnce(expiringPaginationGuard)
        .mockReturnValueOnce(mockOperationGuard);
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      const currentCallEvent = {
        ...mockMessage,
        id: 'current-call-event',
        content: '',
        type: 'call_event',
      } as MessageWithStatus;
      mockFetchResponse([currentCallEvent]);

      const { result } = renderHook(() =>
        useMessageFetch('channel-1', { type: 'channel', limit: 1 })
      );
      await waitFor(() => expect(result.current.hasMore).toBe(true));

      mockDecryptWithKey
        .mockResolvedValueOnce('stale older plaintext')
        .mockResolvedValueOnce('fresh current plaintext');
      mockFetchResponse([
        { ...mockMessage, id: 'stale-pagination', content: 'stale-older-ciphertext' },
      ]);
      mockFetchResponse([
        { ...mockMessage, id: 'fresh-after-rotation', content: 'fresh-current-ciphertext' },
      ]);

      await act(async () => {
        await result.current.handleLoadMore();
      });

      await waitFor(() => expect(mockApiFetch).toHaveBeenCalledTimes(3));
      await waitFor(() => {
        const stored = useChatStore.getState().messagesByChannel.get('channel-1');
        expect(stored).toHaveLength(1);
        expect(stored?.[0]).toMatchObject({
          id: 'fresh-after-rotation',
          content: 'fresh current plaintext',
        });
      });
      expect(result.current.error).toBeNull();
    });

    it('retries the same older page when an unloaded row is invalidated during decrypt', async () => {
      mockGetChannelKey.mockResolvedValue({} as CryptoKey);
      const initialRows = [
        { ...mockMessage, id: 'current-call-1', content: '', type: 'call_event' },
        { ...mockMessage, id: 'current-call-2', content: '', type: 'call_event' },
      ] as MessageWithStatus[];
      mockFetchResponse(initialRows);

      const { result } = renderHook(() =>
        useMessageFetch('channel-1', { type: 'channel', limit: 2 })
      );
      await waitFor(() => expect(result.current.hasMore).toBe(true));

      let resolveStalePage!: (plaintext: string) => void;
      const stalePageDecrypt = new Promise<string>((resolve) => {
        resolveStalePage = resolve;
      });
      mockDecryptWithKey
        .mockReturnValueOnce(stalePageDecrypt)
        .mockResolvedValueOnce('authoritative older edit');
      mockFetchResponse([
        { ...mockMessage, id: 'older-inflight-edit', content: 'stale-older-ciphertext' },
      ]);
      mockFetchResponse([
        { ...mockMessage, id: 'older-inflight-edit', content: 'edited-older-ciphertext' },
      ]);

      let loadMorePromise!: Promise<void>;
      act(() => {
        loadMorePromise = result.current.handleLoadMore();
      });
      await waitFor(() => expect(mockDecryptWithKey).toHaveBeenCalledTimes(1));

      act(() => {
        removeMessage('older-inflight-edit');
        resolveStalePage('stale older plaintext');
      });
      await act(async () => {
        await loadMorePromise;
      });

      expect(mockApiFetch).toHaveBeenCalledTimes(3);
      expect(
        useChatStore
          .getState()
          .messagesByChannel.get('channel-1')
          ?.find((message) => message.id === 'older-inflight-edit')
      ).toMatchObject({ content: 'authoritative older edit' });
      expect(result.current.hasMore).toBe(false);
    });
  });

  // --- Custom limit ---

  describe('custom limit', () => {
    it('respects custom limit parameter', async () => {
      const msgs = Array.from({ length: 10 }, (_, i) => ({
        ...mockMessage,
        id: `msg-${i}`,
      }));
      mockFetchResponse(msgs);

      renderHook(() => useMessageFetch('channel-1', { type: 'channel', limit: 10 }));

      await waitFor(() => {
        expect(mockApiFetch).toHaveBeenCalledWith(expect.stringContaining('limit=10'));
      });
    });
  });

  // --- DM endpoint ---

  describe('DM endpoint', () => {
    it('uses dm conversation endpoint', async () => {
      mockFetchResponse([]);
      renderHook(() => useMessageFetch('conv-abc', { type: 'dm' }));

      await waitFor(() => {
        expect(mockApiFetch).toHaveBeenCalledWith(
          expect.stringContaining('/api/v1/dm/conversations/conv-abc/messages')
        );
      });
    });
  });
});
