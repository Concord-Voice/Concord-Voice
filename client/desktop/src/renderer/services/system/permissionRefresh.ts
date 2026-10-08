/**
 * Permission refresh scheduling for the two permission-change WebSocket events
 * (#3456, spec 3.8).
 *
 * - `server_permissions_changed` (a server's MFA enforcement changed): every
 *   member's permissions on that server may differ, so every connected member
 *   re-reads them. The refetch is delayed by uniform 0-5 s jitter per server so
 *   a large server does not turn one toggle into a synchronized burst against a
 *   route capped at 30/min/user. One timer per server; an event that arrives
 *   while it is pending is DROPPED, not re-armed, because the refetch it would
 *   schedule reads the same state.
 * - `permissions_changed` (this account's own inline factors changed): a 300 ms
 *   trailing debounce, then ONE `fetchServers()` that begins after the event and
 *   the active server's and active channel's permissions. Three requests at most,
 *   never one per server. Every OTHER cached server and channel answer is evicted
 *   rather than re-read, so the next visit reads it.
 *
 * A failed refetch is NOT retried and nothing here re-arms itself: the stale
 * display stands until the next navigation, which is what keeps the burst bound
 * above honest. The server remains the authority for every decision.
 *
 * Timers are cleared by `dispose()` and whenever the signed-in ACCOUNT changes.
 * The second matters beyond tidiness: a pending timer left by the previous
 * account would drop the next account's event for the same server.
 */

import { useAuthStore } from '../../stores/auth/authStore';
import { useChannelStore } from '../../stores/chat/channelStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { fullJitter } from '../messaging/websocketService';
import { captureAuthLifecycle, isSameAuthLifecycle } from './postLoginHydrationLifecycle';

/** Upper bound (exclusive) of the per-server jitter before a server refetch. */
export const SERVER_REFRESH_JITTER_MS = 5000;

/** Trailing-edge debounce for the account-wide event. */
export const OWN_REFRESH_DEBOUNCE_MS = 300;

/**
 * One coalesced, jittered, cancellable re-read: the contract of the per-server
 * timer below, for a consumer that re-reads something of its own on
 * `server_permissions_changed` (the MFA-enforcement switch re-GETs its setting).
 */
export interface JitteredRefetch {
  /** Arm the timer; a request that arrives while it is pending is DROPPED. */
  request: () => void;
  /** Disarm it (unmount, server change). A fired timer has nothing to cancel. */
  cancel: () => void;
}

export function createJitteredRefetch(
  run: () => void,
  // Wrapped so `fullJitter` is read when an event arrives, not when this is
  // created: many suites mount a consumer over a websocketService stub without it.
  jitterMs: (maxMs: number) => number = (maxMs) => fullJitter(maxMs)
): JitteredRefetch {
  let timer: ReturnType<typeof setTimeout> | null = null;
  return {
    request: () => {
      if (timer !== null) return;
      timer = setTimeout(() => {
        timer = null;
        run();
      }, jitterMs(SERVER_REFRESH_JITTER_MS));
    },
    cancel: () => {
      if (timer !== null) clearTimeout(timer);
      timer = null;
    },
  };
}

export interface PermissionRefreshOptions {
  /** Delay in [0, maxMs) for a server refetch. Injectable so tests need no clock tricks. */
  jitterMs?: (maxMs: number) => number;
}

export interface PermissionRefresh {
  /** `server_permissions_changed` for one server. */
  serverPermissionsChanged: (serverId: string) => void;
  /** `permissions_changed` for the signed-in account. */
  ownPermissionsChanged: () => void;
  /** Cancel every pending timer and stop watching the account. */
  dispose: () => void;
}

function isKnownServer(serverId: string): boolean {
  return useServerStore.getState().servers.some((server) => server.id === serverId);
}

/** The active channel, but only when it is one of `serverId`'s. */
function activeChannelOf(serverId: string): string | null {
  const { activeChannelId, channelIdsByServer } = useChannelStore.getState();
  if (activeChannelId === null) return null;
  return (channelIdsByServer[serverId] ?? []).includes(activeChannelId) ? activeChannelId : null;
}

/**
 * Re-read what a server's permissions change can have invalidated: its own
 * effective permissions (only if they were ever read), the active channel's, and
 * evict every other channel's cached answer, which the next visit re-reads.
 */
function refreshServer(serverId: string): void {
  if (!isKnownServer(serverId)) return;
  const permissions = usePermissionStore.getState();
  const activeChannelId = activeChannelOf(serverId);
  const others = (useChannelStore.getState().channelIdsByServer[serverId] ?? []).filter(
    (channelId) => channelId !== activeChannelId
  );
  permissions.evictChannelPermissions(others);
  if (permissions.serverPermissions[serverId] !== undefined) {
    void permissions.fetchServerPermissions(serverId);
  }
  if (activeChannelId !== null) void permissions.fetchChannelPermissions(activeChannelId);
}

/**
 * Resolves when the server-list fetch in flight settles. `fetchServers()` called
 * while one is out returns AT ONCE (it dedups), and that fetch, begun before the
 * change, can still commit pre-change `servers[].permissions` after the call has
 * returned, so a caller that needs a read begun after the change waits first.
 */
function serverFetchSettled(): Promise<void> {
  return new Promise((resolve) => {
    const unsubscribe = useServerStore.subscribe((state) => {
      if (state.isLoading) return;
      unsubscribe();
      resolve();
    });
  });
}

/**
 * Re-read the account's memberships, then the active server's and active
 * channel's permissions, and evict every other cached answer: the account's own
 * factors changed, so a server or channel it is not looking at is just as stale
 * (the "Set up verification" item would keep showing there after enrolling, or
 * unmasked bits would survive a removed factor). The next visit re-reads.
 *
 * `fetchServers()` is called WITHOUT a hydration guard on purpose: a guarded
 * (Recovery-A, #2329) fetch never dedups and would supersede one whose caller is
 * awaiting its authoritative commit. Unguarded, it would dedup into a fetch
 * already in flight and re-read nothing, so that fetch is waited out first (it
 * may be a Recovery-A one, which commits its own answer) and the fetch that
 * follows begins after the change. The account check after the awaits keeps a
 * sign-out or switch that landed meanwhile from reading the NEW account's
 * active server.
 */
async function refreshOwn(): Promise<void> {
  const lifecycle = captureAuthLifecycle();
  if (useServerStore.getState().isLoading) await serverFetchSettled();
  await useServerStore.getState().fetchServers();
  if (!isSameAuthLifecycle(lifecycle)) return;
  const activeServerId = useServerStore.getState().activeServerId;
  const permissions = usePermissionStore.getState();
  const activeChannelId = activeServerId === null ? null : activeChannelOf(activeServerId);
  permissions.evictServerPermissions(
    Object.keys(permissions.serverPermissions).filter((serverId) => serverId !== activeServerId)
  );
  permissions.evictChannelPermissions(
    Object.keys(permissions.channelPermissions).filter((channelId) => channelId !== activeChannelId)
  );
  if (activeServerId === null) return;
  void permissions.fetchServerPermissions(activeServerId);
  if (activeChannelId !== null) void permissions.fetchChannelPermissions(activeChannelId);
}

export function createPermissionRefresh(options: PermissionRefreshOptions = {}): PermissionRefresh {
  const serverRefetches = new Map<string, JitteredRefetch>();
  let ownTimer: ReturnType<typeof setTimeout> | null = null;

  const cancelAll = () => {
    for (const refetch of serverRefetches.values()) refetch.cancel();
    serverRefetches.clear();
    if (ownTimer !== null) clearTimeout(ownTimer);
    ownTimer = null;
  };

  // Compares `authGeneration`, never a credential: a routine token rotation
  // keeps the account and must not cancel a pending refresh.
  const unsubscribeAuth = useAuthStore.subscribe((state, previous) => {
    if (state.authGeneration !== previous.authGeneration) cancelAll();
  });

  return {
    serverPermissionsChanged: (serverId) => {
      if (!isKnownServer(serverId)) return;
      let refetch = serverRefetches.get(serverId);
      if (refetch === undefined) {
        refetch = createJitteredRefetch(() => refreshServer(serverId), options.jitterMs);
        serverRefetches.set(serverId, refetch);
      }
      refetch.request();
    },
    ownPermissionsChanged: () => {
      if (ownTimer !== null) clearTimeout(ownTimer);
      ownTimer = setTimeout(() => {
        ownTimer = null;
        void refreshOwn();
      }, OWN_REFRESH_DEBOUNCE_MS);
    },
    dispose: () => {
      unsubscribeAuth();
      cancelAll();
    },
  };
}
