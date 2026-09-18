import { useServerStore } from '@/renderer/stores/chat/serverStore';

/**
 * Is the signed-in user already a member of `serverId`?
 *
 * Three states, and the third is the point (#2372):
 *
 * - `true` / `false` — answered from `serverStore.servers`, which is live: it is
 *   the same array the sidebar renders, and `inviteStore.joinServer` writes to
 *   it through `addServer` the instant a join commits. So a "Joined" state
 *   derived from this updates in the same tick as the join, with no cache to
 *   invalidate.
 * - `undefined` — **cannot tell**, because the caller has no server id. That is
 *   a real state: `GET /api/v1/invites/{code}` only started carrying
 *   `server_id` in #2372, and Concord is self-hostable, so an older control
 *   plane omits it.
 *
 * `false` carries a SECOND meaning the tri-state does not separate, and it is
 * worth knowing before trusting one: `serverStore.servers` starts empty and is
 * never rehydrated (`partialize` persists only `activeServerId`), so until the
 * first `fetchServers` commits, a server the user really is in also reads
 * `false`. That window is left failing OPEN on purpose — Join is offered, the
 * control plane answers 409 "You are already a member of this server", and
 * #2372 is what surfaces that reason verbatim instead of guessing at expiry.
 *
 * Failing CLOSED there was considered and rejected (CodeRabbit, #3353). It needs
 * a readiness flag the store does not have — `isLoading` is also `false` before
 * any fetch starts, so it cannot tell "not begun" from "loaded, no servers" —
 * and, more importantly, it can only work by making callers treat an unknown as
 * non-joinable. That is precisely what the `undefined` case forbids: the two
 * unknowns would share one treatment, and every invite against a control plane
 * predating #2372 would acquire a permanently dead Join button. One extra click
 * answered by an accurate error is the cheaper failure.
 *
 * `undefined` must NOT be collapsed into `false` by a caller. They mean
 * different things — "we don't know" versus "we know you are not a member" —
 * and the only safe reading of the first is to offer Join anyway and let the
 * server's 409 be the authority. Returning a tri-state rather than a boolean is
 * what stops two call sites inventing two different defaults for it.
 *
 * This is deliberately a client-side, best-effort check. It exists to stop a
 * user being offered a button that can only fail; it is not an authorization
 * boundary, and `checkBanAndMembership` remains the one that decides.
 */
export function useIsServerMember(serverId: string | undefined): boolean | undefined {
  return useServerStore((s) =>
    serverId === undefined ? undefined : s.servers.some((sv) => sv.id === serverId)
  );
}
