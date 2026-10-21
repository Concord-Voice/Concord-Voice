/**
 * Search Service — E2EE-native in-memory search index.
 *
 * Uses MiniSearch to index decrypted message content client-side.
 * No plaintext ever leaves the device. Zero disk persistence.
 *
 * Index stores only: { id: messageUUID, content: tokenized terms, scope: channelId }
 * No usernames, timestamps, or other PII in the index itself.
 *
 * Dual cap from the active search profile: a message count OR an estimated
 * size (index overhead plus stored result text), whichever comes first. LRU
 * eviction by scope (channel) when the cap is exceeded.
 *
 * Beside the index it keeps session-only result records and per-scope search
 * coverage (#3875). Both live in module memory and are never persisted; every
 * path that drops a message or a scope drops them too.
 */

import MiniSearch from 'minisearch';
import { ACTIVE_SEARCH_PROFILE } from './searchProfile';

const ESTIMATED_BYTES_PER_DOC = 60; // index terms + id + overhead per doc
const UTF16_BYTES = 2;
export const MAX_BACKFILL_TOMBSTONES = 4_096;

export interface SearchRecord {
  id: string;
  scope: string;
  createdAt: string;
  authorId: string;
  username: string;
  displayName?: string;
  content: string;
}

export interface ScopeCoverage {
  /** Oldest message fetched; the next request's `before`. */
  cursorId: string | null;
  /** Searched back to this time in this scope; null before the first page. */
  coveredFrom: string | null;
  /** The scope reached the start of its history. */
  done: boolean;
  /** The index cannot hold more of this scope; the walk stopped at `cursorId`. */
  full?: boolean;
  /**
   * The cursor row was searchable when the cursor was set. An empty answer for
   * a cursor that has since gone is the server not finding it, not the start of
   * history; a cursor never indexed (a system row) cannot go that way.
   */
  cursorIndexed?: boolean;
  /**
   * The cursor that was lost when this one was moved back to a held row. A
   * walk that comes back to it shows that row still exists, so the empty
   * answer for it was the start of history.
   */
  reanchoredFrom?: string;
}

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

// Session-only plaintext, beside the index. Module memory, never persisted (#3875).
// Invariant: every record's id is indexed, with the record's own content.
const records = new Map<string, SearchRecord>();
/**
 * Records whose index entry search wrote. An attached record sits on an entry
 * the chat path wrote, and releasing it must leave that entry in the index.
 */
const searchIndexedIds = new Set<string>();
let recordBytes = 0;
const recordBytesByScope = new Map<string, number>();
const coverageByScope = new Map<string, ScopeCoverage>();

// Content revisions (#3875). Every chat-path add or replace stamps the row with
// the next value; a removal drops the stamp. A search decrypt captures the
// counter before its page is fetched and attaches a record only to a row whose
// stamp is no newer, so an edit or delete it raced never puts stale text on
// screen. Never reset, so an epoch from before a reset cannot match a new stamp.
let contentRevision = 0;
const revisionById = new Map<string, number>();

/** The content-revision epoch; read it before fetching a page. */
export function currentContentRevision(): number {
  return contentRevision;
}

function addRecordBytes(scope: string, bytes: number): void {
  recordBytes += bytes;
  const scopeBytes = (recordBytesByScope.get(scope) ?? 0) + bytes;
  if (scopeBytes === 0) recordBytesByScope.delete(scope);
  else recordBytesByScope.set(scope, scopeBytes);
}

/**
 * What a record pins in memory: every string it holds, not only its text. The
 * server supplies the id, author and time fields, so counting only `content`
 * would let a page pin memory the cap never sees.
 */
function recordBytesOf(record: SearchRecord): number {
  const chars =
    record.id.length +
    record.scope.length +
    record.createdAt.length +
    record.authorId.length +
    record.username.length +
    (record.displayName?.length ?? 0) +
    record.content.length;
  return chars * UTF16_BYTES;
}

function dropRecord(id: string): void {
  const existing = records.get(id);
  if (!existing) return;
  addRecordBytes(existing.scope, -recordBytesOf(existing));
  records.delete(id);
  searchIndexedIds.delete(id);
}

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
  return indexedIds.size * ESTIMATED_BYTES_PER_DOC + recordBytes;
}

/** Whether an index of `docs` documents and `bytes` estimated bytes is at or over a cap. */
function atCap(docs: number, bytes: number): boolean {
  return (
    docs >= ACTIVE_SEARCH_PROFILE.indexMaxMessages || bytes >= ACTIVE_SEARCH_PROFILE.indexMaxBytes
  );
}

function needsEviction(): boolean {
  return atCap(indexedIds.size, estimatedSizeBytes());
}

function evictOldestScope(): void {
  const oldestScope = scopeAccessOrder.shift();
  if (oldestScope === undefined) return;
  // Whatever triggered it (the chat path, or a search write to another scope),
  // a walk over this scope must not record coverage for entries now gone.
  invalidateBackfillsForScope(oldestScope);
  // An open result copied a dropped record's plaintext; it must go with it, or
  // the cards outlive the cap.
  discardScope(oldestScope);
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
  dropRecord(id); // the chat path now owns this row

  if (indexedIds.has(id)) {
    const previousScope = scopeById.get(id);
    try {
      index.replace({ id, content, scope });
      revisionById.set(id, ++contentRevision);
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
    revisionById.set(id, ++contentRevision);
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
  revisionById.delete(id);
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
  dropRecord(id);
  discardIndexedMessage(id); // drops its revision stamp, so no attach can pass
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
function discardScope(scope: string): void {
  coverageByScope.delete(scope);
  const ids = idsByScope.get(scope);
  if (!ids) return;

  for (const id of ids) {
    dropRecord(id);
    try {
      index.discard(id);
    } catch {
      // Already removed
    }
    indexedIds.delete(id);
    scopeById.delete(id);
    revisionById.delete(id);
    notifySearchResultInvalidated(id);
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
  discardScope(scope);
  // Scope notification is intentionally unconditional: rows LRU eviction
  // already dropped left no ids to notify, and an access-loss purge must still
  // remove every open result for this scope.
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
  revisionById.clear();
  records.clear();
  searchIndexedIds.clear();
  recordBytes = 0;
  recordBytesByScope.clear();
  coverageByScope.clear();
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

/** The scope a message is indexed under, if it is indexed. */
export function indexedScopeOf(id: string): string | undefined {
  return scopeById.get(id);
}

/**
 * Make room for `docs` more documents and `bytes` more estimated bytes in
 * `scope`, evicting other scopes but never this one. False when it cannot fit.
 */
function makeRoom(scope: string, docs: number, bytes: number): boolean {
  const scopeDocs = idsByScope.get(scope)?.size ?? 0;
  const scopeBytes = scopeDocs * ESTIMATED_BYTES_PER_DOC + (recordBytesByScope.get(scope) ?? 0);
  if (atCap(scopeDocs + docs, scopeBytes + bytes)) return false;
  touchScope(scope);
  while (atCap(indexedIds.size + docs, estimatedSizeBytes() + bytes)) {
    // Only this scope is left. The check above makes that unreachable while
    // every scope holding entries is in the LRU order; refuse rather than evict it.
    if (scopeAccessOrder[0] === scope) return false;
    evictOldestScope();
  }
  return true;
}

/**
 * Index a decrypted search row and keep it for display (#3875).
 *
 * Never replaces a row that is already indexed: a chat-path write (a live
 * edit, or a row the chat view decrypted) is authoritative, and a backfill
 * decrypt that resolves after it must not restore older plaintext.
 *
 * Never evicts its own scope. Evicting it would drop the entries and coverage
 * of the walk that is writing it, and that walk would then record coverage for
 * a range it no longer holds. So the scope becomes the most recent before any
 * other is evicted, and a record that would not fit even with every other
 * scope gone is refused: nothing changes and the call returns false.
 *
 * @returns false only when the record is refused for lack of room.
 */
export function indexSearchRecord(record: SearchRecord): boolean {
  if (indexedIds.has(record.id) || !record.content) return true;
  const bytes = recordBytesOf(record);
  if (!makeRoom(record.scope, 1, ESTIMATED_BYTES_PER_DOC + bytes)) return false;
  // Fits now, so indexMessage's own eviction pass has nothing to do.
  indexMessage(record.id, record.content, record.scope);
  if (!indexedIds.has(record.id)) return true;
  records.set(record.id, record);
  searchIndexedIds.add(record.id);
  addRecordBytes(record.scope, bytes);
  return true;
}

export type AttachOutcome = 'attached' | 'fenced' | 'full';

/**
 * Keep a display record for a row the chat path indexed but no longer shows
 * (#3875). Never touches the index. Refused ('fenced') unless the row is still
 * indexed under the record's scope with no record, the backfill may still write
 * it, and its revision is no newer than `sinceRevision`: an edit or delete
 * after the page was fetched would otherwise show text the user no longer has.
 */
export function attachSearchRecord(
  record: SearchRecord,
  sinceRevision: number,
  guard: SearchBackfillGuard
): AttachOutcome {
  const revision = revisionById.get(record.id);
  if (
    !record.content ||
    !canIndexBackfillMessage(guard, record.id, record.scope) ||
    scopeById.get(record.id) !== record.scope ||
    records.has(record.id) ||
    revision === undefined ||
    revision > sinceRevision
  ) {
    return 'fenced';
  }
  const bytes = recordBytesOf(record);
  if (!makeRoom(record.scope, 0, bytes)) return 'full';
  records.set(record.id, record);
  addRecordBytes(record.scope, bytes);
  return 'attached';
}

export function getSearchRecord(id: string): SearchRecord | undefined {
  return records.get(id);
}

/** The oldest record this session holds for `scope`. */
export function oldestSearchRecord(scope: string): SearchRecord | undefined {
  let oldest: SearchRecord | undefined;
  for (const r of records.values()) {
    if (r.scope !== scope) continue;
    if (oldest === undefined || Date.parse(r.createdAt) < Date.parse(oldest.createdAt)) oldest = r;
  }
  return oldest;
}

export function getScopeCoverage(scope: string): ScopeCoverage | undefined {
  return coverageByScope.get(scope);
}

/** Forget how far a scope was searched, so it is walked again from the newest message. */
export function clearScopeCoverage(scope: string, guard: SearchBackfillGuard): void {
  if (guard.isCurrent(scope)) coverageByScope.delete(scope);
}

/** Record how far a scope has been searched; ignored once the backfill is fenced. */
export function setScopeCoverage(
  scope: string,
  coverage: ScopeCoverage,
  guard: SearchBackfillGuard
): void {
  if (!guard.isCurrent(scope)) return;
  coverageByScope.set(scope, coverage);
}

/**
 * End of a search session: forget what only search knew about these scopes.
 * A backfill still running over a released scope is fenced, so a decrypt that
 * resolves afterwards cannot write a record or coverage back.
 */
export function releaseSearchScopes(scopes: Iterable<string>): void {
  const released = new Set(scopes);
  for (const scope of released) {
    invalidateBackfillsForScope(scope);
    coverageByScope.delete(scope);
  }
  // Deleting the current entry while iterating a Map is defined behaviour.
  for (const [id, rec] of records) {
    if (!released.has(rec.scope)) continue;
    // Only the entries search wrote: an attached record's entry is the chat path's (spec §3.6).
    const searchWroteEntry = searchIndexedIds.has(id);
    dropRecord(id);
    if (searchWroteEntry) discardIndexedMessage(id);
  }
}

/**
 * Which of `ids` match `query` in `scope`, using the same prefix and fuzzy rules
 * as every other hit. A server names the ids on a page, so an id indexed under
 * another scope is never a hit here, whatever the page said.
 */
export function matchMessages(query: string, ids: ReadonlySet<string>, scope: string): string[] {
  if (!query.trim() || ids.size === 0) return [];
  return index
    .search(query, { filter: (r) => ids.has(r.id as string) && r.scope === scope })
    .map((r) => r.id as string);
}
