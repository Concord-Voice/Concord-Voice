import { create } from 'zustand';
import { persist, devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import { ServerWithRole } from '../types/server';
import { apiFetch } from '../services/apiClient';
import {
  isHydrationLifecycleCurrent,
  type HydrationLifecycleGuard,
} from '../services/postLoginHydrationLifecycle';
import { useChannelStore } from './channelStore';
import { useUnreadStore } from './unreadStore';

interface ServerState {
  servers: ServerWithRole[];
  activeServerId: string | null;
  isLoading: boolean;
  error: string | null;

  fetchServers: (guard?: HydrationLifecycleGuard) => Promise<void>;
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

// Monotonic fetch sequence (#2358, Gitar). The #2329 dedup-bypass lets a guarded
// Recovery-A fetch run concurrently with an in-flight unguarded fetchServers();
// without ordering, two independent `set({servers})` commits are last-write-wins
// and a stale response can clobber the authoritative one. Only the latest-started
// fetch is allowed to mutate persistent state (servers/activeServerId/error).
let serverFetchSequence = 0;

/**
 * True only while this fetch may still mutate persistent state: not
 * journal-cleared, not superseded by an account switch (isHydrationLifecycleCurrent,
 * #2329), and the most recently-started fetch (mySeq, #2358, Gitar). Shared by the
 * commit and error gates so both apply the identical predicate (and keeps
 * fetchServers under the cognitive-complexity limit).
 */
function isAuthoritativeServerFetch(
  journal: ServerFetchJournal,
  guard: HydrationLifecycleGuard | undefined,
  mySeq: number
): boolean {
  return !journal.cleared && isHydrationLifecycleCurrent(guard) && mySeq === serverFetchSequence;
}

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

        fetchServers: async (guard?: HydrationLifecycleGuard) => {
          const journal: ServerFetchJournal = { removedIds: new Set(), cleared: false };
          serverFetchJournals.add(journal);
          const mySeq = ++serverFetchSequence;

          try {
            if (!serverStore.persist.hasHydrated()) {
              await serverStore.persist.rehydrate();
            }
            if (journal.cleared) return;

            // Deduplicate concurrent UNGUARDED fetches (e.g. multiple components
            // mounting). A guarded (Recovery-A, #2329) fetch must NOT dedup-skip:
            // its caller awaits this call to observe the authoritative commit
            // BEFORE dispatching the message backfill, so it runs its own fetch to
            // completion. Running concurrently with an in-flight unguarded fetch is
            // clobber-safe because only the latest-started fetch (mySeq) commits
            // (see the gate below, #2358); the isHydrationLifecycleCurrent guard
            // additionally fences an account switch.
            if (get().isLoading && guard === undefined) return;
            set({ isLoading: true, error: null });

            const response = await apiFetch('/api/v1/servers');

            if (!response.ok) {
              const data = await response.json();
              if (journal.cleared) return;
              throw new Error(data.error || 'Failed to load servers');
            }

            const data = await response.json();

            // Skip ALL store mutation (the purge side-effects below AND the commit)
            // unless this fetch is still authoritative — not journal-cleared, not
            // superseded by an account switch (#2329) or a newer concurrent fetch
            // (#2358, Gitar). `finally` still runs its journal/isLoading cleanup.
            if (!isAuthoritativeServerFetch(journal, guard, mySeq)) return;

            const fetchedServers: ServerWithRole[] = (data.servers || []).filter(
              (server: ServerWithRole) => !journal.removedIds.has(server.id)
            );
            purgeMissingServerState(get().servers, fetchedServers);

            // Validate persisted activeServerId still exists in fetched list
            const validActiveId = retainActiveServerId(get().activeServerId, fetchedServers);

            set({ servers: fetchedServers, activeServerId: validActiveId });
          } catch (error) {
            // Only the authoritative fetch may surface its error; a superseded
            // fetch (an older concurrent fetch, or one whose session was switched
            // out) must not clobber a newer fetch's/session's state (#2358 —
            // CodeRabbit "guard every non-cleanup mutation" + Gitar sequence race).
            if (isAuthoritativeServerFetch(journal, guard, mySeq)) {
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
