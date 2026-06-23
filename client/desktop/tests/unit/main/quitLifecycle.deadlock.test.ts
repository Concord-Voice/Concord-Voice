// @vitest-environment node
//
// Regression for #1383 — desktop client is unquittable.
//
// The BrowserWindow 'close' handler (src/main/main.ts) routes [X] to
// tray/toolbar/quit based on Client Behavior, calling event.preventDefault()
// to suppress the default close→destroy. Electron aborts the ENTIRE app.quit()
// sequence if ANY window's 'close' handler calls preventDefault(). Without an
// `isQuitting` guard, every quit path (⌘Q, Dock→Quit, window:quit, app:quit)
// deadlocks: before-quit fires, the window 'close' event fires, the intercept
// vetoes it, the window is never destroyed, the quit is cancelled.
//
// A real 'close' event fires ALL registered listeners (here: the window-state
// persistence listener from attachWindowState() AND the Client Behavior
// intercept). This test invokes every 'close' listener — exactly as Electron
// would — and asserts none of them vetoes a quit-in-progress.
//
// It re-imports main.ts under each of process.platform ∈ {darwin, win32,
// linux} to confirm the deadlock is platform-INDEPENDENT (the close handler
// has no platform gate). Pre-fix: the deadlock reproduces on all three.
// Post-fix (isQuitting guard): a genuine quit lets the window close on all three.
import { describe, it, expect, vi, afterEach, type Mock } from 'vitest';

// ── Hoisted shared mocks (available during vi.mock factory execution) ──────
const { mockMainWindow, MockBrowserWindow, mockApp } = vi.hoisted(() => {
  const mockWebContents = {
    openDevTools: vi.fn(),
    closeDevTools: vi.fn(),
    isDevToolsOpened: vi.fn(() => false),
    setWindowOpenHandler: vi.fn(),
    send: vi.fn(),
    on: vi.fn(),
  };
  const mockMainWindow = {
    loadURL: vi.fn().mockResolvedValue(undefined),
    loadFile: vi.fn().mockResolvedValue(undefined),
    once: vi.fn(),
    on: vi.fn(),
    show: vi.fn(),
    hide: vi.fn(),
    minimize: vi.fn(),
    close: vi.fn(),
    destroy: vi.fn(),
    focus: vi.fn(),
    isMinimized: vi.fn(() => false),
    isMaximized: vi.fn(() => false),
    getBounds: vi.fn(() => ({ x: 0, y: 0, width: 1280, height: 800 })),
    restore: vi.fn(),
    setAlwaysOnTop: vi.fn(),
    webContents: mockWebContents,
  };
  const bw = vi.fn().mockImplementation(function () {
    return mockMainWindow;
  });
  (bw as Record<string, unknown>).getAllWindows = vi.fn(() => [mockMainWindow]);
  const mockApp = {
    getPath: vi.fn(() => '/tmp/test-userdata'),
    setPath: vi.fn(),
    getAppPath: vi.fn(() => '/tmp/test-app'),
    getVersion: vi.fn(() => '1.0.0-test'),
    requestSingleInstanceLock: vi.fn(() => true),
    whenReady: vi.fn(() => Promise.resolve()),
    isPackaged: false,
    commandLine: { appendSwitch: vi.fn() },
    disableHardwareAcceleration: vi.fn(),
    quit: vi.fn(),
    exit: vi.fn(),
    relaunch: vi.fn(),
    on: vi.fn(),
    getGPUInfo: vi.fn(() => Promise.resolve({ gpuDevice: [] })),
  };
  return { mockMainWindow, MockBrowserWindow: bw, mockApp };
});

vi.mock('electron', () => ({
  BrowserWindow: MockBrowserWindow,
  app: mockApp,
  ipcMain: { handle: vi.fn(), on: vi.fn() },
  nativeImage: {
    createFromPath: vi.fn(() => ({ isEmpty: () => false, toDataURL: () => '' })),
  },
  // Tray/Menu stubs (#1099): main.ts initTray runs during whenReady. Without
  // these, initTray's graceful-failure path fires (console.error noise) and
  // isTrayActive() reads false, silently changing window-all-closed semantics
  // under test.
  Tray: vi.fn().mockImplementation(function () {
    return {
      setToolTip: vi.fn(),
      setContextMenu: vi.fn(),
      on: vi.fn(),
      destroy: vi.fn(),
      isDestroyed: vi.fn(() => false),
    };
  }),
  Menu: { buildFromTemplate: vi.fn((template: unknown) => ({ template })) },
  clipboard: { writeText: vi.fn() },
  desktopCapturer: { getSources: vi.fn(() => Promise.resolve([])) },
  screen: {
    getAllDisplays: vi.fn(() => [
      { id: 1, size: { width: 1920, height: 1080 }, scaleFactor: 1, displayFrequency: 60 },
    ]),
    getPrimaryDisplay: vi.fn(() => ({ id: 1 })),
  },
  session: {
    defaultSession: {
      setPermissionRequestHandler: vi.fn(),
      setPermissionCheckHandler: vi.fn(),
      setDevicePermissionHandler: vi.fn(),
      setCertificateVerifyProc: vi.fn(),
      on: vi.fn(),
      webRequest: { onBeforeSendHeaders: vi.fn() },
    },
  },
  shell: { openExternal: vi.fn(() => Promise.resolve()) },
  powerMonitor: { on: vi.fn() },
}));

vi.mock('node:fs', () => ({
  default: {
    readFileSync: vi.fn(() => JSON.stringify({ enabled: true })),
    writeFileSync: vi.fn(),
    existsSync: vi.fn(() => false),
    statSync: vi.fn(() => ({ isDirectory: () => false })),
    renameSync: vi.fn(),
  },
}));

vi.mock('node:http', () => {
  const mockGet = vi.fn(
    (_url: string, _opts: Record<string, unknown>, _cb: (res: unknown) => void) => {
      const req = {
        on: vi.fn((event: string, handler: (err?: Error) => void) => {
          if (event === 'error') Promise.resolve().then(() => handler(new Error('ECONNREFUSED')));
          return req;
        }),
        destroy: vi.fn(),
      };
      return req;
    }
  );
  return { default: { get: mockGet }, get: mockGet };
});

vi.mock('electron-squirrel-startup', () => ({ default: false }));
vi.mock('../../../src/main/tokenManager', () => ({
  storeRefreshToken: vi.fn(),
  restoreRefreshToken: vi.fn(() => ({
    status: 'ok',
    refreshToken: 't',
    apiBase: 'http://localhost:8080',
  })),
  performRefresh: vi.fn(() => Promise.resolve({ status: 'ok', accessToken: 'a', sessionId: 's' })),
  performLogout: vi.fn(() => Promise.resolve()),
  clearTokens: vi.fn(),
  getCapabilities: vi.fn(() => ({ safeStorage: true, secureKeychain: true })),
  storeE2EEKeys: vi.fn(),
  restoreE2EEKeys: vi.fn(() => ({
    wrappingKeyBase64: 'k',
    preferencesKeyBase64: 'p',
    wrappedPrivateKeyBase64: 'w',
  })),
  setProactiveRefreshCallback: vi.fn(),
  onSystemResume: vi.fn(),
  getCachedAccessToken: vi.fn(() => null),
  getApiBaseOrigin: vi.fn(() => null),
}));
vi.mock('../../../src/main/machineId', () => ({ getMachineId: vi.fn(() => 'mock-machine-id') }));
vi.mock('../../../src/main/updater', () => ({
  initAutoUpdater: vi.fn(),
  stopAutoUpdater: vi.fn(),
  setUpdateFeedUrl: vi.fn(),
  checkForUpdates: vi.fn(() => Promise.resolve({ updateAvailable: false })),
  downloadUpdate: vi.fn(() => Promise.resolve()),
  safeQuitAndInstall: vi.fn(() => Promise.resolve()),
  getAllowPrerelease: vi.fn(() => false),
  setAllowPrerelease: vi.fn(),
  getUpdateLogger: vi.fn(() => null),
  getUpdateLogPath: vi.fn(() => '/tmp/test-logs/update.log'),
}));
vi.mock('../../../src/main/updateSafety', () => ({
  checkUpdateSentinel: vi.fn(() => ({ type: 'none' })),
  finalizeUpdate: vi.fn(() => Promise.resolve()),
  finalizeRollback: vi.fn(() => Promise.resolve()),
  runDeferredCleanup: vi.fn(() => Promise.resolve()),
}));
vi.mock('../../../src/main/userDataMigration', () => ({ migrateUserData: vi.fn() }));
vi.mock('../../../src/main/splashWindow', () => ({
  showSplash: vi.fn(),
  closeSplash: vi.fn(),
  updateSplashError: vi.fn(),
}));
vi.mock('../../../src/main/spaLoader', () => ({
  resolveSpaSource: vi.fn(() => Promise.resolve({ mode: 'bundled', reason: 'test' })),
}));
vi.mock('../../../src/main/ipcContract', () => ({ IPC_CONTRACT_VERSION: '1.0' }));
vi.mock('../../../src/main/permissionManager', () => ({ registerIpcHandlers: vi.fn() }));

// ── Per-platform re-import harness ─────────────────────────────────────────
type HandlerFn = (...args: unknown[]) => unknown;
const PLATFORMS = ['darwin', 'win32', 'linux'] as const;
const originalPlatform = process.platform;

async function importMainUnderPlatform(
  platform: (typeof PLATFORMS)[number]
): Promise<{ closeHandlers: HandlerFn[]; beforeQuit?: HandlerFn }> {
  vi.resetModules();
  mockMainWindow.on.mockClear();
  mockApp.on.mockClear();
  Object.defineProperty(process, 'platform', { value: platform, configurable: true });

  // Import re-runs main.ts module init under this platform; createWindow runs
  // from the (immediately-resolved) whenReady, registering the 'close' handlers.
  await import('../../../src/main/main');
  await new Promise((resolve) => setTimeout(resolve, 100));

  // A real 'close' event fires ALL registered listeners. Collect every one.
  const closeHandlers = (mockMainWindow.on as Mock).mock.calls
    .filter((c) => c[0] === 'close')
    .map((c) => c[1] as HandlerFn);
  const beforeQuitCall = (mockApp.on as Mock).mock.calls.find((c) => c[0] === 'before-quit');
  return { closeHandlers, beforeQuit: beforeQuitCall?.[1] as HandlerFn | undefined };
}

/** Fire every registered 'close' listener with the same event, as Electron does. */
function fireClose(handlers: HandlerFn[], event: { preventDefault: Mock }): void {
  for (const h of handlers) h(event);
}

afterEach(() => {
  Object.defineProperty(process, 'platform', { value: originalPlatform, configurable: true });
});

// ── Tests ──────────────────────────────────────────────────────────────────
describe('quit lifecycle deadlock — regression for #1383', () => {
  for (const platform of PLATFORMS) {
    it(`[${platform}] no 'close' listener may veto the window close once a quit is in progress`, async () => {
      const { closeHandlers, beforeQuit } = await importMainUnderPlatform(platform);
      expect(closeHandlers.length, `[${platform}] at least one 'close' listener`).toBeGreaterThan(
        0
      );
      expect(beforeQuit, `[${platform}] 'before-quit' handler should be registered`).toBeTypeOf(
        'function'
      );

      // Simulate a genuine app.quit(): Electron emits before-quit FIRST, then
      // the window 'close' event (firing all listeners). A correct intercept
      // detects the in-progress quit and lets the window close. The buggy code
      // has no isQuitting guard, so it calls preventDefault() unconditionally →
      // app.quit() is cancelled and the app becomes unquittable (#1383).
      beforeQuit!();
      const event = { preventDefault: vi.fn() };
      fireClose(closeHandlers, event);

      expect(
        event.preventDefault,
        `[${platform}] a quit-in-progress must not be vetoed by the close intercept`
      ).not.toHaveBeenCalled();
    });
  }
});

describe('first-run close default — tray landed (#1099, supersedes the #1383 interim)', () => {
  it('default [X] (not quitting) hides to the system tray', async () => {
    // The tray exists now (#1099): the default Client Behavior routes [X] to
    // hide-to-tray, the behavior #806 designed for. The #1383 interim default
    // (toTray:'none' → [X] quits) is retired; its persisted snapshots are
    // migrated by settingsStore persist v1.
    const { closeHandlers } = await importMainUnderPlatform('darwin');
    expect(closeHandlers.length).toBeGreaterThan(0);

    mockMainWindow.hide.mockClear();
    (mockApp.quit as Mock).mockClear();

    // Plain close, NOT during a quit (before-quit not fired).
    const event = { preventDefault: vi.fn() };
    fireClose(closeHandlers, event);

    expect(mockMainWindow.hide, 'default [X] hides to the tray (#1099)').toHaveBeenCalled();
    expect(
      mockApp.quit,
      'default [X] must not quit now that the tray exists'
    ).not.toHaveBeenCalled();
  });
});
