import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { Channel, ChannelGroup } from '../types/chat';
import { apiFetch } from '../services/apiClient';
import { useChatStore } from './chatStore';
import { useUnreadStore } from './unreadStore';

interface ChannelState {
  channels: Channel[];
  channelGroups: ChannelGroup[];
  collapsedGroups: string[]; // group IDs that are collapsed (persisted)
  activeChannelId: string | null;
  currentServerId: string | null;
  lastChannelByServer: Record<string, string>;
  isLoading: boolean;
  error: string | null;

  fetchChannels: (serverId: string) => Promise<void>;
  addChannel: (channel: Channel) => void;
  updateChannel: (channelId: string, updates: Partial<Channel>) => void;
  removeChannel: (channelId: string) => void;
  setActiveChannel: (channelId: string | null) => void;
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
          isLoading: false,
          error: null,

          fetchChannels: async (serverId: string) => {
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

              if (!response.ok) {
                const data = await response.json();
                throw new Error(data.error || 'Failed to load channels');
              }

              const data = await response.json();
              const channels: Channel[] = data.channels || [];
              const channelGroups: ChannelGroup[] = data.channel_groups || [];

              // Restore last-viewed channel for this server, or pick first text channel
              const lastChannel = updatedLastChannel[serverId];
              let nextChannelId: string | null = null;

              if (lastChannel && channels.some((c) => c.id === lastChannel)) {
                nextChannelId = lastChannel;
              } else {
                const firstText = channels.find((c) => c.type === 'text');
                if (firstText) {
                  nextChannelId = firstText.id;
                }
              }

              set({
                channels,
                channelGroups,
                activeChannelId: nextChannelId,
                isLoading: false,
              });
            } catch (error) {
              set({
                error: error instanceof Error ? error.message : 'Failed to load channels',
                isLoading: false,
              });
            }
          },

          addChannel: (channel: Channel) => {
            set((state) => {
              // Deduplicate: API response + WS broadcast can both call addChannel
              if (state.channels.some((c) => c.id === channel.id)) return state;
              return { channels: [...state.channels, channel] };
            });
          },

          updateChannel: (channelId: string, updates: Partial<Channel>) => {
            set((state) => ({
              channels: state.channels.map((c) => (c.id === channelId ? { ...c, ...updates } : c)),
            }));
          },

          removeChannel: (channelId: string) => {
            const { activeChannelId, lastChannelByServer } = get();

            // Clean up lastChannelByServer references to this channel
            const updatedLastChannel = { ...lastChannelByServer };
            for (const [serverId, chId] of Object.entries(updatedLastChannel)) {
              if (chId === channelId) delete updatedLastChannel[serverId];
            }

            set((state) => ({
              channels: state.channels.filter((c) => c.id !== channelId),
              activeChannelId: activeChannelId === channelId ? null : activeChannelId,
              lastChannelByServer: updatedLastChannel,
            }));

            // Cascade: clear messages and unread count for this channel
            useChatStore.getState().clearMessages(channelId);
            useUnreadStore.getState().clearUnread(channelId);
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

          clearChannels: () => {
            set({
              channels: [],
              channelGroups: [],
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
