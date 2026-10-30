import { computeBadgeTotal } from './badgeSync';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useUserStore } from '../../stores/auth/userStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { useConnectionStore } from '../../stores/ui/connectionStore';
import { useMemberStore } from '../../stores/chat/memberStore';
import { useUnreadStore } from '../../stores/chat/unreadStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { useServerStore } from '../../stores/chat/serverStore';
import {
  useNotificationPrefsStore,
  isChannelMutedInMaps,
  isEntryCurrentlyMuted,
} from '../../stores/ui/notificationPrefsStore';
import { useSettingsStore } from '../../stores/ui/settingsStore';
import {
  TRAY_UNREAD_MAX,
  isTrayStatusSnapshot,
  type TrayStatusSnapshot,
} from '../../../shared/trayStatus';

/** Coalescing window for ordinary changes (leading + trailing). */
export const TRAY_COALESCE_MS = 100;
/** How long speaking stays true after the voice-activity signal drops. */
export const TRAY_SPEAKING_HOLD_MS = 300;

type ConnPhase = ReturnType<typeof useConnectionStore.getState>['phase'];

function connFor(phase: ConnPhase): TrayStatusSnapshot['conn'] {
  switch (phase) {
    case 'stable':
    case 'grace_period':
      return 'ok';
    case 'fatal':
      return 'lost';
    default:
      return 'reconnecting';
  }
}

/**
 * Any mention in a conversation that is not muted. Muted conversations keep
 * their mention counts (the in-app list still shows them), but they stay out of
 * the badge total, so they stay out of the tray too.
 */
function hasMention(): boolean {
  const { mentionCounts, serverMentionSet, serverUnreadChannelWinsSet, allUnreadCounts } =
    useUnreadStore.getState();
  const { mutedChannels, mutedServers, mutedDMs } = useNotificationPrefsStore.getState();
  const activeServerId = useServerStore.getState().activeServerId ?? '';
  const dmIds = new Set(useDMStore.getState().conversations.map((c) => c.id));
  // A mention in an unmuted override channel inside a muted server still counts
  // (channel mute wins, as in the badge); the channel-wins set marks those servers.
  for (const serverId of serverMentionSet) {
    if (serverUnreadChannelWinsSet.has(serverId)) return true;
    if (!isEntryCurrentlyMuted(mutedServers.get(serverId))) return true;
  }
  for (const [id, count] of mentionCounts) {
    if (count <= 0) continue;
    const muted = dmIds.has(id)
      ? isEntryCurrentlyMuted(mutedDMs.get(id))
      : isChannelMutedInMaps(
          id,
          allUnreadCounts.get(id)?.serverId ?? activeServerId,
          mutedChannels,
          mutedServers
        );
    if (!muted) return true;
  }
  return false;
}

function prefersReducedMotion(): boolean {
  return globalThis.matchMedia?.('(prefers-reduced-motion: reduce)')?.matches === true;
}

/**
 * Builds the closed-enum snapshot main renders the tray from. Pure read of
 * store state; carries no ids, names or message text (C4).
 *
 * Voice mapping: a DM call is `voice: 'dm-call'`. `voiceService.joinChannel`
 * synthesises DM rooms with `server_id: ''` (not null), so `activeServerId` is
 * `''` for a DM call; `isDMCall` is the authoritative flag and `!activeServerId`
 * covers both null and ''.
 */
export function buildTraySnapshot(): TrayStatusSnapshot {
  const voice = useVoiceStore.getState();
  const inVoice = voice.activeChannelId !== null;
  const userId = useUserStore.getState().user?.id;
  const me = userId ? voice.participants[userId] : undefined;
  const signedIn = useAuthStore.getState().accessToken !== null;
  const appearance = useSettingsStore.getState().appearance;
  let voiceState: TrayStatusSnapshot['voice'] = 'none';
  if (inVoice) voiceState = voice.isDMCall || !voice.activeServerId ? 'dm-call' : 'channel';

  return {
    v: 1,
    auth: signedIn ? 'signed-in' : 'signed-out',
    presence: signedIn ? useMemberStore.getState().selfStatus : 'offline',
    conn: connFor(useConnectionStore.getState().phase),
    voice: voiceState,
    speaking: inVoice && me?.isSpeaking === true,
    muted: inVoice && voice.isMuted,
    deafened: inVoice && voice.isDeafened,
    sharing: inVoice && voice.isScreenSharing,
    // VoiceControls parity: mic locks on server-mute or media policy; deafen on server-deafen.
    micLocked: inVoice && (me?.serverMuted === true || voice.mediaPolicyPaused.mic !== undefined),
    deafenLocked: inVoice && me?.serverDeafened === true,
    unread: Math.max(0, Math.min(Math.floor(computeBadgeTotal()) || 0, TRAY_UNREAD_MAX)),
    mention: hasMention(),
    reducedMotion: appearance.reduceAnimations || prefersReducedMotion(),
    iconStyle: appearance.trayIconStyle,
  };
}

// `void` from a shell before the throttle flag existed reads as accepted.
type Bridge = { traySetStatus: (s: TrayStatusSnapshot) => Promise<boolean | void> };

function getBridge(): Bridge | null {
  const electron = globalThis.electron;
  return typeof electron?.traySetStatus === 'function' ? electron : null;
}

// An async function runs synchronously up to its first await, so the bridge is
// still called right away, and a synchronous throw from it becomes a rejection.
async function push(bridge: Bridge, snapshot: TrayStatusSnapshot, onThrottled?: () => void) {
  if ((await bridge.traySetStatus(snapshot)) === false) onThrottled?.();
}

function send(snapshot: TrayStatusSnapshot, onThrottled?: () => void): void {
  const bridge = getBridge();
  if (!bridge) return;
  push(bridge, snapshot, onThrottled).catch(() => {
    console.warn('[tray] status push failed');
  });
}

type DevWindow = { __concordTrayForce?: (s: TrayStatusSnapshot | null) => void };

/**
 * Pushes the tray snapshot to main whenever it changes. Edge-triggered;
 * ordinary changes are coalesced (100 ms leading + trailing); mute / deafen /
 * speaking edges bypass coalescing, and speaking true -> false is held 300 ms
 * so VAD flicker cannot strobe the icon (R16). Returns a cleanup.
 */
export function startTraySync(): () => void {
  // Older shell: stay silent and leave the app working.
  if (!getBridge()) return () => undefined;

  let lastSent: TrayStatusSnapshot | null = null;
  let lastSentAt = -Infinity;
  let trailingTimer: ReturnType<typeof setTimeout> | null = null;
  let holdTimer: ReturnType<typeof setTimeout> | null = null;
  let effectiveSpeaking = false;
  let forced = false;
  let disposed = false;

  const clearTrailing = () => {
    if (trailingTimer !== null) clearTimeout(trailingTimer);
    trailingTimer = null;
  };
  const clearHold = () => {
    if (holdTimer !== null) clearTimeout(holdTimer);
    holdTimer = null;
  };

  const sameAsSent = (s: TrayStatusSnapshot): boolean =>
    lastSent !== null && JSON.stringify(lastSent) === JSON.stringify(s);

  const flush = (s: TrayStatusSnapshot) => {
    clearTrailing();
    if (sameAsSent(s)) return;
    lastSent = s;
    lastSentAt = Date.now();
    send(s, () => {
      // Main's rate limit dropped it: forget it was sent and try again shortly,
      // unless something newer has gone out since.
      if (disposed || lastSent !== s) return;
      lastSent = null;
      trailingTimer ??= setTimeout(() => {
        trailingTimer = null;
        evaluate();
      }, TRAY_COALESCE_MS);
    });
  };

  const evaluate = () => {
    if (disposed || forced) return;
    let snap: TrayStatusSnapshot;
    try {
      snap = buildTraySnapshot();
    } catch {
      console.warn('[tray] snapshot build failed');
      return;
    }
    // Speaking: rises immediately, falls only after the hold.
    if (snap.speaking) {
      clearHold();
      effectiveSpeaking = true;
    } else if (effectiveSpeaking) {
      holdTimer ??= setTimeout(() => {
        holdTimer = null;
        effectiveSpeaking = false;
        evaluate();
      }, TRAY_SPEAKING_HOLD_MS);
      snap = { ...snap, speaking: true };
    }
    if (sameAsSent(snap)) {
      clearTrailing();
      return;
    }
    // No previous snapshot reads as an edge: undefined never equals a boolean.
    const edge =
      lastSent?.muted !== snap.muted ||
      lastSent?.deafened !== snap.deafened ||
      lastSent?.speaking !== snap.speaking;
    const wait = lastSentAt + TRAY_COALESCE_MS - Date.now();
    if (edge || wait <= 0) {
      flush(snap);
      return;
    }
    clearTrailing();
    trailingTimer = setTimeout(() => {
      trailingTimer = null;
      evaluate();
    }, wait);
  };

  evaluate();

  const unsubs = [
    useVoiceStore.subscribe(evaluate),
    useUserStore.subscribe(evaluate),
    useAuthStore.subscribe(evaluate),
    useConnectionStore.subscribe(evaluate),
    useMemberStore.subscribe(evaluate),
    useUnreadStore.subscribe(evaluate),
    useDMStore.subscribe(evaluate),
    useServerStore.subscribe(evaluate),
    useNotificationPrefsStore.subscribe(evaluate),
    useSettingsStore.subscribe(evaluate),
  ];

  const mq = globalThis.matchMedia?.('(prefers-reduced-motion: reduce)');
  mq?.addEventListener?.('change', evaluate);

  if (import.meta.env.DEV) {
    // Dev-only state forcing (R18): pauses live sync and pushes `s` through the
    // real channel so main's guard validates it like any snapshot.
    (globalThis as unknown as DevWindow).__concordTrayForce = (s) => {
      if (s === null) {
        forced = false;
        lastSent = null;
        evaluate();
        return;
      }
      forced = true;
      clearTrailing();
      // Same shape check main applies; a malformed forced value is not sent.
      if (isTrayStatusSnapshot(s)) send(s);
    };
  }

  return () => {
    disposed = true;
    clearTrailing();
    clearHold();
    unsubs.forEach((u) => u());
    mq?.removeEventListener?.('change', evaluate);
    if (import.meta.env.DEV) delete (globalThis as unknown as DevWindow).__concordTrayForce;
  };
}
