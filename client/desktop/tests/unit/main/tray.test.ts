import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import path from 'node:path';
import {
  decideActivateAction,
  applyActivate,
  buildTrayMenuTemplate,
  loadTrayManifest,
  menuEffectFor,
  resolveTrayIconPath,
  sanitizeTrayManifest,
  type ActivatableWindow,
} from '../../../src/main/tray';
import type { TrayStatusSnapshot } from '../../../src/shared/trayStatus';

// Structural test double — decideActivateAction takes ActivatableWindow, so
// plain objects suffice (no Electron import needed for the pure core).
function fakeWindow(state: {
  destroyed?: boolean;
  minimized?: boolean;
  visible?: boolean;
  focused?: boolean;
}): ActivatableWindow {
  return {
    isDestroyed: () => state.destroyed ?? false,
    isMinimized: () => state.minimized ?? false,
    isVisible: () => state.visible ?? true,
    isFocused: () => state.focused ?? true,
  };
}

describe('decideActivateAction — decision table (spec §2)', () => {
  it('returns "create" for a null window', () => {
    expect(decideActivateAction(null)).toBe('create');
  });

  it('returns "create" for a destroyed window', () => {
    expect(decideActivateAction(fakeWindow({ destroyed: true }))).toBe('create');
  });

  it('returns "restore-focus" for a minimized window', () => {
    expect(decideActivateAction(fakeWindow({ minimized: true, visible: false }))).toBe(
      'restore-focus'
    );
  });

  it('returns "show-focus" for a hidden window', () => {
    expect(decideActivateAction(fakeWindow({ visible: false }))).toBe('show-focus');
  });

  it('returns "focus" for a visible-but-unfocused window', () => {
    expect(decideActivateAction(fakeWindow({ focused: false }))).toBe('focus');
  });

  it('returns "noop" for a visible focused window (AC: already-focused click is a no-op)', () => {
    expect(decideActivateAction(fakeWindow({}))).toBe('noop');
  });
});

describe('applyActivate', () => {
  function mockWin() {
    return {
      restore: vi.fn(),
      show: vi.fn(),
      focus: vi.fn(),
    };
  }

  it('calls createWindow for "create" and touches nothing else', () => {
    const createWindow = vi.fn();
    applyActivate(null, 'create', createWindow);
    expect(createWindow).toHaveBeenCalledTimes(1);
  });

  it('restores + focuses for "restore-focus"', () => {
    const win = mockWin();
    const createWindow = vi.fn();
    applyActivate(win, 'restore-focus', createWindow);
    expect(win.restore).toHaveBeenCalledTimes(1);
    expect(win.show).not.toHaveBeenCalled();
    expect(win.focus).toHaveBeenCalledTimes(1);
    expect(createWindow).not.toHaveBeenCalled();
  });

  it('shows + focuses for "show-focus"', () => {
    const win = mockWin();
    applyActivate(win, 'show-focus', vi.fn());
    expect(win.show).toHaveBeenCalledTimes(1);
    expect(win.restore).not.toHaveBeenCalled();
    expect(win.focus).toHaveBeenCalledTimes(1);
  });

  it('only focuses for "focus"', () => {
    const win = mockWin();
    applyActivate(win, 'focus', vi.fn());
    expect(win.restore).not.toHaveBeenCalled();
    expect(win.show).not.toHaveBeenCalled();
    expect(win.focus).toHaveBeenCalledTimes(1);
  });

  it('does nothing for "noop"', () => {
    const win = mockWin();
    const createWindow = vi.fn();
    applyActivate(win, 'noop', createWindow);
    expect(win.restore).not.toHaveBeenCalled();
    expect(win.show).not.toHaveBeenCalled();
    expect(win.focus).not.toHaveBeenCalled();
    expect(createWindow).not.toHaveBeenCalled();
  });

  it('falls back to createWindow when the window is null with a non-create action', () => {
    const createWindow = vi.fn();
    applyActivate(null, 'focus', createWindow);
    expect(createWindow).toHaveBeenCalledTimes(1);
  });
});

describe('menuEffectFor — menu id → effect (spec §3, §4)', () => {
  it('open activates; unread activates AND sends open-unread', () => {
    expect(menuEffectFor('open', undefined)).toEqual({ type: 'activate' });
    expect(menuEffectFor('unread', undefined)).toEqual({
      type: 'activate-send',
      action: { kind: 'open-unread' },
    });
  });

  it('quit, download and restart are main-direct', () => {
    expect(menuEffectFor('quit', undefined)).toEqual({ type: 'quit' });
    expect(menuEffectFor('download-update', undefined)).toEqual({ type: 'download-update' });
    expect(menuEffectFor('restart-update', undefined)).toEqual({ type: 'restart-update' });
  });

  it('mute / deafen carry the TARGET value, the inverse of the shown check (R3)', () => {
    expect(menuEffectFor('mute', false)).toEqual({
      type: 'send',
      action: { kind: 'set-muted', value: true },
    });
    expect(menuEffectFor('mute', true)).toEqual({
      type: 'send',
      action: { kind: 'set-muted', value: false },
    });
    expect(menuEffectFor('deafen', false)).toEqual({
      type: 'send',
      action: { kind: 'set-deafened', value: true },
    });
    expect(menuEffectFor('deafen', undefined)).toEqual({
      type: 'send',
      action: { kind: 'set-deafened', value: true },
    });
  });

  it('leave, stop-share and presence radios send their actions', () => {
    expect(menuEffectFor('leave', undefined)).toEqual({
      type: 'send',
      action: { kind: 'leave-voice' },
    });
    expect(menuEffectFor('stop-share', undefined)).toEqual({
      type: 'send',
      action: { kind: 'stop-share' },
    });
    expect(menuEffectFor('presence-online', false)).toEqual({
      type: 'send',
      action: { kind: 'set-presence', value: 'online' },
    });
    expect(menuEffectFor('presence-dnd', false)).toEqual({
      type: 'send',
      action: { kind: 'set-presence', value: 'dnd' },
    });
    expect(menuEffectFor('presence-invisible', true)).toEqual({
      type: 'send',
      action: { kind: 'set-presence', value: 'invisible' },
    });
  });

  it('informational rows have no effect', () => {
    expect(menuEffectFor('status-line', undefined)).toBeNull();
    expect(menuEffectFor('downloading', undefined)).toBeNull();
    expect(menuEffectFor('presence', undefined)).toBeNull();
  });
});

describe('buildTrayMenuTemplate', () => {
  it('maps separators, checkboxes, radios and submenus; clicks report id + shown check', () => {
    const onClick = vi.fn();
    const template = buildTrayMenuTemplate(
      [
        { type: 'item', id: 'status-line', label: 'In voice', enabled: false },
        {
          type: 'item',
          id: 'mute',
          label: 'Mute microphone',
          enabled: true,
          checked: true,
          kind: 'checkbox',
        },
        { type: 'separator' },
        {
          type: 'item',
          id: 'presence',
          label: 'Status: Online',
          enabled: true,
          submenu: [
            {
              type: 'item',
              id: 'presence-dnd',
              label: 'Do Not Disturb',
              enabled: true,
              checked: false,
              kind: 'radio',
            },
          ],
        },
      ],
      onClick
    );
    expect(template).toHaveLength(4);
    expect(template[0]).toMatchObject({
      id: 'status-line',
      label: 'In voice',
      enabled: false,
      type: 'normal',
    });
    expect(template[0].click).toBeUndefined();
    expect(template[1]).toMatchObject({
      id: 'mute',
      type: 'checkbox',
      checked: true,
      enabled: true,
    });
    expect(template[2]).toEqual({ type: 'separator' });
    expect(template[3]).toMatchObject({ id: 'presence', type: 'submenu' });
    const sub = template[3].submenu as Array<{ type?: string; click?: () => void }>;
    expect(sub[0]).toMatchObject({ type: 'radio', checked: false });

    (template[1].click as () => void)();
    expect(onClick).toHaveBeenLastCalledWith('mute', true);
    sub[0].click?.();
    expect(onClick).toHaveBeenLastCalledWith('presence-dnd', false);
  });

  it('gives no click handler to a disabled row', () => {
    const template = buildTrayMenuTemplate(
      [
        {
          type: 'item',
          id: 'mute',
          label: 'Mute microphone',
          enabled: false,
          checked: false,
          kind: 'checkbox',
        },
      ],
      vi.fn()
    );
    expect(template[0].click).toBeUndefined();
  });
});

describe('resolveTrayIconPath', () => {
  it('packaged darwin → <Resources>/tray/<mode>/<key>.png (nativeImage picks @2x)', () => {
    expect(resolveTrayIconPath('darwin', true, '/R', '/c', 'template', 'full.full.star')).toBe(
      '/R/tray/template/full.full.star.png'
    );
  });

  it('linux uses the 22px file', () => {
    expect(resolveTrayIconPath('linux', true, '/R', '/c', 'colour', 'green.full.star')).toBe(
      '/R/tray/colour/green.full.star-22.png'
    );
  });

  it('win32 uses the 16px base', () => {
    expect(resolveTrayIconPath('win32', true, '/R', '/c', 'white', 'full.full.disc')).toBe(
      '/R/tray/white/full.full.disc.png'
    );
  });

  it('falls back to the repo-relative path in dev (unpackaged)', () => {
    expect(
      resolveTrayIconPath(
        'darwin',
        false,
        undefined,
        '/repo/client/desktop',
        'template',
        'full.full.star'
      )
    ).toBe('/repo/client/desktop/assets/tray/template/full.full.star.png');
  });

  it('falls back to the repo-relative path when resourcesPath is undefined despite isPackaged', () => {
    expect(resolveTrayIconPath('win32', true, undefined, '/cwd', 'colour', 'full.full.star')).toBe(
      '/cwd/assets/tray/colour/full.full.star.png'
    );
  });
});

describe('loadTrayManifest', () => {
  it('reads the shipped manifest: keys and the four modes', () => {
    const m = loadTrayManifest(path.join(process.cwd(), 'assets', 'tray'));
    expect(m.modes).toEqual(['template', 'white', 'black', 'colour']);
    expect(m.keys).toContain('full.full.star');
    expect(m.keys.length).toBeGreaterThan(30);
  });

  it('returns an empty manifest for a missing directory', () => {
    expect(loadTrayManifest('/definitely/not/here')).toEqual({ keys: [], modes: [] });
  });

  it('filters keys that are not "<a>.<b>.<c>" and unknown modes', () => {
    expect(
      sanitizeTrayManifest({
        keys: ['full.full.star', '../../etc/passwd', 'a.b', 7, 'x.y.z'],
        modes: ['template', 'evil', 'colour'],
      })
    ).toEqual({ keys: ['full.full.star', 'x.y.z'], modes: ['template', 'colour'] });
    expect(sanitizeTrayManifest(null)).toEqual({ keys: [], modes: [] });
    expect(sanitizeTrayManifest({ keys: 'nope', modes: {} })).toEqual({ keys: [], modes: [] });
  });
});

// ── Impure shell ────────────────────────────────────────────────────────────
// vi.mock is hoisted above the imports at the top of this file, so the pure
// core resolves its type-only electron imports fine while the shell gets stubs.
const {
  mockTrayInstance,
  MockTray,
  mockMenu,
  mockApp,
  mockCreateFromPath,
  mockPowerMonitor,
  mockSystemPreferences,
} = vi.hoisted(() => {
  const mockTrayInstance = {
    setToolTip: vi.fn(),
    setContextMenu: vi.fn(),
    setImage: vi.fn(),
    popUpContextMenu: vi.fn(),
    setIgnoreDoubleClickEvents: vi.fn(),
    on: vi.fn(),
    destroy: vi.fn(),
    isDestroyed: vi.fn(() => false),
  };
  const MockTray = vi.fn().mockImplementation(function () {
    return mockTrayInstance;
  });
  const mockMenu = { buildFromTemplate: vi.fn((t: unknown) => ({ items: t })) };
  const mockApp = { quit: vi.fn(), isPackaged: false };
  const mockCreateFromPath = vi.fn((p: string) => ({
    path: p,
    isEmpty: () => false,
    setTemplateImage: vi.fn(),
  }));
  const mockPowerMonitor = {
    on: vi.fn(),
    removeListener: vi.fn(),
    isOnBatteryPower: vi.fn(() => false),
  };
  const mockSystemPreferences = {
    getAnimationSettings: vi.fn(() => ({
      shouldRenderRichAnimation: true,
      scrollAnimationsEnabledBySystem: true,
      prefersReducedMotion: false,
    })),
  };
  return {
    mockTrayInstance,
    MockTray,
    mockMenu,
    mockApp,
    mockCreateFromPath,
    mockPowerMonitor,
    mockSystemPreferences,
  };
});

vi.mock('electron', () => ({
  Tray: MockTray,
  Menu: mockMenu,
  app: mockApp,
  nativeImage: { createFromPath: mockCreateFromPath },
  powerMonitor: mockPowerMonitor,
  systemPreferences: mockSystemPreferences,
}));

const SNAP: TrayStatusSnapshot = {
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

type MenuTemplateItem = {
  id?: string;
  label?: string;
  type?: string;
  checked?: boolean;
  click?: () => void;
  submenu?: MenuTemplateItem[];
};

function lastTemplate(): MenuTemplateItem[] {
  const calls = mockMenu.buildFromTemplate.mock.calls;
  return calls[calls.length - 1][0] as MenuTemplateItem[];
}

function findItem(items: MenuTemplateItem[], id: string): MenuTemplateItem | undefined {
  for (const i of items) {
    if (i.id === id) return i;
    const inner = i.submenu ? findItem(i.submenu, id) : undefined;
    if (inner) return inner;
  }
  return undefined;
}

function trayListener(event: string): ((...args: unknown[]) => void) | undefined {
  const call = mockTrayInstance.on.mock.calls.find((c) => c[0] === event);
  return call?.[1] as ((...args: unknown[]) => void) | undefined;
}

function lastImagePath(): string {
  const calls = mockTrayInstance.setImage.mock.calls;
  return (calls[calls.length - 1][0] as { path: string }).path;
}

describe('initTray / destroyTray / isTrayActive (shell)', () => {
  const originalPlatform = process.platform;
  const setPlatform = (p: NodeJS.Platform) =>
    Object.defineProperty(process, 'platform', { value: p, configurable: true });

  beforeEach(() => {
    vi.clearAllMocks();
    mockTrayInstance.isDestroyed.mockReturnValue(false);
    MockTray.mockImplementation(function () {
      return mockTrayInstance;
    });
    mockCreateFromPath.mockImplementation((p: string) => ({
      path: p,
      isEmpty: () => false,
      setTemplateImage: vi.fn(),
    }));
    mockSystemPreferences.getAnimationSettings.mockImplementation(() => ({
      shouldRenderRichAnimation: true,
      scrollAnimationsEnabledBySystem: true,
      prefersReducedMotion: false,
    }));
    mockApp.isPackaged = false;
    setPlatform('linux');
    // Fresh module state per test — tray.ts holds a module-scoped singleton.
    vi.resetModules();
  });

  afterEach(() => {
    setPlatform(originalPlatform);
  });

  async function freshTray() {
    return await import('../../../src/main/tray');
  }

  function deps(
    overrides: Partial<Parameters<Awaited<ReturnType<typeof freshTray>>['initTray']>[0]> = {}
  ) {
    return {
      getMainWindow: () => null,
      createWindow: vi.fn(),
      downloadUpdate: vi.fn(() => Promise.resolve([])),
      installUpdate: vi.fn(() => Promise.resolve()),
      ...overrides,
    };
  }

  it('constructs the tray with the neutral frame, tooltip and Open/Quit menu', async () => {
    const trayModule = await freshTray();
    trayModule.initTray(deps());

    expect(MockTray).toHaveBeenCalledTimes(1);
    const initial = MockTray.mock.calls[0][0] as { path: string };
    expect(initial.path).toBe(
      path.join(process.cwd(), 'assets', 'tray', 'colour', 'full.full.star-22.png')
    );
    // Dev build → hardcoded suffix (R19).
    expect(mockTrayInstance.setToolTip).toHaveBeenCalledWith('Concord Voice [dev]');
    expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(1);
    expect(lastTemplate().map((i) => i.id ?? i.type)).toEqual(['open', 'separator', 'quit']);
    expect(trayModule.isTrayActive()).toBe(true);
  });

  it('is idempotent — second init does not construct a second Tray', async () => {
    const trayModule = await freshTray();
    const d = deps();
    trayModule.initTray(d);
    trayModule.initTray(d);
    expect(MockTray).toHaveBeenCalledTimes(1);
  });

  it('decodes every manifest frame once at init; template images are flagged (darwin)', async () => {
    setPlatform('darwin');
    const trayModule = await freshTray();
    trayModule.initTray(deps());
    const m = trayModule.loadTrayManifest(path.join(process.cwd(), 'assets', 'tray'));
    expect(mockCreateFromPath).toHaveBeenCalledTimes(m.keys.length * m.modes.length);
    const results = mockCreateFromPath.mock.results.map(
      (r) => r.value as { path: string; setTemplateImage: ReturnType<typeof vi.fn> }
    );
    const template = results.find((r) => r.path.includes('/template/'));
    const colour = results.find((r) => r.path.includes('/colour/'));
    expect(template?.setTemplateImage).toHaveBeenCalledWith(true);
    expect(colour?.setTemplateImage).toHaveBeenCalledWith(false);
    // darwin/win32 load the 16px base; nativeImage picks the @2x sibling.
    expect(template?.path.endsWith('.png') && !template.path.endsWith('-22.png')).toBe(true);
  });

  describe('click behaviour (spec §3)', () => {
    it('darwin: ignores double clicks, pops the menu on click and right-click, never setContextMenu', async () => {
      setPlatform('darwin');
      const trayModule = await freshTray();
      const createWindow = vi.fn();
      trayModule.initTray(deps({ createWindow }));

      expect(mockTrayInstance.setIgnoreDoubleClickEvents).toHaveBeenCalledWith(true);
      trayListener('click')?.();
      trayListener('right-click')?.();
      expect(mockTrayInstance.popUpContextMenu).toHaveBeenCalledTimes(2);
      const menu = mockTrayInstance.popUpContextMenu.mock.calls[0][0] as { items: unknown };
      expect(menu.items).toBe(lastTemplate());
      expect(trayListener('double-click')).toBeUndefined();
      expect(createWindow).not.toHaveBeenCalled();

      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      expect(mockTrayInstance.setContextMenu).not.toHaveBeenCalled();
    });

    it('win32: click pops the menu, right-click uses the native context menu, double-click activates', async () => {
      setPlatform('win32');
      const trayModule = await freshTray();
      const createWindow = vi.fn();
      trayModule.initTray(deps({ createWindow }));

      expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(1);
      trayListener('click')?.();
      expect(mockTrayInstance.popUpContextMenu).toHaveBeenCalledTimes(1);
      expect(createWindow).not.toHaveBeenCalled();
      trayListener('double-click')?.();
      expect(createWindow).toHaveBeenCalledTimes(1);
      expect(mockTrayInstance.setIgnoreDoubleClickEvents).not.toHaveBeenCalled();
    });

    it('linux: native context menu, click activates, no popUp', async () => {
      const trayModule = await freshTray();
      const createWindow = vi.fn();
      trayModule.initTray(deps({ createWindow }));

      expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(1);
      trayListener('click')?.();
      expect(createWindow).toHaveBeenCalledTimes(1);
      expect(mockTrayInstance.popUpContextMenu).not.toHaveBeenCalled();
      expect(trayListener('double-click')).toBeUndefined();
    });
  });

  describe('menu actions', () => {
    async function inVoiceTray(overrides = {}) {
      const trayModule = await freshTray();
      const d = deps(overrides);
      trayModule.initTray(d);
      const sent: unknown[] = [];
      trayModule.setTrayActionSender((a) => sent.push(a));
      return { trayModule, d, sent };
    }

    it('open activates; unread activates AND sends open-unread', async () => {
      const { trayModule, d, sent } = await inVoiceTray();
      trayModule.applyTraySnapshot({ ...SNAP, unread: 3 });
      findItem(lastTemplate(), 'open')?.click?.();
      expect(d.createWindow).toHaveBeenCalledTimes(1);
      findItem(lastTemplate(), 'unread')?.click?.();
      expect(d.createWindow).toHaveBeenCalledTimes(2);
      expect(sent).toEqual([{ kind: 'open-unread' }]);
    });

    it('quit calls bare app.quit() — no preventDefault path (#1383)', async () => {
      const { trayModule } = await inVoiceTray();
      trayModule.applyTraySnapshot(SNAP);
      findItem(lastTemplate(), 'quit')?.click?.();
      expect(mockApp.quit).toHaveBeenCalledTimes(1);
    });

    it('download-update and restart-update call main directly', async () => {
      const { trayModule, d, sent } = await inVoiceTray();
      trayModule.applyTraySnapshot(SNAP);
      trayModule.notifyTrayUpdate('available');
      findItem(lastTemplate(), 'download-update')?.click?.();
      expect(d.downloadUpdate).toHaveBeenCalledTimes(1);
      trayModule.notifyTrayUpdate('ready');
      findItem(lastTemplate(), 'restart-update')?.click?.();
      expect(d.installUpdate).toHaveBeenCalledTimes(1);
      expect(sent).toEqual([]);
    });

    it('a rejected download / install is logged message-only and does not throw', async () => {
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const { trayModule } = await inVoiceTray({
        downloadUpdate: vi.fn(() => Promise.reject(new Error('net down'))),
        installUpdate: vi.fn(() => Promise.reject('weird')),
      });
      trayModule.applyTraySnapshot(SNAP);
      trayModule.notifyTrayUpdate('available');
      findItem(lastTemplate(), 'download-update')?.click?.();
      trayModule.notifyTrayUpdate('ready');
      findItem(lastTemplate(), 'restart-update')?.click?.();
      await new Promise((r) => setTimeout(r, 0));
      expect(errorSpy).toHaveBeenCalledWith('Tray update action failed:', 'net down');
      expect(errorSpy).toHaveBeenCalledWith('Tray update action failed:', 'tray_update_failed');
      errorSpy.mockRestore();
    });

    it('mute while unchecked sends set-muted:true; voice + presence rows send their actions', async () => {
      const { trayModule, sent } = await inVoiceTray();
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel', sharing: true });
      findItem(lastTemplate(), 'mute')?.click?.();
      findItem(lastTemplate(), 'deafen')?.click?.();
      findItem(lastTemplate(), 'stop-share')?.click?.();
      findItem(lastTemplate(), 'leave')?.click?.();
      findItem(lastTemplate(), 'presence-dnd')?.click?.();
      expect(sent).toEqual([
        { kind: 'set-muted', value: true },
        { kind: 'set-deafened', value: true },
        { kind: 'stop-share' },
        { kind: 'leave-voice' },
        { kind: 'set-presence', value: 'dnd' },
      ]);
    });

    it('drops actions when no sender is set, and survives a throwing sender', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      expect(() => findItem(lastTemplate(), 'leave')?.click?.()).not.toThrow();
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      trayModule.setTrayActionSender(() => {
        throw new Error('destroyed');
      });
      expect(() => findItem(lastTemplate(), 'leave')?.click?.()).not.toThrow();
      expect(errorSpy).toHaveBeenCalledWith('Tray action send failed:', 'destroyed');
      errorSpy.mockRestore();
    });
  });

  describe('applying snapshots', () => {
    it('renders frame, tooltip and menu; re-sets the menu only when its key changed (R7)', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(1);

      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      expect(lastImagePath()).toContain(`${path.sep}colour${path.sep}green.full.star-22.png`);
      expect(mockTrayInstance.setToolTip).toHaveBeenLastCalledWith(
        'Concord Voice — In voice [dev]'
      );
      expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(2);

      // Speaking edge: icon + tooltip change, menu does not.
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel', speaking: true });
      expect(lastImagePath()).toContain('green.lit.star-22.png');
      expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(2);

      // Same tooltip → not re-set.
      const tipCalls = mockTrayInstance.setToolTip.mock.calls.length;
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel', speaking: true });
      expect(mockTrayInstance.setToolTip.mock.calls.length).toBe(tipCalls);
    });

    it('applies the icon style: light → white sprites', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      trayModule.applyTraySnapshot({ ...SNAP, iconStyle: 'light' });
      expect(lastImagePath()).toContain(`${path.sep}white${path.sep}full.full.star-22.png`);
    });

    it('null resets to the neutral icon and the Open/Quit menu', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel', muted: true });
      trayModule.applyTraySnapshot(null);
      expect(lastImagePath()).toContain('full.full.star-22.png');
      expect(lastTemplate().map((i) => i.id ?? i.type)).toEqual(['open', 'separator', 'quit']);
    });

    it('stores the snapshot without touching the OS while the tray is inactive, renders it at init', async () => {
      const trayModule = await freshTray();
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      expect(MockTray).not.toHaveBeenCalled();
      expect(mockTrayInstance.setImage).not.toHaveBeenCalled();
      trayModule.initTray(deps());
      expect(lastImagePath()).toContain('green.full.star-22.png');
    });

    it('falls back to the neutral frame and logs once when a frame is missing', async () => {
      mockCreateFromPath.mockImplementation((p: string) => ({
        path: p,
        isEmpty: () => p.includes('green.full.star'),
        setTemplateImage: vi.fn(),
      }));
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      mockTrayInstance.setImage.mockClear();
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      expect(lastImagePath()).toContain('full.full.star-22.png');
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel', speaking: true });
      trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' });
      const missing = errorSpy.mock.calls.filter((c) => c[0] === 'Tray frame missing:');
      expect(missing).toHaveLength(1);
      expect(missing[0][1]).toBe('colour/green.full.star');
      errorSpy.mockRestore();
    });

    it('a throwing render is contained (message-only log)', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
      mockTrayInstance.setImage.mockImplementationOnce(() => {
        throw new Error('dbus gone');
      });
      expect(() => trayModule.applyTraySnapshot({ ...SNAP, voice: 'channel' })).not.toThrow();
      expect(errorSpy).toHaveBeenCalledWith('Tray render failed:', 'dbus gone');
      errorSpy.mockRestore();
    });

    it('notifyTrayUpdate drives the update rows, including download percent', async () => {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      trayModule.applyTraySnapshot(SNAP);
      trayModule.notifyTrayUpdate('downloading', 42.7);
      expect(findItem(lastTemplate(), 'downloading')?.label).toBe('Downloading update… 40%');
      trayModule.notifyTrayUpdate('downloading', Number.NaN);
      expect(findItem(lastTemplate(), 'downloading')?.label).toBe('Downloading update… 40%');
      trayModule.notifyTrayUpdate('none');
      expect(findItem(lastTemplate(), 'downloading')).toBeUndefined();
    });
  });

  describe('motion inputs', () => {
    it('reads reduced motion from the OS on each apply: breath snaps to its final frame', async () => {
      setPlatform('darwin');
      vi.useFakeTimers();
      try {
        const trayModule = await freshTray();
        trayModule.initTray(deps());
        mockSystemPreferences.getAnimationSettings.mockReturnValue({
          shouldRenderRichAnimation: true,
          scrollAnimationsEnabledBySystem: true,
          prefersReducedMotion: true,
        });
        trayModule.applyTraySnapshot({ ...SNAP, conn: 'reconnecting' });
        expect(trayModule.trayActiveTimers()).toBe(0);
        expect(lastImagePath()).toContain('amber.full.dim35.png');
      } finally {
        vi.useRealTimers();
      }
    });

    it('battery gates the breath; on-ac resumes it; listeners are wired once', async () => {
      setPlatform('darwin');
      vi.useFakeTimers();
      try {
        const trayModule = await freshTray();
        trayModule.initTray(deps());
        const onBattery = mockPowerMonitor.on.mock.calls.find(
          (c) => c[0] === 'on-battery'
        )?.[1] as () => void;
        const onAc = mockPowerMonitor.on.mock.calls.find(
          (c) => c[0] === 'on-ac'
        )?.[1] as () => void;
        expect(onBattery).toBeTypeOf('function');
        expect(onAc).toBeTypeOf('function');

        trayModule.applyTraySnapshot({ ...SNAP, conn: 'reconnecting' });
        expect(trayModule.trayActiveTimers()).toBe(1);
        onBattery();
        expect(trayModule.trayActiveTimers()).toBe(0);
        onAc();
        expect(trayModule.trayActiveTimers()).toBe(1);
        trayModule.destroyTray();
      } finally {
        vi.useRealTimers();
      }
    });

    it('window focus gates the mention burst', async () => {
      setPlatform('darwin');
      vi.useFakeTimers();
      try {
        const trayModule = await freshTray();
        trayModule.initTray(deps());
        trayModule.setTrayWindowFocused(true);
        trayModule.applyTraySnapshot({ ...SNAP, mention: true });
        expect(trayModule.trayActiveTimers()).toBe(0);
        trayModule.applyTraySnapshot(SNAP);
        trayModule.setTrayWindowFocused(false);
        trayModule.applyTraySnapshot({ ...SNAP, mention: true });
        expect(trayModule.trayActiveTimers()).toBe(1);
        trayModule.destroyTray();
      } finally {
        vi.useRealTimers();
      }
    });

    it('survives a missing getAnimationSettings / isOnBatteryPower', async () => {
      mockSystemPreferences.getAnimationSettings.mockImplementation(() => {
        throw new Error('unsupported');
      });
      mockPowerMonitor.isOnBatteryPower.mockImplementationOnce(() => {
        throw new Error('unsupported');
      });
      const trayModule = await freshTray();
      expect(() => trayModule.initTray(deps())).not.toThrow();
      expect(trayModule.isTrayActive()).toBe(true);
    });
  });

  it('destroyTray order: timer disposed, powerMonitor listeners removed, snapshot cleared, then tray.destroy()', async () => {
    setPlatform('darwin');
    vi.useFakeTimers();
    try {
      const trayModule = await freshTray();
      trayModule.initTray(deps());
      trayModule.applyTraySnapshot({ ...SNAP, conn: 'reconnecting' });
      expect(trayModule.trayActiveTimers()).toBe(1);

      trayModule.destroyTray();
      expect(trayModule.trayActiveTimers()).toBe(0);
      expect(mockPowerMonitor.removeListener).toHaveBeenCalledWith(
        'on-battery',
        expect.any(Function)
      );
      expect(mockPowerMonitor.removeListener).toHaveBeenCalledWith('on-ac', expect.any(Function));
      const removeOrder = Math.max(...mockPowerMonitor.removeListener.mock.invocationCallOrder);
      expect(removeOrder).toBeLessThan(mockTrayInstance.destroy.mock.invocationCallOrder[0]);
      expect(trayModule.isTrayActive()).toBe(false);
      expect(vi.getTimerCount()).toBe(0);

      // Snapshot cleared: a re-init shows the neutral frame, not the stored reconnecting one.
      mockTrayInstance.setImage.mockClear();
      MockTray.mockClear();
      trayModule.initTray(deps());
      expect((MockTray.mock.calls[0][0] as { path: string }).path).toContain('full.full.star.png');
      expect(
        mockTrayInstance.setImage.mock.calls.every(
          (c) => !(c[0] as { path: string }).path.includes('amber')
        )
      ).toBe(true);
      trayModule.destroyTray();
    } finally {
      vi.useRealTimers();
    }
  });

  it('survives Tray construction failure: message-only console.error, app continues, inactive', async () => {
    MockTray.mockImplementation(function () {
      throw new Error('no system tray available');
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    expect(() => trayModule.initTray(deps())).not.toThrow();
    // Message-only per [internal]rules/observability.md — never the raw err object.
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'no system tray available');
    expect(trayModule.isTrayActive()).toBe(false);
    // A snapshot after a failed init is stored, never rendered.
    expect(() => trayModule.applyTraySnapshot(SNAP)).not.toThrow();
    expect(mockTrayInstance.setImage).not.toHaveBeenCalled();
    errorSpy.mockRestore();
  });

  it('logs a stable identifier when a non-Error is thrown', async () => {
    MockTray.mockImplementation(function () {
      // eslint-disable-next-line @typescript-eslint/only-throw-error -- exercising the non-Error path
      throw 'string-throw';
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();
    trayModule.initTray(deps());
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'tray_init_failed');
    errorSpy.mockRestore();
  });

  it('treats an empty fallback frame (missing/corrupt icon file) as init failure', async () => {
    // createFromPath never throws — a missing file yields an EMPTY image, and
    // new Tray(emptyImage) can "succeed" as an invisible icon. That would
    // defeat the trayless fallbacks: isTrayActive() true, but nothing to click.
    mockCreateFromPath.mockImplementation((p: string) => ({
      path: p,
      isEmpty: () => true,
      setTemplateImage: vi.fn(),
    }));
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    trayModule.initTray(deps());

    expect(MockTray).not.toHaveBeenCalled();
    expect(trayModule.isTrayActive()).toBe(false);
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'tray icon missing or unreadable');
    errorSpy.mockRestore();
  });

  it('destroys a half-constructed tray when wiring throws after construction, and unhooks powerMonitor', async () => {
    mockTrayInstance.setContextMenu.mockImplementationOnce(() => {
      throw new Error('wiring failed');
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    trayModule.initTray(deps());

    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
    expect(trayModule.isTrayActive()).toBe(false);
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'wiring failed');
    expect(mockPowerMonitor.removeListener).toHaveBeenCalledWith(
      'on-battery',
      expect.any(Function)
    );
    expect(trayModule.trayActiveTimers()).toBe(0);
    errorSpy.mockRestore();
  });

  it('destroyTray destroys and deactivates; idempotent on second call', async () => {
    const trayModule = await freshTray();
    trayModule.initTray(deps());
    expect(trayModule.isTrayActive()).toBe(true);

    trayModule.destroyTray();
    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
    expect(trayModule.isTrayActive()).toBe(false);

    trayModule.destroyTray(); // second call: no throw, no double-destroy
    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
  });
});
