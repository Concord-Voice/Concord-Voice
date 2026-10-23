import { useEffect, useState, useCallback, useRef } from 'react';
import { z } from 'zod';
import { useDMStore } from '../../stores/chat/dmStore';
import { useChatStore } from '../../stores/chat/chatStore';
import { apiFetch, safeJson } from '../../services/system/apiClient';
import { e2eeService, type E2EEChannelOperationGuard } from '../../services/e2ee/e2eeService';
import { isPendingKeyError } from '../../services/e2ee/e2eeErrors';
import type { MessageWithStatus } from '../../types/chat';
import { DMExpirationEventSchema, ISOTimestamp, UUID } from '../../types/ws-events';
import {
  indexMessages,
  removeScope,
  subscribeSearchResultInvalidations,
} from '../../services/messaging/searchService';
import { unwrapGifEnvelope } from '../../utils/messaging/gifEnvelope';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import { isAbortError } from '../../services/system/requestContext';

const DEFAULT_LIMIT = 50;
const MAX_PAGINATION_RECONCILIATION_ATTEMPTS = 2;
/** A channel jump page: the list route's cap (`messages/handlers.go:223`). */
const JUMP_PAGE_LIMIT = 100;

/**
 * How `loadUntil` ended (#3894). `found`: the row is in the store.
 * `exhausted`: history ended without it. `too-far`: the request budget is
 * spent. `interrupted`: a history refetch or a moved head dropped the page;
 * run again once history is ready. `failed`: refused, rate-limited, purged,
 * a network error, or a channel whose own fetch never answered (whether older
 * history exists is unknown). `cancelled`: the channel or the account changed,
 * or the hook's owner went away.
 */
export type LoadUntilStatus =
  'found' | 'exhausted' | 'too-far' | 'interrupted' | 'failed' | 'cancelled';

/** `requests` counts HTTP attempts, including reconcile retries and joined pages. */
export interface LoadUntilResult {
  status: LoadUntilStatus;
  requests: number;
}

interface PageOptions {
  /** Scroll pages report through `error`; jump pages never do. */
  reportError: boolean;
  pageLimit: number;
  maxAttempts: number;
}

interface PageResult {
  status: 'done' | 'interrupted' | 'failed' | 'cancelled';
  requests: number;
}

const plaintextMetadataSchema = z.discriminatedUnion('type', [
  z.object({
    type: z.literal('call_event'),
    content: z.literal(''),
    call_event_payload: z.object({
      ring_id: UUID.optional(),
      caller_user_id: UUID.optional(),
      participant_user_ids: z.array(UUID).nullish(),
      started_at: ISOTimestamp,
      ended_at: ISOTimestamp.optional(),
      status: z.enum(['completed', 'missed', 'declined', 'canceled', 'failed']),
      duration_seconds: z.number().int().nonnegative(),
    }),
  }),
  z.object({
    type: z.literal('expiration_event'),
    content: z.literal(''),
    expiration_event_payload: DMExpirationEventSchema.shape.data
      .pick({
        kind: true,
        actor_user_id: true,
        window_seconds: true,
        previous_window_seconds: true,
        include_pinned: true,
        previous_include_pinned: true,
      })
      .extend({ changed_at: ISOTimestamp }),
  }),
]);

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

/**
 * Live history fetches (initial and refetch), counted per channel across EVERY hook
 * instance (#3894).
 *
 * A page publishes only while none runs for its channel. The refetch replaces that
 * channel's rows, and a page published inside its window is read by it as an arrival
 * and appended after the newest page, out of order and with the cursor unmoved. The
 * count cannot live in the hook: the main chat and the voice panel for a voice
 * channel's linked text chat each run the hook for one channel over one set of rows,
 * so one instance's page has to be fenced by the other's fetch. A fetch for a channel
 * the chat has left is keyed by that channel and gates nothing in the new one.
 */
const liveHistoryFetches = new Map<string, number>();
const historyIdleWaiters = new Set<{ channelId: string; wake: () => void }>();

function historyFetchesFor(key: string | null): number {
  return key ? (liveHistoryFetches.get(key) ?? 0) : 0;
}

function historyFetchBegan(key: string): void {
  liveHistoryFetches.set(key, historyFetchesFor(key) + 1);
}

function historyFetchEnded(key: string): void {
  const left = historyFetchesFor(key) - 1;
  if (left > 0) {
    liveHistoryFetches.set(key, left);
    return;
  }
  liveHistoryFetches.delete(key);
  // Each wake() deletes its own entry, which a Set iterator allows; nothing it does adds one.
  for (const waiter of historyIdleWaiters) if (waiter.channelId === key) waiter.wake();
}

/**
 * Resolves once no history fetch is live for `key` (at once when none is), or when
 * `signal` aborts. A refetch that superseded the one that just ended may already have
 * begun by the time this wakes, so it looks again rather than trusting the wake. The
 * signal matters because the fetch it waits on may belong to another instance: its
 * owner leaving must not keep this jump waiting on a request that is not its own.
 */
async function historyIdle(key: string | null, signal?: AbortSignal): Promise<void> {
  if (!key) return;
  while (historyFetchesFor(key) > 0 && !signal?.aborted) {
    await new Promise<void>((resolve) => {
      const waiter: { channelId: string; wake: () => void } = {
        channelId: key,
        wake: () => {
          historyIdleWaiters.delete(waiter);
          signal?.removeEventListener('abort', waiter.wake);
          resolve();
        },
      };
      historyIdleWaiters.add(waiter);
      signal?.addEventListener('abort', waiter.wake, { once: true });
    });
  }
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
      (message.status === 'pending' ||
        message.status === 'sent' ||
        (message.status === 'failed' && message.id === message.clientMessageId));
    const arrivedDuringRequest = !idsAtRequestStart.has(message.id);
    return isOptimistic || arrivedDuringRequest;
  });

  return { fetched: reconciledFetched, preserved, needsAuthoritativeRefetch };
}

/**
 * Decrypt a single ciphertext with the key epoch it is labelled with. Epoch 1
 * is an epoch like any other: after a member leaves, the current key is epoch
 * 2, and treating 1 as "current" loses the pre-leave history. Only an
 * unlabelled row uses the current key.
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
  if (keyVersion) {
    const vKey = versionedKeys.get(keyVersion);
    // A row labelled 1 that its own key cannot open may predate #2832. The
    // service owns that legacy fallback, so hand the row over rather than fail.
    plaintext = vKey
      ? await e2eeService.decryptWithKey(ciphertext, vKey, operationGuard).catch((err) => {
          // A superseded operation is a fence, never a miss to retry.
          operationGuard.assertCurrent();
          if (keyVersion !== 1) throw err;
          return e2eeService.decryptForChannelWithVersion(channelId, ciphertext, 1);
        })
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
  // Only validated empty-content metadata may bypass message authentication.
  const messages = rawMsgs.map((m) => {
    if (!m.type || m.type === 'user') return m;
    if (plaintextMetadataSchema.safeParse(m).success) {
      if (m.type === 'call_event' && m.call_event_payload) {
        return {
          ...m,
          call_event_payload: {
            ...m.call_event_payload,
            participant_user_ids: m.call_event_payload.participant_user_ids ?? undefined,
          },
        };
      }
      return m;
    }
    return {
      ...m,
      type: 'user',
      call_event_payload: undefined,
      expiration_event_payload: undefined,
    };
  });
  // Fail-closed: if E2EE isn't initialized, blank content rather than
  // leaking ciphertext. Mark as pendingKeys so the UI shows the correct placeholder.
  if (!e2eeService.isInitialized) {
    return messages.map((m) => {
      if (m.type === 'call_event' || m.type === 'expiration_event') {
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
    // A superseded operation is a fence, never a miss: stop before the
    // versioned prefetch runs under the new key generation. A plain miss
    // leaves individual messages to show pending/failed state.
    operationGuard.assertCurrent();
  }

  // Pre-fetch each labelled key version ONCE to avoid N parallel
  // getChannelKeyByVersion() calls. The current epoch is a cache hit there.
  const versionedKeys = new Map<number, CryptoKey>();
  const uniqueVersions = new Set<number>();
  for (const m of messages) {
    if (m.key_version) {
      uniqueVersions.add(m.key_version);
    }
    const rt = m.replied_to;
    if (rt?.key_version) {
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
    messages.map(async (m) => {
      if (m.type === 'call_event' || m.type === 'expiration_event') {
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

class HistoryAccessDeniedError extends Error {}

function clearDeniedHistory(err: unknown, scopeId: string): void {
  if (!(err instanceof HistoryAccessDeniedError)) return;
  recordScopePurge(scopeId);
  e2eeService.revokeChannelAccess(scopeId);
  removeScope(scopeId);
  useChatStore.getState().clearMessages(scopeId);
  useDMStore.getState().bumpConversation(scopeId, null);
}

async function readMessagePage(
  res: Response,
  type: 'channel' | 'dm',
  channelId: string,
  fallbackError: string
): Promise<MessageWithStatus[]> {
  if (!res.ok) {
    const data = await safeJson<{ error?: string; code?: string }>(res);
    // Only the history handler's refusal proves access loss; attestation also returns 403.
    const denied =
      type === 'dm'
        ? data.error === 'Not a participant'
        : data.error === "Not a member of this channel's server" ||
          data.error === 'Insufficient permissions';
    if (res.status === 403 && data.code === undefined && denied) {
      throw new HistoryAccessDeniedError(data.error);
    }
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
): boolean {
  if (isPendingKeyError(err)) {
    retry();
    return true;
  }
  setError(err instanceof Error ? err.message : fallbackError);
  return false;
}

async function loadOlderMessagesAttempt({
  type,
  channelId,
  limit,
  before,
  attempt,
  maxAttempts,
  isCurrent,
  historyFetchInFlight,
  prependMessages,
  setHasMore,
}: {
  type: 'channel' | 'dm';
  channelId: string;
  limit: number;
  before: string;
  attempt: number;
  maxAttempts: number;
  isCurrent: () => boolean;
  historyFetchInFlight: () => boolean;
  prependMessages: (channelId: string, messages: MessageWithStatus[]) => void;
  setHasMore: (hasMore: boolean) => void;
}): Promise<'retry' | 'done' | 'stale'> {
  const attemptMessages = useChatStore.getState().messagesByChannel.get(channelId) || [];
  const idsAtRequestStart = new Set(attemptMessages.map((message) => message.id));
  const invalidatedDuringRequest = new Set<string>();
  const unsubscribeInvalidations = subscribeSearchResultInvalidations((messageId) => {
    invalidatedDuringRequest.add(messageId);
  });
  const operationGuard = e2eeService.createChannelOperationGuard(channelId);

  try {
    const res = await apiFetch(buildEndpoint(type, channelId, limit, before));

    // Channel, account or purge generation changed while the request was in
    // flight — discard results.
    if (!isCurrent()) return 'stale';

    const rawMsgs = await readMessagePage(res, type, channelId, 'Failed to load more messages');
    const msgs = await decryptMessages(channelId, rawMsgs, operationGuard);
    operationGuard.assertCurrent();

    if (!isCurrent()) return 'stale';

    const currentMessages = useChatStore.getState().messagesByChannel.get(channelId) || [];
    // P7: publish only onto the head this page was asked for, and never while a history refetch runs.
    // No await may sit between this check and `prependMessages` below.
    if (currentMessages[0]?.id !== before || historyFetchInFlight()) return 'stale';
    const reconciled = reconcileFetchedMessages(
      msgs,
      currentMessages,
      idsAtRequestStart,
      invalidatedDuringRequest
    );
    if (reconciled.needsAuthoritativeRefetch) {
      if (attempt + 1 < maxAttempts) return 'retry';
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
 * Why a page that returned `stale` published nothing (#3894): the user left
 * or the account changed (`cancelled`), a purge landed (`failed`; the refetch
 * the purge queued is the truth), or the head moved or a history refetch ran
 * (`interrupted`: run again from the new head once history is ready).
 */
function stalePageStatus(sameChannelAndAuth: boolean, purged: boolean): PageResult['status'] {
  if (!sameChannelAndAuth) return 'cancelled';
  if (purged) return 'failed';
  return 'interrupted';
}

/**
 * The status of a page whose attempt threw. A scroll page (`reportError`)
 * keeps its handling: a pending key refetches, anything else sets `error`.
 * A jump page never sets `error`, because it feeds the owner's history banner.
 */
function thrownPageStatus(
  err: unknown,
  page: {
    sameChannelAndAuth: boolean;
    purged: boolean;
    reportError: boolean;
    retry: () => void;
    setError: (error: string) => void;
  }
): PageResult['status'] {
  if (!page.sameChannelAndAuth || isAbortError(err)) return 'cancelled';
  if (page.reportError) {
    const retried = handleFetchError(
      err,
      'Failed to load more messages',
      page.retry,
      page.setError
    );
    return retried ? 'interrupted' : 'failed';
  }
  if (page.purged) return 'failed';
  if (isPendingKeyError(err)) {
    // Rotation: refetch from a fresh first page; the jump resumes in budget.
    page.retry();
    return 'interrupted';
  }
  return 'failed';
}

/**
 * Loads, decrypts, reconciles, and paginates messages for a channel.
 *
 * @param channelId - The channel to load, or `null` when no channel is active.
 * @param options - Message type, page size, and optional completion callback.
 * @returns The channel messages, loading and pagination state, error state, the
 *   older-message loader, and `loadUntil`, which pages older history until a
 *   message is in the store.
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
  // Whether older history remains, per channel. A flag written for the channel the
  // chat left says nothing about the one it entered, so a channel whose own fetch
  // never answered has no entry and is read as unknown, never as "history ends".
  const [hasMoreByChannel, setHasMoreByChannel] = useState<ReadonlyMap<string, boolean>>(
    () => new Map()
  );
  // Mirrors the map for callbacks that must not close over a render (#3894).
  const hasMoreRef = useRef(new Map<string, boolean>());
  const updateHasMore = useCallback((key: string, value: boolean) => {
    if (hasMoreRef.current.get(key) === value) return;
    hasMoreRef.current.set(key, value);
    setHasMoreByChannel(new Map(hasMoreRef.current));
  }, []);
  const hasMore = channelId ? (hasMoreByChannel.get(channelId) ?? false) : false;
  // The older page running now, so a second caller joins it (single flight).
  const inFlightPageRef = useRef<{
    channelId: string | null;
    visit: number;
    promise: Promise<PageResult>;
  } | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [historySettledForChannelId, setHistorySettledForChannelId] = useState<string | null>(null);
  const [fetchTrigger, setFetchTrigger] = useState(0);
  const retryFetch = useCallback(() => {
    setHistorySettledForChannelId(null);
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
  // A visit token, bumped on every channel change. Comparing `channelIdRef` passes again once the
  // chat has gone X -> Y -> X, so a page asked for before the round trip would publish rows that
  // were deleted meanwhile: the purge lane that clears a scope bumps only the hook mounted on
  // it, and a deletion made while away sends no event at all. The token never repeats.
  const visitRef = useRef({ channelId, visit: 0 });
  if (visitRef.current.channelId !== channelId) {
    visitRef.current = { channelId, visit: visitRef.current.visit + 1 };
  }
  // This instance's own live history fetches. `isLoading` is per instance while the history
  // fence is per channel, so only a fetch this instance runs may hold its `isLoading` up.
  const ownLiveFetchesRef = useRef(0);
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
        retryFetch();
      }
    };
    globalThis.addEventListener('e2ee-key-delivered', handler);
    return () => globalThis.removeEventListener('e2ee-key-delivered', handler);
  }, [channelId, retryFetch]);

  // #2329: after any reconnect the WebSocket replays only subscriptions
  // + a presence snapshot, so messages sent during the outage are never
  // redelivered. useConnectionRecovery dispatches `connection-recovered` after
  // a grace reconnect or once longer recovery's session-guarded hydration completes;
  // re-fetch the mounted channel so the gap
  // is backfilled. The fetch effect's own aborted/channel/E2EE-operation guards
  // keep this session-safe.
  useEffect(() => {
    if (!channelId) return;
    const handler = () => retryFetch();
    globalThis.addEventListener('connection-recovered', handler);
    return () => globalThis.removeEventListener('connection-recovered', handler);
  }, [channelId, retryFetch]);

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
      retryFetch();
    };
    globalThis.addEventListener('messages-purged', handler);
    return () => globalThis.removeEventListener('messages-purged', handler);
  }, [channelId, retryFetch]);

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
        retryFetch();
      } catch {
        // Still pending — will retry
      }
    }, 5000);

    return () => clearInterval(interval);
  }, [channelId, hasPendingKeys, retryFetch]);

  // Fetch message history when channel changes or keys are delivered
  useEffect(() => {
    // Reset only when the active scope changes; same-key background retries
    // intentionally preserve readiness after the initial attempt settles.
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- scope fence must invalidate stale readiness before a new fetch can settle
    setHistorySettledForChannelId((settledChannelId) =>
      settledChannelId === channelId ? settledChannelId : null
    );
    if (!channelId) return;

    let aborted = false;
    // This fetch's place in the per-channel count, given back exactly once: by the
    // cleanup that abandons it or by its own end, whichever comes first. An
    // abandoned request may stay on the wire for a long time, but every path
    // below checks `aborted` before publishing, so it can publish nothing and
    // must not hold up paging for the replacement that did.
    let counted = false;
    const retire = () => {
      if (!counted) return;
      counted = false;
      ownLiveFetchesRef.current -= 1;
      historyFetchEnded(channelId);
    };

    const fetchMessages = async () => {
      let retrying = false;
      setIsLoading(true);
      setError(null);
      const idsAtRequestStart = new Set(
        (useChatStore.getState().messagesByChannel.get(channelId) || []).map(
          (message) => message.id
        )
      );
      const invalidatedDuringRequest = new Set<string>();
      const purgeGenerationAtRequestStart = currentPurgeGeneration(channelId);
      const lifecycle = captureAuthLifecycle();
      const unsubscribeInvalidations = subscribeSearchResultInvalidations((messageId) => {
        invalidatedDuringRequest.add(messageId);
      });

      counted = true;
      ownLiveFetchesRef.current += 1;
      historyFetchBegan(channelId);
      try {
        const operationGuard = e2eeService.createChannelOperationGuard(channelId);
        const res = await apiFetch(buildEndpoint(type, channelId, limit));
        if (aborted) return;

        const rawMsgs = await readMessagePage(res, type, channelId, 'Failed to load messages');
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
        if (currentPurgeGeneration(channelId) !== purgeGenerationAtRequestStart) {
          retrying = true;
          return;
        }
        unsubscribeInvalidations();
        indexDecryptedMessages(channelId, merged);
        setMessages(channelId, merged);
        updateHasMore(channelId, rawMsgs.length === limit);
        onFetchCompleteRef.current?.();
        if (reconciled.needsAuthoritativeRefetch) {
          // An edit/delete for an unloaded row can only tombstone this stale
          // snapshot. Fetch once more so the authoritative post-event state
          // (updated row or confirmed absence) is not missing until remount.
          retrying = true;
          retryFetch();
        }
      } catch (err) {
        if (!aborted && isSameAuthLifecycle(lifecycle)) {
          if (currentPurgeGeneration(channelId) === purgeGenerationAtRequestStart) {
            clearDeniedHistory(err, channelId);
          }
          // A key rotation invalidated this batch after decryption. Start a
          // fresh request with the new generation; access revocations use a
          // terminal NOT_MEMBER fence and never enter this retry path.
          retrying = handleFetchError(err, 'Failed to load messages', retryFetch, setError);
        }
      } finally {
        retire();
        unsubscribeInvalidations();
        if (!aborted) {
          setIsLoading(false);
          if (!retrying) setHistorySettledForChannelId(channelId);
        }
      }
    };

    fetchMessages();

    return () => {
      aborted = true;
      retire();
    };
  }, [channelId, setMessages, fetchTrigger, retryFetch, type, limit, updateHasMore]);

  // One older page (#3894). Scrolling and `loadUntil` both come through here,
  // so both get the channel, account, purge and publication fences. Never
  // rejects.
  const runOlderPage = useCallback(
    async (channelKey: string | null, opts: PageOptions): Promise<PageResult> => {
      // A history fetch is replacing this channel's rows; a page now would be dropped.
      if (historyFetchesFor(channelKey) > 0) return { status: 'interrupted', requests: 0 };
      if (!channelKey) return { status: 'done', requests: 0 };
      const head = useChatStore.getState().messagesByChannel.get(channelKey)?.[0];
      if (hasMoreRef.current.get(channelKey) !== true || !head) {
        return { status: 'done', requests: 0 };
      }
      const before = head.id; // re-derived per page: never reuse a cursor across pages
      // A page in flight when a purge lands carries deleted plaintext, so the
      // purge generation fences pagination exactly as it fences the initial
      // fetch above. The auth snapshot keeps account A's page out of account B.
      const purgeGeneration = currentPurgeGeneration(channelKey);
      const lifecycle = captureAuthLifecycle();
      const visit = visitRef.current.visit;
      const sameVisit = () =>
        channelIdRef.current === channelKey && visitRef.current.visit === visit;
      const sameChannelAndAuth = () => sameVisit() && isSameAuthLifecycle(lifecycle);
      const purged = () => currentPurgeGeneration(channelKey) !== purgeGeneration;
      if (opts.reportError) setError(null);
      setIsLoading(true);
      let requests = 0;
      try {
        for (let attempt = 0; attempt < opts.maxAttempts; attempt += 1) {
          requests += 1;
          const outcome = await loadOlderMessagesAttempt({
            type,
            channelId: channelKey,
            limit: opts.pageLimit,
            before,
            attempt,
            maxAttempts: opts.maxAttempts,
            isCurrent: () => sameChannelAndAuth() && !purged(),
            historyFetchInFlight: () => historyFetchesFor(channelKey) > 0,
            prependMessages,
            setHasMore: (value) => updateHasMore(channelKey, value),
          });
          if (outcome === 'retry') continue;
          if (outcome === 'done') return { status: 'done', requests };
          return { status: stalePageStatus(sameChannelAndAuth(), purged()), requests };
        }
        return { status: 'done', requests };
      } catch (err) {
        if (sameChannelAndAuth() && !purged()) clearDeniedHistory(err, channelKey);
        // Reconcile from a fresh initial snapshot after rotation. Retrying
        // the same `before` cursor could be stale after live edits/deletes.
        const status = thrownPageStatus(err, {
          sameChannelAndAuth: sameChannelAndAuth(),
          purged: purged(),
          reportError: opts.reportError,
          retry: retryFetch,
          setError,
        });
        return { status, requests };
      } finally {
        // The initial fetch shares `isLoading`: a page ending mid-refetch must leave it for the refetch to clear.
        // Only this instance's own refetch counts: another instance's fetch on the same channel
        // never clears this flag, so it must not hold it up.
        if (sameVisit() && ownLiveFetchesRef.current === 0) {
          setIsLoading(false);
        }
      }
    },
    [type, prependMessages, retryFetch, updateHasMore]
  );

  // Single flight: a page already running for this channel is joined, so the
  // list's own load triggers and a jump merge instead of racing one cursor.
  const loadOlderPage = useCallback(
    (opts: PageOptions): Promise<PageResult> => {
      const channelKey = channelIdRef.current;
      const visit = visitRef.current.visit;
      const running = inFlightPageRef.current;
      // A page asked for on an earlier visit to this channel will be dropped; never join it.
      if (running && running.channelId === channelKey && running.visit === visit) {
        return running.promise;
      }
      const entry = { channelId: channelKey, visit, promise: runOlderPage(channelKey, opts) };
      inFlightPageRef.current = entry;
      const release = () => {
        if (inFlightPageRef.current === entry) inFlightPageRef.current = null;
      };
      void entry.promise.then(release, release);
      return entry.promise;
    },
    [runOlderPage]
  );

  // A DM asks for its route's fixed 50 so `hasMore` compares with what was asked.
  const jumpPageLimit = type === 'channel' ? JUMP_PAGE_LIMIT : limit;

  /**
   * Pages older history until `messageId` is in the store, spending at most
   * `maxRequests` HTTP attempts it starts (a joined page counts in full). The
   * cursor, channel and account are re-read every turn. Returns at once on
   * `interrupted`; the caller runs it again once history is ready. `signal`
   * ends it as `cancelled` before its next page, so a replaced or abandoned
   * jump stops spending the list route's budget; a page already running is
   * shared with the list and finishes.
   */
  const loadUntil = useCallback(
    async (
      messageId: string,
      maxRequests: number,
      signal?: AbortSignal
    ): Promise<LoadUntilResult> => {
      const channelKey = channelIdRef.current;
      const visit = visitRef.current.visit;
      const lifecycle = captureAuthLifecycle();
      const holds = () =>
        (useChatStore.getState().messagesByChannel.get(channelKey ?? '') ?? []).some(
          (m) => m.id === messageId
        );
      let requests = 0;
      for (;;) {
        // Wait out a history fetch running for this channel, in this instance or in
        // another over the same rows; resolves at once when none does, and when
        // `signal` aborts. One that was abandoned (StrictMode's first, a superseded
        // refetch) no longer counts, however long its request stays out.
        await historyIdle(channelKey, signal);
        if (
          signal?.aborted ||
          !channelKey ||
          channelIdRef.current !== channelKey ||
          visitRef.current.visit !== visit ||
          !isSameAuthLifecycle(lifecycle)
        ) {
          return { status: 'cancelled', requests };
        }
        if (holds()) return { status: 'found', requests };
        const hasOlder = hasMoreRef.current.get(channelKey);
        // No entry: this channel's own fetch never answered, so whether older history
        // exists is unknown. That is a failed load, not a verdict that the message is gone.
        if (hasOlder === undefined) return { status: 'failed', requests };
        if (!hasOlder) return { status: 'exhausted', requests };
        if (requests >= maxRequests) return { status: 'too-far', requests };
        const page = await loadOlderPage({
          reportError: false,
          pageLimit: jumpPageLimit,
          maxAttempts: Math.min(MAX_PAGINATION_RECONCILIATION_ATTEMPTS, maxRequests - requests),
        });
        requests += page.requests;
        if (page.status !== 'done') return { status: page.status, requests };
        if (page.requests === 0) return { status: 'exhausted', requests }; // nothing to page from
      }
    },
    [loadOlderPage, jumpPageLimit]
  );

  // Load older messages (pagination). Unchanged for the list: errors still
  // reach `error`, and a pending key still refetches.
  const handleLoadMore = useCallback(async () => {
    await loadOlderPage({
      reportError: true,
      pageLimit: limit,
      maxAttempts: MAX_PAGINATION_RECONCILIATION_ATTEMPTS,
    });
  }, [loadOlderPage, limit]);

  return {
    messages,
    isLoading,
    hasMore,
    error,
    handleLoadMore,
    loadUntil,
    isHistoryReady: historySettledForChannelId === channelId,
  };
}
