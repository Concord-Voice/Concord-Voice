import { describe, it, expect, vi, beforeEach } from 'vitest';
import {
  decideActivateAction,
  applyActivate,
  buildContextMenuTemplate,
  resolveTrayIconPath,
  type ActivatableWindow,
} from '../../../src/main/tray';

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

describe('buildContextMenuTemplate', () => {
  it('returns Open / separator / Quit in order with hardcoded labels', () => {
    const onOpen = vi.fn();
    const onQuit = vi.fn();
    const template = buildContextMenuTemplate({ onOpen, onQuit });

    expect(template).toHaveLength(3);
    expect(template[0].label).toBe('Open Concord Voice');
    expect(template[1].type).toBe('separator');
    expect(template[2].label).toBe('Quit Concord Voice');
  });

  it('wires the click handlers to the right callbacks', () => {
    const onOpen = vi.fn();
    const onQuit = vi.fn();
    const template = buildContextMenuTemplate({ onOpen, onQuit });

    (template[0].click as () => void)();
    expect(onOpen).toHaveBeenCalledTimes(1);
    expect(onQuit).not.toHaveBeenCalled();

    (template[2].click as () => void)();
    expect(onQuit).toHaveBeenCalledTimes(1);
  });
});

describe('resolveTrayIconPath', () => {
  it('picks the macOS template image on darwin', () => {
    expect(resolveTrayIconPath('darwin', true, '/res', '/cwd')).toBe('/res/tray/iconTemplate.png');
  });

  it('picks the 22px variant on linux', () => {
    expect(resolveTrayIconPath('linux', true, '/res', '/cwd')).toBe('/res/tray/icon-22.png');
  });

  it('picks the 16px color icon on win32', () => {
    expect(resolveTrayIconPath('win32', true, '/res', '/cwd')).toBe('/res/tray/icon.png');
  });

  it('falls back to the repo-relative path in dev (unpackaged)', () => {
    expect(resolveTrayIconPath('darwin', false, undefined, '/repo/client/desktop')).toBe(
      '/repo/client/desktop/assets/tray/iconTemplate.png'
    );
  });

  it('falls back to the repo-relative path when resourcesPath is undefined despite isPackaged (buildInfo.ts guard)', () => {
    expect(resolveTrayIconPath('win32', true, undefined, '/cwd')).toBe('/cwd/assets/tray/icon.png');
  });
});

// ── Impure shell ────────────────────────────────────────────────────────────
// vi.mock is hoisted above the imports at the top of this file, so the pure
// core resolves its type-only electron imports fine while the shell gets stubs.
const { mockTrayInstance, MockTray, mockMenu, mockApp, mockCreateFromPath } = vi.hoisted(() => {
  const mockTrayInstance = {
    setToolTip: vi.fn(),
    setContextMenu: vi.fn(),
    on: vi.fn(),
    destroy: vi.fn(),
    isDestroyed: vi.fn(() => false),
  };
  const MockTray = vi.fn().mockImplementation(function () {
    return mockTrayInstance;
  });
  const mockMenu = { buildFromTemplate: vi.fn((t: unknown) => ({ items: t })) };
  const mockApp = { quit: vi.fn(), isPackaged: false };
  const mockCreateFromPath = vi.fn(() => ({ isEmpty: () => false }));
  return { mockTrayInstance, MockTray, mockMenu, mockApp, mockCreateFromPath };
});

vi.mock('electron', () => ({
  Tray: MockTray,
  Menu: mockMenu,
  app: mockApp,
  nativeImage: {
    createFromPath: mockCreateFromPath,
  },
}));

describe('initTray / destroyTray / isTrayActive (shell)', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    mockTrayInstance.isDestroyed.mockReturnValue(false);
    MockTray.mockImplementation(function () {
      return mockTrayInstance;
    });
    mockCreateFromPath.mockImplementation(() => ({ isEmpty: () => false }));
    // Fresh module state per test — tray.ts holds a module-scoped singleton.
    vi.resetModules();
  });

  async function freshTray() {
    return await import('../../../src/main/tray');
  }

  it('constructs the tray, sets tooltip + context menu, and reports active', async () => {
    const trayModule = await freshTray();
    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });

    expect(MockTray).toHaveBeenCalledTimes(1);
    expect(mockTrayInstance.setToolTip).toHaveBeenCalledWith('Concord Voice');
    expect(mockTrayInstance.setContextMenu).toHaveBeenCalledTimes(1);
    expect(trayModule.isTrayActive()).toBe(true);
  });

  it('is idempotent — second init does not construct a second Tray', async () => {
    const trayModule = await freshTray();
    const deps = { getMainWindow: () => null, createWindow: vi.fn() };
    trayModule.initTray(deps);
    trayModule.initTray(deps);
    expect(MockTray).toHaveBeenCalledTimes(1);
  });

  it('wires click on all platforms (activate handler reaches createWindow for a null window)', async () => {
    const trayModule = await freshTray();
    const createWindow = vi.fn();
    trayModule.initTray({ getMainWindow: () => null, createWindow });

    const clickCall = mockTrayInstance.on.mock.calls.find((c) => c[0] === 'click');
    expect(clickCall).toBeDefined();
    (clickCall?.[1] as () => void)();
    expect(createWindow).toHaveBeenCalledTimes(1);
  });

  it('wires double-click to the same activate handler on macOS only (issue AC)', async () => {
    const originalPlatform = process.platform;
    try {
      Object.defineProperty(process, 'platform', { value: 'darwin', configurable: true });
      const trayModule = await freshTray();
      const createWindow = vi.fn();
      trayModule.initTray({ getMainWindow: () => null, createWindow });

      const dblCall = mockTrayInstance.on.mock.calls.find((c) => c[0] === 'double-click');
      expect(dblCall).toBeDefined();
      (dblCall?.[1] as () => void)();
      expect(createWindow).toHaveBeenCalledTimes(1);
    } finally {
      Object.defineProperty(process, 'platform', { value: originalPlatform, configurable: true });
    }
  });

  it('does not wire double-click on non-macOS platforms', async () => {
    const originalPlatform = process.platform;
    try {
      Object.defineProperty(process, 'platform', { value: 'linux', configurable: true });
      const trayModule = await freshTray();
      trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });

      const dblCall = mockTrayInstance.on.mock.calls.find((c) => c[0] === 'double-click');
      expect(dblCall).toBeUndefined();
    } finally {
      Object.defineProperty(process, 'platform', { value: originalPlatform, configurable: true });
    }
  });

  it('Quit menu item calls bare app.quit() — no preventDefault path (#1383 composition)', async () => {
    const trayModule = await freshTray();
    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });

    const template = mockMenu.buildFromTemplate.mock.calls[0][0] as Array<{
      label?: string;
      click?: () => void;
    }>;
    const quitItem = template.find((i) => i.label === 'Quit Concord Voice');
    expect(quitItem).toBeDefined();
    quitItem?.click?.();
    expect(mockApp.quit).toHaveBeenCalledTimes(1);
  });

  it('survives Tray construction failure: message-only console.error, app continues, inactive', async () => {
    MockTray.mockImplementation(function () {
      throw new Error('no system tray available');
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    expect(() =>
      trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() })
    ).not.toThrow();
    // Message-only per [internal]rules/observability.md — never the raw err object.
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'no system tray available');
    expect(trayModule.isTrayActive()).toBe(false);
    errorSpy.mockRestore();
  });

  it('logs a stable identifier when a non-Error is thrown', async () => {
    MockTray.mockImplementation(function () {
      // eslint-disable-next-line @typescript-eslint/only-throw-error -- exercising the non-Error path
      throw 'string-throw';
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();
    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'tray_init_failed');
    errorSpy.mockRestore();
  });

  it('treats an empty nativeImage (missing/corrupt icon file) as init failure', async () => {
    // createFromPath never throws — a missing file yields an EMPTY image, and
    // new Tray(emptyImage) can "succeed" as an invisible icon. That would
    // defeat the trayless fallbacks: isTrayActive() true, but nothing to click.
    mockCreateFromPath.mockImplementation(() => ({ isEmpty: () => true }));
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });

    expect(MockTray).not.toHaveBeenCalled();
    expect(trayModule.isTrayActive()).toBe(false);
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'tray icon missing or unreadable');
    errorSpy.mockRestore();
  });

  it('destroys a half-constructed tray when wiring throws after construction', async () => {
    // If setContextMenu/on throw AFTER new Tray() succeeded, the catch must
    // destroy the orphan so no handler-less icon lingers until GC.
    mockTrayInstance.setContextMenu.mockImplementationOnce(() => {
      throw new Error('wiring failed');
    });
    const errorSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const trayModule = await freshTray();

    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });

    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
    expect(trayModule.isTrayActive()).toBe(false);
    expect(errorSpy).toHaveBeenCalledWith('Tray init failed:', 'wiring failed');
    errorSpy.mockRestore();
  });

  it('destroyTray destroys and deactivates; idempotent on second call', async () => {
    const trayModule = await freshTray();
    trayModule.initTray({ getMainWindow: () => null, createWindow: vi.fn() });
    expect(trayModule.isTrayActive()).toBe(true);

    trayModule.destroyTray();
    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
    expect(trayModule.isTrayActive()).toBe(false);

    trayModule.destroyTray(); // second call: no throw, no double-destroy
    expect(mockTrayInstance.destroy).toHaveBeenCalledTimes(1);
  });
});
