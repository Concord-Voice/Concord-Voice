import { create } from 'zustand';
import { persist, devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { apiFetch } from '../services/apiClient';
import { e2eeService } from '../services/e2eeService';
import { isPendingKeyError } from '../services/e2eeErrors';
import { removeScope } from '../services/searchService';
import { errorMessage } from '../utils/redactError';
import { useChatStore } from './chatStore';
import type { CallEventPayload } from '../types/chat';

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
 * Fetch public keys for a list of user IDs, returning a Map of userId → publicKey.
 * Failures for individual users are silently skipped.
 */
async function fetchParticipantPublicKeys(userIds: string[]): Promise<Map<string, string>> {
  const results = await Promise.allSettled(
    userIds.map(async (userId) => {
      const pkRes = await apiFetch(`/api/v1/users/${userId}/public-key`);
      if (!pkRes.ok) return null;
      const pkData = await pkRes.json();
      return pkData.public_key ? { userId, publicKey: pkData.public_key as string } : null;
    })
  );
  const memberKeys = new Map<string, string>();
  for (const r of results) {
    if (r.status === 'fulfilled' && r.value) {
      memberKeys.set(r.value.userId, r.value.publicKey);
    }
  }
  return memberKeys;
}

/**
 * Distribute wrapped E2EE channel keys to the server for the given conversation.
 */
async function distributeChannelKeys(
  convId: string,
  memberKeys: Map<string, string>
): Promise<void> {
  const wrappedKeys = await e2eeService.createChannelKeys(memberKeys);
  const distRes = await apiFetch(`/api/v1/e2ee/keys/${convId}`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ wrapped_keys: Object.fromEntries(wrappedKeys) }),
  });
  if (!distRes.ok) {
    console.error('Failed to distribute E2EE key for DM:', distRes.status);
  }
}

/**
 * Ensure E2EE channel key exists for a DM conversation.
 * If no key exists (E2EEKeyUnavailableError with pending=true), fetches
 * participant public keys and distributes a new key.
 */
async function ensureE2EEKey(conv: DMConversation): Promise<void> {
  if (!e2eeService.isInitialized) return;

  let needsKeyDistribution = false;
  try {
    await e2eeService.getChannelKey(conv.id);
  } catch (err) {
    needsKeyDistribution = isPendingKeyError(err);
  }

  if (!needsKeyDistribution) return;

  try {
    const memberKeys = await fetchParticipantPublicKeys(conv.participants.map((p) => p.userId));
    if (memberKeys.size === 0) return;

    await distributeChannelKeys(conv.id, memberKeys);
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

  let keyExists = false;
  try {
    await e2eeService.getChannelKey(conv.id);
    keyExists = true;
  } catch {
    // Key not available
  }

  if (keyExists) return;

  try {
    const userStore = await import('../stores/userStore');
    const userId = userStore.useUserStore.getState().user?.id;
    if (!userId) return;

    const memberKeys = await fetchParticipantPublicKeys([userId]);
    if (memberKeys.size === 0) return;

    await distributeChannelKeys(conv.id, memberKeys);
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
            const userStore = await import('../stores/userStore');
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
