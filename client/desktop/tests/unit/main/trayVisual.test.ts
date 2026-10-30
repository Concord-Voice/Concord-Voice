import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import {
  resolveTrayVisual,
  modeFor,
  animationFrameKeys,
  TRAY_LABELS,
  type MainTrayState,
  type TrayMenuItem,
  type TrayVisual,
} from '../../../src/main/trayVisual';
import {
  TRAY_AUTH_STATES,
  TRAY_CONN_STATES,
  TRAY_ICON_STYLES,
  TRAY_PRESENCES,
  TRAY_VOICE_STATES,
  type TrayStatusSnapshot,
} from '../../../src/shared/trayStatus';

const frames: { keys: string[] } = JSON.parse(
  readFileSync(resolve(__dirname, '../../../assets/tray/frames.json'), 'utf8')
);

const idle: TrayStatusSnapshot = {
  v: 1,
  auth: 'signed-in',
  presence: 'online',
  conn: 'ok',
  voice: 'none',
  speaking: false,
  muted: false,
  deafened: false,
  sharing: false,
  micLocked: false,
  deafenLocked: false,
  unread: 0,
  mention: false,
  reducedMotion: false,
  iconStyle: 'auto',
};
const mainState: MainTrayState = {
  update: 'none',
  downloadPercent: 0,
  windowFocused: true,
  onBattery: false,
  richAnimation: true,
  isDev: false,
};
const snap = (o: Partial<TrayStatusSnapshot> = {}): TrayStatusSnapshot => ({ ...idle, ...o });
const main = (o: Partial<MainTrayState> = {}): MainTrayState => ({ ...mainState, ...o });
const vis = (
  o: Partial<TrayStatusSnapshot> = {},
  m: Partial<MainTrayState> = {},
  p: NodeJS.Platform = 'darwin'
): TrayVisual => resolveTrayVisual(snap(o), main(m), p);

const ids = (menu: TrayMenuItem[]): string[] =>
  menu.map((r) => (r.type === 'separator' ? '-' : r.id));
const find = (menu: TrayMenuItem[], id: string) =>
  menu.find((r) => r.type === 'item' && r.id === id) as Extract<TrayMenuItem, { type: 'item' }>;

describe('resolveTrayVisual scenario matrix (spec §3)', () => {
  const rows: Array<{
    name: string;
    s: Partial<TrayStatusSnapshot> | null;
    m?: Partial<MainTrayState>;
    frame: string;
    menu: string[];
    tooltip: string;
  }> = [
    {
      name: 'no snapshot',
      s: null,
      frame: 'full.full.star',
      menu: ['open', '-', 'quit'],
      tooltip: 'Concord Voice',
    },
    {
      name: 'signed out',
      s: { auth: 'signed-out', voice: 'channel', muted: true, mention: true },
      frame: 'dim35.dim35.dim35',
      menu: ['status-line', '-', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Signed out',
    },
    {
      name: 'idle',
      s: {},
      frame: 'full.full.star',
      menu: ['open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice',
    },
    {
      name: 'unread',
      s: { unread: 4 },
      frame: 'full.full.disc',
      menu: ['unread', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — 4 unread',
    },
    {
      name: 'mention (focused)',
      s: { unread: 4, mention: true },
      frame: 'full.full.discmention',
      menu: ['unread', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — Mentions waiting',
    },
    {
      name: 'dnd',
      s: { presence: 'dnd' },
      frame: 'dim45.full.star',
      menu: ['open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — Do Not Disturb',
    },
    {
      name: 'dnd + unread',
      s: { presence: 'dnd', unread: 1 },
      frame: 'dim45.full.disc',
      menu: ['unread', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — 1 unread',
    },
    {
      name: 'invisible',
      s: { presence: 'invisible' },
      frame: 'outline.full.star',
      menu: ['open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — Invisible',
    },
    {
      name: 'offline presence',
      s: { presence: 'offline' },
      frame: 'full.full.star',
      menu: ['open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice',
    },
    {
      name: 'update ready',
      s: {},
      m: { update: 'ready' },
      frame: 'full.full.discupdate',
      menu: ['restart-update', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — Update ready',
    },
    {
      name: 'update available',
      s: {},
      m: { update: 'available' },
      frame: 'full.full.star',
      menu: ['download-update', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice',
    },
    {
      name: 'voice silent',
      s: { voice: 'channel' },
      frame: 'green.full.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In voice',
    },
    {
      name: 'dm call',
      s: { voice: 'dm-call' },
      frame: 'green.full.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In a call',
    },
    {
      name: 'voice dnd (not shown on C)',
      s: { voice: 'channel', presence: 'dnd' },
      frame: 'green.full.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In voice',
    },
    {
      name: 'speaking',
      s: { voice: 'channel', speaking: true },
      frame: 'green.lit.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In voice · Speaking',
    },
    {
      name: 'muted',
      s: { voice: 'channel', muted: true, speaking: true },
      frame: 'green.slash.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In voice · Muted',
    },
    {
      name: 'deafened',
      s: { voice: 'channel', deafened: true, muted: true, speaking: true },
      frame: 'green35.slashlong.star',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', 'presence', '-', 'quit'],
      tooltip: 'Concord Voice — In voice · Deafened',
    },
    {
      name: 'sharing',
      s: { voice: 'channel', sharing: true, mention: true },
      frame: 'green.full.ring',
      menu: [
        'status-line',
        'mute',
        'deafen',
        'stop-share',
        'leave',
        '-',
        'unread',
        'open',
        'presence',
        '-',
        'quit',
      ],
      tooltip: 'Concord Voice — In voice · Sharing your screen',
    },
    {
      name: 'voice + mention',
      s: { voice: 'channel', mention: true },
      frame: 'green.full.discmention',
      menu: [
        'status-line',
        'mute',
        'deafen',
        'leave',
        '-',
        'unread',
        'open',
        'presence',
        '-',
        'quit',
      ],
      tooltip: 'Concord Voice — In voice',
    },
    {
      name: 'reconnecting',
      s: { conn: 'reconnecting', unread: 3, mention: true, presence: 'dnd' },
      frame: 'amber.full.dim35',
      menu: ['status-line', '-', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Reconnecting…',
    },
    {
      name: 'reconnecting in voice, muted',
      s: { conn: 'reconnecting', voice: 'channel', muted: true },
      frame: 'amber.slash.dim35',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Reconnecting…',
    },
    {
      name: 'reconnecting in voice, deafened',
      s: { conn: 'reconnecting', voice: 'channel', muted: true, deafened: true, sharing: true },
      frame: 'amber.slashlong.dim35',
      menu: ['status-line', 'mute', 'deafen', 'leave', '-', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Reconnecting…',
    },
    {
      name: 'connection lost',
      s: { conn: 'lost', muted: true, deafened: true },
      frame: 'amber.dim35.dim35',
      menu: ['status-line', '-', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Connection lost',
    },
    {
      name: 'connection lost with session',
      s: { conn: 'lost', voice: 'dm-call' },
      frame: 'amber.dim35.dim35',
      menu: ['status-line', '-', 'leave', 'open', '-', 'quit'],
      tooltip: 'Concord Voice — Connection lost',
    },
  ];

  it.each(rows)('$name', ({ s, m, frame, menu, tooltip }) => {
    const v = s === null ? resolveTrayVisual(null, main(m), 'darwin') : vis(s, m);
    expect(v.frameKey).toBe(frame);
    expect(ids(v.menu)).toEqual(menu);
    expect(v.tooltip).toBe(tooltip);
  });

  it('muted/deafened flags are ignored outside voice', () => {
    expect(vis({ muted: true, deafened: true, sharing: true, speaking: true }).frameKey).toBe(
      'full.full.star'
    );
  });

  it('mode and frame per platform for an in-voice state under auto', () => {
    expect(vis({ voice: 'channel' }, {}, 'darwin').mode).toBe('colour');
    expect(vis({}, {}, 'darwin').mode).toBe('template');
    expect(vis({}, {}, 'win32').mode).toBe('colour');
    expect(vis({}, {}, 'linux').mode).toBe('colour');
    expect(vis({ conn: 'lost' }, {}, 'darwin').mode).toBe('colour');
    expect(vis({ conn: 'reconnecting' }, {}, 'darwin').mode).toBe('colour');
    expect(vis({ auth: 'signed-out', conn: 'lost' }, {}, 'darwin').mode).toBe('template');
  });
});

describe('resolveTrayVisual rules', () => {
  it('null snapshot → full.full.star, menu [open, separator, quit]', () => {
    const v = resolveTrayVisual(null, main(), 'darwin');
    expect(v.frameKey).toBe('full.full.star');
    expect(ids(v.menu)).toEqual(['open', '-', 'quit']);
    expect(v.mode).toBe('template');
    expect(v.animation).toBeNull();
  });

  it('mute item disabled when micLocked', () => {
    const mute = find(vis({ voice: 'channel', micLocked: true }).menu, 'mute');
    expect(mute.enabled).toBe(false);
    expect(mute.checked).toBe(false);
  });

  it('deafen item disabled when deafenLocked', () => {
    expect(find(vis({ voice: 'channel', deafenLocked: true }).menu, 'deafen').enabled).toBe(false);
  });

  it('deafened → mute checked and disabled (in-app parity)', () => {
    const v = vis({ voice: 'channel', deafened: true });
    const mute = find(v.menu, 'mute');
    expect(mute.checked).toBe(true);
    expect(mute.enabled).toBe(false);
    expect(find(v.menu, 'deafen').checked).toBe(true);
  });

  it('reconnecting controls stay enabled', () => {
    const v = vis({ conn: 'reconnecting', voice: 'channel' });
    expect(find(v.menu, 'mute').enabled).toBe(true);
    expect(find(v.menu, 'deafen').enabled).toBe(true);
    expect(find(v.menu, 'leave').enabled).toBe(true);
  });

  it('muted while reconnecting → amber + slash', () => {
    expect(vis({ conn: 'reconnecting', voice: 'channel', muted: true }).frameKey).toBe(
      'amber.slash.dim35'
    );
  });

  it('sharing beats mention beats unread beats update-ready on S', () => {
    const base = { voice: 'channel' as const, sharing: true, mention: true, unread: 3 };
    const upd = { update: 'ready' as const };
    expect(vis(base, upd).frameKey).toBe('green.full.ring');
    expect(vis({ ...base, sharing: false }, upd).frameKey).toBe('green.full.discmention');
    expect(vis({ ...base, sharing: false, mention: false }, upd).frameKey).toBe('green.full.disc');
    expect(vis({ ...base, sharing: false, mention: false, unread: 0 }, upd).frameKey).toBe(
      'green.full.discupdate'
    );
    // sharing outside voice has no ring
    expect(vis({ sharing: true, unread: 1 }).frameKey).toBe('full.full.disc');
  });

  it('speaking ignored while muted', () => {
    expect(vis({ voice: 'channel', muted: true, speaking: true }).frameKey).toBe(
      vis({ voice: 'channel', muted: true, speaking: false }).frameKey
    );
  });

  it('text DM state does not exist: identical snapshots resolve identically', () => {
    expect(vis({ unread: 2 })).toEqual(vis({ unread: 2 }));
  });

  it.each(['darwin', 'win32', 'linux'] as const)('iconStyle light/dark/colour on %s', (p) => {
    const states: Array<Partial<TrayStatusSnapshot>> = [
      {},
      { voice: 'channel' },
      { voice: 'channel', muted: true },
      { conn: 'lost' },
      { auth: 'signed-out' },
    ];
    for (const st of states) {
      expect(vis({ ...st, iconStyle: 'light' }, {}, p).mode).toBe('white');
      expect(vis({ ...st, iconStyle: 'dark' }, {}, p).mode).toBe('black');
      expect(vis({ ...st, iconStyle: 'colour' }, {}, p).mode).toBe('colour');
    }
  });

  it('colour on darwin even when idle; auto idle darwin is template', () => {
    expect(vis({ iconStyle: 'colour' }, {}, 'darwin').mode).toBe('colour');
    expect(vis({ iconStyle: 'auto' }, {}, 'darwin').mode).toBe('template');
  });

  it('modeFor table', () => {
    expect(modeFor('auto', 'darwin', true)).toBe('colour');
    expect(modeFor('auto', 'darwin', false)).toBe('template');
    expect(modeFor('auto', 'win32', false)).toBe('colour');
    expect(modeFor('light', 'darwin', true)).toBe('white');
  });

  it('update rows: downloading is disabled and rounds down to 5%', () => {
    const v = vis({}, { update: 'downloading', downloadPercent: 47.9 });
    const row = find(v.menu, 'downloading');
    expect(row.label).toBe('Downloading update… 45%');
    expect(row.enabled).toBe(false);
    expect(
      find(vis({}, { update: 'downloading', downloadPercent: Number.NaN }).menu, 'downloading')
        .label
    ).toBe('Downloading update… 0%');
    expect(
      find(vis({}, { update: 'downloading', downloadPercent: 250 }).menu, 'downloading').label
    ).toBe('Downloading update… 100%');
  });

  it('presence submenu checks the current presence', () => {
    const sub = find(vis({ presence: 'dnd' }).menu, 'presence');
    expect(sub.label).toBe('Status: Do Not Disturb');
    expect((sub.submenu ?? []).map((r) => (r.type === 'item' ? [r.id, r.checked] : null))).toEqual([
      ['presence-online', false],
      ['presence-dnd', true],
      ['presence-invisible', false],
    ]);
    expect(find(vis({ presence: 'invisible' }).menu, 'presence').label).toBe('Status: Invisible');
    expect(find(vis({ presence: 'offline' }).menu, 'presence').label).toBe('Status: Offline');
  });

  it('unread label caps at 99+', () => {
    expect(find(vis({ unread: 99 }).menu, 'unread').label).toBe('99+ unread messages');
    expect(find(vis({ unread: 98 }).menu, 'unread').label).toBe('98 unread messages');
    expect(vis({ unread: 99 }).tooltip).toBe('Concord Voice — 99+ unread');
  });

  it('dev builds are marked in the tooltip', () => {
    expect(vis({}, { isDev: true }).tooltip).toBe('Concord Voice [dev]');
    expect(vis({ voice: 'channel' }, { isDev: true }).tooltip).toBe(
      'Concord Voice — In voice [dev]'
    );
  });

  it('menuKey unchanged when only speaking flips', () => {
    expect(vis({ voice: 'channel', speaking: true }).menuKey).toBe(
      vis({ voice: 'channel', speaking: false }).menuKey
    );
  });

  it('menuKey unchanged between 41% and 44%, changes at 45%', () => {
    const k = (p: number) => vis({}, { update: 'downloading', downloadPercent: p }).menuKey;
    expect(k(41)).toBe(k(44));
    expect(k(44)).not.toBe(k(45));
  });

  it('burst only when mention && !windowFocused && platform !== linux', () => {
    const mention = { mention: true };
    expect(vis(mention, { windowFocused: false }, 'darwin').animation).toBe('burst');
    expect(vis(mention, { windowFocused: false }, 'win32').animation).toBe('burst');
    expect(vis(mention, { windowFocused: false }, 'linux').animation).toBeNull();
    expect(vis(mention, { windowFocused: true }, 'darwin').animation).toBeNull();
    expect(vis({}, { windowFocused: false }, 'darwin').animation).toBeNull();
    expect(vis({ voice: 'channel', mention: true }, { windowFocused: false }).animation).toBe(
      'burst'
    );
    expect(
      vis({ voice: 'channel', mention: true, sharing: true }, { windowFocused: false }).animation
    ).toBeNull();
    expect(
      vis({ voice: 'channel', mention: true, muted: true }, { windowFocused: false }).animation
    ).toBeNull();
  });

  it('breath never on linux; reconnecting breath elsewhere only on the base amber frame', () => {
    expect(vis({ conn: 'reconnecting' }, {}, 'linux').animation).toBeNull();
    expect(vis({ conn: 'reconnecting' }, {}, 'darwin').animation).toBe('breath');
    expect(vis({ conn: 'reconnecting' }, {}, 'win32').animation).toBe('breath');
    expect(
      vis({ conn: 'reconnecting', voice: 'channel', muted: true }, {}, 'darwin').animation
    ).toBeNull();
    expect(vis({ conn: 'lost' }, {}, 'darwin').animation).toBeNull();
  });

  it('animationFrameKeys', () => {
    expect(animationFrameKeys('green.full.discmention', 'burst')).toHaveLength(7);
    expect(animationFrameKeys('full.full.discmention', 'burst')?.[0]).toBe(
      'full.burst1.discmention'
    );
    expect(animationFrameKeys('full.full.disc', 'burst')).toBeNull();
    expect(animationFrameKeys('amber.full.dim35', 'breath')).toEqual([
      'amber.full.dim35',
      'amber.breath70.dim35',
      'amber.breath40.dim35',
      'amber.breath70.dim35',
    ]);
    expect(animationFrameKeys('full.full.star', 'breath')).toBeNull();
    expect(animationFrameKeys('bogus', 'burst')).toBeNull();
  });
});

describe('state-space coverage', () => {
  const bools = [false, true];
  const all: Array<{ v: TrayVisual; s: TrayStatusSnapshot; platform: NodeJS.Platform }> = [];
  for (const auth of TRAY_AUTH_STATES)
    for (const presence of TRAY_PRESENCES)
      for (const conn of TRAY_CONN_STATES)
        for (const voice of TRAY_VOICE_STATES)
          for (const speaking of bools)
            for (const muted of bools)
              for (const deafened of bools)
                for (const sharing of bools)
                  for (const mention of bools)
                    for (const unread of [0, 5])
                      for (const update of ['none', 'available', 'downloading', 'ready'] as const) {
                        const s = snap({
                          auth,
                          presence,
                          conn,
                          voice,
                          speaking,
                          muted,
                          deafened,
                          sharing,
                          mention,
                          unread,
                        });
                        for (const platform of ['darwin', 'linux'] as const) {
                          for (const windowFocused of bools) {
                            if (windowFocused && !mention) continue;
                            all.push({
                              s,
                              platform,
                              v: resolveTrayVisual(
                                s,
                                main({ update, downloadPercent: 33, windowFocused }),
                                platform
                              ),
                            });
                          }
                        }
                      }

  it('every resolvable frameKey (and animation frame) is in frames.json, and every key is reachable', () => {
    const known = new Set(frames.keys);
    const reached = new Set<string>();
    for (const { v } of all) {
      reached.add(v.frameKey);
      if (v.animation !== null) {
        const seq = animationFrameKeys(v.frameKey, v.animation);
        expect(seq, `${v.animation} on ${v.frameKey}`).not.toBeNull();
        for (const k of seq ?? []) reached.add(k);
      }
    }
    expect([...reached].filter((k) => !known.has(k))).toEqual([]);
    expect([...known].filter((k) => !reached.has(k))).toEqual([]);
    expect(frames.keys.length).toBeLessThanOrEqual(60);
  });

  it('every menu label is a TRAY_LABELS value or a formatted count', () => {
    const allowed = new Set(Object.values(TRAY_LABELS));
    const dynamic = /^\d+\+? unread messages$|^Downloading update… \d+%$/;
    const walk = (menu: TrayMenuItem[], out: string[]): void => {
      for (const r of menu) {
        if (r.type === 'separator') continue;
        out.push(r.label);
        if (r.submenu) walk(r.submenu, out);
      }
    };
    const bad = new Set<string>();
    for (const { v } of all) {
      const labels: string[] = [];
      walk(v.menu, labels);
      for (const l of labels) if (!allowed.has(l) && !dynamic.test(l)) bad.add(l);
    }
    expect([...bad]).toEqual([]);
  });

  it('every tooltip starts with the app name and the style never changes the frame key', () => {
    for (const { v } of all.slice(0, 2000))
      expect(v.tooltip.startsWith('Concord Voice')).toBe(true);
    for (const style of TRAY_ICON_STYLES) {
      expect(vis({ voice: 'channel', iconStyle: style }).frameKey).toBe('green.full.star');
    }
  });
});
