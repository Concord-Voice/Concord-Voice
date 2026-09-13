import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import { Channel, ChannelGroup } from '../../types/chat';
import { apiFetch } from '../../services/system/apiClient';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { removeScope } from '../../services/messaging/searchService';
import {
  createExpirationPolicyStorage,
  hasMalformedExpirationPolicyListRow,
  mergeExpirationPolicy,
  parseExpirationPolicyFromListRow,
  parseSeenExpirationRevisions,
  type ExpirationPolicy,
  type ExpirationPolicyReadRequest,
  type ExpirationPolicyReadResult,
} from '../../services/messaging/expirationPolicyApi';
import { isSameAuthLifecycle } from '../../services/system/postLoginHydrationLifecycle';
import { useChatStore } from './chatStore';
import { useUnreadStore } from './unreadStore';

function invalidateChannelAccessState(channelId: string): void {
  // Fence pending decrypts before purging searchable plaintext so a stale
  // generation cannot recreate the search scope after channel access is lost.
  e2eeService.revokeChannelAccess(channelId);
  removeScope(channelId);
}

function purgeChannelAccessState(channelId: string): void {
  invalidateChannelAccessState(channelId);
  useChatStore.getState().clearMessages(channelId);
  useUnreadStore.getState().clearUnread(channelId);
}

interface ChannelFetchJournal {
  serverId: string;
  removedChannelIds: Set<string>;
  discarded: boolean;
  viewDiscarded: boolean;
}

const channelFetchJournals = new Set<ChannelFetchJournal>();
const hasLiveChannelFetch = () =>
  Array.from(channelFetchJournals).some((journal) => !journal.discarded && !journal.viewDiscarded);

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

function mapChannel(row: Record<string, unknown>): Channel {
  const {
    expiration_window_seconds: _expirationWindowSeconds,
    expiration_updated_at: _expirationUpdatedAt,
    expiration_revision: _expirationRevision,
    expiration_backfill_pending: _expirationBackfillPending,
    ...channel
  } = row;
  const mapped = channel as unknown as Channel;
  return { ...mapped, expirationPolicy: parseExpirationPolicyFromListRow(row) };
}

interface ChannelState {
  channels: Channel[];
  channelGroups: ChannelGroup[];
  collapsedGroups: string[]; // group IDs that are collapsed (persisted)
  activeChannelId: string | null;
  currentServerId: string | null;
  lastChannelByServer: Record<string, string>;
  channelIdsByServer: Record<string, string[]>;
  isLoading: boolean;
  error: string | null;
  seenExpirationRevisionsByAccount: Record<string, Record<string, number>>;
  invalidExpirationPolicyIds: Record<string, true>;

  fetchChannels: (
    serverId: string,
    read?: ExpirationPolicyReadRequest
  ) => Promise<ExpirationPolicyReadResult | undefined>;
  applyExpirationPolicy: (targetId: string, policy: ExpirationPolicy) => void;
  markExpirationSeen: (accountId: string, targetId: string, revision: number) => void;
  addChannel: (channel: Channel) => void;
  updateChannel: (channelId: string, updates: Partial<Channel>) => void;
  removeChannel: (channelId: string) => void;
  removeServerChannels: (serverId: string) => void;
  setActiveChannel: (channelId: string | null) => void;
  clearChannelView: () => void;
  clearChannels: () => void;

  // Channel group actions
  addChannelGroup: (group: ChannelGroup) => void;
  updateChannelGroup: (groupId: string, updates: Partial<ChannelGroup>) => void;
  removeChannelGroup: (groupId: string) => void;
  toggleGroupCollapsed: (groupId: string) => void;
  reorderChannels: (
    updates: { channel_id: string; group_id: string | null; position: number }[]
  ) => void;

  // Voice text chat helpers
  getLinkedTextChannel: (voiceChannelId: string) => Channel | undefined;
}

function purgeMissingChannelAccessState(knownChannelIds: string[], channels: Channel[]): void {
  const fetchedIds = new Set(channels.map((channel) => channel.id));
  for (const channelId of knownChannelIds) {
    if (!fetchedIds.has(channelId)) purgeChannelAccessState(channelId);
  }
}

function selectActiveChannelId(channels: Channel[], lastChannelId?: string): string | null {
  if (lastChannelId && channels.some((channel) => channel.id === lastChannelId)) {
    return lastChannelId;
  }
  return channels.find((channel) => channel.type === 'text')?.id ?? null;
}

function buildFetchedChannelState(
  journal: ChannelFetchJournal,
  currentServerId: string | null,
  requestedServerId: string,
  channels: Channel[],
  channelGroups: ChannelGroup[],
  channelIdsByServer: Record<string, string[]>,
  activeChannelId: string | null
): Partial<ChannelState> {
  if (journal.viewDiscarded || currentServerId !== requestedServerId) return { channelIdsByServer };
  return { channels, channelGroups, channelIdsByServer, activeChannelId };
}

function canReportChannelFetchError(
  journal: ChannelFetchJournal,
  currentServerId: string | null,
  requestedServerId: string
): boolean {
  return !journal.discarded && !journal.viewDiscarded && currentServerId === requestedServerId;
}

function isChannelReadSuperseded(
  journal: ChannelFetchJournal,
  currentServerId: string | null,
  requestedServerId: string,
  read: ExpirationPolicyReadRequest | undefined
): boolean {
  return Boolean(
    read &&
    (journal.discarded ||
      journal.viewDiscarded ||
      currentServerId !== requestedServerId ||
      !isSameAuthLifecycle(read.lifecycle))
  );
}

function isChannelFetchSuperseded(
  journal: ChannelFetchJournal,
  currentServerId: string | null,
  requestedServerId: string,
  read: ExpirationPolicyReadRequest | undefined
): boolean {
  return (
    journal.discarded || isChannelReadSuperseded(journal, currentServerId, requestedServerId, read)
  );
}

function channelFetchResult(
  read: ExpirationPolicyReadRequest | undefined,
  kind: 'superseded' | 'unavailable'
): ExpirationPolicyReadResult | undefined {
  return read ? { kind } : undefined;
}

function prepareChannelFetch(
  requestedServerId: string,
  currentState: Pick<ChannelState, 'activeChannelId' | 'currentServerId' | 'lastChannelByServer'>
): { updatedLastChannel: Record<string, string>; state: Partial<ChannelState> } {
  const updatedLastChannel = { ...currentState.lastChannelByServer };
  if (currentState.currentServerId && currentState.activeChannelId) {
    updatedLastChannel[currentState.currentServerId] = currentState.activeChannelId;
  }
  if (requestedServerId === currentState.currentServerId) {
    return { updatedLastChannel, state: { isLoading: true, error: null } };
  }
  return {
    updatedLastChannel,
    state: {
      activeChannelId: null,
      currentServerId: requestedServerId,
      lastChannelByServer: updatedLastChannel,
      isLoading: true,
      error: null,
    },
  };
}

function reportChannelFetchError(
  journal: ChannelFetchJournal,
  currentServerId: string | null,
  requestedServerId: string,
  error: unknown,
  set: (state: Partial<ChannelState>) => void
): void {
  if (!canReportChannelFetchError(journal, currentServerId, requestedServerId)) return;
  set({ error: error instanceof Error ? error.message : 'Failed to load channels' });
}

type FetchedChannels = {
  channels: Channel[];
  channelGroups: ChannelGroup[];
  invalidExpirationPolicyIds: Set<string>;
};

function parseFetchedChannels(
  data: unknown,
  journal: ChannelFetchJournal
): FetchedChannels | undefined {
  if (!isRecord(data) || !Array.isArray(data.channels)) return undefined;
  if (
    data.channels.some(
      (channel) => !isRecord(channel) || typeof channel.id !== 'string' || channel.id === ''
    )
  ) {
    return undefined;
  }
  const rows = data.channels.map((channel) => channel as Record<string, unknown>);
  return {
    channels: rows.map(mapChannel).filter((channel) => !journal.removedChannelIds.has(channel.id)),
    channelGroups: Array.isArray(data.channel_groups)
      ? (data.channel_groups as ChannelGroup[])
      : [],
    invalidExpirationPolicyIds: new Set(
      rows
        .filter((channel) => hasMalformedExpirationPolicyListRow(channel))
        .map((channel) => channel.id as string)
    ),
  };
}

function buildFetchedChannelCommit(
  journal: ChannelFetchJournal,
  requestedServerId: string,
  updatedLastChannel: Record<string, string>,
  fetched: FetchedChannels,
  currentState: Pick<
    ChannelState,
    | 'channels'
    | 'currentServerId'
    | 'activeChannelId'
    | 'channelIdsByServer'
    | 'invalidExpirationPolicyIds'
  >
): Partial<ChannelState> {
  purgeMissingChannelAccessState(
    currentState.channelIdsByServer[requestedServerId] ?? [],
    fetched.channels
  );
  const currentById = new Map(currentState.channels.map((channel) => [channel.id, channel]));
  const channels = fetched.channels.map((channel) => {
    if (fetched.invalidExpirationPolicyIds.has(channel.id)) return channel;
    const expirationPolicy = mergeExpirationPolicy(
      currentById.get(channel.id)?.expirationPolicy,
      channel.expirationPolicy
    );
    return expirationPolicy ? { ...channel, expirationPolicy } : channel;
  });
  const liveSelection =
    currentState.currentServerId === requestedServerId ? currentState.activeChannelId : null;
  const activeChannelId = selectActiveChannelId(
    channels,
    liveSelection ?? updatedLastChannel[requestedServerId]
  );
  const channelIdsByServer = {
    ...currentState.channelIdsByServer,
    [requestedServerId]: channels.map((channel) => channel.id),
  };
  const invalidExpirationPolicyIds = { ...currentState.invalidExpirationPolicyIds };
  for (const channel of channels) {
    if (fetched.invalidExpirationPolicyIds.has(channel.id))
      invalidExpirationPolicyIds[channel.id] = true;
    else delete invalidExpirationPolicyIds[channel.id];
  }
  return {
    ...buildFetchedChannelState(
      journal,
      currentState.currentServerId,
      requestedServerId,
      channels,
      fetched.channelGroups,
      channelIdsByServer,
      activeChannelId
    ),
    invalidExpirationPolicyIds,
  };
}

function channelReadResult(
  read: ExpirationPolicyReadRequest,
  channels: Channel[],
  committedChannels: Channel[]
): ExpirationPolicyReadResult {
  const target = channels.find((channel) => channel.id === read.targetId);
  if (!target) return { kind: 'missing' };
  if (!target.expirationPolicy) return { kind: 'unavailable' };
  const policy = committedChannels.find(
    (channel) => channel.id === read.targetId
  )?.expirationPolicy;
  return policy ? { kind: 'fresh', policy } : { kind: 'superseded' };
}

function removeChannelFromServerIndex(
  channelIdsByServer: Record<string, string[]>,
  channelId: string
): Record<string, string[]> {
  return Object.fromEntries(
    Object.entries(channelIdsByServer).map(([serverId, channelIds]) => [
      serverId,
      channelIds.filter((id) => id !== channelId),
    ])
  );
}

export const useChannelStore = wrapStore(
  create<ChannelState>()(
    devtools(
      persist(
        (set, get) => ({
          channels: [],
          channelGroups: [],
          collapsedGroups: [],
          activeChannelId: null,
          currentServerId: null,
          lastChannelByServer: {},
          channelIdsByServer: {},
          isLoading: false,
          error: null,
          seenExpirationRevisionsByAccount: {},
          invalidExpirationPolicyIds: {},

          fetchChannels: async (serverId: string, read?: ExpirationPolicyReadRequest) => {
            if (read && !isSameAuthLifecycle(read.lifecycle)) return { kind: 'superseded' };
            const journal: ChannelFetchJournal = {
              serverId,
              removedChannelIds: new Set(),
              discarded: false,
              viewDiscarded: false,
            };
            channelFetchJournals.add(journal);
            const prepared = prepareChannelFetch(serverId, get());
            set(prepared.state);

            try {
              const response = await apiFetch(`/api/v1/servers/${serverId}/channels`);
              if (isChannelFetchSuperseded(journal, get().currentServerId, serverId, read)) {
                return channelFetchResult(read, 'superseded');
              }

              if (!response.ok) {
                const data = await response.json();
                if (isChannelFetchSuperseded(journal, get().currentServerId, serverId, read)) {
                  return channelFetchResult(read, 'superseded');
                }
                throw new Error(data.error || 'Failed to load channels');
              }

              const data: unknown = await response.json();
              if (isChannelFetchSuperseded(journal, get().currentServerId, serverId, read)) {
                return channelFetchResult(read, 'superseded');
              }

              const fetched = parseFetchedChannels(data, journal);
              if (!fetched) throw new Error('Failed to load channels');
              const committedState = get();
              set(
                buildFetchedChannelCommit(
                  journal,
                  serverId,
                  prepared.updatedLastChannel,
                  fetched,
                  committedState
                )
              );
              if (!read) return undefined;
              if (isChannelFetchSuperseded(journal, get().currentServerId, serverId, read)) {
                return { kind: 'superseded' };
              }
              return channelReadResult(read, fetched.channels, get().channels);
            } catch (error) {
              if (isChannelFetchSuperseded(journal, get().currentServerId, serverId, read)) {
                return channelFetchResult(read, 'superseded');
              }
              reportChannelFetchError(journal, get().currentServerId, serverId, error, set);
              return channelFetchResult(read, 'unavailable');
            } finally {
              channelFetchJournals.delete(journal);
              set({ isLoading: hasLiveChannelFetch() });
            }
          },

          applyExpirationPolicy: (targetId: string, policy: ExpirationPolicy) => {
            set((state) => ({
              channels: state.channels.map((channel) => {
                if (channel.id !== targetId) return channel;
                const expirationPolicy = mergeExpirationPolicy(channel.expirationPolicy, policy);
                return expirationPolicy ? { ...channel, expirationPolicy } : channel;
              }),
            }));
          },

          markExpirationSeen: (accountId: string, targetId: string, revision: number) => {
            if (!accountId || !targetId || !Number.isSafeInteger(revision) || revision < 0) return;
            set((state) => ({
              seenExpirationRevisionsByAccount: {
                ...state.seenExpirationRevisionsByAccount,
                [accountId]: {
                  ...state.seenExpirationRevisionsByAccount[accountId],
                  [targetId]: Math.max(
                    state.seenExpirationRevisionsByAccount[accountId]?.[targetId] ?? 0,
                    revision
                  ),
                },
              },
            }));
          },

          addChannel: (channel: Channel) => {
            set((state) => {
              // Deduplicate: API response + WS broadcast can both call addChannel
              if (state.channels.some((c) => c.id === channel.id)) return state;
              const serverChannelIds = state.channelIdsByServer[channel.server_id] ?? [];
              return {
                channels: [...state.channels, channel],
                channelIdsByServer: {
                  ...state.channelIdsByServer,
                  [channel.server_id]: [...serverChannelIds, channel.id],
                },
              };
            });
          },

          updateChannel: (channelId: string, updates: Partial<Channel>) => {
            set((state) => ({
              channels: state.channels.map((c) => (c.id === channelId ? { ...c, ...updates } : c)),
            }));
          },

          removeChannel: (channelId: string) => {
            for (const journal of channelFetchJournals) {
              journal.removedChannelIds.add(channelId);
            }
            const { activeChannelId, lastChannelByServer } = get();

            purgeChannelAccessState(channelId);

            // Clean up lastChannelByServer references to this channel
            const updatedLastChannel = { ...lastChannelByServer };
            for (const [serverId, chId] of Object.entries(updatedLastChannel)) {
              if (chId === channelId) delete updatedLastChannel[serverId];
            }

            set((state) => ({
              channels: state.channels.filter((c) => c.id !== channelId),
              channelIdsByServer: removeChannelFromServerIndex(state.channelIdsByServer, channelId),
              activeChannelId: activeChannelId === channelId ? null : activeChannelId,
              lastChannelByServer: updatedLastChannel,
              isLoading: hasLiveChannelFetch(),
            }));
          },

          removeServerChannels: (serverId: string) => {
            for (const journal of channelFetchJournals) {
              if (journal.serverId === serverId) journal.discarded = true;
            }
            const state = get();
            const channelIds = new Set(state.channelIdsByServer[serverId] ?? []);
            for (const channel of state.channels) {
              if (channel.server_id === serverId) channelIds.add(channel.id);
            }
            for (const channelId of channelIds) purgeChannelAccessState(channelId);

            const channelIdsByServer = { ...state.channelIdsByServer };
            delete channelIdsByServer[serverId];
            const lastChannelByServer = { ...state.lastChannelByServer };
            delete lastChannelByServer[serverId];

            set({
              ...(state.currentServerId === serverId
                ? {
                    channels: [],
                    channelGroups: [],
                    activeChannelId: null,
                    currentServerId: null,
                    isLoading: false,
                    error: null,
                  }
                : {}),
              channelIdsByServer,
              lastChannelByServer,
              isLoading: hasLiveChannelFetch(),
            });
          },

          setActiveChannel: (channelId: string | null) => {
            const { currentServerId, lastChannelByServer } = get();
            const updates: Partial<ChannelState> = { activeChannelId: channelId };

            // Track last-viewed channel per server
            if (currentServerId && channelId) {
              updates.lastChannelByServer = {
                ...lastChannelByServer,
                [currentServerId]: channelId,
              };
            }

            set(updates);
          },

          clearChannelView: () => {
            // No server is selected, but access to other known servers has not
            // been revoked. Clear only the rendered server view and preserve
            // per-server key/search/message/unread state.
            for (const journal of channelFetchJournals) journal.viewDiscarded = true;
            set({
              channels: [],
              channelGroups: [],
              activeChannelId: null,
              currentServerId: null,
              isLoading: false,
              error: null,
              invalidExpirationPolicyIds: {},
            });
          },

          clearChannels: () => {
            for (const journal of channelFetchJournals) journal.discarded = true;
            const state = get();
            const channelIds = new Set([
              ...state.channels.map((channel) => channel.id),
              ...Object.values(state.channelIdsByServer).flat(),
            ]);
            for (const channelId of channelIds) purgeChannelAccessState(channelId);

            set({
              channels: [],
              channelGroups: [],
              channelIdsByServer: {},
              activeChannelId: null,
              currentServerId: null,
              isLoading: false,
              error: null,
              seenExpirationRevisionsByAccount: {},
              invalidExpirationPolicyIds: {},
            });
          },

          // Channel group actions
          addChannelGroup: (group: ChannelGroup) => {
            set((state) => {
              // Deduplicate: API response + WS broadcast can both call addChannelGroup
              if (state.channelGroups.some((g) => g.id === group.id)) return state;
              return {
                channelGroups: [...state.channelGroups, group].sort(
                  (a, b) => a.position - b.position
                ),
              };
            });
          },

          updateChannelGroup: (groupId: string, updates: Partial<ChannelGroup>) => {
            set((state) => ({
              channelGroups: state.channelGroups
                .map((g) => (g.id === groupId ? { ...g, ...updates } : g))
                .sort((a, b) => a.position - b.position),
            }));
          },

          removeChannelGroup: (groupId: string) => {
            set((state) => ({
              channelGroups: state.channelGroups.filter((g) => g.id !== groupId),
              // Channels in this group become uncategorized (group_id = null)
              channels: state.channels.map((c) =>
                c.group_id === groupId ? { ...c, group_id: null } : c
              ),
              collapsedGroups: state.collapsedGroups.filter((id) => id !== groupId),
            }));
          },

          toggleGroupCollapsed: (groupId: string) => {
            set((state) => ({
              collapsedGroups: state.collapsedGroups.includes(groupId)
                ? state.collapsedGroups.filter((id) => id !== groupId)
                : [...state.collapsedGroups, groupId],
            }));
          },

          reorderChannels: (updates) => {
            set((state) => {
              const channelMap = new Map(state.channels.map((c) => [c.id, c]));
              for (const u of updates) {
                const ch = channelMap.get(u.channel_id);
                if (ch) {
                  channelMap.set(u.channel_id, {
                    ...ch,
                    group_id: u.group_id,
                    position: u.position,
                  });
                }
              }
              return { channels: Array.from(channelMap.values()) };
            });
          },

          // Voice text chat helpers
          getLinkedTextChannel: (voiceChannelId: string) => {
            return get().channels.find((c) => c.linked_voice_channel_id === voiceChannelId);
          },
        }),
        {
          name: 'concord-channels',
          partialize: (state) => ({
            activeChannelId: state.activeChannelId,
            currentServerId: state.currentServerId,
            lastChannelByServer: state.lastChannelByServer,
            collapsedGroups: state.collapsedGroups,
            seenExpirationRevisionsByAccount: state.seenExpirationRevisionsByAccount,
          }),
          merge: (persistedState, currentState) => {
            const persisted = isRecord(persistedState) ? persistedState : {};
            return {
              ...currentState,
              ...persisted,
              seenExpirationRevisionsByAccount: parseSeenExpirationRevisions(
                persisted.seenExpirationRevisionsByAccount
              ),
            };
          },
          storage: createExpirationPolicyStorage<Partial<ChannelState>>(),
        }
      ),
      { name: 'ChannelStore' }
    )
  )
);
