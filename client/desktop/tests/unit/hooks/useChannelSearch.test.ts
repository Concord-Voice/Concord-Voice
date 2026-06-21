import { renderHook, act, waitFor } from '@testing-library/react';
import { vi, describe, it, expect, beforeEach } from 'vitest';
import { useChatStore } from '@/renderer/stores/chatStore';
import { mockMessage, mockMessage2 } from '../../mocks/fixtures';
import type { MessageWithStatus, MessageWithUser } from '@/renderer/types/chat';

// Mock apiFetch and safeJson
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();
vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

// Mock e2eeService
const mockDecryptForChannel = vi.fn();
const mockDecryptForChannelWithVersion = vi.fn();
let mockE2eeInitialized = true;

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return mockE2eeInitialized;
    },
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannelWithVersion(...args),
  },
}));

// Mock searchService
const mockSearchMessages = vi.fn().mockReturnValue([]);
const mockSearchMessagesMultiScope = vi.fn().mockReturnValue([]);
const mockIndexMessage = vi.fn();
const mockIsIndexed = vi.fn().mockReturnValue(false);

vi.mock('@/renderer/services/searchService', () => ({
  searchMessages: (...args: unknown[]) => mockSearchMessages(...args),
  searchMessagesMultiScope: (...args: unknown[]) => mockSearchMessagesMultiScope(...args),
  indexMessage: (...args: unknown[]) => mockIndexMessage(...args),
  isIndexed: (...args: unknown[]) => mockIsIndexed(...args),
}));

import {
  useChannelSearch,
  decryptMessageContent,
  buildBulkUrl,
  contentMatchesQuery,
  processBackfillMessage,
  resolveMessagesFromStore,
  BACKFILL_BATCH_SIZE,
  DEBOUNCE_MS,
} from '@/renderer/hooks/useChannelSearch';

// Helper: mock a successful bulk fetch response
function mockBulkResponse(messages: MessageWithUser[], ok = true) {
  mockApiFetch.mockResolvedValueOnce({ ok, status: ok ? 200 : 500 });
  if (ok) {
    mockSafeJson.mockResolvedValueOnce({ messages });
  }
}

// Helper: create a minimal MessageWithUser
function makeMsg(overrides: Partial<MessageWithUser> = {}): MessageWithUser {
  return {
    id: 'msg-test',
    channel_id: 'channel-1',
    user_id: 'user-1',
    content: 'test content',
    username: 'testuser',
    display_name: 'Test User',
    created_at: '2025-01-01T12:00:00Z',
    updated_at: '2025-01-01T12:00:00Z',
    ...overrides,
  };
}

describe('useChannelSearch helpers', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockE2eeInitialized = true;
  });

  // --- buildBulkUrl ---

  describe('buildBulkUrl', () => {
    it('builds URL without cursor', () => {
      const url = buildBulkUrl('ch-1', undefined);
      expect(url).toBe(`/api/v1/channels/ch-1/messages/bulk?limit=${BACKFILL_BATCH_SIZE}`);
    });

    it('builds URL with cursor', () => {
      const url = buildBulkUrl('ch-1', 'msg-abc');
      expect(url).toBe(
        `/api/v1/channels/ch-1/messages/bulk?limit=${BACKFILL_BATCH_SIZE}&before=msg-abc`
      );
    });
  });

  // --- contentMatchesQuery ---

  describe('contentMatchesQuery', () => {
    it('matches case-insensitively', () => {
      expect(contentMatchesQuery('Hello World', 'hello')).toBe(true);
      expect(contentMatchesQuery('Hello World', 'WORLD')).toBe(true);
    });

    it('returns false for non-matching', () => {
      expect(contentMatchesQuery('Hello World', 'goodbye')).toBe(false);
    });

    it('matches substring', () => {
      expect(contentMatchesQuery('deployment guide updated', 'deploy')).toBe(true);
    });
  });

  // --- decryptMessageContent ---

  describe('decryptMessageContent', () => {
    it('returns decrypted content for messages', async () => {
      mockDecryptForChannel.mockResolvedValueOnce('plain text');
      const msg = makeMsg({ content: 'plain text' });
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBe('plain text');
    });

    it('returns plaintext content when e2ee not initialized', async () => {
      mockE2eeInitialized = false;
      const msg = makeMsg({ content: 'encrypted' });
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBe('encrypted');
    });

    it('decrypts with decryptForChannel for version 1', async () => {
      mockDecryptForChannel.mockResolvedValueOnce('decrypted text');
      const msg = makeMsg({ key_version: 1, content: 'enc' });
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBe('decrypted text');
      expect(mockDecryptForChannel).toHaveBeenCalledWith('ch-1', 'enc');
    });

    it('decrypts with decryptForChannelWithVersion for version > 1', async () => {
      mockDecryptForChannelWithVersion.mockResolvedValueOnce('v2 decrypted');
      const msg = makeMsg({ key_version: 3, content: 'enc-v3' });
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBe('v2 decrypted');
      expect(mockDecryptForChannelWithVersion).toHaveBeenCalledWith('ch-1', 'enc-v3', 3);
    });

    it('returns null on decryption failure', async () => {
      mockDecryptForChannel.mockRejectedValueOnce(new Error('key not found'));
      const msg = makeMsg({ key_version: 1, content: 'enc' });
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBeNull();
    });

    it('decrypts with decryptForChannel when key_version is undefined', async () => {
      mockDecryptForChannel.mockResolvedValueOnce('decrypted');
      const msg = makeMsg({ content: 'enc' });
      delete (msg as Record<string, unknown>).key_version;
      const result = await decryptMessageContent(msg, 'ch-1');
      expect(result).toBe('decrypted');
      expect(mockDecryptForChannel).toHaveBeenCalledWith('ch-1', 'enc');
    });
  });

  // --- processBackfillMessage ---

  describe('processBackfillMessage', () => {
    it('skips already-indexed messages', async () => {
      mockIsIndexed.mockReturnValueOnce(true);
      const onNewResult = vi.fn();
      await processBackfillMessage(makeMsg(), 'ch-1', 'test', new Set(), onNewResult);
      expect(mockIndexMessage).not.toHaveBeenCalled();
      expect(onNewResult).not.toHaveBeenCalled();
    });

    it('indexes and emits matching message', async () => {
      mockIsIndexed.mockReturnValueOnce(false);
      mockDecryptForChannel.mockResolvedValueOnce('hello world');
      const onNewResult = vi.fn();
      const msg = makeMsg({ id: 'msg-match', content: 'hello world' });
      await processBackfillMessage(msg, 'ch-1', 'hello', new Set(), onNewResult);
      expect(mockIndexMessage).toHaveBeenCalledWith('msg-match', 'hello world', 'ch-1');
      expect(onNewResult).toHaveBeenCalledTimes(1);
      expect(onNewResult.mock.calls[0][0]).toMatchObject({
        id: 'msg-match',
        content: 'hello world',
        status: 'delivered',
      });
    });

    it('indexes but does not emit non-matching message', async () => {
      mockIsIndexed.mockReturnValueOnce(false);
      mockDecryptForChannel.mockResolvedValueOnce('goodbye world');
      const onNewResult = vi.fn();
      const msg = makeMsg({ id: 'msg-no', content: 'goodbye world' });
      await processBackfillMessage(msg, 'ch-1', 'hello', new Set(), onNewResult);
      expect(mockIndexMessage).toHaveBeenCalledWith('msg-no', 'goodbye world', 'ch-1');
      expect(onNewResult).not.toHaveBeenCalled();
    });

    it('does not emit duplicate results', async () => {
      mockIsIndexed.mockReturnValueOnce(false);
      mockDecryptForChannel.mockResolvedValueOnce('hello');
      const onNewResult = vi.fn();
      const allResultIds = new Set(['msg-dup']);
      const msg = makeMsg({ id: 'msg-dup', content: 'hello' });
      await processBackfillMessage(msg, 'ch-1', 'hello', allResultIds, onNewResult);
      expect(onNewResult).not.toHaveBeenCalled();
    });

    it('skips message when decryption fails', async () => {
      mockIsIndexed.mockReturnValueOnce(false);
      mockDecryptForChannel.mockRejectedValueOnce(new Error('fail'));
      const onNewResult = vi.fn();
      const msg = makeMsg({ key_version: 1, content: 'enc' });
      await processBackfillMessage(msg, 'ch-1', 'test', new Set(), onNewResult);
      expect(mockIndexMessage).not.toHaveBeenCalled();
      expect(onNewResult).not.toHaveBeenCalled();
    });
  });

  // --- resolveMessagesFromStore ---

  describe('resolveMessagesFromStore', () => {
    it('returns matching messages from store', () => {
      useChatStore.setState({
        messagesByChannel: new Map([['ch-1', [mockMessage, mockMessage2]]]),
      });

      const result = resolveMessagesFromStore(['msg-1']);
      expect(result).toHaveLength(1);
      expect(result[0].id).toBe('msg-1');
    });

    it('returns empty array for no matches', () => {
      useChatStore.setState({
        messagesByChannel: new Map([['ch-1', [mockMessage]]]),
      });

      const result = resolveMessagesFromStore(['nonexistent']);
      expect(result).toHaveLength(0);
    });

    it('resolves across multiple channels', () => {
      useChatStore.setState({
        messagesByChannel: new Map([
          ['ch-1', [mockMessage]],
          ['ch-2', [mockMessage2]],
        ]),
      });

      const result = resolveMessagesFromStore(['msg-1', 'msg-2']);
      expect(result).toHaveLength(2);
    });
  });
});

describe('useChannelSearch hook', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockE2eeInitialized = true;
    useChatStore.setState({
      messagesByChannel: new Map(),
    });
  });

  it('returns initial state', () => {
    const { result } = renderHook(() => useChannelSearch('channel-1'));
    expect(result.current.results).toEqual([]);
    expect(result.current.isSearching).toBe(false);
    expect(result.current.progress).toBeNull();
  });

  it('does nothing when channelId is null', async () => {
    const { result } = renderHook(() => useChannelSearch(null));
    act(() => result.current.search('hello'));

    // Wait past debounce
    await new Promise((r) => setTimeout(r, DEBOUNCE_MS + 100));

    expect(result.current.isSearching).toBe(false);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('does nothing when query is empty', async () => {
    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search(''));

    await new Promise((r) => setTimeout(r, DEBOUNCE_MS + 100));

    expect(result.current.isSearching).toBe(false);
    expect(mockApiFetch).not.toHaveBeenCalled();
  });

  it('uses searchMessages for single-channel scope', async () => {
    mockSearchMessages.mockReturnValueOnce([]);
    mockBulkResponse([]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));

    await waitFor(() => {
      expect(mockSearchMessages).toHaveBeenCalledWith('hello', 'channel-1');
    });
  });

  it('uses searchMessagesMultiScope when scopes provided', async () => {
    mockSearchMessagesMultiScope.mockReturnValueOnce([]);
    mockBulkResponse([]);
    mockBulkResponse([]);

    const { result } = renderHook(() =>
      useChannelSearch('channel-1', { scopes: ['ch-1', 'ch-2'] })
    );
    act(() => result.current.search('hello'));

    await waitFor(() => {
      expect(mockSearchMessagesMultiScope).toHaveBeenCalledWith('hello', ['ch-1', 'ch-2']);
    });
  });

  it('returns instant results from index', async () => {
    mockSearchMessages.mockReturnValueOnce(['msg-1']);
    useChatStore.setState({
      messagesByChannel: new Map([['channel-1', [mockMessage]]]),
    });
    mockBulkResponse([]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('Hello'));

    await waitFor(() => {
      expect(result.current.results).toHaveLength(1);
      expect(result.current.results[0].id).toBe('msg-1');
    });
  });

  it('finds matches during backfill', async () => {
    mockSearchMessages.mockReturnValueOnce([]);
    mockDecryptForChannel.mockResolvedValueOnce('hello backfill');
    const backfillMsg = makeMsg({ id: 'bf-1', content: 'hello backfill' });
    mockBulkResponse([backfillMsg]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));

    await waitFor(() => {
      expect(result.current.results).toHaveLength(1);
      expect(result.current.results[0].id).toBe('bf-1');
      expect(result.current.isSearching).toBe(false);
    });
  });

  it('stops backfill on failed fetch', async () => {
    mockSearchMessages.mockReturnValueOnce([]);
    mockApiFetch.mockResolvedValueOnce({ ok: false, status: 500 });

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));

    await waitFor(() => {
      expect(result.current.isSearching).toBe(false);
    });
  });

  it('cancel() stops search and resets state', () => {
    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));
    act(() => result.current.cancel());

    expect(result.current.isSearching).toBe(false);
    expect(result.current.progress).toBeNull();
    expect(result.current.results).toEqual([]);
  });

  it('debounces search calls', async () => {
    mockSearchMessages.mockReturnValue([]);
    mockBulkResponse([]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));

    // Fire multiple searches rapidly
    act(() => {
      result.current.search('h');
      result.current.search('he');
      result.current.search('hel');
      result.current.search('hello');
    });

    // Before debounce fires
    expect(mockSearchMessages).not.toHaveBeenCalled();

    await waitFor(() => {
      expect(mockSearchMessages).toHaveBeenCalledTimes(1);
      expect(mockSearchMessages).toHaveBeenCalledWith('hello', 'channel-1');
    });
  });

  it('reports progress during backfill', async () => {
    mockSearchMessages.mockReturnValueOnce([]);
    const fullBatch = Array.from({ length: BACKFILL_BATCH_SIZE }, (_, i) =>
      makeMsg({ id: `bf-${i}`, content: `msg ${i}` })
    );
    mockBulkResponse(fullBatch);
    mockBulkResponse([makeMsg({ id: 'bf-last', content: 'last' })]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('msg'));

    await waitFor(() => {
      expect(result.current.isSearching).toBe(false);
    });
  });

  it('cleans up on unmount without errors', () => {
    const { result, unmount } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));
    unmount();
    expect(result.current.isSearching).toBe(false);
  });

  it('uses cursor from loaded messages in store', async () => {
    mockSearchMessages.mockReturnValueOnce([]);
    useChatStore.setState({
      messagesByChannel: new Map([
        ['channel-1', [{ ...mockMessage, id: 'oldest-msg' } as MessageWithStatus]],
      ]),
    });
    mockBulkResponse([]);

    const { result } = renderHook(() => useChannelSearch('channel-1'));
    act(() => result.current.search('hello'));

    await waitFor(() => {
      expect(mockApiFetch).toHaveBeenCalledWith(
        expect.stringContaining('&before=oldest-msg'),
        expect.any(Object)
      );
    });
  });
});
