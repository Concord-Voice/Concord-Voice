import { create } from 'zustand';
import { persist, devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { ServerWithRole } from '../types/server';
import { apiFetch } from '../services/apiClient';
import { useChannelStore } from './channelStore';
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

interface ServerFetchJournal {
  removedIds: Set<string>;
  cleared: boolean;
}

const serverFetchJournals = new Set<ServerFetchJournal>();
const hasLiveServerFetch = () =>
  Array.from(serverFetchJournals).some((journal) => !journal.cleared);

function purgeMissingServerState(
  currentServers: ServerWithRole[],
  fetchedServers: ServerWithRole[]
): void {
  const fetchedServerIds = new Set(fetchedServers.map((server) => server.id));
  for (const server of currentServers) {
    if (fetchedServerIds.has(server.id)) continue;
    useChannelStore.getState().removeServerChannels(server.id);
    useUnreadStore.getState().clearServerUnread(server.id);
  }
}

function retainActiveServerId(
  activeServerId: string | null,
  fetchedServers: ServerWithRole[]
): string | null {
  return activeServerId && fetchedServers.some((server) => server.id === activeServerId)
    ? activeServerId
    : null;
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
          const journal: ServerFetchJournal = { removedIds: new Set(), cleared: false };
          serverFetchJournals.add(journal);

          try {
            if (!serverStore.persist.hasHydrated()) {
              await serverStore.persist.rehydrate();
            }
            if (journal.cleared) return;

            // Deduplicate concurrent fetches (e.g. multiple components mounting)
            if (get().isLoading) return;
            set({ isLoading: true, error: null });

            const response = await apiFetch('/api/v1/servers');

            if (!response.ok) {
              const data = await response.json();
              if (journal.cleared) return;
              throw new Error(data.error || 'Failed to load servers');
            }

            const data = await response.json();
            if (journal.cleared) return;

            const fetchedServers: ServerWithRole[] = (data.servers || []).filter(
              (server: ServerWithRole) => !journal.removedIds.has(server.id)
            );
            purgeMissingServerState(get().servers, fetchedServers);

            // Validate persisted activeServerId still exists in fetched list
            const validActiveId = retainActiveServerId(get().activeServerId, fetchedServers);

            set({ servers: fetchedServers, activeServerId: validActiveId });
          } catch (error) {
            if (!journal.cleared) {
              set({
                error: error instanceof Error ? error.message : 'Failed to load servers',
              });
            }
          } finally {
            serverFetchJournals.delete(journal);
            set({ isLoading: hasLiveServerFetch() });
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
          for (const journal of serverFetchJournals) journal.removedIds.add(serverId);
          const { activeServerId } = get();
          useChannelStore.getState().removeServerChannels(serverId);

          // Clear server-level unread
          useUnreadStore.getState().clearServerUnread(serverId);

          set((state) => ({
            servers: state.servers.filter((s) => s.id !== serverId),
            activeServerId: activeServerId === serverId ? null : activeServerId,
            isLoading: hasLiveServerFetch(),
          }));
        },

        setActiveServer: (serverId: string | null) => {
          set({ activeServerId: serverId });
        },

        clearServers: () => {
          for (const journal of serverFetchJournals) journal.cleared = true;
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
