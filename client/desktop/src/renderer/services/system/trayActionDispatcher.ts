import { voiceService } from '../voice/voiceService';
import { setOwnPresence } from '../presence/setOwnPresence';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useUnreadStore, type RecentUnreadTarget } from '../../stores/chat/unreadStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { useNotificationNavigationStore } from '../../stores/ui/notificationNavigationStore';
import {
  useNotificationPrefsStore,
  isChannelMutedInMaps,
  isEntryCurrentlyMuted,
} from '../../stores/ui/notificationPrefsStore';
import { isTrayAction, type TrayAction } from '../../../shared/trayStatus';

/**
 * Applies a tray-menu action by calling the same functions the in-app controls
 * call. The menu was rendered from a snapshot that may be stale (a hotkey can
 * flip mute while the menu is open), so every action re-reads live state and
 * is idempotent: "set muted to true" while already muted does nothing, and a
 * locked control (server-muted, policy-paused, server-deafened) is never called.
 */

function logFailure(what: string): void {
  // Message-only: an error object can carry ids or tokens.
  console.warn(`[tray] ${what} failed`);
}

function run(what: string, fn: () => unknown): void {
  try {
    const result = fn();
    if (result instanceof Promise) result.catch(() => logFailure(what));
  } catch {
    logFailure(what);
  }
}

function localParticipant() {
  const userId = useUserStore.getState().user?.id;
  return userId ? useVoiceStore.getState().participants[userId] : undefined;
}

function setMuted(value: boolean): void {
  const voice = useVoiceStore.getState();
  if (!voice.activeChannelId) return;
  // Mirrors VoiceControls `locked={isServerMuted || micPoliced}`.
  if (localParticipant()?.serverMuted || voice.mediaPolicyPaused.mic !== undefined) return;
  // Deafened shows the mute item checked and disabled (in-app parity).
  if (voice.isDeafened) return;
  if (voice.isMuted === value) return;
  run('toggle mute', () => voiceService.toggleMute());
}

function setDeafened(value: boolean): void {
  const voice = useVoiceStore.getState();
  if (!voice.activeChannelId) return;
  if (localParticipant()?.serverDeafened) return;
  if (voice.isDeafened === value) return;
  run('toggle deafen', () => voiceService.toggleDeafen());
}

/** Muted conversations stay out of the badge, so they stay out of the unread row too. */
function isMuted(target: RecentUnreadTarget): boolean {
  const { mutedChannels, mutedServers, mutedDMs } = useNotificationPrefsStore.getState();
  if (target.type === 'dm') return isEntryCurrentlyMuted(mutedDMs.get(target.targetId));
  const serverId =
    target.serverId ??
    useUnreadStore.getState().allUnreadCounts.get(target.targetId)?.serverId ??
    '';
  return isChannelMutedInMaps(target.targetId, serverId, mutedChannels, mutedServers);
}

function isStillUnread(target: RecentUnreadTarget): boolean {
  if (isMuted(target)) return false;
  if (target.type === 'dm') {
    const conv = useDMStore.getState().conversations.find((c) => c.id === target.targetId);
    return (conv?.unreadCount ?? 0) > 0;
  }
  const unread = useUnreadStore.getState();
  return (
    (unread.unreadCounts.get(target.targetId) ?? 0) > 0 ||
    (unread.allUnreadCounts.get(target.targetId)?.count ?? 0) > 0
  );
}

/**
 * Unread restored at start-up or after a reload never passed through the live stack,
 * so fall back to the authoritative collections: the most active unread DM (the list
 * is kept sorted by activity), then any unread channel. Channels are read the way
 * computeBadgeTotal reads them: the active server from `unreadCounts` when that map
 * owns it, every other server from `allUnreadCounts`. Otherwise a count the badge
 * shows (e.g. set on leaving a scrolled-up channel) would have no target.
 */
function restoredUnreadTarget(): RecentUnreadTarget | undefined {
  const dm = useDMStore
    .getState()
    .conversations.find(
      (c) => (c.unreadCount ?? 0) > 0 && !isMuted({ type: 'dm', targetId: c.id })
    );
  if (dm) return { type: 'dm', targetId: dm.id };
  return firstUnreadChannel();
}

/** Channel rows in badge order (see restoredUnreadTarget); the first unread, unmuted one. */
function firstUnreadChannel(): RecentUnreadTarget | undefined {
  const { unreadCounts, unreadCountsServerId, allUnreadCounts } = useUnreadStore.getState();
  const activeServerId = useServerStore.getState().activeServerId;
  const activeOwned = activeServerId !== null && unreadCountsServerId === activeServerId;
  const rows: Array<[RecentUnreadTarget, number]> = [];
  if (activeOwned) {
    for (const [targetId, count] of unreadCounts) {
      rows.push([{ type: 'channel', targetId, serverId: activeServerId }, count]);
    }
  }
  for (const [targetId, { serverId, count }] of allUnreadCounts) {
    // Owned active-server rows above are authoritative; its bulk rows can be stale.
    if (!activeOwned || serverId !== activeServerId) {
      rows.push([{ type: 'channel', targetId, serverId }, count]);
    }
  }
  return rows.find(([target, count]) => count > 0 && !isMuted(target))?.[0];
}

/** LIFO: the most recently received conversation that is still unread. */
function openUnread(): void {
  const target =
    useUnreadStore.getState().recentUnreadTargets.find(isStillUnread) ?? restoredUnreadTarget();
  if (!target) return;
  useNotificationNavigationStore.getState().setPendingNavigation({
    type: target.type,
    targetId: target.targetId,
    ...(target.serverId ? { serverId: target.serverId } : {}),
  });
}

export function handleTrayAction(action: TrayAction): void {
  switch (action.kind) {
    case 'set-muted':
      setMuted(action.value);
      return;
    case 'set-deafened':
      setDeafened(action.value);
      return;
    case 'leave-voice':
      if (useVoiceStore.getState().activeChannelId) {
        run('leave voice', () => voiceService.leaveChannel());
      }
      return;
    case 'stop-share':
      if (useVoiceStore.getState().isScreenSharing) {
        run('stop share', () => voiceService.toggleScreenShare());
      }
      return;
    case 'set-presence':
      run('set presence', () => setOwnPresence(action.value));
      return;
    case 'open-unread':
      openUnread();
      return;
  }
}

/** Subscribes to tray actions from main. Returns an unsubscribe. */
export function startTrayActionDispatcher(): () => void {
  const subscribe = globalThis.electron?.onTrayAction;
  // Older shell without the tray bridge: nothing to listen to.
  if (typeof subscribe !== 'function') return () => undefined;
  const unsubscribe = subscribe((action) => {
    // Preload already validates; re-check so this module is safe on its own.
    if (isTrayAction(action)) handleTrayAction(action);
  });
  return typeof unsubscribe === 'function' ? unsubscribe : () => undefined;
}
