/**
 * useChannelSearch — E2EE-native channel search with streaming backfill.
 *
 * 1. Queries the in-memory MiniSearch index for instant results
 * 2. Streams older messages via /messages/bulk, decrypts, scans, and indexes
 * 3. New results appear progressively as backfill runs
 * 4. AbortController cancels on query change or unmount
 */

import { useState, useCallback, useRef, useEffect } from 'react';
import { useChatStore } from '../stores/chatStore';
import { apiFetch, safeJson } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import {
  searchMessages,
  searchMessagesMultiScope,
  indexMessage,
  isIndexed,
} from '../services/searchService';
import type { MessageWithStatus, MessageWithUser } from '../types/chat';
import { unwrapGifEnvelope } from '../utils/gifEnvelope';

export const BACKFILL_BATCH_SIZE = 200;
export const BACKFILL_DELAY_MS = 100;
export const MAX_BACKFILL_MESSAGES = 2000;
export const DEBOUNCE_MS = 500;

interface SearchProgress {
  checked: number;
  total: number | null; // null = unknown
}

export interface UseChannelSearchResult {
  results: MessageWithStatus[];
  isSearching: boolean;
  progress: SearchProgress | null;
  search: (query: string) => void;
  cancel: () => void;
}

/** Decrypt a single message, returning decrypted content or null on failure. */
export async function decryptMessageContent(
  msg: MessageWithUser,
  scope: string
): Promise<string | null> {
  if (!e2eeService.isInitialized) {
    return msg.content;
  }
  try {
    if (msg.key_version && msg.key_version > 1) {
      return await e2eeService.decryptForChannelWithVersion(scope, msg.content, msg.key_version);
    }
    return await e2eeService.decryptForChannel(scope, msg.content);
  } catch {
    return null;
  }
}

/** Build a bulk-fetch URL for a scope with optional cursor. */
export function buildBulkUrl(scope: string, cursor: string | undefined): string {
  const base = `/api/v1/channels/${scope}/messages/bulk?limit=${BACKFILL_BATCH_SIZE}`;
  return cursor ? `${base}&before=${cursor}` : base;
}

/** Check if decrypted content matches the query (case-insensitive substring). */
export function contentMatchesQuery(content: string, query: string): boolean {
  return content.toLowerCase().includes(query.toLowerCase());
}

/** Process a single message from a backfill batch: decrypt, index, and check for match. */
export async function processBackfillMessage(
  msg: MessageWithUser,
  scope: string,
  query: string,
  allResultIds: Set<string>,
  onNewResult: (result: MessageWithStatus) => void
): Promise<void> {
  if (isIndexed(msg.id)) return;

  const decrypted = await decryptMessageContent(msg, scope);
  if (decrypted === null) return;

  // Unwrap GIF envelope so the search index sees the user-facing text, not
  // the JSON wrapper.
  const { text: content, gifSlug } = unwrapGifEnvelope(decrypted);
  indexMessage(msg.id, content, scope);

  if (contentMatchesQuery(content, query) && !allResultIds.has(msg.id)) {
    allResultIds.add(msg.id);
    onNewResult({ ...msg, content, gif_slug: gifSlug ?? msg.gif_slug, status: 'delivered' });
  }
}

/** Resolve message IDs to full MessageWithStatus from the chat store, preserving order. */
export function resolveMessagesFromStore(messageIds: string[]): MessageWithStatus[] {
  const allMessages = useChatStore.getState().messagesByChannel;
  const messageMap = new Map<string, MessageWithStatus>();

  for (const [, channelMsgs] of allMessages) {
    for (const msg of channelMsgs) {
      if (!messageMap.has(msg.id)) {
        messageMap.set(msg.id, msg);
      }
    }
  }

  const resolved: MessageWithStatus[] = [];
  for (const id of messageIds) {
    const msg = messageMap.get(id);
    if (msg) resolved.push(msg);
  }
  return resolved;
}

/**
 * Search messages in a channel (or multiple scopes for server-wide search).
 * Combines instant in-memory index results with streaming backfill of older messages.
 */
export function useChannelSearch(
  channelId: string | null,
  options?: { scopes?: string[] }
): UseChannelSearchResult {
  const [results, setResults] = useState<MessageWithStatus[]>([]);
  const [isSearching, setIsSearching] = useState(false);
  const [progress, setProgress] = useState<SearchProgress | null>(null);

  const abortRef = useRef<AbortController | null>(null);
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null);

  const cancel = useCallback(() => {
    abortRef.current?.abort();
    abortRef.current = null;
    setIsSearching(false);
    setProgress(null);
    setResults([]);
  }, []);

  const doSearch = useCallback(
    async (query: string) => {
      if (!channelId || !query.trim()) {
        setResults([]);
        setIsSearching(false);
        setProgress(null);
        return;
      }

      abortRef.current?.abort();
      const controller = new AbortController();
      abortRef.current = controller;

      setIsSearching(true);

      // Phase 1: Instant results from in-memory index
      const scopes = options?.scopes;
      const indexHits = scopes
        ? searchMessagesMultiScope(query, scopes)
        : searchMessages(query, channelId);
      setResults(resolveMessagesFromStore(indexHits));

      // Phase 2: Streaming backfill
      const searchScopes = scopes || [channelId];
      let totalChecked = 0;
      const allResultIds = new Set(indexHits);

      for (const scope of searchScopes) {
        if (controller.signal.aborted) break;
        totalChecked += await backfillScope(
          scope,
          query,
          controller,
          allResultIds,
          totalChecked,
          setResults,
          setProgress
        );
      }

      if (!controller.signal.aborted) {
        setIsSearching(false);
        setProgress(null);
      }
    },
    [channelId, options?.scopes]
  );

  const search = useCallback(
    (query: string) => {
      if (debounceRef.current) clearTimeout(debounceRef.current);
      debounceRef.current = setTimeout(() => {
        doSearch(query);
      }, DEBOUNCE_MS);
    },
    [doSearch]
  );

  useEffect(() => {
    return () => {
      cancel();
      if (debounceRef.current) clearTimeout(debounceRef.current);
    };
  }, [cancel]);

  return { results, isSearching, progress, search, cancel };
}

/** Backfill a single scope: fetch, decrypt, index, and collect matches. Returns messages checked. */
async function backfillScope(
  scope: string,
  query: string,
  controller: AbortController,
  allResultIds: Set<string>,
  priorChecked: number,
  setResults: React.Dispatch<React.SetStateAction<MessageWithStatus[]>>,
  setProgress: React.Dispatch<React.SetStateAction<SearchProgress | null>>
): Promise<number> {
  const loaded = useChatStore.getState().messagesByChannel.get(scope) || [];
  let cursor = loaded.length > 0 ? loaded[0].id : undefined;
  let scopeChecked = 0;

  while (scopeChecked < MAX_BACKFILL_MESSAGES && !controller.signal.aborted) {
    const batch = await fetchBatch(scope, cursor, controller);
    if (!batch) break;

    await processBatch(batch, scope, query, controller, allResultIds, setResults);

    scopeChecked += batch.length;
    setProgress({ checked: priorChecked + scopeChecked, total: null });
    cursor = batch.at(-1)?.id;

    if (batch.length < BACKFILL_BATCH_SIZE) break;
    await new Promise((resolve) => setTimeout(resolve, BACKFILL_DELAY_MS));
  }

  return scopeChecked;
}

/** Fetch a single batch of messages. Returns null on error or empty batch. */
async function fetchBatch(
  scope: string,
  cursor: string | undefined,
  controller: AbortController
): Promise<MessageWithUser[] | null> {
  try {
    const url = buildBulkUrl(scope, cursor);
    const res = await apiFetch(url, { signal: controller.signal });
    if (!res.ok) return null;
    const data = await safeJson<{ messages: MessageWithUser[] }>(res);
    const batch = data.messages || [];
    return batch.length > 0 ? batch : null;
  } catch {
    return null;
  }
}

/** Process all messages in a batch: decrypt, index, and emit matches. */
async function processBatch(
  batch: MessageWithUser[],
  scope: string,
  query: string,
  controller: AbortController,
  allResultIds: Set<string>,
  setResults: React.Dispatch<React.SetStateAction<MessageWithStatus[]>>
): Promise<void> {
  for (const msg of batch) {
    if (controller.signal.aborted) break;
    await processBackfillMessage(msg, scope, query, allResultIds, (result) => {
      setResults((prev) => [...prev, result]);
    });
  }
}
