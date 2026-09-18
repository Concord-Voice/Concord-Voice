import { useUnreadStore } from '../../stores/chat/unreadStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { useServerStore } from '../../stores/chat/serverStore';
import {
  useNotificationPrefsStore,
  isChannelMutedInMaps,
  isEntryCurrentlyMuted,
  type MuteEntry,
} from '../../stores/ui/notificationPrefsStore';

/**
 * The desktop badge is DERIVED, never accumulated (#2403).
 *
 * It used to be a counter in desktopNotificationService with two increment
 * sites and one clear site, and that clear lived inside the notification-click
 * navigation subscriber — so the badge cleared only if the user clicked the OS
 * popup. Alt-tabbing back, clicking the Dock icon, or opening the channel in the
 * sidebar left it stuck, and a reload could not correct it because nothing
 * re-asserted the value main had already pushed to the OS.
 *
 * Deriving it means every existing read-recorder clears the badge with no call
 * site of its own, and starting the subscription re-asserts the true value.
 *
 * Mute is resolved HERE, at compute time, rather than at write time: the bulk
 * seed cannot filter by mute (no server-side mute resolution exists) and a timed
 * mute expires without any store changing.
 */

/**
 * The mute state every summing helper below needs, read once per recompute.
 *
 * Passed as one object rather than three parameters because all three helpers
 * want the same snapshot, and a per-helper `getState()` could read a DIFFERENT
 * one: `push` runs synchronously inside `setState`, so a mute change committing
 * between two reads would have the badge sum two halves of different worlds.
 */
interface MuteSnapshot {
  mutedChannels: Map<string, MuteEntry>;
  mutedServers: Map<string, MuteEntry>;
  mutedDMs: Map<string, MuteEntry>;
}

/**
 * A malformed count reaching any of the three helpers below is a bug upstream,
 * but `total` must not become NaN: `NaN === last` is false forever, which would
 * defeat `push`'s unchanged-total skip and turn every store mutation into an IPC
 * call. Hence the `Number.isFinite` guard in each of them.
 */
function sumBackgroundServers(
  allUnreadCounts: Map<string, { serverId: string; count: number }>,
  activeServerId: string | null,
  skipActiveServer: boolean,
  mutes: MuteSnapshot
): number {
  let total = 0;
  for (const [channelId, { serverId, count }] of allUnreadCounts) {
    if (skipActiveServer && serverId === activeServerId) continue;
    if (isChannelMutedInMaps(channelId, serverId, mutes.mutedChannels, mutes.mutedServers))
      continue;
    if (!Number.isFinite(count)) continue;
    total += count;
  }
  return total;
}

function sumActiveServer(
  unreadCounts: Map<string, number>,
  activeServerId: string,
  mutes: MuteSnapshot
): number {
  let total = 0;
  for (const [channelId, count] of unreadCounts) {
    if (isChannelMutedInMaps(channelId, activeServerId, mutes.mutedChannels, mutes.mutedServers)) {
      continue;
    }
    if (!Number.isFinite(count)) continue;
    total += count;
  }
  return total;
}

function sumDirectMessages(
  conversations: Array<{ id: string; unreadCount: number }>,
  mutedDMs: Map<string, MuteEntry>
): number {
  let total = 0;
  for (const conversation of conversations) {
    if (isEntryCurrentlyMuted(mutedDMs.get(conversation.id))) continue;
    if (!Number.isFinite(conversation.unreadCount)) continue;
    total += conversation.unreadCount;
  }
  return total;
}

/**
 * ONE channel's count is read from exactly one map, and which map depends on
 * whether its server is the active one.
 *
 * `unreadCounts` is the ACTIVE server's authoritative per-channel map. It is the
 * one every in-app reconciliation already maintains: the per-server fetch
 * filters the channel the user is looking at out of it, `setUnreadCount`
 * restores a count on leave, the DND-off refresh repairs it, and
 * `incrementUnread` feeds it even for a notify that carried no `server_id`.
 *
 * `allUnreadCounts` exists only because `unreadCounts` is active-server-scoped
 * and a BACKGROUND server's unread has nowhere else to live.
 *
 * Reading both — the first version of this function summed `allUnreadCounts`
 * alone — made the two maps a pair that every unread writer had to update
 * together, and five of them did not. The worst case was self-inflicted: the
 * bulk seed writes the active channel's count (its per-server sibling
 * deliberately filters that channel out), and `handleLatestSeen` clears it only
 * when `unreadCounts.get(activeChannelId) > 0`, which is never true for the open
 * channel. The badge then showed a count for the conversation on screen and
 * reading it did not clear it — #2403's own symptom, through the new path.
 *
 * So the active server is sourced from `unreadCounts` and skipped in
 * `allUnreadCounts`. The guard is `unreadCountsServerId === activeServerId`,
 * the same staleness test `useServerChannelSubscriptions` already applies at its
 * own two read sites: `unreadCounts` is replaced wholesale on a server switch
 * and is NOT cleared on the way out, so without that check a switch would read
 * the previous server's counts as the new one's. When it does not hold, the
 * active server falls back to `allUnreadCounts` rather than contributing
 * nothing — a slightly stale count beats a silently absent one.
 */
export function computeBadgeTotal(): number {
  const { allUnreadCounts, unreadCounts, unreadCountsServerId } = useUnreadStore.getState();
  const { mutedChannels, mutedServers, mutedDMs } = useNotificationPrefsStore.getState();
  const mutes: MuteSnapshot = { mutedChannels, mutedServers, mutedDMs };
  const activeServerId = useServerStore.getState().activeServerId;
  const activeFromUnreadCounts = activeServerId !== null && unreadCountsServerId === activeServerId;

  // Three sources, summed once each. Split into named helpers rather than three
  // inline loops because the inline form measured 23 against S3776's ceiling of
  // 15 — each loop carries three guards, and a guard nested in a loop costs
  // double. The split is pure movement: no guard, order or arithmetic changed.
  const background = sumBackgroundServers(
    allUnreadCounts,
    activeServerId,
    activeFromUnreadCounts,
    mutes
  );
  const active = activeFromUnreadCounts ? sumActiveServer(unreadCounts, activeServerId, mutes) : 0;
  const dms = sumDirectMessages(useDMStore.getState().conversations, mutes.mutedDMs);

  return background + active + dms;
}

/**
 * Subscribe the OS badge to unread state. Returns an unsubscribe.
 *
 * Pushes once on start — that is what closes "a reload does not correct it",
 * with no main-process code.
 */
export function startBadgeSync(): () => void {
  let last = -1;

  const dispatch = (total: number): void => {
    // `setBadgeCount` is `ipcRenderer.invoke`, so it REJECTS when the shell has
    // no handler for the channel — the ordinary below-contract case, since the
    // renderer ships to Cloudflare Pages while the shell ships on the
    // electron-updater train. Dropping the promise made that an unhandled
    // rejection with no diagnostic. The `?.` chain is separate and correct: it
    // covers a shell with no bridge at all, and bare unit tests.
    // Read the bridge object once and call the method ON it — a detached
    // `const fn = electron.setBadgeCount; fn(total)` would lose `this`.
    const bridge = globalThis.electron;
    if (!bridge?.setBadgeCount) {
      // No bridge at all: a web/dev build, or a bare unit test. Nothing was
      // sent, so `total` must not be recorded as delivered.
      return;
    }
    // Recorded on DISPATCH, not on resolution. The skip above asks "has the OS
    // already been handed this number", and a bridge that returns void rather
    // than its promise is a legitimate shape — gating on the promise meant a
    // mock, or any such bridge, never recorded `last` and the skip never fired.
    // A rejection resets it below so the value is retried.
    last = total;
    const result = bridge.setBadgeCount(total);
    // `setBadgeCount` is `ipcRenderer.invoke`, so it REJECTS when the shell has
    // no handler for the channel — the ordinary below-contract case, since the
    // renderer ships to Cloudflare Pages while the shell ships on the
    // electron-updater train. Dropping the promise made that an unhandled
    // rejection with no diagnostic anywhere.
    result?.catch?.((err: unknown) => {
      last = -1;
      console.warn(
        'setBadgeCount failed; badge may be stale:',
        err instanceof Error ? err.message : 'unknown'
      );
    });
  };

  const push = () => {
    // These run synchronously INSIDE zustand's `setState`, so a throw here
    // aborts the remaining listeners and propagates into whoever called `set` —
    // which includes `clearUnread`, the read recorder this whole fix depends on.
    // A wrong badge must never be able to break marking a message read.
    try {
      const total = computeBadgeTotal();
      if (total === last) return;
      dispatch(total);
    } catch (err) {
      console.warn('badge recompute failed:', err instanceof Error ? err.message : 'unknown');
    }
  };

  push();

  const unsubs = [
    useUnreadStore.subscribe(push),
    useDMStore.subscribe(push),
    useNotificationPrefsStore.subscribe(push),
    useServerStore.subscribe(push),
  ];

  return () => unsubs.forEach((u) => u());
}
