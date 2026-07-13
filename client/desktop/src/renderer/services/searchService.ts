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
export const MAX_BACKFILL_TOMBSTONES = 4_096;

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
// Track the owning scope for replacement/removal by message ID.
const scopeById = new Map<string, string>();
// Deletes are retained only while a backfill generation can still publish
// stale plaintext. Tracking is capped; overflow invalidates that generation
// fail-closed rather than allowing this auxiliary set to grow without bound.
const deletedDuringBackfill = new Set<string>();
const activeBackfillsByGeneration = new Map<number, number>();
interface SearchBackfillState {
  generation: number;
  scopes: Set<string>;
  invalidatedScopes: Set<string>;
  closed: boolean;
}
const activeBackfillsByScope = new Map<string, Set<SearchBackfillState>>();
const searchResultInvalidationListeners = new Set<(messageId: string) => void>();
const searchScopeInvalidationListeners = new Set<(scope: string | null) => void>();
let backfillGeneration = 0;

export interface SearchBackfillGuard {
  isCurrent: (scope?: string) => boolean;
  close: () => void;
}

function invalidateActiveBackfills(): void {
  backfillGeneration += 1;
  deletedDuringBackfill.clear();
}

export function beginSearchBackfill(scopes: readonly string[]): SearchBackfillGuard {
  const generation = backfillGeneration;
  const state: SearchBackfillState = {
    generation,
    scopes: new Set(scopes),
    invalidatedScopes: new Set(),
    closed: false,
  };
  activeBackfillsByGeneration.set(
    generation,
    (activeBackfillsByGeneration.get(generation) ?? 0) + 1
  );
  for (const scope of state.scopes) {
    let activeBackfills = activeBackfillsByScope.get(scope);
    if (!activeBackfills) {
      activeBackfills = new Set();
      activeBackfillsByScope.set(scope, activeBackfills);
    }
    activeBackfills.add(state);
  }

  return {
    isCurrent: (scope?: string) =>
      !state.closed &&
      generation === backfillGeneration &&
      (scope === undefined || (state.scopes.has(scope) && !state.invalidatedScopes.has(scope))),
    close: () => {
      if (state.closed) return;
      state.closed = true;
      for (const scope of state.scopes) {
        const activeBackfills = activeBackfillsByScope.get(scope);
        activeBackfills?.delete(state);
        if (activeBackfills?.size === 0) activeBackfillsByScope.delete(scope);
      }
      const remaining = (activeBackfillsByGeneration.get(generation) ?? 1) - 1;
      if (remaining > 0) {
        activeBackfillsByGeneration.set(generation, remaining);
        return;
      }
      activeBackfillsByGeneration.delete(generation);
      if (generation === backfillGeneration) {
        deletedDuringBackfill.clear();
      }
    },
  };
}

export function canIndexBackfillMessage(
  guard: SearchBackfillGuard,
  id: string,
  scope: string
): boolean {
  return guard.isCurrent(scope) && !deletedDuringBackfill.has(id);
}

function invalidateBackfillsForScope(scope: string): void {
  const activeBackfills = activeBackfillsByScope.get(scope);
  if (!activeBackfills) return;
  for (const state of activeBackfills) state.invalidatedScopes.add(scope);
  activeBackfillsByScope.delete(scope);
}

export function subscribeSearchResultInvalidations(
  listener: (messageId: string) => void
): () => void {
  searchResultInvalidationListeners.add(listener);
  return () => searchResultInvalidationListeners.delete(listener);
}

export function subscribeSearchScopeInvalidations(
  listener: (scope: string | null) => void
): () => void {
  searchScopeInvalidationListeners.add(listener);
  return () => searchScopeInvalidationListeners.delete(listener);
}

function notifySearchResultInvalidated(id: string): void {
  for (const listener of searchResultInvalidationListeners) {
    listener(id);
  }
}

function notifySearchScopeInvalidated(scope: string | null): void {
  for (const listener of searchScopeInvalidationListeners) {
    listener(scope);
  }
}

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
  discardScope(oldestScope, false);
}

/**
 * Index a single message. Replaces an existing document with the same ID.
 * Triggers LRU eviction if dual cap is exceeded.
 */
export function indexMessage(id: string, content: string, scope: string): void {
  if (!content) {
    removeMessage(id);
    return;
  }

  if (indexedIds.has(id)) {
    const previousScope = scopeById.get(id);
    try {
      index.replace({ id, content, scope });
      if (previousScope !== scope) {
        removeIdFromScope(id, previousScope);
        addIdToScope(id, scope);
        scopeById.set(id, scope);
      }
      touchScope(scope);
      notifySearchResultInvalidated(id);
    } catch {
      // Keep the prior index/tracking intact if replacement fails.
    }
    return;
  }

  while (needsEviction()) {
    evictOldestScope();
  }

  try {
    index.add({ id, content, scope });
    indexedIds.add(id);
    scopeById.set(id, scope);
    addIdToScope(id, scope);
    touchScope(scope);
  } catch {
    // MiniSearch throws if doc with same ID exists (race condition safety)
  }
}

function addIdToScope(id: string, scope: string): void {
  let scopeIds = idsByScope.get(scope);
  if (!scopeIds) {
    scopeIds = new Set();
    idsByScope.set(scope, scopeIds);
  }
  scopeIds.add(id);
}

function removeIdFromScope(id: string, scope: string | undefined): void {
  if (scope === undefined) return;
  const scopeIds = idsByScope.get(scope);
  scopeIds?.delete(id);
  if (scopeIds?.size === 0) {
    idsByScope.delete(scope);
    const idx = scopeAccessOrder.indexOf(scope);
    if (idx !== -1) scopeAccessOrder.splice(idx, 1);
  }
}

function discardIndexedMessage(id: string): void {
  if (!indexedIds.has(id)) return;

  const scope = scopeById.get(id);
  try {
    index.discard(id);
  } catch {
    // The MiniSearch document may already be absent; tracking must still converge.
  }
  indexedIds.delete(id);
  scopeById.delete(id);
  removeIdFromScope(id, scope);
}

/** Remove a message and guard any active backfill from restoring it. */
export function removeMessage(id: string): void {
  const activeBackfills = activeBackfillsByGeneration.get(backfillGeneration) ?? 0;
  if (activeBackfills > 0 && !deletedDuringBackfill.has(id)) {
    if (deletedDuringBackfill.size >= MAX_BACKFILL_TOMBSTONES) {
      invalidateActiveBackfills();
    } else {
      deletedDuringBackfill.add(id);
    }
  }
  discardIndexedMessage(id);
  notifySearchResultInvalidated(id);
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
function discardScope(scope: string, notifyInvalidations: boolean): void {
  const ids = idsByScope.get(scope);
  if (!ids) return;

  for (const id of ids) {
    try {
      index.discard(id);
    } catch {
      // Already removed
    }
    indexedIds.delete(id);
    scopeById.delete(id);
    if (notifyInvalidations) notifySearchResultInvalidated(id);
  }
  idsByScope.delete(scope);

  const idx = scopeAccessOrder.indexOf(scope);
  if (idx !== -1) scopeAccessOrder.splice(idx, 1);
}

export function removeScope(scope: string): void {
  // Any in-flight backfill may already hold decrypted plaintext for this
  // scope. Fence only that scope before removing indexed rows so no stale
  // continuation can republish it without aborting unrelated backfills.
  invalidateBackfillsForScope(scope);
  discardScope(scope, true);
  // Scope notification is intentionally unconditional. LRU eviction removes
  // index tracking without dismissing open result objects; an access-loss
  // purge must still remove those copied plaintext rows later.
  notifySearchScopeInvalidated(scope);
}

/**
 * Clear the entire index. Used on logout or app reset.
 */
export function clearIndex(): void {
  for (const id of indexedIds) notifySearchResultInvalidated(id);
  notifySearchScopeInvalidated(null);
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
  scopeById.clear();
  invalidateActiveBackfills();
  activeBackfillsByGeneration.clear();
  activeBackfillsByScope.clear();
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
