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

/**
 * The outcome of ONE `joinServer` call, carried back to that caller.
 *
 * `error` on this store is a single shared field owned by the newest join (see
 * `joinSequence` below), which makes it the wrong place for a caller to read a
 * reason from: `InviteEmbed` renders one card per invite link and a chat can
 * hold several, so two joins racing means the older call's component reads a
 * value that is not its own. Both failure shapes are reachable — it reads the
 * NEWER join's message, or it reads the `null` that join's own start wrote and
 * falls back to a generic guess. Returning the reason per call removes the
 * shared read entirely rather than trying to time it (Gitar, PR #3353).
 *
 * `abandoned` is a third state and not a failure: a different account owns the
 * session now, the join did happen for the ORIGINAL user, and there is nothing
 * to tell whoever is sitting here. Folding it into `failed` would surface one
 * account's error to the next.
 */
export type JoinServerOutcome =
  | { status: 'joined'; response: JoinServerResponse }
  | { status: 'failed'; reason: string }
  | { status: 'abandoned' };

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
  joinServer: (code: string) => Promise<JoinServerOutcome>;
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
          // A failure that lands AFTER the account changed is `abandoned`, not
          // `failed` — the same rule the success path applies further down, and
          // the reason it has to be applied here too is that a stale failure
          // otherwise hands a reason to a caller belonging to a session that no
          // longer exists. Symmetry is the point: the two halves disagreeing is
          // how one account's error reaches the next one's screen.
          const failure = (reason: string): JoinServerOutcome =>
            isSameAuthLifecycle(lifecycle) ? { status: 'failed', reason } : { status: 'abandoned' };
          set({ isLoading: true, error: null });
          try {
            const res = await apiFetch('/api/v1/invites/join', {
              method: 'POST',
              headers: { 'Content-Type': 'application/json' },
              body: JSON.stringify({ code }),
            });
            // Parse DEFENSIVELY and never let a parse failure become user-facing
            // text. `res.json()` on a non-JSON body — a Cloudflare 502, a WAF
            // interstitial, an edge 429 — throws a SyntaxError whose message
            // embeds a fragment of that body ("Unexpected token '<', \"<html>…").
            // `InviteEmbed` renders the reason verbatim, so that fragment would
            // reach the screen. Note `apiClient.safeJson` does NOT solve this:
            // it embeds a 120-char body preview of its own.
            //
            // Only a string the SERVER authored is ever surfaced; anything else
            // collapses to a fixed message (security review, PR #3353).
            let data: unknown = null;
            try {
              data = await res.json();
            } catch {
              data = null;
            }
            const serverError =
              typeof (data as { error?: unknown } | null)?.error === 'string'
                ? (data as { error: string }).error
                : null;
            if (!res.ok) {
              throw new Error(serverError || 'Failed to join server');
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
              return failure('Failed to join server');
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
              // Touch nothing ACCOUNT-scoped: a stale lifecycle can never write, so
              // the successor session's own state is left as it found it. The
              // shared spinner is a different question — it is operation-scoped,
              // and leaving it pinned true would strand it for the rest of the
              // session, so it is cleared under the sequence fence alone.
              if (mySeq === joinSequence) set({ isLoading: false });
              return { status: 'abandoned' };
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
            return { status: 'joined', response: joined };
          } catch (error) {
            const reason = error instanceof Error ? error.message : 'Failed to join server';
            // The shared field is still written, unchanged. Stating its status
            // honestly rather than implying a consumer: after this PR it has NO
            // production reader anywhere — `InviteEmbed` and `JoinServerModal`
            // were the last two, and every other importer of this store
            // subscribes only to actions. It is kept deliberately, as the store
            // contract every other action here follows, and because dropping it
            // would also retire `joinSequence`, whose only job is deciding who
            // may write it. Retiring both is a separate, larger change than a
            // review fix (code review, PR #3353).
            if (mayWriteJoinState()) set({ error: reason, isLoading: false });
            return failure(reason);
          }
        },

        getInviteInfo: async (code: string) => {
          try {
            const res = await apiFetch(`/api/v1/invites/${encodeURIComponent(code)}`);
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
