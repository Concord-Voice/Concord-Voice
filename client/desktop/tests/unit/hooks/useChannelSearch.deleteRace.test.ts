import { act, renderHook, waitFor } from '@testing-library/react';
import { beforeEach, describe, expect, it, vi } from 'vitest';
import type { MessageWithUser } from '@/renderer/types/chat';
import { useChatStore } from '@/renderer/stores/chatStore';

const mockDecryptForChannel = vi.fn();
const mockApiFetch = vi.fn();
const mockSafeJson = vi.fn();

vi.mock('@/renderer/services/apiClient', () => ({
  apiFetch: (...args: unknown[]) => mockApiFetch(...args),
  safeJson: (...args: unknown[]) => mockSafeJson(...args),
}));

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return true;
    },
    decryptForChannel: (...args: unknown[]) => mockDecryptForChannel(...args),
    decryptForChannelWithVersion: (...args: unknown[]) => mockDecryptForChannel(...args),
  },
}));

import { processBackfillMessage, useChannelSearch } from '@/renderer/hooks/useChannelSearch';
import { deferred } from '../../helpers/deferred';
import {
  beginSearchBackfill,
  clearIndex,
  indexMessage,
  isIndexed,
  removeMessage,
  removeScope,
  searchMessages,
} from '@/renderer/services/searchService';

function makeMessage(): MessageWithUser {
  return {
    id: 'message-deleted-during-decrypt',
    channel_id: 'channel-1',
    user_id: 'user-1',
    content: 'ciphertext',
    key_version: 1,
    username: 'testuser',
    display_name: 'Test User',
    created_at: '2025-01-01T12:00:00Z',
    updated_at: '2025-01-01T12:00:00Z',
  };
}

describe('useChannelSearch deletion race', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    clearIndex();
    useChatStore.getState().reset();
  });

  it('does not index or emit plaintext deleted while backfill decryption awaits', async () => {
    const decryptStarted = deferred<void>();
    const plaintext = deferred<string>();
    mockDecryptForChannel.mockImplementationOnce(() => {
      decryptStarted.resolve(undefined);
      return plaintext.promise;
    });
    const guard = beginSearchBackfill(['channel-1']);
    const onNewResult = vi.fn();
    const message = makeMessage();

    const processing = processBackfillMessage(
      message,
      'channel-1',
      'deleted',
      new Set(),
      guard,
      onNewResult
    );
    await decryptStarted.promise;

    removeMessage(message.id);
    plaintext.resolve('deleted plaintext');
    await processing;
    guard.close();

    expect(isIndexed(message.id)).toBe(false);
    expect(searchMessages('deleted', 'channel-1')).toEqual([]);
    expect(onNewResult).not.toHaveBeenCalled();
  });

  it('keeps a newer authoritative edit when an older backfill decrypt resolves later', async () => {
    const decryptStarted = deferred<void>();
    const plaintext = deferred<string>();
    mockDecryptForChannel.mockImplementationOnce(() => {
      decryptStarted.resolve(undefined);
      return plaintext.promise;
    });
    const guard = beginSearchBackfill(['channel-1']);
    const onNewResult = vi.fn();
    const message = makeMessage();

    const processing = processBackfillMessage(
      message,
      'channel-1',
      'old',
      new Set(),
      guard,
      onNewResult
    );
    await decryptStarted.promise;

    indexMessage(message.id, 'newer plaintext', message.channel_id);
    plaintext.resolve('old plaintext');
    await processing;
    guard.close();

    expect(searchMessages('newer', 'channel-1')).toEqual([message.id]);
    expect(searchMessages('old', 'channel-1')).toEqual([]);
    expect(onNewResult).not.toHaveBeenCalled();
  });

  it('does not restore plaintext when scope access is removed after decrypt resolves', async () => {
    const plaintext = deferred<string>();
    mockDecryptForChannel.mockReturnValueOnce(plaintext.promise);
    const guard = beginSearchBackfill(['channel-1']);
    const onNewResult = vi.fn();
    const message = makeMessage();
    const processing = processBackfillMessage(
      message,
      'channel-1',
      'revoked',
      new Set(),
      guard,
      onNewResult
    );

    plaintext.resolve('revoked plaintext');
    removeScope('channel-1');
    await processing;
    guard.close();

    expect(isIndexed(message.id)).toBe(false);
    expect(searchMessages('revoked', 'channel-1')).toEqual([]);
    expect(onNewResult).not.toHaveBeenCalled();
  });

  it.each([
    ['message deletion', (message: MessageWithUser) => removeMessage(message.id)],
    ['scope removal', (message: MessageWithUser) => removeScope(message.channel_id)],
    [
      'index replacement',
      (message: MessageWithUser) =>
        indexMessage(message.id, 'replacement plaintext', message.channel_id),
    ],
    [
      'index discard',
      (message: MessageWithUser) => indexMessage(message.id, '', message.channel_id),
    ],
  ])('removes plaintext already emitted after %s', async (_reason, remove) => {
    const message = makeMessage();
    mockDecryptForChannel.mockResolvedValueOnce('visible plaintext');
    mockApiFetch.mockResolvedValueOnce({ ok: true });
    mockSafeJson.mockResolvedValueOnce({ messages: [message] });
    const { result, unmount } = renderHook(() => useChannelSearch('channel-1'));

    act(() => result.current.search('visible'));
    await waitFor(
      () => {
        expect(result.current.results.map((item) => item.id)).toEqual([message.id]);
      },
      { timeout: 2_000 }
    );

    act(() => remove(message));

    expect(result.current.results).toEqual([]);
    unmount();
  });

  it('evicts a loaded result object when its indexed plaintext is replaced', async () => {
    const message = {
      ...makeMessage(),
      content: 'visible plaintext',
      status: 'delivered' as const,
    };
    useChatStore.getState().addMessage('channel-1', message);
    indexMessage(message.id, message.content, message.channel_id);
    mockApiFetch.mockResolvedValueOnce({ ok: true });
    mockSafeJson.mockResolvedValueOnce({ messages: [] });
    const { result, unmount } = renderHook(() => useChannelSearch('channel-1'));

    act(() => result.current.search('visible'));
    await waitFor(() => expect(result.current.results).toHaveLength(1));

    act(() => indexMessage(message.id, 'replacement plaintext', message.channel_id));

    expect(result.current.results).toEqual([]);
    unmount();
  });
});
