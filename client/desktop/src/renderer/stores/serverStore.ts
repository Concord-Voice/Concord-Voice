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
  /**
   * Servers ADDED while this read was in flight. The response predates them, so
   * a whole-array replace would delete them; the commit re-attaches any that the
   * response does not already contain. Symmetric with `removedIds`, and for the
   * same reason (#2363).
   */
  addedIds: Set<string>;
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
          const journal: ServerFetchJournal = {
            removedIds: new Set(),
            addedIds: new Set(),
            cleared: false,
          };
          serverFetchJournals.add(journal);
          let mySeq = serverFetchSequence;

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
            mySeq = ++serverFetchSequence;
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
            // Re-attach rows added while this read was in flight and absent from
            // its response. Read from the CURRENT store rather than a copy taken
            // at add time, so the row carries any later merge; the replace below
            // has not happened yet, so it is still there.
            const fetchedIds = new Set(fetchedServers.map((server) => server.id));
            const readded = get().servers.filter(
              (server) => journal.addedIds.has(server.id) && !fetchedIds.has(server.id)
            );
            const committed = [...readded, ...fetchedServers];

            // Purge and activeServerId both read the COMMITTED list, not the
            // response — purging against the response would tear down the
            // re-attached server's channels and unread state moments after a
            // join, which is the same defect one layer down.
            purgeMissingServerState(get().servers, committed);
            const validActiveId = retainActiveServerId(get().activeServerId, committed);

            set({ servers: committed, activeServerId: validActiveId });
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
          // Journal the addition into every in-flight read, exactly as
          // `removeServer` journals a removal (#2363). `fetchServers` commits a
          // whole-array replace, and a join is not a fetch — so a read that
          // snapshotted membership first still passed the authoritativeness gate
          // and deleted the row just added, reproducing this issue's own
          // missing-sidebar and missing-`subscribe_server` defect until some
          // later fetch ran. `ServerBar` starts that GET on mount and on every
          // token rotation, so the overlap is ordinary rather than exotic.
          //
          // Journalled rather than invalidating those reads by bumping
          // `serverFetchSequence` — the first shape of this fix. Invalidation
          // ALSO skipped the fetch's purge, while its caller saw a resolved
          // promise and a still-current guard: `useConnectionRecovery` would
          // then announce `connection-recovered` with membership never
          // refreshed, so a server the user was removed from during an outage
          // stayed visible and backfill ran against it (#2329's exact defect,
          // reintroduced sideways — Codex). Journalling invalidates nothing.
          for (const journal of serverFetchJournals) journal.addedIds.add(server.id);
          set((state) => {
            const index = state.servers.findIndex((s) => s.id === server.id);
            if (index === -1) return { servers: [server, ...state.servers] };
            // Upsert, not clobber — as a BOUNDARY GUARD on a public store
            // action, with no currently-reachable collision. Both earlier
            // rationales for it were wrong and are recorded so they are not
            // reinvented: a repeat join does not reach here (the control plane
            // 409s an existing member before the insert, internal/invites/
            // handlers.go checkBanAndMembership), and neither does a rehydrated
            // row (`partialize` persists ONLY activeServerId, so `servers` never
            // rehydrates). Today's two callers — joinServer and CreateServerModal
            // — cannot collide either. What the guard buys is that `addServer`
            // means "this server is in the list", so a third caller cannot
            // reintroduce the duplicate `key={server.id}` at ServerList.tsx:78
            // that an unconditional prepend allows. Cheap, and the alternative is
            // an action whose correctness depends on its caller list.
            // Merge, do not replace —
            // an ABSENT key is already preserved by the spread, so the `??`
            // below only guards a caller that passes an EXPLICIT
            // `permissions: undefined` — which the optional field permits and
            // no current caller does. Kept as a boundary guard, not because a
            // live path needs it: today nothing in the renderer reads
            // `servers[].permissions` (permission checks come from
            // permissionStore), so clobbering it would not downgrade anything
            // yet. `fetchServers` does populate the field, so a future reader
            // could start depending on it (#2363).
            const servers = [...state.servers];
            servers[index] = {
              ...servers[index],
              ...server,
              permissions: server.permissions ?? servers[index].permissions,
              // joinServer fabricates 0/0 (JoinServerResponse carries only
              // { server, role }), so on the UPDATE path the incoming counts are
              // known-wrong and must never win. Pinned rather than merged with a
              // non-zero heuristic, which would misread a server that genuinely
              // has 0 online. Every OTHER field legitimately takes the fresh
              // response — a repeat-join reply reflects current server state.
              member_count: servers[index].member_count,
              online_count: servers[index].online_count,
            };
            return { servers };
          });
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
