/**
 * useServerChannelSubscriptions - Manages server-level unread notifications
 *
 * Two responsibilities:
 * 1. Subscribe to ALL user's servers for lightweight unread_notify pings
 *    (enables unread dots on server icons across servers).
 * 2. Fetch per-channel unread counts for the active server
 *    (enables unread badges on channel names).
 */

import { useEffect, useRef } from 'react';
import { useServerStore } from '../../stores/chat/serverStore';
import { useChatStore } from '../../stores/chat/chatStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useUnreadStore } from '../../stores/chat/unreadStore';
import { getWebSocketService } from '../../services/messaging/websocketService';
import {
  useNotificationPrefsStore,
  hasUnmutedChannel,
  isChannelMutedInMaps,
  isEntryCurrentlyMuted,
} from '../../stores/ui/notificationPrefsStore';
import { apiFetch } from '../../services/system/apiClient';
import { errorMessage } from '../../utils/runtime/redactError';

interface SeedRow {
  channelId: string;
  serverId: string;
  count: number;
}

/**
 * Validate and narrow the `/servers/unread-status` rows into seed entries.
 *
 * Extracted from the fetch closure rather than inlined because that closure
 * measured 16 against S3776's ceiling of 15, and a validation loop nested three
 * levels deep inside it is the most expensive thing there — every guard costs
 * double for the nesting. It is also the only genuinely pure part, so it can be
 * reasoned about, and tested, without a fetch.
 *
 * The row shape was an ASSERTION, not a check. A non-numeric `unread_count`
 * reaches `computeBadgeTotal` as NaN, and `NaN === last` is false forever —
 * which defeats the unchanged-total skip and turns every later store mutation
 * into an IPC round-trip. A malformed row is therefore dropped, not coerced.
 *
 * The ACTIVE channel is excluded, mirroring the per-server seed and for the same
 * stated reason: the user is looking at it, so it is not unread. Without this
 * the seed re-adds a count no read recorder can remove — `handleLatestSeen`
 * gates on `unreadCounts`, which structurally never holds the active channel —
 * so the badge kept a count for the conversation on screen and reading it did
 * not clear it. That is #2403's own symptom.
 */
function buildSeedRows(
  channels: unknown[],
  activeChannelId: string | null,
  knownServerIds: ReadonlySet<string>
): SeedRow[] | null {
  const rows: SeedRow[] = [];
  for (const entry of channels) {
    const row = entry as { channel_id?: unknown; server_id?: unknown; unread_count?: unknown };
    if (
      typeof row?.channel_id !== 'string' ||
      typeof row?.server_id !== 'string' ||
      !Number.isFinite(row?.unread_count)
    ) {
      // ALL OR NOTHING. Dropping the row and committing the rest looks
      // conservative and is not: setInitialChannelUnreads REPLACES
      // allUnreadCounts, so a dropped row does not merely fail to be added —
      // it deletes whatever that channel's count already was. A partial seed
      // therefore silently UNDERCOUNTS the badge, which is #2403's symptom
      // arriving through the validator meant to prevent it. A type violation
      // here is a server contract break, not a transient, so refusing the whole
      // payload and retrying is the honest response.
      console.error('[unread] malformed unread-status row — refusing the whole seed');
      return null;
    }
    // A row for a server we are no longer in is NOT malformed — it is the
    // ordinary race where the user left a server while this request was in
    // flight. removeServer already dropped that server's rows, and committing
    // this one would restore them with nothing able to clear them again. Skip
    // it; do not refuse the payload over it.
    if (!knownServerIds.has(row.server_id)) continue;
    if (row.channel_id === activeChannelId) continue;
    rows.push({
      channelId: row.channel_id,
      serverId: row.server_id,
      count: row.unread_count as number,
    });
  }
  return rows;
}

export function useServerChannelSubscriptions() {
  const servers = useServerStore((s) => s.servers);
  const activeServerId = useServerStore((s) => s.activeServerId);
  const isConnected = useChatStore((s) => s.isConnected);
  const setInitialUnreads = useUnreadStore((s) => s.setInitialUnreads);
  const setInitialServerUnreads = useUnreadStore((s) => s.setInitialServerUnreads);
  const markServerUnread = useUnreadStore((s) => s.markServerUnread);
  const clearServerUnread = useUnreadStore((s) => s.clearServerUnread);
  const demoteServerPrecise = useUnreadStore((s) => s.demoteServerPrecise);
  // Subscribe to the mute maps so the server-dot recompute below re-runs the
  // instant a channel/server mute is toggled (#84 / epic #1029 close audit,
  // P2 review follow-up). Map identity changes on every setMute/removeMute.
  const mutedChannels = useNotificationPrefsStore((s) => s.mutedChannels);
  const mutedServers = useNotificationPrefsStore((s) => s.mutedServers);
  const subscribedServersRef = useRef<Set<string>>(new Set());

  // Subscribe to ALL servers for unread notifications + fetch server-level unread status.
  // Server subscriptions are long-lived (persist until WS disconnect or server leave).
  // The ref-based diffing handles additions/removals without full unsubscribe cycles.
  // No cleanup unsubscribe — WS reconnect resubscribes via resubscribeChannels(),
  // and logout disconnects the WS entirely. Removing cleanup prevents StrictMode
  // double-mount from causing 3x subscribe/unsubscribe thrashing (~60 events → ~17).
  const unreadsFetchedRef = useRef(false);
  // Generation fence for the bulk seed. `unreadsFetchedRef` is a latch, not an
  // identity: a disconnect (or a failed response) releases it while the earlier
  // request is still in flight, so a reconnect can start a second one and the
  // two can resolve in either order. The older one committing last restores
  // stale server state over a local read; the older one committing FIRST clears
  // `channelUnreadDirty`, after which the newer one overwrites every local
  // change the fence existed to protect. Only the newest request may commit.
  const unreadSeedGenerationRef = useRef(0);

  useEffect(() => {
    if (!isConnected || servers.length === 0) {
      // Invalidate any seed still in flight. Its response describes a session
      // that has ended, and the reconnect below will issue a fresh one.
      unreadSeedGenerationRef.current += 1;
      return;
    }

    const wsService = getWebSocketService();
    const currentServerIds = new Set(servers.map((s) => s.id));

    // Subscribe to new servers
    for (const serverId of currentServerIds) {
      if (!subscribedServersRef.current.has(serverId)) {
        wsService.subscribeServer(serverId);
        subscribedServersRef.current.add(serverId);
      }
    }

    // Unsubscribe from removed servers (e.g. user left a server)
    for (const serverId of subscribedServersRef.current) {
      if (!currentServerIds.has(serverId)) {
        wsService.unsubscribeServer(serverId);
        subscribedServersRef.current.delete(serverId);
      }
    }

    // Fetch server-level unread status once per connection (not on every re-render)
    if (!unreadsFetchedRef.current) {
      unreadsFetchedRef.current = true;
      // Open the fence BEFORE the request: anything the user reads, or any
      // unread_notify that lands, while this is in flight is fresher than the
      // snapshot the server is computing, and must survive the seed.
      const seedGeneration = (unreadSeedGenerationRef.current += 1);
      useUnreadStore.getState().beginChannelUnreadSeed();
      (async () => {
        const isCurrentSeed = () => seedGeneration === unreadSeedGenerationRef.current;
        try {
          const res = await apiFetch('/api/v1/servers/unread-status');
          if (!isCurrentSeed()) return;
          if (!res.ok) {
            // apiFetch RESOLVES for a non-2xx, so without this arm every 500,
            // 401 and 429 fell out of the IIFE with no diagnostic at all — the
            // `catch` below only ever saw thrown errors. Since #2403 this is the
            // badge's ONLY cross-server backfill (the per-server fetch does not
            // touch allUnreadCounts), so one failure at launch reads to the user
            // as "nothing unread anywhere" — the defect this endpoint exists to
            // fix, arriving through the error path.
            console.error(
              `[unread] /servers/unread-status failed: ${res.status} — unread state not seeded`
            );
            // Releasing the latch ENABLES a retry rather than scheduling one:
            // this effect re-runs on `servers`/`isConnected`, so recovery waits
            // for the next of those. That is a real bound and it is the honest
            // one — a timer here would be machinery for a case a reconnect
            // already heals. What it removes is the silent never.
            unreadsFetchedRef.current = false;
            return;
          }
          const data = await res.json();
          // Second fence: `res.json()` is a second await, so a newer seed can
          // have started and committed inside it. Everything below this line is
          // synchronous, so this is the last point a stale continuation can be
          // stopped before it writes.
          if (!isCurrentSeed()) return;
          // #2403: per-channel counts across every server. Absent from an
          // older control plane, in which case the badge falls back to DM
          // unread alone rather than reporting a wrong number.
          if (Array.isArray(data.channels)) {
            // The ACTIVE channel is excluded, mirroring the per-server seed
            // below and for the same stated reason: the user is looking at it,
            // so it is not unread. Without this the seed re-adds a count that
            // no read recorder can remove — `handleLatestSeen` gates on
            // `unreadCounts`, which structurally never holds the active channel
            // — so the badge kept a count for the conversation on screen and
            // reading it did not clear it. That is #2403's own symptom.
            const activeChannelId = useChannelStore.getState().activeChannelId;
            const knownServerIds = new Set(useServerStore.getState().servers.map((srv) => srv.id));
            const seedRows = buildSeedRows(data.channels, activeChannelId, knownServerIds);
            if (seedRows === null) {
              // Keep whatever is already known and let a later effect run retry,
              // rather than committing a payload we could not fully parse.
              unreadsFetchedRef.current = false;
              return;
            }
            useUnreadStore.getState().setInitialChannelUnreads(seedRows, true);
          }
          // KNOWN CEILING (#84 / epic #1029 close audit): the server does not
          // filter /servers/unread-status by mute, so a background server whose
          // ONLY unreads are muted channels can still show a dot until its
          // per-channel fetch runs. Upgrade path is server-side mute filtering.
          //
          // AMENDED #2403 — the ceiling holds, one of its two stated reasons no
          // longer does. The payload DOES carry per-channel detail now (see the
          // `channels` seed above), so a consumer willing to resolve mute itself
          // is no longer blocked; the desktop badge does exactly that, filtering
          // through isChannelMutedInMaps at compute time. What remains is that
          // `server_ids` — this line's input — is still a mute-unaware boolean
          // per server. So the ceiling now bites the DOT only, not the badge.
          // Deriving the dot from `channels` here would close it for real, and is
          // deliberately out of scope: it changes #84's behaviour, not #2403's.
          setInitialServerUnreads(data.server_ids || []);

          // Reconcile the ACTIVE server against its own per-channel data.
          // This bulk seed is mute-UNAWARE and races the active server's
          // per-channel fetch: if that fetch already cleared a muted-only
          // active server's dot, the seed can re-add it as an approximate
          // unread, which isServerUnreadVisible then shows permanently (the
          // server itself isn't muted) with no per-channel correction until a
          // switch/reconnect (unreadFetchServerRef is latched). When the
          // per-channel counts already belong to the active server, re-derive
          // its dot from them so a mute-only server goes back dark
          // (#84 / epic #1029 close audit, P2 review follow-up).
          const activeId = useServerStore.getState().activeServerId;
          const { unreadCounts, unreadCountsServerId } = useUnreadStore.getState();
          if (activeId && unreadCountsServerId === activeId) {
            if (hasUnmutedChannel(unreadCounts.keys(), activeId)) {
              useUnreadStore.getState().markServerUnread(activeId, true);
            } else {
              useUnreadStore.getState().clearServerUnread(activeId);
            }
          }
        } catch (err) {
          console.error('Failed to fetch server unread status:', errorMessage(err));
          // Same reason as the non-ok arm above: a network blip must not latch
          // the badge dark for the whole connection. Fenced for the same reason
          // every commit above is — a STALE request failing must not release the
          // latch a newer in-flight request is holding, which would let a third
          // request start while the second is still running.
          if (isCurrentSeed()) unreadsFetchedRef.current = false;
        }
      })();
    }
  }, [isConnected, servers, setInitialServerUnreads]);

  // Reset the unread fetch flag when WS disconnects so it refetches on reconnect
  useEffect(() => {
    if (!isConnected) {
      unreadsFetchedRef.current = false;
    }
  }, [isConnected]);

  // Fetch per-channel unreads for the active server.
  // Ref guard prevents StrictMode double-mount from firing duplicate HTTP requests.
  const unreadFetchServerRef = useRef<string | null>(null);

  useEffect(() => {
    if (!isConnected || !activeServerId) return;

    // NOTE: Do NOT eagerly clearServerUnread here. The server dot should
    // only clear when all channels are actually read (handled in ChannelList).

    // StrictMode dedup: skip if we already fetched for this server
    if (unreadFetchServerRef.current === activeServerId) return;
    unreadFetchServerRef.current = activeServerId;

    // Guard against stale responses from rapid server switches or WS reconnects.
    let aborted = false;

    const fetchUnreads = async () => {
      try {
        const res = await apiFetch(`/api/v1/servers/${activeServerId}/unread`);
        if (aborted) return;
        if (res.ok) {
          const data = await res.json();
          if (aborted) return;
          const counts = new Map<string, number>();

          // Filter out the currently active channel — user is already viewing it,
          // so it shouldn't show as unread. This prevents the race condition where
          // setInitialUnreads overwrites a clearUnread that already ran.
          const currentActiveChannel = useChannelStore.getState().activeChannelId;
          for (const entry of data.unreads || []) {
            if (entry.unread_count > 0 && entry.channel_id !== currentActiveChannel) {
              counts.set(entry.channel_id, entry.unread_count);
            }
          }
          setInitialUnreads(counts, activeServerId);

          // Update server dot based on remaining unreads — but a muted
          // channel must NOT light the server dot (#84 acceptance criterion;
          // epic #1029 close audit). Mark only when at least one unread
          // channel is not effectively muted. Counts stay seeded so un-muting
          // reveals the badge without a refetch.
          if (hasUnmutedChannel(counts.keys(), activeServerId)) {
            markServerUnread(activeServerId, true);
          } else {
            clearServerUnread(activeServerId);
          }
        }
      } catch (err) {
        console.error('Failed to fetch unread counts:', errorMessage(err));
      }
    };
    fetchUnreads();

    // No clearAll() here — setInitialUnreads already replaces the whole map,
    // and clearing eagerly during WS reconnects can cause stale DB data to
    // resurface as phantom unread badges on already-read channels.
    return () => {
      aborted = true;
    };
  }, [activeServerId, isConnected, setInitialUnreads, markServerUnread, clearServerUnread]);

  // Recompute the active server's unread dot when a channel/server mute is
  // TOGGLED — not just when unreads arrive or a per-channel fetch runs
  // (#84 / epic #1029 close audit, P2 review follow-up). Without this, muting
  // the last unmuted unread channel leaves the server icon lit, and un-muting
  // a channel whose count is still seeded never brings the dot back.
  //
  // Per-channel unread data (unreadCounts) only exists for the ACTIVE server,
  // which is also the only server whose channels the user can mute (channel
  // mutes are issued from its channel list), so recomputing here covers the
  // interactive path. We read unreadCounts/activeServerId via getState so this
  // fires solely on mute-map changes (server switches + arrivals are already
  // handled above); an empty count map is a no-op, so it never clobbers the
  // fetch-seeded dot before per-channel data has loaded.
  useEffect(() => {
    const activeId = useServerStore.getState().activeServerId;
    const {
      unreadCounts,
      unreadCountsServerId,
      serverUnreadPreciseSet,
      serverUnreadChannelWinsSet,
    } = useUnreadStore.getState();

    // Reconcile BACKGROUND servers first. The active-server recompute below
    // can only see the active server's per-channel unread data, so a non-active
    // server that was marked precise from a channel with NO mute override and is
    // now server-muted would otherwise keep a stale precise dot forever. Demote
    // those precise flags so they fall back to the server-level mute gate and
    // go dark; a genuine unmuted-channel unread re-promotes them on the next
    // unread_notify or when the server is opened. This trades a rare,
    // self-correcting false-negative for not violating the mute contract with a
    // persistent false-positive (#84 / epic #1029 close audit, P2 follow-up).
    //
    // Channel-wins precise marks are exempt: a precise flag that came from a
    // channel explicitly unmuted under a muted server is exactly what
    // isServerUnreadVisible is designed to honor, so muting the server (or, since
    // this effect reruns on every mute toggle, any unrelated server) must NOT
    // drop it. Only server-fallback precise marks are demotable here (P2 review
    // follow-up 3562576306).
    for (const serverId of serverUnreadPreciseSet) {
      if (serverId === activeId) continue;
      if (serverUnreadChannelWinsSet.has(serverId)) continue;
      if (isEntryCurrentlyMuted(mutedServers.get(serverId))) {
        demoteServerPrecise(serverId);
      }
    }

    // Active server: recompute its dot from live per-channel unread data.
    if (!activeId) return;
    // Only recompute from counts that actually belong to the active server.
    // On a server switch the global unreadCounts still holds the PREVIOUS
    // server's data until /servers/{activeId}/unread lands; pairing those
    // channel IDs with the newly opened server's mute state would mark or clear
    // the wrong server's dot. Skipping until ownership catches up also means an
    // empty map here is a genuine "active server has no unreads" (its fetch
    // returned nothing), not "data not loaded yet" (#84 / epic #1029 close
    // audit, P2 review follow-up).
    if (unreadCountsServerId !== activeId) return;
    const hasUnmutedUnread = [...unreadCounts.keys()].some(
      (channelId) => !isChannelMutedInMaps(channelId, activeId, mutedChannels, mutedServers)
    );
    if (hasUnmutedUnread) {
      markServerUnread(activeId, true);
    } else {
      clearServerUnread(activeId);
    }
  }, [mutedChannels, mutedServers, markServerUnread, clearServerUnread, demoteServerPrecise]);
}
