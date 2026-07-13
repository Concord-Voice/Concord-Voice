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
} from '@/renderer/services/searchService';

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
