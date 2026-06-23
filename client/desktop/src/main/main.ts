// MUST be the first import: pins userData before tokenManager/machineId/updater
// capture app.getPath('userData') at module-load time. See pinUserDataPath.ts.
import './pinUserDataPath';
import { registerOpenExternalHandler } from './ipc/openExternal';
import { registerSaveImageHandler } from './ipc/saveImage';
import { registerSSOIPC } from './ipc/sso';
import { cancelActiveAppleFlow } from './oauth/apple/appleFlow';
import { cancelActiveGoogleFlow } from './oauth/google/googleFlow';
import { registerAttestationIpc } from './ipc/attestation';
import { registerWindowControlsIpc, getCachedClientBehavior } from './ipc/windowControls';
import { initTray, destroyTray, isTrayActive } from './tray';
import { registerVersionInfoIpc } from './ipc/versionInfo';
import { buildBrowserWindowConfig } from './browserWindowConfig';
import { revealLoadFailure } from './loadFailureVisibility';
import { loadWindowState, attachWindowState } from './windowState';
import { isWayland } from './waylandDetect';
import { deriveCloseAction, deriveMinimizeAction } from '../shared/clientBehavior';
import {
  app,
  BrowserWindow,
  clipboard,
  desktopCapturer,
  ipcMain,
  nativeImage,
  net,
  powerMonitor,
  protocol,
  screen,
  session,
  shell,
} from 'electron';
import path from 'node:path';
import { pathToFileURL } from 'node:url';
import fs from 'node:fs';
import {
  storeRefreshToken,
  restoreRefreshToken,
  performRefresh,
  performLogout,
  clearTokens,
  getCapabilities,
  storeE2EEKeys,
  restoreE2EEKeys,
  setProactiveRefreshCallback,
  onSystemResume,
  getCachedAccessToken,
  getApiBaseOrigin,
} from './tokenManager';
import { getMachineId } from './machineId';
import {
  initAutoUpdater,
  stopAutoUpdater,
  setUpdateFeedUrl,
  checkForUpdates,
  forceCheckForUpdates,
  downloadUpdate,
  safeQuitAndInstall,
  getAllowPrerelease,
  setAllowPrerelease,
  getUpdateLogger,
  getUpdateLogPath,
} from './updater';
import { createPinningVerifyProc } from './updatePinning';
import { PIN_CONFIG } from './updatePinningConfig';
import { getBuildTag } from './buildInfo';
import {
  checkUpdateSentinel,
  finalizeUpdate,
  finalizeRollback,
  runDeferredCleanup,
  type SentinelResult,
} from './updateSafety';
import { resolveAppProtocolPath } from './appProtocol';
import {
  resolveSpaSource,
  isUnexpectedBundled,
  captureSpaHash,
  hashEntryHtml,
  type SpaLoadDecision,
} from './spaLoader';
import { handleDidFailLoad, handleSpaRequestSelfHeal } from './spaSelfHealMainFrame';
import { buildRemotePipUrl, isValidPipOpenSender } from './pipUrl';
import { extractInviteDeepLinkFromArgv, normalizeInviteDeepLink } from './deepLink';
import {
  getRemoteSpaBaseDir,
  getRemoteSpaBaseUrl,
  getRemoteSpaUrl,
  getSpaHash,
  setRemoteSpaState,
} from './spaState';
import { isPermittedFrameUrl } from './ipc/frameValidation';
import { IPC_CONTRACT_VERSION } from './ipcContract';
import { registerIpcHandlers as registerPermissionHandlers } from './permissionManager';
import { migrateUserData } from './userDataMigration';
import { showSplash, closeSplash, updateSplashError } from './splashWindow';

// One-time migration: consolidate any legacy userData tree into the pinned
// "ConcordVoice" dir (#1291). Runs after pinUserDataPath.ts has set the path,
// before any userData read below.
//
// Note: this runs before the single-instance lock (acquired later, in
// app.whenReady). A rare concurrent double-launch could race two migrations,
// but every fs op is guarded + idempotent and archives rather than deletes, and
// promoteLegacy rolls back a failed move — so the worst case is a caught warn,
// never data loss (#1314 review, Gitar). Acquiring the lock this early would
// reorder unrelated startup and isn't worth the risk.
migrateUserData();

// Hardware acceleration preference — must be checked before app.whenReady()
const hwAccelPrefPath = path.join(app.getPath('userData'), 'hw-accel.json');
function readHwAccelPref(): boolean {
  try {
    const data = JSON.parse(fs.readFileSync(hwAccelPrefPath, 'utf-8'));
    return data.enabled !== false;
  } catch {
    return true; // Default: enabled
  }
}

// Developer Mode preference (#TBD) — gates DevTools access in packaged builds.
// MUST be removed before BETA release per security review.
const devModePrefPath = path.join(app.getPath('userData'), 'developer-mode.json');
function readDeveloperModePref(): boolean {
  try {
    const data = JSON.parse(fs.readFileSync(devModePrefPath, 'utf-8'));
    return data.enabled === true;
  } catch {
    return false;
  }
}
function writeDeveloperModePref(enabled: boolean): void {
  try {
    fs.writeFileSync(devModePrefPath, JSON.stringify({ enabled }), 'utf-8');
  } catch (err) {
    console.error('[DeveloperMode] Failed to persist preference:', (err as Error).message);
  }
}

if (readHwAccelPref()) {
  // Hardware acceleration flags for video encode/decode (IGNIS insight: fastest preset wins)
  app.commandLine.appendSwitch(
    'enable-features',
    'AcceleratedVideoEncoder,AcceleratedVideoDecodeLinuxGL'
  );
} else {
  app.disableHardwareAcceleration();
}

// Allow autoplay for all media — Concord is a real-time communication app that
// dynamically creates <audio>/<video> elements for voice/video calls.  The default
// 'document-user-activation-required' policy can silently block these elements.
app.commandLine.appendSwitch('autoplay-policy', 'no-user-gesture-required');

// Handle creating/removing shortcuts on Windows when installing/uninstalling
if (require('electron-squirrel-startup')) {
  app.quit();
}

// app:// scheme registration (#830) — the bundled-fallback renderer loads
// from app://concord/index.html instead of file:// so it has a non-null
// Origin header that the server's CORS allowlist can match. Must run
// BEFORE app.whenReady per Electron API contract. Only registered for
// packaged builds; dev mode uses the existing Vite + file fallback.
if (app.isPackaged) {
  protocol.registerSchemesAsPrivileged([
    {
      scheme: 'app',
      privileges: {
        standard: true,
        secure: true,
        supportFetchAPI: true,
        corsEnabled: true,
      },
    },
  ]);
}

let mainWindow: BrowserWindow | null = null;
let inviteRendererReady = false;
const pendingInviteCodes: string[] = [];

function registerInviteProtocolClient(): void {
  if (!app.isPackaged) return;
  if (typeof app.setAsDefaultProtocolClient !== 'function') return;
  try {
    if (process.platform === 'win32') {
      app.setAsDefaultProtocolClient('concord', process.execPath, []);
    } else {
      app.setAsDefaultProtocolClient('concord');
    }
  } catch {
    console.warn('[DeepLink] protocol registration failed');
  }
}

function emitInviteReceived(code: string): boolean {
  if (!mainWindow || mainWindow.isDestroyed()) return false;
  if (!inviteRendererReady) return false;
  mainWindow.webContents.send('invite:received', { code });
  return true;
}

function queueInviteCode(code: string): void {
  if (!emitInviteReceived(code)) pendingInviteCodes.push(code);
}

function drainPendingInviteCodes(): void {
  const codes = pendingInviteCodes.splice(0);
  for (const code of codes) {
    queueInviteCode(code);
  }
}

function handleInviteDeepLink(raw: string | undefined, source: string): void {
  const result = normalizeInviteDeepLink(raw);
  if (result.ok) {
    queueInviteCode(result.code);
    return;
  }
  if (result.reason !== 'empty') {
    console.warn('[DeepLink] rejected invite deep link', 'source', source, 'reason', result.reason);
  }
}

function handleInviteDeepLinksFromArgv(argv: readonly string[] | undefined, source: string): void {
  const result = extractInviteDeepLinkFromArgv(argv);
  if (result.ok) {
    queueInviteCode(result.code);
  } else if (result.reason !== 'empty') {
    console.warn('[DeepLink] rejected invite argv', 'source', source, 'reason', result.reason);
  }
}

// Set true by the before-quit handler so the [X] close intercept can
// distinguish a genuine app quit (⌘Q, Dock→Quit, window:quit/app:quit, the
// updater relaunch) from the user clicking the window close button. Without
// this guard the intercept's preventDefault() cancels Electron's app.quit()
// sequence and the app becomes unquittable — force-quit only (#1383).
let isQuitting = false;
// SPA-load state lives in ./spaState so spaSelfHeal.ts can mutate it on
// fallback/recovery paths. See that module for the lockstep invariant and
// reader/writer enumeration.

/**
 * Probe the Vite dev server, then load it. Falls back to the bundled
 * renderer on any failure (timeout, ECONNREFUSED, etc.). Extracted from
 * createWindow to keep cognitive complexity in check.
 */
async function loadDevRendererWithFallback(
  window: BrowserWindow,
  bundledPath: string
): Promise<void> {
  const devServerUrl = 'http://localhost:3001';
  const http = await import('node:http');
  const httpGet = (http.default?.get ?? http.get) as typeof import('node:http').get;
  try {
    await new Promise<void>((resolve, reject) => {
      const req = httpGet(devServerUrl, { timeout: 1000 }, (res) => {
        res.resume();
        resolve();
      });
      req.on('error', reject);
      req.on('timeout', () => {
        req.destroy();
        reject(new Error('timeout'));
      });
    });
    await window.loadURL(devServerUrl);
  } catch {
    await window.loadFile(bundledPath);
  }
}

/**
 * Apply a resolved SPA decision to a live window. Sets the remote-SPA state
 * BEFORE loadURL — load-bearing so the will-navigate gate, PiP, openExternal,
 * SSO, and versionInfo origin consumers key on the ACTUAL loaded origin — then
 * navigates and best-effort-captures the entry HTML hash. Fails closed to the
 * bundled app:// scheme on any remote-load failure. Shared by the launch path
 * (loadPackagedRenderer) and the runtime `spa:reloadLatest` handler so the two
 * can never drift. Returns the mode actually loaded.
 */
async function applySpaDecision(
  window: BrowserWindow,
  decision: SpaLoadDecision
): Promise<'remote' | 'bundled'> {
  if (decision.mode === 'remote' && decision.url) {
    try {
      setRemoteSpaState(decision.url);
      await window.loadURL(decision.url);
      // Capture the entry HTML hash for client attestation (#677). Best-effort:
      // captureSpaHash never throws, so this cannot break the load path.
      await captureSpaHash('remote', decision.url);
      return 'remote';
    } catch {
      console.warn('[SpaLoader] Failed to load remote SPA, falling back to bundled');
      setRemoteSpaState(null);
    }
  }
  // Bundled-mode load: clear state so PiP and origin-only consumers see a
  // consistent "not in remote-SPA mode" signal even on re-entry.
  setRemoteSpaState(null);
  // #830: load bundled via app:// scheme so renderer has a non-null Origin.
  // Bundled is the terminal load layer, so a failure here would leave a blank
  // window with no further fallback — surface it to the splash error overlay.
  try {
    await window.loadURL('app://concord/index.html');
    // Covers genuine bundled mode AND the remote→bundled fallback, so the
    // effective mode is always reflected accurately. Best-effort: never throws.
    await captureSpaHash('bundled');
  } catch (err) {
    console.error('[SpaLoader] bundled app:// loadURL failed:', (err as Error).message);
    revealLoadFailure(window, 'Could not load application — please reinstall');
  }
  return 'bundled';
}

// #1742 follow-up: the SPA-source decision is made once at launch with a 5s
// config-fetch timeout (spaLoader CONFIG_TIMEOUT_MS) and no retry. A cold-start
// network (DNS/TLS/CF edge not yet warm) loses that race and strands the client
// on the bundled SPA for the whole session. When the bundled fallback was
// UNEXPECTED (config fetch failed — not a logged-out / no-spaUrl / contract
// case), retry resolveSpaSource a few seconds later, on a now-warm network, and
// switch to the remote SPA if it resolves. Bounded; stops on success or once
// the delays are exhausted. The manual "Load latest UI" button (spa:reloadLatest)
// remains the fallback. Mirrors the WebSocket onlineFallbackTimer pattern (#1768).
const SPA_RETRY_DELAYS_MS = [4_000, 10_000];

function scheduleSpaSourceRetry(window: BrowserWindow, attempt = 0): void {
  if (attempt >= SPA_RETRY_DELAYS_MS.length) return;
  setTimeout(() => {
    void (async () => {
      // Abort if the window is gone, or if we are already in remote mode (a
      // prior retry or a manual reload succeeded — getRemoteSpaUrl is non-null
      // only when the remote SPA is loaded).
      if (window.isDestroyed() || getRemoteSpaUrl() !== null) return;
      const decision = await resolveSpaSource();
      if (decision.mode === 'remote' && decision.url) {
        console.debug('[SpaLoader/retry] remote SPA reachable on warm network — switching');
        // Use the EFFECTIVE mode: resolveSpaSource can return remote (config
        // host reachable) while the remote SPA host itself is still down, in
        // which case applySpaDecision falls back to bundled. Only stop retrying
        // when we actually reached remote; otherwise keep retrying.
        const mode = await applySpaDecision(window, decision);
        if (mode === 'remote') return;
      }
      scheduleSpaSourceRetry(window, attempt + 1);
    })();
  }, SPA_RETRY_DELAYS_MS[attempt]);
}

/**
 * Load the packaged renderer: try the remote SPA first (Tier 3), fall back
 * to the bundled file on any failure. Extracted to keep createWindow simple.
 */
async function loadPackagedRenderer(window: BrowserWindow, _bundledPath: string): Promise<void> {
  const decision = await resolveSpaSource();
  console.debug(`[SpaLoader] ${decision.mode}: ${decision.reason}`);
  await applySpaDecision(window, decision);

  // #830 Option C: surface a non-blocking diagnostic event if the bundled
  // fallback fired for an unexpected reason (config fetch failed, network
  // issue, spaUrl rejected, etc.). Expected fallbacks (first launch, no
  // spaUrl, contract zero) do NOT trigger it. The renderer shows the
  // "Could not reach Concord servers" banner on app:configFetchFailed.
  // Delay 2000ms to ensure renderer listeners are registered.
  if (decision.mode === 'bundled' && isUnexpectedBundled(decision.reason)) {
    setTimeout(() => {
      // Guard against window destroyed during the 2s delay (rapid quit, crash).
      if (window.isDestroyed()) return;
      window.webContents.send('app:configFetchFailed', {
        reason: 'Could not reach Concord servers',
      });
    }, 2000);
    // Self-heal the cold-start race: retry on a warm network and switch to
    // remote if it becomes reachable (#1742 follow-up launch-time retry).
    scheduleSpaSourceRetry(window);
  }
}

const createWindow = async (): Promise<void> => {
  // Build the per-platform BrowserWindow config (#806). Pure factory so
  // each platform gets the correct titleBarStyle / titleBarOverlay shape:
  //   - darwin → hiddenInset (preserves traffic-light controls)
  //   - win32 / linux → hidden + titleBarOverlay (native min/max/close)
  const baseConfig = buildBrowserWindowConfig({
    platform: process.platform,
    isWayland: isWayland(),
    preloadPath: path.join(__dirname, '../preload/preload.js'),
    isPackaged: app.isPackaged,
  });

  // Restore the user's last window placement (#806). loadWindowState returns
  // the saved bounds if window-state.json exists and the validator accepts
  // them; otherwise defaults to centered (x/y undefined → OS default placement).
  const savedState = loadWindowState();

  mainWindow = new BrowserWindow({
    ...baseConfig,
    x: savedState.x,
    y: savedState.y,
    width: savedState.width,
    height: savedState.height,
  });
  inviteRendererReady = false;

  if (savedState.isMaximized) {
    mainWindow.maximize();
  }

  mainWindow.webContents.on('did-start-loading', () => {
    inviteRendererReady = false;
  });

  // Wire resize/move/maximize/unmaximize/close listeners that persist
  // bounds to window-state.json with 500ms debounce (#806).
  attachWindowState(mainWindow);

  // Register the window-control + version-info IPC surfaces (#806). The
  // factory closures defer the window lookup so the late-bind window
  // reference is always current (handles the brief teardown window).
  registerWindowControlsIpc(() => mainWindow);
  registerVersionInfoIpc(() => mainWindow);

  const bundledPath = path.join(__dirname, '../renderer/index.html');

  // Register before loading any URL — ready-to-show fires during loadURL
  // and the listener must be attached first to avoid a race condition where
  // the event fires before the handler is registered (window never shows).
  mainWindow.once('ready-to-show', () => {
    closeSplash();
    mainWindow?.show();
  });

  if (app.isPackaged) {
    await loadPackagedRenderer(mainWindow, bundledPath);
  } else {
    await loadDevRendererWithFallback(mainWindow, bundledPath);
    // Always open DevTools in dev mode regardless of load path
    if (process.env.DEVTOOLS !== '0') {
      mainWindow.webContents.openDevTools();
    }
  }

  // Open DevTools at startup if Developer Mode preference is enabled.
  // Applies to packaged builds (bundled or remote SPA) and to dev fallback.
  if (readDeveloperModePref() && !mainWindow.webContents.isDevToolsOpened()) {
    mainWindow.webContents.openDevTools({ mode: 'detach' });
  }

  // Cmd/Ctrl+Opt+I toggles DevTools when Developer Mode is enabled.
  // (TEMPORARY — remove before BETA along with Developer Mode feature.)
  mainWindow.webContents.on('before-input-event', (_event, input) => {
    if (input.type !== 'keyDown') return;
    const isToggleCombo =
      input.key.toLowerCase() === 'i' && input.alt && (input.meta || input.control);
    if (!isToggleCombo) return;
    if (!readDeveloperModePref()) return;
    if (mainWindow?.webContents.isDevToolsOpened()) {
      mainWindow.webContents.closeDevTools();
    } else {
      mainWindow?.webContents.openDevTools({ mode: 'detach' });
    }
  });

  // Open external links in browser. https-only after #754 tightening —
  // see [internal]specs/2026-04-26-754-externalize-blocked-nav-design.md
  // and [internal]rules/electron.md "External-link scheme policy" for the
  // threat-model rationale (passive nav is held to a stricter scheme set
  // than the user-clicked Markdown-link IPC path).
  mainWindow.webContents.setWindowOpenHandler(({ url }) => {
    try {
      const parsed = new URL(url);
      if (parsed.protocol === 'https:') {
        // Fire-and-forget — main process can't surface a result here. .catch
        // suppresses unhandled-rejection if the OS denies (sandbox, no handler).
        // Symmetric with the IPC handler at src/main/ipc/openExternal.ts.
        shell.openExternal(url).catch(() => {});
      }
      // http: intentionally rejected — Electron app externalization is
      // https-only. Markdown-rendered chat links keep http: via
      // the open-external IPC handler (different consent model).
    } catch {
      // Invalid URL — ignore (no externalization, deny in-app open).
    }
    return { action: 'deny' };
  });

  // Client Behavior [X] close intercept (#806). When the user clicks the
  // native close button, route based on the cached Client Behavior:
  //   - 'tray'    -> hide() (stays running, accessed from system tray, #1099)
  //   - 'toolbar' -> minimize() (stays running in the OS taskbar)
  //   - 'quit'    -> app.quit() (graceful full-app shutdown)
  // event.preventDefault() suppresses the default close -> destroy path so the
  // hide/minimize fall-throughs can take over. The 'quit' branch also calls
  // preventDefault so app.quit can run its before-quit hooks without racing
  // a window-destroy mid-shutdown.
  //
  // isQuitting guard (#1383): once a genuine quit is underway, before-quit has
  // already fired and set isQuitting = true. We MUST let this close proceed
  // (no preventDefault) or we veto Electron's own app.quit() sequence and the
  // app becomes unquittable. This also resolves the 'quit' branch's
  // re-entrancy: app.quit() -> before-quit sets the flag -> re-fired close ->
  // early return here -> window destroys -> quit completes.
  //
  // Trayless fallback (#1099): if action='tray' but the tray failed to
  // initialize (sandboxed env, missing StatusNotifier host, etc.),
  // mainWindow.hide() would strand an invisible, unrecoverable resident
  // process on Windows/Linux (macOS still has Dock activate). Quit instead so
  // a trayless session degrades to a recoverable state. window-all-closed's
  // !isTrayActive() branch alone is not enough: preventDefault()+hide() means
  // the window never actually closes, so window-all-closed never fires.
  mainWindow.on('close', (event) => {
    if (!mainWindow) return;
    if (isQuitting) return;
    const action = deriveCloseAction(getCachedClientBehavior());

    if (action === 'tray') {
      event.preventDefault();
      if (isTrayActive()) {
        mainWindow.hide();
      } else {
        app.quit();
      }
    } else if (action === 'toolbar') {
      event.preventDefault();
      mainWindow.minimize();
    } else {
      // action === 'quit'
      event.preventDefault();
      app.quit();
    }
  });

  // Client Behavior [-] minimize intercept (#806). When the user clicks the
  // native minimize button AND their Client Behavior config routes minimize
  // to the system tray, redirect the post-fact minimize to hide(). Brief
  // flicker is accepted per spec §3.2 trade-off; minimize is a post-fact
  // event, not interceptible-before-the-OS-acts the way close is.
  //
  // Trayless fallback (#1099): if tray init failed, calling restore()+hide()
  // would strand the window invisibly with no taskbar entry. Skip the
  // redirect so the native minimize stands and the window stays recoverable
  // from the taskbar.
  mainWindow.on('minimize', () => {
    if (!mainWindow) return;
    const action = deriveMinimizeAction(getCachedClientBehavior());
    if (action === 'tray' && isTrayActive()) {
      mainWindow.restore();
      mainWindow.hide();
    }
    // else: leave native minimize -> toolbar as-is
  });

  mainWindow.on('closed', () => {
    // #974/#975: a window-less SSO flow (apple or google) has no UI to deliver
    // its result to — tear both down so the loopback listener and 5-minute
    // deadline don't outlive the renderer (teardown trigger (b) in each flow's
    // documented lifecycle).
    cancelActiveAppleFlow();
    cancelActiveGoogleFlow();
    mainWindow = null;
    inviteRendererReady = false;
  });

  // Log renderer crashes to diagnose voice join segfaults
  mainWindow.webContents.on('render-process-gone', (_event, details) => {
    inviteRendererReady = false;
    console.error('[MAIN] Renderer process gone:', details.reason, 'exitCode:', details.exitCode);
  });
};

// Track rollback result so we can notify the renderer after window creation
let rollbackResult: SentinelResult | null = null;

// App lifecycle handlers
app.whenReady().then(async () => {
  registerInviteProtocolClient();

  // app:// protocol handler (#830) — serves the bundled SPA from the asar
  // bundle root. The pure resolver in appProtocol.ts validates the URL,
  // rejects path-traversal, and returns the absolute file path. Here we
  // wrap that with net.fetch which handles asar bundle paths transparently.
  if (app.isPackaged) {
    const bundleRoot = path.resolve(__dirname, '../renderer');
    protocol.handle('app', async (request) => {
      const result = resolveAppProtocolPath(request.url, bundleRoot);
      if (!result.ok || !result.absolutePath) {
        return new Response(null, { status: result.status });
      }
      // pathToFileURL is the canonical cross-platform-safe path→file://
      // transformation. String concat (`'file://' + absolutePath`) works
      // on macOS/Linux but produces malformed URLs on Windows where
      // path.resolve emits drive-letter paths like C:\app\...
      return net.fetch(pathToFileURL(result.absolutePath).href);
    });
  }

  registerOpenExternalHandler(getRemoteSpaBaseUrl);
  // #1729 — native Save-As for decrypted image attachments. Lazy window provider
  // (mirrors registerPermissionHandlers) so the dialog parents to the live window.
  registerSaveImageHandler(() => mainWindow, getRemoteSpaBaseUrl);
  registerSSOIPC(getRemoteSpaBaseUrl);
  registerAttestationIpc(getRemoteSpaBaseUrl);
  // Permission request handler: explicitly allow app-required permissions, deny risky ones.
  // Notifications are allowed (JIT-managed by permissionManager #197).
  session.defaultSession.setPermissionRequestHandler((_webContents, permission, callback) => {
    const denied = ['geolocation'];
    if (denied.includes(permission)) {
      callback(false);
      return;
    }
    callback(true);
  });

  // Permission check handler: Chromium queries this synchronously to verify grants.
  // Default to true so internal checks (e.g. WebAuthn/FIDO2) aren't silently blocked.
  session.defaultSession.setPermissionCheckHandler((_webContents, permission) => {
    const denied = ['geolocation'];
    return !denied.includes(permission);
  });

  // Device permission handler: allow camera, microphone, speaker, and HID (security keys)
  session.defaultSession.setDevicePermissionHandler((details) => {
    return ['camera', 'microphone', 'speaker', 'hid'].includes(details.deviceType ?? '');
  });

  // Update-feed TLS cert pinning (#658). Installs a hostname-gated verify proc
  // on the default session: pinned SaaS hosts enforce SPKI SHA-256 matching
  // against PIN_CONFIG; all other hosts defer to Chromium's default validation.
  // Must be installed BEFORE initAutoUpdater below so the first update check
  // is already armed. See:
  //   [internal]specs/2026-04-20-658-updater-feed-cert-pin-design.md
  //   [internal]
  session.defaultSession.setCertificateVerifyProc(createPinningVerifyProc(PIN_CONFIG, console));

  // HID device selection: auto-select for WebAuthn hardware security keys
  session.defaultSession.on('select-hid-device', (event, details, callback) => {
    event.preventDefault();
    if (details.deviceList && details.deviceList.length > 0) {
      callback(details.deviceList[0].deviceId);
    } else {
      callback('');
    }
  });

  // OS permission management (#197) — JIT prompting replaces proactive startup requests.
  // Camera/mic/screen/notifications are requested when the feature is first used.
  registerPermissionHandlers(() => mainWindow);

  // KLIPY media proxy auth injection (#626): <img>/<video> src attributes
  // send plain GETs without Authorization headers. This interceptor injects
  // the cached JWT so the authenticated media proxy returns 200 instead of 401.
  // Origin-restricted: only injects when the request targets the known API base.
  session.defaultSession.webRequest.onBeforeSendHeaders(
    { urls: ['*://*/api/v1/klipy/media*'] },
    (details, callback) => {
      const token = getCachedAccessToken();
      const apiOrigin = getApiBaseOrigin();
      if (token && apiOrigin) {
        try {
          const requestOrigin = new URL(details.url).origin;
          if (requestOrigin === apiOrigin) {
            details.requestHeaders['Authorization'] = `Bearer ${token}`;
          }
        } catch {
          // Malformed URL — skip injection
        }
      }
      callback({ requestHeaders: details.requestHeaders });
    }
  );

  // ─── Update safety: startup validation (#384) ─────────────────────
  // Must run before createWindow() so we can stash rollback info,
  // but after initAutoUpdater() so the logger is available.
  initAutoUpdater(() => mainWindow);

  const updateLogger = getUpdateLogger();
  if (updateLogger) {
    // Check for deferred cleanup from a previous incomplete finalization
    runDeferredCleanup(updateLogger).catch(() => {});

    const sentinelResult = checkUpdateSentinel(updateLogger);
    if (sentinelResult.type === 'success') {
      updateLogger.info(
        `Update validated: v${sentinelResult.fromVersion} → v${sentinelResult.toVersion}`
      );
      finalizeUpdate(updateLogger, sentinelResult).catch((err) => {
        updateLogger.error(`Post-update cleanup failed: ${(err as Error).message}`);
      });
    } else if (sentinelResult.type === 'rollback') {
      updateLogger.warn(
        `Update to v${sentinelResult.toVersion} failed, rolled back to v${sentinelResult.fromVersion}`
      );
      finalizeRollback(updateLogger, sentinelResult).catch((err) => {
        updateLogger.error(`Rollback cleanup failed: ${(err as Error).message}`);
      });
      rollbackResult = sentinelResult;
    }
  }

  // Show branded splash while the main window loads (#387)
  if (app.isPackaged) {
    const iconPath = path.join(app.getAppPath(), 'build', 'icon.png');
    const icon = nativeImage.createFromPath(iconPath);
    showSplash(icon.isEmpty() ? undefined : icon.toDataURL());

    // If a rollback was detected, show error state on the splash while main window loads
    if (rollbackResult) {
      updateSplashError(
        `Update to v${rollbackResult.toVersion} failed — rolled back to v${rollbackResult.fromVersion}`
      );
    }
  }

  void createWindow().then(() => {
    drainPendingInviteCodes();
    setTimeout(drainPendingInviteCodes, 1000);
  });

  // System tray (#1099): init after the first window exists so the activate
  // handler has a window to reveal. Init failure is non-fatal — the app runs
  // trayless and window-all-closed keeps its quit (see handler below).
  initTray({ getMainWindow: () => mainWindow, createWindow });

  // Notify renderer of rollback after window is created (#384)
  if (rollbackResult) {
    // Capture to local const so TypeScript narrows through the setTimeout closure.
    const rollback = rollbackResult;
    // Delay slightly to ensure renderer IPC listeners are registered
    setTimeout(() => {
      mainWindow?.webContents.send('update:rollback', {
        fromVersion: rollback.fromVersion,
        toVersion: rollback.toVersion,
        message: `Update to v${rollback.toVersion} failed. You are still on v${rollback.fromVersion}.`,
      });
    }, 2000);
  }

  // Proactive token refresh (#254): notify renderer when main process
  // refreshes the token (timer or sleep/wake), so authStore stays current.
  setProactiveRefreshCallback((accessToken, sessionId) => {
    mainWindow?.webContents.send('auth:token-refreshed', { accessToken, sessionId });
  });

  // Refresh token immediately on system wake — main process timers may have
  // drifted past the token's expiry window during sleep (#248).
  powerMonitor.on('resume', () => {
    onSystemResume();
  });

  app.on('activate', () => {
    // On macOS, re-create window when dock icon is clicked
    if (BrowserWindow.getAllWindows().length === 0) {
      createWindow();
    }
  });
});

// Quit when all windows are closed — except on macOS (platform convention)
// and except when the tray is active: the tray is the persistent affordance
// to reopen or quit (#1099). If tray init FAILED, isTrayActive() is false and
// the pre-#1099 quit-on-all-closed behavior is preserved, so a trayless
// session can never strand an invisible resident process.
app.on('window-all-closed', () => {
  if (process.platform !== 'darwin' && !isTrayActive()) {
    app.quit();
  }
});

// Clean up scheduled update checks on quit; flush update log (#383)
app.on('before-quit', () => {
  // Release the [X] close intercept's veto so the window can actually close and
  // the quit can complete — without this, app.quit() deadlocks (#1383).
  isQuitting = true;
  // Release the OS tray resource so no orphaned icon outlives the app (#1099).
  destroyTray();
  const ul = getUpdateLogger();
  ul?.info('Application quitting');
  ul?.flush();
  stopAutoUpdater();
});

// Crash-safe logging: flush update log before unhandled exceptions terminate the process (#383).
// Registering this listener disables Node's default crash behavior, so we must exit explicitly.
process.on('uncaughtException', (error) => {
  const ul = getUpdateLogger();
  ul?.error(`Uncaught exception: ${error.message}\n${error.stack ?? ''}`);
  ul?.flush();
  setImmediate(() => app.exit(1));
});

// IPC Handlers
ipcMain.handle('app:getVersion', () => {
  return app.getVersion();
});

ipcMain.handle('app:getPlatform', () => {
  return process.platform;
});

ipcMain.handle('app:getIpcContract', () => {
  return IPC_CONTRACT_VERSION;
});

// Forensic build-tag observability (#920 §5.13, #939). Returns the CI
// build tag baked into the packaged app (via forge extraResource
// buildtag.json) or 'unknown' for local dev builds. Read-only,
// information-only — knowing the tag does not unlock any capability.
ipcMain.handle('app:getBuildTag', () => {
  return getBuildTag();
});

// Screen capture sources for voice/video screen share picker (#44)
ipcMain.handle('media:getDesktopSources', async () => {
  const sources = await desktopCapturer.getSources({
    types: ['window', 'screen'],
    thumbnailSize: { width: 320, height: 180 },
  });
  return sources.map((s) => ({
    id: s.id,
    name: s.name,
    thumbnail: s.thumbnail.toDataURL(),
    appIcon: s.appIcon?.toDataURL() || null,
  }));
});

// Clipboard write (navigator.clipboard.writeText is blocked in Electron)
ipcMain.handle('clipboard:writeText', (_event, text: string) => {
  clipboard.writeText(text);
});

// GPU info
const GPU_VENDORS: Record<number, string> = {
  0x106b: 'Apple',
  0x8086: 'Intel',
  0x10de: 'NVIDIA',
  0x1002: 'AMD',
  0x1022: 'AMD',
  0x5143: 'Qualcomm',
  0x13b5: 'ARM',
};

ipcMain.handle('gpu:getInfo', async () => {
  try {
    const info = await app.getGPUInfo('basic');
    const gpu = (
      info as {
        gpuDevice?: Array<{
          vendorId: number;
          deviceId: number;
          driverVendor?: string;
          driverDescription?: string;
        }>;
      }
    ).gpuDevice?.[0];
    if (gpu) {
      // driverVendor on macOS often contains the hex vendorId as a string — skip it
      const driverName =
        gpu.driverVendor && !/^0x[0-9a-f]+$/i.test(gpu.driverVendor) ? gpu.driverVendor : '';
      const vendor =
        driverName || GPU_VENDORS[gpu.vendorId] || `Unknown (0x${gpu.vendorId.toString(16)})`;
      const device =
        gpu.driverDescription || (gpu.deviceId ? `Device 0x${gpu.deviceId.toString(16)}` : '');
      return { vendor, device };
    }
    return null;
  } catch {
    return null;
  }
});

// Display info for resolution/refresh rate awareness
ipcMain.handle('screen:getDisplayInfo', () => {
  const displays = screen.getAllDisplays();
  const primary = screen.getPrimaryDisplay();
  return displays.map((d) => ({
    width: d.size.width * d.scaleFactor,
    height: d.size.height * d.scaleFactor,
    refreshRate: d.displayFrequency,
    scaleFactor: d.scaleFactor,
    isPrimary: d.id === primary.id,
    colorDepth: d.colorDepth, // 24 = SDR, 30/48 = HDR
    colorSpace: d.colorSpace, // "srgb" = SDR, "p3"/"rec2020" = wide gamut
  }));
});

// Hardware acceleration preference
ipcMain.handle('app:getHardwareAcceleration', () => readHwAccelPref());

ipcMain.handle('app:setHardwareAcceleration', (_event, enabled: boolean) => {
  fs.writeFileSync(hwAccelPrefPath, JSON.stringify({ enabled }), 'utf-8');
});

// Relaunch (for hardware acceleration toggle)
ipcMain.handle('app:relaunch', () => {
  app.relaunch();
  app.quit();
});

// Hot SPA reload — main process re-runs the spaLoader safety chain and points
// the live window at the freshly-resolved remote SPA URL (or bundled on
// fallback). Powers the Settings ▸ About "Load latest UI" button: lets a client
// stranded on the bundled SPA (the cold-start config-fetch race) escape to
// remote WITHOUT an app restart. SECURITY: the renderer only TRIGGERS this; the
// URL is derived entirely in main from resolveSpaSource (getPersistedApiBase +
// the authenticated /api/v1/client/config fetch). NO URL is accepted from the
// renderer, so a compromised renderer cannot choose the origin. Reuses
// applySpaDecision so launch and runtime cannot drift.
ipcMain.handle('spa:reloadLatest', async (event) => {
  // Privileged: this reaches a top-frame navigation. Reject an untrusted sender
  // frame FIRST — before any window/packaged state check — so a compromised
  // frame is refused regardless of state. isPermittedFrameUrl accepts both
  // states this feature spans: app://concord (the stranded bundled state) and
  // the active remote origin.
  if (!isPermittedFrameUrl(event.senderFrame?.url ?? '', getRemoteSpaBaseUrl())) {
    console.warn('[SpaLoader/reload] rejected spa:reloadLatest from untrusted frame');
    return { mode: 'bundled', changed: false, rejected: true };
  }
  if (!mainWindow || !app.isPackaged) return { mode: 'bundled', changed: false };
  const before = getSpaHash();
  const decision = await resolveSpaSource();
  console.debug(`[SpaLoader/reload] ${decision.mode}: ${decision.reason}`);
  const mode = await applySpaDecision(mainWindow, decision);
  return { mode, changed: getSpaHash() !== before };
});

// SPA (UI) update check — the SECOND update axis, distinct from the
// electron-updater desktop-binary axis (update:*). Reports whether the renderer
// is on the bundled fallback vs remote, and whether NEWER remote-SPA bytes are
// live (a SHA-256 diff of the served index.html — /client/config exposes no SPA
// build id and the SPA URL is constant post-#976, so there is no version number
// to show, only "newer bytes available"). Read-only and best-effort (never
// throws); the network target is server-derived, not renderer-supplied.
ipcMain.handle('spa:checkForUpdate', async (event) => {
  // Read-only, but validate the sender frame for parity with spa:reloadLatest
  // and defense-in-depth — closes the (minor) redundant-self-fetch surface a
  // compromised/sandboxed frame could otherwise trigger.
  if (!isPermittedFrameUrl(event.senderFrame?.url ?? '', getRemoteSpaBaseUrl())) {
    return {
      currentMode: 'bundled',
      remoteAvailable: false,
      newerBytesAvailable: null,
      reason: 'rejected',
    };
  }
  if (!app.isPackaged) {
    return {
      currentMode: 'remote',
      remoteAvailable: false,
      newerBytesAvailable: null,
      reason: 'dev mode',
    };
  }
  const currentMode = getRemoteSpaUrl() === null ? 'bundled' : 'remote';
  const decision = await resolveSpaSource();
  const remoteAvailable = decision.mode === 'remote' && !!decision.url;
  let newerBytesAvailable: boolean | null = null;
  if (remoteAvailable && decision.url) {
    const available = await hashEntryHtml(decision.url);
    // null (re-fetch failed) → unknown; the UI degrades to an unconditional offer.
    newerBytesAvailable = available === null ? null : available !== getSpaHash();
  }
  return { currentMode, remoteAvailable, newerBytesAvailable, reason: decision.reason };
});

// Clean app exit (preserves persisted auth/remember-me state)
ipcMain.handle('app:quit', () => {
  app.quit();
});

// ─── Desktop Notification Helpers (#175) ────────────────────────────

ipcMain.handle('app:setBadgeCount', (_event, count: number) => {
  app.setBadgeCount(count);
});

ipcMain.handle('app:flashFrame', (_event, flash: boolean) => {
  mainWindow?.flashFrame(flash);
});

ipcMain.handle('app:focusWindow', () => {
  if (mainWindow) {
    if (mainWindow.isMinimized()) mainWindow.restore();
    mainWindow.focus();
  }
});

// ─── Auto-Update IPC ──────────────────────────────────────────────────

ipcMain.handle('update:check', () => checkForUpdates());
ipcMain.handle('update:download', () => downloadUpdate());
ipcMain.handle('update:install', () => safeQuitAndInstall());
ipcMain.handle('update:getAllowPrerelease', () => getAllowPrerelease());
ipcMain.handle('update:setAllowPrerelease', (_event, enabled: boolean) =>
  setAllowPrerelease(enabled)
);
ipcMain.handle('update:getLogPath', () => getUpdateLogPath());
// No sender-frame validation, by parity with the sibling `update:*` handlers
// above. `forceCheckForUpdates` only triggers a check against the pinned
// electron-updater feed (#719); `reason` flows solely to a log line and cannot
// influence the network target. Worst case from a compromised renderer is a
// redundant check against our own server (DoS-equivalent) — strictly less
// powerful than the adjacent unvalidated update:download / update:install.
ipcMain.handle('updater:force-check', (_event, reason: 'attestation_required' | 'user_triggered') =>
  forceCheckForUpdates(reason)
);

// Developer Mode (gates DevTools in packaged builds — REMOVE BEFORE BETA)
ipcMain.handle('app:getDeveloperMode', () => readDeveloperModePref());
ipcMain.handle('app:setDeveloperMode', (_event, enabled: boolean) => {
  writeDeveloperModePref(enabled);
  if (!mainWindow) return;
  if (enabled) {
    if (!mainWindow.webContents.isDevToolsOpened()) {
      mainWindow.webContents.openDevTools({ mode: 'detach' });
    }
  } else if (mainWindow.webContents.isDevToolsOpened()) {
    mainWindow.webContents.closeDevTools();
  }
});

// System info for About page (#155 Tier 4)
ipcMain.handle('app:getSystemInfo', () => ({
  platform: process.platform,
  arch: process.arch,
  electronVersion: process.versions.electron,
  chromiumVersion: process.versions.chrome,
  nodeVersion: process.versions.node,
}));

// ─── Secure Auth Token Management (safeStorage) ──────────────────────

ipcMain.handle(
  'auth:storeRefreshToken',
  (
    _event,
    data: {
      refreshToken: string;
      rememberMe: boolean;
      apiBase: string;
      accessToken?: string;
    }
  ) => {
    storeRefreshToken(data);
    // Clear any cached restore result so subsequent restoreSession calls
    // (e.g. after HMR or renderer reload) use the fresh token, not a stale
    // 'refresh_failed' from a previous attempt.
    restoreSessionPromise = null;

    // Activate auto-updater now that we have an API base
    // (first-launch users won't have this until login)
    if (data.apiBase) {
      setUpdateFeedUrl(data.apiBase);
    }
  }
);

// Deduplicate restoreSession: React Strict Mode can fire the renderer's
// useEffect twice, causing two IPC calls.  Without dedup each call would
// trigger a separate token rotation, wasting rotations and widening the
// window for race conditions.
let restoreSessionPromise: Promise<{
  status: string;
  accessToken?: string;
  sessionId?: string;
  e2eeKeys?: unknown;
}> | null = null;

ipcMain.handle('auth:restoreSession', () => {
  if (restoreSessionPromise) return restoreSessionPromise;

  restoreSessionPromise = (async () => {
    const restored = restoreRefreshToken();
    if (restored.status !== 'ok') {
      return { status: restored.status };
    }
    // Token restored from disk — refresh it to get a fresh access token
    const refreshResult = await performRefresh();
    if (refreshResult.status === 'ok' && refreshResult.accessToken) {
      // Also restore E2EE keys if available
      const e2eeKeys = restoreE2EEKeys();
      return {
        status: 'restored',
        accessToken: refreshResult.accessToken,
        sessionId: refreshResult.sessionId,
        e2eeKeys,
      };
    }
    return { status: 'refresh_failed' };
  })();

  return restoreSessionPromise;
});

ipcMain.handle(
  'auth:storeE2EEKeys',
  (
    _event,
    data: {
      wrappingKeyBase64: string;
      preferencesKeyBase64: string;
      wrappedPrivateKeyBase64: string;
    }
  ) => {
    storeE2EEKeys(data);
  }
);

ipcMain.handle('auth:refreshToken', async () => {
  return performRefresh();
});

ipcMain.handle('auth:logout', async (_event, data?: { accessToken?: string }) => {
  await performLogout(data?.accessToken);
});

ipcMain.handle('auth:clearTokens', () => {
  clearTokens();
});

ipcMain.handle('auth:getCapabilities', () => {
  return getCapabilities();
});

ipcMain.handle('auth:getMachineId', () => {
  return getMachineId();
});

// ─── PiP (Picture-in-Picture) Window Management ──────────────────────
const pipWindows = new Map<string, BrowserWindow>();

ipcMain.handle(
  'pip:open',
  (event, opts: { id: string; width?: number; height?: number; title?: string }) => {
    // Defense-in-depth: only accept pip:open from the active SPA, the dev
    // server, or the bundled renderer. Without validation any frame loaded
    // in any window could spawn PiP windows.
    const remoteUrl = getRemoteSpaUrl();
    if (!isValidPipOpenSender(event.senderFrame?.url ?? '', app.isPackaged, remoteUrl)) {
      console.warn('[pip:open] rejected — sender frame validation failed');
      return;
    }

    const existing = pipWindows.get(opts.id);
    if (existing) {
      existing.focus();
      return;
    }

    const pip = new BrowserWindow({
      width: opts.width || 320,
      height: opts.height || 240,
      minWidth: 160,
      minHeight: 120,
      frame: false,
      // #806: opt out of the OS-drawn drop shadow so the lightweight chrome
      // visually reads as "floating glass" rather than "compact app window".
      // On macOS this drops the standard window shadow; on Wayland and X11
      // the compositor decides regardless. Safe on Windows (no-op).
      hasShadow: false,
      alwaysOnTop: true,
      skipTaskbar: true,
      resizable: true,
      title: opts.title || 'Concord Voice PiP',
      webPreferences: {
        preload: path.join(__dirname, '../preload/preload.js'),
        nodeIntegration: false,
        contextIsolation: true,
        sandbox: true,
      },
      backgroundColor: '#000',
    });

    // Load PiP route (hash-based routing)
    if (!app.isPackaged) {
      pip.loadURL(`http://localhost:3001#/pip/${opts.id}`);
    } else if (remoteUrl) {
      // Tier 3: PiP windows load from the remote SPA, including the
      // `/spa/<sha>/` path component. Origin-only would fall through to
      // nginx's catch-all and redirect to the marketing site (#802).
      pip.loadURL(buildRemotePipUrl(remoteUrl, opts.id));
    } else {
      // #830: PiP bundled mode loads via app:// for consistent origin.
      pip.loadURL(`app://concord/index.html#/pip/${opts.id}`);
    }

    pipWindows.set(opts.id, pip);

    pip.on('closed', () => {
      pipWindows.delete(opts.id);
      mainWindow?.webContents.send('pip:closed', { id: opts.id });
    });
  }
);

ipcMain.handle('pip:close', (_event, opts: { id: string }) => {
  pipWindows.get(opts.id)?.close();
});

ipcMain.handle('pip:setAlwaysOnTop', (_event, opts: { id: string; flag: boolean }) => {
  pipWindows.get(opts.id)?.setAlwaysOnTop(opts.flag);
});

ipcMain.handle('spa:requestSelfHeal', async (event, payload: unknown) => {
  // Thin adapter: unpack Electron's event-args, delegate to the pure-data
  // handler body. The handler itself does sender-frame validation,
  // payload validation, and dispatches to attemptSelfHeal. Extracted for
  // unit-testability per #753 reconciliation finding TA3.
  await handleSpaRequestSelfHeal({
    senderFrameUrl: event.senderFrame?.url ?? '',
    payload,
    remoteSpaBaseUrl: getRemoteSpaBaseUrl(),
    remoteSpaBaseDir: getRemoteSpaBaseDir(),
  });
});

ipcMain.on('invite:renderer-ready', (event) => {
  if (!mainWindow || event.sender !== mainWindow.webContents) return;
  inviteRendererReady = true;
  drainPendingInviteCodes();
});

// Prevent multiple instances
const gotTheLock = app.requestSingleInstanceLock();

if (gotTheLock) {
  handleInviteDeepLinksFromArgv(process.argv, 'argv');

  app.on('open-url', (event, url) => {
    event.preventDefault();
    handleInviteDeepLink(url, 'open-url');
  });

  app.on('second-instance', (_event, argv: string[]) => {
    handleInviteDeepLinksFromArgv(argv, 'second-instance');
    // Someone tried to run a second instance, focus our window
    if (mainWindow) {
      if (mainWindow.isMinimized()) mainWindow.restore();
      mainWindow.focus();
    }
  });
} else {
  app.quit();
}

// Security: validate navigation. After #754: extends with externalization
// safety net — bare <a href="https://..."> clicks (or programmatic
// navigation) route to the OS browser via shell.openExternal instead of
// silently failing. https-only, symmetric with setWindowOpenHandler.
// See [internal]specs/2026-04-26-754-externalize-blocked-nav-design.md
// and [internal]rules/electron.md "External-link scheme policy".
app.on('web-contents-created', (_, contents) => {
  contents.on('will-navigate', (event, navigationUrl) => {
    let parsedUrl: URL;
    try {
      parsedUrl = new URL(navigationUrl);
    } catch {
      // Malformed URL — fail closed. Do not navigate, do not externalize.
      event.preventDefault();
      return;
    }

    if (app.isPackaged) {
      // Packaged release: allow same-origin SPA navigation. spaOrigin is
      // already an origin string (set via `new URL(decision.url).origin`),
      // so a direct read suffices.
      const spaOrigin = getRemoteSpaBaseUrl();
      if (spaOrigin && parsedUrl.origin === spaOrigin) {
        return;
      }
      // #830 review: in bundled-fallback mode (post-Task-4), the renderer
      // loads from app://concord/index.html and setRemoteSpaState(null)
      // leaves spaOrigin empty. SPA navigations within the app://concord
      // origin (e.g., chunk reloads, programmatic location.assign) must
      // still be allowed through this gate. Use protocol+host comparison
      // rather than parsedUrl.origin because WHATWG URL spec returns
      // "null" for URL.origin on non-special schemes — same gotcha as in
      // pipUrl.ts:isValidPipOpenSender.
      if (parsedUrl.protocol === 'app:' && parsedUrl.host === 'concord') {
        return;
      }
      // Block in-window navigation to anything else.
      event.preventDefault();
      // Drift safety net: route https: navigations to OS browser so a
      // future bare-<a> link or SPA-driven redirect doesn't silently fail.
      // .catch suppresses unhandled-rejection (OS deny) — symmetric with
      // setWindowOpenHandler and the IPC handler.
      if (parsedUrl.protocol === 'https:') {
        shell.openExternal(navigationUrl).catch(() => {});
      }
      // Other schemes (javascript:, data:, file:, http:, vbscript:, ...)
      // are silently dropped after preventDefault — fail-closed posture.
    } else if (parsedUrl.hostname !== 'localhost') {
      // Dev mode: allow any localhost port (HMR), block everything else.
      event.preventDefault();
      if (parsedUrl.protocol === 'https:') {
        shell.openExternal(navigationUrl).catch(() => {});
      }
    }
  });

  // SPA self-heal main-process detection (#753, ADR-0001) — runs outside
  // the renderer bundle, so it survives renderer-side corruption that
  // would silence the renderer-side listener in spaSelfHealClient.ts.
  // Thin adapter delegating to handleDidFailLoad (extracted for unit
  // testability per #753 reconciliation finding TA3).
  contents.on(
    'did-fail-load',
    (_event, errorCode, _errorDescription, validatedURL, isMainFrame) => {
      void handleDidFailLoad({
        errorCode,
        validatedURL,
        isMainFrame,
        remoteSpaBaseUrl: getRemoteSpaBaseUrl(),
        remoteSpaBaseDir: getRemoteSpaBaseDir(),
      });
      if (isMainFrame && validatedURL === 'app://concord/index.html' && errorCode !== -3) {
        revealLoadFailure(mainWindow, 'Could not load application — please reinstall');
      }
    }
  );
});
