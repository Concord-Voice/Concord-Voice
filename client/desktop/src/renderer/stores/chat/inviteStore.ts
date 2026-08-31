import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import {
  ServerInviteWithCreator,
  CreateInviteRequest,
  JoinServerResponse,
  InviteInfoResponse,
  ServerWithRole,
} from '../../types/server';
import { apiFetch } from '../../services/system/apiClient';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import { useServerStore } from './serverStore';

export type { InviteInfoResponse };

interface InviteState {
  invites: Record<string, ServerInviteWithCreator[]>; // keyed by serverId
  isLoading: boolean;
  error: string | null;

  fetchInvites: (serverId: string) => Promise<void>;
  createInvite: (
    serverId: string,
    opts?: CreateInviteRequest
  ) => Promise<ServerInviteWithCreator | null>;
  revokeInvite: (serverId: string, inviteId: string) => Promise<boolean>;
  joinServer: (code: string) => Promise<JoinServerResponse | null>;
  getInviteInfo: (code: string) => Promise<InviteInfoResponse | null>;
  clearInvites: () => void;
}

/**
 * Monotonic join sequence. `isLoading` and `error` are shared across every
 * caller of `joinServer`, so only the newest in-flight join may write them.
 */
let joinSequence = 0;

const SERVER_ROLES = ['owner', 'admin', 'member'] as const;

const isNonEmptyString = (value: unknown): value is string =>
  typeof value === 'string' && value !== '';

/**
 * True only when the join response describes a server the renderer can actually
 * render (CODEX P2). `res.ok` says the request succeeded, not that the body is
 * what was asked for, and an unvalidated cast writes the gaps straight into the
 * sidebar: a missing `id` collides `key={server.id}` for every such row, and a
 * missing `name` CRASHES the authenticated chrome outright — `ServerList` and
 * `ServerBar` call `server.name.charAt(0)` unguarded at three sites.
 *
 * Checks exactly what the store and its consumers dereference, not the whole
 * `Server` shape: a schema validator here would be a second source of truth for
 * a type that already has one, and would fail closed on ordinary contract
 * ADDITIONS. A malformed response is treated as a failed join rather than
 * repaired — there is no server to show.
 */
function isJoinedServerUsable(joined: JoinServerResponse | undefined): boolean {
  return (
    isNonEmptyString(joined?.server?.id) &&
    isNonEmptyString(joined?.server?.name) &&
    (SERVER_ROLES as readonly string[]).includes(joined?.role ?? '')
  );
}

export const useInviteStore = wrapStore(
  create<InviteState>()(
    devtools(
      (set, _get) => ({
        invites: {},
        isLoading: false,
        error: null,

        fetchInvites: async (serverId: string) => {
          set({ isLoading: true, error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites`);
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to fetch invites');
            }
            const data = await res.json();
            set((state) => ({
              invites: { ...state.invites, [serverId]: data.invites || [] },
              isLoading: false,
            }));
          } catch (error) {
            set({
              error: error instanceof Error ? error.message : 'Failed to fetch invites',
              isLoading: false,
            });
          }
        },

        createInvite: async (serverId: string, opts?: CreateInviteRequest) => {
          set({ error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites`, {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify(opts || {}),
            });
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to create invite');
            }
            const data = await res.json();
            const invite = data.invite as ServerInviteWithCreator;

            // Append to local cache
            set((state) => {
              const existing = state.invites[serverId] || [];
              return { invites: { ...state.invites, [serverId]: [invite, ...existing] } };
            });

            return invite;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Failed to create invite' });
            return null;
          }
        },

        revokeInvite: async (serverId: string, inviteId: string) => {
          set({ error: null });
          try {
            const res = await apiFetch(`/api/v1/servers/${serverId}/invites/${inviteId}`, {
              method: 'DELETE',
            });
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Failed to revoke invite');
            }

            // Update local cache
            set((state) => {
              const existing = state.invites[serverId] || [];
              return {
                invites: {
                  ...state.invites,
                  [serverId]: existing.map((inv) =>
                    inv.id === inviteId ? { ...inv, is_revoked: true } : inv
                  ),
                },
              };
            });

            return true;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Failed to revoke invite' });
            return false;
          }
        },

        joinServer: async (code: string) => {
          // Captured BEFORE the request, checked before the write. gracefulReset()
          // clears the stores; it cannot cancel a POST already in flight. If user A
          // starts a join, logs out, and user B signs in before it resolves, an
          // unconditional write puts A's server into B's sidebar — and into B's
          // WebSocket subscribe_server set, which is derived from the same array.
          // Newly reachable because reconciliation moved here, where InviteEmbed
          // also lands; before #2363 only JoinServerModal wrote the store.
          const lifecycle = captureAuthLifecycle();
          // `isLoading` and `error` are SHARED, so only the newest join may write
          // them (CODEX P2). Without this, user A's continuation — resolving after
          // A logged out and B started a join of their own — cleared B's spinner
          // while B's request was still in flight, and its failure would have set
          // B's error. The same ownership question the auth fence asks, one level
          // down: that one guards WHOSE data, this one guards WHOSE operation.
          joinSequence += 1;
          const mySeq = joinSequence;
          // BOTH questions, on every write. `joinSequence` alone does not fence a
          // stale ACCOUNT: if A's join resolves after B signed in and B never
          // started a join of their own, A still owns the sequence and would have
          // written its failure into B's store.
          const mayWriteJoinState = () => mySeq === joinSequence && isSameAuthLifecycle(lifecycle);
          set({ isLoading: true, error: null });
          try {
            const res = await apiFetch('/api/v1/invites/join', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ code }),
            });
            const data = await res.json();
            if (!res.ok) {
              throw new Error(data.error || 'Failed to join server');
            }
            const joined = data as JoinServerResponse;
            // `res.ok` says the request succeeded, not that the body is what we
            // asked for. An unvalidated cast writes `id: undefined` into the
            // sidebar on any 200 that omits `server` — and `key={server.id}`
            // then collides for every such row, which is the duplicate-key
            // defect this PR fixes arriving by a different door. Treated as a
            // failed join rather than repaired: there is no server to show.
            if (!isJoinedServerUsable(joined)) {
              if (mayWriteJoinState()) set({ error: 'Failed to join server', isLoading: false });
              return null;
            }
            // DELIBERATELY `isSameAuthLifecycle` here and `mayWriteJoinState()`
            // above, and the two are NOT interchangeable — collapsing them into
            // one choke point looks tidier and is wrong. They answer different
            // questions:
            //   isSameAuthLifecycle  — may this continuation write ACCOUNT-scoped
            //                          data? (the serverStore row)
            //   mayWriteJoinState()  — may it write the SHARED UI flags?
            //                          (isLoading/error, owned by the newest join)
            // A join superseded by a NEWER join in the SAME account must still add
            // its server — the user really did join it — while leaving the newer
            // join's spinner alone. One predicate cannot express that: gating
            // `addServer` on the sequence would silently drop a legitimate
            // membership row every time a user joined twice in quick succession.
            if (!isSameAuthLifecycle(lifecycle)) {
              // A different account (or none) owns the session now. The join did
              // happen server-side for the ORIGINAL user, so this is not an error
              // to surface to whoever is sitting here — drop the continuation and
              // let the new session's own fetchServers describe its membership.
              // Touch nothing at all: a stale lifecycle can never write, so the
              // successor session's own state is left exactly as it found it.
              return null;
            }
            // Reconciliation lives here, not in the callers: there are two call
            // sites (JoinServerModal and InviteEmbed) and one of them forgot,
            // which left a joined server out of the sidebar AND out of the
            // WebSocket subscribe_server set derived from the same array (#2363).
            // ponytail: member_count/online_count are fabricated zeros because
            // JoinServerResponse carries only { server, role }. Both are
            // transient — updateOnlineCounts corrects online_count from the next
            // presence frame and the next fetchServers replaces the row.
            // Widening the join response is a control-plane change, out of scope.
            useServerStore.getState().addServer({
              ...joined.server,
              role: joined.role as ServerWithRole['role'],
              member_count: 0,
              online_count: 0,
            });
            if (mayWriteJoinState()) set({ isLoading: false });
            return joined;
          } catch (error) {
            if (mayWriteJoinState()) {
              set({
                error: error instanceof Error ? error.message : 'Failed to join server',
                isLoading: false,
              });
            }
            return null;
          }
        },

        getInviteInfo: async (code: string) => {
          try {
            const res = await apiFetch(`/api/v1/invites/${code}`);
            if (!res.ok) {
              const data = await res.json();
              throw new Error(data.error || 'Invalid invite code');
            }
            return (await res.json()) as InviteInfoResponse;
          } catch (error) {
            set({ error: error instanceof Error ? error.message : 'Invalid invite code' });
            return null;
          }
        },

        clearInvites: () => {
          set({ invites: {}, isLoading: false, error: null });
        },
      }),
      { name: 'InviteStore' }
    )
  )
);
