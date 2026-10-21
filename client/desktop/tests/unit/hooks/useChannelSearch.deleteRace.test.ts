import { act, renderHook, waitFor } from '@testing-library/react';
import { afterAll, afterEach, beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import type { MessageWithStatus } from '@/renderer/types/chat';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useChannelSearch } from '@/renderer/hooks/messaging/useChannelSearch';
import { RequestPacer } from '@/renderer/services/messaging/searchChunk';
import {
  clearIndex,
  getScopeCoverage,
  getSearchRecord,
  indexMessage,
  isIndexed,
  removeMessage,
  removeScope,
  searchMessages,
} from '@/renderer/services/messaging/searchService';
import { DESKTOP_SEARCH_PROFILE } from '@/renderer/services/messaging/searchProfile';

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    decryptForChannelWithVersion: vi.fn(async (_s: string, c: string) => c),
    decryptForChannel: vi.fn(async (_s: string, c: string) => c),
  },
}));

const API = 'http://localhost:8080';
const CH = 'channel-1';
const ID = 'message-deleted-during-decrypt';
const FAST = { ...DESKTOP_SEARCH_PROFILE, requestsPerMinute: 6000, requestBurst: 100 };
const createdAt = new Date(Date.now() - 86_400_000).toISOString();

/** One bulk page holding `rows`, then empty pages; the decrypt mock echoes `content`. */
function serveRows(rows: Array<{ id: string; content: string }>) {
  server.use(
    http.get(`${API}/api/v1/channels/${CH}/messages/bulk`, ({ request }) => {
      const before = new URL(request.url).searchParams.get('before');
      return HttpResponse.json({
        messages: before
          ? []
          : rows.map((r) => ({
              type: 'user', // every ordinary row (messages.type NOT NULL DEFAULT 'user')
              ...r,
              channel_id: CH,
              user_id: 'user-1',
              username: 'testuser',
              key_version: 1,
              created_at: createdAt,
              updated_at: createdAt,
            })),
      });
    })
  );
}

function chatRow(content: string): MessageWithStatus {
  return {
    id: ID,
    channel_id: CH,
    user_id: 'user-1',
    content,
    username: 'testuser',
    display_name: 'Test User',
    status: 'delivered',
    created_at: createdAt,
    updated_at: createdAt,
  };
}

const render = () =>
  renderHook(() =>
    useChannelSearch(CH, {
      debounceMs: 0,
      pacer: new RequestPacer(6000, 100),
      profile: FAST,
    })
  );

/** Holds the decrypt of `ciphertext` until released; every other row decrypts at once. */
async function holdDecrypt(ciphertext: string) {
  const started = deferred();
  const plaintext = deferred<string>();
  const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
  vi.mocked(e2eeService.decryptForChannelWithVersion).mockImplementation(async (_s, c) => {
    if (c !== ciphertext) return c;
    started.resolve();
    return plaintext.promise;
  });
  return { started: started.promise, release: plaintext.resolve };
}

/** Lets a released decrypt continuation run to the end (no network follows it). */
async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => server.resetHandlers());

describe('useChannelSearch deletion race', () => {
  beforeEach(async () => {
    resetAllStores();
    clearIndex();
    useAuthStore.getState().setAccessToken('test-token-123');
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    vi.mocked(e2eeService.decryptForChannelWithVersion)
      .mockReset()
      .mockImplementation(async (_s, c) => c);
  });

  it('does not index or emit plaintext deleted while its decrypt awaits', async () => {
    serveRows([
      { id: 'keep', content: 'visible keep' },
      { id: ID, content: 'visible deleted' },
    ]);
    const { started, release } = await holdDecrypt('visible deleted');
    const { result, unmount } = render();
    act(() => result.current.search('visible'));
    await started;

    removeMessage(ID);
    release('visible deleted');
    await waitFor(() => expect(result.current.status.kind).toBe('more'));

    expect(result.current.results.map((r) => r.id)).toEqual(['keep']);
    expect(getSearchRecord(ID)).toBeUndefined();
    expect(isIndexed(ID)).toBe(false);
    expect(searchMessages('deleted', CH)).toEqual([]);
    unmount();
  });

  it('keeps a newer authoritative edit when an older backfill decrypt resolves later', async () => {
    serveRows([{ id: ID, content: 'old plaintext' }]);
    const { started, release } = await holdDecrypt('old plaintext');
    const { result, unmount } = render();
    act(() => result.current.search('plaintext'));
    await started;

    // The chat path decrypts a live edit while the backfill decrypt is out.
    useChatStore.getState().setMessages(CH, [chatRow('newer plaintext')]);
    indexMessage(ID, 'newer plaintext', CH);
    release('old plaintext');
    await waitFor(() => expect(result.current.status.kind).toBe('more'));

    expect(result.current.results.map((r) => r.content)).toEqual(['newer plaintext']);
    expect(searchMessages('newer', CH)).toEqual([ID]);
    expect(searchMessages('old', CH)).toEqual([]);
    expect(getSearchRecord(ID)).toBeUndefined();
    unmount();
  });

  describe('a hit the chat path indexed but no longer shows', () => {
    it('is decrypted and shown from its own record', async () => {
      indexMessage(ID, 'evicted plaintext', CH); // indexed, but its chat row was evicted
      serveRows([{ id: ID, content: 'evicted plaintext' }]);
      const { result, unmount } = render();
      act(() => result.current.search('evicted'));
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      expect(result.current.results.map((r) => r.content)).toEqual(['evicted plaintext']);
      expect(getSearchRecord(ID)?.content).toBe('evicted plaintext');
      unmount();
    });

    it('a live edit during its decrypt keeps the stale text off screen', async () => {
      indexMessage(ID, 'old plaintext', CH);
      serveRows([{ id: ID, content: 'old plaintext' }]);
      const { started, release } = await holdDecrypt('old plaintext');
      const { result, unmount } = render();
      act(() => result.current.search('plaintext'));
      await started;
      indexMessage(ID, 'newer plaintext', CH); // the chat path re-indexes an edit
      release('old plaintext');
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      expect(getSearchRecord(ID)).toBeUndefined();
      expect(result.current.results.map((r) => r.content)).not.toContain('old plaintext');
      expect(searchMessages('newer', CH)).toEqual([ID]);
      unmount();
    });

    it('a delete during its decrypt keeps it out', async () => {
      indexMessage(ID, 'gone plaintext', CH);
      serveRows([{ id: ID, content: 'gone plaintext' }]);
      const { started, release } = await holdDecrypt('gone plaintext');
      const { result, unmount } = render();
      act(() => result.current.search('plaintext'));
      await started;
      removeMessage(ID);
      release('gone plaintext');
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      expect(getSearchRecord(ID)).toBeUndefined();
      expect(isIndexed(ID)).toBe(false);
      expect(result.current.results).toEqual([]);
      unmount();
    });
  });

  it('does not restore plaintext when scope access is removed as the decrypt resolves', async () => {
    serveRows([{ id: ID, content: 'revoked plaintext' }]);
    const { started, release } = await holdDecrypt('revoked plaintext');
    const { result, unmount } = render();
    act(() => result.current.search('revoked'));
    await started;

    release('revoked plaintext');
    removeScope(CH);
    await waitFor(() => expect(result.current.status.kind).toBe('more'));

    expect(isIndexed(ID)).toBe(false);
    expect(getSearchRecord(ID)).toBeUndefined();
    expect(getScopeCoverage(CH)).toBeUndefined();
    expect(result.current.results).toEqual([]);
    unmount();
  });

  it('keeps nothing when the session ends while a decrypt is out', async () => {
    serveRows([{ id: ID, content: 'late plaintext' }]);
    const { started, release } = await holdDecrypt('late plaintext');
    const { result, unmount } = render();
    act(() => result.current.search('late'));
    await started;

    act(() => result.current.endSession());
    release('late plaintext');
    await settle();

    expect(isIndexed(ID)).toBe(false);
    expect(getSearchRecord(ID)).toBeUndefined();
    expect(getScopeCoverage(CH)).toBeUndefined();
    expect(result.current.results).toEqual([]);
    unmount();
  });

  it.each([
    ['message deletion', () => removeMessage(ID)],
    ['scope removal', () => removeScope(CH)],
    ['index replacement', () => indexMessage(ID, 'replacement plaintext', CH)],
    ['index discard', () => indexMessage(ID, '', CH)],
  ])('removes plaintext already emitted after %s', async (_reason, remove) => {
    serveRows([{ id: ID, content: 'visible plaintext' }]);
    const { result, unmount } = render();

    act(() => result.current.search('visible'));
    await waitFor(() => expect(result.current.results.map((item) => item.id)).toEqual([ID]));

    act(() => remove());

    expect(result.current.results).toEqual([]);
    expect(getSearchRecord(ID)).toBeUndefined();
    unmount();
  });

  it('evicts a loaded result object when its indexed plaintext is replaced', async () => {
    const message = chatRow('visible plaintext');
    useChatStore.getState().addMessage(CH, message);
    indexMessage(message.id, message.content, CH);
    serveRows([]);
    const { result, unmount } = render();

    act(() => result.current.search('visible'));
    await waitFor(() => expect(result.current.results).toHaveLength(1));

    act(() => indexMessage(message.id, 'replacement plaintext', CH));

    expect(result.current.results).toEqual([]);
    unmount();
  });
});
