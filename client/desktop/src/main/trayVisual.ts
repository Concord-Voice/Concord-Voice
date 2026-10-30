// Pure tray visual + menu resolver (spec 2026-10-29-tray-status-design §2, §2a, §3).
//
// `resolveTrayVisual(snapshot, mainState, platform)` maps the renderer's closed-enum
// snapshot plus main-owned state to: a render mode, a sprite frame key, an optional
// animation kind, a tooltip and a menu model. It has no Electron import and no I/O,
// so the whole state space is enumerable in tests.
//
// Frame keys use the grammar "<c>.<l>.<s>" and MUST stay a subset of
// `assets/tray/frames.json` (a test pins the set equality, burst/breath frames
// included). NO USER-CONTROLLED TEXT reaches any string here: every label and tooltip
// piece comes from TRAY_LABELS or from an integer formatted by this module.

import type { TrayIconStyle, TrayStatusSnapshot } from '../shared/trayStatus';

export type RenderMode = 'template' | 'white' | 'black' | 'colour';

export interface MainTrayState {
  update: 'none' | 'available' | 'downloading' | 'ready';
  downloadPercent: number;
  windowFocused: boolean;
  onBattery: boolean;
  richAnimation: boolean;
  isDev: boolean;
}

export type AnimationKind = 'burst' | 'breath';

export type TrayMenuId =
  | 'status-line'
  | 'unread'
  | 'open'
  | 'quit'
  | 'mute'
  | 'deafen'
  | 'leave'
  | 'stop-share'
  | 'presence'
  | 'presence-online'
  | 'presence-dnd'
  | 'presence-invisible'
  | 'download-update'
  | 'downloading'
  | 'restart-update';

export type TrayMenuItem =
  | { type: 'separator' }
  | {
      type: 'item';
      id: TrayMenuId;
      label: string;
      enabled: boolean;
      checked?: boolean;
      kind?: 'checkbox' | 'radio';
      submenu?: TrayMenuItem[];
    };

export interface TrayVisual {
  mode: RenderMode;
  frameKey: string;
  animation: AnimationKind | null;
  tooltip: string;
  menu: TrayMenuItem[];
  menuKey: string;
}

/** The only source of tray label / tooltip text. */
export const TRAY_LABELS: Readonly<Record<string, string>> = Object.freeze({
  open: 'Open Concord Voice',
  quit: 'Quit Concord Voice',
  signedOut: 'Signed out',
  reconnecting: 'Reconnecting…',
  connectionLost: 'Connection lost',
  inVoice: 'In voice',
  inCall: 'In a call',
  mute: 'Mute microphone',
  deafen: 'Deafen',
  leaveVoice: 'Leave voice',
  leaveCall: 'Leave call',
  stopShare: 'Stop sharing screen',
  mentionsWaiting: 'Mentions waiting',
  statusOnline: 'Status: Online',
  statusDnd: 'Status: Do Not Disturb',
  statusInvisible: 'Status: Invisible',
  statusOffline: 'Status: Offline',
  presenceOnline: 'Online',
  presenceDnd: 'Do Not Disturb',
  presenceInvisible: 'Invisible',
  downloadUpdate: 'Download update',
  restartUpdate: 'Restart to update',
  // Tooltip pieces.
  tipApp: 'Concord Voice',
  tipDnd: 'Do Not Disturb',
  tipInvisible: 'Invisible',
  tipDeafened: 'Deafened',
  tipMuted: 'Muted',
  tipSpeaking: 'Speaking',
  tipSharing: 'Sharing your screen',
  tipUpdateReady: 'Update ready',
  tipDev: '[dev]',
});

const L = TRAY_LABELS as Record<string, string>;

const BURST_FRAME_COUNT = 7;
const BREATH_LEVELS = ['full', 'breath70', 'breath40', 'breath70'] as const;
const DOWNLOAD_STEP = 5;
const UNREAD_CAP = 99;

/**
 * Mode for a style. `needsColour` (in voice, reconnecting, lost) only matters for
 * `auto` on macOS, where the idle glyph is a template image but status needs colour.
 */
export function modeFor(
  style: TrayIconStyle,
  platform: NodeJS.Platform,
  needsColour: boolean
): RenderMode {
  switch (style) {
    case 'light':
      return 'white';
    case 'dark':
      return 'black';
    case 'colour':
      return 'colour';
    case 'auto':
      if (platform !== 'darwin') return 'colour';
      return needsColour ? 'colour' : 'template';
  }
}

/**
 * Frame keys for an animation over `baseKey`, first frame first. Burst runs
 * burst1..burst7 (the scheduler then lands on the base); breath cycles the L
 * alpha. Null when the base key has no such animation.
 */
export function animationFrameKeys(baseKey: string, kind: AnimationKind): string[] | null {
  const [c, l, s] = baseKey.split('.');
  if (c === undefined || l !== 'full' || s === undefined) return null;
  if (kind === 'burst') {
    if (s !== 'discmention' || (c !== 'full' && c !== 'green')) return null;
    return Array.from({ length: BURST_FRAME_COUNT }, (_, i) => `${c}.burst${i + 1}.${s}`);
  }
  if (c !== 'amber') return null;
  return BREATH_LEVELS.map((level) => `${c}.${level}.${s}`);
}

type Presence = TrayStatusSnapshot['presence'];

function smallStar(s: TrayStatusSnapshot, m: MainTrayState, inVoice: boolean): string {
  if (inVoice && s.sharing) return 'ring';
  if (s.mention) return 'discmention';
  if (s.unread > 0) return 'disc';
  if (m.update === 'ready') return 'discupdate';
  return 'star';
}

function crescentIdle(presence: Presence): string {
  if (presence === 'dnd') return 'dim45';
  if (presence === 'invisible') return 'outline';
  return 'full';
}

function baseFrameKey(s: TrayStatusSnapshot, m: MainTrayState): string {
  if (s.auth === 'signed-out') return 'dim35.dim35.dim35';
  if (s.conn === 'lost') return 'amber.dim35.dim35';
  const inVoice = s.voice !== 'none';
  if (s.conn === 'reconnecting') {
    if (inVoice && s.deafened) return 'amber.slashlong.dim35';
    if (inVoice && s.muted) return 'amber.slash.dim35';
    return 'amber.full.dim35';
  }
  const small = smallStar(s, m, inVoice);
  if (!inVoice) return `${crescentIdle(s.presence)}.full.${small}`;
  if (s.deafened) return `green35.slashlong.${small}`;
  if (s.muted) return `green.slash.${small}`;
  return `green.${s.speaking ? 'lit' : 'full'}.${small}`;
}

function animationFor(
  key: string,
  s: TrayStatusSnapshot,
  m: MainTrayState,
  platform: NodeJS.Platform
): AnimationKind | null {
  if (platform === 'linux') return null;
  if (key === 'amber.full.dim35') return 'breath';
  if (
    (key === 'full.full.discmention' || key === 'green.full.discmention') &&
    s.mention &&
    !m.windowFocused
  ) {
    return 'burst';
  }
  return null;
}

function cappedCount(n: number): string {
  return n >= UNREAD_CAP ? `${UNREAD_CAP}+` : String(n);
}

function unreadLabel(n: number): string {
  return `${cappedCount(n)} unread messages`;
}

function leaveItem(s: TrayStatusSnapshot): TrayMenuItem {
  return item('leave', s.voice === 'dm-call' ? L.leaveCall : L.leaveVoice);
}

function sep(): TrayMenuItem {
  return { type: 'separator' };
}

function item(
  id: TrayMenuId,
  label: string,
  extra: {
    enabled?: boolean;
    checked?: boolean;
    kind?: 'checkbox' | 'radio';
    submenu?: TrayMenuItem[];
  } = {}
): TrayMenuItem {
  return { type: 'item', id, label, ...extra, enabled: extra.enabled ?? true };
}

function presenceItem(presence: Presence): TrayMenuItem {
  const labels: Record<Presence, string> = {
    online: L.statusOnline,
    dnd: L.statusDnd,
    invisible: L.statusInvisible,
    offline: L.statusOffline,
  };
  const label = labels[presence] ?? L.statusOnline;
  const radio = (id: TrayMenuId, text: string, value: Presence): TrayMenuItem =>
    item(id, text, { kind: 'radio', checked: presence === value });
  return item('presence', label, {
    submenu: [
      radio('presence-online', L.presenceOnline, 'online'),
      radio('presence-dnd', L.presenceDnd, 'dnd'),
      radio('presence-invisible', L.presenceInvisible, 'invisible'),
    ],
  });
}

function updateRow(m: MainTrayState): TrayMenuItem | null {
  switch (m.update) {
    case 'available':
      return item('download-update', L.downloadUpdate);
    case 'downloading': {
      const raw = Number.isFinite(m.downloadPercent) ? m.downloadPercent : 0;
      const pct = Math.floor(Math.min(100, Math.max(0, raw)) / DOWNLOAD_STEP) * DOWNLOAD_STEP;
      return item('downloading', `Downloading update… ${pct}%`, { enabled: false });
    }
    case 'ready':
      return item('restart-update', L.restartUpdate);
    case 'none':
      return null;
  }
}

function voiceControls(s: TrayStatusSnapshot, withShare: boolean): TrayMenuItem[] {
  const rows = [
    item('mute', L.mute, {
      kind: 'checkbox',
      checked: s.muted || s.deafened,
      enabled: !s.micLocked && !s.deafened,
    }),
    item('deafen', L.deafen, { kind: 'checkbox', checked: s.deafened, enabled: !s.deafenLocked }),
  ];
  if (withShare && s.sharing) rows.push(item('stop-share', L.stopShare));
  rows.push(leaveItem(s));
  return rows;
}

function join(groups: Array<Array<TrayMenuItem | null>>): TrayMenuItem[] {
  const out: TrayMenuItem[] = [];
  for (const group of groups) {
    const rows = group.filter((r): r is TrayMenuItem => r !== null);
    if (rows.length === 0) continue;
    if (out.length > 0) out.push(sep());
    out.push(...rows);
  }
  return out;
}

function buildMenu(s: TrayStatusSnapshot, m: MainTrayState): TrayMenuItem[] {
  const open = item('open', L.open);
  const quit = item('quit', L.quit);
  if (s.auth === 'signed-out') {
    return join([[item('status-line', L.signedOut, { enabled: false })], [open], [quit]]);
  }
  const inVoice = s.voice !== 'none';
  if (s.conn === 'lost') {
    const leave = inVoice ? leaveItem(s) : null;
    return join([
      [item('status-line', L.connectionLost, { enabled: false })],
      [leave, open],
      [quit],
    ]);
  }
  if (s.conn === 'reconnecting') {
    return join([
      [
        item('status-line', L.reconnecting, { enabled: false }),
        ...(inVoice ? voiceControls(s, false) : []),
      ],
      [open],
      [quit],
    ]);
  }
  let unread: TrayMenuItem | null = null;
  if (s.mention) unread = item('unread', L.mentionsWaiting);
  else if (s.unread > 0) unread = item('unread', unreadLabel(s.unread));
  const nav = [unread, updateRow(m), open, presenceItem(s.presence)];
  if (!inVoice) return join([nav, [quit]]);
  const status = item('status-line', s.voice === 'dm-call' ? L.inCall : L.inVoice, {
    enabled: false,
  });
  return join([[status, ...voiceControls(s, true)], nav, [quit]]);
}

function voiceTip(s: TrayStatusSnapshot): string {
  const state = s.voice === 'dm-call' ? L.inCall : L.inVoice;
  let modifier = '';
  if (s.deafened) modifier = L.tipDeafened;
  else if (s.muted) modifier = L.tipMuted;
  else if (s.sharing) modifier = L.tipSharing;
  else if (s.speaking) modifier = L.tipSpeaking;
  return modifier === '' ? state : `${state} · ${modifier}`;
}

/** Out of voice and connected: the first of mentions, unread, presence, update. */
function idleTip(s: TrayStatusSnapshot, m: MainTrayState): string | null {
  if (s.mention) return L.mentionsWaiting;
  if (s.unread > 0) return `${cappedCount(s.unread)} unread`;
  if (s.presence === 'dnd') return L.tipDnd;
  if (s.presence === 'invisible') return L.tipInvisible;
  if (m.update === 'ready') return L.tipUpdateReady;
  return null;
}

function tooltipFor(s: TrayStatusSnapshot, m: MainTrayState): string {
  let detail: string | null;
  if (s.auth === 'signed-out') detail = L.signedOut;
  else if (s.conn === 'lost') detail = L.connectionLost;
  else if (s.conn === 'reconnecting') detail = L.reconnecting;
  else if (s.voice === 'none') detail = idleTip(s, m);
  else detail = voiceTip(s);
  return detail === null ? L.tipApp : `${L.tipApp} — ${detail}`;
}

export function resolveTrayVisual(
  s: TrayStatusSnapshot | null,
  m: MainTrayState,
  platform: NodeJS.Platform
): TrayVisual {
  const devSuffix = m.isDev ? ` ${L.tipDev}` : '';
  let menu: TrayMenuItem[];
  let frameKey: string;
  let mode: RenderMode;
  let animation: AnimationKind | null = null;
  let tooltip: string;
  if (s === null) {
    menu = join([[item('open', L.open)], [item('quit', L.quit)]]);
    frameKey = 'full.full.star';
    mode = modeFor('auto', platform, false);
    tooltip = L.tipApp;
  } else {
    menu = buildMenu(s, m);
    frameKey = baseFrameKey(s, m);
    const needsColour = s.auth === 'signed-in' && (s.voice !== 'none' || s.conn !== 'ok');
    mode = modeFor(s.iconStyle, platform, needsColour);
    animation = s.auth === 'signed-in' ? animationFor(frameKey, s, m, platform) : null;
    tooltip = tooltipFor(s, m);
  }
  return {
    mode,
    frameKey,
    animation,
    tooltip: tooltip + devSuffix,
    menu,
    menuKey: JSON.stringify(menu),
  };
}
