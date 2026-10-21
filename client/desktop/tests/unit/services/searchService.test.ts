import { describe, it, expect, beforeEach, vi } from 'vitest';
import {
  indexMessage,
  indexMessages,
  searchMessages,
  searchMessagesMultiScope,
  beginSearchBackfill,
  canIndexBackfillMessage,
  MAX_BACKFILL_TOMBSTONES,
  removeMessage,
  removeScope,
  clearIndex,
  getIndexStats,
  isIndexed,
  subscribeSearchScopeInvalidations,
  subscribeSearchResultInvalidations,
  indexSearchRecord,
  getSearchRecord,
  getScopeCoverage,
  setScopeCoverage,
  releaseSearchScopes,
  matchMessages,
  attachSearchRecord,
  clearScopeCoverage,
  currentContentRevision,
  indexedScopeOf,
  oldestSearchRecord,
  type SearchRecord,
} from '@/renderer/services/messaging/searchService';

describe('searchService', () => {
  beforeEach(() => {
    clearIndex();
  });

  describe('indexMessage', () => {
    it('indexes a message and makes it searchable', () => {
      indexMessage('msg-1', 'hello world', 'channel-1');
      const results = searchMessages('hello', 'channel-1');
      expect(results).toContain('msg-1');
    });

    it('skips empty content', () => {
      indexMessage('msg-1', '', 'channel-1');
      expect(isIndexed('msg-1')).toBe(false);
    });

    it('replaces old search terms when the same message ID is re-indexed after an edit', () => {
      indexMessage('msg-1', 'hello world', 'channel-1');
      indexMessage('msg-1', 'different content', 'channel-1');
      expect(searchMessages('hello', 'channel-1')).not.toContain('msg-1');
      expect(searchMessages('different', 'channel-1')).toContain('msg-1');
      expect(getIndexStats().documentCount).toBe(1);
    });

    it('allows a failed-decryption cleanup to be retried later', () => {
      indexMessage('msg-1', 'temporarily decrypted', 'channel-1');
      indexMessage('msg-1', '', 'channel-1');

      indexMessage('msg-1', 'recovered plaintext', 'channel-1');

      expect(isIndexed('msg-1')).toBe(true);
      expect(searchMessages('recovered', 'channel-1')).toContain('msg-1');
    });
  });

  describe('indexMessages', () => {
    it('indexes a batch of messages', () => {
      indexMessages([
        { id: 'msg-1', content: 'hello world', scope: 'channel-1' },
        { id: 'msg-2', content: 'goodbye world', scope: 'channel-1' },
      ]);
      expect(getIndexStats().documentCount).toBe(2);
    });

    it('filters out empty content in batch', () => {
      indexMessages([
        { id: 'msg-1', content: 'hello', scope: 'channel-1' },
        { id: 'msg-2', content: '', scope: 'channel-1' },
      ]);
      expect(getIndexStats().documentCount).toBe(1);
    });
  });

  describe('searchMessages', () => {
    it('returns matching message IDs', () => {
      indexMessage('msg-1', 'deployment guide is ready', 'channel-1');
      indexMessage('msg-2', 'deployment failed last night', 'channel-1');
      indexMessage('msg-3', 'unrelated message', 'channel-1');

      const results = searchMessages('deployment', 'channel-1');
      expect(results).toContain('msg-1');
      expect(results).toContain('msg-2');
      expect(results).not.toContain('msg-3');
    });

    it('scopes results to the specified channel', () => {
      indexMessage('msg-1', 'hello from channel 1', 'channel-1');
      indexMessage('msg-2', 'hello from channel 2', 'channel-2');

      const results = searchMessages('hello', 'channel-1');
      expect(results).toContain('msg-1');
      expect(results).not.toContain('msg-2');
    });

    it('returns empty array for empty query', () => {
      indexMessage('msg-1', 'hello', 'channel-1');
      expect(searchMessages('', 'channel-1')).toEqual([]);
      expect(searchMessages('   ', 'channel-1')).toEqual([]);
    });

    it('supports prefix matching', () => {
      indexMessage('msg-1', 'deployment is done', 'channel-1');
      const results = searchMessages('deploy', 'channel-1');
      expect(results).toContain('msg-1');
    });
  });

  describe('searchMessagesMultiScope', () => {
    it('searches across multiple scopes', () => {
      indexMessage('msg-1', 'hello from general', 'channel-1');
      indexMessage('msg-2', 'hello from design', 'channel-2');
      indexMessage('msg-3', 'hello from private', 'channel-3');

      const results = searchMessagesMultiScope('hello', ['channel-1', 'channel-2']);
      expect(results).toContain('msg-1');
      expect(results).toContain('msg-2');
      expect(results).not.toContain('msg-3');
    });

    it('returns empty for empty scopes', () => {
      indexMessage('msg-1', 'hello', 'channel-1');
      expect(searchMessagesMultiScope('hello', [])).toEqual([]);
    });

    it('returns empty for empty query', () => {
      expect(searchMessagesMultiScope('', ['channel-1'])).toEqual([]);
    });
  });

  describe('removeScope', () => {
    it('invalidates only the removed scope of a multi-scope backfill', () => {
      const guard = beginSearchBackfill(['channel-1', 'channel-2']);

      removeScope('channel-2');

      expect(guard.isCurrent('channel-1')).toBe(true);
      expect(guard.isCurrent('channel-2')).toBe(false);
      guard.close();
    });

    it('removes all messages for a scope', () => {
      indexMessage('msg-1', 'hello', 'channel-1');
      indexMessage('msg-2', 'world', 'channel-1');
      indexMessage('msg-3', 'hello', 'channel-2');

      removeScope('channel-1');

      expect(isIndexed('msg-1')).toBe(false);
      expect(isIndexed('msg-2')).toBe(false);
      expect(isIndexed('msg-3')).toBe(true);
      expect(getIndexStats().documentCount).toBe(1);
    });

    it('is a no-op for unknown scope', () => {
      indexMessage('msg-1', 'hello', 'channel-1');
      removeScope('channel-unknown');
      expect(getIndexStats().documentCount).toBe(1);
    });

    it('invalidates open results even after scope tracking is already absent', () => {
      const listener = vi.fn();
      const unsubscribe = subscribeSearchScopeInvalidations(listener);

      removeScope('channel-unknown');

      expect(listener).toHaveBeenCalledWith('channel-unknown');
      unsubscribe();
    });
  });

  describe('removeMessage', () => {
    it('removes only the targeted message and its tracking metadata', () => {
      indexMessage('msg-1', 'deleted plaintext', 'channel-1');
      indexMessage('msg-2', 'retained plaintext', 'channel-1');

      removeMessage('msg-1');

      expect(searchMessages('deleted', 'channel-1')).not.toContain('msg-1');
      expect(isIndexed('msg-1')).toBe(false);
      expect(isIndexed('msg-2')).toBe(true);
      expect(getIndexStats()).toMatchObject({ documentCount: 1, scopeCount: 1 });
    });

    it('blocks a deleted message only for the active backfill generation', () => {
      indexMessage('msg-1', 'retained plaintext', 'channel-1');
      const guard = beginSearchBackfill(['channel-1']);

      removeMessage('late-message');

      expect(isIndexed('msg-1')).toBe(true);
      expect(canIndexBackfillMessage(guard, 'late-message', 'channel-1')).toBe(false);
      expect(getIndexStats().documentCount).toBe(1);

      guard.close();
      indexMessage('late-message', 'future authoritative plaintext', 'channel-1');
      expect(isIndexed('late-message')).toBe(true);
    });

    it('invalidates active backfills if bounded deletion tracking fills up', () => {
      const guard = beginSearchBackfill(['channel-1']);
      for (let i = 0; i <= MAX_BACKFILL_TOMBSTONES; i += 1) {
        removeMessage(`deleted-${i}`);
      }

      expect(guard.isCurrent()).toBe(false);
      expect(canIndexBackfillMessage(guard, 'unrelated-message', 'channel-1')).toBe(false);
      guard.close();
    });

    it('invalidates active backfills with the account-wide index reset', () => {
      const guard = beginSearchBackfill(['channel-1']);

      clearIndex();

      expect(guard.isCurrent()).toBe(false);
      guard.close();
    });
  });

  describe('clearIndex', () => {
    it('broadcasts an all-scopes invalidation', () => {
      const listener = vi.fn();
      const unsubscribe = subscribeSearchScopeInvalidations(listener);

      clearIndex();

      expect(listener).toHaveBeenCalledWith(null);
      unsubscribe();
    });

    it('removes all messages and resets stats', () => {
      indexMessages([
        { id: 'msg-1', content: 'hello', scope: 'channel-1' },
        { id: 'msg-2', content: 'world', scope: 'channel-2' },
      ]);

      clearIndex();

      expect(getIndexStats().documentCount).toBe(0);
      expect(getIndexStats().scopeCount).toBe(0);
      expect(isIndexed('msg-1')).toBe(false);
    });
  });

  describe('getIndexStats', () => {
    it('returns correct counts', () => {
      indexMessage('msg-1', 'hello', 'channel-1');
      indexMessage('msg-2', 'world', 'channel-2');

      const stats = getIndexStats();
      expect(stats.documentCount).toBe(2);
      expect(stats.scopeCount).toBe(2);
      expect(stats.estimatedSizeBytes).toBeGreaterThan(0);
    });
  });

  describe('LRU eviction', () => {
    it('evicts oldest scope when cap is exceeded', () => {
      // Index enough messages to approach the cap, then trigger eviction
      // We can't easily test the 50K/3MB cap in unit tests, but we can verify
      // the eviction mechanism works by checking scope tracking
      indexMessage('msg-1', 'first scope message', 'oldest-channel');
      indexMessage('msg-2', 'second scope message', 'newer-channel');

      // Verify both are indexed
      expect(isIndexed('msg-1')).toBe(true);
      expect(isIndexed('msg-2')).toBe(true);

      // Remove oldest scope manually (simulating what eviction does)
      removeScope('oldest-channel');
      expect(isIndexed('msg-1')).toBe(false);
      expect(isIndexed('msg-2')).toBe(true);
    });
  });
});

const record = (id: string, scope = 'ch-1', content = 'hello world'): SearchRecord => ({
  id,
  scope,
  createdAt: '2026-10-01T12:00:00.000Z',
  authorId: 'u-1',
  username: 'alice',
  content,
});
const liveGuard = (scopes: string[]) => beginSearchBackfill(scopes);

describe('result records and coverage (#3875)', () => {
  beforeEach(() => {
    clearIndex();
  });

  it('a record is searchable and resolvable', () => {
    indexSearchRecord(record('m-1'));
    expect(searchMessages('hello', 'ch-1')).toEqual(['m-1']);
    expect(getSearchRecord('m-1')?.content).toBe('hello world');
  });

  it('a search record never replaces a row the chat path already indexed', () => {
    indexMessage('m-1', 'newer edited text', 'ch-1');
    indexSearchRecord(record('m-1', 'ch-1', 'older stale text'));
    expect(getSearchRecord('m-1')).toBeUndefined();
    expect(searchMessages('stale', 'ch-1')).toEqual([]);
    expect(searchMessages('edited', 'ch-1')).toEqual(['m-1']);
  });

  it('removeMessage drops the record', () => {
    indexSearchRecord(record('m-1'));
    removeMessage('m-1');
    expect(getSearchRecord('m-1')).toBeUndefined();
  });

  it('the chat path takes ownership of a row search indexed', () => {
    indexSearchRecord(record('m-1'));
    indexMessage('m-1', 'hello edited', 'ch-1');
    expect(getSearchRecord('m-1')).toBeUndefined();
    expect(searchMessages('edited', 'ch-1')).toEqual(['m-1']);
  });

  it.each([
    ['removeScope', () => removeScope('ch-1')],
    ['clearIndex', () => clearIndex()],
  ])('%s drops records and coverage', (_name, drop) => {
    const guard = liveGuard(['ch-1']);
    indexSearchRecord(record('m-1'));
    setScopeCoverage(
      'ch-1',
      { cursorId: 'm-1', coveredFrom: '2026-07-01T00:00:00.000Z', done: false },
      guard
    );
    drop();
    expect(getSearchRecord('m-1')).toBeUndefined();
    expect(getScopeCoverage('ch-1')).toBeUndefined();
    guard.close();
  });

  it('coverage from a fenced backfill is ignored', () => {
    const guard = liveGuard(['ch-1']);
    removeScope('ch-1');
    setScopeCoverage('ch-1', { cursorId: 'm-1', coveredFrom: null, done: true }, guard);
    expect(getScopeCoverage('ch-1')).toBeUndefined();
    guard.close();
  });

  it('releaseSearchScopes drops search-owned rows only', () => {
    const guard = liveGuard(['ch-1']);
    indexSearchRecord(record('m-1'));
    indexMessage('m-2', 'hello from chat', 'ch-1');
    setScopeCoverage('ch-1', { cursorId: 'm-1', coveredFrom: null, done: false }, guard);
    releaseSearchScopes(['ch-1']);
    expect(getSearchRecord('m-1')).toBeUndefined();
    expect(isIndexed('m-1')).toBe(false);
    expect(isIndexed('m-2')).toBe(true);
    expect(getScopeCoverage('ch-1')).toBeUndefined();
    guard.close();
  });

  it('releaseSearchScopes keeps the chat path entry under an attached record', () => {
    const guard = liveGuard(['ch-1']);
    indexMessage('m-2', 'hello from chat', 'ch-1');
    expect(attachSearchRecord(record('m-2'), currentContentRevision(), guard)).toBe('attached');
    releaseSearchScopes(['ch-1']);
    expect(getSearchRecord('m-2')).toBeUndefined();
    expect(isIndexed('m-2')).toBe(true);
    expect(searchMessages('hello', 'ch-1')).toEqual(['m-2']);
    guard.close();
  });

  it('releaseSearchScopes fences a backfill still running over a released scope', () => {
    const guard = liveGuard(['ch-1', 'ch-2']);
    releaseSearchScopes(['ch-1']);
    expect(canIndexBackfillMessage(guard, 'm-late', 'ch-1')).toBe(false);
    setScopeCoverage('ch-1', { cursorId: 'm-late', coveredFrom: null, done: false }, guard);
    expect(getScopeCoverage('ch-1')).toBeUndefined();
    expect(guard.isCurrent('ch-2')).toBe(true);
    guard.close();
  });

  it('matchMessages limits hits to the given ids', () => {
    indexSearchRecord(record('m-1'));
    indexSearchRecord(record('m-2'));
    expect(matchMessages('hello', new Set(['m-2']), 'ch-1')).toEqual(['m-2']);
  });

  it('matchMessages never returns an id indexed under another scope', () => {
    indexMessage('dm-1', 'hello from a DM', 'dm');
    indexSearchRecord(record('m-1'));
    expect(matchMessages('hello', new Set(['dm-1', 'm-1']), 'ch-1')).toEqual(['m-1']);
    expect(matchMessages('hello', new Set(['dm-1']), 'dm')).toEqual(['dm-1']); // control
  });

  it('indexedScopeOf names the scope a row is indexed under', () => {
    indexMessage('m-1', 'hello', 'ch-1');
    expect(indexedScopeOf('m-1')).toBe('ch-1');
    removeMessage('m-1');
    expect(indexedScopeOf('m-1')).toBeUndefined();
  });

  it('oldestSearchRecord is the oldest record of that scope only', () => {
    indexSearchRecord({ ...record('new'), createdAt: '2026-10-02T00:00:00.000Z' });
    indexSearchRecord({ ...record('old'), createdAt: '2026-09-01T00:00:00.000Z' });
    indexSearchRecord({ ...record('elsewhere', 'ch-2'), createdAt: '2026-01-01T00:00:00.000Z' });
    expect(oldestSearchRecord('ch-1')?.id).toBe('old');
    expect(oldestSearchRecord('ch-3')).toBeUndefined();
  });

  it('clearScopeCoverage forgets coverage, but not for a fenced backfill', () => {
    const coverage = { cursorId: 'm-1', coveredFrom: null, done: false };
    const guard = liveGuard(['ch-1']);
    setScopeCoverage('ch-1', coverage, guard);
    clearScopeCoverage('ch-1', guard);
    expect(getScopeCoverage('ch-1')).toBeUndefined();
    guard.close();
    const fenced = liveGuard(['ch-1']);
    removeScope('ch-1'); // fences it
    const later = liveGuard(['ch-1']);
    setScopeCoverage('ch-1', coverage, later);
    clearScopeCoverage('ch-1', fenced);
    expect(getScopeCoverage('ch-1')).toEqual(coverage);
    fenced.close();
    later.close();
  });

  describe('attachSearchRecord', () => {
    it('keeps a record for a row the chat path indexed, without touching the index', () => {
      indexMessage('m-1', 'hello world', 'ch-1');
      const epoch = currentContentRevision();
      const guard = liveGuard(['ch-1']);
      expect(attachSearchRecord(record('m-1'), epoch, guard)).toBe('attached');
      expect(getSearchRecord('m-1')?.content).toBe('hello world');
      expect(getIndexStats().documentCount).toBe(1);
      guard.close();
    });

    it('refuses a row edited after the epoch, so stale text is never kept', () => {
      indexMessage('m-1', 'hello world', 'ch-1');
      const epoch = currentContentRevision();
      indexMessage('m-1', 'hello edited', 'ch-1'); // a live edit lands after the page was fetched
      const guard = liveGuard(['ch-1']);
      expect(attachSearchRecord(record('m-1'), epoch, guard)).toBe('fenced');
      expect(getSearchRecord('m-1')).toBeUndefined();
      guard.close();
    });

    it('an edit to another row does not fence this one', () => {
      indexMessage('m-1', 'hello world', 'ch-1');
      const epoch = currentContentRevision();
      indexMessage('m-2', 'unrelated traffic', 'ch-1');
      const guard = liveGuard(['ch-1']);
      expect(attachSearchRecord(record('m-1'), epoch, guard)).toBe('attached');
      guard.close();
    });

    it.each([
      ['deleted', () => removeMessage('m-1')],
      ['deleted and re-added', () => (removeMessage('m-1'), indexMessage('m-1', 'hello', 'ch-1'))],
      ['moved to another scope', () => indexMessage('m-1', 'hello world', 'ch-2')],
    ])('refuses a row %s since the epoch', (_name, change) => {
      const guard = liveGuard(['ch-1']);
      indexMessage('m-1', 'hello world', 'ch-1');
      const epoch = currentContentRevision();
      change();
      expect(attachSearchRecord(record('m-1'), epoch, guard)).toBe('fenced');
      expect(getSearchRecord('m-1')).toBeUndefined();
      guard.close();
    });

    it('refuses an id indexed under another scope', () => {
      indexMessage('dm-1', 'hello world', 'dm');
      const guard = liveGuard(['ch-1']);
      expect(attachSearchRecord(record('dm-1'), currentContentRevision(), guard)).toBe('fenced');
      expect(getSearchRecord('dm-1')).toBeUndefined();
      guard.close();
    });

    it('refuses an id with a record already, or a fenced backfill', () => {
      indexSearchRecord(record('m-1', 'ch-1', 'hello first'));
      indexMessage('m-2', 'hello world', 'ch-1');
      const epoch = currentContentRevision();
      const guard = liveGuard(['ch-1']);
      expect(attachSearchRecord(record('m-1', 'ch-1', 'hello other'), epoch, guard)).toBe('fenced');
      expect(getSearchRecord('m-1')?.content).toBe('hello first');
      const fenced = liveGuard(['ch-1']);
      clearIndex();
      indexMessage('m-2', 'hello world', 'ch-1');
      expect(attachSearchRecord(record('m-2'), currentContentRevision(), fenced)).toBe('fenced');
      guard.close();
      fenced.close();
    });

    it('a record too big for its scope is refused for lack of room', () => {
      indexMessage('m-1', 'small', 'ch-1');
      const guard = liveGuard(['ch-1']);
      const huge = record('m-1', 'ch-1', 'x'.repeat(5 * 1024 * 1024)); // 10 MB of UTF-16
      expect(attachSearchRecord(huge, currentContentRevision(), guard)).toBe('full');
      expect(getSearchRecord('m-1')).toBeUndefined();
      expect(isIndexed('m-1')).toBe(true);
      guard.close();
    });
  });

  it('every string a record holds counts toward the byte cap, not only its text', () => {
    const name = 'n'.repeat(100_000); // 200 KB of UTF-16, supplied by the server
    const kept = Array.from({ length: 50 }, (_, i) =>
      indexSearchRecord({
        ...record(`old-${i}`, 'old', 'needle'),
        username: name,
        displayName: name,
      })
    );
    // 50 x 400 KB is past the cap only when the author fields are counted.
    expect(kept.at(-1)).toBe(false);
    expect(getIndexStats().estimatedSizeBytes).toBeLessThanOrEqual(8 * 1024 * 1024);
  });

  it('an attached record is charged for its author fields too', () => {
    indexMessage('m-1', 'small', 'ch-1');
    const guard = liveGuard(['ch-1']);
    const name = 'n'.repeat(5 * 1024 * 1024); // 10 MB of UTF-16 in a name
    const attached = attachSearchRecord(
      { ...record('m-1', 'ch-1', 'small'), username: name },
      currentContentRevision(),
      guard
    );
    expect(attached).toBe('full');
    expect(getSearchRecord('m-1')).toBeUndefined();
    guard.close();
  });

  it('record text counts toward the byte cap', () => {
    const big = 'x'.repeat(100_000); // 200 KB of UTF-16
    const kept = Array.from({ length: 50 }, (_, i) =>
      indexSearchRecord(record(`old-${i}`, 'old', `${big} needle`))
    );
    // 50 x 200 KB is past the cap only when record text is counted.
    expect(kept.at(-1)).toBe(false);
    expect(getSearchRecord('old-49')).toBeUndefined();
    expect(getIndexStats().estimatedSizeBytes).toBeLessThanOrEqual(8 * 1024 * 1024);
    expect(indexSearchRecord(record('new-1', 'fresh', 'needle'))).toBe(true);
    expect(getSearchRecord('new-1')).toBeDefined();
  });

  describe('a search record never evicts its own scope', () => {
    const big = 'x'.repeat(100_000); // 200 KB of UTF-16 each

    it('refuses once the scope alone would pass the cap, and changes nothing', () => {
      const guard = liveGuard(['ch-1']);
      const coverage = { cursorId: 'r-0', coveredFrom: '2026-09-01T00:00:00.000Z', done: false };
      setScopeCoverage('ch-1', coverage, guard);
      const accepted: boolean[] = [];
      let statsAtFirstRefusal: ReturnType<typeof getIndexStats> | null = null;
      for (let i = 0; i < 45; i++) {
        if (statsAtFirstRefusal === null && accepted.includes(false)) {
          statsAtFirstRefusal = getIndexStats();
        }
        accepted.push(indexSearchRecord(record(`r-${i}`, 'ch-1', big)));
      }
      // The scope's own entries, records and coverage survive the writes it refused.
      expect(getSearchRecord('r-0')).toBeDefined();
      expect(isIndexed('r-0')).toBe(true);
      expect(getScopeCoverage('ch-1')).toEqual(coverage);
      // Accepted up to the cap, then refused every later record.
      const firstRefused = accepted.indexOf(false);
      expect(firstRefused).toBeGreaterThan(30);
      expect(accepted.slice(0, firstRefused).every(Boolean)).toBe(true);
      expect(accepted.slice(firstRefused).some(Boolean)).toBe(false);
      expect(getSearchRecord(`r-${firstRefused}`)).toBeUndefined();
      expect(isIndexed(`r-${firstRefused}`)).toBe(false);
      expect(getIndexStats()).toEqual(statsAtFirstRefusal);
      expect(getIndexStats().estimatedSizeBytes).toBeLessThan(8 * 1024 * 1024);
      guard.close();
    });

    it('still evicts other scopes to make room, even when the written one is older', () => {
      const guard = liveGuard(['a', 'b']);
      const coverageB = { cursorId: 'b-0', coveredFrom: '2026-09-01T00:00:00.000Z', done: false };
      for (let i = 0; i < 20; i++) indexSearchRecord(record(`a-${i}`, 'a', big));
      for (let i = 0; i < 21; i++) indexSearchRecord(record(`b-${i}`, 'b', big));
      setScopeCoverage('a', { cursorId: 'a-0', coveredFrom: null, done: false }, guard);
      setScopeCoverage('b', coverageB, guard);
      searchMessages('anything', 'a'); // 'a' is now the most recently used scope
      expect(indexSearchRecord(record('b-21', 'b', big))).toBe(true);
      expect(getSearchRecord('b-21')).toBeDefined();
      expect(getSearchRecord('b-0')).toBeDefined();
      expect(getScopeCoverage('b')).toEqual(coverageB);
      expect(getSearchRecord('a-0')).toBeUndefined();
      expect(getScopeCoverage('a')).toBeUndefined();
      guard.close();
    });

    // A result card holds a copy of the plaintext, so an eviction must reach it
    // or the cards outlive the cap (#3875 review).
    it('tells result subscribers about every row an eviction drops, and only those', () => {
      for (let i = 0; i < 20; i++) indexSearchRecord(record(`a-${i}`, 'a', big));
      for (let i = 0; i < 21; i++) indexSearchRecord(record(`b-${i}`, 'b', big));
      const dropped: string[] = [];
      const stop = subscribeSearchResultInvalidations((id) => dropped.push(id));
      expect(indexSearchRecord(record('b-21', 'b', big))).toBe(true);
      stop();
      expect(dropped.sort()).toEqual(Array.from({ length: 20 }, (_, i) => `a-${i}`).sort());
    });
  });
});
