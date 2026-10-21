import { renderHook, act, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { mockMessage } from '../../mocks/fixtures';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import { useConnectionStore } from '@/renderer/stores/ui/connectionStore';
import { useConnectionRecovery } from '@/renderer/hooks/voice/useConnectionRecovery';
import { ConnectionState } from '@/renderer/services/messaging/websocketService';
import {
  useChannelSearch,
  mergeResults,
  resolveHits,
  type SearchResultItem,
} from '@/renderer/hooks/messaging/useChannelSearch';
import { RequestPacer } from '@/renderer/services/messaging/searchChunk';
import {
  beginSearchBackfill,
  clearIndex,
  getScopeCoverage,
  getSearchRecord,
  indexMessage,
  indexSearchRecord,
  removeScope,
  setScopeCoverage,
  type ScopeCoverage,
} from '@/renderer/services/messaging/searchService';
import { DESKTOP_SEARCH_PROFILE } from '@/renderer/services/messaging/searchProfile';

const e2ee = vi.hoisted(() => ({ initialized: true }));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return e2ee.initialized;
    },
    decryptForChannelWithVersion: vi.fn(async (_s: string, c: string) => c),
    decryptForChannel: vi.fn(async (_s: string, c: string) => c),
    processPendingKeyRequests: vi.fn(async () => {}),
  },
}));

const API = 'http://localhost:8080';
const DAY = 86_400_000;
// The hook runs on the real clock, so rows are dated from it (a fixed date would rot).
const NOW = Date.now();
const FAST = { ...DESKTOP_SEARCH_PROFILE, requestsPerMinute: 6000, requestBurst: 100 };

type Row = { id: string; created_at: string; content: string; type?: string };
const row = (id: string, ageDays: number, content = `msg ${id}`): Row => ({
  id,
  created_at: new Date(NOW - ageDays * DAY).toISOString(),
  content,
});

/**
 * Serves `rows` (newest first) honouring limit, before and since; records each URL.
 * `status` may vary by call (1-based).
 */
function serveChannel(
  scope: string,
  rows: Row[],
  seen: URL[],
  status: number | ((call: number) => number) = 200,
  headers?: Record<string, string>
) {
  server.use(
    http.get(`${API}/api/v1/channels/${scope}/messages/bulk`, ({ request }) => {
      const url = new URL(request.url);
      seen.push(url);
      const code = typeof status === 'function' ? status(seen.length) : status;
      if (code !== 200) return new HttpResponse(null, { status: code, headers });
      const limit = Number(url.searchParams.get('limit'));
      const before = url.searchParams.get('before');
      const since = url.searchParams.get('since');
      let list = rows;
      if (before) {
        // The server's cursor is a subselect on the row: a cursor it cannot find returns nothing.
        const at = list.findIndex((r) => r.id === before);
        list = at === -1 ? [] : list.slice(at + 1);
      }
      if (since) list = list.filter((r) => Date.parse(r.created_at) >= Date.parse(since));
      return HttpResponse.json({
        messages: list.slice(0, limit).map((r) => ({
          type: 'user', // every ordinary row (messages.type NOT NULL DEFAULT 'user')
          ...r,
          channel_id: scope,
          user_id: 'u-1',
          username: 'alice',
          key_version: 1,
          updated_at: r.created_at,
        })),
      });
    })
  );
}

const render = (scopes = ['ch'], pacer = new RequestPacer(6000, 100), debounceMs = 0) =>
  renderHook(({ ch }) => useChannelSearch(ch, { scopes, debounceMs, pacer, profile: FAST }), {
    initialProps: { ch: 'ch' },
  });

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  vi.useRealTimers();
  vi.restoreAllMocks();
});
beforeEach(async () => {
  resetAllStores();
  clearIndex();
  e2ee.initialized = true;
  useAuthStore.getState().setAccessToken('test-token-123');
  // Drain any queued *Once implementation (tests.md: the queue outlives the test).
  const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
  vi.mocked(e2eeService.decryptForChannelWithVersion)
    .mockReset()
    .mockImplementation(async (_s, c) => c);
  vi.mocked(e2eeService.decryptForChannel)
    .mockReset()
    .mockImplementation(async (_s, c) => c);
});

describe('useChannelSearch (#3875)', () => {
  it('starts idle with no results', () => {
    const { result } = render();
    expect(result.current.status).toEqual({ kind: 'idle' });
    expect(result.current.results).toEqual([]);
  });

  it('the first query runs one chunk and ends in more', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 120, 'hello b')], seen);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    expect(result.current.results.map((r) => r.id)).toEqual(['a']);
    expect(result.current.results[0]).toMatchObject({
      channelId: 'ch',
      authorName: 'alice',
      content: 'hello a',
    });
    expect(seen).toHaveLength(1);
  });

  it('a second query in a covered scope makes zero requests (P4)', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 2, 'world b')], seen);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => result.current.search('world'));
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toEqual(['b']));
    expect(seen).toHaveLength(1);
  });

  it('a message backfilled by query one is shown by query two (P5)', async () => {
    serveChannel('ch', [row('a', 1, 'needle haystack')], []);
    const { result } = render();
    act(() => result.current.search('haystack'));
    await waitFor(() => expect(result.current.results).toHaveLength(1));
    act(() => result.current.search('needle'));
    await waitFor(() =>
      expect(result.current.results.map((r) => r.content)).toEqual(['needle haystack'])
    );
  });

  it('loadMore runs exactly one more chunk and appends older hits', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 120, 'hello b')], seen);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => {
      result.current.loadMore();
      result.current.loadMore();
    });
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toEqual(['a', 'b']));
    expect(seen).toHaveLength(2);
  });

  it('a 429 pauses, never reports no results, then resumes', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    serveChannel('ch', [], [], 429, { 'Retry-After': '2' });
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('paused'));
    expect(result.current.results).toEqual([]);
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100);
    });
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toEqual(['a']));
    vi.useRealTimers();
  });

  it('a 429 inside a chunk resumes that chunk: same floor, only the budget it had left', async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    const seen: URL[] = [];
    const rows = Array.from({ length: 10 }, (_, i) => row(`m${i}`, i + 1, `hello ${i}`));
    // One page, then a 429 on the second request; everything after is answered.
    serveChannel('ch', rows, seen, (call) => (call === 2 ? 429 : 200), { 'Retry-After': '2' });
    const small = { ...FAST, chunkMaxMessages: 4, pageSize: 2 };
    const pacer = new RequestPacer(6000, 100);
    const { result } = renderHook(() =>
      useChannelSearch('ch', { scopes: ['ch'], debounceMs: 0, pacer, profile: small })
    );
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('paused'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2_100);
    });
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    // 2 rows before the 429 and 2 after: the 4-message chunk, not a second one.
    expect(result.current.results.map((r) => r.id)).toEqual(['m0', 'm1', 'm2', 'm3']);
    expect(seen).toHaveLength(3);
    expect(seen[2].searchParams.get('limit')).toBe('2');
    expect(seen[2].searchParams.get('since')).toBe(seen[0].searchParams.get('since'));
    vi.useRealTimers();
  });

  it('a new scope array with the same channels leaves the session alone', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a')], seen);
    const pacer = new RequestPacer(6000, 100);
    // ChatView builds the server-wide list inline, so every render hands the hook a new array.
    const { result, rerender } = renderHook(
      ({ ch }) => useChannelSearch(ch, { scopes: [ch], debounceMs: 0, pacer, profile: FAST }),
      { initialProps: { ch: 'ch' } }
    );
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    rerender({ ch: 'ch' });
    rerender({ ch: 'ch' });
    expect(result.current.status.kind).toBe('more');
    expect(result.current.results.map((r) => r.id)).toEqual(['a']);
    expect(getScopeCoverage('ch')).toBeDefined();
    expect(seen).toHaveLength(1);
  });

  it('a 500 is failed, and loadMore retries', async () => {
    serveChannel('ch', [], [], 500);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('failed'));
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    act(() => result.current.loadMore());
    await waitFor(() => expect(result.current.results).toHaveLength(1));
  });

  it('reaches finished when the channel reaches the start of its history', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [], seen); // empty window, empty probe
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status).toEqual({ kind: 'finished' }));
    expect(seen).toHaveLength(2);
    act(() => result.current.loadMore()); // nothing more to load
    expect(result.current.status).toEqual({ kind: 'finished' });
  });

  it('account reset (clearIndex) returns to idle, not failed', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => clearIndex());
    expect(result.current.status.kind).toBe('idle');
    expect(result.current.results).toEqual([]);
  });

  it('a scope invalidation removes that channel’s results', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.results).toHaveLength(1));
    act(() => removeScope('ch'));
    expect(result.current.results).toEqual([]);
    expect(result.current.status).toEqual({ kind: 'more', reachedAt: null });
  });

  it('endSession releases the session records and coverage', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => result.current.endSession());
    expect(getSearchRecord('a')).toBeUndefined();
    expect(getScopeCoverage('ch')).toBeUndefined();
    expect(result.current.status).toEqual({ kind: 'idle' });
  });

  it('a channel change ends the session', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result, rerender } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    rerender({ ch: 'other' });
    expect(getScopeCoverage('ch')).toBeUndefined();
    expect(getSearchRecord('a')).toBeUndefined();
  });

  it('unmounting ends the session', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result, unmount } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    unmount();
    expect(getScopeCoverage('ch')).toBeUndefined();
    expect(getSearchRecord('a')).toBeUndefined();
  });

  it('connection-recovered drops search-only rows and offers more', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    useChatStore
      .getState()
      .setMessages('ch', [{ ...mockMessage, id: 'live', channel_id: 'ch', content: 'hello live' }]);
    indexMessage('live', 'hello live', 'ch');
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() =>
      expect(result.current.results.map((r) => r.id).sort()).toEqual(['a', 'live'])
    );
    act(() => {
      globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
    });
    expect(getScopeCoverage('ch')).toBeUndefined();
    // 'a' may have been edited or deleted while offline; the chat-owned row stays.
    expect(result.current.results.map((r) => r.id)).toEqual(['live']);
    expect(result.current.status).toEqual({ kind: 'more', reachedAt: null });
  });

  it('a reopened panel inherits the 429 pause (shared pacer)', async () => {
    const pacer = new RequestPacer(6000, 100);
    pacer.block(Date.now() + 60_000);
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1)], seen);
    const { result } = render(['ch'], pacer);
    act(() => result.current.search('msg'));
    await waitFor(() => expect(result.current.status.kind).toBe('paused'));
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(seen).toHaveLength(0);
  });

  it('an empty query goes idle and sends nothing', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a')], seen);
    const { result } = render();
    act(() => result.current.search('   '));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(result.current.status).toEqual({ kind: 'idle' });
    // Positive control: a real query in the same render does fetch, once.
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    expect(seen).toHaveLength(1);
  });

  it('cancel clears the results and the status', async () => {
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.results).toHaveLength(1));
    act(() => result.current.cancel());
    expect(result.current.results).toEqual([]);
    expect(result.current.status).toEqual({ kind: 'idle' });
  });

  it('debounces rapid queries into one search', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a')], seen);
    const { result } = render(['ch'], new RequestPacer(6000, 100), 30);
    act(() => {
      result.current.search('h');
      result.current.search('he');
      result.current.search('hello');
    });
    expect(result.current.status).toEqual({ kind: 'idle' });
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    expect(seen).toHaveLength(1);
    expect(result.current.results.map((r) => r.id)).toEqual(['a']);
  });

  // The old run must not keep spending budget while the next query waits out
  // its debounce (#3875 review).
  it('a new query stops the running chunk at once, not when its debounce ends', async () => {
    let release!: () => void;
    const held = new Promise<void>((r) => (release = r));
    server.use(
      http.get(`${API}/api/v1/channels/ch/messages/bulk`, async () => {
        await held;
        return HttpResponse.json({ messages: [] });
      })
    );
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    const { result } = render(['ch'], new RequestPacer(6000, 100), 300);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(fetchSpy).toHaveBeenCalledTimes(1));
    const signal = (fetchSpy.mock.calls[0][1] as RequestInit | undefined)?.signal;
    expect(signal?.aborted).toBe(false);
    act(() => result.current.search('world'));
    expect(signal?.aborted).toBe(true);
    release();
    await waitFor(() => expect(result.current.status.kind).toBe('finished'));
  });

  // A cursor lost while the only held rows are the chat's own must move to the
  // oldest of those, not forget how far the scope was searched (#3875 review).
  it('a lost cursor moves back to the oldest row the chat still shows', async () => {
    const seen: URL[] = [];
    const rows = [row('a', 1), row('b', 2), row('d', 3), row('e', 4)];
    serveChannel('ch', rows, seen);
    useChatStore.getState().setMessages(
      'ch',
      rows.slice(0, 2).map((r) => ({ ...mockMessage, id: r.id, channel_id: 'ch', ...r }))
    );
    indexMessage('a', 'msg a', 'ch');
    indexMessage('b', 'msg b', 'ch');
    const guard = beginSearchBackfill(['ch']);
    setScopeCoverage(
      'ch',
      { cursorId: 'c', coveredFrom: rows[1].created_at, done: false, cursorIndexed: true },
      guard
    );
    guard.close();
    const { result } = render();
    act(() => result.current.search('msg'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => result.current.loadMore());
    await waitFor(() => expect(seen.some((u) => u.searchParams.get('before') === 'b')).toBe(true));
    await waitFor(() => expect(result.current.status.kind).not.toBe('running'));
  });

  it('a server-wide search drops a 403 channel and reports how far the rest reached', async () => {
    serveChannel('gone', [], [], 403);
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 120, 'hello b')], seen);
    const { result } = render(['gone', 'ch']);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    const first = result.current.status;
    expect(first.kind === 'more' && first.reachedAt).toBe(getScopeCoverage('ch')?.coveredFrom);
    act(() => result.current.loadMore());
    // The dropped channel never holds the reported date back.
    const running = result.current.status;
    expect(running.kind === 'running' && running.reachedAt).toBe(
      first.kind === 'more' ? first.reachedAt : 'unreachable'
    );
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toEqual(['a', 'b']));
    expect(seen).toHaveLength(2);
  });
});

describe('the shared pacer, account changes and reconnects (#3875)', () => {
  /** Renders the hook and records every status it ever rendered. */
  function renderRecorded(scopes: string[], pacer: RequestPacer, debounceMs = 0) {
    const kinds: string[] = [];
    const hook = renderHook(() => {
      const r = useChannelSearch('ch', { scopes, debounceMs, pacer, profile: FAST });
      kinds.push(r.status.kind);
      return r;
    });
    return { ...hook, kinds };
  }

  it('a click while another session holds the pacer pauses at once, never announcing a search', async () => {
    const pacer = new RequestPacer(6000, 100);
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 120, 'hello b')], seen);
    const { result, kinds } = renderRecorded(['ch'], pacer);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    kinds.length = 0;
    act(() => result.current.loadMore());
    expect(kinds).toContain('running'); // control: a click with the pacer free announces it
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    kinds.length = 0;
    pacer.block(Date.now() + 60_000); // a 429 from another open panel
    act(() => result.current.loadMore());
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(result.current.status).toEqual({ kind: 'paused', resumeAt: pacer.blockedUntil });
    expect(kinds).not.toContain('running');
    expect(seen).toHaveLength(2);
  });

  it('a query that needs no chunk rests paused, not more, while the pacer is blocked', async () => {
    const pacer = new RequestPacer(6000, 100);
    const guard = beginSearchBackfill(['ch']);
    const coveredFrom = new Date(NOW - DAY).toISOString();
    setScopeCoverage('ch', { cursorId: 'a', coveredFrom, done: false }, guard);
    guard.close();
    pacer.block(Date.now() + 60_000);
    const { result } = render(['ch'], pacer);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('paused'));
    expect(result.current.status).toEqual({ kind: 'paused', resumeAt: pacer.blockedUntil });
  });

  it('connection-recovered rests paused, not more, while the pacer is blocked', async () => {
    const pacer = new RequestPacer(6000, 100);
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render(['ch'], pacer);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    pacer.block(Date.now() + 60_000);
    act(() => {
      globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
    });
    expect(result.current.status).toEqual({ kind: 'paused', resumeAt: pacer.blockedUntil });
  });

  it('an abort the run did not ask for (the account changed under it) is idle, with no results', async () => {
    serveChannel('ch', [row('a', 1, 'hello a'), row('b', 120, 'hello b')], []);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toEqual(['a']));
    const fetchSpy = vi
      .spyOn(globalThis, 'fetch')
      .mockRejectedValueOnce(
        new DOMException('Request lifecycle changed before dispatch', 'AbortError')
      );
    act(() => result.current.loadMore());
    await waitFor(() => expect(fetchSpy).toHaveBeenCalled());
    await waitFor(() => expect(result.current.status.kind).not.toBe('running'));
    expect(result.current.status).toEqual({ kind: 'idle' });
    // The old account's or server's results do not stay on screen.
    expect(result.current.results).toEqual([]);
    act(() => result.current.loadMore()); // and nothing resumes the old query
    expect(result.current.status).toEqual({ kind: 'idle' });
  });

  it('a query typed before an account reset never runs under the next account', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1, 'hello a')], seen);
    const { result } = render(['ch'], new RequestPacer(6000, 100), 30);
    act(() => result.current.search('hello'));
    act(() => clearIndex()); // logout or account switch while the debounce waits
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 80));
    });
    expect(seen).toHaveLength(0);
    expect(result.current.status).toEqual({ kind: 'idle' });
  });

  it('a channel refused under the old account is searched again after a reset', async () => {
    serveChannel('gone', [], [], 403);
    serveChannel('ch', [row('a', 1, 'hello a')], []);
    const { result } = render(['gone', 'ch']);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('more'));
    act(() => clearIndex());
    const seen: URL[] = [];
    serveChannel('gone', [row('g', 1, 'hello g')], seen);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.results.map((r) => r.id)).toContain('g'));
    expect(seen.length).toBeGreaterThan(0);
  });

  describe('a chunk owed after a pause (#3875 review)', () => {
    const small = { ...FAST, chunkMaxMessages: 4, pageSize: 2 };
    const rows = Array.from({ length: 10 }, (_, i) => row(`m${i}`, i + 1, `hello ${i}`));

    function renderScoped(scopes: string[], pacer: RequestPacer, profile = small) {
      return renderHook(
        ({ sc }) => useChannelSearch('ch', { scopes: sc, debounceMs: 0, pacer, profile }),
        { initialProps: { sc: scopes } }
      );
    }

    /** One page, then a 429 inside the chunk: the session rests paused, owing the rest. */
    async function pausedMidChunk(seen: URL[]) {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      // Built after the clock is faked: the pacer and the hook must read the same one.
      const pacer = new RequestPacer(6000, 100);
      serveChannel('ch', rows, seen, (call) => (call === 2 ? 429 : 200), { 'Retry-After': '2' });
      const hook = renderScoped(['ch'], pacer);
      act(() => hook.result.current.search('hello'));
      await waitFor(() => expect(hook.result.current.status.kind).toBe('paused'));
      expect(seen).toHaveLength(2); // precondition: the 429 landed inside the chunk
      return hook;
    }

    const afterTheWait = async () => {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(2_100);
      });
    };

    it('a covered query that rests paused under another panel’s block fetches nothing when it lifts', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const pacer = new RequestPacer(6000, 100);
      const guard = beginSearchBackfill(['ch']);
      setScopeCoverage(
        'ch',
        { cursorId: 'a', coveredFrom: new Date(NOW - DAY).toISOString(), done: false },
        guard
      );
      guard.close();
      const seen: URL[] = [];
      serveChannel('ch', rows, seen);
      pacer.block(Date.now() + 2_000);
      const { result } = renderScoped(['ch'], pacer);
      act(() => result.current.search('hello'));
      await waitFor(() => expect(result.current.status.kind).toBe('paused'));
      await afterTheWait();
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      expect(seen).toHaveLength(0);
    });

    it('a chunk owed to one set of channels is not spent on another', async () => {
      const seen: URL[] = [];
      const other: URL[] = [];
      serveChannel('other', rows, other);
      const { result, rerender } = await pausedMidChunk(seen);
      rerender({ sc: ['ch', 'other'] }); // the server-wide toggle, with no new query yet
      await afterTheWait();
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      expect(seen).toHaveLength(2);
      expect(other).toHaveLength(0);
    });

    it('ending the session during the pause leaves nothing owed: the next chunk starts fresh', async () => {
      const seen: URL[] = [];
      const { result } = await pausedMidChunk(seen);
      act(() => result.current.endSession());
      act(() => result.current.search('hello')); // still blocked: rests paused again
      await waitFor(() => expect(result.current.status.kind).toBe('paused'));
      await afterTheWait();
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      // A whole 4-message chunk from the top (two pages), not the 2 the old one had left.
      const fresh = seen.slice(2);
      expect(fresh).toHaveLength(2);
      expect(fresh[0].searchParams.get('before')).toBeNull();
      expect(result.current.results.map((r) => r.id)).toEqual(['m0', 'm1', 'm2', 'm3']);
    });

    it('a reconnect during the pause leaves nothing owed: nothing runs when the wait ends', async () => {
      const seen: URL[] = [];
      const { result } = await pausedMidChunk(seen);
      act(() => {
        globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
      });
      await afterTheWait();
      await waitFor(() => expect(result.current.status).toEqual({ kind: 'more', reachedAt: null }));
      expect(seen).toHaveLength(2);
    });

    it('a second 429 on a resumed chunk keeps its floor and only what it still had left', async () => {
      vi.useFakeTimers({ shouldAdvanceTime: true });
      const seen: URL[] = [];
      serveChannel('ch', rows, seen, (call) => (call === 2 || call === 4 ? 429 : 200), {
        'Retry-After': '2',
      });
      const { result } = renderScoped(['ch'], new RequestPacer(6000, 100), {
        ...small,
        chunkMaxMessages: 6,
      });
      act(() => result.current.search('hello'));
      await waitFor(() => expect(result.current.status.kind).toBe('paused'));
      await afterTheWait();
      await waitFor(() => expect(seen).toHaveLength(4));
      await waitFor(() => expect(result.current.status.kind).toBe('paused'));
      await afterTheWait();
      await waitFor(() => expect(result.current.status.kind).toBe('more'));
      // 2 + 2 + 2: one 6-message chunk across two 429s, never a 4-message remainder twice.
      expect(result.current.results.map((r) => r.id)).toEqual(['m0', 'm1', 'm2', 'm3', 'm4', 'm5']);
      expect(seen).toHaveLength(5);
      const since = seen[0].searchParams.get('since');
      expect(seen.every((u) => u.searchParams.get('since') === since)).toBe(true);
    });
  });

  it('every channel refusing a server-wide search fails, and stays failed for the next query', async () => {
    const seen: URL[] = [];
    serveChannel('gone1', [], seen, 403);
    serveChannel('gone2', [], seen, 403);
    const { result } = render(['gone1', 'gone2']);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('failed'));
    const sent = seen.length;
    act(() => result.current.search('world'));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 10));
    });
    expect(result.current.status).toEqual({ kind: 'failed' });
    expect(seen).toHaveLength(sent);
  });

  describe('a reconnect forgets the session copies', () => {
    async function openSession() {
      serveChannel('ch', [row('a', 1, 'hello a')], []);
      const hook = render();
      act(() => hook.result.current.search('hello'));
      await waitFor(() => expect(hook.result.current.results.map((r) => r.id)).toEqual(['a']));
      expect(getSearchRecord('a')).toBeDefined(); // precondition
      return hook;
    }

    it('inside the grace period, where no connection-recovered is sent', async () => {
      const { result } = await openSession();
      const ws = { setAggressiveReconnect: vi.fn() } as unknown as Parameters<
        typeof useConnectionRecovery
      >[0];
      const recovery = renderHook(() => useConnectionRecovery(ws, async () => {}));
      act(() => useConnectionStore.setState({ phase: 'grace_period' })); // the WS dropped
      act(() => recovery.result.current(ConnectionState.CONNECTED)); // and is back
      expect(useConnectionStore.getState().phase).toBe('stable'); // the grace branch ran
      expect(getSearchRecord('a')).toBeUndefined();
      expect(result.current.results).toEqual([]);
      expect(result.current.status).toEqual({ kind: 'more', reachedAt: null });
      recovery.unmount();
    });

    it('once only, when recovery both returns to stable and sends connection-recovered', async () => {
      const { result } = await openSession();
      act(() => useConnectionStore.setState({ phase: 'recovery_a' }));
      act(() => {
        useConnectionStore.setState({ phase: 'stable' });
        globalThis.dispatchEvent(new CustomEvent('connection-recovered'));
      });
      expect(getSearchRecord('a')).toBeUndefined();
      expect(result.current.results).toEqual([]);
      expect(result.current.status).toEqual({ kind: 'more', reachedAt: null });
    });

    it('a phase change that is not a return to stable keeps them', async () => {
      await openSession();
      act(() => useConnectionStore.setState({ phase: 'grace_period' }));
      expect(getSearchRecord('a')).toBeDefined();
    });
  });
});

describe('a channel the index cannot hold (#3875)', () => {
  const big = 'x'.repeat(100_000); // 200 KB of UTF-16; the desktop cap holds 41
  const SEP = '2026-09-01T00:00:00.000Z';
  const JUL = '2026-07-01T00:00:00.000Z';

  function cover(entries: Record<string, ScopeCoverage>) {
    const guard = beginSearchBackfill(Object.keys(entries));
    for (const [scope, coverage] of Object.entries(entries)) {
      setScopeCoverage(scope, coverage, guard);
    }
    guard.close();
  }

  it('ends in full with the date it reached, and loadMore does nothing there', async () => {
    for (let i = 0; i < 39; i++) {
      indexSearchRecord({
        id: `pre-${i}`,
        scope: 'ch',
        createdAt: new Date(NOW).toISOString(),
        authorId: 'u-1',
        username: 'alice',
        content: big,
      });
    }
    const seen: URL[] = [];
    const rows = [row('r0', 1, big), row('r1', 2, big), row('r2', 3, big)];
    serveChannel('ch', rows, seen);
    const { result } = render();
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status.kind).toBe('full'));
    expect(result.current.status).toEqual({ kind: 'full', reachedAt: rows[1].created_at });
    expect(seen).toHaveLength(1);

    act(() => result.current.loadMore());
    // A chunk would have set 'running' synchronously inside this act.
    expect(result.current.status.kind).toBe('full');

    // Losing the channel recomputes the status, as it does for 'finished'.
    act(() => removeScope('ch'));
    expect(result.current.status).toEqual({ kind: 'more', reachedAt: null });
  });

  it('a full channel beside a finished one is full, not finished', async () => {
    cover({
      a: { cursorId: 'a1', coveredFrom: SEP, done: false, full: true },
      b: { cursorId: null, coveredFrom: null, done: true },
    });
    const { result } = render(['a', 'b']);
    act(() => result.current.search('hello'));
    await waitFor(() => expect(result.current.status).toEqual({ kind: 'full', reachedAt: SEP }));
  });

  it('a full channel still holds back the date while another is walked', async () => {
    cover({
      a: { cursorId: 'a1', coveredFrom: SEP, done: false, full: true },
      b: { cursorId: 'b1', coveredFrom: JUL, done: false },
    });
    const { result } = render(['a', 'b']);
    act(() => result.current.search('hello'));
    // 'a' was searched back to September only, so July would over-claim it.
    await waitFor(() => expect(result.current.status).toEqual({ kind: 'more', reachedAt: SEP }));
  });
});

describe('resolveHits', () => {
  it('shows a hit from its session record, else from a loaded chat row', () => {
    indexSearchRecord({
      id: 'rec',
      scope: 'ch',
      createdAt: '2026-10-01T00:00:00.000Z',
      authorId: 'u-1',
      username: 'alice',
      displayName: 'Alice A',
      content: 'from record',
    });
    useChatStore.getState().setMessages('ch', [
      { ...mockMessage, id: 'row', channel_id: 'ch', content: 'from row' },
      { ...mockMessage, id: 'broken', channel_id: 'ch', content: '', decryptFailed: true },
    ]);
    expect(resolveHits(['rec', 'row', 'broken', 'unknown'])).toEqual([
      {
        id: 'rec',
        channelId: 'ch',
        authorName: 'Alice A',
        createdAt: '2026-10-01T00:00:00.000Z',
        content: 'from record',
      },
      {
        id: 'row',
        channelId: 'ch',
        authorName: 'Test User',
        createdAt: mockMessage.created_at,
        content: 'from row',
      },
    ]);
  });
});

describe('mergeResults', () => {
  const item = (id: string, createdAt: string, content = id): SearchResultItem => ({
    id,
    channelId: 'ch',
    authorName: 'alice',
    createdAt,
    content,
  });

  it('keeps one card per id, newest first, ties by id', () => {
    const merged = mergeResults(
      [item('b', '2026-10-01T00:00:00.000Z'), item('c', '2026-09-01T00:00:00.000Z')],
      [item('a', '2026-10-01T00:00:00.000Z'), item('c', '2026-09-01T00:00:00.000Z', 'updated')]
    );
    expect(merged.map((r) => r.id)).toEqual(['a', 'b', 'c']);
    expect(merged[2].content).toBe('updated');
  });
});
