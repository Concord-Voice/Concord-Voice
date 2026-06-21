import { renderHook, waitFor, act } from '@testing-library/react';
import { useChatStore } from '@/renderer/stores/chatStore';
import { mockMessage } from '../../mocks/fixtures';
import { resetAllStores } from '../../helpers/store-helpers';
import type { MessageWithStatus } from '@/renderer/types/chat';

// Mock apiFetch and safeJson
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
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

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return mockIsInitialized;
    },
    getChannelKey: (...args: unknown[]) => mockGetChannelKey(...args),
    getChannelKeyByVersion: (...args: unknown[]) => mockGetChannelKeyByVersion(...args),
    decryptWithKey: (...args: unknown[]) => mockDecryptWithKey(...args),
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

import { useMessageFetch } from '@/renderer/hooks/useMessageFetch';

function mockFetchResponse(messages: MessageWithStatus[], ok = true) {
  const response = { ok, status: ok ? 200 : 500 };
  mockApiFetch.mockResolvedValueOnce(response);
  if (ok) {
    mockSafeJson.mockResolvedValueOnce({ messages });
  } else {
    mockSafeJson.mockResolvedValueOnce({ error: 'Server error' });
  }
}

describe('useMessageFetch — extended coverage', () => {
  beforeEach(() => {
    vi.clearAllMocks();
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

    it('leaves replied_to ciphertext on decryption failure', async () => {
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
        // replied_to content stays as ciphertext (graceful degradation)
        expect(stored![0].replied_to?.content).toBe('undecryptable-ciphertext');
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
