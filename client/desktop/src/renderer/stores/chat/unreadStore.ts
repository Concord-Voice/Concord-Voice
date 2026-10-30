import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import {
  hasUnmutedChannel,
  hasChannelMuteOverride,
  isChannelMuted,
} from '../ui/notificationPrefsStore';

export interface RecentUnreadTarget {
  type: 'channel' | 'dm';
  targetId: string;
  serverId?: string;
}

/** Cap of the LIFO stack the tray's "unread" item resolves against. */
export const RECENT_UNREAD_TARGETS_MAX = 20;

/** Newest first, de-duplicated by `targetId`, capped. Pure. */
export function pushRecentUnreadTarget(
  list: RecentUnreadTarget[],
  target: RecentUnreadTarget
): RecentUnreadTarget[] {
  const rest = list.filter((t) => t.targetId !== target.targetId);
  return [target, ...rest].slice(0, RECENT_UNREAD_TARGETS_MAX);
}

/** Drops the entry for `targetId`; returns the same array when absent. */
export function removeRecentUnreadTarget(
  list: RecentUnreadTarget[],
  targetId: string
): RecentUnreadTarget[] {
  return list.some((t) => t.targetId === targetId)
    ? list.filter((t) => t.targetId !== targetId)
    : list;
}

interface UnreadState {
  // Conversations that most recently received a NEW unread message, newest
  // first (cap 20, unique by targetId). Backs the tray's "N unread messages"
  // item, which opens the most recent still-unread conversation (LIFO). Never
  // persisted. DM entries are pushed/removed by dmStore's unread actions.
  recentUnreadTargets: RecentUnreadTarget[];
  // Channel ID -> unread count (per-channel, active server only)
  unreadCounts: Map<string, number>;

  // Which server `unreadCounts` currently describes, or null before any
  // per-channel fetch has landed. The map is replaced wholesale on each active
  // server's fetch but is NOT cleared on server switch, so consumers that pair
  // it with the live activeServerId (the mute-toggle recompute) must confirm
  // ownership first; otherwise they evaluate the previous server's channel IDs
  // under the new server's mute state (#84 / epic #1029 close audit, P2 review).
  unreadCountsServerId: string | null;
  // Channels belonging to that authoritative snapshot. Live generic writers
  // can insert another server's channel while its fetch is still pending.
  unreadCountsSnapshotIds: Set<string>;

  // Channel ID -> { serverId, unread count } across EVERY server, unlike
  // `unreadCounts` which holds only the active server's channels. Seeded from
  // the bulk /servers/unread-status `channels` field and maintained live by the
  // WS handler. Mute-UNAWARE by construction: the seed cannot filter by mute
  // (no server-side mute resolution exists) so consumers filter at read time
  // with isChannelMutedInMaps (#2403).
  allUnreadCounts: Map<string, { serverId: string; count: number }>;
  // Only a validated detailed bulk response proves these servers complete.
  // Positive live rows prove ownership, never the absence of other unread rows.
  allUnreadCompleteServerIds: Set<string>;
  // Channels mutated locally since `beginChannelUnreadSeed`. The fence that
  // stops an in-flight bulk seed from overwriting fresher local state.
  channelUnreadDirty: Set<string>;

  // Server-dot decisions made while the bulk seed is in flight. The channel
  // fence alone cannot stop server_ids from resurrecting a read server (#3773).
  serverUnreadDirty: Set<string>;
  // A channel read can await complete detail before deciding the server is
  // empty. Later server-only decisions supersede this narrower read reason.
  serverUnreadReadDirty: Set<string>;
  // Whole-server reads suppress every older row for that server, including
  // channels first discovered in the pending bulk response. New dirty arrivals
  // are carried afterward by the per-channel fence. This does not end the request.
  serverReadDirty: Set<string>;
  // Per-server whole-read authority outlives the bulk request's dirty window.
  // A copied map changes only the selected server; reset has its own unique
  // identity so clearing this map cannot revive an older undefined version.
  serverReadVersions: Map<string, symbol>;
  unreadResetId: symbol;
  serverUnreadSeedPending: boolean;
  // Shared identity and request-time membership, invalidated by clearAll and
  // replaced by a successor seed even when it belongs to another hook instance.
  channelUnreadSeedRequest: { id: symbol; serverIds: Set<string> } | null;

  // Channel or DM conversation ID -> mention count (subset of unreadCounts where user was @mentioned)
  mentionCounts: Map<string, number>;

  // Server IDs that have any unread messages (cross-server)
  serverUnreadSet: Set<string>;

  // Subset of serverUnreadSet whose membership was decided by a mute-AWARE
  // source (unread_notify, the per-channel fetch, the mute-toggle recompute,
  // the DND-off refresh, and complete merged detail for pending channel reads —
  // all resolve channel-wins mutes before marking). Other bulk seed decisions
  // remain mute-UNAWARE (see the KNOWN CEILING in useServerChannelSubscriptions).
  // Consumers that suppress dots for muted servers (ServerBar) must trust a
  // precise entry outright — it already means "has an unmuted unread", so a
  // channel explicitly unmuted under a muted server still shows its dot — and
  // fall back to the server-level mute check only for approximate entries.
  serverUnreadPreciseSet: Set<string>;

  // Subset of serverUnreadPreciseSet whose precise mark came from a channel
  // that carries its OWN mute preference (channel-wins), e.g. a channel
  // explicitly unmuted under a muted server. Such a mark stays valid even after
  // the parent server is muted, so the background demote sweep in
  // useServerChannelSubscriptions must skip these; otherwise muting the server
  // (or any unrelated server, since the sweep reruns on every mute toggle) would
  // drop a dot isServerUnreadVisible is meant to honor. A server-FALLBACK precise
  // mark (channel with no override, server unmuted at notify time) is absent from
  // this set and stays demotable once the server is muted (#84 / epic #1029 close
  // audit, P2 review follow-up).
  serverUnreadChannelWinsSet: Set<string>;

  // Server IDs that have unread mentions (cross-server, for priority badges)
  serverMentionSet: Set<string>;

  // Replaces the per-channel map for `serverId` (the active server). Pass the
  // owning server id so the mute-toggle recompute can tell whether the counts
  // are still current; omit only from non-server-scoped call sites.
  setInitialUnreads: (counts: Map<string, number>, serverId?: string | null) => void;
  setUnreadCount: (channelId: string, count: number) => void;
  incrementUnread: (channelId: string) => void;
  clearUnread: (channelId: string) => void;
  // Read only this server's positively or captured-owned counts and mentions.
  // Unlike clearAll, keep other conversations and the admitted seed window.
  markServerRead: (serverId: string) => void;
  clearAll: () => void;

  // Supply completeServerIds only for a successfully validated detailed bulk
  // response. Completeness is limited further to eligible request membership.
  setInitialChannelUnreads: (
    rows: Array<{ channelId: string; serverId: string; count: number }>,
    preserveDirty?: boolean,
    completeServerIds?: ReadonlySet<string>
  ) => void;
  // Call immediately before issuing the bulk /servers/unread-status request.
  // Everything mutated between this call and the seed is a channel whose LOCAL
  // value is fresher than the snapshot the server is computing, and the seed
  // must not overwrite it — see setInitialChannelUnreads. The returned shared
  // identity must still be current after each await before committing a seed.
  beginChannelUnreadSeed: (serverIds?: ReadonlySet<string>) => symbol;
  bumpChannelUnread: (channelId: string, serverId: string) => void;
  // Drop every cross-server count belonging to `serverId`. Call this ONLY when
  // the server itself is gone (left, deleted, kicked, or absent from a fetch) —
  // separate from `clearServerUnread`, which also dims mute-only dots while
  // retaining real unread data so it is available when the mute expires.
  // Without departure cleanup, a departed server's counts are
  // unreachable: `removeServerChannels` has already dropped the channels, so no
  // read recorder can ever call `clearUnread` on them again (#2403).
  clearServerChannelUnreads: (serverId: string) => void;

  incrementMention: (channelId: string) => void;
  clearMentions: (channelId: string) => void;

  setInitialServerUnreads: (serverIds: string[], preserveDirty?: boolean) => void;
  // `precise` marks the entry as mute-resolved (default false = approximate,
  // e.g. the bulk seed). Only pass true from mute-aware call sites. `channelWins`
  // records that the precise mark came from a channel with its own mute override
  // (channel-wins), so the background demote sweep leaves it alone; pass true only
  // alongside precise from the WS path that knows the concrete channel's override.
  markServerUnread: (serverId: string, precise?: boolean, channelWins?: boolean) => void;
  // Drop only the precise (mute-resolved) flag while keeping the server in
  // serverUnreadSet. Used when a mute change makes a background server's
  // precise mark unverifiable, so it falls back to the server-level mute gate.
  // The channel-wins flag is a subset of precise, so it is dropped in lockstep.
  demoteServerPrecise: (serverId: string) => void;
  clearServerUnread: (serverId: string) => void;
  markServerMention: (serverId: string) => void;
  clearServerMention: (serverId: string) => void;
}

function recordServerUnreadMutation(state: UnreadState, serverId: string): Set<string> {
  if (!state.serverUnreadSeedPending || state.serverUnreadDirty.has(serverId)) {
    return state.serverUnreadDirty;
  }
  return new Set(state.serverUnreadDirty).add(serverId);
}

function recordChannelRead(state: UnreadState, serverId: string | null) {
  if (!state.serverUnreadSeedPending || serverId === null) return {};
  return {
    serverUnreadDirty: recordServerUnreadMutation(state, serverId),
    serverUnreadReadDirty: new Set(state.serverUnreadReadDirty).add(serverId),
  };
}

function withoutServer(ids: Set<string>, serverId: string): Set<string> {
  if (!ids.has(serverId)) return ids;
  const next = new Set(ids);
  next.delete(serverId);
  return next;
}

function clearServerIndicators(state: UnreadState, serverId: string) {
  return {
    serverUnreadSet: withoutServer(state.serverUnreadSet, serverId),
    serverMentionSet: withoutServer(state.serverMentionSet, serverId),
    serverUnreadPreciseSet: withoutServer(state.serverUnreadPreciseSet, serverId),
    serverUnreadChannelWinsSet: withoutServer(state.serverUnreadChannelWinsSet, serverId),
    serverUnreadDirty: recordServerUnreadMutation(state, serverId),
    serverUnreadReadDirty: withoutServer(state.serverUnreadReadDirty, serverId),
  };
}

function unreadChannelServerId(state: UnreadState, channelId: string): string | null {
  const entry = state.allUnreadCounts.get(channelId);
  if (entry) return entry.serverId;
  return state.unreadCountsSnapshotIds.has(channelId) ? state.unreadCountsServerId : null;
}

function hasRemainingServerUnread(
  counts: Map<string, number> | null,
  allCounts: UnreadState['allUnreadCounts'],
  serverId: string
): boolean {
  const channelIds = new Set<string>();
  // Live writes from the new server can enter the previous snapshot's map.
  // Exclude known foreign channels; legacy counts can lack cross-server rows.
  for (const channelId of counts?.keys() ?? []) {
    const entry = allCounts.get(channelId);
    if (!entry || entry.serverId === serverId) channelIds.add(channelId);
  }
  // A background arrival can be newer than the retained per-server snapshot.
  // Its count must keep the dot lit even if it never entered that snapshot.
  for (const [channelId, entry] of allCounts) {
    if (entry.serverId === serverId && entry.count > 0) channelIds.add(channelId);
  }
  return hasUnmutedChannel(channelIds, serverId);
}

function mergedServerUnread(allCounts: UnreadState['allUnreadCounts'], serverId: string) {
  let unread = false;
  let channelWins = false;
  for (const [channelId, entry] of allCounts) {
    if (entry.serverId !== serverId || entry.count <= 0 || isChannelMuted(channelId, serverId)) {
      continue;
    }
    unread = true;
    channelWins ||= hasChannelMuteOverride(channelId);
  }
  return { unread, channelWins };
}

function reconcileReadServerUnreads(
  state: UnreadState,
  unread: Set<string>,
  precise: Set<string>,
  channelWins: Set<string>
): Set<string> {
  let mentions = state.serverMentionSet;
  for (const serverId of state.serverUnreadReadDirty) {
    if (!state.allUnreadCompleteServerIds.has(serverId)) continue;
    const remaining = mergedServerUnread(state.allUnreadCounts, serverId);
    if (remaining.unread) {
      unread.add(serverId);
      precise.add(serverId);
      if (remaining.channelWins) channelWins.add(serverId);
      else channelWins.delete(serverId);
    } else {
      unread.delete(serverId);
      precise.delete(serverId);
      channelWins.delete(serverId);
      mentions = withoutServer(mentions, serverId);
    }
  }
  return mentions;
}

export const useUnreadStore = wrapStore(
  create<UnreadState>()(
    devtools(
      (set) => ({
        recentUnreadTargets: [],
        unreadCounts: new Map(),
        unreadCountsServerId: null,
        unreadCountsSnapshotIds: new Set<string>(),
        allUnreadCounts: new Map(),
        allUnreadCompleteServerIds: new Set<string>(),
        channelUnreadDirty: new Set<string>(),
        serverUnreadDirty: new Set<string>(),
        serverUnreadReadDirty: new Set<string>(),
        serverReadDirty: new Set<string>(),
        serverReadVersions: new Map<string, symbol>(),
        unreadResetId: Symbol('unread-reset'),
        serverUnreadSeedPending: false,
        channelUnreadSeedRequest: null,
        mentionCounts: new Map(),
        serverUnreadSet: new Set(),
        serverUnreadPreciseSet: new Set(),
        serverUnreadChannelWinsSet: new Set(),
        serverMentionSet: new Set(),

        setInitialUnreads: (counts: Map<string, number>, serverId: string | null = null) =>
          set({
            unreadCounts: new Map(counts),
            unreadCountsServerId: serverId,
            unreadCountsSnapshotIds: new Set(counts.keys()),
          }),

        setUnreadCount: (channelId: string, count: number) =>
          set((state) => {
            const next = new Map(state.unreadCounts);
            if (count > 0) {
              next.set(channelId, count);
            } else {
              next.delete(channelId);
            }
            return { unreadCounts: next };
          }),

        incrementUnread: (channelId: string) =>
          set((state) => {
            const current = state.unreadCounts.get(channelId) || 0;
            state.unreadCounts.set(channelId, current + 1);
            return {
              unreadCounts: new Map(state.unreadCounts),
              recentUnreadTargets: pushRecentUnreadTarget(state.recentUnreadTargets, {
                type: 'channel',
                targetId: channelId,
                ...(state.unreadCountsServerId ? { serverId: state.unreadCountsServerId } : {}),
              }),
            };
          }),

        clearUnread: (channelId: string) =>
          set((state) => {
            const hasUnread = state.unreadCounts.has(channelId);
            const hasMention = state.mentionCounts.has(channelId);
            // The cross-server map must follow the read too, or a BACKGROUND
            // channel's entry survives being read and keeps the badge lit (#2403).
            const hasAll = state.allUnreadCounts.has(channelId);
            const nextRecent = removeRecentUnreadTarget(state.recentUnreadTargets, channelId);
            if (!hasUnread && !hasMention && !hasAll) {
              return nextRecent === state.recentUnreadTargets
                ? state
                : { recentUnreadTargets: nextRecent };
            }
            const nextUnread = new Map(state.unreadCounts);
            const nextMention = new Map(state.mentionCounts);
            const nextAll = new Map(state.allUnreadCounts);
            nextUnread.delete(channelId);
            nextMention.delete(channelId);
            nextAll.delete(channelId);
            // A read during a bulk-seed window is the mirror of a bump: the
            // local state is fresher than the snapshot the server is computing,
            // and without this the seed re-adds a channel the user just read.
            const nextDirty = new Set(state.channelUnreadDirty);
            nextDirty.add(channelId);
            // Resolve positive ownership before the retained snapshot fallback.
            // A different snapshot owner supplies no counts or completeness for
            // this read; only validated complete bulk detail can prove it empty.
            const serverId = unreadChannelServerId(state, channelId);
            const ownsSnapshot = serverId === state.unreadCountsServerId;
            const clearOwnedServer =
              (hasUnread || hasAll) &&
              serverId !== null &&
              (ownsSnapshot || state.allUnreadCompleteServerIds.has(serverId)) &&
              !hasRemainingServerUnread(ownsSnapshot ? nextUnread : null, nextAll, serverId);
            return {
              recentUnreadTargets: nextRecent,
              unreadCounts: nextUnread,
              mentionCounts: nextMention,
              allUnreadCounts: nextAll,
              channelUnreadDirty: nextDirty,
              ...(clearOwnedServer ? clearServerIndicators(state, serverId) : {}),
              // Even an incomplete positive owner read must survive until the
              // detailed response supplies the missing completeness proof.
              ...recordChannelRead(state, serverId),
            };
          }),

        markServerRead: (serverId: string) =>
          set((state) => {
            const nextUnread = new Map(state.unreadCounts);
            const nextMention = new Map(state.mentionCounts);
            const nextAll = new Map(state.allUnreadCounts);
            const nextDirty = new Set(state.channelUnreadDirty);
            const channelIds = new Set([
              ...state.unreadCounts.keys(),
              ...state.allUnreadCounts.keys(),
              ...state.mentionCounts.keys(),
            ]);
            for (const channelId of channelIds) {
              // Positive cross-server ownership wins over captured snapshot
              // membership; unscoped live/DM entries never inherit its owner.
              if (unreadChannelServerId(state, channelId) !== serverId) continue;
              nextUnread.delete(channelId);
              nextMention.delete(channelId);
              nextAll.delete(channelId);
              nextDirty.add(channelId);
            }
            return {
              unreadCounts: nextUnread,
              mentionCounts: nextMention,
              allUnreadCounts: nextAll,
              channelUnreadDirty: nextDirty,
              serverReadDirty: state.serverUnreadSeedPending
                ? new Set(state.serverReadDirty).add(serverId)
                : state.serverReadDirty,
              serverReadVersions: new Map(state.serverReadVersions).set(
                serverId,
                Symbol('server-read')
              ),
              ...clearServerIndicators(state, serverId),
            };
          }),

        beginChannelUnreadSeed: (serverIds = new Set<string>()) => {
          const id = Symbol('channel-unread-seed');
          set({
            channelUnreadDirty: new Set<string>(),
            serverUnreadDirty: new Set<string>(),
            serverUnreadReadDirty: new Set<string>(),
            serverReadDirty: new Set<string>(),
            serverUnreadSeedPending: true,
            channelUnreadSeedRequest: { id, serverIds: new Set(serverIds) },
            allUnreadCompleteServerIds: new Set<string>(),
          });
          return id;
        },

        // Seeded from the bulk /servers/unread-status `channels` field.
        //
        // The response is authoritative for every channel EXCEPT the ones this
        // client changed while the request was in flight. Subscriptions start
        // before the fetch is issued, so an `unread_notify` can bump a channel —
        // or the user can read one — between the server computing its snapshot
        // and this applying it. A wholesale replace silently discarded both.
        //
        // The fence is a dirty SET rather than a value comparison, because the
        // two races need opposite answers: a bump means the local count is
        // HIGHER and correct, a read means it is LOWER (or gone) and correct.
        // `max()` would resurrect a channel the user just read; taking the seed
        // would drop a message that just arrived. Only "who changed" separates
        // them. `beginChannelUnreadSeed` opens the window; anything mutated
        // inside it keeps its local value and the seed loses that key alone.
        //
        // Sibling precedent: `setInitialServerUnreads` below takes the same
        // shape for the same reason — the bulk seed is authoritative for
        // membership and must not erase fresher local state.
        //
        // WHAT THE FENCE CANNOT SEE is WHY a channel is absent from the seed. It
        // reads absence as "nothing unread here", which is right for the timing
        // race it exists for and wrong for a permission one. Since the history
        // gate landed, `/servers/unread-status` also omits a channel the member
        // may SEE but not read history in, while the WebSocket `unread_notify`
        // path is gated on the view bit ALONE and still pushes for it. Such a
        // channel is therefore bumped locally, marked dirty, and then carried
        // across every seed that deliberately left it out — badge and poll
        // disagree until the member opens it and `clearUnread` drops it. Do not
        // "fix" this by trusting absence over a push the server just sent: that
        // is the wholesale replace this fence exists to close. The only seam
        // that can tell a permission omission from a timing one is the WS
        // delivery gate, which is server-side and out of this PR's scope.
        setInitialChannelUnreads: (rows, preserveDirty, completeServerIds) =>
          set((state) => {
            const next = new Map<string, { serverId: string; count: number }>();
            for (const r of rows) {
              if (
                preserveDirty &&
                (state.channelUnreadDirty.has(r.channelId) || state.serverReadDirty.has(r.serverId))
              )
                continue;
              next.set(r.channelId, { serverId: r.serverId, count: r.count });
            }
            if (preserveDirty) {
              // Carry every locally-changed entry across verbatim. A channel
              // read during the window has no entry to carry, which is exactly
              // right — it stays absent rather than being re-added by the seed.
              for (const channelId of state.channelUnreadDirty) {
                const local = state.allUnreadCounts.get(channelId);
                if (local) next.set(channelId, local);
              }
            }
            const complete = new Set<string>();
            for (const serverId of completeServerIds ?? []) {
              // Newly joined servers were absent from the request, and leaving
              // removes request membership even if the same server is rejoined.
              if (state.channelUnreadSeedRequest?.serverIds.has(serverId)) complete.add(serverId);
            }
            return {
              allUnreadCounts: next,
              allUnreadCompleteServerIds: complete,
              channelUnreadDirty: new Set<string>(),
            };
          }),

        // Maintains the cross-server map live. Unlike incrementUnread this is
        // reachable for a BACKGROUND server's channel, which is the case
        // unreadCounts structurally cannot represent.
        bumpChannelUnread: (channelId: string, serverId: string) =>
          set((state) => {
            const next = new Map(state.allUnreadCounts);
            const prev = next.get(channelId);
            next.set(channelId, { serverId, count: (prev?.count ?? 0) + 1 });
            const dirty = new Set(state.channelUnreadDirty);
            dirty.add(channelId);
            return {
              allUnreadCounts: next,
              channelUnreadDirty: dirty,
              recentUnreadTargets: pushRecentUnreadTarget(state.recentUnreadTargets, {
                type: 'channel',
                targetId: channelId,
                serverId,
              }),
            };
          }),

        clearServerChannelUnreads: (serverId: string) =>
          set((state) => {
            const next = new Map(state.allUnreadCounts);
            let removed = false;
            for (const [channelId, entry] of state.allUnreadCounts) {
              if (entry.serverId !== serverId) continue;
              next.delete(channelId);
              removed = true;
            }
            const complete = withoutServer(state.allUnreadCompleteServerIds, serverId);
            const request = state.channelUnreadSeedRequest;
            const nextRequest = request?.serverIds.has(serverId)
              ? { ...request, serverIds: withoutServer(request.serverIds, serverId) }
              : request;
            if (
              !removed &&
              complete === state.allUnreadCompleteServerIds &&
              nextRequest === request
            )
              return state;
            return {
              allUnreadCounts: next,
              allUnreadCompleteServerIds: complete,
              channelUnreadSeedRequest: nextRequest,
            };
          }),

        clearAll: () =>
          set({
            recentUnreadTargets: [],
            unreadResetId: Symbol('unread-reset'),
            serverReadVersions: new Map<string, symbol>(),
            unreadCounts: new Map(),
            unreadCountsServerId: null,
            unreadCountsSnapshotIds: new Set<string>(),
            allUnreadCounts: new Map(),
            allUnreadCompleteServerIds: new Set<string>(),
            channelUnreadDirty: new Set<string>(),
            serverUnreadDirty: new Set<string>(),
            serverUnreadReadDirty: new Set<string>(),
            serverReadDirty: new Set<string>(),
            serverUnreadSeedPending: false,
            channelUnreadSeedRequest: null,
            mentionCounts: new Map(),
            serverUnreadSet: new Set(),
            serverUnreadPreciseSet: new Set(),
            serverUnreadChannelWinsSet: new Set(),
            serverMentionSet: new Set(),
          }),

        incrementMention: (channelId: string) =>
          set((state) => {
            const current = state.mentionCounts.get(channelId) || 0;
            const next = new Map(state.mentionCounts);
            next.set(channelId, current + 1);
            return { mentionCounts: next };
          }),

        clearMentions: (channelId: string) =>
          set((state) => {
            if (state.mentionCounts.has(channelId)) {
              const next = new Map(state.mentionCounts);
              next.delete(channelId);
              return { mentionCounts: next };
            }
            return state;
          }),

        // Ordinary bulk decisions from /servers/unread-status are mute-UNAWARE,
        // so preserve rather than fabricate precise flags. Complete merged detail
        // can mute-resolve a pending channel read below. This request and the
        // active server's per-channel fetch fire
        // independently, so the bulk response can land AFTER a mute-aware path has
        // already promoted a server to precise. Unconditionally clearing here
        // would demote that entry and (for a muted server with an explicitly
        // unmuted unread channel) drop its dot until the next event/refetch.
        // So keep precise flags for IDs still present in the new seed, and drop
        // them for servers no longer unread (their mark is gone with their dot).
        setInitialServerUnreads: (serverIds: string[], preserveDirty = false) =>
          set((state) => {
            const nextUnread = new Set(serverIds);
            // Preserve local reads AND arrivals from this request's window,
            // including when the user switched away before it returned.
            if (preserveDirty) {
              for (const id of state.serverUnreadDirty) {
                if (state.serverUnreadSet.has(id)) nextUnread.add(id);
                else nextUnread.delete(id);
              }
            }
            const nextPrecise = new Set<string>();
            const nextChannelWins = new Set<string>();
            for (const id of state.serverUnreadPreciseSet) {
              if (nextUnread.has(id)) nextPrecise.add(id);
            }
            // Channel-wins is a subset of precise; keep it for surviving IDs too so
            // a late bulk seed does not strip a channel-wins override's protection.
            for (const id of state.serverUnreadChannelWinsSet) {
              if (nextUnread.has(id)) nextChannelWins.add(id);
            }
            // A read of A1 cannot veto untouched eligible A2 first discovered
            // in complete detail. Only read decisions are reconsidered: a
            // newer server-only arrival, per-server fetch or mute correction
            // still wins over this snapshot. Legacy/missing detail stays approximate.
            const nextMentions = preserveDirty
              ? reconcileReadServerUnreads(state, nextUnread, nextPrecise, nextChannelWins)
              : state.serverMentionSet;
            return {
              serverUnreadSet: nextUnread,
              serverUnreadPreciseSet: nextPrecise,
              serverUnreadChannelWinsSet: nextChannelWins,
              serverMentionSet: nextMentions,
              serverUnreadDirty: new Set<string>(),
              serverUnreadReadDirty: new Set<string>(),
              serverReadDirty: new Set<string>(),
              serverUnreadSeedPending: false,
            };
          }),

        markServerUnread: (serverId: string, precise = false, channelWins = false) =>
          set((state) => {
            const alreadyUnread = state.serverUnreadSet.has(serverId);
            const needsPrecise = precise && !state.serverUnreadPreciseSet.has(serverId);
            // Channel-wins protection only rides along with a precise mark and only
            // when not already recorded; a later channel-wins notify can promote a
            // plain precise entry so muting the server no longer demotes it.
            const needsChannelWins =
              precise && channelWins && !state.serverUnreadChannelWinsSet.has(serverId);
            const nextDirty = recordServerUnreadMutation(state, serverId);
            const nextReadDirty = withoutServer(state.serverUnreadReadDirty, serverId);
            // No-op only when the server is already flagged AND neither its
            // precision nor its channel-wins protection needs upgrading (an
            // approximate entry can be promoted to precise by a later mute-aware
            // mark).
            if (
              alreadyUnread &&
              !needsPrecise &&
              !needsChannelWins &&
              nextDirty === state.serverUnreadDirty &&
              nextReadDirty === state.serverUnreadReadDirty
            )
              return state;
            const nextUnread = alreadyUnread
              ? state.serverUnreadSet
              : new Set(state.serverUnreadSet).add(serverId);
            const nextPrecise = needsPrecise
              ? new Set(state.serverUnreadPreciseSet).add(serverId)
              : state.serverUnreadPreciseSet;
            const nextChannelWins = needsChannelWins
              ? new Set(state.serverUnreadChannelWinsSet).add(serverId)
              : state.serverUnreadChannelWinsSet;
            return {
              serverUnreadSet: nextUnread,
              serverUnreadPreciseSet: nextPrecise,
              serverUnreadChannelWinsSet: nextChannelWins,
              serverUnreadDirty: nextDirty,
              serverUnreadReadDirty: nextReadDirty,
            };
          }),

        demoteServerPrecise: (serverId: string) =>
          set((state) => {
            if (!state.serverUnreadPreciseSet.has(serverId)) return state;
            const nextPrecise = new Set(state.serverUnreadPreciseSet);
            nextPrecise.delete(serverId);
            // Channel-wins is a subset of precise; drop it in lockstep so a
            // demoted entry can't leave a dangling protection flag behind.
            let nextChannelWins = state.serverUnreadChannelWinsSet;
            if (nextChannelWins.has(serverId)) {
              nextChannelWins = new Set(nextChannelWins);
              nextChannelWins.delete(serverId);
            }
            return {
              serverUnreadPreciseSet: nextPrecise,
              serverUnreadChannelWinsSet: nextChannelWins,
            };
          }),

        clearServerUnread: (serverId: string) =>
          set((state) => {
            const hadUnread = state.serverUnreadSet.has(serverId);
            const hadMention = state.serverMentionSet.has(serverId);
            const hadPrecise = state.serverUnreadPreciseSet.has(serverId);
            const hadChannelWins = state.serverUnreadChannelWinsSet.has(serverId);
            const next = clearServerIndicators(state, serverId);
            // Clearing an already-dark dot is still a newer read decision
            // during a seed, so it must fence a stale positive response too.
            if (
              !hadUnread &&
              !hadMention &&
              !hadPrecise &&
              !hadChannelWins &&
              next.serverUnreadDirty === state.serverUnreadDirty &&
              next.serverUnreadReadDirty === state.serverUnreadReadDirty
            )
              return state;
            return next;
          }),

        markServerMention: (serverId: string) =>
          set((state) => {
            if (state.serverMentionSet.has(serverId)) return state;
            const next = new Set(state.serverMentionSet);
            next.add(serverId);
            return { serverMentionSet: next };
          }),

        clearServerMention: (serverId: string) =>
          set((state) => {
            if (!state.serverMentionSet.has(serverId)) return state;
            const next = new Set(state.serverMentionSet);
            next.delete(serverId);
            return { serverMentionSet: next };
          }),
      }),
      { name: 'UnreadStore' }
    )
  )
);
