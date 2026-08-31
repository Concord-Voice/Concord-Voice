import { useEffect, useState, useCallback, useRef } from 'react';
import { useChatStore } from '../../stores/chat/chatStore';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import { e2eeService, type E2EEChannelOperationGuard } from '../../services/e2ee/e2eeService';
import { isPendingKeyError } from '../../services/e2ee/e2eeErrors';
import type { MessageWithStatus } from '../../types/chat';
import {
  indexMessages,
  subscribeSearchResultInvalidations,
} from '../../services/messaging/searchService';
import { unwrapGifEnvelope } from '../../utils/messaging/gifEnvelope';

const DEFAULT_LIMIT = 50;
const MAX_PAGINATION_RECONCILIATION_ATTEMPTS = 2;

/**
 * Purge generations, keyed by scope (channel id or DM conversation id).
 *
 * A purge is irreversible deletion, so a request that was already in flight
 * when it landed must never publish. The effect-local `aborted` flag cannot
 * cover that window on its own: it only flips in the fetch effect's cleanup,
 * which runs after React commits the `setFetchTrigger` bump the purge listener
 * queues. A response resolving in between passes every `if (aborted) return`
 * and repopulates both the store and the search index with purged plaintext.
 * The generation is bumped synchronously with the purge event, snapshotted at
 * request start, and re-asserted at publication — the same shape as the
 * existing E2EE `operationGuard` fence it sits beside (#1741).
 */
const purgeGenerationByScope = new Map<string, number>();

function currentPurgeGeneration(scopeId: string): number {
  return purgeGenerationByScope.get(scopeId) ?? 0;
}

function recordScopePurge(scopeId: string): void {
  purgeGenerationByScope.set(scopeId, currentPurgeGeneration(scopeId) + 1);
}

/** Index decrypted messages for search (passive, skips failed/pending). */
function indexDecryptedMessages(channelId: string, msgs: MessageWithStatus[]) {
  const indexable = msgs
    .filter((m) => !m.decryptFailed && !m.pendingKeys && m.content)
    .map((m) => ({ id: m.id, content: m.content, scope: channelId }));
  if (indexable.length > 0) indexMessages(indexable);
}

interface UseMessageFetchOptions {
  type: 'channel' | 'dm';
  limit?: number;
  onFetchComplete?: () => void;
}

function timestampMillis(value: string | null | undefined): number {
  if (!value) return 0;
  const parsed = Date.parse(value);
  return Number.isNaN(parsed) ? 0 : parsed;
}

/** Prefer authoritative edit time; DM edit responses may omit updated_at. */
export function messageFreshness(message: Pick<MessageWithStatus, 'edited_at' | 'updated_at'>) {
  const editedAt = timestampMillis(message.edited_at);
  return editedAt || timestampMillis(message.updated_at);
}

export function reconcileFetchedMessages(
  fetched: MessageWithStatus[],
  current: MessageWithStatus[],
  idsAtRequestStart: ReadonlySet<string>,
  invalidatedDuringRequest: ReadonlySet<string> = new Set()
): {
  fetched: MessageWithStatus[];
  preserved: MessageWithStatus[];
  needsAuthoritativeRefetch: boolean;
} {
  const currentById = new Map(current.map((message) => [message.id, message]));
  let needsAuthoritativeRefetch = false;
  const reconciledFetched = fetched.flatMap((fetchedMessage) => {
    const currentMessage = currentById.get(fetchedMessage.id);
    if (idsAtRequestStart.has(fetchedMessage.id) && !currentMessage) {
      // The row was deleted while this request/decrypt pass was in flight.
      return [];
    }
    if (currentMessage && messageFreshness(currentMessage) > messageFreshness(fetchedMessage)) {
      return [currentMessage];
    }
    if (!currentMessage && invalidatedDuringRequest.has(fetchedMessage.id)) {
      // Covers edit/delete events for rows that were not loaded when the REST
      // snapshot began. Those events still invalidate search by ID.
      needsAuthoritativeRefetch = true;
      return [];
    }
    return [fetchedMessage];
  });

  const fetchedIds = new Set(reconciledFetched.map((message) => message.id));
  const acknowledgedClientIds = new Set(
    reconciledFetched
      .map((message) => message.clientMessageId)
      .filter((id): id is string => Boolean(id))
  );
  const preserved = current.filter((message) => {
    if (
      fetchedIds.has(message.id) ||
      acknowledgedClientIds.has(message.id) ||
      (message.clientMessageId && fetchedIds.has(message.clientMessageId))
    ) {
      return false;
    }
    const isOptimistic =
      Boolean(message.clientMessageId) &&
      (message.status === 'pending' || message.status === 'sent');
    const arrivedDuringRequest = !idsAtRequestStart.has(message.id);
    return isOptimistic || arrivedDuringRequest;
  });

  return { fetched: reconciledFetched, preserved, needsAuthoritativeRefetch };
}

/**
 * Decrypt a single ciphertext using the appropriate key version.
 * Prefers pre-fetched keys from the batch maps; falls back to on-demand fetch.
 */
async function decryptContent(
  channelId: string,
  ciphertext: string,
  keyVersion: number | undefined,
  channelKey: CryptoKey | null,
  versionedKeys: Map<number, CryptoKey>,
  operationGuard: E2EEChannelOperationGuard
): Promise<string> {
  operationGuard.assertCurrent();
  let plaintext: string;
  if (keyVersion && keyVersion > 1) {
    const vKey = versionedKeys.get(keyVersion);
    plaintext = vKey
      ? await e2eeService.decryptWithKey(ciphertext, vKey, operationGuard)
      : await e2eeService.decryptForChannelWithVersion(channelId, ciphertext, keyVersion);
  } else {
    plaintext = channelKey
      ? await e2eeService.decryptWithKey(ciphertext, channelKey, operationGuard)
      : await e2eeService.decryptForChannel(channelId, ciphertext);
  }
  operationGuard.assertCurrent();
  return plaintext;
}

/**
 * Decrypt a batch of messages, pre-fetching the channel key once to avoid
 * N concurrent getChannelKey() calls.
 */
async function decryptMessages(
  channelId: string,
  rawMsgs: MessageWithStatus[],
  operationGuard: E2EEChannelOperationGuard
): Promise<MessageWithStatus[]> {
  operationGuard.assertCurrent();
  // Fail-closed: if E2EE isn't initialized, blank content rather than
  // leaking ciphertext. Mark as pendingKeys so the UI shows the correct placeholder.
  if (!e2eeService.isInitialized) {
    return rawMsgs.map((m) => {
      // Call-event rows have no ciphertext; never mark them pendingKeys (#1219).
      if (m.type === 'call_event') {
        return m;
      }
      const rt = m.replied_to;
      const blankedRt = rt ? { ...rt, content: '' } : rt;
      return { ...m, content: '', pendingKeys: true, replied_to: blankedRt };
    });
  }

  // Pre-fetch channel key ONCE for batch decryption (current version)
  let channelKey: CryptoKey | null = null;
  try {
    channelKey = await e2eeService.getChannelKey(channelId);
  } catch {
    // Key not available yet — individual messages will show pending/failed state
  }

  // Pre-fetch each unique historical key version ONCE to avoid N parallel
  // getChannelKeyByVersion() calls.
  const versionedKeys = new Map<number, CryptoKey>();
  const uniqueVersions = new Set<number>();
  for (const m of rawMsgs) {
    if (m.key_version && m.key_version > 1) {
      uniqueVersions.add(m.key_version);
    }
    const rt = m.replied_to;
    if (rt?.key_version && rt.key_version > 1) {
      uniqueVersions.add(rt.key_version);
    }
  }
  await Promise.all(
    [...uniqueVersions].map(async (version) => {
      try {
        const key = await e2eeService.getChannelKeyByVersion(channelId, version);
        versionedKeys.set(version, key);
      } catch {
        // Will be handled per-message in the decrypt loop below
      }
    })
  );
  operationGuard.assertCurrent();

  const decryptedMessages = await Promise.all(
    rawMsgs.map(async (m) => {
      // Call-event rows carry plaintext server metadata in call_event_payload
      // and empty content; bypass the E2EE decrypt pass (#1219) — decryptContent
      // on '' would set decryptFailed. Return the row untouched. This covers
      // both the initial and pagination fetch sites, which both go through
      // decryptMessages.
      if (m.type === 'call_event') {
        return m;
      }

      // Decrypt replied_to content if the original replied-to message was encrypted
      const rt = m.replied_to;
      let decryptedRt = rt ? { ...rt, content: '' } : rt;
      if (rt?.content) {
        try {
          const rtPlaintext = await decryptContent(
            channelId,
            rt.content,
            rt.key_version,
            channelKey,
            versionedKeys,
            operationGuard
          );
          decryptedRt = { ...rt, content: rtPlaintext };
        } catch {
          // Keep the reply preview fail-closed; never return its ciphertext.
        }
      }

      // Decrypt content
      try {
        const plaintext = await decryptContent(
          channelId,
          m.content,
          m.key_version,
          channelKey,
          versionedKeys,
          operationGuard
        );
        // E2EE GIF messages encrypt a JSON envelope; unwrap so the renderer
        // sees the same shape it gets from the real-time WebSocket path.
        const { text: content, gifSlug } = unwrapGifEnvelope(plaintext);
        return { ...m, content, gif_slug: gifSlug ?? m.gif_slug, replied_to: decryptedRt };
      } catch (err) {
        const isPending = isPendingKeyError(err);
        return {
          ...m,
          content: '',
          decryptFailed: !isPending,
          pendingKeys: isPending,
          replied_to: decryptedRt,
        };
      }
    })
  );
  operationGuard.assertCurrent();
  return decryptedMessages;
}

function buildEndpoint(type: 'channel' | 'dm', channelId: string, limit: number, before?: string) {
  const base =
    type === 'dm'
      ? `/api/v1/dm/conversations/${channelId}/messages`
      : `/api/v1/channels/${channelId}/messages`;
  const params = new URLSearchParams({ limit: String(limit) });
  if (before) params.set('before', before);
  return `${base}?${params}`;
}

async function readMessagePage(
  res: Response,
  channelId: string,
  fallbackError: string
): Promise<MessageWithStatus[]> {
  if (!res.ok) {
    const data = await safeJson<{ error?: string }>(res);
    throw new Error(data.error || fallbackError);
  }

  const data = await safeJson<{ messages?: MessageWithStatus[] }>(res);
  return (data.messages || []).map((message) => ({
    ...message,
    channel_id: message.channel_id || channelId,
    status: 'delivered' as const,
  }));
}

function handleFetchError(
  err: unknown,
  fallbackError: string,
  retry: () => void,
  setError: (error: string) => void
) {
  if (isPendingKeyError(err)) {
    retry();
    return;
  }
  setError(err instanceof Error ? err.message : fallbackError);
}

async function loadOlderMessagesAttempt({
  type,
  channelId,
  limit,
  before,
  attempt,
  isCurrent,
  prependMessages,
  setHasMore,
}: {
  type: 'channel' | 'dm';
  channelId: string;
  limit: number;
  before: string;
  attempt: number;
  isCurrent: () => boolean;
  prependMessages: (channelId: string, messages: MessageWithStatus[]) => void;
  setHasMore: (hasMore: boolean) => void;
}): Promise<'retry' | 'done'> {
  const attemptMessages = useChatStore.getState().messagesByChannel.get(channelId) || [];
  const idsAtRequestStart = new Set(attemptMessages.map((message) => message.id));
  const invalidatedDuringRequest = new Set<string>();
  const unsubscribeInvalidations = subscribeSearchResultInvalidations((messageId) => {
    invalidatedDuringRequest.add(messageId);
  });
  const operationGuard = e2eeService.createChannelOperationGuard(channelId);

  try {
    const res = await apiFetch(buildEndpoint(type, channelId, limit, before));

    // Channel changed while request was in flight — discard results.
    if (!isCurrent()) return 'done';

    const rawMsgs = await readMessagePage(res, channelId, 'Failed to load more messages');
    const msgs = await decryptMessages(channelId, rawMsgs, operationGuard);
    operationGuard.assertCurrent();

    if (!isCurrent()) return 'done';

    const currentMessages = useChatStore.getState().messagesByChannel.get(channelId) || [];
    const reconciled = reconcileFetchedMessages(
      msgs,
      currentMessages,
      idsAtRequestStart,
      invalidatedDuringRequest
    );
    if (reconciled.needsAuthoritativeRefetch) {
      if (attempt + 1 < MAX_PAGINATION_RECONCILIATION_ATTEMPTS) return 'retry';
      // Keep the cursor retryable if this page changed twice in a row.
      setHasMore(true);
      return 'done';
    }

    // Server returns DESC; reverse to ASC so oldest-first prepends correctly.
    reconciled.fetched.reverse();
    operationGuard.assertCurrent();
    prependMessages(channelId, reconciled.fetched);
    setHasMore(rawMsgs.length === limit);
    return 'done';
  } finally {
    unsubscribeInvalidations();
  }
}

/**
 * Loads, decrypts, reconciles, and paginates messages for a channel.
 *
 * @param channelId - The channel to load, or `null` when no channel is active.
 * @param options - Message type, page size, and optional completion callback.
 * @returns The channel messages, loading and pagination state, error state, and older-message loader.
 */
export function useMessageFetch(channelId: string | null, options: UseMessageFetchOptions) {
  const { type, limit = DEFAULT_LIMIT, onFetchComplete } = options;

  // Subscribe only to the current channel's messages to avoid re-renders
  // when other channels receive messages.
  const messages: MessageWithStatus[] =
    useChatStore((s) => (channelId ? s.messagesByChannel.get(channelId) : undefined)) || [];
  const setMessages = useChatStore((s) => s.setMessages);
  const prependMessages = useChatStore((s) => s.prependMessages);

  const [isLoading, setIsLoading] = useState(false);
  const [hasMore, setHasMore] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [fetchTrigger, setFetchTrigger] = useState(0);
  const retryFetch = useCallback(() => {
    setFetchTrigger((prev) => prev + 1);
  }, []);

  // Keep onFetchComplete in a ref to avoid stale closures without
  // including it in effect dependencies (which would cause re-fetches).
  const onFetchCompleteRef = useRef(onFetchComplete);
  onFetchCompleteRef.current = onFetchComplete;

  // Track current channelId so in-flight pagination requests can detect
  // when the channel has changed and skip stale state updates.
  const channelIdRef = useRef(channelId);
  channelIdRef.current = channelId;
  useEffect(() => {
    // React StrictMode replays effect setup/cleanup; restore the live value on
    // each setup so its first cleanup cannot permanently null this guard.
    channelIdRef.current = channelId;
    return () => {
      if (channelIdRef.current === channelId) {
        channelIdRef.current = null;
      }
    };
  }, [channelId]);

  // Listen for key delivery events to re-fetch and decrypt messages
  useEffect(() => {
    if (!channelId) return;
    const handler = (e: Event) => {
      const { channelId: deliveredId } = (e as CustomEvent).detail;
      if (deliveredId === channelId) {
        setFetchTrigger((prev) => prev + 1);
      }
    };
    globalThis.addEventListener('e2ee-key-delivered', handler);
    return () => globalThis.removeEventListener('e2ee-key-delivered', handler);
  }, [channelId]);

  // #2329: after a Recovery-A reconnect the WebSocket replays only subscriptions
  // + a presence snapshot, so messages sent during the outage are never
  // redelivered. useConnectionRecovery dispatches `connection-recovered` once the
  // session-guarded hydration completes; re-fetch the mounted channel so the gap
  // is backfilled. The fetch effect's own aborted/channel/E2EE-operation guards
  // keep this session-safe.
  useEffect(() => {
    if (!channelId) return;
    const handler = () => setFetchTrigger((prev) => prev + 1);
    globalThis.addEventListener('connection-recovered', handler);
    return () => globalThis.removeEventListener('connection-recovered', handler);
  }, [channelId]);

  // A purge clears the scope wholesale; the server is the only source of truth for
  // what survived. One seam covers channels and DMs alike — useMessageFetch already
  // builds the DM URL from `type` (see buildEndpoint).
  useEffect(() => {
    if (!channelId) return;
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { scopeId?: string | null } | undefined;
      const scopeId = detail?.scopeId;
      // A server-wide purge has no channel scope to name — a server id can
      // never equal a channel/conversation id — so it dispatches a null scope
      // meaning "refetch whatever is mounted". A real scope id still has to
      // match this hook's channel exactly.
      if (scopeId != null && scopeId !== channelId) return;
      recordScopePurge(channelId);
      setFetchTrigger((prev) => prev + 1);
    };
    globalThis.addEventListener('messages-purged', handler);
    return () => globalThis.removeEventListener('messages-purged', handler);
  }, [channelId]);

  // Retry key fetch when messages are stuck in pending state.
  // Only retry when E2EE is initialized — fail-closed also sets pendingKeys
  // but retrying without E2EE just wastes cycles.
  const hasPendingKeys = messages.some((m) => m.pendingKeys);
  useEffect(() => {
    if (!hasPendingKeys || !channelId || !e2eeService.isInitialized) return;

    const interval = setInterval(async () => {
      try {
        await e2eeService.getChannelKey(channelId);
        // Key is now available — re-fetch messages to decrypt using the cached key
        setFetchTrigger((prev) => prev + 1);
      } catch {
        // Still pending — will retry
      }
    }, 5000);

    return () => clearInterval(interval);
  }, [channelId, hasPendingKeys]);

  // Fetch message history when channel changes or keys are delivered
  useEffect(() => {
    if (!channelId) return;

    let aborted = false;

    const fetchMessages = async () => {
      setIsLoading(true);
      setError(null);
      const idsAtRequestStart = new Set(
        (useChatStore.getState().messagesByChannel.get(channelId) || []).map(
          (message) => message.id
        )
      );
      const invalidatedDuringRequest = new Set<string>();
      const purgeGenerationAtRequestStart = currentPurgeGeneration(channelId);
      const unsubscribeInvalidations = subscribeSearchResultInvalidations((messageId) => {
        invalidatedDuringRequest.add(messageId);
      });

      try {
        const operationGuard = e2eeService.createChannelOperationGuard(channelId);
        const res = await apiFetch(buildEndpoint(type, channelId, limit));
        if (aborted) return;

        const rawMsgs = await readMessagePage(res, channelId, 'Failed to load messages');
        if (aborted) return;

        const msgs = await decryptMessages(channelId, rawMsgs, operationGuard);
        operationGuard.assertCurrent();
        if (aborted) return;

        // Preserve live edits, deletes, arrivals, and optimistic rows that won
        // while the REST snapshot was being fetched/decrypted.
        const existing = useChatStore.getState().messagesByChannel.get(channelId) || [];
        const reconciled = reconcileFetchedMessages(
          msgs,
          existing,
          idsAtRequestStart,
          invalidatedDuringRequest
        );
        // Server returns DESC (newest first); reverse to ASC for chronological display
        reconciled.fetched.reverse();
        const merged = [...reconciled.fetched, ...reconciled.preserved];
        operationGuard.assertCurrent();
        // A purge landed after this request started: everything below it is
        // deleted plaintext. The refetch the purge queued establishes truth.
        if (currentPurgeGeneration(channelId) !== purgeGenerationAtRequestStart) return;
        unsubscribeInvalidations();
        indexDecryptedMessages(channelId, merged);
        setMessages(channelId, merged);
        setHasMore(rawMsgs.length === limit);
        onFetchCompleteRef.current?.();
        if (reconciled.needsAuthoritativeRefetch) {
          // An edit/delete for an unloaded row can only tombstone this stale
          // snapshot. Fetch once more so the authoritative post-event state
          // (updated row or confirmed absence) is not missing until remount.
          setFetchTrigger((prev) => prev + 1);
        }
      } catch (err) {
        if (!aborted) {
          // A key rotation invalidated this batch after decryption. Start a
          // fresh request with the new generation; access revocations use a
          // terminal NOT_MEMBER fence and never enter this retry path.
          handleFetchError(err, 'Failed to load messages', retryFetch, setError);
        }
      } finally {
        unsubscribeInvalidations();
        if (!aborted) {
          setIsLoading(false);
        }
      }
    };

    fetchMessages();

    return () => {
      aborted = true;
    };
  }, [channelId, setMessages, fetchTrigger, retryFetch, type, limit]);

  // Load older messages (pagination)
  const handleLoadMore = useCallback(async () => {
    if (!channelId || isLoading || !hasMore) return;

    const requestChannelId = channelId;
    const channelMessages = useChatStore.getState().messagesByChannel.get(requestChannelId);
    if (!channelMessages || channelMessages.length === 0) return;

    const oldestMessage = channelMessages[0];
    const purgeGenerationAtRequestStart = currentPurgeGeneration(requestChannelId);

    setIsLoading(true);
    try {
      for (let attempt = 0; attempt < MAX_PAGINATION_RECONCILIATION_ATTEMPTS; attempt += 1) {
        const outcome = await loadOlderMessagesAttempt({
          type,
          channelId: requestChannelId,
          limit,
          before: oldestMessage.id,
          attempt,
          // A page in flight when a purge lands carries deleted plaintext, so
          // the purge generation fences pagination exactly as it fences the
          // initial fetch above.
          isCurrent: () =>
            channelIdRef.current === requestChannelId &&
            currentPurgeGeneration(requestChannelId) === purgeGenerationAtRequestStart,
          prependMessages,
          setHasMore,
        });
        if (outcome === 'done') return;
      }
    } catch (err) {
      if (channelIdRef.current === requestChannelId) {
        // Reconcile from a fresh initial snapshot after rotation. Retrying
        // the same `before` cursor could be stale after live edits/deletes.
        handleFetchError(err, 'Failed to load more messages', retryFetch, setError);
      }
    } finally {
      if (channelIdRef.current === requestChannelId) {
        setIsLoading(false);
      }
    }
  }, [channelId, isLoading, hasMore, prependMessages, retryFetch, type, limit]);

  return { messages, isLoading, hasMore, error, handleLoadMore };
}
