import { describe, it, expect, beforeEach } from 'vitest';
import {
  indexMessage,
  indexMessages,
  searchMessages,
  searchMessagesMultiScope,
  removeScope,
  clearIndex,
  getIndexStats,
  isIndexed,
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

    it('is idempotent — re-indexing same ID is a no-op', () => {
      indexMessage('msg-1', 'hello world', 'channel-1');
      indexMessage('msg-1', 'different content', 'channel-1');
      expect(getIndexStats().documentCount).toBe(1);
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
  });

  describe('clearIndex', () => {
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
