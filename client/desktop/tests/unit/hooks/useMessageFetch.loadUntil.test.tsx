import { renderHook, act, waitFor } from '@testing-library/react';
import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import { useChatStore } from '@/renderer/stores/chat/chatStore';
import type { MessageWithStatus } from '@/renderer/types/chat';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';
import { clearIndex, removeMessage } from '@/renderer/services/messaging/searchService';
import { useMessageFetch, type LoadUntilResult } from '@/renderer/hooks/messaging/useMessageFetch';

// The decrypt layer is a pass-through: a row's content is its own plaintext. `guardFault` makes the
// next operation guard assertion throw once, which is how a rotation lands mid-page (A9).
const e2ee = vi.hoisted(() => ({ guardFault: null as Error | null }));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    createChannelOperationGuard: () => ({
      assertCurrent: () => {
        const fault = e2ee.guardFault;
        if (fault) {
          e2ee.guardFault = null;
          throw fault;
        }
      },
    }),
    getChannelKey: async () => {
      throw new Error('no cached key');
    },
    getChannelKeyByVersion: async () => {
      throw new Error('no cached key');
    },
    decryptWithKey: async (ciphertext: string) => ciphertext,
    decryptForChannel: async (_channelId: string, ciphertext: string) => ciphertext,
    decryptForChannelWithVersion: async (_channelId: string, ciphertext: string) => ciphertext,
  },
}));

const API = 'http://localhost:8080';
const CHANNEL = 'c1';
const MINUTE = 60_000;
const EPOCH = Date.UTC(2026, 0, 1);

/** Row `n` (1 is the oldest). Ids sort like their numbers. */
const pad = (n: number) => `m${String(n).padStart(4, '0')}`;
const ids = (from: number, to: number) =>
  Array.from({ length: to - from + 1 }, (_, i) => pad(from + i));

interface Row {
  id: string;
  channel_id: string;
  user_id: string;
  username: string;
  content: string;
  type: string;
  created_at: string;
  updated_at: string;
}

function makeRow(n: number, channel = CHANNEL): Row {
  const at = new Date(EPOCH + n * MINUTE).toISOString();
  return {
    id: pad(n),
    channel_id: channel,
    user_id: 'u-1',
    username: 'alice',
    content: `text ${pad(n)}`,
    type: 'user',
    created_at: at,
    updated_at: at,
  };
}

interface Hit {
  before: string | null;
  limit: string | null;
}
type HitKind = 'initial' | 'page';
interface Gate {
  release: () => void;
}

interface HistoryOptions {
  size?: number;
  route?: 'channel' | 'dm';
  /** The DM list route ignores `limit` and answers this many whatever is asked. */
  cap?: number;
  /** Serve this channel instead of `CHANNEL`, so two can be live at once. */
  channel?: string;
}

/**
 * A synthetic history served newest first, recording `{ before, limit }` for every list call. One
 * request of a kind can be parked on a gate, and `statuses` makes the next page requests fail.
 */
function serveHistory({
  size = 1000,
  route = 'channel',
  cap,
  channel = CHANNEL,
}: HistoryOptions = {}) {
  const rows: Row[] = [];
  for (let n = size; n >= 1; n -= 1) rows.push(makeRow(n, channel));
  const hits: Hit[] = [];
  const gates: Array<{
    kind: HitKind;
    promise: Promise<void>;
    release: () => void;
    snapshot: boolean;
  }> = [];
  const statuses: number[] = [];
  const initialStatuses: number[] = [];
  const state = {
    onAnswer: null as null | ((kind: HitKind, page: Row[]) => void),
  };
  const path =
    route === 'dm'
      ? `/api/v1/dm/conversations/${channel}/messages`
      : `/api/v1/channels/${channel}/messages`;

  server.use(
    http.get(`${API}${path}`, async ({ request }) => {
      const url = new URL(request.url);
      const before = url.searchParams.get('before');
      const limit = url.searchParams.get('limit');
      const kind: HitKind = before === null ? 'initial' : 'page';
      hits.push({ before, limit });
      const answerNow = () => {
        let list = rows;
        if (before !== null) {
          const at = rows.findIndex((r) => r.id === before);
          list = at === -1 ? [] : rows.slice(at + 1);
        }
        return list.slice(0, cap ?? Number(limit));
      };
      let frozen: Row[] | null = null;
      const parkedAt = gates.findIndex((g) => g.kind === kind);
      if (parkedAt !== -1) {
        const [parked] = gates.splice(parkedAt, 1);
        // A server composes its answer from the rows it holds when the request ARRIVES. A snapshot
        // gate keeps that order: the page is fixed now and delivered only on release.
        if (parked.snapshot) frozen = answerNow();
        await parked.promise;
      }
      if (kind === 'page' && statuses.length > 0) {
        return HttpResponse.json({ error: 'refused' }, { status: statuses.shift() });
      }
      if (kind === 'initial' && initialStatuses.length > 0) {
        return HttpResponse.json({ error: 'refused' }, { status: initialStatuses.shift() });
      }
      const page = frozen ?? answerNow();
      state.onAnswer?.(kind, page);
      return HttpResponse.json({ messages: page });
    })
  );

  const everyGate: Gate[] = [];
  return {
    hits,
    statuses,
    /** The next first-page requests fail with these statuses (a channel whose own fetch is down). */
    initialStatuses,
    /** Server-side arrival of a newer message: the next first page starts one row later. */
    addNewest(): void {
      rows.unshift(makeRow(rows.length + 1, channel));
    },
    onAnswer(fn: (kind: HitKind, page: Row[]) => void): void {
      state.onAnswer = fn;
    },
    /** Server-side deletion of the rows whose number satisfies `isGone`. A frozen answer is unaffected. */
    purge(isGone: (n: number) => boolean): void {
      for (let i = rows.length - 1; i >= 0; i -= 1) {
        if (isGone(Number(rows[i].id.slice(1)))) rows.splice(i, 1);
      }
    },
    /**
     * Parks the next request of `kind`; it is answered once the gate is released. With `snapshot`
     * the answer is composed when the request arrives, so a later `purge` cannot change it.
     */
    park(kind: HitKind, { snapshot = false }: { snapshot?: boolean } = {}): Gate {
      let release!: () => void;
      const promise = new Promise<void>((resolve) => {
        release = resolve;
      });
      gates.push({ kind, promise, release, snapshot });
      const gate = { release };
      everyGate.push(gate);
      return gate;
    },
    releaseAll(): void {
      for (const gate of everyGate) gate.release();
    },
    initialCount: () => hits.filter((h) => h.before === null).length,
    pages: () => hits.filter((h) => h.before !== null),
    pageCount: () => hits.filter((h) => h.before !== null).length,
  };
}

type History = ReturnType<typeof serveHistory>;
let history: History;

type Hook = ReturnType<typeof mountHook>;

/** Every `error` value the hook rendered with, so a transient error that a refetch clears still shows. */
const errorsSeen: Array<string | null> = [];

function mountHook(type: 'channel' | 'dm' = 'channel') {
  return renderHook(() => {
    const out = useMessageFetch(CHANNEL, { type });
    errorsSeen.push(out.error);
    return out;
  });
}

/** Mounts the hook and waits for its first page, so `loadUntil` has a head to page from. */
async function mountReady(type: 'channel' | 'dm' = 'channel'): Promise<Hook> {
  const view = mountHook(type);
  await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
  return view;
}

/** A hook whose channel can change, for the cases that move the chat between two channels. */
function mountSwitching() {
  return renderHook(({ id }) => useMessageFetch(id, { type: 'channel' }), {
    initialProps: { id: CHANNEL },
  });
}

/** `loadUntil` from the hook's current render, or `'stuck'` if it has not settled after a second. */
async function jumpOrStuck(
  view: { result: { current: Pick<ReturnType<typeof useMessageFetch>, 'loadUntil'> } },
  messageId: string,
  maxRequests: number
): Promise<LoadUntilResult | 'stuck' | undefined> {
  let out: LoadUntilResult | 'stuck' | undefined;
  await act(async () => {
    out = await Promise.race([
      view.result.current.loadUntil(messageId, maxRequests),
      new Promise<'stuck'>((resolve) => setTimeout(() => resolve('stuck'), 1000)),
    ]);
  });
  return out;
}

function storeIds(channel = CHANNEL): string[] {
  return (useChatStore.getState().messagesByChannel.get(channel) ?? []).map((m) => m.id);
}

async function runJump(
  view: Hook,
  messageId: string,
  maxRequests: number,
  signal?: AbortSignal
): Promise<LoadUntilResult> {
  let out!: LoadUntilResult;
  await act(async () => {
    out = await view.result.current.loadUntil(messageId, maxRequests, signal);
  });
  return out;
}

/** Starts a jump without waiting for it, for the cases that park its page. */
async function startJump(
  view: Hook,
  messageId: string,
  maxRequests: number,
  signal?: AbortSignal
): Promise<{ done: Promise<LoadUntilResult> }> {
  let done!: Promise<LoadUntilResult>;
  await act(async () => {
    done = view.result.current.loadUntil(messageId, maxRequests, signal);
  });
  return { done };
}

async function finish(gate: Gate, jump: { done: Promise<LoadUntilResult> }) {
  let out!: LoadUntilResult;
  await act(async () => {
    gate.release();
    out = await jump.done;
  });
  return out;
}

function dispatchWindowEvent(type: string, detail: unknown) {
  act(() => {
    globalThis.dispatchEvent(new CustomEvent(type, { detail }));
  });
}

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => {
  history.releaseAll();
  server.resetHandlers();
});
beforeEach(() => {
  resetAllStores();
  clearIndex();
  e2ee.guardFault = null;
  useAuthStore.getState().setAccessToken('test-token-123');
});

describe('useMessageFetch loadUntil (#3894)', () => {
  // kills: cursor captured once
  it('A1 target on page 3: found in 3 requests, each cursor is the oldest id then in the store', async () => {
    history = serveHistory();
    const view = await mountReady();
    expect(storeIds()[0]).toBe(pad(951));
    expect(history.initialCount()).toBe(1);

    const out = await runJump(view, pad(700), 10);

    expect(out).toEqual({ status: 'found', requests: 3 });
    expect(history.pages().map((h) => h.before)).toEqual([pad(951), pad(851), pad(751)]);
    expect(new Set(history.pages().map((h) => h.before)).size).toBe(3);
    expect(history.pages().map((h) => h.limit)).toEqual(['100', '100', '100']);
    expect(storeIds()).toContain(pad(700));
    expect(storeIds()).toHaveLength(350);
  });

  describe('A2 endless full pages', () => {
    // kills: >= turned into >; counting turns, not attempts
    it('A2 with a reconcile retry on one page: too-far, 10 requests, exactly 10 hits after the initial one', async () => {
      history = serveHistory({ size: 1200 });
      const view = await mountReady();
      let pagesAnswered = 0;
      history.onAnswer((kind, page) => {
        if (kind !== 'page') return;
        pagesAnswered += 1;
        // The third page request loses a row to a deletion while it is in flight.
        if (pagesAnswered === 3) removeMessage(page[0].id);
      });

      const out = await runJump(view, 'absent', 10);

      expect(out).toEqual({ status: 'too-far', requests: 10 });
      expect(history.pageCount()).toBe(10);
      // The retry was reached: one cursor was asked for twice, so nine pages cost ten requests.
      const cursors = history.pages().map((h) => h.before);
      expect(cursors.length - new Set(cursors).size).toBe(1);
      expect(history.initialCount()).toBe(1);
    });

    // kills: >= turned into >; counting turns, not attempts
    it('A2 without the invalidation: too-far, 10 requests, exactly 10 hits after the initial one', async () => {
      history = serveHistory({ size: 1200 });
      const view = await mountReady();

      const out = await runJump(view, 'absent', 10);

      expect(out).toEqual({ status: 'too-far', requests: 10 });
      expect(history.pageCount()).toBe(10);
      const cursors = history.pages().map((h) => h.before);
      expect(new Set(cursors).size).toBe(10);
    });

    // kills: attempts not clipped to the remaining request budget
    it('A2c a reconcile conflict on the last allowed request: too-far, 10 requests, no 11th hit', async () => {
      history = serveHistory({ size: 1200 });
      const view = await mountReady();
      let pagesAnswered = 0;
      history.onAnswer((kind, page) => {
        if (kind !== 'page') return;
        pagesAnswered += 1;
        // The tenth and last allowed request loses a row to a deletion while it is in flight.
        if (pagesAnswered === 10) removeMessage(page[0].id);
      });

      const out = await runJump(view, 'absent', 10);

      // A retry here would be an eleventh request: the page must not outspend the budget.
      expect(pagesAnswered).toBe(10);
      expect(out).toEqual({ status: 'too-far', requests: 10 });
      expect(history.pageCount()).toBe(10);
      expect(history.initialCount()).toBe(1);
    });
  });

  // kills: exhausted check removed
  it('A3 history ends after 70 rows: exhausted, one request, one hit', async () => {
    history = serveHistory({ size: 70 });
    const view = await mountReady();
    expect(storeIds()).toHaveLength(50);

    const out = await runJump(view, 'absent', 10);

    expect(out).toEqual({ status: 'exhausted', requests: 1 });
    expect(history.pageCount()).toBe(1);
    expect(storeIds()).toHaveLength(70);
  });

  // kills: exhausted check removed
  it('A3b history ends on the request that spends the budget: exhausted, not too-far', async () => {
    history = serveHistory({ size: 70 });
    const view = await mountReady();

    const out = await runJump(view, 'absent', 1);

    expect(out).toEqual({ status: 'exhausted', requests: 1 });
    expect(history.pageCount()).toBe(1);
  });

  // kills: hasMore compared with the hook's limit
  it('A4a a channel jump asks for 100 and a full page keeps hasMore true so a second request follows', async () => {
    history = serveHistory();
    const view = await mountReady();
    expect(history.pages()).toHaveLength(0);

    const out = await runJump(view, pad(850), 10);

    expect(out).toEqual({ status: 'found', requests: 2 });
    expect(history.pages().map((h) => h.limit)).toEqual(['100', '100']);
    // Opening the channel stays at the scroll size.
    expect(history.hits[0]).toEqual({ before: null, limit: '50' });
  });

  // kills: DM asked for 100
  it('A4b a DM jump asks for 50 and its full 50-row page keeps hasMore true so a second request follows', async () => {
    history = serveHistory({ size: 300, route: 'dm', cap: 50 });
    const view = await mountReady('dm');
    expect(storeIds()[0]).toBe(pad(251));

    const out = await runJump(view, pad(200), 10);

    expect(out).toEqual({ status: 'found', requests: 2 });
    expect(history.pages().map((h) => h.limit)).toEqual(['50', '50']);
    expect(history.pages().map((h) => h.before)).toEqual([pad(251), pad(201)]);
  });

  // kills: purge fence removed
  it('A5 purge mid-page: failed, and none of the page rows reach the store', async () => {
    history = serveHistory();
    const view = await mountReady();
    const gate = history.park('page');
    const jump = await startJump(view, 'absent', 10);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    dispatchWindowEvent('messages-purged', { scopeId: CHANNEL });
    await waitFor(() => expect(history.initialCount()).toBe(2));
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
    const out = await finish(gate, jump);

    expect(out).toEqual({ status: 'failed', requests: 1 });
    const stored = storeIds();
    for (const id of ids(851, 950)) expect(stored).not.toContain(id);
    expect(stored).toEqual(ids(951, 1000));
    expect(view.result.current.error).toBeNull();
  });

  // kills: head check removed
  it('A6a key delivered mid-page and the refetch settles first: interrupted, the store is the fresh page only', async () => {
    history = serveHistory();
    const view = await mountReady();
    const gate = history.park('page');
    const jump = await startJump(view, 'absent', 10);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    // A newer message exists by the time the refetch asks, so the head moves from 951 to 952.
    history.addNewest();
    dispatchWindowEvent('e2ee-key-delivered', { channelId: CHANNEL });
    await waitFor(() => expect(storeIds()[0]).toBe(pad(952)));
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
    expect(history.initialCount()).toBe(2);

    const out = await finish(gate, jump);

    expect(out).toEqual({ status: 'interrupted', requests: 1 });
    const stored = storeIds();
    for (const id of ids(851, 950)) expect(stored).not.toContain(id);
    expect(new Set(stored).size).toBe(stored.length);
    const times = (useChatStore.getState().messagesByChannel.get(CHANNEL) ?? []).map((m) =>
      Date.parse(m.created_at)
    );
    expect(times).toEqual([...times].sort((a, b) => a - b));
    expect(stored).toEqual(ids(952, 1001));
  });

  // kills: in-flight check removed
  it('A6b the page lands while the refetch is still parked: interrupted, then fresh page only and a second jump stays in budget', async () => {
    history = serveHistory();
    const view = await mountReady();
    const pageGate = history.park('page');
    const jump = await startJump(view, 'absent', 10);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    history.addNewest();
    const refetchGate = history.park('initial');
    dispatchWindowEvent('e2ee-key-delivered', { channelId: CHANNEL });
    await waitFor(() => expect(history.initialCount()).toBe(2));

    // The head is still 951 here, so only the in-flight refetch can refuse this page.
    expect(storeIds()[0]).toBe(pad(951));
    // Positive control: the loading state is up while the refetch is parked.
    expect(view.result.current.isLoading).toBe(true);
    const first = await finish(pageGate, jump);
    expect(first).toEqual({ status: 'interrupted', requests: 1 });
    expect(storeIds()).toEqual(ids(951, 1000));
    // The page ended, but the refetch still runs and owns the loading state.
    expect(view.result.current.isLoading).toBe(true);

    await act(async () => {
      refetchGate.release();
    });
    await waitFor(() => expect(storeIds()[0]).toBe(pad(952)));
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
    expect(storeIds()).toEqual(ids(952, 1001));

    const second = await runJump(view, pad(700), 10 - first.requests);

    expect(second.status).toBe('found');
    expect(first.requests + second.requests).toBeLessThanOrEqual(10);
    expect(history.pageCount()).toBe(first.requests + second.requests);
  });

  // kills: single-flight ref removed
  it('A7 handleLoadMore during a parked jump page joins it: one list request, both settle after release', async () => {
    history = serveHistory();
    const view = await mountReady();
    const gate = history.park('page');
    const jump = await startJump(view, pad(900), 10);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    let more!: Promise<void>;
    await act(async () => {
      more = view.result.current.handleLoadMore();
    });
    let settled = 0;
    void jump.done.then(() => (settled += 1));
    void more.then(() => (settled += 1));
    // A joined caller adds no request, so there is nothing to wait for; give a second one time to show up.
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 25));
    });
    expect(settled).toBe(0);
    expect(history.pageCount()).toBe(1);

    let out!: LoadUntilResult;
    await act(async () => {
      gate.release();
      out = await jump.done;
      await more;
    });

    expect(settled).toBe(2);
    expect(out).toEqual({ status: 'found', requests: 1 });
    expect(history.pageCount()).toBe(1);
    expect(storeIds()).toHaveLength(150);
  });

  // kills: auth snapshot removed
  it('A8 authGeneration bump mid-page: cancelled, nothing prepended', async () => {
    history = serveHistory();
    const view = await mountReady();
    const before = storeIds();
    const gate = history.park('page');
    const jump = await startJump(view, 'absent', 10);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    act(() => {
      useAuthStore.setState((s) => ({ authGeneration: s.authGeneration + 1 }));
    });
    const out = await finish(gate, jump);

    expect(out).toEqual({ status: 'cancelled', requests: 1 });
    expect(storeIds()).toEqual(before);
    expect(history.pageCount()).toBe(1);
  });

  // kills: refetch call removed; error reported
  it('A9 a pending-key error mid-jump: interrupted, a second initial page is requested, error stays null, a second call finishes', async () => {
    history = serveHistory();
    errorsSeen.length = 0;
    const view = await mountReady();
    expect(history.initialCount()).toBe(1);
    e2ee.guardFault = new E2EEKeyUnavailableError('NO_KEY_YET', true);

    const first = await runJump(view, pad(900), 10);

    expect(first).toEqual({ status: 'interrupted', requests: 1 });
    expect(view.result.current.error).toBeNull();
    await waitFor(() => expect(history.initialCount()).toBe(2));
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
    expect(view.result.current.error).toBeNull();

    const second = await runJump(view, pad(900), 10 - first.requests);

    expect(second).toEqual({ status: 'found', requests: 1 });
    expect(view.result.current.error).toBeNull();
    // The refetch clears a set error within a render, so only the render log can tell.
    expect(errorsSeen.every((e) => e === null)).toBe(true);
  });

  // kills: reportError true for jump pages
  it('A10 a page answered 429 then 403: failed each time, error stays null, nothing prepended', async () => {
    history = serveHistory();
    const view = await mountReady();
    const before = storeIds();
    history.statuses.push(429, 403);

    const first = await runJump(view, 'absent', 10);
    expect(first).toEqual({ status: 'failed', requests: 1 });
    expect(history.pageCount()).toBe(1);
    expect(view.result.current.error).toBeNull();

    const second = await runJump(view, 'absent', 10);
    expect(second).toEqual({ status: 'failed', requests: 1 });
    expect(history.pageCount()).toBe(2);
    expect(view.result.current.error).toBeNull();
    expect(storeIds()).toEqual(before);
    expect(view.result.current.isLoading).toBe(false);
  });

  // kills: idle wait removed
  it('A11 loadUntil while the initial fetch is parked: no list page request until it settles', async () => {
    history = serveHistory();
    const gate = history.park('initial');
    const view = mountHook();
    await waitFor(() => expect(history.initialCount()).toBe(1));

    const jump = await startJump(view, pad(900), 10);
    let settled = false;
    void jump.done.then(() => (settled = true));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 25));
    });
    expect(settled).toBe(false);
    expect(history.pageCount()).toBe(0);

    const out = await finish(gate, jump);

    expect(out).toEqual({ status: 'found', requests: 1 });
    expect(history.pages().map((h) => h.before)).toEqual([pad(951)]);
  });

  // kills: signal check removed
  it('A12 aborting the signal while a jump page is parked: cancelled once it lands, no further list request', async () => {
    history = serveHistory();
    const view = await mountReady();
    const controller = new AbortController();
    const gate = history.park('page');
    const jump = await startJump(view, 'absent', 10, controller.signal);
    await waitFor(() => expect(history.pageCount()).toBe(1));

    controller.abort();
    const out = await finish(gate, jump);

    expect(out).toEqual({ status: 'cancelled', requests: 1 });
    expect(history.pageCount()).toBe(1);

    // Control: the same call without an aborted signal keeps paging, so the stop above was the signal.
    const resumed = await runJump(view, 'absent', 2);
    expect(resumed).toEqual({ status: 'too-far', requests: 2 });
    expect(history.pageCount()).toBe(3);
  });

  // kills: history gate counted across channels (Codex r1 on #3995)
  it('A13 a history fetch parked for the channel the chat left does not gate paging in the new one', async () => {
    history = serveHistory();
    history.park('initial'); // c1's first page never lands during this test
    const other = serveHistory({ channel: 'c2' });
    const view = renderHook(({ id }) => useMessageFetch(id, { type: 'channel' }), {
      initialProps: { id: CHANNEL },
    });
    await waitFor(() => expect(history.initialCount()).toBe(1)); // on the wire, parked

    view.rerender({ id: 'c2' });
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
    expect(other.initialCount()).toBe(1);

    // c1's request is still parked, so a hook-wide count would hold this call until it lands.
    let out: LoadUntilResult | 'stuck' | undefined;
    await act(async () => {
      out = await Promise.race([
        view.result.current.loadUntil(pad(700), 10),
        new Promise<'stuck'>((resolve) => setTimeout(() => resolve('stuck'), 1000)),
      ]);
    });

    expect(out).toEqual({ status: 'found', requests: 3 });
    expect(other.pageCount()).toBe(3);
    expect(storeIds('c2')).toContain(pad(700));
    expect(history.initialCount()).toBe(1);
    expect(history.pageCount()).toBe(0);
  });

  // A request the hook has abandoned can publish nothing: every path in the fetch checks `aborted`.
  // So it must not gate paging either, whether or not its network call ever returns (Codex r2 on #3995).

  // kills: an abandoned fetch still counted until its network call returns
  it('A14 a history fetch superseded by a refetch, and never answered, does not gate paging', async () => {
    history = serveHistory();
    history.park('initial'); // the first request never lands during this test
    const view = mountHook();
    await waitFor(() => expect(history.initialCount()).toBe(1));

    dispatchWindowEvent('e2ee-key-delivered', { channelId: CHANNEL }); // supersedes it
    await waitFor(() => expect(history.initialCount()).toBe(2));
    await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

    const out = await jumpOrStuck(view, pad(900), 10);

    expect(out).toEqual({ status: 'found', requests: 1 });
    // The list's own scroll goes through the same gate.
    await act(async () => {
      await view.result.current.handleLoadMore();
    });
    expect(history.pageCount()).toBe(2);
  });

  // kills: the abandoned request's own end counts a second time
  it('A14b the abandoned request answering late leaves the replacement still gating paging', async () => {
    history = serveHistory();
    const abandoned = history.park('initial');
    const view = mountHook();
    await waitFor(() => expect(history.initialCount()).toBe(1));
    const replacement = history.park('initial');
    dispatchWindowEvent('e2ee-key-delivered', { channelId: CHANNEL });
    await waitFor(() => expect(history.initialCount()).toBe(2));

    const jump = await startJump(view, pad(900), 10);
    await act(async () => {
      abandoned.release();
      await new Promise((resolve) => setTimeout(resolve, 25));
    });
    // Only the replacement is live, and it is still parked.
    expect(history.pageCount()).toBe(0);

    const out = await finish(replacement, jump);

    expect(out).toEqual({ status: 'found', requests: 1 });
  });

  // kills: a wake taken as idle without looking again
  it('A14d a jump waiting on a fetch that a refetch supersedes keeps waiting for the replacement', async () => {
    history = serveHistory();
    history.park('initial'); // abandoned below, and never answered
    const view = mountHook();
    await waitFor(() => expect(history.initialCount()).toBe(1));
    const jump = await startJump(view, pad(900), 10);
    const replacement = history.park('initial');

    // The supersede ends the first request's turn at once and begins the second in the same commit.
    dispatchWindowEvent('e2ee-key-delivered', { channelId: CHANNEL });
    await waitFor(() => expect(history.initialCount()).toBe(2));
    let settled = false;
    void jump.done.then(() => (settled = true));
    await act(async () => {
      await new Promise((resolve) => setTimeout(resolve, 25));
    });
    expect(settled).toBe(false);
    expect(history.pageCount()).toBe(0);

    const out = await finish(replacement, jump);

    expect(out).toEqual({ status: 'found', requests: 1 });
  });

  // kills: leaving does not release a jump that waits on history
  it('A14cthe owner unmounting while a jump waits on a parked fetch: cancelled at once, no page request', async () => {
    history = serveHistory();
    history.park('initial');
    const view = mountHook();
    await waitFor(() => expect(history.initialCount()).toBe(1));
    const jump = await startJump(view, pad(900), 10);

    await act(async () => {
      view.unmount();
    });
    const out = await Promise.race([
      jump.done,
      new Promise<'stuck'>((resolve) => setTimeout(() => resolve('stuck'), 1000)),
    ]);

    expect(out).toEqual({ status: 'cancelled', requests: 0 });
    expect(history.pageCount()).toBe(0);
  });

  // The main chat and the voice panel for a voice channel's linked text chat each run the hook for the
  // same channel, over one set of rows. A page one publishes while the other's history fetch is live
  // is read by that fetch as an arrival and appended after the newest page (Codex r3 on #3995).
  describe('A16 two hook instances on one channel share the history fence', () => {
    /** Two instances. The server parks one first-page request, so one has its rows and the other waits. */
    async function mountPair(): Promise<{ fast: Hook; slow: Hook; gate: Gate }> {
      const gate = history.park('initial');
      const first = mountHook();
      const second = mountHook();
      await waitFor(() => expect(history.initialCount()).toBe(2));
      // Which request the server parked is not the mount order's to say; the one with rows is `fast`.
      await waitFor(() =>
        expect(first.result.current.isHistoryReady || second.result.current.isHistoryReady).toBe(
          true
        )
      );
      const fast = first.result.current.isHistoryReady ? first : second;
      return { fast, slow: fast === first ? second : first, gate };
    }

    // kills: the history count kept per hook instance
    it("A16a a jump waits for the other instance's fetch, and the rows come out in order", async () => {
      history = serveHistory();
      const { fast, slow, gate } = await mountPair();
      expect(slow.result.current.isHistoryReady).toBe(false); // its request is the parked one

      const jump = await startJump(fast, pad(900), 10);
      let settled = false;
      void jump.done.then(() => (settled = true));
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 25));
      });
      // A page published now would be read by the parked fetch as an arrival once it lands.
      expect(settled).toBe(false);
      expect(history.pageCount()).toBe(0);

      const out = await finish(gate, jump);
      await waitFor(() => expect(slow.result.current.isHistoryReady).toBe(true));

      expect(out).toEqual({ status: 'found', requests: 1 });
      expect(history.pages().map((h) => h.before)).toEqual([pad(951)]);
      expect(storeIds()).toEqual(ids(851, 1000));
    });

    // kills: the history count kept per hook instance (a page already out when the other begins)
    it('A16b a page already on the wire when the other instance begins a fetch publishes nothing', async () => {
      history = serveHistory();
      const fast = await mountReady();
      const pageGate = history.park('page');
      const jump = await startJump(fast, pad(900), 10);
      await waitFor(() => expect(history.pageCount()).toBe(1));

      const slowGate = history.park('initial');
      const slow = mountHook();
      await waitFor(() => expect(history.initialCount()).toBe(2));

      // The page lands while the other instance's fetch is live: it must be dropped, not published.
      const first = await finish(pageGate, jump);
      expect(first).toEqual({ status: 'interrupted', requests: 1 });
      expect(storeIds()).toEqual(ids(951, 1000));

      await act(async () => {
        slowGate.release();
      });
      await waitFor(() => expect(slow.result.current.isHistoryReady).toBe(true));
      expect(storeIds()).toEqual(ids(951, 1000));

      // Control: the fence is not permanent. With no fetch live the same jump pages and finds it.
      const again = await runJump(fast, pad(900), 10);
      expect(again).toEqual({ status: 'found', requests: 1 });
      expect(storeIds()).toEqual(ids(851, 1000));
    });

    // kills: the wait ignores the abort signal
    it("A16c a jump waiting on the other instance's fetch ends when its signal aborts", async () => {
      history = serveHistory();
      const { fast } = await mountPair();
      const controller = new AbortController();
      const jump = await startJump(fast, pad(900), 10, controller.signal);
      await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, 25));
      });

      controller.abort();
      const out = await Promise.race([
        jump.done,
        new Promise<'stuck'>((resolve) => setTimeout(() => resolve('stuck'), 1000)),
      ]);

      // The other instance's request is still parked, so only the signal can have ended the wait.
      expect(out).toEqual({ status: 'cancelled', requests: 0 });
      expect(history.pageCount()).toBe(0);
    });
  });

  // Older history ends per channel: a flag written for one channel says nothing about another
  // (Codex r2 on #3995). A channel whose own fetch never answered has no flag at all.
  describe('A15 pagination availability belongs to its channel', () => {
    // kills: one hasMore flag for the whole hook
    it.each([
      ['short', 5],
      ['long', 150],
    ])(
      'A15a a channel whose fetch failed is failed, not exhausted, after leaving a %s one',
      async (_name, leftSize) => {
        history = serveHistory({ size: leftSize });
        const other = serveHistory({ channel: 'c2' });
        other.initialStatuses.push(500);
        // c2 holds rows from before, but this hook never learned whether older history exists.
        useChatStore
          .getState()
          .setMessages('c2', [makeRow(1000, 'c2')] as unknown as MessageWithStatus[]);
        const view = mountSwitching();
        await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

        view.rerender({ id: 'c2' });
        await waitFor(() => expect(other.initialCount()).toBe(1));
        await waitFor(() => expect(view.result.current.error).not.toBeNull());
        await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

        const out = await jumpOrStuck(view, pad(500), 10);

        expect(out).toEqual({ status: 'failed', requests: 0 });
        expect(other.pageCount()).toBe(0);
        expect(view.result.current.hasMore).toBe(false);
      }
    );

    // kills: availability forgotten when the chat leaves a channel
    it('A15b a channel keeps its own flag across a visit elsewhere: its failed refetch still pages', async () => {
      history = serveHistory({ size: 5 }); // c1 is short, so the last flag written is false
      const other = serveHistory({ channel: 'c2' });
      const view = mountSwitching();
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

      view.rerender({ id: 'c2' });
      await waitFor(() => expect(other.initialCount()).toBe(1));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      view.rerender({ id: CHANNEL });
      await waitFor(() => expect(history.initialCount()).toBe(2));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

      other.initialStatuses.push(500);
      view.rerender({ id: 'c2' });
      await waitFor(() => expect(other.initialCount()).toBe(2));
      await waitFor(() => expect(view.result.current.error).not.toBeNull());
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      expect(storeIds('c2')).toEqual(ids(951, 1000)); // the rows from the first visit are still there

      const out = await jumpOrStuck(view, pad(900), 10);

      expect(out).toEqual({ status: 'found', requests: 1 });
      expect(other.pageCount()).toBe(1);
      expect(view.result.current.hasMore).toBe(true);
    });
  });

  // A page is asked for on one visit to a channel. Comparing the channel id passes again once the
  // chat has gone c1 -> c2 -> c1, so a page spanning the round trip would publish rows the chat
  // never saw deleted: the purge lane that clears a scope bumps the purge generation of the hook
  // mounted on that scope only, and a message deleted while the chat was away sends no event at all
  // (red-team F2 on #3995).
  describe('A17 a page belongs to the visit it was asked for', () => {
    type Switching = ReturnType<typeof mountSwitching>;

    /**
     * Starts a page for c1 (its rows composed now), moves the chat to c2, runs `whileAway`, moves it
     * back and waits for the c1 refetch the return starts. The page is still on the wire.
     */
    async function pageAcrossARoundTrip(
      begin: (view: Switching) => Promise<void>,
      whileAway: () => void
    ): Promise<{ view: Switching; gate: Gate }> {
      history = serveHistory();
      const other = serveHistory({ channel: 'c2' });
      const view = mountSwitching();
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      const gate = history.park('page', { snapshot: true });
      await begin(view);
      await waitFor(() => expect(history.pageCount()).toBe(1));

      view.rerender({ id: 'c2' });
      await waitFor(() => expect(other.initialCount()).toBe(1));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      whileAway();
      view.rerender({ id: CHANNEL });
      await waitFor(() => expect(history.initialCount()).toBe(2));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      return { view, gate };
    }

    /** What the real purge lane does for c1 while the hook is mounted on c2. */
    const purgeC1WhileAway = () => {
      history.purge((n) => n <= 950);
      act(() => {
        useChatStore.getState().clearMessages(CHANNEL);
      });
      dispatchWindowEvent('messages-purged', { scopeId: CHANNEL });
    };

    // kills: the page checks the channel id only
    it('A17a a jump page spanning a leave-and-return publishes none of the rows purged meanwhile', async () => {
      const controller = new AbortController();
      let jump!: Promise<LoadUntilResult>;
      const { gate } = await pageAcrossARoundTrip(
        async (view) => {
          await act(async () => {
            jump = view.result.current.loadUntil('absent', 10, controller.signal);
          });
        },
        () => {
          controller.abort(); // the jump hook ends the jump when the chat leaves
          purgeC1WhileAway();
        }
      );
      expect(storeIds()).toEqual(ids(951, 1000)); // the refetch shows the survivors

      let out!: LoadUntilResult;
      await act(async () => {
        gate.release();
        out = await jump;
      });

      expect(out.status).toBe('cancelled');
      expect(storeIds()).toEqual(ids(951, 1000));
    });

    // kills: the scroll path checks the channel id only
    it('A17b a scroll page spanning a leave-and-return publishes none of the rows purged meanwhile', async () => {
      let more!: Promise<void>;
      const { gate } = await pageAcrossARoundTrip(async (view) => {
        await act(async () => {
          more = view.result.current.handleLoadMore();
        });
      }, purgeC1WhileAway);
      expect(storeIds()).toEqual(ids(951, 1000));

      await act(async () => {
        gate.release();
        await more;
      });

      expect(storeIds()).toEqual(ids(951, 1000));
    });

    // kills: the visit is read from the purge generation (a deletion sends no purge event)
    it('A17c a message deleted while the chat was away does not come back with the old page', async () => {
      const controller = new AbortController();
      let jump!: Promise<LoadUntilResult>;
      const { view, gate } = await pageAcrossARoundTrip(
        async (v) => {
          await act(async () => {
            jump = v.result.current.loadUntil('absent', 10, controller.signal);
          });
        },
        () => {
          controller.abort();
          history.purge((n) => n === 900); // its author deletes one message; no event reaches c1
        }
      );

      await act(async () => {
        gate.release();
        await jump;
      });
      expect(storeIds()).toEqual(ids(951, 1000));
      expect(storeIds()).not.toContain(pad(900));

      // Control: the fence is per visit, not permanent. A jump asked for on this visit pages and
      // finds what is still there, and the deleted row stays gone.
      const again = await runJump(view, pad(899), 10);
      expect(again).toEqual({ status: 'found', requests: 1 });
      expect(storeIds()).toContain(pad(899));
      expect(storeIds()).not.toContain(pad(900));
    });

    // kills: the single-flight join ignores the visit; a page ending on a later visit clears its loading flag
    it('A17d a scroll after the return does not join the page from the earlier visit, nor does its end touch this visit', async () => {
      // Nothing is deleted meanwhile: only the join is under test, and the new page needs rows to land.
      let earlier!: Promise<void>;
      const { view, gate } = await pageAcrossARoundTrip(
        async (v) => {
          await act(async () => {
            earlier = v.result.current.handleLoadMore();
          });
        },
        () => {}
      );

      const ownGate = history.park('page');
      let more!: Promise<void>;
      await act(async () => {
        more = view.result.current.handleLoadMore();
      });
      // The earlier visit's page is still parked, so only a request of its own can have moved this.
      await waitFor(() => expect(history.pageCount()).toBe(2));
      expect(history.pages().map((h) => h.before)).toEqual([pad(951), pad(951)]);

      // The earlier page ends first (awaiting it is the gate: MSW answers turns after the release).
      await act(async () => {
        gate.release();
        await earlier;
      });
      expect(storeIds()).toEqual(ids(951, 1000)); // dropped, not published
      expect(view.result.current.isLoading).toBe(true); // this visit's page is still out

      await act(async () => {
        ownGate.release();
        await more;
      });
      expect(storeIds()).toEqual(ids(901, 1000));
      expect(view.result.current.isLoading).toBe(false);
    });

    // kills: loadUntil's wait for the fence ends on the channel id alone
    it('A17e a jump that waited on the fence across a leave-and-return ends cancelled and sends no page', async () => {
      history = serveHistory();
      const other = serveHistory({ channel: 'c2' });
      const gate = history.park('initial'); // another instance's fetch holds the c1 fence
      const holder = mountHook();
      await waitFor(() => expect(history.initialCount()).toBe(1));
      const view = mountSwitching();
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

      let jump!: Promise<LoadUntilResult>;
      await act(async () => {
        jump = view.result.current.loadUntil('absent', 10); // no signal: only the hook can end it
      });
      view.rerender({ id: 'c2' });
      await waitFor(() => expect(other.initialCount()).toBe(1));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));
      view.rerender({ id: CHANNEL });
      await waitFor(() => expect(history.initialCount()).toBe(3));
      await waitFor(() => expect(view.result.current.isHistoryReady).toBe(true));

      let out!: LoadUntilResult;
      await act(async () => {
        gate.release(); // the fence goes idle with the chat back on c1, on a later visit
        out = await jump;
      });

      expect(out).toEqual({ status: 'cancelled', requests: 0 });
      expect(history.pageCount()).toBe(0);
      await waitFor(() => expect(holder.result.current.isHistoryReady).toBe(true));
    });

    // kills: the visit token stands in for the channel check (an unmount bumps no visit)
    it('A17f a page landing after its hook unmounted publishes nothing', async () => {
      history = serveHistory();
      const view = await mountReady();
      const gate = history.park('page', { snapshot: true });
      let more!: Promise<void>;
      await act(async () => {
        more = view.result.current.handleLoadMore();
      });
      await waitFor(() => expect(history.pageCount()).toBe(1));

      view.unmount();
      await act(async () => {
        gate.release();
        await more;
      });

      expect(storeIds()).toEqual(ids(951, 1000));
    });
  });

  // `isLoading` is per instance, but the history fence is per channel. A page that ends while
  // another instance has a fetch live must clear its own flag, or its owner's list stops asking
  // for older rows for good (red-team F1 on #3995; the fence itself is A16).
  describe('A18 loading belongs to the instance that is loading', () => {
    // kills: isLoading cleared only when no instance has a fetch live on the channel
    it("A18a a page refused because another instance fetches the channel still clears this instance's loading flag", async () => {
      history = serveHistory();
      const main = await mountReady();
      const pageGate = history.park('page');
      let more!: Promise<void>;
      await act(async () => {
        more = main.result.current.handleLoadMore();
      });
      await waitFor(() => expect(history.pageCount()).toBe(1));
      expect(main.result.current.isLoading).toBe(true);

      const fetchGate = history.park('initial');
      const panel = mountHook(); // the voice panel opens on the same channel
      await waitFor(() => expect(history.initialCount()).toBe(2));

      await act(async () => {
        pageGate.release();
        await more; // refused: the panel's fetch is live
      });
      await act(async () => {
        fetchGate.release();
      });
      await waitFor(() => expect(panel.result.current.isHistoryReady).toBe(true));

      // Nothing else will ever clear it, and the list never asks for older rows while it is set.
      await waitFor(() => expect(main.result.current.isLoading).toBe(false));
      expect(panel.result.current.isLoading).toBe(false);
      expect(main.result.current.hasMore).toBe(true);

      // Obeyed, not just flagged: with the flag down the next scroll pages.
      await act(async () => {
        await main.result.current.handleLoadMore();
      });
      expect(history.pageCount()).toBe(2);
      expect(storeIds()[0]).toBe(pad(901));
    });
  });
});
