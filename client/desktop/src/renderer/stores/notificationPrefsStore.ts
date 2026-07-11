/**
 * Server-persisted per-target mute preferences (#84, PR #985 — child of
 * epic #1029, Desktop Notification Preferences & Controls).
 *
 * TWO-STORE SPLIT: this store holds the SERVER-persisted mute rows (servers /
 * channels / DMs), hydrated from GET /api/v1/notifications/preferences on
 * login and NOT persisted locally. Device-local preferences (sounds, desktop
 * toggles, content privacy, quiet hours) live in `notificationStore`.
 */
import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

/**
 * A single notification preference row from the server.
 *
 * `muted=false` is meaningful (not just "no preference") — it expresses
 * "explicitly unmuted." This matters when a parent target is muted and the
 * user wants to selectively re-enable notifications on a child (e.g. mute
 * the server, but keep #general unmuted). The map distinguishes:
 *
 *   no entry        →  no opinion (use default / parent resolution)
 *   { muted: true } →  muted (optionally with expiry)
 *   { muted: false} →  explicitly unmuted (overrides any parent mute)
 *
 * `mutedUntil` is the wall-clock instant the mute expires. The store treats
 * expired entries as "not currently muted" without mutating the map — a
 * background sweep prunes them periodically so the in-memory maps don't grow
 * without bound. (The sweep is client-side only; the server keeps expired
 * rows, so they reappear on the next hydration until overwritten.)
 */
/**
 * The three kinds of mute targets the backend stores. Matches the CHECK
 * constraint on `notification_preferences.target_type`. Lives in the store
 * (rather than the service) so the wire-shape and the action signatures can
 * import a single source of truth without forming a layering cycle — the
 * service depends on the store, never the other way around.
 */
export type MuteTargetType = 'server' | 'channel' | 'dm';

export interface MuteEntry {
  muted: boolean;
  mutedUntil: Date | null;
  updatedAt: Date;
}

/** Wire-format preference returned by the server. */
export interface PreferenceWire {
  target_type: MuteTargetType;
  target_id: string;
  muted: boolean;
  muted_until?: string | null;
  updated_at: string;
}

interface NotificationPrefsState {
  // Three maps keyed by target_id. We split rather than one polymorphic
  // map because lookups happen on the hot WebSocket-event path; a typed
  // get() per target type is faster than discriminating on every call.
  mutedServers: Map<string, MuteEntry>;
  mutedChannels: Map<string, MuteEntry>;
  mutedDMs: Map<string, MuteEntry>;

  /** Bulk-replace state from a server hydration call. */
  setInitialPreferences: (prefs: PreferenceWire[]) => void;

  /**
   * Upsert a single preference locally (for optimistic UI updates ahead of
   * the network round-trip). The service-layer wrapper calls this before
   * the PUT so the UI reflects the change instantly.
   */
  setMute: (
    targetType: MuteTargetType,
    targetId: string,
    muted: boolean,
    mutedUntil: Date | null
  ) => void;

  /**
   * Remove a preference entirely. Used when an explicit unmute row would
   * carry no useful state (e.g., reverting to "no opinion" — though in
   * practice the UI sends `muted=false` rather than deleting).
   */
  removeMute: (targetType: MuteTargetType, targetId: string) => void;

  /**
   * Sweep all three maps and drop entries whose mute expired in the past.
   * Triggered every 60s by `startExpirySweep()` in notificationPrefsService.ts,
   * started on hydration (`tryHydrateNotificationPrefs` via postLoginHydration)
   * — keeps the maps from growing without bound for short timed mutes.
   */
  clearExpiredMutes: () => void;

  /** Resets to empty state. Required for test isolation. */
  clearAll: () => void;
}

/**
 * True if `entry` represents an active mute right now.
 *
 * Exported for callers that need the "is this entry an active mute?" answer
 * outside of the channel/DM selector flow — e.g., the server-icon UI that
 * also wants to know whether to render a corner overlay, or the context-menu
 * item that swaps its label between "Mute" and "Unmute". Centralising the
 * expiry logic in one place keeps the inline-expiry semantics consistent —
 * if we ever add a grace window or change the comparison, only one site has
 * to move.
 */
export function isEntryCurrentlyMuted(entry: MuteEntry | undefined): boolean {
  if (!entry) return false;
  if (!entry.muted) return false;
  if (entry.mutedUntil !== null && entry.mutedUntil.getTime() <= Date.now()) {
    // Expired mute — treat as not muted even though the entry is still in
    // the map. clearExpiredMutes() will reap it on its next pass.
    return false;
  }
  return true;
}

/** Convert a wire row to the internal Date-typed shape. */
function fromWire(p: PreferenceWire): MuteEntry {
  return {
    muted: p.muted,
    mutedUntil: p.muted_until ? new Date(p.muted_until) : null,
    updatedAt: new Date(p.updated_at),
  };
}

export const useNotificationPrefsStore = wrapStore(
  create<NotificationPrefsState>()(
    devtools(
      (set) => ({
        mutedServers: new Map(),
        mutedChannels: new Map(),
        mutedDMs: new Map(),

        setInitialPreferences: (prefs) => {
          const servers = new Map<string, MuteEntry>();
          const channels = new Map<string, MuteEntry>();
          const dms = new Map<string, MuteEntry>();
          for (const p of prefs) {
            const entry = fromWire(p);
            if (p.target_type === 'server') servers.set(p.target_id, entry);
            else if (p.target_type === 'channel') channels.set(p.target_id, entry);
            else if (p.target_type === 'dm') dms.set(p.target_id, entry);
          }
          set({ mutedServers: servers, mutedChannels: channels, mutedDMs: dms });
        },

        setMute: (targetType, targetId, muted, mutedUntil) =>
          set((state) => {
            const entry: MuteEntry = {
              muted,
              mutedUntil,
              updatedAt: new Date(),
            };
            if (targetType === 'server') {
              const next = new Map(state.mutedServers);
              next.set(targetId, entry);
              return { mutedServers: next };
            }
            if (targetType === 'channel') {
              const next = new Map(state.mutedChannels);
              next.set(targetId, entry);
              return { mutedChannels: next };
            }
            const next = new Map(state.mutedDMs);
            next.set(targetId, entry);
            return { mutedDMs: next };
          }),

        removeMute: (targetType, targetId) =>
          set((state) => {
            if (targetType === 'server') {
              if (!state.mutedServers.has(targetId)) return state;
              const next = new Map(state.mutedServers);
              next.delete(targetId);
              return { mutedServers: next };
            }
            if (targetType === 'channel') {
              if (!state.mutedChannels.has(targetId)) return state;
              const next = new Map(state.mutedChannels);
              next.delete(targetId);
              return { mutedChannels: next };
            }
            if (!state.mutedDMs.has(targetId)) return state;
            const next = new Map(state.mutedDMs);
            next.delete(targetId);
            return { mutedDMs: next };
          }),

        clearExpiredMutes: () =>
          set((state) => {
            // Sweep returns a new map only if at least one entry was reaped,
            // so subscribers don't re-render when nothing changed.
            const now = Date.now();
            let serversChanged = false;
            let channelsChanged = false;
            let dmsChanged = false;
            const sweepMap = (m: Map<string, MuteEntry>) => {
              let changed = false;
              const next = new Map(m);
              for (const [id, e] of m.entries()) {
                if (e.muted && e.mutedUntil !== null && e.mutedUntil.getTime() <= now) {
                  next.delete(id);
                  changed = true;
                }
              }
              return { next, changed };
            };
            const s = sweepMap(state.mutedServers);
            const c = sweepMap(state.mutedChannels);
            const d = sweepMap(state.mutedDMs);
            serversChanged = s.changed;
            channelsChanged = c.changed;
            dmsChanged = d.changed;
            if (!serversChanged && !channelsChanged && !dmsChanged) return state;
            return {
              mutedServers: serversChanged ? s.next : state.mutedServers,
              mutedChannels: channelsChanged ? c.next : state.mutedChannels,
              mutedDMs: dmsChanged ? d.next : state.mutedDMs,
            };
          }),

        clearAll: () =>
          set({
            mutedServers: new Map(),
            mutedChannels: new Map(),
            mutedDMs: new Map(),
          }),
      }),
      { name: 'NotificationPrefsStore' }
    )
  )
);

// -----------------------------------------------------------------------
// Selectors
//
// These live outside the store so they can be called from non-component
// contexts (the WebSocket event handlers in useWebSocketMessages.ts and the
// notificationSoundService gates) without subscribing. They read from
// `getState()` directly, which is fine for one-shot event handlers — the
// component-bound read is `useNotificationPrefsStore((s) => ...)`.
// -----------------------------------------------------------------------

/**
 * True if THIS channel should be treated as muted right now.
 *
 * Resolution order (the Discord-style override the issue spec calls out):
 *   1. channel-level pref     — wins outright (mute OR explicit unmute)
 *   2. server-level pref      — used only when the channel has no opinion
 *   3. default                — not muted
 *
 * Pass the channel's `serverId` so step 2 has a target. If the caller
 * doesn't know the server (e.g. a DM-style consumer asking about a
 * channel), pass `null` and the lookup falls back to channel-only.
 */
export function isChannelMuted(channelId: string, serverId: string | null): boolean {
  const state = useNotificationPrefsStore.getState();
  return isChannelMutedInMaps(channelId, serverId, state.mutedChannels, state.mutedServers);
}

/**
 * Pure variant of `isChannelMuted` operating on caller-supplied maps.
 *
 * Components that render unread badges subscribe to `mutedChannels` /
 * `mutedServers` reactively (`useNotificationPrefsStore((s) => s.mutedChannels)`)
 * and resolve through this helper, so a mute toggle re-renders the badge
 * immediately. Keeps the channel-wins / server-fallback resolution order in
 * exactly one place for both the event-handler path and the render path.
 */
export function isChannelMutedInMaps(
  channelId: string,
  serverId: string | null,
  mutedChannels: Map<string, MuteEntry>,
  mutedServers: Map<string, MuteEntry>
): boolean {
  const channelEntry = mutedChannels.get(channelId);
  if (channelEntry) {
    return isEntryCurrentlyMuted(channelEntry);
  }
  if (serverId) {
    return isEntryCurrentlyMuted(mutedServers.get(serverId));
  }
  return false;
}

/**
 * True if a server should show its unread affordance (the ServerBar dot, the
 * FolderBar count badge, and the folder tooltip).
 *
 * A precise entry was set by a mute-AWARE source (unread_notify / per-channel
 * fetch / recompute / DND refresh), so it already means "has an unmuted unread"
 * with channel-wins resolution applied, so it is trusted outright: a channel
 * explicitly unmuted under a muted server still lights up. An approximate
 * entry (the mute-UNAWARE bulk seed) falls back to the server-level mute gate.
 *
 * Shared by ServerBar and FolderBar so the top-level server icon and the
 * folder affordances agree; without this, a foldered muted server whose only
 * unreads are muted still lights its folder badge even though its top-level
 * icon is dark (#84 / epic #1029 close audit).
 */
export function isServerUnreadVisible(
  serverId: string,
  serverUnreadSet: Set<string>,
  serverUnreadPreciseSet: Set<string>,
  mutedServers: Map<string, MuteEntry>
): boolean {
  if (!serverUnreadSet.has(serverId)) return false;
  if (serverUnreadPreciseSet.has(serverId)) return true;
  return !isEntryCurrentlyMuted(mutedServers.get(serverId));
}

/**
 * True if this channel carries its OWN mute preference row, so the
 * channel-wins branch of isChannelMutedInMaps decides its state and the
 * server-level pref is bypassed entirely (mute OR explicit-unmute; either way
 * the channel's own opinion wins). The WS unread path uses this to tag a
 * background server's precise unread as a channel-wins override, so a later
 * server mute does not demote a dot isServerUnreadVisible is meant to honor
 * (#84 / epic #1029 close audit, P2 review follow-up).
 */
export function hasChannelMuteOverride(channelId: string): boolean {
  return useNotificationPrefsStore.getState().mutedChannels.has(channelId);
}

/** True if THIS DM conversation should be treated as muted right now. */
export function isDMMuted(conversationId: string): boolean {
  const entry = useNotificationPrefsStore.getState().mutedDMs.get(conversationId);
  return isEntryCurrentlyMuted(entry);
}

/**
 * True if at least one channel in `channelIds` is NOT effectively muted for
 * `serverId`. The unread-hydration paths use this to decide whether to light a
 * server dot — a server whose only unreads are muted channels must stay dark
 * (#84 acceptance criterion; epic #1029 close audit). A named module-level
 * helper (rather than an inline `.some`) keeps those event-context callbacks
 * under the function-nesting-depth ceiling.
 */
export function hasUnmutedChannel(channelIds: Iterable<string>, serverId: string): boolean {
  for (const channelId of channelIds) {
    if (!isChannelMuted(channelId, serverId)) return true;
  }
  return false;
}
