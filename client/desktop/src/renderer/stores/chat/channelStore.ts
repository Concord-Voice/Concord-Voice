import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';
import { wrapStore } from '../../utils/createStore';
import { Channel, ChannelGroup } from '../../types/chat';
import { apiFetch } from '../../services/system/apiClient';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { removeScope } from '../../services/messaging/searchService';
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

  fetchChannels: (serverId: string) => Promise<void>;
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

          fetchChannels: async (serverId: string) => {
            const journal: ChannelFetchJournal = {
              serverId,
              removedChannelIds: new Set(),
              discarded: false,
              viewDiscarded: false,
            };
            channelFetchJournals.add(journal);
            const { activeChannelId, currentServerId, lastChannelByServer } = get();

            // Save current channel for the server we're leaving
            const updatedLastChannel = { ...lastChannelByServer };
            if (currentServerId && activeChannelId) {
              updatedLastChannel[currentServerId] = activeChannelId;
            }

            // Clear active channel immediately when switching servers
            if (serverId === currentServerId) {
              set({ isLoading: true, error: null });
            } else {
              set({
                activeChannelId: null,
                currentServerId: serverId,
                lastChannelByServer: updatedLastChannel,
                isLoading: true,
                error: null,
              });
            }

            try {
              const response = await apiFetch(`/api/v1/servers/${serverId}/channels`);
              if (journal.discarded) return;

              if (!response.ok) {
                const data = await response.json();
                if (journal.discarded) return;
                throw new Error(data.error || 'Failed to load channels');
              }

              const data = await response.json();
              if (journal.discarded) return;

              const channels: Channel[] = (data.channels || []).filter(
                (channel: Channel) => !journal.removedChannelIds.has(channel.id)
              );
              const channelGroups: ChannelGroup[] = data.channel_groups || [];

              purgeMissingChannelAccessState(get().channelIdsByServer[serverId] ?? [], channels);

              // Restore last-viewed channel for this server, or pick first text channel
              const nextChannelId = selectActiveChannelId(channels, updatedLastChannel[serverId]);

              const channelIdsByServer = {
                ...get().channelIdsByServer,
                [serverId]: channels.map((channel) => channel.id),
              };
              set(
                buildFetchedChannelState(
                  journal,
                  get().currentServerId,
                  serverId,
                  channels,
                  channelGroups,
                  channelIdsByServer,
                  nextChannelId
                )
              );
            } catch (error) {
              if (canReportChannelFetchError(journal, get().currentServerId, serverId)) {
                set({
                  error: error instanceof Error ? error.message : 'Failed to load channels',
                });
              }
            } finally {
              channelFetchJournals.delete(journal);
              set({ isLoading: hasLiveChannelFetch() });
            }
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
          }),
        }
      ),
      { name: 'ChannelStore' }
    )
  )
);
