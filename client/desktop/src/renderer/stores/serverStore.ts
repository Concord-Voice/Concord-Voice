import { create } from 'zustand';
import { persist, devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { ServerWithRole } from '../types/server';
import { apiFetch } from '../services/apiClient';
import { useChannelStore } from './channelStore';
import { useChatStore } from './chatStore';
import { useUnreadStore } from './unreadStore';

interface ServerState {
  servers: ServerWithRole[];
  activeServerId: string | null;
  isLoading: boolean;
  error: string | null;

  fetchServers: () => Promise<void>;
  addServer: (server: ServerWithRole) => void;
  updateServer: (serverId: string, updates: Partial<ServerWithRole>) => void;
  updateOnlineCounts: (counts: Record<string, number>) => void;
  removeServer: (serverId: string) => void;
  setActiveServer: (serverId: string | null) => void;
  clearServers: () => void;
}

const serverStore = create<ServerState>()(
  devtools(
    persist(
      (set, get) => ({
        servers: [],
        activeServerId: null,
        isLoading: false,
        error: null,

        fetchServers: async () => {
          if (!serverStore.persist.hasHydrated()) {
            await serverStore.persist.rehydrate();
          }

          // Deduplicate concurrent fetches (e.g. multiple components mounting)
          if (get().isLoading) return;
          set({ isLoading: true, error: null });

          try {
            const response = await apiFetch('/api/v1/servers');

            if (!response.ok) {
              const data = await response.json();
              throw new Error(data.error || 'Failed to load servers');
            }

            const data = await response.json();
            const fetchedServers: ServerWithRole[] = data.servers || [];

            // Validate persisted activeServerId still exists in fetched list
            const currentActiveId = get().activeServerId;
            const validActiveId =
              currentActiveId && fetchedServers.some((s) => s.id === currentActiveId)
                ? currentActiveId
                : null;

            set({ servers: fetchedServers, activeServerId: validActiveId, isLoading: false });
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to load servers',
              isLoading: false,
            });
          }
        },

        addServer: (server: ServerWithRole) => {
          set((state) => ({ servers: [server, ...state.servers] }));
        },

        updateServer: (serverId: string, updates: Partial<ServerWithRole>) => {
          set((state) => ({
            servers: state.servers.map((s) => (s.id === serverId ? { ...s, ...updates } : s)),
          }));
        },

        updateOnlineCounts: (counts: Record<string, number>) => {
          set((state) => ({
            servers: state.servers.map((s) =>
              counts[s.id] === undefined ? s : { ...s, online_count: counts[s.id] }
            ),
          }));
        },

        removeServer: (serverId: string) => {
          const { activeServerId } = get();
          const channelStore = useChannelStore.getState();

          // Cascade: clear all channels belonging to this server
          // (and their messages/unreads via removeChannel's own cascade)
          if (channelStore.currentServerId === serverId) {
            // Active server being deleted — clear messages for each channel, then wipe channel state
            for (const ch of channelStore.channels) {
              useChatStore.getState().clearMessages(ch.id);
              useUnreadStore.getState().clearUnread(ch.id);
            }
            channelStore.clearChannels();
          }

          // Clean up lastChannelByServer for this server
          const { lastChannelByServer } = channelStore;
          if (lastChannelByServer[serverId]) {
            const updated = { ...lastChannelByServer };
            delete updated[serverId];
            useChannelStore.setState({ lastChannelByServer: updated });
          }

          // Clear server-level unread
          useUnreadStore.getState().clearServerUnread(serverId);

          set((state) => ({
            servers: state.servers.filter((s) => s.id !== serverId),
            activeServerId: activeServerId === serverId ? null : activeServerId,
          }));
        },

        setActiveServer: (serverId: string | null) => {
          set({ activeServerId: serverId });
        },

        clearServers: () => {
          set({ servers: [], activeServerId: null, isLoading: false, error: null });
        },
      }),
      {
        name: 'concord-servers',
        partialize: (state) => ({ activeServerId: state.activeServerId }),
      }
    ),
    { name: 'ServerStore' }
  )
);

export const useServerStore = wrapStore(serverStore);
