import { describe, it, expect, beforeEach, beforeAll, afterAll, afterEach, vi } from 'vitest';
import { http, HttpResponse } from 'msw';
import { server } from '../../mocks/server';
import { resetAllStores } from '../../helpers/store-helpers';
import { deferred } from '../../helpers/deferred';
import { useAuthStore } from '@/renderer/stores/auth/authStore';
import {
  RequestPacer,
  parseRetryAfterMs,
  buildBulkUrl,
  runChunk,
  reachedAt,
  decryptBulkMessage,
  DEFAULT_RETRY_AFTER_MS,
} from '@/renderer/services/messaging/searchChunk';
import {
  beginSearchBackfill,
  clearIndex,
  getScopeCoverage,
  getSearchRecord,
  indexMessage,
  indexSearchRecord,
  isIndexed,
  releaseSearchScopes,
  removeMessage,
  removeScope,
  searchMessages,
  searchMessagesMultiScope,
  setScopeCoverage,
  type ScopeCoverage,
} from '@/renderer/services/messaging/searchService';
import { DESKTOP_SEARCH_PROFILE } from '@/renderer/services/messaging/searchProfile';
import { E2EEKeyUnavailableError } from '@/renderer/services/e2ee/e2eeErrors';

const e2ee = vi.hoisted(() => ({ initialized: true }));

vi.mock('@/renderer/services/e2ee/e2eeService', () => ({
  e2eeService: {
    get isInitialized() {
      return e2ee.initialized;
    },
    decryptForChannelWithVersion: vi.fn(async (_s: string, c: string) => c),
    decryptForChannel: vi.fn(async (_s: string, c: string) => c),
  },
}));

const API = 'http://localhost:8080';
const DAY = 86_400_000;
const NOW = Date.parse('2026-10-15T12:00:00.000Z');
const FAST = { ...DESKTOP_SEARCH_PROFILE, requestsPerMinute: 6000, requestBurst: 100 };
/** The desktop chunk and page sizes, unpaced. */
const DESKTOP_FAST = FAST;

type Row = { id: string; created_at: string; content: string; type?: string };
const row = (id: string, ageDays: number, content = `msg ${id}`): Row => ({
  id,
  created_at: new Date(NOW - ageDays * DAY).toISOString(),
  content,
});

/** Serves `rows` (newest first) honouring limit, before and since; records each URL. */
function serveChannel(
  scope: string,
  rows: Row[],
  seen: URL[],
  status = 200,
  headers?: Record<string, string>
) {
  server.use(
    http.get(`${API}/api/v1/channels/${scope}/messages/bulk`, ({ request }) => {
      const url = new URL(request.url);
      seen.push(url);
      if (status !== 200) return new HttpResponse(null, { status, headers });
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

function deps(scopes: string[], overrides: Partial<Parameters<typeof runChunk>[0]> = {}) {
  const hits: string[] = [];
  return {
    hits,
    d: {
      scopes,
      query: 'msg',
      profile: FAST,
      pacer: new RequestPacer(6000, 100, () => NOW),
      guard: beginSearchBackfill(scopes),
      signal: new AbortController().signal,
      deadScopes: new Set<string>(),
      onHits: (ids: string[]) => hits.push(...ids),
      onProgress: () => {},
      now: () => NOW,
      ...overrides,
    },
  };
}

beforeAll(() => server.listen({ onUnhandledFrame: 'error' }));
afterAll(() => server.close());
afterEach(() => {
  server.resetHandlers();
  vi.unstubAllGlobals();
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

describe('RequestPacer', () => {
  it('allows the burst, then spaces requests at the rate', () => {
    let t = 0;
    const p = new RequestPacer(12, 3, () => t);
    for (let i = 0; i < 3; i++) {
      expect(p.delayMs()).toBe(0);
      p.take();
    }
    expect(p.delayMs()).toBe(5000);
    t = 5000;
    expect(p.delayMs()).toBe(0);
  });

  it('holds every request until a 429 block ends, then allows one', () => {
    let t = 0;
    const p = new RequestPacer(12, 3, () => t);
    p.block(30_000);
    expect(p.blockedUntil).toBe(30_000);
    expect(p.delayMs()).toBe(30_000);
    t = 30_000;
    expect(p.delayMs()).toBe(0);
    p.take();
    expect(p.delayMs()).toBe(5000);
  });

  it('a 429 never adds a token, and a shorter block never ends a longer one', () => {
    let t = 0;
    const p = new RequestPacer(12, 3, () => t);
    for (let i = 0; i < 3; i++) p.take(); // bucket empty
    p.block(30_000);
    p.block(1_000);
    expect(p.blockedUntil).toBe(30_000);
    t = 30_000;
    // The bucket refills from the end of the block; it is not handed a token.
    expect(p.delayMs()).toBe(5000);
  });

  it('a run of 429s answered with Retry-After: 0 sends at most burst + rate in 60 s', async () => {
    const RATE = DESKTOP_SEARCH_PROFILE.requestsPerMinute;
    const BURST = DESKTOP_SEARCH_PROFILE.requestBurst;
    let requests = 0;
    server.use(
      http.get(`${API}/api/v1/channels/ch/messages/bulk`, () => {
        requests += 1;
        return HttpResponse.json(
          { error: 'Rate limit exceeded' },
          { status: 429, headers: { 'Retry-After': '0' } }
        );
      })
    );
    let t = 0;
    const pacer = new RequestPacer(RATE, BURST, () => t);
    while (t < 60_000) {
      const wait = pacer.delayMs(); // what paced() would sleep
      if (wait > 0) {
        t += wait;
        continue;
      }
      const run = deps(['ch'], { profile: DESKTOP_SEARCH_PROFILE, pacer, now: () => t }).d;
      const outcome = await runChunk(run);
      run.guard.close();
      if (outcome.kind !== 'rateLimited') throw new Error(`expected a 429, got ${outcome.kind}`);
      t = outcome.resumeAt; // the hook's resume timer runs the next chunk, unattended
    }
    expect(requests).toBeGreaterThan(BURST); // control: the loop really ran past the burst
    expect(requests).toBeLessThanOrEqual(BURST + RATE);
  });
});

describe('parseRetryAfterMs', () => {
  it.each([
    ['30', 30_000],
    ['0', 1000],
    ['600', 120_000],
    [null, DEFAULT_RETRY_AFTER_MS],
    ['', DEFAULT_RETRY_AFTER_MS],
    ['Wed, 21 Oct 2026 07:28:00 GMT', DEFAULT_RETRY_AFTER_MS],
    ['-5', DEFAULT_RETRY_AFTER_MS],
  ])('%s → %d ms', (header, ms) => expect(parseRetryAfterMs(header)).toBe(ms));
});

it('buildBulkUrl encodes the scope and omits empty bounds', () => {
  expect(buildBulkUrl('a b', { limit: 25, before: null, since: null })).toBe(
    '/api/v1/channels/a%20b/messages/bulk?limit=25'
  );
  expect(buildBulkUrl('c', { limit: 200, before: 'm-1', since: '2026-07-17T12:00:00.000Z' })).toBe(
    '/api/v1/channels/c/messages/bulk?limit=200&before=m-1&since=2026-07-17T12%3A00%3A00.000Z'
  );
});

describe('reachedAt', () => {
  it('is null until every live scope has fetched, then the newest point reached', () => {
    const guard = beginSearchBackfill(['a', 'b', 'c']);
    setScopeCoverage(
      'a',
      { cursorId: 'a1', coveredFrom: '2026-09-01T00:00:00.000Z', done: false },
      guard
    );
    expect(reachedAt(['a', 'b'])).toBeNull();
    setScopeCoverage(
      'b',
      { cursorId: 'b1', coveredFrom: '2026-08-01T00:00:00.000Z', done: false },
      guard
    );
    setScopeCoverage('c', { cursorId: null, coveredFrom: null, done: true }, guard);
    expect(reachedAt(['a', 'b', 'c'])).toBe('2026-09-01T00:00:00.000Z');
    guard.close();
  });
});

describe('decryptBulkMessage', () => {
  const msg = (key_version?: number): Parameters<typeof decryptBulkMessage>[0] => ({
    id: 'm-1',
    user_id: 'u-1',
    username: 'alice',
    content: 'enc',
    created_at: '2026-10-01T12:00:00.000Z',
    key_version,
  });

  it('decrypts version 1 with its own epoch, not the current key', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    await expect(decryptBulkMessage(msg(1), 'ch')).resolves.toEqual({ kind: 'ok', text: 'enc' });
    expect(e2eeService.decryptForChannelWithVersion).toHaveBeenCalledWith('ch', 'enc', 1);
    expect(e2eeService.decryptForChannel).not.toHaveBeenCalled();
  });

  it('uses the channel key only for an unlabelled row', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    await expect(decryptBulkMessage(msg(), 'ch')).resolves.toEqual({ kind: 'ok', text: 'enc' });
    expect(e2eeService.decryptForChannel).toHaveBeenCalledWith('ch', 'enc');
    expect(e2eeService.decryptForChannelWithVersion).not.toHaveBeenCalled();
  });

  it('decrypts nothing when E2EE is not initialized, and asks for a retry', async () => {
    e2ee.initialized = false;
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    await expect(decryptBulkMessage(msg(1), 'ch')).resolves.toEqual({ kind: 'retry' });
    expect(e2eeService.decryptForChannelWithVersion).not.toHaveBeenCalled();
  });

  it.each([
    ['a key still on its way', new E2EEKeyUnavailableError('NO_KEY_YET', true), 'retry'],
    [
      'a revoked epoch (its successor is coming)',
      new E2EEKeyUnavailableError('REVOKED_EPOCH'),
      'retry',
    ],
    ['a server error', new E2EEKeyUnavailableError('INTERNAL_ERROR'), 'retry'],
    ['a network failure', new TypeError('Failed to fetch'), 'retry'],
    ['a refused membership', new E2EEKeyUnavailableError('NOT_MEMBER'), 'unreadable'],
    [
      'an epoch that will never arrive',
      new E2EEKeyUnavailableError('NO_KEY_YET', false),
      'unreadable',
    ],
    ['a malformed key', new E2EEKeyUnavailableError('MALFORMED_PAYLOAD'), 'unreadable'],
    ['a GCM authentication failure', new DOMException('', 'OperationError'), 'unreadable'],
    ['ciphertext that is not base64', new DOMException('', 'InvalidCharacterError'), 'unreadable'],
  ])('%s is %s', async (_label, err, kind) => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    vi.mocked(e2eeService.decryptForChannelWithVersion).mockRejectedValueOnce(err);
    await expect(decryptBulkMessage(msg(3), 'ch')).resolves.toEqual({ kind });
  });
});

describe('runChunk', () => {
  it('stops at the 90-day floor when a channel is quiet', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1), row('b', 30), row('c', 120)], seen);
    const { d, hits } = deps(['ch']);
    await expect(runChunk(d)).resolves.toEqual({ kind: 'done' });
    expect(hits.sort()).toEqual(['a', 'b']);
    expect(seen).toHaveLength(1);
    expect(seen[0].searchParams.get('since')).toBe(new Date(NOW - 90 * DAY).toISOString());
    expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'b', done: false });
  });

  it('decrypts and indexes ordinary rows, never a system row', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    const system = { ...row('sys', 2, 'msg sys ciphertext'), type: 'expiration_event' };
    serveChannel('ch', [row('a', 1), system, row('b', 3)], []);
    const { d, hits } = deps(['ch']);
    await runChunk(d);
    expect(hits.sort()).toEqual(['a', 'b']);
    expect(isIndexed('a')).toBe(true);
    expect(isIndexed('b')).toBe(true);
    expect(isIndexed('sys')).toBe(false);
    expect(getSearchRecord('sys')).toBeUndefined();
    const decrypted = vi
      .mocked(e2eeService.decryptForChannelWithVersion)
      .mock.calls.map((c) => c[1]);
    expect(decrypted.sort()).toEqual(['msg a', 'msg b']);
  });

  it('stops at the message cap when a channel is busy', async () => {
    const seen: URL[] = [];
    serveChannel(
      'ch',
      Array.from({ length: 900 }, (_, i) => row(`m${i}`, i / 100)),
      seen
    );
    const { d } = deps(['ch']);
    await runChunk(d);
    expect(seen.map((u) => u.searchParams.get('limit'))).toEqual(['200', '200', '200']);
    expect(getScopeCoverage('ch')?.cursorId).toBe('m599');
  });

  it('probes once without since before marking a quiet scope finished', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('old', 200)], seen);
    const { d, hits } = deps(['ch']);
    await runChunk(d);
    expect(seen.map((u) => u.searchParams.has('since'))).toEqual([true, false]);
    expect(hits).toEqual(['old']);
    expect(getScopeCoverage('ch')?.done).toBe(true);
  });

  it('marks a scope finished when the probe is empty too', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [], seen);
    const { d } = deps(['ch']);
    await expect(runChunk(d)).resolves.toEqual({ kind: 'done' });
    expect(seen).toHaveLength(2);
    expect(getScopeCoverage('ch')).toEqual({ cursorId: null, coveredFrom: null, done: true });
  });

  it('walks the scope whose coverage reaches newest first', async () => {
    const seen: URL[] = [];
    serveChannel(
      'a',
      Array.from({ length: 400 }, (_, i) => row(`a${i}`, i / 20)),
      seen
    ); // busy
    serveChannel(
      'b',
      Array.from({ length: 400 }, (_, i) => row(`b${i}`, i / 2)),
      seen
    ); // quiet
    const { d } = deps(['a', 'b'], { profile: { ...FAST, pageSize: 50, chunkMaxMessages: 200 } });
    await runChunk(d);
    // Both start uncovered (a first, then b). After one page a reaches back
    // ~2.5 days and b ~25 days, so a stays the newest and takes the rest.
    expect(seen.map((u) => u.pathname.split('/')[4])).toEqual(['a', 'b', 'a', 'a']);
    expect(seen.map((u) => u.searchParams.get('limit'))).toEqual(['50', '50', '50', '50']);
  });

  it('a pacer still blocked by an earlier 429 returns rateLimited without a request', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1)], seen);
    const pacer = new RequestPacer(6000, 100, () => NOW);
    pacer.block(NOW + 45_000);
    await expect(runChunk(deps(['ch'], { pacer }).d)).resolves.toEqual({
      kind: 'rateLimited',
      resumeAt: NOW + 45_000,
    });
    expect(seen).toHaveLength(0);
  });

  it('returns rateLimited with the server Retry-After and blocks the pacer', async () => {
    serveChannel('ch', [], [], 429, { 'Retry-After': '30' });
    const pacer = new RequestPacer(6000, 100, () => NOW);
    const { d } = deps(['ch'], { pacer });
    await expect(runChunk(d)).resolves.toEqual({
      kind: 'rateLimited',
      resumeAt: NOW + 30_000,
      // Nothing was fetched yet: the whole chunk is still owed.
      resume: { floorMs: NOW - 90 * DAY, remaining: FAST.chunkMaxMessages },
    });
    expect(pacer.delayMs()).toBe(30_000);
  });

  it('a resumed chunk keeps its floor and spends only what it had left', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [row('a', 1), row('b', 2), row('c', 3)], seen);
    const resume = { floorMs: NOW - 200 * DAY, remaining: 2 };
    await expect(runChunk(deps(['ch'], { resume }).d)).resolves.toEqual({ kind: 'done' });
    expect(seen).toHaveLength(1);
    expect(seen[0].searchParams.get('limit')).toBe('2');
    expect(seen[0].searchParams.get('since')).toBe(new Date(resume.floorMs).toISOString());
  });

  it('a pacer still blocked hands the owed chunk back unspent', async () => {
    const pacer = new RequestPacer(6000, 100, () => NOW);
    pacer.block(NOW + 5_000);
    const resume = { floorMs: NOW - 10 * DAY, remaining: 7 };
    await expect(runChunk(deps(['ch'], { pacer, resume }).d)).resolves.toEqual({
      kind: 'rateLimited',
      resumeAt: NOW + 5_000,
      resume,
    });
  });

  it('a 403 drops one channel of a server-wide search and keeps going', async () => {
    serveChannel('gone', [], [], 403);
    serveChannel('ok', [row('x', 1)], []);
    const { d, hits } = deps(['gone', 'ok']);
    await expect(runChunk(d)).resolves.toEqual({ kind: 'done' });
    expect(d.deadScopes.has('gone')).toBe(true);
    expect(hits).toEqual(['x']);
  });

  it('a 403 on a single-channel search is a failure', async () => {
    serveChannel('gone', [], [], 403);
    await expect(runChunk(deps(['gone']).d)).resolves.toEqual({ kind: 'failed' });
  });

  it('a server-wide search every channel refuses is a failure, never "searched all"', async () => {
    serveChannel('a', [], [], 403);
    serveChannel('b', [], [], 403);
    await expect(runChunk(deps(['a', 'b']).d)).resolves.toEqual({ kind: 'failed' });
  });

  describe('the first chunk reaches every channel (spec §3.3, D1)', () => {
    const scopeOf = (u: URL) => u.pathname.split('/')[4];

    it('30 busy channels: one request each, none left unsearched', async () => {
      const seen: URL[] = [];
      const scopes = Array.from({ length: 30 }, (_, i) => `c${i}`);
      for (const s of scopes) {
        serveChannel(
          s,
          Array.from({ length: 100 }, (_, i) => row(`${s}-${i}`, i / 100)),
          seen
        );
      }
      const { d } = deps(scopes, { profile: DESKTOP_FAST });
      await runChunk(d);
      expect(new Set(seen.map(scopeOf))).toEqual(new Set(scopes));
    });

    it('channels that finish early cannot spend the budget the rest still need', async () => {
      const seen: URL[] = [];
      // 15 channels with nothing in the window and 19 old messages each (the
      // probe finishes them), then 15 busy ones that take every message offered.
      const quiet = Array.from({ length: 15 }, (_, i) => `q${i}`);
      const busy = Array.from({ length: 15 }, (_, i) => `b${i}`);
      for (const s of quiet) {
        serveChannel(
          s,
          Array.from({ length: 19 }, (_, i) => row(`${s}-${i}`, 200 + i)),
          seen
        );
      }
      for (const s of busy) {
        serveChannel(
          s,
          Array.from({ length: 300 }, (_, i) => row(`${s}-${i}`, i / 100)),
          seen
        );
      }
      const { d } = deps([...quiet, ...busy], { profile: DESKTOP_FAST });
      await runChunk(d);
      expect(new Set(seen.map(scopeOf))).toEqual(new Set([...quiet, ...busy]));
    });
  });

  describe('a row that may decrypt later', () => {
    async function decryptFailsFor(content: string, err: Error) {
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
      vi.mocked(e2eeService.decryptForChannelWithVersion).mockImplementation(async (_s, c) => {
        if (c === content) throw err;
        return c;
      });
    }

    it('stops the walk before it, claims nothing past it, and fails the chunk', async () => {
      await decryptFailsFor('msg b', new E2EEKeyUnavailableError('NO_KEY_YET', true));
      serveChannel('ch', [row('a', 1), row('b', 2), row('c', 3)], []);
      const { d, hits } = deps(['ch']);
      await expect(runChunk(d)).resolves.toEqual({ kind: 'failed' });
      expect(hits).toEqual(['a']);
      expect(isIndexed('c')).toBe(false);
      expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'a', done: false });
      expect(getScopeCoverage('ch')?.full).toBeUndefined();
    });

    it('the next chunk retries it from where the walk stopped', async () => {
      await decryptFailsFor('msg b', new E2EEKeyUnavailableError('NO_KEY_YET', true));
      const seen: URL[] = [];
      serveChannel('ch', [row('a', 1), row('b', 2), row('c', 3)], seen);
      const first = deps(['ch']);
      await runChunk(first.d);
      first.d.guard.close();
      await decryptFailsFor('nothing', new Error('unused')); // the key has arrived
      const second = deps(['ch']);
      await expect(runChunk(second.d)).resolves.toEqual({ kind: 'done' });
      expect(seen[1].searchParams.get('before')).toBe('a');
      expect(second.hits.sort()).toEqual(['b', 'c']);
    });

    it('a stall on the first row of a first page records no coverage', async () => {
      await decryptFailsFor('msg a', new E2EEKeyUnavailableError('NO_KEY_YET', true));
      serveChannel('ch', [row('a', 1)], []);
      await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
      expect(getScopeCoverage('ch')).toBeUndefined();
    });

    it('one stalled channel does not stop the others in a server-wide search', async () => {
      await decryptFailsFor('msg s1', new E2EEKeyUnavailableError('NO_KEY_YET', true));
      serveChannel('stuck', [row('s1', 1)], []);
      serveChannel('ok', [row('o1', 1), row('o2', 2)], []);
      const { d, hits } = deps(['stuck', 'ok']);
      await expect(runChunk(d)).resolves.toEqual({ kind: 'failed' });
      expect(hits.sort()).toEqual(['o1', 'o2']);
    });

    it('a row no key will open is counted as searched and walked past', async () => {
      await decryptFailsFor('msg b', new DOMException('', 'OperationError'));
      serveChannel('ch', [row('a', 1), row('b', 2), row('c', 3)], []);
      const { d, hits } = deps(['ch']);
      await expect(runChunk(d)).resolves.toEqual({ kind: 'done' });
      expect(hits.sort()).toEqual(['a', 'c']);
    });
  });

  it.each([
    ['a single-channel search', ['b'], 'failed'],
    ['a server-wide search', ['a', 'b'], 'done'],
  ])('a 403 in %s drops what was decrypted from that channel', async (_name, scopes, kind) => {
    serveChannel('a', [], []); // has nothing to give
    serveChannel('b', [row('secret', 1, 'zebra launch codes')], []);
    const first = deps(scopes);
    await runChunk(first.d);
    first.d.guard.close();
    expect(getSearchRecord('secret')?.content).toBe('zebra launch codes'); // precondition
    serveChannel('b', [], [], 403); // the user loses access to b
    const second = deps(scopes, { deadScopes: first.d.deadScopes });
    await expect(runChunk(second.d)).resolves.toEqual({ kind });
    expect(getSearchRecord('secret')).toBeUndefined();
    expect(isIndexed('secret')).toBe(false);
    expect(searchMessagesMultiScope('zebra', ['a', 'b'])).toEqual([]);
  });

  it('a 500 is a failure, never an empty result', async () => {
    serveChannel('ch', [], [], 500);
    await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
  });

  it('a network error is a failure', async () => {
    server.use(http.get(`${API}/api/v1/channels/ch/messages/bulk`, () => HttpResponse.error()));
    await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
  });

  describe('a server that does not keep to the request', () => {
    const wire = (r: Row) => ({
      type: 'user',
      ...r,
      channel_id: 'ch',
      user_id: 'u-1',
      username: 'alice',
      key_version: 1,
    });

    it('a page longer than the limit is cut to it: no more rows are decrypted or kept', async () => {
      const rows = Array.from({ length: 50 }, (_, i) => row(`r${i}`, i / 100));
      server.use(
        http.get(`${API}/api/v1/channels/ch/messages/bulk`, () =>
          HttpResponse.json({ messages: rows.map(wire) })
        )
      );
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
      vi.mocked(e2eeService.decryptForChannelWithVersion).mockClear();
      const profile = { ...FAST, pageSize: 5, chunkMaxMessages: 5 };
      await runChunk(deps(['ch'], { profile }).d);
      expect(e2eeService.decryptForChannelWithVersion).toHaveBeenCalledTimes(5);
      expect(isIndexed('r4')).toBe(true);
      expect(isIndexed('r5')).toBe(false);
      expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'r4' });
    });

    it('a row carrying a name no server allows makes the page malformed', async () => {
      server.use(
        http.get(`${API}/api/v1/channels/ch/messages/bulk`, () =>
          HttpResponse.json({ messages: [{ ...wire(row('a', 1)), username: 'n'.repeat(300) }] })
        )
      );
      await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
      expect(isIndexed('a')).toBe(false);
    });

    // An empty id would become an empty cursor, which drops `before` and
    // fetches the newest page again; a bad time would make the next floor NaN.
    it.each([
      ['an empty id', { id: '' }],
      ['a created_at that is not a timestamp', { created_at: 'bad' }],
    ])('a row with %s makes the page malformed and records no coverage', async (_, bad) => {
      server.use(
        http.get(`${API}/api/v1/channels/ch/messages/bulk`, () =>
          HttpResponse.json({ messages: [{ ...wire(row('a', 1)), ...bad }] })
        )
      );
      await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
      expect(getScopeCoverage('ch')).toBeUndefined();
    });
  });

  it('a malformed page is a failure', async () => {
    server.use(
      http.get(`${API}/api/v1/channels/ch/messages/bulk`, () =>
        HttpResponse.json({ messages: [{ id: 7 }] })
      )
    );
    await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'failed' });
  });

  it('does not decrypt or keep a message deleted while its page decrypts', async () => {
    serveChannel('ch', [row('keep', 1), row('gone', 2)], []);
    const { d, hits } = deps(['ch']);
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    vi.mocked(e2eeService.decryptForChannelWithVersion).mockImplementationOnce(async (_s, c) => {
      removeMessage('gone');
      return c;
    });
    await runChunk(d);
    expect(getSearchRecord('gone')).toBeUndefined();
    expect(hits).not.toContain('gone');
    expect(hits).toContain('keep');
  });

  describe('a page naming a message held under another scope', () => {
    async function decrypted() {
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
      return vi.mocked(e2eeService.decryptForChannelWithVersion).mock.calls.map((c) => c[1]);
    }

    it('never reports a row the chat path indexed in a DM, even one the chat view shows', async () => {
      indexMessage('dm-msg', 'payroll password hunter2', 'dm');
      serveChannel('ch', [row('a', 1, 'payroll schedule'), row('dm-msg', 2, 'opaque')], []);
      const { d, hits } = deps(['ch'], { query: 'payroll', isDisplayable: () => true });
      await runChunk(d);
      expect(hits).toEqual(['a']); // control: the channel's own row is a hit
      expect(await decrypted()).toContain('opaque'); // not trusted as already searchable
      expect(getSearchRecord('dm-msg')).toBeUndefined();
      expect(searchMessages('payroll', 'dm')).toEqual(['dm-msg']); // the DM keeps its own row
    });

    it('never reports a record this session decrypted for another scope', async () => {
      indexSearchRecord({
        id: 'other-rec',
        scope: 'other',
        createdAt: new Date(NOW).toISOString(),
        authorId: 'u-1',
        username: 'alice',
        content: 'payroll from elsewhere',
      });
      serveChannel('ch', [row('a', 1, 'payroll schedule'), row('other-rec', 2, 'opaque')], []);
      const { d, hits } = deps(['ch'], { query: 'payroll' });
      await runChunk(d);
      expect(hits).toEqual(['a']);
      expect(await decrypted()).toContain('opaque');
      expect(getSearchRecord('other-rec')?.scope).toBe('other');
    });
  });

  it('decrypts a row the chat path indexed but no longer shows; one it shows needs no decrypt', async () => {
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    indexMessage('shown', 'msg shown', 'ch');
    indexMessage('hidden', 'msg hidden', 'ch');
    serveChannel('ch', [row('shown', 1), row('hidden', 2)], []);
    const { d, hits } = deps(['ch'], { isDisplayable: (id: string) => id === 'shown' });
    await runChunk(d);
    const decrypted = vi
      .mocked(e2eeService.decryptForChannelWithVersion)
      .mock.calls.map((c) => c[1]);
    expect(decrypted).toEqual(['msg hidden']);
    expect(getSearchRecord('hidden')?.content).toBe('msg hidden');
    expect(getSearchRecord('shown')).toBeUndefined();
    expect(hits.sort()).toEqual(['hidden', 'shown']);
  });

  it('a probe that returns a full page of old rows is not the start of history', async () => {
    const old = Array.from({ length: 200 }, (_, i) => row(`o${i}`, 100 + i / 10));
    const seen: URL[] = [];
    serveChannel('ch', old, seen);
    await runChunk(deps(['ch']).d);
    expect(seen.map((u) => u.searchParams.has('since'))).toEqual([true, false]);
    expect(seen[1].searchParams.get('limit')).toBe('200'); // exactly a page
    expect(getScopeCoverage('ch')).toMatchObject({
      cursorId: 'o199',
      coveredFrom: old[199].created_at,
      done: false,
    });
  });

  it('yields to the main thread after each slice once the budget is spent', async () => {
    const yieldSpy = vi.fn(() => Promise.resolve());
    vi.stubGlobal('scheduler', { yield: yieldSpy });
    serveChannel(
      'ch',
      Array.from({ length: 8 }, (_, i) => row(`m${i}`, i)),
      []
    );
    await runChunk(
      deps(['ch'], { profile: { ...FAST, sliceBudgetMs: 0, decryptConcurrency: 4 } }).d
    );
    expect(yieldSpy).toHaveBeenCalledTimes(2);
  });

  it('waits for the pacer between requests', async () => {
    const seen: URL[] = [];
    serveChannel('ch', [], seen); // empty window, then the probe: two requests
    const pacer = new RequestPacer(3000, 1); // one token per 20 ms, real clock
    const started = performance.now();
    await runChunk(deps(['ch'], { pacer, now: Date.now }).d);
    expect(seen).toHaveLength(2);
    expect(performance.now() - started).toBeGreaterThanOrEqual(15);
  });

  it('an abort while the pacer waits sends nothing', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch');
    serveChannel('ch', [row('a', 1)], []);
    const pacer = new RequestPacer(12, 1, () => NOW);
    pacer.take(); // empty bucket: the next request waits 5 s
    const controller = new AbortController();
    const run = runChunk(deps(['ch'], { pacer, signal: controller.signal }).d);
    controller.abort();
    await expect(run).resolves.toEqual({ kind: 'done' });
    expect(fetchSpy).not.toHaveBeenCalled();
    expect(getScopeCoverage('ch')).toBeUndefined();
  });

  describe('a cursor row deleted since the last chunk', () => {
    const rec = (r: Row) =>
      indexSearchRecord({
        id: r.id,
        scope: 'ch',
        createdAt: r.created_at,
        authorId: 'u-1',
        username: 'alice',
        content: r.content,
      });

    function cover(coverage: ScopeCoverage) {
      const guard = beginSearchBackfill(['ch']);
      setScopeCoverage('ch', coverage, guard);
      guard.close();
    }

    const before = (u: URL) =>
      `${u.searchParams.get('before')}${u.searchParams.has('since') ? '+since' : ''}`;

    it('re-anchors on the oldest record and walks past it, never reading as the start', async () => {
      const [a, b, c, d, e] = ['a', 'b', 'c', 'd', 'e'].map((id, i) => row(id, i + 1));
      rec(a);
      rec(b);
      cover({ cursorId: 'c', coveredFrom: c.created_at, done: false, cursorIndexed: true });
      const seen: URL[] = [];
      serveChannel('ch', [a, b, d, e], seen); // 'c' was deleted
      const { d: run, hits } = deps(['ch']);
      await expect(runChunk(run)).resolves.toEqual({ kind: 'done' });
      expect(seen.map(before)).toEqual(['c+since', 'c', 'b+since']);
      expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'e', done: false });
      expect(hits.sort()).toEqual(['d', 'e']);
    });

    it('with no record to re-anchor on, forgets the coverage instead of finishing', async () => {
      const [a, b, c, d] = ['a', 'b', 'c', 'd'].map((id, i) => row(id, i + 1));
      cover({ cursorId: 'c', coveredFrom: c.created_at, done: false, cursorIndexed: true });
      const seen: URL[] = [];
      serveChannel('ch', [a, b, d], seen);
      await runChunk(deps(['ch']).d);
      expect(seen.map(before)).toEqual(['c+since', 'c']);
      expect(getScopeCoverage('ch')).toBeUndefined();
    });

    it('a cursor row still held means the empty probe is the start of history', async () => {
      const [a, b] = ['a', 'b'].map((id, i) => row(id, i + 1));
      rec(a);
      rec(b);
      cover({ cursorId: 'b', coveredFrom: b.created_at, done: false, cursorIndexed: true });
      const seen: URL[] = [];
      serveChannel('ch', [a, b], seen);
      await runChunk(deps(['ch']).d);
      expect(seen.map(before)).toEqual(['b+since', 'b']);
      expect(getScopeCoverage('ch')?.done).toBe(true);
    });

    it('after a re-anchor, nothing older is the start of history', async () => {
      const [a, b, c] = ['a', 'b', 'c'].map((id, i) => row(id, i + 1));
      rec(a);
      rec(b);
      cover({ cursorId: 'c', coveredFrom: c.created_at, done: false, cursorIndexed: true });
      const seen: URL[] = [];
      serveChannel('ch', [a, b], seen);
      await runChunk(deps(['ch']).d);
      expect(seen.map(before)).toEqual(['c+since', 'c', 'b+since', 'b']);
      expect(getScopeCoverage('ch')?.done).toBe(true);
    });

    it('a re-anchored walk never moves "Searched back to" forward', async () => {
      const [a, b, c, d] = ['a', 'b', 'c', 'd'].map((id, i) => row(id, i + 1));
      const lost = row('x', 10);
      rec(a);
      rec(b);
      cover({ cursorId: 'x', coveredFrom: lost.created_at, done: false, cursorIndexed: true });
      serveChannel('ch', [a, b, c, d, row('e', 11)], []); // 'x' was deleted
      // One page of two from the anchor 'b', then the budget is spent.
      const profile = { ...FAST, pageSize: 2, chunkMaxMessages: 2 };
      await runChunk(deps(['ch'], { profile }).d);
      expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'd', coveredFrom: lost.created_at });
    });

    it('a system row as the oldest message still finishes (it was never indexed, not deleted)', async () => {
      const system = { ...row('sys', 2, 'msg sys'), type: 'expiration_event' };
      const seen: URL[] = [];
      serveChannel('ch', [row('a', 1), system], seen);
      await runChunk(deps(['ch']).d); // first chunk: reaches the 90-day floor at 'sys'
      expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'sys', done: false });
      await runChunk(deps(['ch']).d); // second chunk: nothing older
      expect(getScopeCoverage('ch')?.done).toBe(true);
      // One extra request: from the record 'a', the walk comes back to 'sys', so it exists.
      expect(seen.map(before)).toEqual(['null+since', 'sys+since', 'sys', 'a+since']);
    });

    describe('a cursor row this session never held (attachment-only)', () => {
      const unheld = (coverage: ScopeCoverage) => cover({ ...coverage, cursorIndexed: false });

      it('deleted between chunks, the walk goes on to older messages', async () => {
        const [a, b, att, d, e] = ['a', 'b', 'att', 'd', 'e'].map((id, i) => row(id, i + 1));
        rec(a);
        rec(b);
        unheld({ cursorId: 'att', coveredFrom: att.created_at, done: false });
        const seen: URL[] = [];
        serveChannel('ch', [a, b, d, e], seen); // 'att' was deleted
        const { d: run, hits } = deps(['ch']);
        await runChunk(run);
        expect(seen.map(before)).toEqual(['att+since', 'att', 'b+since']);
        expect(getScopeCoverage('ch')).toMatchObject({ cursorId: 'e', done: false });
        expect(hits.sort()).toEqual(['d', 'e']);
      });

      it('at the true start of history, finishes in the same chunk and is not walked again', async () => {
        const [a, b] = ['a', 'b'].map((id, i) => row(id, i + 1));
        const att = row('att', 3, ''); // decrypts to nothing: never held
        rec(a);
        rec(b);
        unheld({ cursorId: 'att', coveredFrom: att.created_at, done: false });
        const seen: URL[] = [];
        serveChannel('ch', [a, b, att], seen);
        await runChunk(deps(['ch']).d);
        expect(seen.map(before)).toEqual(['att+since', 'att', 'b+since']);
        expect(getScopeCoverage('ch')?.done).toBe(true);
        await runChunk(deps(['ch']).d);
        expect(seen).toHaveLength(3);
      });

      it('with no record held, the empty answer is the start of history', async () => {
        const att = row('att', 3, '');
        unheld({ cursorId: 'att', coveredFrom: att.created_at, done: false });
        const seen: URL[] = [];
        serveChannel('ch', [att], seen);
        await runChunk(deps(['ch']).d);
        expect(seen.map(before)).toEqual(['att+since', 'att']);
        expect(getScopeCoverage('ch')?.done).toBe(true);
      });
    });
  });

  describe('a scope the index cannot hold', () => {
    const big = 'x'.repeat(100_000); // 200 KB of UTF-16; the desktop cap holds 41
    const SMALL = { ...FAST, chunkMaxMessages: 20 };

    function fillScope(scope: string, count: number) {
      for (let i = 0; i < count; i++) {
        indexSearchRecord({
          id: `pre-${i}`,
          scope,
          createdAt: new Date(NOW).toISOString(),
          authorId: 'u-1',
          username: 'alice',
          content: big,
        });
      }
    }

    it('stops at the row it could not index and claims coverage only up to the last one handled', async () => {
      fillScope('ch', 39); // room for two more
      const seen: URL[] = [];
      const rows = ['r0', 'r1', 'r2', 'r3', 'r4'].map((id, i) => row(id, i + 1, big));
      serveChannel('ch', rows, seen);
      await expect(runChunk(deps(['ch'], { profile: SMALL }).d)).resolves.toEqual({
        kind: 'done',
      });
      expect(getScopeCoverage('ch')).toEqual({
        cursorId: 'r1',
        coveredFrom: rows[1].created_at,
        done: false,
        full: true,
        cursorIndexed: true,
      });
      // Everything the coverage claims is still held.
      for (const id of ['pre-0', 'pre-38', 'r0', 'r1']) expect(isIndexed(id)).toBe(true);
      // Nothing at or after the refused row was written.
      for (const id of ['r2', 'r3', 'r4']) expect(getSearchRecord(id)).toBeUndefined();
      // The full scope is not walked again in this chunk.
      expect(seen).toHaveLength(1);
    });

    it('a refusal on the first row of a page keeps the earlier coverage and marks it full', async () => {
      fillScope('ch', 41); // no room left
      const rows = [row('c0', 10), row('n1', 11, big), row('n2', 12, big)];
      const guard = beginSearchBackfill(['ch']);
      const earlier = { cursorId: 'c0', coveredFrom: rows[0].created_at, done: false };
      setScopeCoverage('ch', earlier, guard);
      guard.close();
      const seen: URL[] = [];
      serveChannel('ch', rows, seen);
      await runChunk(deps(['ch'], { profile: SMALL }).d);
      expect(seen[0].searchParams.get('before')).toBe('c0');
      expect(getScopeCoverage('ch')).toEqual({ ...earlier, full: true });
      expect(isIndexed('pre-0')).toBe(true);
      expect(getSearchRecord('n1')).toBeUndefined();
      expect(getSearchRecord('n2')).toBeUndefined();
    });

    it('an eviction by the chat path mid-page fences the walk, so it claims nothing', async () => {
      fillScope('ch', 40); // near the cap, so the chat path soon evicts this scope
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
      const started = deferred();
      const held = deferred<string>();
      vi.mocked(e2eeService.decryptForChannelWithVersion).mockImplementation(async (_s, c) => {
        if (c !== 'msg r3') return c;
        started.resolve();
        return held.promise;
      });
      const seen: URL[] = [];
      serveChannel(
        'ch',
        ['r0', 'r1', 'r2', 'r3', 'r4'].map((id, i) => row(id, i + 1)),
        seen
      );
      const run = runChunk(deps(['ch'], { profile: { ...FAST, pageSize: 2 } }).d);
      await started.promise; // page 1 (r0, r1) is indexed and covered; page 2 is decrypting
      expect(getScopeCoverage('ch')?.cursorId).toBe('r1');
      // Live traffic in another channel pushes the index to its cap: the LRU evicts 'ch'.
      for (let i = 0; isIndexed('r0') && i < 20_000; i++) indexMessage(`chat-${i}`, 'hi', 'other');
      expect(isIndexed('r0')).toBe(false);
      held.resolve('msg r3');
      await expect(run).resolves.toEqual({ kind: 'done' });
      // Page 1 is gone, so no coverage may claim it; the scope re-walks next chunk.
      expect(getScopeCoverage('ch')).toBeUndefined();
      expect(getSearchRecord('r2')).toBeUndefined();
      expect(getSearchRecord('r3')).toBeUndefined();
      expect(seen).toHaveLength(2);
    });

    it('a full scope is not walked by a later chunk', async () => {
      const guard = beginSearchBackfill(['ch']);
      setScopeCoverage(
        'ch',
        {
          cursorId: 'r1',
          coveredFrom: new Date(NOW - 2 * DAY).toISOString(),
          done: false,
          full: true,
        },
        guard
      );
      guard.close();
      const seen: URL[] = [];
      serveChannel('ch', [row('r2', 3)], seen);
      await expect(runChunk(deps(['ch']).d)).resolves.toEqual({ kind: 'done' });
      expect(seen).toHaveLength(0);
    });
  });

  describe('plaintext against its authority, raced', () => {
    const HELD = 'heldsecret plaintext';

    /** Holds the decrypt of HELD until released. */
    async function holdDecrypt() {
      const started = deferred();
      const plaintext = deferred<string>();
      const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
      vi.mocked(e2eeService.decryptForChannelWithVersion).mockImplementation(async (_s, c) => {
        if (c !== HELD) return c;
        started.resolve();
        return plaintext.promise;
      });
      return { started: started.promise, release: () => plaintext.resolve(HELD) };
    }

    /** One page holding the row; the first request waits for `hold`. */
    function servePage(hold?: Promise<void>, requested?: () => void) {
      server.use(
        http.get(`${API}/api/v1/channels/ch/messages/bulk`, async ({ request }) => {
          const before = new URL(request.url).searchParams.get('before');
          if (!before) {
            requested?.();
            if (hold) await hold;
          }
          return HttpResponse.json({
            messages: before
              ? []
              : [
                  {
                    ...row('held', 1, HELD),
                    type: 'user',
                    user_id: 'u-1',
                    username: 'alice',
                    key_version: 1,
                  },
                ],
          });
        })
      );
    }

    function start(signal?: AbortSignal) {
      const { d, hits } = deps(['ch'], { query: 'heldsecret', ...(signal ? { signal } : {}) });
      return { hits, done: runChunk(d).finally(() => d.guard.close()) };
    }

    function expectGone(hits: string[]) {
      expect(getSearchRecord('held')).toBeUndefined();
      expect(isIndexed('held')).toBe(false);
      expect(hits).not.toContain('held');
      expect(searchMessages('heldsecret', 'ch')).toEqual([]);
    }

    describe.each([
      ['a delete', () => removeMessage('held')],
      ['an account switch', () => clearIndex()],
      ['access loss', () => removeScope('ch')],
      ['the session ending', () => releaseSearchScopes(['ch'])],
    ])('%s', (_name, fire) => {
      it('during a held decrypt leaves nothing behind', async () => {
        servePage();
        const { started, release } = await holdDecrypt();
        const { hits, done } = start();
        await started;
        fire();
        release();
        await done;
        expectGone(hits);
      });

      it('during a held page fetch leaves nothing behind', async () => {
        const hold = deferred();
        const requested = deferred();
        servePage(hold.promise, () => requested.resolve());
        const { hits, done } = start();
        await requested.promise;
        fire();
        hold.resolve();
        await done;
        expectGone(hits);
      });
    });

    it('a live edit during a held decrypt keeps the edited text, not the backfill text', async () => {
      servePage();
      const { started, release } = await holdDecrypt();
      const { done } = start();
      await started;
      indexMessage('held', 'edited wording', 'ch');
      release();
      await done;
      expect(getSearchRecord('held')).toBeUndefined();
      expect(searchMessages('edited', 'ch')).toEqual(['held']);
      expect(searchMessages('heldsecret', 'ch')).toEqual([]);
    });

    it('an abort during a held decrypt writes nothing', async () => {
      servePage();
      const { started, release } = await holdDecrypt();
      const controller = new AbortController();
      const { hits, done } = start(controller.signal);
      await started;
      controller.abort();
      release();
      await done;
      expectGone(hits);
    });
  });

  it('waits while the window is hidden and resumes when it is shown', async () => {
    let hidden = true;
    vi.spyOn(document, 'hidden', 'get').mockImplementation(() => hidden);
    const listen = vi.spyOn(document, 'addEventListener');
    serveChannel('ch', [row('a', 1)], []);
    const { e2eeService } = await import('@/renderer/services/e2ee/e2eeService');
    const { d, hits } = deps(['ch']);
    const run = runChunk(d);
    await vi.waitFor(() =>
      expect(listen).toHaveBeenCalledWith('visibilitychange', expect.any(Function))
    );
    expect(e2eeService.decryptForChannelWithVersion).not.toHaveBeenCalled();
    hidden = false;
    document.dispatchEvent(new Event('visibilitychange'));
    await expect(run).resolves.toEqual({ kind: 'done' });
    expect(hits).toEqual(['a']);
  });
});
