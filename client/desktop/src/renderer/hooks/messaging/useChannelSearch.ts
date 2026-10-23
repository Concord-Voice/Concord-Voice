/**
 * useChannelSearch — chunked, lazy E2EE search (#3875).
 *
 * A query first searches what this session already decrypted. Scopes with no
 * coverage get one chunk automatically; every further chunk runs only when
 * the panel asks (scroll prefetch or "Search older messages"). A chunk the
 * pacer held back, or one a 429 cut short, runs on its own when the wait ends,
 * the cut-short one with only the budget it had left, and only for the
 * channels it was asked for. Records and coverage live for the search session
 * and are released when it ends.
 */
import { useState, useCallback, useRef, useEffect, useMemo } from 'react';
import { useChatStore } from '../../stores/chat/chatStore';
import { useConnectionStore } from '../../stores/ui/connectionStore';
import {
  beginSearchBackfill,
  getScopeCoverage,
  getSearchRecord,
  indexedScopeOf,
  releaseSearchScopes,
  searchMessages,
  searchMessagesMultiScope,
  subscribeSearchResultInvalidations,
  subscribeSearchScopeInvalidations,
  type SearchRecord,
} from '../../services/messaging/searchService';
import { ACTIVE_SEARCH_PROFILE, type SearchProfile } from '../../services/messaging/searchProfile';
import {
  bulkRequestPacer,
  reachedAt,
  runChunk,
  type ChunkOutcome,
  type ChunkResume,
  type RequestPacer,
} from '../../services/messaging/searchChunk';
import type { MessageWithStatus } from '../../types/chat';

export const DEBOUNCE_MS = 500;

export interface SearchResultItem {
  id: string;
  channelId: string;
  authorName: string;
  createdAt: string;
  content: string;
}

export type SearchStatus =
  | { kind: 'idle' }
  | { kind: 'running'; reachedAt: string | null }
  | { kind: 'more'; reachedAt: string | null }
  /** Nothing left to walk, and a channel stopped because the index is at its cap. */
  | { kind: 'full'; reachedAt: string | null }
  | { kind: 'paused'; resumeAt: number }
  | { kind: 'failed' }
  | { kind: 'finished' };

export interface UseChannelSearchOptions {
  scopes?: string[];
  profile?: SearchProfile;
  pacer?: RequestPacer;
  debounceMs?: number;
}

export interface UseChannelSearchResult {
  results: SearchResultItem[];
  status: SearchStatus;
  search: (query: string) => void;
  /** Runs one chunk when more is available or the last one failed. */
  loadMore: () => void;
  cancel: () => void;
  endSession: () => void;
}

const IDLE: SearchStatus = { kind: 'idle' };

interface OwedChunk {
  scopesKey: string;
  resume?: ChunkResume;
}

function fromRecord(r: SearchRecord): SearchResultItem {
  return {
    id: r.id,
    channelId: r.scope,
    authorName: r.displayName || r.username,
    createdAt: r.createdAt,
    content: r.content,
  };
}

function fromRow(m: MessageWithStatus): SearchResultItem {
  return {
    id: m.id,
    channelId: m.channel_id,
    authorName: m.display_name || m.username,
    createdAt: m.created_at,
    content: m.content,
  };
}

/** The readable ids of one channel's rows; the store replaces the array on every change. */
const readableIdsByRows = new WeakMap<readonly MessageWithStatus[], Set<string>>();

/** Whether the chat view still holds this row readably, so a hit can show from it. */
function isDisplayable(id: string, scope: string): boolean {
  const rows = useChatStore.getState().messagesByChannel.get(scope);
  if (!rows) return false;
  let ids = readableIdsByRows.get(rows);
  if (!ids) {
    ids = new Set(rows.filter((m) => !m.decryptFailed && !!m.content).map((m) => m.id));
    readableIdsByRows.set(rows, ids);
  }
  return ids.has(id);
}

/** The oldest row of a channel the chat shows readably and the index holds. */
function oldestDisplayable(scope: string): { id: string; createdAt: string } | undefined {
  const rows = useChatStore.getState().messagesByChannel.get(scope);
  let oldest: { id: string; createdAt: string } | undefined;
  for (const m of rows ?? []) {
    if (m.decryptFailed || !m.content || indexedScopeOf(m.id) !== scope) continue;
    if (oldest === undefined || Date.parse(m.created_at) < Date.parse(oldest.createdAt)) {
      oldest = { id: m.id, createdAt: m.created_at };
    }
  }
  return oldest;
}

/** A hit shows from its session record, else from a loaded chat row. */
export function resolveHits(ids: readonly string[]): SearchResultItem[] {
  let rows: Map<string, MessageWithStatus> | null = null;
  const out: SearchResultItem[] = [];
  for (const id of ids) {
    const record = getSearchRecord(id);
    if (record) {
      out.push(fromRecord(record));
      continue;
    }
    if (rows === null) {
      rows = new Map();
      for (const list of useChatStore.getState().messagesByChannel.values()) {
        for (const m of list) rows.set(m.id, m);
      }
    }
    const row = rows.get(id);
    if (row && !row.decryptFailed && row.content) out.push(fromRow(row));
  }
  return out;
}

/** Newest first, so a later chunk's older hits land at the end. */
export function mergeResults(
  current: readonly SearchResultItem[],
  incoming: readonly SearchResultItem[]
): SearchResultItem[] {
  const byId = new Map(current.map((r) => [r.id, r]));
  for (const r of incoming) byId.set(r.id, r);
  return [...byId.values()].sort(
    (a, b) => Date.parse(b.createdAt) - Date.parse(a.createdAt) || a.id.localeCompare(b.id)
  );
}

/** Scopes still being walked: not dropped, not at the start of history, not full. */
function liveScopesOf(scopes: readonly string[], dead: ReadonlySet<string>): string[] {
  return scopes.filter((s) => {
    const coverage = getScopeCoverage(s);
    return !dead.has(s) && !coverage?.done && !coverage?.full;
  });
}

/**
 * "Searched back to" over every scope not dropped after a 403. `reachedAt`
 * skips finished scopes; a full one counts, because it was searched only back
 * to where it stopped.
 */
function reachedOf(scopes: readonly string[], dead: ReadonlySet<string>): string | null {
  return reachedAt(scopes.filter((s) => !dead.has(s)));
}

/** Where a session rests between chunks. More to walk while the pacer is blocked is a pause. */
function settledStatus(
  scopes: readonly string[],
  dead: ReadonlySet<string>,
  pacer: RequestPacer
): SearchStatus {
  // Every channel refused access: nothing was searched, so never "Searched all messages".
  if (scopes.length > 0 && scopes.every((s) => dead.has(s))) return { kind: 'failed' };
  if (liveScopesOf(scopes, dead).length > 0) {
    if (pacer.blockedUntil > Date.now()) return { kind: 'paused', resumeAt: pacer.blockedUntil };
    return { kind: 'more', reachedAt: reachedOf(scopes, dead) };
  }
  if (scopes.some((s) => !dead.has(s) && getScopeCoverage(s)?.full)) {
    return { kind: 'full', reachedAt: reachedOf(scopes, dead) };
  }
  return { kind: 'finished' };
}

/** Results updater that drops one message. */
const withoutMessage = (messageId: string) => (current: SearchResultItem[]) =>
  current.filter((r) => r.id !== messageId);

/** Results updater that drops every hit from one scope. */
const withoutScope = (scope: string) => (current: SearchResultItem[]) =>
  current.filter((r) => r.channelId !== scope);

export function useChannelSearch(
  channelId: string | null,
  options: UseChannelSearchOptions = {}
): UseChannelSearchResult {
  const profile = options.profile ?? ACTIVE_SEARCH_PROFILE;
  const pacer = options.pacer ?? bulkRequestPacer;
  const debounceMs = options.debounceMs ?? DEBOUNCE_MS;
  const optionScopes = options.scopes;
  const scopes = useMemo(
    () => optionScopes ?? (channelId ? [channelId] : []),
    [optionScopes, channelId]
  );

  const [results, setResults] = useState<SearchResultItem[]>([]);
  const [statusValue, setStatusValue] = useState<SearchStatus>(IDLE);
  const statusRef = useRef<SearchStatus>(IDLE);
  const queryRef = useRef('');
  const runRef = useRef<AbortController | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);
  const deadRef = useRef(new Set<string>());
  const sessionRef = useRef(new Set<string>());
  /**
   * A chunk asked for and not yet run, for one set of channels: held back by
   * the pacer (no resume), or cut short by a 429 (its resume). Only the
   * automatic run after the wait spends it, and only for the same channels.
   * A new query keeps it: a chunk is fetch work, not tied to the query text.
   */
  const owedRef = useRef<OwedChunk | null>(null);
  const scopesKey = useMemo(
    () => [...scopes].sort((a, b) => a.localeCompare(b)).join(','),
    [scopes]
  );

  const setStatus = useCallback((next: SearchStatus) => {
    statusRef.current = next;
    setStatusValue(next);
  }, []);

  const stopRun = useCallback(() => {
    runRef.current?.abort();
    runRef.current = null;
  }, []);

  /** "Searched back to" for the session (a dropped channel never holds it back). */
  const reachedLive = useCallback(() => reachedOf(scopes, deadRef.current), [scopes]);

  const runOneChunk = useCallback(
    async (query: string, finishOwed = false): Promise<void> => {
      if (runRef.current || scopes.length === 0) return;
      // Still blocked by an earlier 429: say so, without announcing a search
      // that cannot start, and owe the chunk unless these channels already are.
      if (pacer.blockedUntil > Date.now()) {
        if (owedRef.current?.scopesKey !== scopesKey) owedRef.current = { scopesKey };
        setStatus({ kind: 'paused', resumeAt: pacer.blockedUntil });
        return;
      }
      const owed = owedRef.current;
      const resume = finishOwed && owed?.scopesKey === scopesKey ? owed.resume : undefined;
      owedRef.current = null;
      const controller = new AbortController();
      runRef.current = controller;
      for (const s of scopes) sessionRef.current.add(s);
      const guard = beginSearchBackfill(scopes);
      setStatus({ kind: 'running', reachedAt: reachedLive() });
      let outcome: ChunkOutcome;
      let lifecycleChanged = false;
      try {
        outcome = await runChunk({
          scopes,
          query,
          profile,
          pacer,
          guard,
          signal: controller.signal,
          deadScopes: deadRef.current,
          isDisplayable,
          oldestDisplayable,
          resume,
          onHits: (ids) => {
            if (!controller.signal.aborted)
              setResults((prev) => mergeResults(prev, resolveHits(ids)));
          },
          onProgress: () => {
            if (!controller.signal.aborted)
              setStatus({ kind: 'running', reachedAt: reachedLive() });
          },
        });
      } catch (err) {
        // An abort this run did not ask for: apiFetch refused to send because the
        // account or server changed under it. The session is gone, not failed.
        lifecycleChanged = err instanceof DOMException && err.name === 'AbortError';
        outcome = { kind: 'failed' };
      } finally {
        guard.close();
        if (runRef.current === controller) runRef.current = null;
      }
      if (controller.signal.aborted) return;
      if (lifecycleChanged) {
        // Nothing of the old account's or server's search stays on screen.
        queryRef.current = '';
        setResults([]);
        setStatus(IDLE);
      } else if (outcome.kind === 'rateLimited') {
        owedRef.current = { scopesKey, resume: outcome.resume };
        setStatus({ kind: 'paused', resumeAt: outcome.resumeAt });
      } else if (outcome.kind === 'failed') setStatus({ kind: 'failed' });
      else setStatus(settledStatus(scopes, deadRef.current, pacer));
    },
    [scopes, scopesKey, profile, pacer, setStatus, reachedLive]
  );

  /** Replace the results with what the index already holds for `query`; no fetch. */
  const showIndexHits = useCallback(
    (query: string) => {
      const hits =
        scopes.length === 1
          ? searchMessages(query, scopes[0])
          : searchMessagesMultiScope(query, scopes);
      setResults(mergeResults([], resolveHits(hits)));
    },
    [scopes]
  );

  const startQuery = useCallback(
    (query: string) => {
      stopRun();
      if (!query.trim() || scopes.length === 0) {
        // Nothing to search: no request, no chunk, no session growth.
        queryRef.current = '';
        setResults([]);
        setStatus(IDLE);
        return;
      }
      queryRef.current = query;
      showIndexHits(query);
      const uncovered = scopes.some(
        (s) => !deadRef.current.has(s) && getScopeCoverage(s) === undefined
      );
      if (uncovered) void runOneChunk(query);
      else setStatus(settledStatus(scopes, deadRef.current, pacer));
    },
    [scopes, pacer, stopRun, showIndexHits, runOneChunk, setStatus]
  );

  const search = useCallback(
    (query: string) => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      // The running chunk is for a query the user has moved on from; it must
      // not keep spending the request budget through the debounce.
      stopRun();
      debounceRef.current = setTimeout(() => {
        debounceRef.current = null;
        startQuery(query);
      }, debounceMs);
    },
    [startQuery, debounceMs, stopRun]
  );

  const loadMore = useCallback(() => {
    const kind = statusRef.current.kind;
    if ((kind === 'more' || kind === 'failed') && queryRef.current) {
      void runOneChunk(queryRef.current);
    }
  }, [runOneChunk]);

  const cancel = useCallback(() => {
    if (debounceRef.current) {
      clearTimeout(debounceRef.current);
      debounceRef.current = null;
    }
    stopRun();
    queryRef.current = '';
    setResults([]);
    setStatus(IDLE);
  }, [stopRun, setStatus]);

  // ponytail: records and coverage are module-wide; one SearchPanel exists today. A second concurrent hook on the same scope would release the other's records at session end.
  const endSession = useCallback(() => {
    cancel();
    releaseSearchScopes(sessionRef.current);
    sessionRef.current = new Set();
    deadRef.current = new Set();
    owedRef.current = null;
  }, [cancel]);

  // When the server's wait is over, run the chunk owed to these channels; with
  // none owed (the session only rested while blocked), settle without fetching.
  useEffect(() => {
    if (statusValue.kind !== 'paused') return;
    const timer = setTimeout(
      () => {
        if (queryRef.current && owedRef.current?.scopesKey === scopesKey) {
          setStatus({ kind: 'more', reachedAt: reachedLive() });
          void runOneChunk(queryRef.current, true);
        } else {
          setStatus(settledStatus(scopes, deadRef.current, pacer));
        }
      },
      Math.max(0, statusValue.resumeAt - Date.now())
    );
    return () => clearTimeout(timer);
  }, [statusValue, scopes, scopesKey, pacer, runOneChunk, setStatus, reachedLive]);

  useEffect(
    () =>
      subscribeSearchResultInvalidations((messageId) => {
        setResults(withoutMessage(messageId));
      }),
    []
  );

  useEffect(
    () =>
      subscribeSearchScopeInvalidations((scope) => {
        if (scope === null) {
          // Logout or account reset: the session is gone, not failed. A query
          // typed before it must not start one under the next account.
          if (debounceRef.current) {
            clearTimeout(debounceRef.current);
            debounceRef.current = null;
          }
          stopRun();
          deadRef.current = new Set();
          sessionRef.current = new Set();
          owedRef.current = null;
          queryRef.current = '';
          setResults([]);
          setStatus(IDLE);
          return;
        }
        setResults(withoutScope(scope));
        const kind = statusRef.current.kind;
        if (queryRef.current && (kind === 'more' || kind === 'full' || kind === 'finished')) {
          setStatus(settledStatus(scopes, deadRef.current, pacer));
        }
      }),
    [scopes, pacer, stopRun, setStatus]
  );

  // A reconnect may have missed edits and deletes: forget the session's copies
  // and show only what the chat path still owns. No automatic refetch. A
  // grace-period reconnect also returns to 'stable' before dispatching the
  // event; running both is harmless.
  useEffect(() => {
    const onRecovered = () => {
      stopRun();
      releaseSearchScopes(sessionRef.current);
      // The coverage an owed chunk would continue from is gone.
      owedRef.current = null;
      if (!queryRef.current) return;
      showIndexHits(queryRef.current);
      setStatus(settledStatus(scopes, deadRef.current, pacer));
    };
    const unsubscribe = useConnectionStore.subscribe((state, prev) => {
      if (state.phase === 'stable' && prev.phase !== 'stable') onRecovered();
    });
    globalThis.addEventListener('connection-recovered', onRecovered);
    return () => {
      unsubscribe();
      globalThis.removeEventListener('connection-recovered', onRecovered);
    };
  }, [scopes, pacer, stopRun, showIndexHits, setStatus]);

  // The session belongs to one channel and one mounted panel.
  useEffect(() => () => endSession(), [channelId, endSession]);

  return { results, status: statusValue, search, loadMore, cancel, endSession };
}
