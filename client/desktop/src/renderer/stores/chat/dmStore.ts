import { create } from 'zustand';
import { persist, devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import { apiFetch } from '../../services/system/apiClient';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { E2EEKeyUnavailableError, isPendingKeyError } from '../../services/e2ee/e2eeErrors';
import { removeScope } from '../../services/messaging/searchService';
import { errorMessage } from '../../utils/runtime/redactError';
import { useChatStore } from './chatStore';
import type { CallEventPayload } from '../../types/chat';

function purgeConversationAccessState(conversationId: string): void {
  e2eeService.revokeChannelAccess(conversationId);
  useChatStore.getState().clearMessages(conversationId);
  removeScope(conversationId);
}

interface ConversationFetchJournal {
  baselineIds: Set<string>;
  removedIds: Set<string>;
  changedFieldsById: Map<string, Set<MutableConversationField>>;
  cleared: boolean;
}

const conversationFetchJournals = new Set<ConversationFetchJournal>();
let conversationRefetchQueued = false;
const hasLiveConversationFetch = () =>
  Array.from(conversationFetchJournals).some((journal) => !journal.cleared);

function markConversationFetchMutation(
  conversationId: string,
  ...fields: MutableConversationField[]
): void {
  for (const journal of conversationFetchJournals) {
    if (journal.cleared) continue;
    let changedFields = journal.changedFieldsById.get(conversationId);
    if (!changedFields) {
      changedFields = new Set();
      journal.changedFieldsById.set(conversationId, changedFields);
    }
    for (const field of fields) changedFields.add(field);
  }
}

export interface DMParticipant {
  userId: string;
  username: string;
  displayName?: string;
  avatarUrl?: string;
  colorScheme?: string;
  status?: string;
  role?: 'admin' | 'member';
}

export interface DMLastMessage {
  content: string;
  userId: string;
  username: string;
  createdAt: string;
  /** Server-authored metadata for plaintext voice-call history rows. */
  type?: string;
  callEventPayload?: CallEventPayload;
  /** In-memory preview for optimistic local sends; dmStore only persists activeConversationId. */
  plaintextPreview?: string;
  /** E2EE GIF slug metadata for local optimistic previews and server-enriched summaries. */
  gifSlug?: string;
  /** Media type label when message has no text content (e.g. 'photo', 'video', 'file'). */
  attachmentType?: string;
}

export interface DMConversation {
  id: string;
  isGroup: boolean;
  isPersonal: boolean;
  name: string | null;
  iconUrl?: string;
  createdBy?: string;
  participants: DMParticipant[];
  lastMessage: DMLastMessage | null;
  unreadCount: number;
  createdAt: string;
}

type MutableConversationField = Exclude<keyof DMConversation, 'id'>;

function markConversationFetchUpdate(
  conversationId: string,
  updates: Partial<DMConversation>
): void {
  const fields = (Object.keys(updates) as (keyof DMConversation)[]).filter(
    (field): field is MutableConversationField => field !== 'id'
  );
  markConversationFetchMutation(conversationId, ...fields);
}

function sortConversationsByActivity(conversations: DMConversation[]): DMConversation[] {
  return conversations.sort((a, b) => {
    const aTime = a.lastMessage?.createdAt || a.createdAt;
    const bTime = b.lastMessage?.createdAt || b.createdAt;
    return new Date(bTime).getTime() - new Date(aTime).getTime();
  });
}

function updateParticipantProfileInConversation(
  conversation: DMConversation,
  userId: string,
  updates: Partial<Omit<DMParticipant, 'userId'>>
): DMConversation {
  if (!conversation.participants.some((participant) => participant.userId === userId)) {
    return conversation;
  }
  markConversationFetchMutation(conversation.id, 'participants');
  return {
    ...conversation,
    participants: conversation.participants.map((participant) =>
      participant.userId === userId ? { ...participant, ...updates } : participant
    ),
  };
}

/**
 * Fetch public keys for a list of user IDs.
 *
 * `keys` maps userId → publicKey and `versions` maps userId → that key's
 * `key_version`. The versions are what activate #2420's recipient-freshness
 * guard: the server runs `recipientKeyFresh` only for recipients named in
 * `wrapped_key_versions`, so omitting them makes every insert take the
 * fail-open branch and a wrap against a since-rotated identity key is stored
 * with no self-heal row enqueued. `GET /public-key` already returns the field
 * and this function used to discard it.
 *
 * `missing` names the users whose fetch failed. A partial map is otherwise
 * indistinguishable from a complete one, so a participant whose request 500'd
 * silently gets no wrapped key and the distribution still reports success.
 */
interface ParticipantPublicKeys {
  keys: Map<string, string>;
  versions: Map<string, number>;
  missing: string[];
}

async function fetchParticipantPublicKeys(userIds: string[]): Promise<ParticipantPublicKeys> {
  const results = await Promise.allSettled(
    userIds.map(async (userId) => {
      const pkRes = await apiFetch(`/api/v1/users/${userId}/public-key`);
      if (!pkRes.ok) return { userId, publicKey: null };
      const pkData = await pkRes.json();
      return {
        userId,
        publicKey: (pkData.public_key as string | undefined) ?? null,
        keyVersion:
          typeof pkData.key_version === 'number' && Number.isSafeInteger(pkData.key_version)
            ? (pkData.key_version as number)
            : undefined,
      };
    })
  );
  const keys = new Map<string, string>();
  const versions = new Map<string, number>();
  const missing: string[] = [];
  for (const [i, r] of results.entries()) {
    if (r.status === 'fulfilled' && r.value.publicKey) {
      keys.set(r.value.userId, r.value.publicKey);
      if (r.value.keyVersion !== undefined) versions.set(r.value.userId, r.value.keyVersion);
    } else {
      missing.push(r.status === 'fulfilled' ? r.value.userId : userIds[i]);
    }
  }
  return { keys, versions, missing };
}

/**
 * Two limiters answer on POST /e2ee/keys/:context_id and their scopes differ,
 * so one deadline cannot serve both (#1218).
 *
 * The per-conversation limiter is keyed by conversation, and a shared scalar
 * for it would suppress distribution for unrelated DMs. The route's per-user
 * limiter is keyed on the route PATTERN, so its budget is spent across every
 * context at once — recording that one per conversation under-suppresses: each
 * further DM opened inside the same closed window repeats the public-key
 * fetches, a fresh CSK, an RSA-OAEP wrap per participant and the POST, only to
 * be refused again. Since the suppression check runs before any of that work,
 * a correctly-scoped deadline is what avoids it.
 *
 * `X-RateLimit-Limit` is the only signal that distinguishes them.
 */
const distributionRateLimitedUntil = new Map<string, number>();
let distributionRateLimitedUntilAll = 0;

/**
 * Bumped by `clearDMs()`. Clearing the two deadlines above cannot stop a
 * distribution that is already awaiting its response: `apiFetch` returns any
 * non-401 to its caller without consulting the auth lifecycle, so a 429 for the
 * account that just logged out arrives AFTER the clear and its continuation
 * repopulates what the clear emptied.
 *
 * That matters because a group DM's id is shared by its participants, so the
 * successor account can legitimately open the very conversation the deadline
 * was recorded under and be suppressed by a window it never spent. Capturing
 * this generation on entry and re-checking it before the write is what makes
 * the clear authoritative.
 */
let distributionLifecycleGeneration = 0;

/**
 * Mirrors `dmKeyDistributeLimit` in `internal/channels/handlers.go`. Compared as
 * a string because that is what the header carries. A drift here is safe in one
 * direction only: an unrecognised limit is treated as route-wide, which
 * over-suppresses rather than letting a spent budget through.
 */
const DM_KEY_DISTRIBUTE_LIMIT_HEADER = '40';

/**
 * The moment before which distribution for `convId` is suppressed — the later
 * of its own deadline and the route-wide one.
 *
 * Exposed to the callers deliberately. The check inside distributeChannelKeys
 * runs AFTER they have already fetched every participant's public key, and that
 * fetch has its own separate budget (RateLimitByUser 30/min on
 * GET /users/:id/public-key). A 10-member group therefore spent 10 of those 30
 * on each suppressed attempt, so three suppressed reopens exhausted the budget
 * that key wrapping needs once the distribution window reopens — a suppression
 * that made the recovery it was protecting harder.
 */
function distributionSuppressedUntil(convId: string): number {
  return Math.max(distributionRateLimitedUntil.get(convId) ?? 0, distributionRateLimitedUntilAll);
}

/**
 * Record the suppression deadline a 429 implies — if it is one this API issued,
 * and if the account that made the request is still the current one.
 *
 * Written as guard clauses rather than nested conditions because each `return`
 * is a distinct reason to record nothing, and stating them separately is what
 * keeps the two scopes below readable.
 */
function recordDistributionRateLimit(convId: string, res: Response, generation: number): void {
  // Both server responders set X-RateLimit-Limit. The API is always
  // Cloudflare-proxied, so an edge-issued 429 carrying neither header is
  // realistic — and self-blocking on an error we never diagnosed is worse than
  // letting the throw propagate.
  const limitHeader = res.headers.get('X-RateLimit-Limit');
  if (limitHeader === null) return;

  // A teardown while the request was in flight makes this the previous
  // account's deadline; the successor must not inherit a window it never spent.
  if (generation !== distributionLifecycleGeneration) return;

  // Same Retry-After read as e2eeService's throwKeyFetchError.
  //
  // Zero is a VALID value meaning "the window has already rolled, retry now",
  // and it is emittable rather than hypothetical: both server responders build
  // the header with int(ttl.Seconds()), which truncates a sub-second remaining
  // TTL to 0. Rejecting it would install a 60s local deadline the server never
  // asked for and leave the conversation without its key for that minute. The
  // fallback is therefore only for a missing, non-numeric or negative header.
  const retryAfter = Number.parseInt(res.headers.get('Retry-After') || '', 10);
  const seconds = Number.isFinite(retryAfter) && retryAfter >= 0 ? retryAfter : 60;
  const deadline = Date.now() + seconds * 1000;

  if (limitHeader === DM_KEY_DISTRIBUTE_LIMIT_HEADER) {
    distributionRateLimitedUntil.set(convId, deadline);
    return;
  }
  // The route's per-user budget is spent across every context at once, so
  // scoping its deadline to one conversation would leave the next DM opened in
  // the same window to redo the whole wrap-and-POST for another 429.
  distributionRateLimitedUntilAll = deadline;
}

/**
 * Distribute wrapped E2EE channel keys to the server for the given conversation.
 *
 * Throws on failure, where it previously folded every non-ok response into a
 * console.error and returned normally. Be precise about what that buys: both
 * callers already wrap this in `try/catch { console.error }` and do nothing
 * after it, so the throw changes no control flow today — the behavioural
 * mechanism is entirely the per-conversation `Retry-After` deadline, which
 * stops a rate-limited conversation from re-POSTing into the same closed
 * window. The throw is what makes the deadline reachable and what will carry
 * the failure if a caller ever needs to act on it.
 *
 * Recovery needs no new UI: the key is still missing, so `getChannelKey`
 * answers NO_KEY_YET + pending, `throwKeyFetchError` fires `requestRewrap`,
 * and a pending row is enrolled for a peer to fulfil. That last step does NOT
 * apply to `ensurePersonalThreadKey`, whose conversation has exactly one
 * participant and therefore no peer — there, recovery is the next conversation
 * load after the deadline expires.
 */
async function distributeChannelKeys(
  convId: string,
  memberKeys: Map<string, string>,
  memberKeyVersions: Map<string, number> | undefined,
  /**
   * The lifecycle generation captured when the ENCLOSING operation began, not
   * when this function was entered. Capturing it here would be too late: the
   * caller awaits `getChannelKey` and `fetchParticipantPublicKeys` first, and a
   * `clearDMs()` during either of those already bumped the counter — so a local
   * capture would read the SUCCESSOR's generation, match at the write, and let
   * the previous account's 429 install a deadline the new account inherits.
   * Required rather than defaulted so every call site has to decide.
   */
  generation: number
): Promise<void> {
  // Cheapest and most decisive check first: if the account already changed
  // while the caller was fetching keys, this whole operation belongs to a dead
  // session. Refusing here also avoids a pointless CSK generation and an
  // RSA-OAEP wrap per participant for a request that must not be recorded.
  if (generation !== distributionLifecycleGeneration) {
    throw new Error(`DM key distribution abandoned for ${convId}: account changed`);
  }

  if (Date.now() < distributionSuppressedUntil(convId)) {
    // Distinct from the server-driven message below on purpose: identical
    // strings made the two indistinguishable in the log, so a suppression
    // could not be told from the 429 that caused it.
    throw new Error(`DM key distribution suppressed by local deadline for ${convId}`);
  }
  if (distributionRateLimitedUntilAll !== 0) distributionRateLimitedUntilAll = 0;
  // Evict the expired entry here rather than only on the next success. A
  // conversation rate-limited once and then abandoned would otherwise keep its
  // entry for the renderer's lifetime, and the map's boundedness would rest on
  // the Date.now() guard making stale entries harmless rather than on there
  // being none.
  distributionRateLimitedUntil.delete(convId);

  const wrappedKeys = await e2eeService.createChannelKeys(memberKeys);
  const body: {
    wrapped_keys: Record<string, string>;
    wrapped_key_versions?: Record<string, number>;
  } = { wrapped_keys: Object.fromEntries(wrappedKeys) };
  if (memberKeyVersions && memberKeyVersions.size > 0) {
    body.wrapped_key_versions = Object.fromEntries(memberKeyVersions);
  }
  // Recheck after createChannelKeys, which awaits an RSA-OAEP wrap per
  // recipient and is therefore a wide window. apiFetch attaches whatever access
  // token is current at call time, so a teardown during that wrap would send
  // the PREVIOUS account's participant snapshot under the SUCCESSOR's
  // credentials — and in a group DM both accounts are participants, so the
  // server would accept it and bill the successor's budget for it.
  if (generation !== distributionLifecycleGeneration) {
    throw new Error(`DM key distribution abandoned for ${convId}: account changed`);
  }

  const distRes = await apiFetch(`/api/v1/e2ee/keys/${convId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

  if (distRes.status === 429) {
    recordDistributionRateLimit(convId, distRes, generation);
    throw new Error(`DM key distribution rate limited for ${convId}`);
  }

  if (!distRes.ok) {
    throw new Error(`Failed to distribute E2EE key for DM: ${distRes.status}`);
  }
}

/**
 * Ensure E2EE channel key exists for a DM conversation.
 * If no key exists (E2EEKeyUnavailableError with pending=true), fetches
 * participant public keys and distributes a new key.
 */
async function ensureE2EEKey(conv: DMConversation): Promise<void> {
  if (!e2eeService.isInitialized) return;
  // Captured before the first await — see distributeChannelKeys' `generation`.
  const generation = distributionLifecycleGeneration;

  let needsKeyDistribution = false;
  try {
    await e2eeService.getChannelKey(conv.id);
  } catch (err) {
    needsKeyDistribution = isPendingKeyError(err);
  }

  if (!needsKeyDistribution) return;

  try {
    if (Date.now() < distributionSuppressedUntil(conv.id)) {
      // Bail before the public-key fetch, not after it — see
      // distributionSuppressedUntil for why that ordering is load-bearing.
      return;
    }
    const { keys, versions, missing } = await fetchParticipantPublicKeys(
      conv.participants.map((p) => p.userId)
    );
    if (missing.length > 0) {
      // A dropped participant gets no wrapped key and the distribution still
      // reports success, so without this the omission is unobservable.
      console.warn(
        `DM key distribution: no public key for ${missing.length} participant(s) in ${conv.id}`
      );
    }
    if (keys.size === 0) {
      console.error(`Failed to distribute E2EE key for DM ${conv.id}: no participant public keys`);
      return;
    }

    await distributeChannelKeys(conv.id, keys, versions, generation);
  } catch (err) {
    console.error('Failed to distribute E2EE key for DM:', errorMessage(err));
  }
}

/**
 * Ensure E2EE channel key exists for a personal thread DM conversation.
 * Unlike ensureE2EEKey, this checks for key existence (not the pending
 * E2EEKeyUnavailableError shape) and only distributes to the current user.
 */
async function ensurePersonalThreadKey(conv: DMConversation): Promise<void> {
  if (!e2eeService.isInitialized) return;
  // Captured before the first await — see distributeChannelKeys' `generation`.
  const generation = distributionLifecycleGeneration;

  let needsKeyDistribution = false;
  try {
    await e2eeService.getChannelKey(conv.id);
  } catch (err) {
    // Narrowed on the CODE, not on isPendingKeyError. That distinction is
    // load-bearing: a personal thread has one participant, so no peer can ever
    // service a pending row, and throwKeyFetchError defaults `pending` to
    // false — so isPendingKeyError is false for exactly the keyless case this
    // function exists to fix, and mirroring ensureE2EEKey here would stop
    // personal threads getting a first key at all.
    //
    // What the narrowing does close is the bare `catch {}` this replaces,
    // which let ANY failure — a 500, a dropped connection, a WebCrypto unwrap
    // error, NOT_MEMBER, REVOKED_EPOCH — admit a fresh key generation and
    // distribution POST. At an unchanged version the server drops that insert
    // via ON CONFLICT DO NOTHING and answers 200, so it never converged: it
    // just respent the conversation's budget on every open, undiagnosed.
    needsKeyDistribution = err instanceof E2EEKeyUnavailableError && err.code === 'NO_KEY_YET';
    if (!needsKeyDistribution) {
      console.error('Personal thread key check failed:', errorMessage(err));
    }
  }

  if (!needsKeyDistribution) return;

  try {
    const userStore = await import('../auth/userStore');
    const userId = userStore.useUserStore.getState().user?.id;
    if (!userId) return;

    if (Date.now() < distributionSuppressedUntil(conv.id)) {
      // Bail before the public-key fetch, not after it — see
      // distributionSuppressedUntil for why that ordering is load-bearing.
      return;
    }
    const { keys, versions } = await fetchParticipantPublicKeys([userId]);
    if (keys.size === 0) {
      console.error(
        `Failed to distribute E2EE key for personal thread ${conv.id}: own public key unavailable`
      );
      return;
    }

    await distributeChannelKeys(conv.id, keys, versions, generation);
  } catch (err) {
    console.error('Failed to distribute E2EE key for personal thread:', errorMessage(err));
  }
}

interface DMState {
  conversations: DMConversation[];
  activeConversationId: string | null;
  isLoading: boolean;
  error: string | null;

  // Removed in #1209: dmCallActive / dmCallConversationId / setDMCallActive
  // were never read by any component. DM call state lives on voiceStore
  // (isDMCall, dmConversationId, callState). Single source of truth.

  // API actions
  fetchConversations: () => Promise<void>;
  openDM: (userId: string) => Promise<DMConversation>;
  createGroupDM: (userIds: string[], name?: string) => Promise<DMConversation>;
  openPersonalThread: () => Promise<DMConversation>;
  setActiveConversation: (id: string | null) => void;

  // Real-time updates (called from WebSocket handlers)
  addConversation: (conv: DMConversation) => void;
  updateConversation: (id: string, updates: Partial<DMConversation>) => void;
  removeConversation: (id: string) => void;
  updateLastMessage: (convId: string, message: DMLastMessage) => void;
  /**
   * Bump a conversation to the top of the list and update its last-message preview.
   * Pass `null` to clear or roll back the stored `lastMessage` preview.
   * No-ops if the conversation isn't in state yet (covers initial-load races).
   */
  bumpConversation: (conversationId: string, message: DMLastMessage | null) => void;
  incrementUnread: (convId: string) => void;
  clearUnread: (convId: string) => void;

  updateParticipantProfile: (
    userId: string,
    updates: Partial<Omit<DMParticipant, 'userId'>>
  ) => void;

  clearDMs: () => void;

  // Group management actions
  addGroupMember: (conversationId: string, userId: string) => Promise<void>;
  removeGroupMember: (conversationId: string, userId: string) => Promise<void>;
  leaveGroup: (conversationId: string) => Promise<void>;
  updateMemberRole: (
    conversationId: string,
    userId: string,
    role: 'admin' | 'member'
  ) => Promise<void>;
  deleteGroup: (conversationId: string) => Promise<void>;
}

function queueConversationRefetchIfLoading(isLoading: boolean): boolean {
  if (!isLoading) return false;
  if (hasLiveConversationFetch()) conversationRefetchQueued = true;
  return true;
}

function reconcileConversationResponse(
  data: { conversations?: Record<string, unknown>[] },
  journal: ConversationFetchJournal,
  currentState: Pick<DMState, 'conversations' | 'activeConversationId'>
): Pick<DMState, 'conversations' | 'activeConversationId'> {
  const currentById = new Map(
    currentState.conversations.map((conversation) => [conversation.id, conversation])
  );
  const conversations = (data.conversations || [])
    .map((conversation) => mapConversation(conversation))
    .filter((conversation) => !journal.removedIds.has(conversation.id))
    .map((fetched) => {
      const current = currentById.get(fetched.id);
      if (!current) return fetched;
      const addedAfterFetchStarted = !journal.baselineIds.has(fetched.id);
      if (addedAfterFetchStarted) return current;

      const changedFields = journal.changedFieldsById.get(fetched.id);
      if (!changedFields) return fetched;
      return Array.from(changedFields).reduce<DMConversation>(
        (conversation, field) => ({ ...conversation, [field]: current[field] }),
        fetched
      );
    });
  const fetchedIds = new Set(conversations.map((conversation) => conversation.id));

  for (const conversation of currentState.conversations) {
    if (fetchedIds.has(conversation.id)) continue;
    if (!journal.baselineIds.has(conversation.id) && !journal.removedIds.has(conversation.id)) {
      conversations.push(conversation);
      continue;
    }
    purgeConversationAccessState(conversation.id);
  }

  const hasPostFetchAddition = currentState.conversations.some(
    (conversation) =>
      !journal.baselineIds.has(conversation.id) && !journal.removedIds.has(conversation.id)
  );
  const hasChangedLastMessage = Array.from(journal.changedFieldsById.values()).some((fields) =>
    fields.has('lastMessage')
  );
  if (hasChangedLastMessage || hasPostFetchAddition) {
    sortConversationsByActivity(conversations);
  }

  const activeConversationId =
    currentState.activeConversationId &&
    conversations.some((conversation) => conversation.id === currentState.activeConversationId)
      ? currentState.activeConversationId
      : null;

  return { conversations, activeConversationId };
}

function finishConversationFetch(journal: ConversationFetchJournal): {
  isLoading: boolean;
  shouldRefetch: boolean;
} {
  conversationFetchJournals.delete(journal);
  const isLoading = hasLiveConversationFetch();
  const shouldRefetch = !isLoading && conversationRefetchQueued;
  if (shouldRefetch) conversationRefetchQueued = false;
  return { isLoading, shouldRefetch };
}

export const useDMStore = wrapStore(
  create<DMState>()(
    devtools(
      persist(
        (set, get) => ({
          conversations: [],
          activeConversationId: null,
          isLoading: false,
          error: null,

          fetchConversations: async () => {
            if (queueConversationRefetchIfLoading(get().isLoading)) return;
            const journal: ConversationFetchJournal = {
              baselineIds: new Set(get().conversations.map((conversation) => conversation.id)),
              removedIds: new Set(),
              changedFieldsById: new Map(),
              cleared: false,
            };
            conversationFetchJournals.add(journal);
            set({ isLoading: true, error: null });

            try {
              const response = await apiFetch('/api/v1/dm/conversations');
              if (!response.ok) {
                const data = await response.json();
                throw new Error(data.error || 'Failed to load conversations');
              }

              const data = await response.json();
              if (journal.cleared) return;
              set(reconcileConversationResponse(data, journal, get()));
            } catch (error) {
              if (!journal.cleared) {
                set({
                  error: error instanceof Error ? error.message : 'Failed to load conversations',
                });
              }
            } finally {
              const { isLoading, shouldRefetch } = finishConversationFetch(journal);
              set({ isLoading });
              if (shouldRefetch) await get().fetchConversations();
            }
          },

          openDM: async (userId: string) => {
            const response = await apiFetch('/api/v1/dm/conversations', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ user_id: userId }),
            });

            if (!response.ok) {
              const data = await response.json();
              if (data.error === 'dm_disabled' || data.error === 'privacy_blocked') {
                throw new Error(
                  "This user isn't accepting DMs right now due to their privacy settings. Please try again later."
                );
              }
              throw new Error(data.error || 'Failed to open DM');
            }

            const data = await response.json();
            const conv = mapConversation(data.conversation);

            await ensureE2EEKey(conv);

            // Add to list if not already present
            set((state) => {
              const exists = state.conversations.some((c) => c.id === conv.id);
              return {
                conversations: exists ? state.conversations : [conv, ...state.conversations],
                activeConversationId: conv.id,
              };
            });

            return conv;
          },

          createGroupDM: async (userIds: string[], name?: string) => {
            const body: Record<string, unknown> = { user_ids: userIds };
            if (name) body.name = name;

            const response = await apiFetch('/api/v1/dm/conversations/group', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(body),
            });

            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to create group DM');
            }

            const data = await response.json();
            const conv = mapConversation(data.conversation);

            await ensureE2EEKey(conv);

            set((state) => ({
              conversations: [conv, ...state.conversations],
              activeConversationId: conv.id,
            }));

            return conv;
          },

          openPersonalThread: async () => {
            const response = await apiFetch('/api/v1/dm/conversations/personal', {
              method: 'POST',
            });

            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to open personal thread');
            }

            const data = await response.json();
            const conv = mapConversation(data.conversation);

            // Ensure E2EE channel key exists for encrypted personal threads.
            // Not gated on isNewlyCreated — covers key distribution failures,
            // post-reset recovery, and first-open when E2EE wasn't ready at creation.
            await ensurePersonalThreadKey(conv);

            set((state) => {
              const exists = state.conversations.some((c) => c.id === conv.id);
              return {
                conversations: exists ? state.conversations : [conv, ...state.conversations],
                activeConversationId: conv.id,
              };
            });

            return conv;
          },

          setActiveConversation: (id: string | null) => set({ activeConversationId: id }),

          addConversation: (conv: DMConversation) =>
            set((state) => {
              if (state.conversations.some((c) => c.id === conv.id)) return state;
              return { conversations: [conv, ...state.conversations] };
            }),

          updateConversation: (id: string, updates: Partial<DMConversation>) => {
            markConversationFetchUpdate(id, updates);
            set((state) => ({
              conversations: state.conversations.map((c) =>
                c.id === id ? { ...c, ...updates } : c
              ),
            }));
          },

          removeConversation: (id: string) => {
            for (const journal of conversationFetchJournals) journal.removedIds.add(id);
            // Fence pending decrypts first, then purge every plaintext-bearing
            // in-memory representation for this conversation.
            purgeConversationAccessState(id);
            set((state) => ({
              conversations: state.conversations.filter((c) => c.id !== id),
              activeConversationId:
                state.activeConversationId === id ? null : state.activeConversationId,
              isLoading: hasLiveConversationFetch(),
            }));
          },

          updateParticipantProfile: (
            userId: string,
            updates: Partial<Omit<DMParticipant, 'userId'>>
          ) =>
            set((state) => ({
              conversations: state.conversations.map((conversation) =>
                updateParticipantProfileInConversation(conversation, userId, updates)
              ),
            })),

          updateLastMessage: (convId: string, message: DMLastMessage) => {
            markConversationFetchMutation(convId, 'lastMessage');
            set((state) => {
              const updated = state.conversations.map((c) =>
                c.id === convId ? { ...c, lastMessage: message } : c
              );
              return { conversations: sortConversationsByActivity(updated) };
            });
          },

          bumpConversation: (conversationId: string, message: DMLastMessage | null) =>
            set((state) => {
              const exists = state.conversations.some((c) => c.id === conversationId);
              if (!exists) return state;
              markConversationFetchMutation(conversationId, 'lastMessage');
              const updated = state.conversations.map((c) =>
                c.id === conversationId ? { ...c, lastMessage: message } : c
              );
              return { conversations: sortConversationsByActivity(updated) };
            }),

          incrementUnread: (convId: string) => {
            markConversationFetchMutation(convId, 'unreadCount');
            set((state) => ({
              conversations: state.conversations.map((c) =>
                c.id === convId ? { ...c, unreadCount: c.unreadCount + 1 } : c
              ),
            }));
          },

          clearUnread: (convId: string) => {
            markConversationFetchMutation(convId, 'unreadCount');
            set((state) => ({
              conversations: state.conversations.map((c) =>
                c.id === convId ? { ...c, unreadCount: 0 } : c
              ),
            }));
          },

          clearDMs: () => {
            conversationRefetchQueued = false;
            // Module-scope, so `set()` cannot reach it and no store reset would
            // have. gracefulReset() calls clearDMs() on every login-screen and
            // reload transition, which is the account boundary this needs to
            // respect — the same reasoning as clearFriendEligibilityCache()
            // (#1241), folded in here rather than exported because this store
            // already clears its other module state in this function.
            distributionRateLimitedUntil.clear();
            distributionRateLimitedUntilAll = 0;
            distributionLifecycleGeneration++;
            for (const journal of conversationFetchJournals) journal.cleared = true;
            for (const conversation of get().conversations) {
              purgeConversationAccessState(conversation.id);
            }
            set({
              conversations: [],
              activeConversationId: null,
              isLoading: false,
            });
          },

          addGroupMember: async (conversationId: string, userId: string) => {
            const response = await apiFetch(`/api/v1/dm/conversations/${conversationId}/members`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ user_id: userId }),
            });
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to add member');
            }
            const data = await response.json();
            if (data.conversation) {
              const conv = mapConversation(data.conversation);
              markConversationFetchUpdate(conv.id, conv);
              set((state) => ({
                conversations: state.conversations.map((c) => (c.id === conv.id ? conv : c)),
              }));
            }
          },

          removeGroupMember: async (conversationId: string, userId: string) => {
            const response = await apiFetch(
              `/api/v1/dm/conversations/${conversationId}/members/${userId}`,
              {
                method: 'DELETE',
              }
            );
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to remove member');
            }
          },

          leaveGroup: async (conversationId: string) => {
            const userStore = await import('../auth/userStore');
            const userId = userStore.useUserStore.getState().user?.id;
            if (!userId) throw new Error('Not authenticated');

            const response = await apiFetch(
              `/api/v1/dm/conversations/${conversationId}/members/${userId}`,
              {
                method: 'DELETE',
              }
            );
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to leave group');
            }
            get().removeConversation(conversationId);
          },

          updateMemberRole: async (
            conversationId: string,
            userId: string,
            role: 'admin' | 'member'
          ) => {
            const response = await apiFetch(
              `/api/v1/dm/conversations/${conversationId}/members/${userId}`,
              {
                method: 'PATCH',
                headers: { 'Content-Type': 'application/json' },
                body: JSON.stringify({ role }),
              }
            );
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to update role');
            }
            // Optimistically update the participant's role
            markConversationFetchMutation(conversationId, 'participants');
            const updateRole = (conv: DMConversation) =>
              conv.id === conversationId
                ? {
                    ...conv,
                    participants: conv.participants.map((p) =>
                      p.userId === userId ? { ...p, role } : p
                    ),
                  }
                : conv;
            set((state) => ({ conversations: state.conversations.map(updateRole) }));
          },

          deleteGroup: async (conversationId: string) => {
            const response = await apiFetch(`/api/v1/dm/conversations/${conversationId}`, {
              method: 'DELETE',
            });
            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to delete group');
            }
            get().removeConversation(conversationId);
          },
        }),
        {
          name: 'concord:dm-store',
          partialize: (state) => ({
            activeConversationId: state.activeConversationId,
          }),
        }
      ),
      { name: 'DMStore' }
    )
  )
);

// Map API response to DMConversation
function mapConversation(c: Record<string, unknown>): DMConversation {
  const participants: DMParticipant[] = Array.isArray(c.participants)
    ? c.participants.map((p: Record<string, unknown>) => ({
        userId: p.user_id as string,
        username: p.username as string,
        displayName: p.display_name as string | undefined,
        avatarUrl: p.avatar_url as string | undefined,
        colorScheme: p.color_scheme as string | undefined,
        status: p.status as string | undefined,
        role: p.role === 'admin' || p.role === 'member' ? p.role : undefined,
      }))
    : [];

  const lastMsg = c.last_message as Record<string, unknown> | null;

  return {
    id: c.id as string,
    isGroup: c.is_group as boolean,
    isPersonal: (c.is_personal as boolean) || false,
    name: (c.name as string) || null,
    iconUrl: (c.icon_url as string) || undefined,
    createdBy: (c.created_by as string) || undefined,
    participants,
    lastMessage: lastMsg
      ? {
          content: lastMsg.content as string,
          userId: lastMsg.user_id as string,
          username: lastMsg.username as string,
          createdAt: lastMsg.created_at as string,
          type: lastMsg.type as string | undefined,
          callEventPayload: lastMsg.call_event_payload as CallEventPayload | undefined,
        }
      : null,
    unreadCount: (c.unread_count as number) || 0,
    createdAt: c.created_at as string,
  };
}
