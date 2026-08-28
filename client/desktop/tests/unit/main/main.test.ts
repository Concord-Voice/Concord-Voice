// @vitest-environment node
import { describe, it, expect, vi, beforeAll, beforeEach, type Mock } from 'vitest';
import { deferred } from '../../helpers/deferred';

// ── Hoisted mocks (available during vi.mock factory execution) ─────────

const { mockMainWindow, mockWebContents, MockBrowserWindow } = vi.hoisted(() => {
  const mockWebContents = {
    openDevTools: vi.fn(),
    closeDevTools: vi.fn(),
    isDevToolsOpened: vi.fn(() => false),
    setWindowOpenHandler: vi.fn(),
    send: vi.fn(),
    on: vi.fn(),
    getURL: vi.fn(() => ''),
    mainFrame: { url: 'http://localhost:3001/', frameTreeNodeId: 77 },
  };
  const mockMainWindow = {
    loadURL: vi.fn().mockResolvedValue(undefined),
    loadFile: vi.fn().mockResolvedValue(undefined),
    once: vi.fn(),
    on: vi.fn(),
    show: vi.fn(),
    destroy: vi.fn(),
    close: vi.fn(),
    focus: vi.fn(),
    isMinimized: vi.fn(() => false),
    isDestroyed: vi.fn(() => false),
    restore: vi.fn(),
    setAlwaysOnTop: vi.fn(),
    setContentProtection: vi.fn(),
    webContents: mockWebContents,
  };
  // Use mockImplementation (not mockReturnValue) — Vitest requires this for `new` calls
  const bw = vi.fn().mockImplementation(function () {
    return mockMainWindow;
  });
  // Static method used by activate handler
  (bw as Record<string, unknown>).getAllWindows = vi.fn(() => [mockMainWindow]);
  return { mockMainWindow, mockWebContents, MockBrowserWindow: bw };
});

// ── SSO flow teardown spies (#975: window-close tears down BOTH flows) ──
const { mockCancelAppleFlow, mockCancelGoogleFlow } = vi.hoisted(() => ({
  mockCancelAppleFlow: vi.fn(),
  mockCancelGoogleFlow: vi.fn(),
}));
const { mockRevealLoadFailure } = vi.hoisted(() => ({
  mockRevealLoadFailure: vi.fn(),
}));
const { mockProbeSelfHostedServer } = vi.hoisted(() => ({
  mockProbeSelfHostedServer: vi.fn(),
}));
// #2354: the native approval ceremony + its pre-resolution seam.
const { mockShowMessageBox, mockResolveForDisplay, approvalStore } = vi.hoisted(() => ({
  mockShowMessageBox: vi.fn(async () => ({ response: 1 })),
  mockResolveForDisplay: vi.fn(async () => ({
    ok: true,
    address: '10.0.0.5',
    addresses: ['10.0.0.5'],
    decision: { tier: 'tier2', reason: 'private' },
  })),
  // `failAppend` simulates a durable-write failure (safeStorage/fs), so the
  // approval_not_saved path stays reachable after the commit moved behind the probe.
  approvalStore: { records: [] as { origin: string }[], failAppend: false },
}));
const { mockMaybePromptMove } = vi.hoisted(() => ({
  mockMaybePromptMove: vi.fn(() => false),
}));
vi.mock('../../../src/main/oauth/apple/appleFlow', () => ({
  cancelActiveAppleFlow: mockCancelAppleFlow,
  runAppleSignIn: vi.fn(),
}));
vi.mock('../../../src/main/oauth/google/googleFlow', () => ({
  cancelActiveGoogleFlow: mockCancelGoogleFlow,
  runGoogleSignIn: vi.fn(),
}));

// ── Electron mock ──────────────────────────────────────────────────────

vi.mock('electron', () => ({
  BrowserWindow: MockBrowserWindow,
  nativeImage: {
    createFromPath: vi.fn(() => ({
      isEmpty: vi.fn(() => false),
      toDataURL: vi.fn(() => 'data:image/png;base64,MOCKICON'),
    })),
  },
  autoUpdater: { on: vi.fn() },
  app: {
    getPath: vi.fn(() => '/tmp/test-userdata'),
    setPath: vi.fn(), // pinUserDataPath.ts calls app.setPath at import time (#1291)
    getAppPath: vi.fn(() => '/tmp/test-app'),
    getVersion: vi.fn(() => '1.0.0-test'),
    requestSingleInstanceLock: vi.fn(() => true),
    whenReady: vi.fn(() => Promise.resolve()),
    isPackaged: false,
    commandLine: { appendSwitch: vi.fn() },
    disableHardwareAcceleration: vi.fn(),
    quit: vi.fn(),
    relaunch: vi.fn(),
    on: vi.fn(),
    getGPUInfo: vi.fn(() =>
      Promise.resolve({
        gpuDevice: [
          {
            vendorId: 0x10de,
            deviceId: 0x1234,
            driverVendor: 'NVIDIA',
            driverDescription: 'GeForce GTX 1080',
          },
        ],
      })
    ),
  },
  ipcMain: { handle: vi.fn(), on: vi.fn() },
  clipboard: { writeText: vi.fn() },
  desktopCapturer: {
    getSources: vi.fn(() =>
      Promise.resolve([
        {
          id: 'screen:1',
          name: 'Screen 1',
          thumbnail: { toDataURL: () => 'data:image/png;base64,thumb' },
          appIcon: { toDataURL: () => 'data:image/png;base64,icon' },
        },
      ])
    ),
  },
  screen: {
    getAllDisplays: vi.fn(() => [
      {
        id: 1,
        size: { width: 1920, height: 1080 },
        scaleFactor: 2,
        displayFrequency: 120,
        colorDepth: 24,
        colorSpace: 'srgb',
      },
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
      webRequest: {
        onBeforeSendHeaders: vi.fn(),
      },
    },
  },
  shell: { openExternal: vi.fn(() => Promise.resolve()) },
  powerMonitor: { on: vi.fn() },
  dialog: { showMessageBox: mockShowMessageBox },
}));

// ── Node / internal module mocks ───────────────────────────────────────

vi.mock('node:fs', () => ({
  default: {
    readFileSync: vi.fn((filePath: string) =>
      filePath.includes('content-protection.json')
        ? JSON.stringify({ enabled: false })
        : JSON.stringify({ enabled: true })
    ),
    writeFileSync: vi.fn(),
    existsSync: vi.fn(() => false),
    statSync: vi.fn(() => ({ isDirectory: () => false })),
    renameSync: vi.fn(),
    unlinkSync: vi.fn(),
  },
}));

// Mock node:http — make dev server check fail so createWindow falls back to loadFile
vi.mock('node:http', () => {
  const mockGet = vi.fn(
    (_url: string, _opts: Record<string, unknown>, _cb: (res: unknown) => void) => {
      const req = {
        on: vi.fn((event: string, handler: (err?: Error) => void) => {
          if (event === 'error') {
            // Fire error asynchronously to simulate ECONNREFUSED
            Promise.resolve().then(() => handler(new Error('ECONNREFUSED')));
          }
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
  storeRefreshToken: vi.fn(() => 41),
  restoreRefreshToken: vi.fn(() => ({
    status: 'ok',
    token: 'mock-token',
    apiBase: 'http://localhost:8080',
    rememberMe: true,
  })),
  performRefresh: vi.fn(() =>
    Promise.resolve({ status: 'ok', accessToken: 'mock-access', sessionId: 'mock-session' })
  ),
  performLogout: vi.fn(() => Promise.resolve()),
  clearTokens: vi.fn(),
  clearTokensIfOwner: vi.fn((owner: number) => owner === 41),
  getCapabilities: vi.fn(() => ({ safeStorage: true, secureKeychain: true })),
  storeE2EEKeys: vi.fn(),
  storeE2EEKeysIfOwner: vi.fn((_data: unknown, owner: number) => owner === 41),
  restoreE2EEKeys: vi.fn(() => ({
    wrappingKeyBase64: 'key',
    preferencesKeyBase64: 'pkey',
    wrappedPrivateKeyBase64: 'wpk',
  })),
  getCredentialCustodyState: vi.fn(() => ({
    credentialOwner: 41,
    pendingE2EEUnlock: false,
  })),
  setProactiveRefreshCallback: vi.fn(),
  onSystemResume: vi.fn(),
  getCachedAccessToken: vi.fn(() => null),
  getApiBaseOrigin: vi.fn(() => null),
}));

vi.mock('../../../src/main/machineId', () => ({
  getMachineId: vi.fn(() => 'mock-machine-id'),
}));

vi.mock('../../../src/main/selfHostedProbe', async (importActual) => ({
  // normalizeSelfHostedUrl stays REAL — the ceremony keys its approval on the same
  // normalized origin the probe will dial.
  ...(await importActual<typeof import('../../../src/main/selfHostedProbe')>()),
  probeSelfHostedServer: mockProbeSelfHostedServer,
}));

// In-memory approval store so the ceremony's durable writer stays hermetic here.
vi.mock('../../../src/main/selfHostedApprovals', () => ({
  readApprovalsFile: () => approvalStore.records,
  appendApprovalRecord: (r: { origin: string }) => {
    if (approvalStore.failAppend) return false;
    approvalStore.records.push(r);
    return true;
  },
  // Most-recent-wins, matching the real append-only reader.
  findApprovalRecord: (origin: string) =>
    [...approvalStore.records].reverse().find((r) => r.origin === origin),
  _resetApprovalsCacheForTesting: () => {
    approvalStore.records.length = 0;
  },
}));

vi.mock('../../../src/main/guardedRequest', () => ({
  resolveForDisplay: mockResolveForDisplay,
}));

vi.mock('../../../src/main/updater', () => ({
  initAutoUpdater: vi.fn(),
  stopAutoUpdater: vi.fn(),
  setUpdateFeedUrl: vi.fn(),
  checkForUpdates: vi.fn(() => Promise.resolve({ updateAvailable: false })),
  downloadUpdate: vi.fn(() => Promise.resolve()),
  safeQuitAndInstall: vi.fn(() => Promise.resolve()),
  getAllowPrerelease: vi.fn(() => false),
  setAllowPrerelease: vi.fn(),
  forceCheckForUpdates: vi.fn(() => Promise.resolve()),
  getUpdateLogger: vi.fn(() => null),
  getUpdateLogPath: vi.fn(() => '/tmp/test-logs/update-2026-03-25.log'),
}));

vi.mock('../../../src/main/updateSafety', () => ({
  checkUpdateSentinel: vi.fn(() => ({ type: 'none' })),
  finalizeUpdate: vi.fn(() => Promise.resolve()),
  finalizeRollback: vi.fn(() => Promise.resolve()),
  runDeferredCleanup: vi.fn(() => Promise.resolve()),
}));

vi.mock('../../../src/main/userDataMigration', () => ({
  migrateUserData: vi.fn(),
}));

vi.mock('../../../src/main/splashWindow', () => ({
  showSplash: vi.fn(),
  closeSplash: vi.fn(),
  updateSplashError: vi.fn(),
}));

vi.mock('../../../src/main/loadFailureVisibility', () => ({
  revealLoadFailure: mockRevealLoadFailure,
}));

vi.mock('../../../src/main/applicationsFolderGate', () => ({
  maybePromptMove: mockMaybePromptMove,
}));

vi.mock('../../../src/main/spaLoader', () => ({
  resolveSpaSource: vi.fn(() => Promise.resolve({ mode: 'bundled', reason: 'test' })),
  isUnexpectedBundled: vi.fn(() => false),
  isTransientRemoteFailure: vi.fn(() => false),
  captureSpaHash: vi.fn(() => Promise.resolve()),
  hashEntryHtml: vi.fn(() => Promise.resolve('sha256:available')),
  SPA_NO_CACHE_LOAD_OPTIONS: {
    extraHeaders: 'Cache-Control: no-cache\nPragma: no-cache\n',
  },
}));

vi.mock('../../../src/main/ipcContract', () => ({
  IPC_CONTRACT_VERSION: '1.0',
}));

vi.mock('../../../src/main/permissionManager', () => ({
  registerIpcHandlers: vi.fn(),
}));

// ── Import + extract ───────────────────────────────────────────────────

type HandlerFn = (...args: unknown[]) => unknown;
type CallbackFn = (...args: unknown[]) => void;
let handlers: Map<string, HandlerFn>;
const trustedIpcEvent = {
  sender: { id: 41 },
  senderFrame: { url: 'http://localhost:3001/', frameTreeNodeId: 41 },
};
const foreignIpcEvent = {
  sender: { id: 99 },
  senderFrame: { url: 'https://evil.example/', frameTreeNodeId: 99 },
};
let appOnCallbacks: Map<string, CallbackFn>;
let sessionHandlers: {
  permissionRequest?: CallbackFn;
  permissionCheck?: CallbackFn;
  devicePermission?: CallbackFn;
  hidSelect?: CallbackFn;
};
let klipyInterceptor: {
  filter: { urls: string[] };
  callback: (
    details: { url: string; requestHeaders: Record<string, string> },
    cb: (opts: { requestHeaders: Record<string, string> }) => void
  ) => void;
} | null = null;

// #2354: dialog calls attributable to main.ts's module load + init, captured before
// any test can add its own. Order-independent, unlike reading the mock later.
let dialogCallsAfterImport = -1;

// #2354: the approval-prompt bucket is module-level state in the real (unmocked)
// selfHostedCeremonyBudget, so without this every ceremony in this file spends from
// one shared 10-minute window and later tests would throttle on earlier ones.
beforeEach(async () => {
  const { _resetCeremonyBudgetForTesting } =
    await import('../../../src/main/selfHostedCeremonyBudget');
  _resetCeremonyBudgetForTesting();
});

beforeAll(async () => {
  // Import triggers all module-scope side effects + whenReady resolves
  await import('../../../src/main/main');
  dialogCallsAfterImport = mockShowMessageBox.mock.calls.length;

  // Allow async operations to settle (createWindow, whenReady callback)
  await new Promise((resolve) => setTimeout(resolve, 100));

  const electron = await import('electron');

  // Extract IPC handlers from ipcMain.handle mock calls
  handlers = new Map();
  for (const call of (electron.ipcMain.handle as Mock).mock.calls) {
    handlers.set(call[0] as string, call[1] as HandlerFn);
  }

  // Extract app.on callbacks
  appOnCallbacks = new Map();
  for (const call of (electron.app.on as Mock).mock.calls) {
    appOnCallbacks.set(call[0] as string, call[1] as CallbackFn);
  }

  // Extract session permission handlers
  const sess = electron.session.defaultSession;
  sessionHandlers = {};
  const prh = (sess.setPermissionRequestHandler as Mock).mock.calls;
  if (prh.length > 0) sessionHandlers.permissionRequest = prh[0][0] as CallbackFn;
  const pch = (sess.setPermissionCheckHandler as Mock).mock.calls;
  if (pch.length > 0) sessionHandlers.permissionCheck = pch[0][0] as CallbackFn;
  const dph = (sess.setDevicePermissionHandler as Mock).mock.calls;
  if (dph.length > 0) sessionHandlers.devicePermission = dph[0][0] as CallbackFn;
  const hid = (sess.on as Mock).mock.calls.find((c: unknown[]) => c[0] === 'select-hid-device');
  if (hid) sessionHandlers.hidSelect = hid[1] as CallbackFn;

  // Extract KLIPY media proxy webRequest interceptor (#626)
  const wrCalls = (sess.webRequest.onBeforeSendHeaders as Mock).mock.calls;
  if (wrCalls.length > 0) {
    klipyInterceptor = {
      filter: wrCalls[0][0] as { urls: string[] },
      callback: wrCalls[0][1] as NonNullable<typeof klipyInterceptor>['callback'],
    };
  }
});

// ── Tests ──────────────────────────────────────────────────────────────

describe('main.ts', () => {
  describe('module initialization', () => {
    it('calls migrateUserData on startup (before app.whenReady)', async () => {
      const { migrateUserData } = await import('../../../src/main/userDataMigration');
      expect(migrateUserData).toHaveBeenCalled();
    });

    it('reads hardware acceleration preference on startup', async () => {
      const fs = (await import('node:fs')).default;
      expect(fs.readFileSync).toHaveBeenCalled();
    });

    it('applies initial content protection before loading renderer content', () => {
      const protectionOrder = mockMainWindow.setContentProtection.mock.invocationCallOrder[0];
      const rendererLoadOrders = [
        mockMainWindow.loadURL.mock.invocationCallOrder[0],
        mockMainWindow.loadFile.mock.invocationCallOrder[0],
      ].filter((order): order is number => order !== undefined);

      expect(protectionOrder).toBeDefined();
      expect(rendererLoadOrders.length).toBeGreaterThan(0);
      expect(protectionOrder).toBeLessThan(Math.min(...rendererLoadOrders));
    });

    it('appends accelerated video flags when hw accel enabled', async () => {
      const { app } = await import('electron');
      expect(app.commandLine.appendSwitch).toHaveBeenCalledWith(
        'enable-features',
        'AcceleratedVideoEncoder,AcceleratedVideoDecodeLinuxGL,WebRtcAV1HWEncode'
      );
    });

    it('sets autoplay policy for WebRTC', async () => {
      const { app } = await import('electron');
      expect(app.commandLine.appendSwitch).toHaveBeenCalledWith(
        'autoplay-policy',
        'no-user-gesture-required'
      );
    });

    it('requests single instance lock', async () => {
      const { app } = await import('electron');
      expect(app.requestSingleInstanceLock).toHaveBeenCalled();
    });

    it('registers second-instance handler when lock acquired', () => {
      expect(appOnCallbacks.has('second-instance')).toBe(true);
    });

    it('registers web-contents-created handler', () => {
      expect(appOnCallbacks.has('web-contents-created')).toBe(true);
    });

    it('registers window-all-closed handler', () => {
      expect(appOnCallbacks.has('window-all-closed')).toBe(true);
    });

    it('registers before-quit handler', () => {
      expect(appOnCallbacks.has('before-quit')).toBe(true);
    });

    it('checks Applications-folder relocation before creating BrowserWindow', () => {
      const gateOrder = mockMaybePromptMove.mock.invocationCallOrder[0];
      const windowOrder = MockBrowserWindow.mock.invocationCallOrder[0];

      expect(gateOrder).toBeDefined();
      expect(windowOrder).toBeDefined();
      expect(gateOrder).toBeLessThan(windowOrder);
    });
  });

  describe('IPC: app info', () => {
    it('registers content-protection getter and setter channels', () => {
      expect(handlers.has('app:getContentProtection')).toBe(true);
      expect(handlers.has('app:setContentProtection')).toBe(true);
    });

    it('returns false for absent or malformed content-protection preferences', async () => {
      const fs = (await import('node:fs')).default;
      (fs.readFileSync as Mock).mockImplementation(() => '{"enabled":"yes"}');
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(false);
      (fs.readFileSync as Mock).mockImplementation(
        () => '{"enabled":true,"previousEnabled":false,"staged":false}'
      );
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(false);
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? '{"enabled":false}' : '{"enabled":true}'
      );
    });

    it('returns false when content-protection preferences cannot be read', async () => {
      const fs = (await import('node:fs')).default;
      (fs.readFileSync as Mock).mockImplementation((filePath: string) => {
        if (filePath.includes('content-protection.json')) throw new Error('unreadable');
        return JSON.stringify({ enabled: true });
      });

      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(false);
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : JSON.stringify({ enabled: true })
      );
    });

    it('persists a trusted boolean before updating live windows', async () => {
      const fs = (await import('node:fs')).default;
      const pip = { isDestroyed: vi.fn(() => false), setContentProtection: vi.fn() };
      Object.assign(pip, {
        loadURL: vi.fn().mockResolvedValue(undefined),
        on: vi.fn(),
        focus: vi.fn(),
        destroy: vi.fn(),
      });
      (MockBrowserWindow as Mock).mockImplementationOnce(function () {
        return pip;
      });
      await handlers.get('pip:open')!(trustedIpcEvent, { id: 'content-protection-pip' });
      (fs.writeFileSync as Mock).mockClear();
      mockMainWindow.setContentProtection.mockClear();
      pip.setContentProtection.mockClear();
      const result = await handlers.get('app:setContentProtection')!(trustedIpcEvent, true);
      expect(result).toBe(true);
      expect(fs.writeFileSync).toHaveBeenNthCalledWith(
        1,
        expect.stringMatching(/content-protection\.json\.\d+\.tmp$/),
        JSON.stringify({ enabled: true, previousEnabled: false, staged: true }),
        'utf-8'
      );
      expect(fs.renameSync).toHaveBeenNthCalledWith(
        1,
        expect.stringMatching(/content-protection\.json\.\d+\.tmp$/),
        expect.stringContaining('content-protection.json')
      );
      expect(fs.writeFileSync).toHaveBeenNthCalledWith(
        2,
        expect.stringMatching(/content-protection\.json\.\d+\.tmp$/),
        JSON.stringify({ enabled: true }),
        'utf-8'
      );
      expect(mockMainWindow.setContentProtection).toHaveBeenCalledWith(true);
      expect(pip.setContentProtection).toHaveBeenCalledWith(true);
      expect(fs.renameSync.mock.invocationCallOrder[0]).toBeLessThan(
        mockMainWindow.setContentProtection.mock.invocationCallOrder[0]
      );
      expect(fs.renameSync.mock.invocationCallOrder[0]).toBeLessThan(
        pip.setContentProtection.mock.invocationCallOrder[0]
      );
    });

    it('canonicalizes the target value only after every live window accepts it', async () => {
      const fs = (await import('node:fs')).default;
      let durable = JSON.stringify({ enabled: false });
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
      );
      let temporary = '';
      (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
        temporary = contents;
      });
      (fs.renameSync as Mock).mockImplementation(() => {
        durable = temporary;
      });

      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, true)).resolves.toBe(
        true
      );
      expect(durable).toBe(JSON.stringify({ enabled: true }));
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(true);

      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : '{"enabled":true}'
      );
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      (fs.renameSync as Mock).mockImplementation(() => undefined);
    });

    it('keeps the staged previous value durable when a native content-protection call fails', async () => {
      const fs = (await import('node:fs')).default;
      const pip = {
        isDestroyed: vi.fn(() => false),
        setContentProtection: vi.fn(),
        loadURL: vi.fn().mockResolvedValue(undefined),
        on: vi.fn(),
        focus: vi.fn(),
        destroy: vi.fn(),
      };
      (MockBrowserWindow as Mock).mockImplementationOnce(function () {
        return pip;
      });
      await handlers.get('pip:open')!(trustedIpcEvent, { id: 'content-protection-rollback-pip' });
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      mockMainWindow.setContentProtection.mockReset();
      pip.setContentProtection.mockReset();
      pip.setContentProtection.mockImplementation(() => {
        throw new Error('native failure');
      });
      const result = await handlers.get('app:setContentProtection')!(trustedIpcEvent, true);
      expect(result).toBe(false);
      expect(mockMainWindow.setContentProtection).toHaveBeenLastCalledWith(false);
      expect(fs.writeFileSync).toHaveBeenLastCalledWith(
        expect.stringMatching(/content-protection\.json\.\d+\.tmp$/),
        JSON.stringify({ enabled: true, previousEnabled: false, staged: true }),
        'utf-8'
      );
    });

    it('reads the old value from a staged record after native failure in either direction', async () => {
      const fs = (await import('node:fs')).default;
      let durable = JSON.stringify({ enabled: true });
      let temporary = '';
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
      );
      (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
        temporary = contents;
      });
      (fs.renameSync as Mock).mockImplementation(() => {
        durable = temporary;
      });
      mockMainWindow.setContentProtection.mockImplementationOnce(() => {
        throw new Error('native failure');
      });

      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, false)).resolves.toBe(
        false
      );
      expect(durable).toBe(JSON.stringify({ enabled: false, previousEnabled: true, staged: true }));
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(true);

      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : '{"enabled":true}'
      );
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      (fs.renameSync as Mock).mockImplementation(() => undefined);
      mockMainWindow.setContentProtection.mockImplementation(() => undefined);
    });

    it('rolls back live windows and keeps the old value durable when canonicalization fails', async () => {
      const fs = (await import('node:fs')).default;
      let durable = JSON.stringify({ enabled: false });
      let temporary = '';
      let renames = 0;
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
      );
      (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
        temporary = contents;
      });
      (fs.renameSync as Mock).mockImplementation(() => {
        renames += 1;
        if (renames === 2) throw new Error('canonicalization failed');
        durable = temporary;
      });
      mockMainWindow.setContentProtection.mockClear();

      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, true)).resolves.toBe(
        false
      );
      expect(mockMainWindow.setContentProtection).toHaveBeenNthCalledWith(1, true);
      expect(mockMainWindow.setContentProtection).toHaveBeenLastCalledWith(false);
      expect(durable).toBe(JSON.stringify({ enabled: true, previousEnabled: false, staged: true }));
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(false);

      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : '{"enabled":true}'
      );
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      (fs.renameSync as Mock).mockImplementation(() => undefined);
    });

    it('preserves the previous enabled value when disabling cannot be canonicalized', async () => {
      const fs = (await import('node:fs')).default;
      let durable = JSON.stringify({ enabled: true });
      let temporary = '';
      let renames = 0;
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
      );
      (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
        temporary = contents;
      });
      (fs.renameSync as Mock).mockImplementation(() => {
        renames += 1;
        if (renames === 2) throw new Error('canonicalization failed');
        durable = temporary;
      });
      mockMainWindow.setContentProtection.mockClear();

      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, false)).resolves.toBe(
        false
      );
      expect(mockMainWindow.setContentProtection).toHaveBeenNthCalledWith(1, false);
      expect(mockMainWindow.setContentProtection).toHaveBeenLastCalledWith(true);
      expect(durable).toBe(JSON.stringify({ enabled: false, previousEnabled: true, staged: true }));
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(true);

      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : '{"enabled":true}'
      );
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      (fs.renameSync as Mock).mockImplementation(() => undefined);
      mockMainWindow.setContentProtection.mockImplementation(() => undefined);
    });

    it('keeps the staged destination authoritative when canonical temp write mutates then fails', async () => {
      const fs = (await import('node:fs')).default;
      let durable = JSON.stringify({ enabled: true });
      let temporary = '';
      let writes = 0;
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
      );
      (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
        temporary = contents;
        writes += 1;
        if (writes === 2) throw new Error('canonical temp write reached disk then failed');
      });
      (fs.renameSync as Mock).mockImplementation(() => {
        durable = temporary;
      });
      mockMainWindow.setContentProtection.mockClear();

      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, false)).resolves.toBe(
        false
      );
      expect(durable).toBe(JSON.stringify({ enabled: false, previousEnabled: true, staged: true }));
      await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(true);
      expect(mockMainWindow.setContentProtection).toHaveBeenNthCalledWith(1, false);
      expect(mockMainWindow.setContentProtection).toHaveBeenLastCalledWith(true);

      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json')
          ? JSON.stringify({ enabled: false })
          : '{"enabled":true}'
      );
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
      (fs.renameSync as Mock).mockImplementation(() => undefined);
      mockMainWindow.setContentProtection.mockImplementation(() => undefined);
    });

    it.each([
      { previous: false, target: true },
      { previous: true, target: false },
    ])(
      'keeps the existing authority when the staged temp write fails after mutation ($previous → $target)',
      async ({ previous, target }) => {
        const fs = (await import('node:fs')).default;
        let durable = JSON.stringify({ enabled: previous });
        let temporary = '';
        (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
          filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
        );
        (fs.writeFileSync as Mock).mockImplementation((_filePath: string, contents: string) => {
          temporary = contents;
          throw new Error('temp write reached disk then failed');
        });
        (fs.renameSync as Mock).mockClear();
        mockMainWindow.setContentProtection.mockClear();

        await expect(
          handlers.get('app:setContentProtection')!(trustedIpcEvent, target)
        ).resolves.toBe(false);
        await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(
          previous
        );
        expect(temporary).toBe(
          JSON.stringify({ enabled: target, previousEnabled: previous, staged: true })
        );
        expect(fs.renameSync).not.toHaveBeenCalled();
        expect(fs.unlinkSync).toHaveBeenCalledWith(
          expect.stringMatching(/content-protection\.json\.\d+\.tmp$/)
        );
        expect(mockMainWindow.setContentProtection).not.toHaveBeenCalled();

        (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
          filePath.includes('content-protection.json')
            ? JSON.stringify({ enabled: false })
            : '{"enabled":true}'
        );
        (fs.writeFileSync as Mock).mockImplementation(() => undefined);
        (fs.renameSync as Mock).mockImplementation(() => undefined);
      }
    );

    it.each([
      { previous: false, target: true },
      { previous: true, target: false },
    ])(
      'keeps the existing authority when the staged rename fails ($previous → $target)',
      async ({ previous, target }) => {
        const fs = (await import('node:fs')).default;
        let durable = JSON.stringify({ enabled: previous });
        (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
          filePath.includes('content-protection.json') ? durable : JSON.stringify({ enabled: true })
        );
        (fs.writeFileSync as Mock).mockImplementation(() => undefined);
        (fs.renameSync as Mock).mockImplementation(() => {
          throw new Error('rename failed');
        });
        mockMainWindow.setContentProtection.mockClear();

        await expect(
          handlers.get('app:setContentProtection')!(trustedIpcEvent, target)
        ).resolves.toBe(false);
        await expect(handlers.get('app:getContentProtection')!(trustedIpcEvent)).resolves.toBe(
          previous
        );
        expect(fs.unlinkSync).toHaveBeenCalledWith(
          expect.stringMatching(/content-protection\.json\.\d+\.tmp$/)
        );
        expect(mockMainWindow.setContentProtection).not.toHaveBeenCalled();

        (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
          filePath.includes('content-protection.json')
            ? JSON.stringify({ enabled: false })
            : '{"enabled":true}'
        );
        (fs.renameSync as Mock).mockImplementation(() => undefined);
      }
    );

    it('applies a persisted enabled value to a new PiP before loading it', async () => {
      const fs = (await import('node:fs')).default;
      (fs.readFileSync as Mock).mockImplementation((filePath: string) =>
        filePath.includes('content-protection.json') ? '{"enabled":true}' : '{"enabled":true}'
      );
      const pip = {
        isDestroyed: vi.fn(() => false),
        setContentProtection: vi.fn(),
        loadURL: vi.fn().mockResolvedValue(undefined),
        on: vi.fn(),
        focus: vi.fn(),
        destroy: vi.fn(),
      };
      (MockBrowserWindow as Mock).mockImplementationOnce(function () {
        return pip;
      });
      await handlers.get('pip:open')!(trustedIpcEvent, { id: 'content-protection-persisted-pip' });
      expect(pip.setContentProtection).toHaveBeenCalledWith(true);
      expect(pip.setContentProtection.mock.invocationCallOrder[0]).toBeLessThan(
        pip.loadURL.mock.invocationCallOrder[0]
      );
    });

    it('rejects an untrusted sender and non-boolean payload without side effects', async () => {
      const fs = (await import('node:fs')).default;
      (fs.writeFileSync as Mock).mockClear();
      mockMainWindow.setContentProtection.mockClear();
      await expect(handlers.get('app:setContentProtection')!(foreignIpcEvent, true)).resolves.toBe(
        false
      );
      await expect(
        handlers.get('app:setContentProtection')!(trustedIpcEvent, 'true')
      ).resolves.toBe(false);
      expect(fs.writeFileSync).not.toHaveBeenCalled();
      expect(mockMainWindow.setContentProtection).not.toHaveBeenCalled();
    });

    it('returns false on a preference write failure without native calls', async () => {
      const fs = (await import('node:fs')).default;
      (fs.writeFileSync as Mock).mockImplementation(() => {
        throw new Error('disk full');
      });
      mockMainWindow.setContentProtection.mockClear();
      await expect(handlers.get('app:setContentProtection')!(trustedIpcEvent, true)).resolves.toBe(
        false
      );
      expect(mockMainWindow.setContentProtection).not.toHaveBeenCalled();
      (fs.writeFileSync as Mock).mockImplementation(() => undefined);
    });

    it('app:getVersion returns version', async () => {
      const result = await handlers.get('app:getVersion')!();
      expect(result).toBe('1.0.0-test');
    });

    it('app:getPlatform returns platform', async () => {
      const result = await handlers.get('app:getPlatform')!();
      expect(result).toBe(process.platform);
    });

    it('app:getIpcContract returns contract version', async () => {
      const result = await handlers.get('app:getIpcContract')!();
      expect(result).toBe('1.0');
    });

    it('app:getSystemInfo returns system details', async () => {
      const result = (await handlers.get('app:getSystemInfo')!()) as Record<string, string>;
      expect(result).toHaveProperty('platform');
      expect(result).toHaveProperty('arch');
      expect(result).toHaveProperty('electronVersion');
      expect(result).toHaveProperty('chromiumVersion');
      expect(result).toHaveProperty('nodeVersion');
    });
  });

  describe('IPC: hardware acceleration', () => {
    it('app:getHardwareAcceleration returns current pref', async () => {
      const result = await handlers.get('app:getHardwareAcceleration')!();
      expect(result).toBe(true);
    });

    it('app:setHardwareAcceleration writes pref file', async () => {
      const fs = (await import('node:fs')).default;
      await handlers.get('app:setHardwareAcceleration')!(trustedIpcEvent, false);
      expect(fs.writeFileSync).toHaveBeenCalledWith(
        expect.stringContaining('hw-accel.json'),
        JSON.stringify({ enabled: false }),
        'utf-8'
      );
    });

    it('app:relaunch restarts the app', async () => {
      const { app } = await import('electron');
      await handlers.get('app:relaunch')!(trustedIpcEvent);
      expect(app.relaunch).toHaveBeenCalled();
      expect(app.quit).toHaveBeenCalled();
    });

    it('app:quit exits the app', async () => {
      const { app } = await import('electron');
      (app.quit as Mock).mockClear();
      await handlers.get('app:quit')!();
      expect(app.quit).toHaveBeenCalled();
    });
  });

  describe('IPC: GPU info', () => {
    it('gpu:getInfo returns vendor and device', async () => {
      const { app } = await import('electron');
      const result = (await handlers.get('gpu:getInfo')!()) as {
        vendor: string;
        device: string;
        encodeProfiles: string[];
      } | null;
      expect(result).not.toBeNull();
      expect(result!.vendor).toBe('NVIDIA');
      expect(result!.device).toBe('GeForce GTX 1080');
      expect(result!.encodeProfiles).toEqual([]);
      expect(app.getGPUInfo).toHaveBeenCalledWith('complete');
    });

    it('gpu:getInfo returns null on failure', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockRejectedValueOnce(new Error('fail'));
      const result = await handlers.get('gpu:getInfo')!();
      expect(result).toBeNull();
    });
  });

  describe('IPC: screen info', () => {
    it('screen:getDisplayInfo returns display list', async () => {
      const result = (await handlers.get('screen:getDisplayInfo')!()) as Array<{
        width: number;
        height: number;
        refreshRate: number;
        isPrimary: boolean;
      }>;
      expect(result).toHaveLength(1);
      expect(result[0].width).toBe(3840); // 1920 * 2 (scaleFactor)
      expect(result[0].height).toBe(2160);
      expect(result[0].refreshRate).toBe(120);
      expect(result[0].isPrimary).toBe(true);
    });
  });

  describe('IPC: media & clipboard', () => {
    it('media:getDesktopSources returns source list', async () => {
      const result = (await handlers.get('media:getDesktopSources')!(trustedIpcEvent)) as Array<{
        id: string;
        name: string;
        thumbnail: string;
      }>;
      expect(result).toHaveLength(1);
      expect(result[0].id).toBe('screen:1');
      expect(result[0].name).toBe('Screen 1');
      expect(result[0].thumbnail).toContain('data:image');
    });

    it('clipboard:writeText writes to clipboard', async () => {
      const { clipboard } = await import('electron');
      await handlers.get('clipboard:writeText')!(trustedIpcEvent, 'hello');
      expect(clipboard.writeText).toHaveBeenCalledWith('hello');
    });
  });

  describe('IPC: auth (tokenManager passthrough)', () => {
    it('auth:storeRefreshToken stores token and sets update feed', async () => {
      const { storeRefreshToken } = await import('../../../src/main/tokenManager');
      const { setUpdateFeedUrl } = await import('../../../src/main/updater');
      const data = { refreshToken: 'tok', rememberMe: true, apiBase: 'http://localhost:8080' };
      const result = await handlers.get('auth:storeRefreshToken')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        data
      );
      expect(result).toBe(41);
      expect(storeRefreshToken).toHaveBeenCalledWith(data);
      expect(setUpdateFeedUrl).toHaveBeenCalledWith('http://localhost:8080');
    });

    it('auth:storeRefreshToken rejects untrusted sender frames', async () => {
      const { storeRefreshToken } = await import('../../../src/main/tokenManager');
      (storeRefreshToken as Mock).mockClear();
      const result = await handlers.get('auth:storeRefreshToken')!(
        { senderFrame: { url: 'https://evil.example/' } },
        { refreshToken: 'tok', rememberMe: true, apiBase: 'http://localhost:8080' }
      );
      expect(result).toEqual({ status: 'rejected' });
      expect(storeRefreshToken).not.toHaveBeenCalled();
    });

    it('auth:storeRefreshToken rejects malformed payloads', async () => {
      const { storeRefreshToken } = await import('../../../src/main/tokenManager');
      const { setUpdateFeedUrl } = await import('../../../src/main/updater');
      (storeRefreshToken as Mock).mockClear();
      (setUpdateFeedUrl as Mock).mockClear();

      const result = await handlers.get('auth:storeRefreshToken')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        { refreshToken: 'tok', rememberMe: true, apiBase: 'javascript:alert(1)' }
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(storeRefreshToken).not.toHaveBeenCalled();
      expect(setUpdateFeedUrl).not.toHaveBeenCalled();
    });

    it('auth:storeRefreshToken rejects unvalidated non-SaaS apiBase values in packaged builds (#1872)', async () => {
      const { storeRefreshToken } = await import('../../../src/main/tokenManager');
      const { setUpdateFeedUrl } = await import('../../../src/main/updater');
      const electron = await import('electron');
      // Same object main.ts holds (destructured from the mocked module); flipping
      // it exercises the isPackaged-gated host pin in isValidApiBase.
      const app = electron.app as unknown as { isPackaged: boolean };
      (storeRefreshToken as Mock).mockClear();
      (setUpdateFeedUrl as Mock).mockClear();

      app.isPackaged = true;
      try {
        // Attacker host rejected even with a trusted frame + well-formed https URL.
        const evil = await handlers.get('auth:storeRefreshToken')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          { refreshToken: 'tok', rememberMe: true, apiBase: 'https://evil.example' }
        );
        expect(evil).toEqual({ status: 'rejected' });
        expect(storeRefreshToken).not.toHaveBeenCalled();
        expect(setUpdateFeedUrl).not.toHaveBeenCalled();

        // The single SaaS control-plane origin is accepted (trailing slash too).
        const data = {
          refreshToken: 'tok',
          rememberMe: true,
          apiBase: 'https://api.concordvoice.chat/',
        };
        await handlers.get('auth:storeRefreshToken')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          data
        );
        expect(storeRefreshToken).toHaveBeenCalledWith(data);
        expect(setUpdateFeedUrl).toHaveBeenCalledWith('https://api.concordvoice.chat/');
      } finally {
        app.isPackaged = false;
      }
    });

    it('auth:storeRefreshToken accepts a validated self-hosted apiBase in packaged builds', async () => {
      const { storeRefreshToken } = await import('../../../src/main/tokenManager');
      const { setUpdateFeedUrl } = await import('../../../src/main/updater');
      const { _resetSelfHostedProfileForTesting, commitSelfHostedApproval } =
        await import('../../../src/main/selfHostedProfile');
      const electron = await import('electron');
      const app = electron.app as unknown as { isPackaged: boolean };
      (storeRefreshToken as Mock).mockClear();
      (setUpdateFeedUrl as Mock).mockClear();
      _resetSelfHostedProfileForTesting();
      commitSelfHostedApproval('https://homelab.lan/setup', '10.0.0.5');

      app.isPackaged = true;
      try {
        const data = {
          refreshToken: 'tok',
          rememberMe: true,
          apiBase: 'https://homelab.lan',
        };
        await handlers.get('auth:storeRefreshToken')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          data
        );
        expect(storeRefreshToken).toHaveBeenCalledWith(data);
        expect(setUpdateFeedUrl).toHaveBeenCalledWith('https://homelab.lan');
      } finally {
        app.isPackaged = false;
        _resetSelfHostedProfileForTesting();
      }
    });

    it('auth:restoreSession restores and refreshes token', async () => {
      const result = (await handlers.get('auth:restoreSession')!({
        senderFrame: { url: 'app://concord/index.html' },
      })) as {
        status: string;
        accessToken?: string;
        rememberMe?: boolean;
      };
      expect(result.status).toBe('restored');
      expect(result.accessToken).toBe('mock-access');
      expect(result.rememberMe).toBe(true);
      expect(result).toMatchObject({ credentialOwner: 41, pendingE2EEUnlock: false });
    });

    it('auth:restoreSession reports owner-bound E2EE custody as pending', async () => {
      const { getCredentialCustodyState, restoreE2EEKeys } =
        await import('../../../src/main/tokenManager');
      (restoreE2EEKeys as Mock).mockReturnValueOnce(null);
      (getCredentialCustodyState as Mock)
        .mockReturnValueOnce({ credentialOwner: 41, pendingE2EEUnlock: true })
        .mockReturnValueOnce({ credentialOwner: 41, pendingE2EEUnlock: true })
        .mockReturnValueOnce({ credentialOwner: 41, pendingE2EEUnlock: true });

      const result = await handlers.get('auth:restoreSession')!({
        senderFrame: { url: 'app://concord/index.html' },
      });

      expect(result).toMatchObject({
        status: 'restored',
        accessToken: 'mock-access',
        credentialOwner: 41,
        pendingE2EEUnlock: true,
        e2eeKeys: null,
      });
    });

    it('auth:restoreSession never pairs an old refresh result with successor E2EE custody', async () => {
      const { getCredentialCustodyState, restoreE2EEKeys } =
        await import('../../../src/main/tokenManager');
      (getCredentialCustodyState as Mock)
        .mockReturnValueOnce({ credentialOwner: 41, pendingE2EEUnlock: true })
        .mockReturnValueOnce({ credentialOwner: 42, pendingE2EEUnlock: false });
      (restoreE2EEKeys as Mock).mockClear();

      const result = await handlers.get('auth:restoreSession')!({
        senderFrame: { url: 'app://concord/index.html' },
      });

      expect(result).toEqual({ status: 'refresh_failed' });
      expect(restoreE2EEKeys).not.toHaveBeenCalled();
    });

    it('auth:restoreSession returns rememberMe=false for memory-only sessions', async () => {
      const { restoreRefreshToken } = await import('../../../src/main/tokenManager');
      (restoreRefreshToken as Mock).mockReturnValueOnce({
        status: 'ok',
        token: 'memory-token',
        apiBase: 'http://localhost:8080',
        rememberMe: false,
      });

      const result = (await handlers.get('auth:restoreSession')!({
        senderFrame: { url: 'app://concord/index.html' },
      })) as { status: string; rememberMe?: boolean };

      expect(result.status).toBe('restored');
      expect(result.rememberMe).toBe(false);
    });

    it('auth:restoreSession clears single-flight cache after the restore settles', async () => {
      const { restoreRefreshToken, performRefresh } =
        await import('../../../src/main/tokenManager');
      (restoreRefreshToken as Mock).mockClear();
      (performRefresh as Mock).mockClear();

      const event = { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } };
      await handlers.get('auth:restoreSession')!(event);
      await handlers.get('auth:restoreSession')!(event);

      expect(restoreRefreshToken).toHaveBeenCalledTimes(2);
      expect(performRefresh).toHaveBeenCalledTimes(2);
    });

    it('auth:restoreSession rejects untrusted sender frames', async () => {
      const { restoreRefreshToken } = await import('../../../src/main/tokenManager');
      (restoreRefreshToken as Mock).mockClear();
      const result = await handlers.get('auth:restoreSession')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({ status: 'rejected' });
      expect(restoreRefreshToken).not.toHaveBeenCalled();
    });

    it('auth:storeE2EEKeys stores keys', async () => {
      const { storeE2EEKeys } = await import('../../../src/main/tokenManager');
      const data = {
        wrappingKeyBase64: 'a',
        preferencesKeyBase64: 'b',
        wrappedPrivateKeyBase64: 'c',
      };
      await handlers.get('auth:storeE2EEKeys')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        data
      );
      expect(storeE2EEKeys).toHaveBeenCalledWith(data);
    });

    it('auth:storeE2EEKeys rejects untrusted sender frames', async () => {
      const { storeE2EEKeys } = await import('../../../src/main/tokenManager');
      (storeE2EEKeys as Mock).mockClear();
      const result = await handlers.get('auth:storeE2EEKeys')!(
        { senderFrame: { url: 'https://evil.example/' } },
        { wrappingKeyBase64: 'a', preferencesKeyBase64: 'b', wrappedPrivateKeyBase64: 'c' }
      );
      expect(result).toEqual({ status: 'rejected' });
      expect(storeE2EEKeys).not.toHaveBeenCalled();
    });

    it('auth:storeE2EEKeys rejects malformed payloads', async () => {
      const { storeE2EEKeys } = await import('../../../src/main/tokenManager');
      (storeE2EEKeys as Mock).mockClear();

      const result = await handlers.get('auth:storeE2EEKeys')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        { wrappingKeyBase64: 'a', preferencesKeyBase64: 42, wrappedPrivateKeyBase64: 'c' }
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(storeE2EEKeys).not.toHaveBeenCalled();
    });

    it('auth:storeE2EEKeysIfOwner delegates an owner-scoped key write', async () => {
      const { storeE2EEKeysIfOwner } = await import('../../../src/main/tokenManager');
      const data = {
        wrappingKeyBase64: 'a',
        preferencesKeyBase64: 'b',
        wrappedPrivateKeyBase64: 'c',
      };

      const result = await handlers.get('auth:storeE2EEKeysIfOwner')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        data,
        41
      );

      expect(result).toBe(true);
      expect(storeE2EEKeysIfOwner).toHaveBeenCalledWith(data, 41);
    });

    it('auth:storeE2EEKeysIfOwner rejects invalid owners', async () => {
      const { storeE2EEKeysIfOwner } = await import('../../../src/main/tokenManager');
      (storeE2EEKeysIfOwner as Mock).mockClear();

      const result = await handlers.get('auth:storeE2EEKeysIfOwner')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        {
          wrappingKeyBase64: 'a',
          preferencesKeyBase64: 'b',
          wrappedPrivateKeyBase64: 'c',
        },
        0
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
    });

    it('auth:storeE2EEKeysIfOwner rejects untrusted sender frames', async () => {
      const { storeE2EEKeysIfOwner } = await import('../../../src/main/tokenManager');
      (storeE2EEKeysIfOwner as Mock).mockClear();

      const result = await handlers.get('auth:storeE2EEKeysIfOwner')!(
        { senderFrame: { url: 'https://evil.example/' } },
        {
          wrappingKeyBase64: 'a',
          preferencesKeyBase64: 'b',
          wrappedPrivateKeyBase64: 'c',
        },
        41
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(storeE2EEKeysIfOwner).not.toHaveBeenCalled();
    });

    it('auth:refreshToken delegates to performRefresh', async () => {
      const result = (await handlers.get('auth:refreshToken')!({
        senderFrame: { url: 'app://concord/index.html' },
      })) as { status: string };
      expect(result.status).toBe('ok');
    });

    it('auth:refreshToken rejects untrusted sender frames', async () => {
      const { performRefresh } = await import('../../../src/main/tokenManager');
      (performRefresh as Mock).mockClear();
      const result = await handlers.get('auth:refreshToken')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({ status: 'rejected' });
      expect(performRefresh).not.toHaveBeenCalled();
    });

    it('auth:logout delegates to performLogout', async () => {
      const { performLogout } = await import('../../../src/main/tokenManager');
      await handlers.get('auth:logout')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        { accessToken: 'tok' }
      );
      expect(performLogout).toHaveBeenCalledWith('tok');
    });

    it('auth:logout rejects untrusted sender frames', async () => {
      const { performLogout } = await import('../../../src/main/tokenManager');
      (performLogout as Mock).mockClear();
      const result = await handlers.get('auth:logout')!(
        { senderFrame: { url: 'https://evil.example/' } },
        { accessToken: 'tok' }
      );
      expect(result).toEqual({ status: 'rejected' });
      expect(performLogout).not.toHaveBeenCalled();
    });

    it('auth:logout rejects malformed payloads', async () => {
      const { performLogout } = await import('../../../src/main/tokenManager');
      (performLogout as Mock).mockClear();
      const result = await handlers.get('auth:logout')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        { accessToken: 42 }
      );
      expect(result).toEqual({ status: 'rejected' });
      expect(performLogout).not.toHaveBeenCalled();
    });

    it('auth:clearTokens delegates to clearTokens', async () => {
      const { clearTokens } = await import('../../../src/main/tokenManager');
      await handlers.get('auth:clearTokens')!({
        senderFrame: { url: 'app://concord/index.html' },
      });
      expect(clearTokens).toHaveBeenCalled();
    });

    it('auth:clearTokens rejects untrusted sender frames', async () => {
      const { clearTokens } = await import('../../../src/main/tokenManager');
      (clearTokens as Mock).mockClear();
      const result = await handlers.get('auth:clearTokens')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({ status: 'rejected' });
      expect(clearTokens).not.toHaveBeenCalled();
    });

    it('auth:clearTokensIfOwner delegates conditional credential clearing', async () => {
      const { clearTokensIfOwner } = await import('../../../src/main/tokenManager');

      const cleared = await handlers.get('auth:clearTokensIfOwner')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        41
      );
      const preserved = await handlers.get('auth:clearTokensIfOwner')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        40
      );

      expect(cleared).toBe(true);
      expect(preserved).toBe(false);
      expect(clearTokensIfOwner).toHaveBeenNthCalledWith(1, 41);
      expect(clearTokensIfOwner).toHaveBeenNthCalledWith(2, 40);
    });

    it('auth:clearTokensIfOwner rejects invalid owners', async () => {
      const { clearTokensIfOwner } = await import('../../../src/main/tokenManager');
      (clearTokensIfOwner as Mock).mockClear();

      const result = await handlers.get('auth:clearTokensIfOwner')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        Number.NaN
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(clearTokensIfOwner).not.toHaveBeenCalled();
    });

    it('auth:clearTokensIfOwner rejects untrusted sender frames', async () => {
      const { clearTokensIfOwner } = await import('../../../src/main/tokenManager');
      (clearTokensIfOwner as Mock).mockClear();

      const result = await handlers.get('auth:clearTokensIfOwner')!(
        { senderFrame: { url: 'https://evil.example/' } },
        41
      );

      expect(result).toEqual({ status: 'rejected' });
      expect(clearTokensIfOwner).not.toHaveBeenCalled();
    });

    it('auth:getCapabilities returns capabilities for a trusted sender', async () => {
      const result = (await handlers.get('auth:getCapabilities')!({
        senderFrame: { url: 'app://concord/index.html' },
      })) as {
        safeStorage: boolean;
      };
      expect(result.safeStorage).toBe(true);
    });

    it('auth:getCapabilities rejects untrusted sender frames (fails closed)', async () => {
      const result = await handlers.get('auth:getCapabilities')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({ persistAvailable: false });
    });

    it('auth:getMachineId returns machine id', async () => {
      const { getMachineId } = await import('../../../src/main/machineId');
      const result = await handlers.get('auth:getMachineId')!({
        senderFrame: { url: 'app://concord/index.html' },
      });
      expect(result).toBe('mock-machine-id');
      expect(getMachineId).toHaveBeenCalledWith();
    });

    it('auth:getMachineId returns the machine id for a validated self-hosted apiBase', async () => {
      const { getMachineId } = await import('../../../src/main/machineId');
      const { commitSelfHostedApproval } = await import('../../../src/main/selfHostedProfile');
      (getMachineId as Mock).mockClear();
      commitSelfHostedApproval('https://homelab.lan', '10.0.0.5');

      const result = await handlers.get('auth:getMachineId')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://homelab.lan'
      );

      expect(result).toBe('mock-machine-id');
      expect(getMachineId).toHaveBeenCalledWith('https://homelab.lan');
    });

    it('auth:getMachineId rejects untrusted sender frames', async () => {
      const result = await handlers.get('auth:getMachineId')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toBe('');
    });

    it('selfHosted:probeServer delegates for trusted sender frames', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      (probeSelfHostedServer as Mock).mockResolvedValueOnce({
        status: 'ok',
        apiBase: 'https://homelab.lan',
        clientConfig: {},
        capabilities: {},
      });

      const result = await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://homelab.lan'
      );

      expect(result).toEqual({
        status: 'ok',
        apiBase: 'https://homelab.lan',
        clientConfig: {},
        capabilities: {},
      });
      expect(probeSelfHostedServer).toHaveBeenCalledWith('https://homelab.lan');
    });

    it('selfHosted:probeServer rejects an event carrying no sender id (Gitar, #2668)', async () => {
      // The single-flight key was `event.sender?.id ?? -1`, which collapsed every
      // sender-less event onto ONE bucket — unrelated frames would then refuse each
      // other's probes as 'busy'. Fail closed instead, and with 'rejected' rather than
      // 'busy': 'busy' advertises a transient state the caller should retry, and this
      // is not transient. Electron always populates `sender` for ipcMain.handle today;
      // this guards the refactor or harness where it does not.
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      (probeSelfHostedServer as Mock).mockClear();
      const result = await handlers.get('selfHosted:probeServer')!(
        { senderFrame: { url: 'app://concord/index.html' } },
        'https://real.lan'
      );
      expect(result).toEqual({
        status: 'error',
        code: 'rejected',
        message: 'Self-hosted server probing is not available from this frame.',
      });
      // Refused before any side effect: no probe, no ceremony.
      expect(probeSelfHostedServer).not.toHaveBeenCalled();
    });

    it('selfHosted:probeServer rejects untrusted sender frames', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      (probeSelfHostedServer as Mock).mockClear();

      const result = await handlers.get('selfHosted:probeServer')!(
        { senderFrame: { url: 'https://evil.example/' } },
        'https://homelab.lan'
      );

      expect(result).toEqual({
        status: 'error',
        code: 'rejected',
        message: 'Self-hosted server probing is not available from this frame.',
      });
      expect(probeSelfHostedServer).not.toHaveBeenCalled();
    });
  });

  describe('approval dialog copy (#2354)', () => {
    // The §8.4 forbidden-vocabulary list is scoped to RENDERER-visible strings. The
    // `resolve` stem is deliberately NOT asserted here: §7.2 mandates this native
    // dialog's copy verbatim, and that copy carries the `Resolves to:` label.
    const FORBIDDEN = [
      'DNS',
      'lookup',
      'denylist',
      'blocklist',
      'tier',
      'CIDR',
      'RFC1918',
      'private network',
      'loopback',
      'origin',
      'IPC',
      'SSRF',
      'socket',
      'agent',
      'redirect hop',
    ];

    it('renders the loopback variant with identical weight and no softening', async () => {
      const { buildApprovalDialogCopy } = await import('../../../src/main/main');
      const { classifyAddress } = await import('../../../src/main/egressPolicy');
      const { message, detail } = buildApprovalDialogCopy({
        host: 'concord.lan',
        address: '127.0.0.1',
        decision: classifyAddress('127.0.0.1'),
      });
      expect(message).toContain('Trust concord.lan?');
      expect(detail).toContain('Host:         concord.lan');
      expect(detail).toContain('127.0.0.1, on this device');
      expect(detail).toContain('store your sign-in on this device');
      expect(detail).toContain('Concord Voice will remember this choice on this device.');
      expect(detail).not.toContain('You will not be asked again');
    });

    it('renders the LAN / CGNAT / public suffixes', async () => {
      const { buildApprovalDialogCopy } = await import('../../../src/main/main');
      const { classifyAddress } = await import('../../../src/main/egressPolicy');
      expect(
        buildApprovalDialogCopy({
          host: 'a',
          address: '10.0.0.5',
          decision: classifyAddress('10.0.0.5'),
        }).detail
      ).toContain('10.0.0.5, on your network');
      expect(
        buildApprovalDialogCopy({
          host: 'a',
          address: '100.64.0.1',
          decision: classifyAddress('100.64.0.1'),
        }).detail
      ).toContain("100.64.0.1, on your provider's network");
      expect(
        buildApprovalDialogCopy({
          host: 'a',
          address: '93.184.216.34',
          decision: classifyAddress('93.184.216.34'),
        }).detail
      ).toContain('93.184.216.34, on the internet');
    });

    it('uses no forbidden mechanism vocabulary and never decodes punycode', async () => {
      const { buildApprovalDialogCopy } = await import('../../../src/main/main');
      const { classifyAddress } = await import('../../../src/main/egressPolicy');
      const { message, detail } = buildApprovalDialogCopy({
        host: 'xn--bcher-kva.example',
        address: '203.0.113.4',
        decision: classifyAddress('203.0.113.4'),
      });
      const text = `${message}\n${detail}`.toLowerCase();
      for (const word of FORBIDDEN) expect(text).not.toContain(word.toLowerCase());
      expect(detail).toContain('xn--bcher-kva.example'); // punycode preserved verbatim
    });
  });

  // Every assertion here locks #14a — "main introduces no trigger of its own" — and only
  // that. None of them measures renderer-side anchoring, which ADR-0035 § "Non-property
  // #14b" records as a property this design does not have.
  describe('#14a — main introduces no ceremony trigger of its own (#2354)', () => {
    it('shows no dialog at module import or main init (no startup/revalidation trigger)', () => {
      // Captured immediately after the import in beforeAll, so this cannot be
      // satisfied or broken by test-execution order.
      expect(dialogCallsAfterImport).toBe(0);
    });

    it('refuses a tier-1 address with no dialog and no probe', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      (probeSelfHostedServer as Mock).mockClear();
      mockShowMessageBox.mockClear();
      mockResolveForDisplay.mockResolvedValueOnce({
        ok: false,
        kind: 'tier1',
        reason: 'metadata_link_local',
      } as never);

      const result = await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://metadata.invalid'
      );

      expect(result).toEqual({
        status: 'error',
        code: 'address_not_allowed',
        message: 'metadata_link_local',
      });
      expect(mockShowMessageBox).not.toHaveBeenCalled();
      expect(probeSelfHostedServer).not.toHaveBeenCalled();
    });

    it('declining the ceremony mints nothing and never probes', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting, isValidatedSelfHostedApiBase } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      (probeSelfHostedServer as Mock).mockClear();
      mockShowMessageBox.mockClear();
      mockShowMessageBox.mockResolvedValueOnce({ response: 0 });

      const result = await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://declined.lan'
      );

      expect(result).toEqual({
        status: 'error',
        code: 'approval_declined',
        message: 'Connection cancelled.',
      });
      expect(mockShowMessageBox).toHaveBeenCalledTimes(1);
      expect(probeSelfHostedServer).not.toHaveBeenCalled();
      expect(isValidatedSelfHostedApiBase('https://declined.lan')).toBe(false);
      expect(approvalStore.records).toHaveLength(0);
    });

    // Consent alone used to mint the durable grant, so EVERY post-consent failure — a
    // non-Concord server, TLS, ECONNREFUSED, HTTP 500, an oversized body — left the origin
    // permanently in the approved set and the approvals file, gating auth:storeRefreshToken
    // and the SSO exchange with no in-app revocation. A compromised renderer only needed the
    // user to approve some origin once; it never had to make that origin behave like Concord.
    it('a probe that fails after consent mints nothing durable (#2354 review item 3)', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting, isValidatedSelfHostedApiBase } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockClear();
      const failure = {
        status: 'error',
        code: 'client_config_failed',
        message: 'The server did not respond like a Concord server.',
      };
      (probeSelfHostedServer as Mock).mockResolvedValueOnce(failure);

      const result = await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://not-concord.lan'
      );

      // The user consented and the probe ran on the provisional grant …
      expect(mockShowMessageBox).toHaveBeenCalledTimes(1);
      expect(probeSelfHostedServer).toHaveBeenCalledTimes(1);
      expect(result).toEqual(failure);
      // … but nothing durable was minted, so the origin holds no credential or SSO trust.
      expect(isValidatedSelfHostedApiBase('https://not-concord.lan')).toBe(false);
      expect(approvalStore.records).toHaveLength(0);
    });

    it('a successful probe still mints, and still reports a failed durable write', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting, isValidatedSelfHostedApiBase } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockClear();
      const ok = { status: 'ok', apiBase: 'https://real.lan' };
      (probeSelfHostedServer as Mock).mockResolvedValue(ok);

      expect(
        await handlers.get('selfHosted:probeServer')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          'https://real.lan'
        )
      ).toEqual(ok);
      expect(isValidatedSelfHostedApiBase('https://real.lan')).toBe(true);
      expect(approvalStore.records).toHaveLength(1);

      // approval_not_saved must survive the reorder: a durable-write failure on an
      // otherwise-successful probe is still surfaced, not silently swallowed.
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      approvalStore.failAppend = true;
      try {
        expect(
          await handlers.get('selfHosted:probeServer')!(
            { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
            'https://unwritable.lan'
          )
        ).toEqual({
          status: 'error',
          code: 'approval_not_saved',
          message: "Concord couldn't save your choice.",
        });
      } finally {
        approvalStore.failAppend = false;
      }
      expect(isValidatedSelfHostedApiBase('https://unwritable.lan')).toBe(false);
    });

    it('approving mints once, then a second probe of the same origin does not re-prompt', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting, isValidatedSelfHostedApiBase } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockClear();
      (probeSelfHostedServer as Mock).mockResolvedValue({ status: 'ok' });

      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://approved.lan'
      );
      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://approved.lan'
      );

      expect(mockShowMessageBox).toHaveBeenCalledTimes(1); // rarity: once per origin
      expect(isValidatedSelfHostedApiBase('https://approved.lan')).toBe(true);
      expect(approvalStore.records).toHaveLength(1);
      expect(probeSelfHostedServer).toHaveBeenCalledTimes(2);
    });

    it('re-runs the ceremony when a public-approved origin now resolves into tier 2', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockClear();
      (probeSelfHostedServer as Mock).mockResolvedValue({ status: 'ok' });

      // First connect: the ceremony displays a public address; consent is 'public'.
      mockResolveForDisplay.mockResolvedValueOnce({
        ok: true,
        address: '203.0.113.10',
        addresses: ['203.0.113.10'],
        decision: { tier: 'public' },
      } as never);
      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://moved.lan'
      );
      expect(approvalStore.records).toEqual([
        expect.objectContaining({ tierAtApproval: 'public' }),
      ]);

      // Second connect: the same name now resolves onto the LAN. Consent taken against
      // a public address never authorized that, so the user is asked again — showing
      // the private address this time — rather than the dial being silently permitted.
      mockResolveForDisplay.mockResolvedValueOnce({
        ok: true,
        address: '10.0.0.5',
        addresses: ['10.0.0.5'],
        decision: { tier: 'tier2', reason: 'private' },
      } as never);
      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://moved.lan'
      );

      expect(mockShowMessageBox).toHaveBeenCalledTimes(2);
      expect(approvalStore.records).toHaveLength(2);
      expect(approvalStore.records[1]).toEqual(
        expect.objectContaining({ tierAtApproval: 'tier2' })
      );
    });

    it('does not re-prompt a public-approved origin that still resolves public', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockClear();
      (probeSelfHostedServer as Mock).mockResolvedValue({ status: 'ok' });

      const publicResolution = {
        ok: true,
        address: '203.0.113.10',
        addresses: ['203.0.113.10'],
        decision: { tier: 'public' },
      } as never;
      mockResolveForDisplay.mockResolvedValueOnce(publicResolution);
      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://steady.lan'
      );
      mockResolveForDisplay.mockResolvedValueOnce(publicResolution);
      await handlers.get('selfHosted:probeServer')!(
        { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
        'https://steady.lan'
      );

      // Rarity invariant: an approved origin produces no prompt on repeat connect.
      expect(mockShowMessageBox).toHaveBeenCalledTimes(1);
      expect(approvalStore.records).toHaveLength(1);
      expect(probeSelfHostedServer).toHaveBeenCalledTimes(2);
    });

    // Assertion 4 of #14a. This locks that the dialog answers THIS SPECIFIC INVOCATION —
    // an invocation, not a user action. It does not, and cannot, show a human initiated
    // it: see ADR-0035 § "Non-property #14b".
    it('resolves an approval from the dialog that same invocation asked to show', async () => {
      const { requestSelfHostedApproval } = await import('../../../src/main/main');
      const showMessageBox = vi.fn().mockResolvedValue({ response: 1 });
      const approved = await requestSelfHostedApproval(
        {},
        {
          host: 'concord.lan',
          address: '10.0.0.5',
          decision: { tier: 'tier2', reason: 'private' },
        },
        { showMessageBox }
      );
      expect(showMessageBox).toHaveBeenCalledTimes(1);
      expect(approved).toBe(true);
    });

    it('treats Cancel (response 0) and dismiss as decline', async () => {
      const { requestSelfHostedApproval } = await import('../../../src/main/main');
      const showMessageBox = vi.fn().mockResolvedValue({ response: 0 });
      const approved = await requestSelfHostedApproval(
        {},
        {
          host: 'concord.lan',
          address: '127.0.0.1',
          decision: { tier: 'tier2', reason: 'loopback' },
        },
        { showMessageBox }
      );
      expect(approved).toBe(false);
    });

    it('passes a destructive-safe button configuration', async () => {
      const { requestSelfHostedApproval } = await import('../../../src/main/main');
      const showMessageBox = vi.fn().mockResolvedValue({ response: 0 });
      await requestSelfHostedApproval(
        {},
        {
          host: 'concord.lan',
          address: '10.0.0.5',
          decision: { tier: 'tier2', reason: 'private' },
        },
        { showMessageBox }
      );
      const options = showMessageBox.mock.calls[0][1] as Record<string, unknown>;
      expect(options.type).toBe('warning');
      expect(options.buttons).toEqual(['Cancel', 'Trust This Server']);
      expect(options.defaultId).toBe(0);
      expect(options.cancelId).toBe(0);
      expect(options.noLink).toBe(true);
      expect(options.icon).toBeUndefined();
      // A decision, not an omission — ADR-0035 § "Non-property #14b". MessageBoxOptions
      // cannot gate the affirmative on checkbox state, so an unchecked accept becomes a
      // silent no-op that a native dialog cannot explain or validate inline.
      expect(options.checkboxLabel).toBeUndefined();
    });
  });

  describe('ceremony budget (#2354)', () => {
    // The bucket rations DIALOGS, not probes: an approved origin re-probes freely.
    it('refuses a ceremony past the budget while an approved origin still probes', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      const { CEREMONY_BUDGET } = await import('../../../src/main/selfHostedCeremonyBudget');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockReset();
      (probeSelfHostedServer as Mock).mockResolvedValue({ status: 'ok' });

      // Each distinct origin is a fresh ceremony.
      for (let i = 0; i < CEREMONY_BUDGET; i++) {
        await handlers.get('selfHosted:probeServer')!(trustedIpcEvent, `https://burst${i}.lan`);
      }
      expect(mockShowMessageBox).toHaveBeenCalledTimes(CEREMONY_BUDGET);

      const refused = await handlers.get('selfHosted:probeServer')!(
        trustedIpcEvent,
        'https://burst-over.lan'
      );
      expect(refused).toEqual({ status: 'error', code: 'too_many_prompts', message: '' });
      expect(mockShowMessageBox).toHaveBeenCalledTimes(CEREMONY_BUDGET);

      // An origin approved before the bucket drained is unaffected — no dialog, so
      // no token, so the throttle must not touch it.
      const approved = await handlers.get('selfHosted:probeServer')!(
        trustedIpcEvent,
        'https://burst0.lan'
      );
      expect(approved).toEqual({ status: 'ok' });
      expect(mockShowMessageBox).toHaveBeenCalledTimes(CEREMONY_BUDGET);
    });
  });

  describe('probe single-flight (#2354)', () => {
    // resolveForDisplay's dns.lookup is a libuv-threadpool job that cannot be
    // cancelled, so concurrency here is a resource-exhaustion lever, not a
    // convenience issue. The gate must therefore cover the WHOLE handler.
    it('refuses a concurrent probe from the same sender while letting another sender through', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockReset();

      let release: () => void = () => {};
      const gate = new Promise<void>((resolve) => {
        release = resolve;
      });
      (probeSelfHostedServer as Mock).mockImplementation(async () => {
        await gate;
        return { status: 'ok' };
      });

      const senderA = { senderFrame: { url: 'app://concord/index.html' }, sender: { id: 7 } };
      const senderB = { senderFrame: { url: 'app://concord/index.html' }, sender: { id: 8 } };

      const first = handlers.get('selfHosted:probeServer')!(senderA, 'https://inflight.lan');
      const refused = await handlers.get('selfHosted:probeServer')!(senderA, 'https://other.lan');
      const third = handlers.get('selfHosted:probeServer')!(senderB, 'https://third.lan');

      expect(refused).toEqual({ status: 'error', code: 'busy', message: '' });

      release();
      await expect(first).resolves.toEqual({ status: 'ok' });
      await expect(third).resolves.toEqual({ status: 'ok' });
      // The refused call must not have dialled or prompted for its own origin.
      expect(probeSelfHostedServer).toHaveBeenCalledTimes(2);
    });

    it('releases the slot after a probe settles, including on rejection', async () => {
      const { probeSelfHostedServer } = await import('../../../src/main/selfHostedProbe');
      const { _resetSelfHostedProfileForTesting } =
        await import('../../../src/main/selfHostedProfile');
      _resetSelfHostedProfileForTesting();
      approvalStore.records.length = 0;
      mockShowMessageBox.mockClear();
      (probeSelfHostedServer as Mock).mockReset();
      (probeSelfHostedServer as Mock).mockRejectedValueOnce(new Error('boom'));

      const sender = { senderFrame: { url: 'app://concord/index.html' }, sender: { id: 9 } };
      await expect(
        handlers.get('selfHosted:probeServer')!(sender, 'https://flaky.lan')
      ).rejects.toThrow('boom');

      (probeSelfHostedServer as Mock).mockResolvedValueOnce({ status: 'ok' });
      await expect(
        handlers.get('selfHosted:probeServer')!(sender, 'https://flaky.lan')
      ).resolves.toEqual({ status: 'ok' });
    });
  });

  describe('IPC: auto-update', () => {
    it('update:check delegates to checkForUpdates', async () => {
      const result = (await handlers.get('update:check')!(trustedIpcEvent)) as {
        updateAvailable: boolean;
      };
      expect(result.updateAvailable).toBe(false);
    });

    it('update:download delegates to downloadUpdate', async () => {
      await handlers.get('update:download')!(trustedIpcEvent);
      const { downloadUpdate } = await import('../../../src/main/updater');
      expect(downloadUpdate).toHaveBeenCalled();
    });

    it('update:install delegates to safeQuitAndInstall', async () => {
      await handlers.get('update:install')!(trustedIpcEvent);
      const { safeQuitAndInstall } = await import('../../../src/main/updater');
      expect(safeQuitAndInstall).toHaveBeenCalled();
    });

    it('update:getAllowPrerelease returns current setting', async () => {
      const result = await handlers.get('update:getAllowPrerelease')!();
      expect(result).toBe(false);
    });

    it('update:setAllowPrerelease updates setting', async () => {
      await handlers.get('update:setAllowPrerelease')!(trustedIpcEvent, true);
      const { setAllowPrerelease } = await import('../../../src/main/updater');
      expect(setAllowPrerelease).toHaveBeenCalledWith(true);
    });

    it.each([foreignIpcEvent, { senderFrame: null }])(
      'rejects an untrusted or missing sender before privileged work',
      async (event) => {
        const electron = await import('electron');
        const fs = (await import('node:fs')).default;
        const updater = await import('../../../src/main/updater');
        const sideEffects = [
          electron.desktopCapturer.getSources,
          electron.clipboard.writeText,
          fs.writeFileSync,
          electron.app.relaunch,
          electron.app.quit,
          updater.checkForUpdates,
          updater.downloadUpdate,
          updater.safeQuitAndInstall,
          updater.setAllowPrerelease,
          updater.forceCheckForUpdates,
          mockWebContents.openDevTools,
          mockWebContents.closeDevTools,
        ] as Mock[];
        sideEffects.forEach((effect) => effect.mockClear());

        await handlers.get('media:getDesktopSources')!(event);
        await handlers.get('clipboard:writeText')!(event, 'secret');
        await handlers.get('app:setHardwareAcceleration')!(event, false);
        await handlers.get('app:relaunch')!(event);
        await handlers.get('update:check')!(event);
        await handlers.get('update:download')!(event);
        await handlers.get('update:install')!(event);
        await handlers.get('update:setAllowPrerelease')!(event, true);
        await handlers.get('updater:force-check')!(event, 'user_triggered');
        await handlers.get('app:setDeveloperMode')!(event, true);

        sideEffects.forEach((effect) => expect(effect).not.toHaveBeenCalled());
      }
    );
  });

  describe('IPC: PiP window management', () => {
    // pip:open requires a valid senderFrame URL post-#815 (isValidPipOpenSender
    // defense-in-depth). In this test environment isPackaged=false, so a
    // localhost URL satisfies the validator.
    const pipOwnerEvent = {
      senderFrame: { url: 'http://localhost:3001/', frameTreeNodeId: 41 },
    };
    const otherPermittedPipEvent = {
      senderFrame: { url: 'http://localhost:3001/', frameTreeNodeId: 99 },
    };

    async function openPip(id: string, event = pipOwnerEvent): Promise<() => void> {
      const firstNewCall = mockMainWindow.on.mock.calls.length;
      await handlers.get('pip:open')!(event, { id });
      const closed = mockMainWindow.on.mock.calls
        .slice(firstNewCall)
        .find((call) => call[0] === 'closed')?.[1] as (() => void) | undefined;
      expect(closed).toBeDefined();
      return closed!;
    }

    it('lets the opener focus, update, and close its PiP window', async () => {
      const { BrowserWindow } = await import('electron');
      const before = (BrowserWindow as unknown as Mock).mock.calls.length;
      const closed = await openPip('owner-controls-pip');
      expect((BrowserWindow as unknown as Mock).mock.calls.length).toBe(before + 1);

      mockMainWindow.focus.mockClear();
      mockMainWindow.setAlwaysOnTop.mockClear();
      mockMainWindow.close.mockClear();
      await handlers.get('pip:open')!(pipOwnerEvent, { id: 'owner-controls-pip' });
      expect((BrowserWindow as unknown as Mock).mock.calls.length).toBe(before + 1);
      await handlers.get('pip:setAlwaysOnTop')!(pipOwnerEvent, {
        id: 'owner-controls-pip',
        flag: false,
      });
      await handlers.get('pip:close')!(pipOwnerEvent, { id: 'owner-controls-pip' });
      expect(mockMainWindow.focus).toHaveBeenCalled();
      expect(mockMainWindow.setAlwaysOnTop).toHaveBeenCalledWith(false);
      expect(mockMainWindow.close).toHaveBeenCalled();
      closed();
    });

    it('lets the PiP window main frame update and close itself', async () => {
      const closed = await openPip('child-controls-pip');
      const pipMainFrameEvent = {
        sender: mockWebContents,
        senderFrame: mockWebContents.mainFrame,
      };
      mockMainWindow.setAlwaysOnTop.mockClear();
      mockMainWindow.close.mockClear();

      await handlers.get('pip:setAlwaysOnTop')!(pipMainFrameEvent, {
        id: 'child-controls-pip',
        flag: false,
      });
      await handlers.get('pip:close')!(pipMainFrameEvent, { id: 'child-controls-pip' });

      expect(mockMainWindow.setAlwaysOnTop).toHaveBeenCalledWith(false);
      expect(mockMainWindow.close).toHaveBeenCalled();
      closed();
    });

    it.each([foreignIpcEvent, { senderFrame: null }])(
      'rejects an untrusted or missing sender for every PiP mutation',
      async (event) => {
        // Use call-count delta (not mockClear) so the BrowserWindow mock's
        // history remains intact for downstream tests (e.g. createWindow's
        // "called during module init" assertion).
        const { BrowserWindow } = await import('electron');
        const before = (BrowserWindow as unknown as Mock).mock.calls.length;
        mockMainWindow.close.mockClear();
        mockMainWindow.setAlwaysOnTop.mockClear();
        await handlers.get('pip:open')!(event, { id: 'rejected-pip' });
        await handlers.get('pip:close')!(event, { id: 'rejected-pip' });
        await handlers.get('pip:setAlwaysOnTop')!(event, { id: 'rejected-pip', flag: false });
        const after = (BrowserWindow as unknown as Mock).mock.calls.length;
        expect(after).toBe(before);
        expect(mockMainWindow.close).not.toHaveBeenCalled();
        expect(mockMainWindow.setAlwaysOnTop).not.toHaveBeenCalled();
      }
    );

    it('rejects another permitted frame with a different frameTreeNodeId', async () => {
      const { BrowserWindow } = await import('electron');
      const before = (BrowserWindow as unknown as Mock).mock.calls.length;
      const closed = await openPip('frame-owned-pip');
      expect((BrowserWindow as unknown as Mock).mock.calls.length).toBe(before + 1);
      mockMainWindow.focus.mockClear();
      mockMainWindow.close.mockClear();
      mockMainWindow.setAlwaysOnTop.mockClear();

      await handlers.get('pip:open')!(otherPermittedPipEvent, { id: 'frame-owned-pip' });
      expect((BrowserWindow as unknown as Mock).mock.calls.length).toBe(before + 1);
      await handlers.get('pip:close')!(otherPermittedPipEvent, { id: 'frame-owned-pip' });
      await handlers.get('pip:setAlwaysOnTop')!(otherPermittedPipEvent, {
        id: 'frame-owned-pip',
        flag: false,
      });

      expect(mockMainWindow.focus).not.toHaveBeenCalled();
      expect(mockMainWindow.close).not.toHaveBeenCalled();
      expect(mockMainWindow.setAlwaysOnTop).not.toHaveBeenCalled();
      closed();
    });

    it.each([
      {
        name: 'remote to bundled',
        initialRemoteSpaUrl: 'https://spa.example.test/spa/a/index.html',
        nextRemoteSpaUrl: null,
        senderFrameUrl: 'https://spa.example.test/spa/a/index.html#/pip/transition-pip',
      },
      {
        name: 'bundled to remote',
        initialRemoteSpaUrl: null,
        nextRemoteSpaUrl: 'https://spa.example.test/spa/b/index.html',
        senderFrameUrl: 'app://concord/index.html#/pip/transition-pip',
      },
    ])('keeps PiP self controls working across a $name SPA transition', async (transition) => {
      const { app } = await import('electron');
      const { setRemoteSpaState } = await import('../../../src/main/spaState');
      const originalMainFrame = mockWebContents.mainFrame;
      const testApp = app as unknown as { isPackaged: boolean };
      testApp.isPackaged = true;
      setRemoteSpaState(transition.initialRemoteSpaUrl);
      mockWebContents.mainFrame = {
        url: transition.senderFrameUrl,
        frameTreeNodeId: 77,
      };

      try {
        const closed = await openPip('transition-pip', {
          senderFrame: {
            url: transition.senderFrameUrl,
            frameTreeNodeId: 41,
          },
        });
        const pipMainFrameEvent = {
          sender: mockWebContents,
          senderFrame: mockWebContents.mainFrame,
        };
        setRemoteSpaState(transition.nextRemoteSpaUrl);
        mockMainWindow.setAlwaysOnTop.mockClear();
        mockMainWindow.close.mockClear();

        await handlers.get('pip:setAlwaysOnTop')!(pipMainFrameEvent, {
          id: 'transition-pip',
          flag: false,
        });
        await handlers.get('pip:close')!(pipMainFrameEvent, { id: 'transition-pip' });

        expect(mockMainWindow.setAlwaysOnTop).toHaveBeenCalledWith(false);
        expect(mockMainWindow.close).toHaveBeenCalled();
        closed();
      } finally {
        setRemoteSpaState(null);
        testApp.isPackaged = false;
        mockWebContents.mainFrame = originalMainFrame;
      }
    });

    it('assigns a fresh owner when a closed PiP ID is reused', async () => {
      const closeFirst = await openPip('reused-pip');
      closeFirst();
      const closeSecond = await openPip('reused-pip', otherPermittedPipEvent);
      mockMainWindow.setAlwaysOnTop.mockClear();

      await handlers.get('pip:setAlwaysOnTop')!(pipOwnerEvent, {
        id: 'reused-pip',
        flag: false,
      });
      expect(mockMainWindow.setAlwaysOnTop).not.toHaveBeenCalled();
      await handlers.get('pip:setAlwaysOnTop')!(otherPermittedPipEvent, {
        id: 'reused-pip',
        flag: false,
      });
      expect(mockMainWindow.setAlwaysOnTop).toHaveBeenCalledWith(false);
      closeSecond();
    });
  });

  describe('permission handlers', () => {
    it('denies geolocation requests', () => {
      expect(sessionHandlers.permissionRequest).toBeDefined();
      const callback = vi.fn();
      sessionHandlers.permissionRequest!(null, 'geolocation', callback);
      expect(callback).toHaveBeenCalledWith(false);
    });

    it('allows camera requests', () => {
      const callback = vi.fn();
      sessionHandlers.permissionRequest!(null, 'media', callback);
      expect(callback).toHaveBeenCalledWith(true);
    });

    it('allows notification requests', () => {
      const callback = vi.fn();
      sessionHandlers.permissionRequest!(null, 'notifications', callback);
      expect(callback).toHaveBeenCalledWith(true);
    });

    it('allows speaker-selection requests', () => {
      const callback = vi.fn();
      sessionHandlers.permissionRequest!(null, 'speaker-selection', callback);
      expect(callback).toHaveBeenCalledWith(true);
    });

    it('permission check denies geolocation', () => {
      expect(sessionHandlers.permissionCheck).toBeDefined();
      const result = sessionHandlers.permissionCheck!(null, 'geolocation');
      expect(result).toBe(false);
    });

    it('permission check allows camera', () => {
      const result = sessionHandlers.permissionCheck!(null, 'media');
      expect(result).toBe(true);
    });

    it('permission check allows speaker-selection', () => {
      const result = sessionHandlers.permissionCheck!(null, 'speaker-selection');
      expect(result).toBe(true);
    });

    it('device permission allows camera/microphone/speaker/hid', () => {
      expect(sessionHandlers.devicePermission).toBeDefined();
      expect(sessionHandlers.devicePermission!({ deviceType: 'camera' })).toBe(true);
      expect(sessionHandlers.devicePermission!({ deviceType: 'microphone' })).toBe(true);
      expect(sessionHandlers.devicePermission!({ deviceType: 'speaker' })).toBe(true);
      expect(sessionHandlers.devicePermission!({ deviceType: 'hid' })).toBe(true);
    });

    it('device permission denies unknown types', () => {
      expect(sessionHandlers.devicePermission!({ deviceType: 'serial' })).toBe(false);
    });

    it('HID device selection picks first device', () => {
      expect(sessionHandlers.hidSelect).toBeDefined();
      const event = { preventDefault: vi.fn() };
      const callback = vi.fn();
      sessionHandlers.hidSelect!(event, { deviceList: [{ deviceId: 'dev-1' }] }, callback);
      expect(event.preventDefault).toHaveBeenCalled();
      expect(callback).toHaveBeenCalledWith('dev-1');
    });

    it('HID device selection returns empty when no devices', () => {
      const event = { preventDefault: vi.fn() };
      const callback = vi.fn();
      sessionHandlers.hidSelect!(event, { deviceList: [] }, callback);
      expect(callback).toHaveBeenCalledWith('');
    });
  });

  describe('app lifecycle callbacks', () => {
    it('before-quit stops auto-updater and force-destroys PiP windows', async () => {
      const { stopAutoUpdater } = await import('../../../src/main/updater');
      const beforeQuit = appOnCallbacks.get('before-quit');
      expect(beforeQuit).toBeDefined();
      await handlers.get('pip:open')!(
        { senderFrame: { url: 'http://localhost:3001/' } },
        { id: 'before-quit-pip' }
      );
      mockMainWindow.destroy.mockClear();

      beforeQuit!();

      expect(stopAutoUpdater).toHaveBeenCalled();
      expect(mockMainWindow.destroy).toHaveBeenCalled();
    });

    it('before-quit-for-update force-destroys PiP windows', async () => {
      const { autoUpdater } = await import('electron');
      const beforeQuitForUpdate = (autoUpdater.on as Mock).mock.calls.find(
        (call) => call[0] === 'before-quit-for-update'
      )?.[1] as (() => void) | undefined;
      expect(beforeQuitForUpdate).toBeDefined();
      await handlers.get('pip:open')!(
        { senderFrame: { url: 'http://localhost:3001/' } },
        { id: 'before-update-quit-pip' }
      );
      mockMainWindow.destroy.mockClear();

      beforeQuitForUpdate!();

      expect(mockMainWindow.destroy).toHaveBeenCalled();
    });

    it('window-all-closed quits on non-macOS', async () => {
      const { app } = await import('electron');
      (app.quit as Mock).mockClear();
      const windowAllClosed = appOnCallbacks.get('window-all-closed');
      expect(windowAllClosed).toBeDefined();
      windowAllClosed!();
      // On non-darwin platforms, quit is called
      if (process.platform !== 'darwin') {
        expect(app.quit).toHaveBeenCalled();
      }
    });

    it('proactive refresh callback sends to renderer', async () => {
      const { setProactiveRefreshCallback } = await import('../../../src/main/tokenManager');
      const cb = (setProactiveRefreshCallback as Mock).mock.calls[0]?.[0] as (
        ...args: unknown[]
      ) => void;
      expect(cb).toBeDefined();
      mockWebContents.send.mockClear();
      cb('new-access-token', 'new-session-id', 'previous-session-id');
      expect(mockWebContents.send).toHaveBeenCalledWith('auth:token-refreshed', {
        accessToken: 'new-access-token',
        sessionId: 'new-session-id',
        previousSessionId: 'previous-session-id',
      });
    });

    it('resume handler triggers onSystemResume', async () => {
      const { powerMonitor } = await import('electron');
      const { onSystemResume } = await import('../../../src/main/tokenManager');
      const resumeCall = (powerMonitor.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'resume'
      );
      expect(resumeCall).toBeDefined();
      (onSystemResume as Mock).mockClear();
      resumeCall![1]();
      expect(onSystemResume).toHaveBeenCalled();
    });
  });

  describe('createWindow', () => {
    it('creates a BrowserWindow', async () => {
      const { BrowserWindow } = await import('electron');
      // BrowserWindow was called during module init (createWindow from whenReady)
      expect(BrowserWindow).toHaveBeenCalled();
    });

    it('registers ready-to-show handler', () => {
      expect(mockMainWindow.once).toHaveBeenCalledWith('ready-to-show', expect.any(Function));
    });

    it('registers closed handler', () => {
      expect(mockMainWindow.on).toHaveBeenCalledWith('closed', expect.any(Function));
    });

    it('registers window open handler for external links', () => {
      expect(mockWebContents.setWindowOpenHandler).toHaveBeenCalled();
    });
  });

  describe('GPU_VENDORS mapping', () => {
    it('maps Intel vendor ID', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [{ vendorId: 0x8086, deviceId: 0x5678 }],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as { vendor: string } | null;
      expect(result!.vendor).toBe('Intel');
    });

    it('maps Apple vendor ID', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [{ vendorId: 0x106b, deviceId: 0x0001 }],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as { vendor: string } | null;
      expect(result!.vendor).toBe('Apple');
    });

    it('maps AMD vendor ID', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [{ vendorId: 0x1002, deviceId: 0x0001 }],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as { vendor: string } | null;
      expect(result!.vendor).toBe('AMD');
    });

    it('handles unknown vendor ID', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [{ vendorId: 0x9999, deviceId: 0x0001 }],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as { vendor: string } | null;
      expect(result!.vendor).toContain('Unknown');
    });

    it('returns null when no GPU device', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({ gpuDevice: [] });
      const result = await handlers.get('gpu:getInfo')!();
      expect(result).toBeNull();
    });

    it('skips hex-only driverVendor on macOS', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [
          { vendorId: 0x106b, deviceId: 0x0001, driverVendor: '0x106b', driverDescription: 'M1' },
        ],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as { vendor: string } | null;
      expect(result!.vendor).toBe('Apple'); // Falls back to vendor map, not hex string
    });

    it('maps interleaved Chromium encode profiles by exact value', async () => {
      const { app } = await import('electron');
      (app.getGPUInfo as Mock).mockResolvedValueOnce({
        gpuDevice: [{ vendorId: 0x10de, deviceId: 0x0001 }],
        videoEncodeAcceleratorSupportedProfiles: [
          { profile: 24 }, // AV1, not HEVC
          { profile: 16 }, // HEVC
          { profile: 23 }, // Theora, ignored
          { profile: 27 }, // Dolby Vision, ignored
          { profile: 35 }, // HEVC extension profile
          { profile: 11 }, // VP8
        ],
      });
      const result = (await handlers.get('gpu:getInfo')!()) as {
        encodeProfiles: string[];
      } | null;
      expect(result!.encodeProfiles.sort()).toEqual(['video/AV1', 'video/HEVC', 'video/VP8']);
    });
  });

  describe('deep-link queue (#945)', () => {
    // Valid codes only — deepLink.ts rejects I/O/l/0/1 and anything but 8 chars.
    const TEN_CODES = [
      'AAAAAAA2',
      'AAAAAAA3',
      'AAAAAAA4',
      'AAAAAAA5',
      'AAAAAAA6',
      'AAAAAAA7',
      'AAAAAAA8',
      'AAAAAAA9',
      'AAAAAAB2',
      'AAAAAAB3',
    ];
    const FRIEND_CODE = 'AbCdEfGh';
    const INVITE_CODE = 'AbCdEfGj';
    const rendererEvent = { sender: mockWebContents };

    let DEEP_LINK_EMIT_WINDOW_MS: number;
    let deliverDeepLink: (url: string) => void;
    let signalInviteReady: CallbackFn;
    let signalFriendReady: CallbackFn;
    let simulateReload: CallbackFn;

    beforeAll(async () => {
      const { ipcMain } = await import('electron');
      const ipcListener = (channel: string): CallbackFn => {
        const call = (ipcMain.on as Mock).mock.calls.find((c: unknown[]) => c[0] === channel);
        expect(call, `ipcMain.on('${channel}') was never registered`).toBeDefined();
        return call![1] as CallbackFn;
      };
      signalInviteReady = ipcListener('invite:renderer-ready');
      signalFriendReady = ipcListener('deeplink:renderer-ready');

      const didStartLoading = mockWebContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'did-start-loading'
      )?.[1] as CallbackFn | undefined;
      expect(didStartLoading).toBeDefined();
      simulateReload = didStartLoading!;

      const openUrl = appOnCallbacks.get('open-url');
      expect(openUrl).toBeDefined();
      deliverDeepLink = (url: string) => openUrl!({ preventDefault: vi.fn() }, url);

      // Import the window rather than restating 1000: a burst test that
      // advanced a stale literal would leave a real deferred send to fire
      // mid-suite and pollute a later test's send assertions.
      ({ DEEP_LINK_EMIT_WINDOW_MS } = await import('../../../src/main/main'));
    });

    // The queue and both readiness flags are module state in main.ts that
    // outlives a single test. Arm both flags so a drain empties whatever a
    // previous test left queued, then drop both the way a renderer reload does.
    // Synchronous on purpose: an await here would let createWindow's 1s
    // deferred drain interleave with a test's own arm/drain sequence.
    // Drains to quiescence rather than assuming one ready+reload empties things.
    // Since #945 (M0/Md3) the gate holds a bounded FIFO and releases ONE code per
    // emit window, and a lifecycle reset re-queues whatever is still held instead
    // of relying on a timer it has just cleared — so after a burst test a single
    // ready+reload leaves codes queued, and they surfaced in whichever test ran
    // next. Cycling ready + one window until the buffers are empty is what makes
    // each case independent again.
    function resetDeepLinkState(): void {
      vi.useFakeTimers();
      try {
        // Loop to QUIESCENCE, not a fixed count: switching back to real timers
        // destroys any still-pending flush, which would strand a held code that
        // the closing reload then re-queues — the leak this helper exists to
        // prevent. A cycle that emits nothing means both buffers are empty.
        for (let i = 0; i < 40; i += 1) {
          const before = mockWebContents.send.mock.calls.length;
          // Reload FIRST. A prior test usually leaves readiness already true, so
          // signalling it again drains nothing and the quiescence check would
          // break out on cycle 0 with codes still queued. The reload drops both
          // flags (and re-queues anything held), so the signals below are always
          // a false->true transition and always force a drain.
          simulateReload();
          signalInviteReady(rendererEvent);
          signalFriendReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
          if (mockWebContents.send.mock.calls.length === before) break;
        }
      } finally {
        vi.useRealTimers();
      }
      simulateReload();
      mockWebContents.send.mockClear();
    }

    function sentOn(channel: string): string[] {
      return mockWebContents.send.mock.calls
        .filter((c: unknown[]) => c[0] === channel)
        .map((c: unknown[]) => (c[1] as { code: string }).code);
    }

    it('caps the pending queue at 8 entries, dropping oldest', () => {
      resetDeepLinkState();
      vi.useFakeTimers();
      try {
        for (const code of TEN_CODES) deliverDeepLink(`concord://invite/${code}`);

        signalInviteReady(rendererEvent);

        // The two oldest were dropped by the cap, so the drain leads with the
        // third code and not the first — that is the drop-oldest proof.
        expect(sentOn('invite:received')).toEqual([TEN_CODES[2]]);

        // #945 (M0): the rest are released IN ORDER, one per window — they are no
        // longer collapsed to the newest. This assertion previously read
        // `[TEN_CODES[2], TEN_CODES[9]]`, and that collapse is exactly the silent
        // drop the fix removes: six codes the user clicked vanished with no log
        // and no trace, while both the gate comment and spec §6a asserted a
        // different code is deferred and never dropped.
        vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        expect(sentOn('invite:received')).toEqual([TEN_CODES[2], TEN_CODES[3]]);

        // Drain the remainder to prove every queued code arrives, in click order.
        for (let i = 0; i < 6; i += 1) vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        expect(sentOn('invite:received')).toEqual(TEN_CODES.slice(2));
      } finally {
        vi.useRealTimers();
      }
    });

    // VULN-005: one shared queue let friend traffic spend the invite allowance.
    // The kinds arrive from the same untrusted surfaces but wait on independent
    // readiness flags, so they get independent queues — the same reasoning that
    // gave the friend WAF rule its own ref and counter.
    it('keeps a queued invite when a friend burst fills the friend queue', () => {
      resetDeepLinkState();
      vi.useFakeTimers();
      try {
        // The user clicks a genuine invite during cold start...
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        // ...and a page they have open fires a full cap's worth of friend codes.
        for (const code of TEN_CODES.slice(0, 8)) deliverDeepLink(`concord://friend/${code}`);

        signalInviteReady(rendererEvent);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        signalFriendReady(rendererEvent);
        // The friend queue kept its own eight, and #945 (M0) releases them IN
        // ORDER, one per window — previously this asserted
        // `[TEN_CODES[0], TEN_CODES[7]]`, collapsing the burst to its newest and
        // silently discarding the six between.
        vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        expect(sentOn('deeplink:friend-code')).toEqual([TEN_CODES[0], TEN_CODES[1]]);

        // Every one of the eight arrives, in click order — the invariant the
        // gate comment and spec §6a both assert.
        for (let i = 0; i < 6; i += 1) vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        expect(sentOn('deeplink:friend-code')).toEqual(TEN_CODES.slice(0, 8));
      } finally {
        vi.useRealTimers();
      }
    });

    // VULN-006: post-readiness delivery was unbounded — N deliveries meant N IPC
    // sends, N forced AddFriendModal re-opens, and N authenticated
    // previewFriendCode calls, from a page the user merely visited.
    it('collapses a post-readiness burst of one code into a single send', () => {
      resetDeepLinkState();
      signalFriendReady(rendererEvent);
      vi.useFakeTimers();
      try {
        for (let i = 0; i < 250; i += 1) deliverDeepLink(`concord://friend/${FRIEND_CODE}`);
        expect(sentOn('deeplink:friend-code')).toEqual([FRIEND_CODE]);

        // Nothing is waiting at the window edge: re-sending the code the
        // renderer is already showing would change nothing.
        vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        expect(sentOn('deeplink:friend-code')).toEqual([FRIEND_CODE]);
      } finally {
        vi.useRealTimers();
      }
    });

    it('defers, never drops, a different code arriving inside the window', () => {
      resetDeepLinkState();
      signalInviteReady(rendererEvent);
      vi.useFakeTimers();
      try {
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        deliverDeepLink(`concord://invite/${TEN_CODES[9]}`);
        // Invite B is held back by the window, not discarded...
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);
        // ...so a user who clicks invite A then invite B still lands on B.
        expect(sentOn('invite:received')).toEqual([INVITE_CODE, TEN_CODES[9]]);
      } finally {
        vi.useRealTimers();
      }
    });

    it('terminates the drain when only the invite renderer is ready', () => {
      resetDeepLinkState();
      deliverDeepLink(`concord://friend/${FRIEND_CODE}`);
      deliverDeepLink(`concord://invite/${INVITE_CODE}`);

      signalInviteReady(rendererEvent);

      // A blind re-queue of the unemittable friend entry would re-enter the
      // queue in this same tick and loop forever. A filtered drain emits
      // exactly the one emittable entry and stops.
      expect(mockWebContents.send).toHaveBeenCalledTimes(1);
      expect(mockWebContents.send).toHaveBeenCalledWith('invite:received', { code: INVITE_CODE });
    });

    it('preserves a friend entry across an invite-only drain', () => {
      resetDeepLinkState();
      deliverDeepLink(`concord://friend/${FRIEND_CODE}`);
      deliverDeepLink(`concord://invite/${INVITE_CODE}`);

      signalInviteReady(rendererEvent);
      expect(sentOn('deeplink:friend-code')).toEqual([]);

      signalFriendReady(rendererEvent);
      expect(sentOn('deeplink:friend-code')).toEqual([FRIEND_CODE]);
      expect(mockWebContents.send).toHaveBeenCalledTimes(2);
    });

    it('emits ZERO invite:received for a friend deep link (old-SPA safety lock)', () => {
      resetDeepLinkState();
      deliverDeepLink(`concord://friend/${FRIEND_CODE}`);

      // A pre-22 SPA subscribes to invite:received and never calls
      // friendRendererReady. It must receive NOTHING — a widened
      // invite:received payload would instead open JoinServerModal with a
      // friend code (spec X1). Repeat the signal: every drain site must stay
      // silent, not just the first.
      signalInviteReady(rendererEvent);
      signalInviteReady(rendererEvent);

      expect(mockWebContents.send).not.toHaveBeenCalled();
      expect(sentOn('invite:received')).toEqual([]);
    });

    it('keeps invite:received emitting a bare { code } payload', () => {
      resetDeepLinkState();
      signalInviteReady(rendererEvent);
      deliverDeepLink(`concord://invite/${INVITE_CODE}`);

      const call = mockWebContents.send.mock.calls.find(
        (c: unknown[]) => c[0] === 'invite:received'
      );
      expect(call).toBeDefined();
      // toEqual, not toMatchObject — a `kind` field would fail this.
      expect(call![1]).toEqual({ code: INVITE_CODE });
    });

    it('ignores a friend readiness signal from an untrusted sender', () => {
      resetDeepLinkState();
      deliverDeepLink(`concord://friend/${FRIEND_CODE}`);

      signalFriendReady({ sender: { id: 99 } });
      expect(mockWebContents.send).not.toHaveBeenCalled();

      signalFriendReady(rendererEvent);
      expect(sentOn('deeplink:friend-code')).toEqual([FRIEND_CODE]);
    });

    it('logs only the rejection reason, never the code', () => {
      resetDeepLinkState();
      const warn = vi.spyOn(console, 'warn').mockImplementation(() => {});
      try {
        deliverDeepLink('concord://friend/nope');
        expect(warn).toHaveBeenCalled();
        for (const call of warn.mock.calls) {
          expect(JSON.stringify(call)).not.toContain('nope');
        }
      } finally {
        warn.mockRestore();
      }
    });

    // ── #2363: carry a delivered invite code across a main-initiated SPA swap ──
    describe('main-initiated SPA swap carry (#2363)', () => {
      // Not yet implemented in production: main.ts exports no primitive for
      // arming the carry. Driving the real spa:reloadLatest handler cannot
      // substitute — mockMainWindow.loadURL is a bare vi.fn that resolves
      // without ever invoking the captured did-start-loading listener (see
      // the harness above at :2094-2098 and the spa:reloadLatest coverage at
      // :2860-2872, which asserts loadURL was called but never that reload
      // fired). So this imports the not-yet-existing carryDeepLinkAcrossNextLoad
      // export directly, per spec §8 Q1 / plan Task 1 step 5.
      //
      // Pre-fix this export does not exist. The guard lets the assertion run,
      // so each test below fails on the OBSERVABLE (one send where two are
      // expected, or vice versa) rather than on a missing symbol — a
      // missing-symbol error reads like a falsification and is not one.
      // Post-fix the guard is inert (the export exists and is called) and the
      // assertion is the whole test.
      async function armDeepLinkCarryForTest(): Promise<() => void> {
        const mainModule = await import('../../../src/main/main');
        const carry = (mainModule as unknown as { carryDeepLinkAcrossNextLoad?: () => unknown })
          .carryDeepLinkAcrossNextLoad;
        if (typeof carry !== 'function') return () => {};
        const release = carry();
        return typeof release === 'function' ? (release as () => void) : () => {};
      }

      it('re-delivers a spent invite code across a main-initiated SPA swap (regression for #2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        // Main initiates a source swap: arm the carry, then fire the real
        // did-start-loading listener the window registers at main.ts:723.
        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
      });

      it('does NOT re-deliver a spent invite code across an ordinary reload (regression for #2363)', () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        simulateReload(); // carry NOT armed — an ordinary reload
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
      });

      // INVERTED (CODEX P2, round 8). This originally asserted that
      // auth:clearTokens forgets, and that was wrong: not one of its three
      // renderer callers is unambiguously a logout. useSSOFlow.begin() calls it
      // immediately BEFORE starting SSO, so forgetting there destroys an invite
      // queued while the app was closed — the click-invite-then-sign-in flow,
      // which is the primary path #2363 exists to repair. App.tsx's
      // ownerless-credential rejection and gracefulReset's rememberMe path are
      // login-side too. Real logout has auth:logout, which does forget, so
      // narrowing costs nothing.
      //
      // The second handler to be wrongly read as a logout edge. The first was
      // auth:clearTokensIfOwner, and the test below it is this one's twin.
      // The one caller of auth:clearTokens that IS a logout: nuclearReset(), on a
      // refresh failure with rememberMe === false. It reaches this handler through
      // gracefulReset and never calls auth:logout, so without the opt-in user A's
      // code survived into user B's session (CODEX P2, round 10).
      it('auth:clearTokens forgets BY DEFAULT — an older SPA sends no argument (#2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        await handlers.get('auth:clearTokens')!({
          senderFrame: { url: 'app://concord/index.html' },
        });

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
      });

      // The flag is renderer-supplied, so it is read strictly: anything that is
      // not the literal `true` fails to opt out, and the forgetting default
      // applies. Strictness points at the SAFE side — a malformed opt-out loses
      // an invite, where a lenient one would carry a code across accounts.
      it('auth:clearTokens treats a truthy-but-not-true keepDeepLinks as no (#2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        await handlers.get('auth:clearTokens')!(
          { senderFrame: { url: 'app://concord/index.html' } },
          { keepDeepLinks: 'yes' }
        );

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
      });

      it('auth:clearTokens does NOT forget when the caller opts out (#2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        await handlers.get('auth:clearTokens')!(
          { senderFrame: { url: 'app://concord/index.html' } },
          { keepDeepLinks: true }
        );

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        // The invite survives an SSO-start token clear and is carried into the
        // successor document, which is the whole point of the feature.
        expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
      });

      // Companion fence for the path the app ACTUALLY takes on a user-initiated
      // logout. userStore.logout() calls electron.logout() -> auth:logout ->
      // performLogout(), which clears credentials inside tokenManager and never
      // reaches the auth:clearTokens handler the case above drives. The same
      // logout does reach it eventually, but only through nuclearReset()'s
      // doubly-optional-chained `globalThis.electron?.clearTokens?.()`
      // (resetService.ts) — a renderer-side call ordering that no main-process
      // test can observe and that a refactor could drop with nothing going red.
      // So spec §5.2 must hold at the auth:logout edge on its own, and this is
      // what says so.
      it('does not re-deliver a delivered code after auth:logout, even with the carry armed (fence for #2363 spec §5.2)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        await handlers.get('auth:logout')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          { accessToken: 'tok' }
        );

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
      });

      // Companion fence for the auth:clearTokensIfOwner edge (#2363). Every
      // caller of this handler is login-side (aborted SSO, interrupted email
      // verification, cancelled account-link/passphrase-setup, aborted login
      // continuation, eager-unlock cleanup) — never logout — so unlike the two
      // fences above it must NOT forget a deliverable code: a
      // `concord://invite/CODE` queued before any window existed would
      // otherwise be erased by an aborted SSO sign-in.
      it('auth:clearTokensIfOwner does NOT forget a delivered invite code (regression for #2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        const cleared = await handlers.get('auth:clearTokensIfOwner')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          41
        );
        expect(cleared).toBe(true);

        const releaseCarry = await armDeepLinkCarryForTest();
        try {
          simulateReload();
        } finally {
          releaseCarry();
        }
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
      });

      // Companion fence for #2363: forgetDeliveredDeepLinks must clear
      // gate.held, not just gate.lastCode. resetDeepLinkDelivery re-queues
      // WHATEVER is in gate.held on every reload, carried or not (see main.ts
      // "for (const code of carried) queueDeepLink(...)" — unconditional), so
      // an un-cleared held FIFO leaks into the very next ordinary reload after
      // logout, no carry required. Advancing timers alone does not observe
      // this (nothing re-arms the cancelled flush timer without a reload),
      // which is why the reload below is load-bearing, not incidental.
      it('auth:logout forgets codes still held in gate.held, not just the delivered one (regression for #2363)', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        vi.useFakeTimers();
        try {
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Inside the emit window: held back by the gate, not dropped.
          const held = TEN_CODES.slice(0, 3);
          for (const code of held) deliverDeepLink(`concord://invite/${code}`);

          await handlers.get('auth:logout')!(
            { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
            { accessToken: 'tok' }
          );

          // An ORDINARY reload — no carry armed. If `held` had survived the
          // logout it would be re-queued here regardless, and the drain below
          // would deliver it into what is meant to be a fresh session.
          simulateReload();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 5);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // Companion fence for #2363: forgetDeliveredDeepLinks must run BEFORE
      // performLogout's await, not after — performLogout clears credentials on
      // its first line and then issues an unbounded network POST, so a forget
      // placed after the await leaves a deliverable code exposed for the width
      // of that request.
      it('auth:logout forgets deep links BEFORE performLogout resolves, not after (regression for #2363)', async () => {
        const { performLogout } = await import('../../../src/main/tokenManager');
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        const logoutPost = deferred<void>();
        (performLogout as Mock).mockImplementationOnce(() => logoutPost.promise);

        const handlerPromise = handlers.get('auth:logout')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          { accessToken: 'tok' }
        );

        // The POST is still in flight — the forget must already have run.
        const releaseCarry = await armDeepLinkCarryForTest();
        try {
          simulateReload();
        } finally {
          releaseCarry();
        }
        signalInviteReady(rendererEvent);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        logoutPost.resolve();
        await handlerPromise;
      });

      // ── Red-team regression guards (#2363 adversarial pass) ──────────────
      // Three exploits were built against the carry as first written and are
      // now closed. Each guard below pins ONE of those fixes. They are not
      // redundant with the four tests above: those pin the FEATURE (a spent
      // code is carried across a main-initiated swap and not across an
      // ordinary reload, and never across a logout). These pin the BOUNDS
      // that stop the same feature being turned against the user.

      // F1. The carry re-queues the delivered code FIRST, into a queue whose
      // overflow policy is drop-OLDEST at PENDING_DEEP_LINK_CAP — the same
      // value as DEEP_LINK_HELD_MAX. That made the carried code the guaranteed
      // eviction victim whenever a hostile page had filled gate.held: a web
      // page firing 8 concord:// links deleted the invite the user actually
      // clicked and put 8 attacker-chosen prefilled join modals in its place.
      // The fix trims the oldest HELD codes instead. Delete this and that
      // substitution comes back silently.
      it('F1 (red-team): a hostile 8-code burst must NOT evict the carried invite — oldest HELD codes drop instead', async () => {
        const { DEEP_LINK_HELD_MAX } = await import('../../../src/main/main');
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          // The user clicks a real invite: sent immediately, gate.lastCode = it.
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // A hostile page fills gate.held to capacity inside the emit window.
          const burst = TEN_CODES.slice(0, DEEP_LINK_HELD_MAX);
          for (const code of burst) deliverDeepLink(`concord://invite/${code}`);

          // Main swaps the SPA source on its own initiative.
          const releaseCarry = await armDeepLinkCarryForTest();
          simulateReload();
          releaseCarry();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * (DEEP_LINK_HELD_MAX + 4));

          const afterSwap = sentOn('invite:received').slice(1);
          // The code the user acted on survives, and arrives FIRST.
          expect(afterSwap[0]).toBe(INVITE_CODE);
          // Exactly one code was dropped, and it is the OLDEST HELD one.
          expect(afterSwap).not.toContain(burst[0]);
          expect(afterSwap).toEqual([INVITE_CODE, ...burst.slice(1)]);
        } finally {
          vi.useRealTimers();
        }
      });

      // F1 control. One code below DEEP_LINK_HELD_MAX nothing is trimmed at
      // all. This is what isolates the cap as the cause rather than some other
      // property of a burst — without it, a future change that dropped a held
      // code unconditionally would still satisfy the guard above.
      it('F1 CONTROL (red-team): a 7-code burst trims nothing — isolates DEEP_LINK_HELD_MAX as the cause', async () => {
        const { DEEP_LINK_HELD_MAX } = await import('../../../src/main/main');
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          const burst = TEN_CODES.slice(0, DEEP_LINK_HELD_MAX - 1);
          for (const code of burst) deliverDeepLink(`concord://invite/${code}`);

          const releaseCarry = await armDeepLinkCarryForTest();
          simulateReload();
          releaseCarry();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * (DEEP_LINK_HELD_MAX + 4));

          expect(sentOn('invite:received').slice(1)).toEqual([INVITE_CODE, ...burst]);
        } finally {
          vi.useRealTimers();
        }
      });

      // F2. The arming used to be a boolean that resetDeepLinkDelivery consumed
      // on the FIRST reset. applySpaDecision awaits a navigation, and other
      // navigations land in that window — a renderer location.reload(), a
      // crash-reload, and applySpaDecision's OWN remote->cache->bundled
      // fallback, which issues two loadURL calls inside one armed wrapper. Any
      // of them spent the arming, so main's own swap carried nothing and #2363
      // reproduced with no error and no log. This test is the one that catches
      // a revert to a boolean: it asserts the arming SURVIVES a foreign reset.
      it('F2 (red-team): an unrelated navigation inside the armed window must NOT spend the arming', async () => {
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // applySpaDecisionCarryingDeepLink has armed and is mid-await.
          const releaseCarry = await armDeepLinkCarryForTest();

          // A navigation main did NOT initiate lands first.
          simulateReload();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);

          // Main's own navigation finally starts. The arming must still be live.
          simulateReload();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          releaseCarry(); // applySpaDecision resolved; the wrapper's finally runs

          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // F3. gate.lastCode is set at send time and main is never told the modal
      // was closed, so the carry had no notion of CONSUMPTION: every later
      // main-initiated swap re-presented an attacker-chosen prefilled join
      // modal after an explicit decline, indefinitely and on a schedule
      // (SPA_RETRY_DELAYS_MS, plus every "Load latest UI" click) an attacker
      // can predict. DEEP_LINK_CARRY_MAX_AGE_MS encodes the recency condition
      // the carry's own rationale always assumed. BOTH halves are asserted on
      // purpose — a one-sided "is not re-presented" is satisfied by the carry
      // being broken outright.
      it('F3 (red-team): a code older than DEEP_LINK_CARRY_MAX_AGE_MS is NOT re-presented; a fresh one IS', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        // Inside the window: the #2363 repair still works.
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          vi.advanceTimersByTime(DEEP_LINK_CARRY_MAX_AGE_MS - DEEP_LINK_EMIT_WINDOW_MS);
          const releaseCarry = await armDeepLinkCarryForTest();
          simulateReload();
          releaseCarry();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);

          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }

        // Outside the window: a dismissed invite stays dismissed.
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          vi.advanceTimersByTime(DEEP_LINK_CARRY_MAX_AGE_MS + DEEP_LINK_EMIT_WINDOW_MS);
          const releaseCarry = await armDeepLinkCarryForTest();
          simulateReload();
          releaseCarry();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);

          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P1. The fence was a hand-picked 15 s documented as covering the
      // cold-start retry schedule "with margin". It covered nothing: the delays
      // are SEQUENTIAL and each attempt first awaits resolveSpaSource, so the
      // second attempt's swap lands at 4 s + T + 10 s + T. The fence then
      // declined the carry on the exact recovery path it exists to repair —
      // silently, presenting as #2363 itself. This reds on any return to a
      // literal: 15 s against a 24 s worst case.
      it('CODEX P1 (red-team): the carry window exceeds the WORST-CASE cold-start retry schedule', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');
        // From the shared module, never a mirrored literal: a mirror here would
        // pass while production under-covered, which is the whole defect.
        const { CONFIG_TIMEOUT_MS, SPA_RETRY_DELAYS_MS } =
          await import('../../../src/main/spaTiming');

        // Every BOUNDED step of the chain, not just the delays. The first retry
        // is scheduled only after the initial applySpaDecision resolves, and each
        // attempt schedules its successor only after its own does — and every
        // applySpaDecision awaits a captureSpaHash bounded by the same timeout.
        // So: one hash before the chain, then per attempt one config fetch AND
        // one hash. Counting only the config fetches is what left the previous
        // version of this fence short (CODEX P2, round 6).
        const worstCase =
          SPA_RETRY_DELAYS_MS.reduce((total, delay) => total + delay, 0) +
          (2 * SPA_RETRY_DELAYS_MS.length + 1) * CONFIG_TIMEOUT_MS;

        expect(DEEP_LINK_CARRY_MAX_AGE_MS).toBeGreaterThan(worstCase);
      });

      // The forget-only channel (#2363, v25). Every other edge that forgets also
      // destroys credentials; a rememberMe teardown must forget WITHOUT doing so,
      // because the disk tokens stay for the next launch to retry. Without it main
      // kept the code alive across that teardown and a swap replayed user A's
      // invite into user B's renderer.
      it('deeplink:forget clears deliverable state without a credential clear', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        await handlers.get('deeplink:forget')!({
          senderFrame: { url: 'app://concord/index.html' },
        });

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'the forgotten code must not survive into the successor document'
        ).toEqual([INVITE_CODE]);
      });

      // Supervisor Q (2026-08-28): are the scheduled SPA retry and spa:reloadLatest
      // two entry points to the same replay? Structurally they are ONE — both reach
      // the carry only through applySpaDecisionCarryingDeepLink, so the forget
      // zeroes the state both would read. What they can differ on is TIMING, and
      // that is what this covers: the retry is already in flight (carry armed,
      // applySpaDecision pending) when the refresh failure fires the forget. Its
      // release token then lands AFTER the forget. The epoch advance in
      // forgetDeliveredDeepLinks exists for exactly that token and had no test.
      //
      // The failure it prevents is a lost invite, not a leak: a stale token that
      // still decrements drives the depth to -1, and the NEXT genuine swap arms it
      // back to 0, where resetDeepLinkDelivery's `depth > 0` check declines to
      // carry. So the assertion is on the swap AFTER the forget, not on the forget.
      it('a carry token issued before the forget cannot disarm the swap after it', async () => {
        const OTHER_CODE = 'ZZZZZZAC';
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // The in-flight retry, armed while user A's code was still live.
        const staleRelease = await armDeepLinkCarryForTest();

        await handlers.get('deeplink:forget')!({
          senderFrame: { url: 'app://concord/index.html' },
        });
        staleRelease();

        // A later, unrelated swap — user B has signed in and clicked their own invite.
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${OTHER_CODE}`);
        const liveRelease = await armDeepLinkCarryForTest();
        simulateReload();
        liveRelease();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received').filter((code) => code === OTHER_CODE),
          'a stale token that still decremented would leave the depth at -1, and this swap would decline to carry'
        ).toEqual([OTHER_CODE, OTHER_CODE]);
      });

      // Codex P2 on PR #2967, and the reason deeplink:forget alone is not enough.
      // spaLoader's version gate is deliberately one-directional -- it refuses an
      // SPA NEWER than the shell and loads an older one indefinitely -- so a
      // renderer built before contract 25 never calls the forget channel at all.
      // On those SPAs a rememberMe refresh failure still leaves main holding user
      // A's code, and a scheduled retry inside the 44 s carry window replays it
      // into whoever signed in next.
      //
      // The fence is therefore MAIN-side and reads the credential store, which
      // every SPA must go through regardless of its contract version: a code
      // retained under owner A is not deliverable once a DIFFERENT owner is
      // current. No renderer cooperation, so SPA age stops mattering.
      it('does not deliver a code retained under one owner to a different owner', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        // The rememberMe refresh failure on an OLD SPA: no deeplink:forget call,
        // and clearTokens is not called either, so main is told nothing at all.
        // Then user B signs in -- storeRefreshToken mints the next owner.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          "user A's code must not reach user B's renderer, whatever the SPA's contract version"
        ).toEqual([INVITE_CODE]);
      });

      // The positive control, and the flow the fence could most easily break.
      // A code queued before anyone signed in belongs to NOBODY, and delivering it
      // to the account that then signs in IS the feature (click invite -> sign in
      // -> join). Its owner tag is null, so the fence must never fire for it.
      it('still delivers a code queued before sign-in to the account that signs in', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        // Logged out, no window ready: the code sits in the pending queue.
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        expect(sentOn('invite:received')).toEqual([]);

        // The user signs in; a fresh owner is minted.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'an invite clicked while logged out is exactly what the user is signing in to accept'
        ).toEqual([INVITE_CODE]);
      });

      // Each fence site below covers a delivery path the others do not reach.
      // Written because a mutation sweep found both of them uncovered: dropping
      // either fence left the suite green.

      // flushDeepLinkGate calls sendDeepLink DIRECTLY (main.ts) — not through the
      // drain, not through emitDeepLink — so a code sitting in gate.held when the
      // window timer fires reaches the renderer on a path nothing else fences.
      it('does not flush a held code to an owner that did not receive it', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        vi.useFakeTimers();
        try {
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          // Inside the emit window, so this one is HELD rather than sent.
          deliverDeepLink(`concord://invite/${TEN_CODES[9]}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          (getCredentialCustodyState as Mock).mockReturnValue({
            credentialOwner: 42,
            pendingE2EEUnlock: false,
          });
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS);

          expect(
            sentOn('invite:received'),
            'the held code belongs to the owner who clicked it, not to whoever the timer finds'
          ).toEqual([INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // The arrival fence. Without it, noteDeepLinkOwner tags the whole retained
      // set with the NEW owner as B's own click comes in, and the drain then sees
      // a matching owner and hands A's queued codes over too — the fence
      // disarming itself one call before it was needed.
      it('a new owner clicking their own link does not inherit the previous owner queue', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        // Renderer not ready: A's code waits in the pending queue.
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        deliverDeepLink(`concord://invite/${TEN_CODES[9]}`);
        signalInviteReady(rendererEvent);

        expect(sentOn('invite:received'), "B gets B's invite and only B's invite").toEqual([
          TEN_CODES[9],
        ]);
      });

      // The current === null carve-out, which is a documented behaviour rather
      // than a defensive default: an aborted SSO, a cancelled account-link and an
      // interrupted email verification all clear credentials WITHOUT minting a
      // successor. Those are login-side, and forgetting there deletes the invite
      // the user is about to retry with ([internal]rules/electron.md).
      it('keeps a queued code when credentials are cleared with no successor owner', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // Credentials gone, nobody replaced them.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'an aborted login must not delete the invite it was aborting in the middle of'
        ).toEqual([INVITE_CODE]);
      });

      // noteDeepLinkOwner refuses to write a NULL tag, and that refusal is the
      // fence's own laundering guard rather than a null-check for tidiness. A
      // logged-out arrival is exempt from the fence by the tag-null carve-out, so
      // if it also RESET the tag, it would hand user A's still-queued code the
      // same exemption — and the next owner to sign in would receive both.
      // Plausible on a shared machine: sign out, click a link, hand over the laptop.
      it('a logged-out arrival does not launder the previous owner retained code', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // Signed out — no owner at all.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        deliverDeepLink(`concord://invite/${TEN_CODES[9]}`);

        // Someone else signs in.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          "a signed-out click must not re-open user A's retained code to the next account"
        ).not.toContain(INVITE_CODE);
      });

      // Codex P2, main.ts:688. The tag-null carve-out is what lets a pre-login
      // code reach whoever signs in -- but the tag was only ever written at OS
      // ARRIVAL, so after that delivery it stayed null and the exemption never
      // expired. The primary flow (click invite -> sign in -> join) therefore
      // produced a permanently ownerless code: A receives it, and on a pre-v25
      // SPA a later swap could still replay it to B. Delivery binds it now.
      it('binds a pre-login code to the account that actually receives it', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // User A signs in and receives it — the feature, and it must still work.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        signalInviteReady(rendererEvent);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        // A's session ends without telling main (old SPA), B signs in, swap.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 43,
          pendingE2EEUnlock: false,
        });
        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          "once A has received it the code is A's, and the pre-login exemption is spent"
        ).toEqual([INVITE_CODE]);
      });

      // Codex P2, main.ts:924. `handleInviteDeepLink` is NOT the only OS entry
      // point: on Windows and Linux a protocol activation against a running app
      // arrives as `second-instance` -> handleInviteDeepLinksFromArgv, which
      // called queueDeepLink directly and so met neither the fence nor the bind.
      //
      // The DELIVERY bind already covers a code that gets DELIVERED, so the
      // reproducing shape is the ARRIVAL fence — and the symptom is the opposite
      // of the one reported. Without a fence at the argv arrival, B's code is
      // queued while the tag is still A's, and the drain fence then fires once
      // and forgets EVERYTHING pending: A's retained code AND B's own fresh
      // click. B loses the invite they just opened. Confirmed by a positive
      // control (drop B's arrival and the queue still drains to nothing), which
      // is the only reason this was not read as a broken harness.
      //
      // The startup `process.argv` call shares this path deliberately and is
      // unaffected: at cold start both the tag and the current owner are null,
      // so the fence and the bind are each a no-op there.
      it('fences an argv deep link from a second instance (Windows/Linux)', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        // Renderer not ready: A's code waits in the pending queue.
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // B signs in and clicks their own link, which reaches the running app
        // through second-instance rather than open-url.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        appOnCallbacks.get('second-instance')!({}, [
          'concord.exe',
          `concord://invite/${TEN_CODES[9]}`,
        ]);
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'an argv arrival is an OS arrival and must fence like any other'
        ).toEqual([TEN_CODES[9]]);
      });

      // Codex P2, main.ts:621, and the round-15 test below with ONE step added:
      // an unrelated reset between the re-click and the flood. That is the whole
      // finding. Round 15 split `reservedCode` from `carriedCode` so a re-click
      // keeps its exemption; the reset branch then read `carriedStillPending` --
      // false *because carriedCode is null* -- and cleared the reservation of a
      // code that was still queued, re-tying the two fields it had just split.
      it('keeps the eviction reservation across a reset that carries nothing', async () => {
        const { PENDING_DEEP_LINK_CAP } = await import('../../../src/main/main');
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // The re-click retires the carried marker and keeps the reservation.
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          // An ordinary reload that carries nothing -- the step round 15 lacked.
          simulateReload();

          const SAFE = 'ABCDEFGHJKMNPQRS';
          for (let i = 0; i < PENDING_DEEP_LINK_CAP; i += 1) {
            deliverDeepLink(`concord://invite/ZZZZZZB${SAFE[i % SAFE.length]}`);
          }

          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 40);

          const deliveries = sentOn('invite:received').filter((c) => c === INVITE_CODE);
          expect(
            deliveries.length,
            'a reset that carries nothing must not spend a reservation whose code is still queued'
          ).toBeGreaterThanOrEqual(2);
        } finally {
          vi.useRealTimers();
        }
      });

      // Codex P2, main.ts:702, and a fair hit on the laundering test above: it
      // asserts A's code is ABSENT and never that the fresh one SURVIVED.
      //
      // On a pre-v25 SPA the teardown leaves A's tag in place. A link clicked
      // while logged out could not clear it -- noteDeepLinkOwner refuses to write
      // null, deliberately -- so the fresh code joined A's queue under A's tag,
      // and when B's credential was minted the drain fence swept the whole queue
      // including the link the user had just opened.
      //
      // Arrival is the separating boundary, and it needs no per-entry ownership:
      // at a logged-out arrival everything ALREADY queued belongs to the retained
      // tag and the arriving code does not.
      it('a logged-out arrival keeps its own code while discarding the retained owner queue', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        // A's session ends on an old SPA: nothing tells main, so the tag stays 41.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        deliverDeepLink(`concord://invite/${TEN_CODES[9]}`);

        // B's credential is minted -- an SSO completion during a reload.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        signalInviteReady(rendererEvent);

        const delivered = sentOn('invite:received');
        expect(delivered, "A's retained code must still not cross").not.toContain(INVITE_CODE);
        expect(
          delivered,
          'but the link the user clicked while logged out is the one they are signing in to accept'
        ).toContain(TEN_CODES[9]);
      });

      // Codex P2, main.ts:713, and a fair hit on the delivery-bind test above:
      // that test mints the owner BEFORE signalling readiness, which is not the
      // order a cold start uses. App.tsx signals invite-readiness from a mount
      // effect keyed on [isPipWindow] -- unconditionally, before anyone signs in
      // -- and the authenticated replay that opens the modal is renderer-local
      // and never calls back into main. So the real startup order delivers the
      // code while the owner is still null, noteDeepLinkOwner declines to write,
      // and the tag stays null forever: the delivery bind fixed a case that does
      // not occur and left the one that does.
      //
      // Vacuity mode 7 -- sensitive to the mutation I imagined, blind to the
      // ordering that actually ships.
      it('binds a startup code when authentication later succeeds', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        // The REAL order: the renderer is ready before anyone has signed in.
        signalInviteReady(rendererEvent);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        // A signs in. Main learns of it here and nowhere else -- the renderer's
        // own replay into the modal is local and sends no IPC.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        await handlers.get('auth:storeRefreshToken')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          { refreshToken: 'tok', rememberMe: true, apiBase: 'http://localhost:8080' }
        );

        // A's teardown on an old SPA tells main nothing; B signs in; swap.
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 43,
          pendingE2EEUnlock: false,
        });
        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'a startup code the user signed in to accept belongs to that account afterwards'
        ).toEqual([INVITE_CODE]);
      });

      // The fence must run BEFORE the bind at every auth point, or signing in
      // ADOPTS the previous owner's retained queue instead of discarding it --
      // turning the remedy for a cross-account replay into a cause of one.
      it('a sign-in discards the previous owner queue rather than adopting it', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 41,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        // A's code waits in the queue; the renderer never became ready.
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        await handlers.get('auth:storeRefreshToken')!(
          { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } },
          { refreshToken: 'tok', rememberMe: true, apiBase: 'http://localhost:8080' }
        );
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          "signing in must not adopt the previous account's queued codes"
        ).toEqual([]);
      });

      // The restore path is the likelier of the two in practice: a remembered
      // session at launch is exactly when a startup deep link is waiting.
      it('binds a startup code when a remembered session is restored', async () => {
        const { getCredentialCustodyState } = await import('../../../src/main/tokenManager');
        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: null,
          pendingE2EEUnlock: false,
        });
        resetDeepLinkState();
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);
        signalInviteReady(rendererEvent);
        expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 42,
          pendingE2EEUnlock: false,
        });
        await handlers.get('auth:restoreSession')!({
          sender: { id: 1 },
          senderFrame: { url: 'app://concord/index.html' },
        });

        (getCredentialCustodyState as Mock).mockReturnValue({
          credentialOwner: 43,
          pendingE2EEUnlock: false,
        });
        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        expect(
          sentOn('invite:received'),
          'a restored session owns the startup code it was launched with'
        ).toEqual([INVITE_CODE]);
      });

      it('deeplink:forget rejects an untrusted sender frame', async () => {
        resetDeepLinkState();
        signalInviteReady(rendererEvent);
        deliverDeepLink(`concord://invite/${INVITE_CODE}`);

        await handlers.get('deeplink:forget')!(foreignIpcEvent);

        const releaseCarry = await armDeepLinkCarryForTest();
        simulateReload();
        releaseCarry();
        signalInviteReady(rendererEvent);

        // Refused, so the carry still works — a hostile frame cannot wipe a
        // pending invite even though forgetting is the fail-safe direction.
        expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
      });

      // CODEX P2 (round 15). Round 14 refreshed the re-clicked code's clock and
      // marker but left its single occurrence at index 0. Retiring the marker also
      // retires the eviction reservation, so the entry was then the oldest AND
      // unexempt — the next different link overflowed the cap and evicted the
      // fresh re-click. The fix honoured the action and then threw it away.
      // Moving the entry to the FIFO tail — the obvious remedy — does NOT work:
      // at re-click time it is usually the only entry, so the move is a no-op and
      // the later links still append after it. The reservation had to be split
      // from the replay marker instead.
      it('CODEX P2 (red-team): a refreshed re-click keeps its eviction reservation', async () => {
        const { PENDING_DEEP_LINK_CAP } = await import('../../../src/main/main');
        expect(PENDING_DEEP_LINK_CAP).toBeGreaterThan(0);
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // The user re-clicks the carried code while the successor is loading...
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          // ...then enough different links arrive to overflow the cap.
          const SAFE = 'ABCDEFGHJKMNPQRS';
          for (let i = 0; i < PENDING_DEEP_LINK_CAP; i += 1) {
            deliverDeepLink(`concord://invite/ZZZZZZB${SAFE[i % SAFE.length]}`);
          }

          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 40);

          const deliveries = sentOn('invite:received').filter((c) => c === INVITE_CODE);
          expect(
            deliveries.length,
            'the deliberate re-click must not be evicted as the oldest entry'
          ).toBeGreaterThanOrEqual(2);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 14). An interaction between two earlier fixes, neither
      // wrong alone. The pending-queue dedupe (round 11) discarded a duplicate of
      // the carried code; the drain-time expiry (round 13) then dropped the stale
      // original. Together, a user who re-opens the same link while the successor
      // is still loading got NOTHING — both fences firing on the same code and
      // cancelling each other out.
      it('CODEX P2 (red-team): a re-click behind a queued carried code still opens', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Carried and queued; the successor never signals ready.
          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // The user opens the same link again, deliberately.
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          // Long past the ORIGINAL delivery's window — the replay would expire.
          vi.advanceTimersByTime(DEEP_LINK_CARRY_MAX_AGE_MS * 2);
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 4);

          expect(
            sentOn('invite:received'),
            'the deliberate re-click must reach the renderer, not be eaten by the replay it queued behind'
          ).toEqual([INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 13). The age was checked where the code was QUEUED and
      // never again. Pending-queue residence is unbounded — the successor
      // renderer signals readiness when it is ready — so a stalled init left a
      // carried code sitting in the queue and the drain replayed it on the
      // strength of the marker alone. The fence passed once and was never asked
      // again, which is how a dismissed invite reappears hours later.
      it('CODEX P2 (red-team): a carried code that ages out IN the queue is not replayed', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Carried well inside the window, so it is queued...
          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // ...but the successor never signals ready until long after it expired.
          vi.advanceTimersByTime(DEEP_LINK_CARRY_MAX_AGE_MS * 3);
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 4);

          expect(
            sentOn('invite:received'),
            'a code that expired while queued must not be replayed on drain'
          ).toEqual([INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 11). The reservation matched by VALUE, so every copy of
      // the carried code was exempt. A page that knows that code — its own, if it
      // fired the link that got carried — could fill the queue with duplicates,
      // leaving a later legitimate link as the only non-reserved entry and thus
      // the immediate eviction victim: suppression rather than substitution.
      it('CODEX P2 (red-team): duplicates of the carried code cannot starve a later link', async () => {
        const { PENDING_DEEP_LINK_CAP } = await import('../../../src/main/main');
        expect(PENDING_DEEP_LINK_CAP).toBeGreaterThan(0);
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);

          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // Flood with copies of the CARRIED code, then one different, legitimate
          // link that must not be the one thrown away.
          for (let i = 0; i < PENDING_DEEP_LINK_CAP * 2; i += 1) {
            deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          }
          const LATER_CODE = 'ZZZZZZAB';
          deliverDeepLink(`concord://invite/${LATER_CODE}`);

          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 40);

          const sent = sentOn('invite:received');
          expect(sent, 'the carried invite still survives').toContain(INVITE_CODE);
          expect(sent, 'and so does the later legitimate one').toContain(LATER_CODE);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 7). F1 reserved the carried code in the HOLD buffer and
      // left the PENDING queue open — and the carried code is re-queued first, so
      // it is the oldest entry and the guaranteed drop-oldest victim once the cap
      // is exceeded while the successor document loads. Any page can fire that
      // burst, so the carry silently hands the user the attacker's invites in
      // place of their own: F1's exact defect, one queue over.
      it('CODEX P2 (red-team): a burst during the successor load cannot evict the carried code', async () => {
        const { PENDING_DEEP_LINK_CAP } = await import('../../../src/main/main');
        // The import is load-bearing and was silently undefined once: `i < NaN`
        // is false, so the burst never ran and this test passed on zero attacker
        // links. Assert the fixture before using it.
        expect(PENDING_DEEP_LINK_CAP).toBeGreaterThan(0);
        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();

          // The successor has NOT signalled ready, so everything queues behind
          // the carried code. Overflow the cap several times over.
          //
          // Codes must be EXACTLY 8 chars from deepLink.ts's alphabet (no
          // I/O/l/0/1). The first version of this burst used `ATTACKER00`-style
          // strings, every one of which the parser rejected — so the burst never
          // happened and the test passed against plain drop-oldest.
          const SAFE = 'ABCDEFGHJKMNPQRS';
          const burst = Array.from(
            { length: PENDING_DEEP_LINK_CAP * 2 },
            (_unused, i) => `ZZZZZZA${SAFE[i % SAFE.length]}`
          );
          for (const code of burst) deliverDeepLink(`concord://invite/${code}`);

          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 40);

          // COUNT, do not merely contain. The original pre-reload delivery is
          // already in this list, so `toContain` is satisfied whether or not the
          // carry survived — which is exactly how the first version of this test
          // passed against plain drop-oldest.
          const deliveries = sentOn('invite:received').filter((code) => code === INVITE_CODE);
          expect(
            deliveries.length,
            "the user's own invite must be re-delivered after a burst it was queued before"
          ).toBeGreaterThanOrEqual(2);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 6). Two main-owned swaps can overlap — the case the
      // depth count exists for — and the SECOND reset sees lastCode === null
      // because the first already cleared it. Clearing carriedCode there erased
      // the replay marker and the clock for a code still sitting in the pending
      // queue, so it drained as a "fresh" delivery and restarted its own window:
      // the recency bound defeated for the third time, by its own repair.
      it('CODEX P2 (red-team): a second reset before ready does not reset the carried clock', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Two resets, both BEFORE the successor signals ready. The first
          // carries; the second must not erase what the first recorded.
          const releaseA = await armDeepLinkCarryForTest();
          simulateReload();
          const releaseB = await armDeepLinkCarryForTest();
          simulateReload();
          releaseA();
          releaseB();

          // The timing is the whole test. The code sits in the pending queue for
          // most of the window, so the REPLAY is late while the ORIGINAL delivery
          // is later still. A version that restarts the clock at drain time then
          // measures from the replay and happily carries; one that keeps the
          // original clock is already past the fence. Draining and asserting at
          // the same instant — the first shape of this test — makes both versions
          // decline, and passes on the bug.
          const beforeDrain = Math.floor(DEEP_LINK_CARRY_MAX_AGE_MS * 0.8);
          const afterDrain = Math.floor(DEEP_LINK_CARRY_MAX_AGE_MS * 0.4);

          vi.advanceTimersByTime(beforeDrain);
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          expect(
            sentOn('invite:received'),
            'the carried code must still reach the renderer, or this proves nothing'
          ).toEqual([INVITE_CODE, INVITE_CODE]);

          // 1.2x the window since FIRST delivery, but only 0.4x since the replay.
          vi.advanceTimersByTime(afterDrain);
          const releaseC = await armDeepLinkCarryForTest();
          simulateReload();
          releaseC();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);

          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2 (round 5). The first version of the replay test asked "is this
      // the same code as last time", which conflated a REPLAY with a deliberate
      // RE-CLICK. A user who dismisses an invite and then opens the same link
      // again is making a fresh delivery; inheriting the old clock meant the new
      // modal was born stale and would not survive a swap — the carry declining
      // the very thing it exists to protect, one more time.
      it('CODEX P2 (red-team): a deliberate re-click starts a NEW recency window', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Long past the window: this code is stale and must not be carried...
          vi.advanceTimersByTimeAsync(0);
          vi.advanceTimersByTime(DEEP_LINK_CARRY_MAX_AGE_MS * 2);

          // ...but the user opens the SAME link again, deliberately.
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          expect(
            sentOn('invite:received'),
            'the re-click itself must reach the renderer, or this test proves nothing'
          ).toEqual([INVITE_CODE, INVITE_CODE]);

          const release = await armDeepLinkCarryForTest();
          simulateReload();
          release();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);

          // The re-click is fresh, so the swap must carry it.
          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });

      // CODEX P2. sendDeepLink refreshes gate.lastAt on EVERY delivery, a
      // carry's own replay included, so measuring the fence off lastAt made it a
      // SLIDING window: a dismissed attacker-supplied invite stayed eligible
      // forever as long as main-initiated swaps ("Load latest UI" clicks) kept
      // landing inside it. The clock must run from the FIRST delivery.
      it('CODEX P2 (red-team): the recency window runs from FIRST delivery, not from the last carry', async () => {
        const { DEEP_LINK_CARRY_MAX_AGE_MS } = await import('../../../src/main/main');

        resetDeepLinkState();
        vi.useFakeTimers();
        try {
          signalInviteReady(rendererEvent);
          deliverDeepLink(`concord://invite/${INVITE_CODE}`);
          expect(sentOn('invite:received')).toEqual([INVITE_CODE]);

          // Two hops, each well INSIDE the window on its own, together past it.
          const hop = Math.floor(DEEP_LINK_CARRY_MAX_AGE_MS * 0.6);

          vi.advanceTimersByTime(hop);
          let release = await armDeepLinkCarryForTest();
          simulateReload();
          release();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          // Positive control: the first carry is legitimate and MUST happen, or
          // the assertion below would pass on a carry that is simply broken.
          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);

          vi.advanceTimersByTime(hop);
          release = await armDeepLinkCarryForTest();
          simulateReload();
          release();
          signalInviteReady(rendererEvent);
          vi.advanceTimersByTime(DEEP_LINK_EMIT_WINDOW_MS * 2);
          // 1.2x the window has elapsed since FIRST delivery. Off lastAt the
          // replay would have restarted the clock and a third send would land.
          expect(sentOn('invite:received')).toEqual([INVITE_CODE, INVITE_CODE]);
        } finally {
          vi.useRealTimers();
        }
      });
    });
  });

  describe('createWindow callbacks', () => {
    it('ready-to-show shows the window', () => {
      const readyCall = mockMainWindow.once.mock.calls.find(
        (c: unknown[]) => c[0] === 'ready-to-show'
      );
      expect(readyCall).toBeDefined();
      readyCall![1](); // invoke the callback
      expect(mockMainWindow.show).toHaveBeenCalled();
    });

    it('closed handler tears down both SSO flows and nulls mainWindow', () => {
      const closedCall = mockMainWindow.on.mock.calls.find((c: unknown[]) => c[0] === 'closed');
      expect(closedCall).toBeDefined();
      closedCall![1](); // invoke — sets mainWindow = null internally
      // #974/#975: a window-less SSO flow has no UI for its result, so the
      // 'closed' handler must tear down BOTH provider flows (else the loopback
      // listener + 5-min deadline outlive the renderer). Regression guard for
      // the google-parity gap.
      expect(mockCancelAppleFlow).toHaveBeenCalled();
      expect(mockCancelGoogleFlow).toHaveBeenCalled();
    });

    it('window open handler allows https and denies others', async () => {
      const handlerCall = mockWebContents.setWindowOpenHandler.mock.calls[0];
      expect(handlerCall).toBeDefined();
      const handler = handlerCall[0] as (details: { url: string }) => { action: string };

      const { shell } = await import('electron');

      // HTTPS opens external
      (shell.openExternal as Mock).mockClear();
      const result1 = handler({ url: 'https://example.com' });
      expect(shell.openExternal).toHaveBeenCalledWith('https://example.com');
      expect(result1.action).toBe('deny');

      // Invalid URL is ignored
      (shell.openExternal as Mock).mockClear();
      const result2 = handler({ url: 'not-a-url' });
      expect(shell.openExternal).not.toHaveBeenCalled();
      expect(result2.action).toBe('deny');

      // javascript: protocol is blocked
      (shell.openExternal as Mock).mockClear();
      const result3 = handler({ url: 'javascript:alert(1)' });
      expect(shell.openExternal).not.toHaveBeenCalled();
      expect(result3.action).toBe('deny');

      // http: protocol is rejected (was accepted pre-#754 tightening)
      (shell.openExternal as Mock).mockClear();
      const result4 = handler({ url: 'http://example.com/foo' });
      expect(shell.openExternal).not.toHaveBeenCalled();
      expect(result4.action).toBe('deny');

      // data: URL is blocked
      (shell.openExternal as Mock).mockClear();
      const result5 = handler({ url: 'data:text/html,<script>alert(1)</script>' });
      expect(shell.openExternal).not.toHaveBeenCalled();
      expect(result5.action).toBe('deny');
    });
  });

  describe('navigation guard', () => {
    it('blocks external navigation in dev mode', async () => {
      const { app } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      expect(webContentsCreated).toBeDefined();

      // Invoke web-contents-created to get the will-navigate handler
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);

      const willNavigateCall = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      );
      expect(willNavigateCall).toBeDefined();
      const willNavigate = willNavigateCall![1] as (
        event: { preventDefault: () => void },
        url: string
      ) => void;

      // Localhost should be allowed (dev mode, isPackaged=false)
      const event1 = { preventDefault: vi.fn() };
      willNavigate(event1, 'http://localhost:3001/some/path');
      expect(event1.preventDefault).not.toHaveBeenCalled();

      // External URL should be blocked in dev
      const event2 = { preventDefault: vi.fn() };
      willNavigate(event2, 'https://evil.com/steal');
      expect(event2.preventDefault).toHaveBeenCalled();
    });

    it('externalizes https: navigation in dev mode after preventDefault', async () => {
      const { app } = await import('electron');
      const { shell } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);
      const willNavigate = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      )![1] as (event: { preventDefault: () => void }, url: string) => void;

      (shell.openExternal as Mock).mockClear();
      const event = { preventDefault: vi.fn() };
      willNavigate(event, 'https://external.example/foo');
      expect(event.preventDefault).toHaveBeenCalled();
      expect(shell.openExternal).toHaveBeenCalledWith('https://external.example/foo');
    });

    it('blocks but does not externalize http: navigation in dev mode', async () => {
      const { app } = await import('electron');
      const { shell } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);
      const willNavigate = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      )![1] as (event: { preventDefault: () => void }, url: string) => void;

      (shell.openExternal as Mock).mockClear();
      const event = { preventDefault: vi.fn() };
      willNavigate(event, 'http://external.example/foo');
      expect(event.preventDefault).toHaveBeenCalled();
      expect(shell.openExternal).not.toHaveBeenCalled();
    });

    it('blocks but does not externalize javascript: URL', async () => {
      const { app } = await import('electron');
      const { shell } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);
      const willNavigate = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      )![1] as (event: { preventDefault: () => void }, url: string) => void;

      (shell.openExternal as Mock).mockClear();
      const event = { preventDefault: vi.fn() };
      willNavigate(event, 'javascript:alert(1)');
      expect(event.preventDefault).toHaveBeenCalled();
      expect(shell.openExternal).not.toHaveBeenCalled();
    });

    it('blocks but does not externalize data: URL', async () => {
      const { app } = await import('electron');
      const { shell } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);
      const willNavigate = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      )![1] as (event: { preventDefault: () => void }, url: string) => void;

      (shell.openExternal as Mock).mockClear();
      const event = { preventDefault: vi.fn() };
      willNavigate(event, 'data:text/html,<script>alert(1)</script>');
      expect(event.preventDefault).toHaveBeenCalled();
      expect(shell.openExternal).not.toHaveBeenCalled();
    });

    it('does not throw on malformed URL — preventDefault called, no externalization', async () => {
      const { app } = await import('electron');
      const { shell } = await import('electron');
      const webContentsCreated = (app.on as Mock).mock.calls.find(
        (c: unknown[]) => c[0] === 'web-contents-created'
      );
      const mockContents = { on: vi.fn() };
      webContentsCreated![1]({}, mockContents);
      const willNavigate = mockContents.on.mock.calls.find(
        (c: unknown[]) => c[0] === 'will-navigate'
      )![1] as (event: { preventDefault: () => void }, url: string) => void;

      (shell.openExternal as Mock).mockClear();
      const event = { preventDefault: vi.fn() };
      // Should not throw
      expect(() => willNavigate(event, '')).not.toThrow();
      expect(event.preventDefault).toHaveBeenCalled();
      expect(shell.openExternal).not.toHaveBeenCalled();
    });

    it('packaged mode (no SPA loaded): externalizes https navigation after preventDefault', async () => {
      const electron = await import('electron');
      const { app, shell } = electron;
      // Temporarily flip isPackaged to exercise packaged-mode branch.
      // The mock's `isPackaged` is a plain boolean property, so direct
      // mutation works (no vi.spyOn needed).
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      try {
        // Re-derive the will-navigate handler under packaged mode.
        // Note: web-contents-created callbacks were registered at module
        // init time (before isPackaged was flipped) — but the handler
        // body reads app.isPackaged at runtime, so the flip takes effect
        // when the handler fires.
        const webContentsCreated = (app.on as Mock).mock.calls.find(
          (c: unknown[]) => c[0] === 'web-contents-created'
        );
        const mockContents = { on: vi.fn() };
        webContentsCreated![1]({}, mockContents);
        const willNavigate = mockContents.on.mock.calls.find(
          (c: unknown[]) => c[0] === 'will-navigate'
        )![1] as (event: { preventDefault: () => void }, url: string) => void;

        // Note: remoteSpaBaseUrl is null in test mock because the file-level
        // spaLoader mock returns {mode: 'bundled'} (see line ~224). With no
        // SPA origin set, the SPA-origin early-return path at main.ts L864
        // is unreachable from this test fixture. The test below verifies
        // that with no SPA loaded, packaged-mode externalizes https as
        // expected. Coverage of the SPA-origin allow-path itself would
        // require either a test-only setter export from main.ts or full
        // mock re-isolation via vi.resetModules — both add complexity
        // disproportionate to the value, since the allow-path is
        // structurally equivalent to the dev-mode `localhost` early-return
        // exercised by the dev-mode tests above. Documented as #775's
        // sibling concern; revisit if the allow-path semantics ever diverge
        // from the localhost path.
        (shell.openExternal as Mock).mockClear();
        const event = { preventDefault: vi.fn() };
        willNavigate(event, 'https://external.example/foo');
        expect(event.preventDefault).toHaveBeenCalled();
        expect(shell.openExternal).toHaveBeenCalledWith('https://external.example/foo');
      } finally {
        (app as unknown as { isPackaged: boolean }).isPackaged = false;
      }
    });

    it('packaged mode: blocks but does not externalize http: URL', async () => {
      const electron = await import('electron');
      const { app, shell } = electron;
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      try {
        const webContentsCreated = (app.on as Mock).mock.calls.find(
          (c: unknown[]) => c[0] === 'web-contents-created'
        );
        const mockContents = { on: vi.fn() };
        webContentsCreated![1]({}, mockContents);
        const willNavigate = mockContents.on.mock.calls.find(
          (c: unknown[]) => c[0] === 'will-navigate'
        )![1] as (event: { preventDefault: () => void }, url: string) => void;

        (shell.openExternal as Mock).mockClear();
        const event = { preventDefault: vi.fn() };
        willNavigate(event, 'http://external.example/foo');
        expect(event.preventDefault).toHaveBeenCalled();
        expect(shell.openExternal).not.toHaveBeenCalled();
      } finally {
        (app as unknown as { isPackaged: boolean }).isPackaged = false;
      }
    });

    it('packaged mode: handles malformed URL without throwing', async () => {
      const electron = await import('electron');
      const { app, shell } = electron;
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      try {
        const webContentsCreated = (app.on as Mock).mock.calls.find(
          (c: unknown[]) => c[0] === 'web-contents-created'
        );
        const mockContents = { on: vi.fn() };
        webContentsCreated![1]({}, mockContents);
        const willNavigate = mockContents.on.mock.calls.find(
          (c: unknown[]) => c[0] === 'will-navigate'
        )![1] as (event: { preventDefault: () => void }, url: string) => void;

        (shell.openExternal as Mock).mockClear();
        const event = { preventDefault: vi.fn() };
        expect(() => willNavigate(event, 'not-a-valid-url-at-all')).not.toThrow();
        expect(event.preventDefault).toHaveBeenCalled();
        expect(shell.openExternal).not.toHaveBeenCalled();
      } finally {
        (app as unknown as { isPackaged: boolean }).isPackaged = false;
      }
    });

    // #1870 Finding D: the signed LKG cache serves from spa-cache://concord and
    // (like bundled app://concord) leaves the SPA origin empty. A same-origin
    // full-page reload within the cache (error-boundary "Reload", crash recovery)
    // must be ALLOWED through the packaged will-navigate gate; without the branch
    // it would hit preventDefault() and strand the user in the very degraded state
    // the cache exists for. These mirror the app://concord same-origin allow case
    // and prove the new branch did NOT widen the gate to other origins / hosts.
    it('packaged mode: ALLOWS same-origin spa-cache://concord navigation (no preventDefault)', async () => {
      const electron = await import('electron');
      const { app, shell } = electron;
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      try {
        const webContentsCreated = (app.on as Mock).mock.calls.find(
          (c: unknown[]) => c[0] === 'web-contents-created'
        );
        const mockContents = { on: vi.fn() };
        webContentsCreated![1]({}, mockContents);
        const willNavigate = mockContents.on.mock.calls.find(
          (c: unknown[]) => c[0] === 'will-navigate'
        )![1] as (event: { preventDefault: () => void }, url: string) => void;

        (shell.openExternal as Mock).mockClear();
        const event = { preventDefault: vi.fn() };
        willNavigate(event, 'spa-cache://concord/index.html');
        // Allowed in-window: the handler returns WITHOUT preventDefault, and does
        // not externalize to the OS browser.
        expect(event.preventDefault).not.toHaveBeenCalled();
        expect(shell.openExternal).not.toHaveBeenCalled();
      } finally {
        (app as unknown as { isPackaged: boolean }).isPackaged = false;
      }
    });

    it('packaged mode: BLOCKS spa-cache:// with a non-concord host (host-exactness)', async () => {
      const electron = await import('electron');
      const { app, shell } = electron;
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      try {
        const webContentsCreated = (app.on as Mock).mock.calls.find(
          (c: unknown[]) => c[0] === 'web-contents-created'
        );
        const mockContents = { on: vi.fn() };
        webContentsCreated![1]({}, mockContents);
        const willNavigate = mockContents.on.mock.calls.find(
          (c: unknown[]) => c[0] === 'will-navigate'
        )![1] as (event: { preventDefault: () => void }, url: string) => void;

        // spa-cache://evil and spa-cache://concord.evil are NOT the exact host
        // SPA_CACHE_HOST ('concord'), so the allow branch must not match → blocked.
        for (const url of ['spa-cache://evil/index.html', 'spa-cache://concord.evil/index.html']) {
          (shell.openExternal as Mock).mockClear();
          const event = { preventDefault: vi.fn() };
          willNavigate(event, url);
          expect(event.preventDefault).toHaveBeenCalled();
          // Non-https scheme → silently dropped after preventDefault (no externalize).
          expect(shell.openExternal).not.toHaveBeenCalled();
        }
      } finally {
        (app as unknown as { isPackaged: boolean }).isPackaged = false;
      }
    });
  });

  describe('second-instance handler', () => {
    it('focuses existing window', () => {
      const secondInstance = appOnCallbacks.get('second-instance');
      expect(secondInstance).toBeDefined();
      // Note: mainWindow might be null from the 'closed' test above,
      // but the handler checks for it gracefully
      secondInstance!();
      // Just verify it doesn't throw
    });
  });

  describe('restoreSession deduplication', () => {
    it('dedup cache returns same promise on repeated calls', async () => {
      // The restoreSession handler caches its promise — calling it twice
      // should return the same result without re-invoking performRefresh.
      const handler = handlers.get('auth:restoreSession')!;
      const event = { sender: { id: 1 }, senderFrame: { url: 'app://concord/index.html' } };
      const result1 = await handler(event);
      const result2 = await handler(event);
      expect(result1).toEqual(result2);
    });

    it('restoreSession handler exists and returns an object', async () => {
      const handler = handlers.get('auth:restoreSession')!;
      const result = await handler({
        sender: { id: 1 },
        senderFrame: { url: 'app://concord/index.html' },
      });
      expect(result).toHaveProperty('status');
    });
  });

  describe('IPC handler registration', () => {
    it('registers all expected IPC channels', () => {
      const expected = [
        'app:getVersion',
        'app:getPlatform',
        'app:getIpcContract',
        'app:getSystemInfo',
        'app:getHardwareAcceleration',
        'app:setHardwareAcceleration',
        'app:relaunch',
        'app:quit',
        'media:getDesktopSources',
        'clipboard:writeText',
        'gpu:getInfo',
        'screen:getDisplayInfo',
        'auth:storeRefreshToken',
        'auth:restoreSession',
        'auth:storeE2EEKeys',
        'auth:storeE2EEKeysIfOwner',
        'auth:refreshToken',
        'auth:logout',
        'auth:clearTokens',
        'auth:clearTokensIfOwner',
        'auth:getCapabilities',
        'auth:getMachineId',
        'update:check',
        'update:download',
        'update:install',
        'update:getAllowPrerelease',
        'update:setAllowPrerelease',
        'update:getLogPath',
        'pip:open',
        'pip:close',
        'pip:setAlwaysOnTop',
      ];
      for (const channel of expected) {
        expect(handlers.has(channel), `missing handler: ${channel}`).toBe(true);
      }
    });
  });

  // ── KLIPY media proxy webRequest interceptor (#626) ──────────────────

  describe('KLIPY media proxy interceptor', () => {
    it('registers onBeforeSendHeaders with the correct URL filter', () => {
      expect(klipyInterceptor).not.toBeNull();
      expect(klipyInterceptor!.filter.urls).toEqual(['*://*/api/v1/klipy/media*']);
    });

    it('injects Authorization header when token and matching origin are available', async () => {
      const tokenManager = await import('../../../src/main/tokenManager');
      (tokenManager.getCachedAccessToken as Mock).mockReturnValueOnce('test-jwt-token');
      (tokenManager.getApiBaseOrigin as Mock).mockReturnValueOnce('http://localhost:8080');

      const details = {
        url: 'http://localhost:8080/api/v1/klipy/media?url=https%3A%2F%2Fstatic.klipy.com%2Fimg.gif',
        requestHeaders: {} as Record<string, string>,
      };
      const callback = vi.fn();

      klipyInterceptor!.callback(details, callback);

      expect(callback).toHaveBeenCalledWith({
        requestHeaders: { Authorization: 'Bearer test-jwt-token' },
      });
    });

    it('does not inject header when token is null (pre-login)', async () => {
      const tokenManager = await import('../../../src/main/tokenManager');
      (tokenManager.getCachedAccessToken as Mock).mockReturnValueOnce(null);

      const details = {
        url: 'http://localhost:8080/api/v1/klipy/media?url=https%3A%2F%2Fstatic.klipy.com%2Fimg.gif',
        requestHeaders: {} as Record<string, string>,
      };
      const callback = vi.fn();

      klipyInterceptor!.callback(details, callback);

      expect(callback).toHaveBeenCalledWith({ requestHeaders: {} });
    });

    it('does not inject header when request origin does not match API base', async () => {
      const tokenManager = await import('../../../src/main/tokenManager');
      (tokenManager.getCachedAccessToken as Mock).mockReturnValueOnce('test-jwt-token');
      (tokenManager.getApiBaseOrigin as Mock).mockReturnValueOnce('http://localhost:8080');

      const details = {
        url: 'https://attacker.tld/api/v1/klipy/media?steal=true',
        requestHeaders: {} as Record<string, string>,
      };
      const callback = vi.fn();

      klipyInterceptor!.callback(details, callback);

      expect(callback).toHaveBeenCalledWith({ requestHeaders: {} });
      expect(details.requestHeaders['Authorization']).toBeUndefined();
    });

    it('does not inject header when API base origin is null', async () => {
      const tokenManager = await import('../../../src/main/tokenManager');
      (tokenManager.getCachedAccessToken as Mock).mockReturnValueOnce('test-jwt-token');
      (tokenManager.getApiBaseOrigin as Mock).mockReturnValueOnce(null);

      const details = {
        url: 'http://localhost:8080/api/v1/klipy/media?url=test',
        requestHeaders: {} as Record<string, string>,
      };
      const callback = vi.fn();

      klipyInterceptor!.callback(details, callback);

      expect(callback).toHaveBeenCalledWith({ requestHeaders: {} });
    });
  });

  describe('SPA reload IPC (spa:reloadLatest / spa:checkForUpdate)', () => {
    it('spa:reloadLatest rejects an untrusted sender frame and does NOT navigate', async () => {
      // The renderer never supplies a URL; an untrusted frame must be refused at
      // the boundary FIRST — before any state check, resolveSpaSource, or loadURL.
      (mockMainWindow.loadURL as Mock).mockClear();
      const result = await handlers.get('spa:reloadLatest')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({ mode: 'bundled', changed: false, rejected: true });
      expect(mockMainWindow.loadURL).not.toHaveBeenCalled();
    });

    it('spa:reloadLatest is inert (no navigation) in dev/unpackaged mode', async () => {
      const { app } = await import('electron');
      (app as unknown as { isPackaged: boolean }).isPackaged = false;
      (mockMainWindow.loadURL as Mock).mockClear();
      const result = await handlers.get('spa:reloadLatest')!({
        senderFrame: { url: 'app://concord/index.html' },
      });
      expect(result).toEqual({ mode: 'bundled', changed: false });
      expect(mockMainWindow.loadURL).not.toHaveBeenCalled();
    });

    it('spa:reloadLatest loads remote SPA with no-cache headers in packaged mode', async () => {
      const { app } = await import('electron');
      const { BrowserWindow } = await import('electron');
      const { resolveSpaSource } = await import('../../../src/main/spaLoader');
      (app as unknown as { isPackaged: boolean }).isPackaged = true;
      (
        (BrowserWindow as unknown as { getAllWindows: Mock }).getAllWindows as Mock
      ).mockReturnValueOnce([]);
      appOnCallbacks.get('activate')!();
      (resolveSpaSource as unknown as Mock).mockResolvedValueOnce({
        mode: 'remote',
        url: 'https://spa.concordvoice.chat/index.html',
        reason: 'remote SPA compatible',
      });
      (mockMainWindow.loadURL as Mock).mockClear();

      const result = await handlers.get('spa:reloadLatest')!({
        senderFrame: { url: 'app://concord/index.html' },
      });

      expect(mockMainWindow.loadURL).toHaveBeenCalledWith(
        'https://spa.concordvoice.chat/index.html',
        expect.objectContaining({
          extraHeaders: expect.stringContaining('Cache-Control: no-cache'),
        })
      );
      expect(result).toEqual({ mode: 'remote', changed: false });
    });

    it('spa:checkForUpdate rejects an untrusted sender frame', async () => {
      const result = await handlers.get('spa:checkForUpdate')!({
        senderFrame: { url: 'https://evil.example/' },
      });
      expect(result).toEqual({
        currentMode: 'bundled',
        remoteAvailable: false,
        newerBytesAvailable: null,
        reason: 'rejected',
      });
    });

    it('spa:checkForUpdate returns an inert result in dev/unpackaged mode (permitted frame)', async () => {
      const { app } = await import('electron');
      (app as unknown as { isPackaged: boolean }).isPackaged = false;
      const result = await handlers.get('spa:checkForUpdate')!({
        senderFrame: { url: 'app://concord/index.html' },
      });
      expect(result).toEqual({
        currentMode: 'remote',
        remoteAvailable: false,
        newerBytesAvailable: null,
        reason: 'dev mode',
      });
    });
  });

  describe('bundled SPA load failure visibility', () => {
    function getDidFailLoadHandler(): CallbackFn {
      const webContentsCreated = appOnCallbacks.get('web-contents-created');
      expect(webContentsCreated).toBeDefined();
      const mockContents = { on: vi.fn() };
      webContentsCreated!({}, mockContents);

      const call = mockContents.on.mock.calls.find(([event]) => event === 'did-fail-load');
      expect(call).toBeTruthy();
      return call![1] as CallbackFn;
    }

    it('reveals a real main-frame bundled app load failure', () => {
      mockRevealLoadFailure.mockClear();

      getDidFailLoadHandler()({}, -6, 'ERR_FILE_NOT_FOUND', 'app://concord/index.html', true);

      expect(mockRevealLoadFailure).toHaveBeenCalledOnce();
      expect(mockRevealLoadFailure.mock.calls[0]?.[1]).toBe(
        'Could not load application — please reinstall'
      );
    });

    it('does not reveal ERR_ABORTED app:// failures', () => {
      mockRevealLoadFailure.mockClear();

      getDidFailLoadHandler()({}, -3, 'ERR_ABORTED', 'app://concord/index.html', true);

      expect(mockRevealLoadFailure).not.toHaveBeenCalled();
    });
  });
});
