/**
 * Search Service — E2EE-native in-memory search index.
 *
 * Uses MiniSearch to index decrypted message content client-side.
 * No plaintext ever leaves the device. Zero disk persistence.
 *
 * Index stores only: { id: messageUUID, content: tokenized terms, scope: channelId }
 * No usernames, timestamps, or other PII in the index itself.
 *
 * Dual cap: 50K messages OR 3MB estimated size, whichever comes first.
 * LRU eviction by scope (channel) when cap is exceeded.
 */

import MiniSearch from 'minisearch';

const MAX_INDEXED_MESSAGES = 50_000;
const MAX_INDEX_SIZE_BYTES = 3 * 1024 * 1024; // 3MB
const ESTIMATED_BYTES_PER_DOC = 60; // avg: ~60 bytes per indexed doc (terms + ID + overhead)

interface IndexedMessage {
  id: string;
  content: string;
  scope: string;
}

let index = new MiniSearch<IndexedMessage>({
  fields: ['content'],
  storeFields: ['scope'],
  searchOptions: {
    prefix: true,
    fuzzy: 0.2,
  },
});

// Track scope access order for LRU eviction
const scopeAccessOrder: string[] = [];
// Track which message IDs are indexed (for dedup)
const indexedIds = new Set<string>();
// Track IDs per scope (for targeted eviction)
const idsByScope = new Map<string, Set<string>>();

function touchScope(scope: string) {
  const idx = scopeAccessOrder.indexOf(scope);
  if (idx !== -1) scopeAccessOrder.splice(idx, 1);
  scopeAccessOrder.push(scope);
}

function estimatedSizeBytes(): number {
  return indexedIds.size * ESTIMATED_BYTES_PER_DOC;
}

function needsEviction(): boolean {
  return indexedIds.size >= MAX_INDEXED_MESSAGES || estimatedSizeBytes() >= MAX_INDEX_SIZE_BYTES;
}

function evictOldestScope(): void {
  const oldestScope = scopeAccessOrder.shift();
  if (oldestScope === undefined) return;
  removeScope(oldestScope);
}

/**
 * Index a single message. Idempotent — skips if already indexed.
 * Triggers LRU eviction if dual cap is exceeded.
 */
export function indexMessage(id: string, content: string, scope: string): void {
  if (!content || indexedIds.has(id)) return;

  while (needsEviction()) {
    evictOldestScope();
  }

  try {
    index.add({ id, content, scope });
    indexedIds.add(id);

    let scopeIds = idsByScope.get(scope);
    if (!scopeIds) {
      scopeIds = new Set();
      idsByScope.set(scope, scopeIds);
    }
    scopeIds.add(id);

    touchScope(scope);
  } catch {
    // MiniSearch throws if doc with same ID exists (race condition safety)
  }
}

/**
 * Index a batch of messages. Filters out already-indexed and empty content.
 */
export function indexMessages(
  messages: Array<{ id: string; content: string; scope: string }>
): void {
  for (const msg of messages) {
    indexMessage(msg.id, msg.content, msg.scope);
  }
}

/**
 * Search messages within a single scope (channel or conversation).
 * Returns an array of message IDs ordered by relevance.
 */
export function searchMessages(query: string, scope: string): string[] {
  if (!query.trim()) return [];
  touchScope(scope);

  const results = index.search(query, {
    filter: (result) => result.scope === scope,
  });

  return results.map((r) => r.id);
}

/**
 * Search messages across multiple scopes (server-wide search).
 * Returns an array of message IDs ordered by relevance.
 */
export function searchMessagesMultiScope(query: string, scopes: string[]): string[] {
  if (!query.trim() || scopes.length === 0) return [];

  const scopeSet = new Set(scopes);
  const results = index.search(query, {
    filter: (result) => scopeSet.has(result.scope as string),
  });

  return results.map((r) => r.id);
}

/**
 * Remove all indexed messages for a scope (e.g., user kicked from channel).
 */
export function removeScope(scope: string): void {
  const ids = idsByScope.get(scope);
  if (!ids) return;

  for (const id of ids) {
    try {
      index.discard(id);
    } catch {
      // Already removed
    }
    indexedIds.delete(id);
  }
  idsByScope.delete(scope);

  const idx = scopeAccessOrder.indexOf(scope);
  if (idx !== -1) scopeAccessOrder.splice(idx, 1);
}

/**
 * Clear the entire index. Used on logout or app reset.
 */
export function clearIndex(): void {
  index = new MiniSearch<IndexedMessage>({
    fields: ['content'],
    storeFields: ['scope'],
    searchOptions: {
      prefix: true,
      fuzzy: 0.2,
    },
  });
  indexedIds.clear();
  idsByScope.clear();
  scopeAccessOrder.length = 0;
}

/**
 * Get index statistics for monitoring/debugging.
 */
export function getIndexStats(): {
  documentCount: number;
  estimatedSizeBytes: number;
  scopeCount: number;
} {
  return {
    documentCount: indexedIds.size,
    estimatedSizeBytes: estimatedSizeBytes(),
    scopeCount: idsByScope.size,
  };
}

/**
 * Check if a message is already indexed.
 */
export function isIndexed(id: string): boolean {
  return indexedIds.has(id);
}
