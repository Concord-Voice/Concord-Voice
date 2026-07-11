import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

interface UnreadState {
  // Channel ID -> unread count (per-channel, active server only)
  unreadCounts: Map<string, number>;

  // Which server `unreadCounts` currently describes, or null before any
  // per-channel fetch has landed. The map is replaced wholesale on each active
  // server's fetch but is NOT cleared on server switch, so consumers that pair
  // it with the live activeServerId (the mute-toggle recompute) must confirm
  // ownership first; otherwise they evaluate the previous server's channel IDs
  // under the new server's mute state (#84 / epic #1029 close audit, P2 review).
  unreadCountsServerId: string | null;

  // Channel or DM conversation ID -> mention count (subset of unreadCounts where user was @mentioned)
  mentionCounts: Map<string, number>;

  // Server IDs that have any unread messages (cross-server)
  serverUnreadSet: Set<string>;

  // Subset of serverUnreadSet whose membership was decided by a mute-AWARE
  // source (unread_notify, the per-channel fetch, the mute-toggle recompute,
  // and the DND-off refresh — all resolve channel-wins mutes before marking).
  // The bulk /servers/unread-status seed is mute-UNAWARE (see the KNOWN CEILING
  // in useServerChannelSubscriptions) and therefore marks servers as NOT precise.
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
  clearAll: () => void;

  incrementMention: (channelId: string) => void;
  clearMentions: (channelId: string) => void;

  setInitialServerUnreads: (serverIds: string[]) => void;
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

export const useUnreadStore = wrapStore(create<UnreadState>()(
  devtools(
    (set) => ({
      unreadCounts: new Map(),
      unreadCountsServerId: null,
      mentionCounts: new Map(),
      serverUnreadSet: new Set(),
      serverUnreadPreciseSet: new Set(),
      serverUnreadChannelWinsSet: new Set(),
      serverMentionSet: new Set(),

      setInitialUnreads: (counts: Map<string, number>, serverId: string | null = null) =>
        set({ unreadCounts: new Map(counts), unreadCountsServerId: serverId }),

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
          return { unreadCounts: new Map(state.unreadCounts) };
        }),

      clearUnread: (channelId: string) =>
        set((state) => {
          const hasUnread = state.unreadCounts.has(channelId);
          const hasMention = state.mentionCounts.has(channelId);
          if (!hasUnread && !hasMention) return state;
          const nextUnread = new Map(state.unreadCounts);
          const nextMention = new Map(state.mentionCounts);
          nextUnread.delete(channelId);
          nextMention.delete(channelId);
          return { unreadCounts: nextUnread, mentionCounts: nextMention };
        }),

      clearAll: () =>
        set({
          unreadCounts: new Map(),
          unreadCountsServerId: null,
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

      // Bulk seed from /servers/unread-status: mute-UNAWARE, so it must never
      // FABRICATE precise flags. But it must not ERASE fresher mute-aware state
      // either: this request and the active server's per-channel fetch fire
      // independently, so the bulk response can land AFTER a mute-aware path has
      // already promoted a server to precise. Unconditionally clearing here
      // would demote that entry and (for a muted server with an explicitly
      // unmuted unread channel) drop its dot until the next event/refetch.
      // So keep precise flags for IDs still present in the new seed, and drop
      // them for servers no longer unread (their mark is gone with their dot).
      setInitialServerUnreads: (serverIds: string[]) =>
        set((state) => {
          const nextUnread = new Set(serverIds);
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
          return {
            serverUnreadSet: nextUnread,
            serverUnreadPreciseSet: nextPrecise,
            serverUnreadChannelWinsSet: nextChannelWins,
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
          // No-op only when the server is already flagged AND neither its
          // precision nor its channel-wins protection needs upgrading (an
          // approximate entry can be promoted to precise by a later mute-aware
          // mark).
          if (alreadyUnread && !needsPrecise && !needsChannelWins) return state;
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
          if (!hadUnread && !hadMention && !hadPrecise && !hadChannelWins) return state;
          const nextUnread = new Set(state.serverUnreadSet);
          const nextMention = new Set(state.serverMentionSet);
          const nextPrecise = new Set(state.serverUnreadPreciseSet);
          const nextChannelWins = new Set(state.serverUnreadChannelWinsSet);
          nextUnread.delete(serverId);
          nextMention.delete(serverId);
          nextPrecise.delete(serverId);
          nextChannelWins.delete(serverId);
          return {
            serverUnreadSet: nextUnread,
            serverMentionSet: nextMention,
            serverUnreadPreciseSet: nextPrecise,
            serverUnreadChannelWinsSet: nextChannelWins,
          };
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
));
