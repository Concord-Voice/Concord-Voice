// @vitest-environment node
//
// Regression for #1099 trayless-fallback bug (gitar-bot report on #1411).
//
// With the close-to-tray default landed (#1099), a [X] click routes to
// 'tray' and the BrowserWindow 'close' handler calls preventDefault()+hide().
// initTray() is deliberately fail-soft (sandbox / no StatusNotifier host),
// so a failed init leaves isTrayActive() false. If the close path hides
// unconditionally on Windows/Linux, the user gets an invisible, unrecoverable
// resident process. The window-all-closed guard does NOT cover this because
// preventDefault() means the window never actually closes.
//
// The fix gates the hide-to-tray path on isTrayActive(): when the tray
// failed to initialize, [X] falls back to app.quit() (recoverable). The
// minimize-to-tray redirect (which would otherwise restore()+hide() the
// window) is skipped for the same reason, letting native minimize keep the
// taskbar entry.
import { describe, it, expect, vi, afterEach, type Mock } from 'vitest';

// ── Hoisted mocks: Tray constructor THROWS to exercise initTray's catch ────
const { mockMainWindow, MockBrowserWindow, mockApp, mockTrayCtor } = vi.hoisted(() => {
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
  const mockTrayCtor = vi.fn().mockImplementation(() => {
    throw new Error('tray_init_failed: no StatusNotifier host');
  });
  return { mockMainWindow, MockBrowserWindow: bw, mockApp, mockTrayCtor };
});

vi.mock('electron', () => ({
  BrowserWindow: MockBrowserWindow,
  app: mockApp,
  ipcMain: { handle: vi.fn(), on: vi.fn() },
  nativeImage: {
    createFromPath: vi.fn(() => ({ isEmpty: () => false, toDataURL: () => '' })),
  },
  Tray: mockTrayCtor,
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
const TRAYLESS_PLATFORMS = ['win32', 'linux'] as const;
const originalPlatform = process.platform;

async function importMain(platform: (typeof TRAYLESS_PLATFORMS)[number]): Promise<{
  closeHandlers: HandlerFn[];
  minimizeHandlers: HandlerFn[];
}> {
  vi.resetModules();
  mockMainWindow.on.mockClear();
  mockApp.on.mockClear();
  // Make sure the original-error noise from initTray's catch is silenced.
  vi.spyOn(console, 'error').mockImplementation(() => {});
  Object.defineProperty(process, 'platform', { value: platform, configurable: true });

  await import('../../../src/main/main');
  await new Promise((resolve) => setTimeout(resolve, 100));

  const closeHandlers = (mockMainWindow.on as Mock).mock.calls
    .filter((c) => c[0] === 'close')
    .map((c) => c[1] as HandlerFn);
  const minimizeHandlers = (mockMainWindow.on as Mock).mock.calls
    .filter((c) => c[0] === 'minimize')
    .map((c) => c[1] as HandlerFn);
  return { closeHandlers, minimizeHandlers };
}

function fireAll(handlers: HandlerFn[], event: unknown): void {
  for (const h of handlers) h(event);
}

afterEach(() => {
  Object.defineProperty(process, 'platform', { value: originalPlatform, configurable: true });
  vi.restoreAllMocks();
});

// ── Tests ──────────────────────────────────────────────────────────────────
describe('trayless fallback (#1099): tray init failure must not strand the window', () => {
  for (const platform of TRAYLESS_PLATFORMS) {
    it(`[${platform}] [X] quits when tray failed to initialize`, async () => {
      const { closeHandlers } = await importMain(platform);
      expect(closeHandlers.length).toBeGreaterThan(0);
      // Verify tray init was attempted and threw (precondition for the fallback).
      expect(mockTrayCtor).toHaveBeenCalled();

      mockMainWindow.hide.mockClear();
      (mockApp.quit as Mock).mockClear();

      const event = { preventDefault: vi.fn() };
      fireAll(closeHandlers, event);

      expect(
        mockMainWindow.hide,
        `[${platform}] must NOT hide the window when tray init failed (no way to restore it)`
      ).not.toHaveBeenCalled();
      expect(
        mockApp.quit,
        `[${platform}] must quit instead so the session degrades to a recoverable state`
      ).toHaveBeenCalled();
      expect(event.preventDefault).toHaveBeenCalled();
    });

    it(`[${platform}] minimize-to-tray is skipped when tray failed to initialize`, async () => {
      const { minimizeHandlers } = await importMain(platform);
      expect(minimizeHandlers.length).toBeGreaterThan(0);
      expect(mockTrayCtor).toHaveBeenCalled();

      mockMainWindow.hide.mockClear();
      mockMainWindow.restore.mockClear();

      fireAll(minimizeHandlers, {});

      expect(
        mockMainWindow.hide,
        `[${platform}] minimize must NOT hide the window when tray init failed`
      ).not.toHaveBeenCalled();
      expect(
        mockMainWindow.restore,
        `[${platform}] minimize must NOT restore+hide when tray init failed`
      ).not.toHaveBeenCalled();
    });
  }
});
