import {
  autoUpdater,
  type UpdateInfo,
  type UpdateDownloadedEvent,
  type ProgressInfo,
} from 'electron-updater';
import { app, net, type BrowserWindow } from 'electron';
import path from 'node:path';
import fs from 'node:fs';
import { UPDATE_ENDPOINT_URL } from '../shared/updateEndpoint';
import { ALLOWED_WINDOWS_PUBLISHERS } from '../shared/allowedWindowsPublishers';
import { createUpdateLogger, type UpdateLogger } from './updateLogger';
import { prepareForUpdate, discardPreparedUpdate } from './updateSafety';
import { updateSplashStatus, showSplashProgress, updateSplashError } from './splashWindow';
import { extractChain, verifyChain } from './verifyWindowsSignature';
import { verifyLinuxArtifact, type LinuxVerifyResult } from './verifyLinuxSignature';

// Check every 4 hours; delay first check 10s so it doesn't block app startup
const UPDATE_CHECK_INTERVAL_MS = 4 * 60 * 60 * 1000;
const STARTUP_CHECK_DELAY_MS = 10_000;

// Persisted preference for pre-release updates (same pattern as hw-accel.json)
const prereleasePrefPath = path.join(app.getPath('userData'), 'update-prefs.json');

interface UpdatePrefs {
  allowPrerelease: boolean;
}

function logReadError(msg: string): void {
  if (logger) logger.error(msg);
  // eslint-disable-next-line no-restricted-syntax -- msg is a caller-stringified error message (param type `msg: string`), not a raw Error; no err.cause chain to propagate
  else console.error('[updater]', msg);
}

function readUpdatePrefs(): UpdatePrefs {
  try {
    const data = JSON.parse(fs.readFileSync(prereleasePrefPath, 'utf-8'));
    return { allowPrerelease: data.allowPrerelease !== false };
  } catch (err) {
    const e = err as NodeJS.ErrnoException;
    if (e.code === 'ENOENT') {
      // Expected on first run; silently return default.
      return { allowPrerelease: true };
    }
    if (err instanceof SyntaxError) {
      const corruptPath = `${prereleasePrefPath}.corrupt.${Date.now()}`;
      try {
        fs.renameSync(prereleasePrefPath, corruptPath);
        logReadError(
          `Corrupt update-prefs.json (SyntaxError); moved to ${corruptPath}. Resetting to defaults.`
        );
      } catch (renameErr) {
        logReadError(
          `Corrupt update-prefs.json (SyntaxError); also failed to rename it: ${(renameErr as Error).message}`
        );
      }
      return { allowPrerelease: true };
    }
    logReadError(`Failed to read update prefs (${e.code ?? 'unknown'}): ${e.message}`);
    return { allowPrerelease: true };
  }
}

function writeUpdatePrefs(prefs: UpdatePrefs): void {
  try {
    fs.writeFileSync(prereleasePrefPath, JSON.stringify(prefs), 'utf-8');
  } catch (err) {
    const message = `Failed to write update prefs: ${(err as Error).message}`;
    if (logger) logger.error(message);
    // eslint-disable-next-line no-restricted-syntax -- message is a locally-built string from (err as Error).message above, not a raw Error; no err.cause chain to propagate
    else console.error('[updater]', message);
  }
}

let checkInterval: ReturnType<typeof setInterval> | null = null;
let getWindow: (() => BrowserWindow | null) | null = null;
let releaseQuitVetoForUpdate: (() => void) | null = null;
let handlersRegistered = false;
let logger: UpdateLogger | null = null;
let pendingUpdateVersion: string | null = null;
// Captured artifact selection for the Windows and Linux install gates.
// Every downloaded event advances its identity, including same-path reuse.
let pendingDownloadedFile: string | null = null;
let pendingGeneration = 0;
let installInFlight = false;
let downloadsInFlight = 0;
// Status-aware tray (tray-status spec §4): the update phase is main-owned and reaches
// the tray through this callback, injected by main.ts at init. `announcedUpdateVersion`
// remembers an `update-available` that has not been downloaded yet, so an unrelated
// error does not drop the tray's "Download update" row (autoDownload is off, so
// "available" is a lasting state).
export type TrayUpdateState = 'none' | 'available' | 'downloading' | 'ready';
let onTrayUpdateState: ((state: TrayUpdateState, percent?: number) => void) | null = null;
let announcedUpdateVersion: string | null = null;

function notifyTray(state: TrayUpdateState, percent?: number): void {
  if (onTrayUpdateState === null) return;
  try {
    if (percent === undefined) onTrayUpdateState(state);
    else onTrayUpdateState(state, percent);
  } catch {
    // The tray is optional chrome: it can never break an updater event.
  }
}

/** The phase the tray should show when no event names a newer one. */
function settledTrayState(): TrayUpdateState {
  if (pendingUpdateVersion) return 'ready';
  return announcedUpdateVersion ? 'available' : 'none';
}

type WindowsVerification = 'verified' | 'rejected' | 'unavailable';

/** Shared extraction and chain policy for the download and install gates. */
async function verifyWindowsUpdateSignature(
  filePath: string,
  publisherNames: readonly string[]
): Promise<WindowsVerification> {
  try {
    const chain = await extractChain(filePath);
    if (verifyChain(chain, publisherNames) !== null) {
      logger?.error('SECURITY: Windows update signature rejected by publisher/intermediate policy');
      return 'rejected';
    }
    logger?.info('Runtime chain verification passed');
    return 'verified';
  } catch {
    // PowerShell output can contain local paths; expose only the bounded verdict.
    logger?.warn('Windows update signature extraction unavailable');
    return 'unavailable';
  }
}

/**
 * Returns the module logger, throwing if it has not been initialized.
 * Used inside code paths that only run after ensureUpdaterReady() has
 * executed (event handlers, post-init hooks). Converts a structural
 * invariant into an explicit runtime check so we don't need non-null
 * assertions sprinkled at every call site.
 */
function requireLogger(): UpdateLogger {
  if (!logger) {
    throw new Error('updater: logger accessed before ensureUpdaterReady()');
  }
  return logger;
}

function logPublisherVerificationPosture(): void {
  if (process.platform === 'win32') {
    logger?.info(`Publisher verification active: ${ALLOWED_WINDOWS_PUBLISHERS.join(', ')}`);
  } else if (process.platform === 'darwin') {
    logger?.info('Publisher verification delegated to macOS Gatekeeper/notarization');
  } else {
    logger?.warn('Publisher verification not enforced by electron-updater on this platform');
  }
}

function configureUpdateFeed(): void {
  autoUpdater.setFeedURL({
    provider: 'generic',
    url: UPDATE_ENDPOINT_URL,
    publisherName: [...ALLOWED_WINDOWS_PUBLISHERS],
  });
  logPublisherVerificationPosture();
  requireLogger().info(`Feed URL set to ${UPDATE_ENDPOINT_URL}`);
}

/** Send an IPC event to the renderer (if the window exists). */
function sendToRenderer(channel: string, data: unknown): void {
  const win = getWindow?.();
  if (win && !win.isDestroyed()) {
    win.webContents.send(channel, data);
  }
}

/** Configure updater settings and register event handlers (runs exactly once). */
function ensureUpdaterReady(): void {
  if (handlersRegistered) return;
  handlersRegistered = true;

  // Initialize structured file logger (#383)
  logger = createUpdateLogger();
  autoUpdater.logger = logger;

  // Privacy-first: require explicit user action to download.
  // autoInstallOnAppQuit is false so updates always go through safeQuitAndInstall (#384).
  autoUpdater.autoDownload = false;
  autoUpdater.autoInstallOnAppQuit = false;

  // Read persisted preference (defaults to true for 0.x pre-release cycle)
  autoUpdater.allowPrerelease = readUpdatePrefs().allowPrerelease;

  // Windows-only runtime cert-chain verification. Defeats MITM scenarios
  // where an attacker obtains a CN-matching cert from a different CA —
  // pinning the Microsoft Trusted Signing intermediate blocks that.
  //
  // Leaf-thumbprint pinning is intentionally NOT used: Microsoft Trusted
  // Signing leaf certs are 72-hour short-lived, auto-renewed daily. See
  // spec [internal]specs/2026-04-15-644-update-trust-hardening-design.md
  if (process.platform === 'win32') {
    // electron-updater exposes `verifyUpdateCodeSignature` as a runtime hook
    // on the AppUpdater instance — it's a documented monkey-patch point in
    // electron-builder's codebase but is not in the published TypeScript
    // types. A narrow intersection cast `as typeof autoUpdater & { ... }` is
    // redundant per typescript:S4323 (the intersection doesn't actually
    // change the assignability, since `autoUpdater` already accepts property
    // assignment). `as any` is the pragmatic workaround with a specific
    // invariant rationale rather than a pretend-narrower type.
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- electron-updater.AppUpdater type omits the runtime-monkey-patchable verifyUpdateCodeSignature hook; see extractChain comment block above for the pinning rationale this override enables
    (autoUpdater as any).verifyUpdateCodeSignature = async (
      publisherNames: string[],
      filePath: string
    ): Promise<string | null> => {
      const result = await verifyWindowsUpdateSignature(filePath, publisherNames);
      return result === 'verified' ? null : 'Windows update signature verification failed';
    };
  }

  // ─── Event handlers ─────────────────────────────────────────────────

  autoUpdater.on('checking-for-update', () => {
    requireLogger().info(
      `Checking for updates (current: v${app.getVersion()}, ${process.platform}/${process.arch})`
    );
    updateSplashStatus('Checking the airwaves...');
  });

  autoUpdater.on('update-available', (info: UpdateInfo) => {
    requireLogger().info(
      `Update available: v${app.getVersion()} → v${info.version} (released ${info.releaseDate ?? 'unknown'})`
    );
    updateSplashStatus(`New flight plan detected: v${info.version}`);
    sendToRenderer('update:available', {
      version: info.version,
      releaseNotes: info.releaseNotes,
      releaseDate: info.releaseDate,
    });
    announcedUpdateVersion = info.version;
    notifyTray(settledTrayState());
  });

  autoUpdater.on('update-not-available', (info: UpdateInfo) => {
    requireLogger().info(`Up to date (v${info.version})`);
    updateSplashStatus('All systems go');
    sendToRenderer('update:not-available', { version: info.version });
    if (!pendingUpdateVersion) {
      announcedUpdateVersion = null;
      notifyTray('none');
    }
  });

  autoUpdater.on('download-progress', (progress: ProgressInfo) => {
    const msg = `${progress.percent.toFixed(1)}% (${formatBytes(progress.transferred)}/${formatBytes(progress.total)})`;
    requireLogger().info(`Download: ${msg}`);
    showSplashProgress(progress.percent);
    updateSplashStatus(`Fueling up... ${progress.percent.toFixed(0)}%`);
    sendToRenderer('update:download-progress', {
      percent: progress.percent,
      transferred: progress.transferred,
      total: progress.total,
      bytesPerSecond: progress.bytesPerSecond,
    });
    notifyTray('downloading', progress.percent);
  });

  autoUpdater.on('update-downloaded', (event: UpdateDownloadedEvent) => {
    pendingGeneration += 1;
    pendingUpdateVersion = event.version;
    pendingDownloadedFile = event.downloadedFile;
    requireLogger().info(`Downloaded v${event.version} — ready to install`);
    showSplashProgress(100);
    updateSplashStatus('Ready for liftoff');
    sendToRenderer('update:downloaded', { version: event.version });
    notifyTray('ready');
  });

  autoUpdater.on('error', (error: Error) => {
    const msg = error.message;
    // Cert-pin failures surface as net-layer TLS errors (e.g. ERR_CERT_*,
    // "certificate verify failed"). They are security events with their own
    // subtype so the renderer can show the cert-pin-specific banner copy. #658
    const isCertPinFailure = /certificate|cert.?pin|ERR_CERT/i.test(msg);
    const isPublisherFailure = /publisher|signature|not signed/i.test(msg);
    const isSecurityEvent = isCertPinFailure || isPublisherFailure;
    let subtype: 'cert-pin-failure' | 'publisher-failure' | undefined;
    if (isCertPinFailure) {
      subtype = 'cert-pin-failure';
    } else if (isPublisherFailure) {
      subtype = 'publisher-failure';
    }

    if (isSecurityEvent) {
      const label = isCertPinFailure ? 'cert-pin verification' : 'signer verification';
      requireLogger().error(
        `SECURITY: Update ${label} failed — ${msg}${error.stack ? '\n' + error.stack : ''}`
      );
      updateSplashError(
        isCertPinFailure
          ? 'Update blocked: cert verification failed'
          : 'Update blocked: signature verification failed'
      );
    } else {
      requireLogger().error(`Error: ${msg}${error.stack ? '\n' + error.stack : ''}`);
      updateSplashError('Houston, we have a problem');
    }
    sendToRenderer('update:error', {
      message: msg,
      securityEvent: isSecurityEvent,
      subtype,
    });
    // 'none' only when no version is pending: a downloaded update stays ready, an
    // announced one stays downloadable.
    notifyTray(settledTrayState());
  });
}

/** Start the scheduled update check interval (idempotent). */
function startScheduledChecks(): void {
  if (checkInterval) return;

  const scheduledCheck = () => {
    checkForUpdates().catch((err: unknown) => {
      logger?.error(`Scheduled update check failed: ${(err as Error).message}`);
    });
  };
  setTimeout(scheduledCheck, STARTUP_CHECK_DELAY_MS);
  checkInterval = setInterval(scheduledCheck, UPDATE_CHECK_INTERVAL_MS);
}

export function initAutoUpdater(
  mainWindowGetter: () => BrowserWindow | null,
  releaseUpdateQuitVeto?: () => void,
  trayUpdateState?: (state: TrayUpdateState, percent?: number) => void
): void {
  getWindow = mainWindowGetter;
  releaseQuitVetoForUpdate = releaseUpdateQuitVeto ?? null;
  onTrayUpdateState = trayUpdateState ?? null;

  if (!app.isPackaged) {
    // No logger yet in dev mode — use console directly
    console.debug('[updater] Skipping auto-update in development');
    return;
  }

  // Always register settings and event handlers so renderer events work
  // even when login/session restore has not run yet.
  ensureUpdaterReady();

  configureUpdateFeed();
  startScheduledChecks();
}

// ─── Public API (consumed by IPC handlers in main.ts) ─────────────────

export async function checkForUpdates(): Promise<void> {
  try {
    await autoUpdater.checkForUpdates();
  } catch (error) {
    // Log for diagnostics, then rethrow so the IPC invoke rejects
    // and the renderer can transition out of the "checking" state.
    logger?.error(`Check failed: ${(error as Error).message}`);
    throw error;
  }
}

/**
 * Force an immediate update check, bypassing the scheduled-check cadence.
 * Invoked by the renderer's attestation 403-retry path (and user-triggered
 * "check now" actions). Always uses the static public recovery feed (#1981) —
 * never a server-supplied URL.
 */
export async function forceCheckForUpdates(
  reason: 'attestation_required' | 'user_triggered'
): Promise<void> {
  logger?.info(`forceCheckForUpdates: ${reason}`);
  await autoUpdater.checkForUpdates();
}

export async function downloadUpdate(): Promise<string[]> {
  if (installInFlight) throw new Error('Update installation in progress. Please try again later.');
  // Upstream changes its installer selection before emitting update-downloaded.
  downloadsInFlight += 1;
  try {
    return await autoUpdater.downloadUpdate();
  } finally {
    downloadsInFlight -= 1;
  }
}

/**
 * Linux-only pre-install signature gate (#653). Verifies the downloaded
 * artifact's detached Ed25519 signature against the bundled public key.
 * Returns a LinuxVerifyResult — `verified: true` to proceed, otherwise REFUSE.
 * Fail-closed: a missing downloaded path refuses with `kind: 'unavailable'`
 * (a config/availability state, not evidence of tampering). The `.sig` is
 * fetched from the same static public recovery feed the updater already uses.
 */
async function verifyLinuxUpdateBeforeInstall(file: string | null): Promise<LinuxVerifyResult> {
  if (!file) {
    return { verified: false, reason: 'no downloaded artifact path captured', kind: 'unavailable' };
  }
  const sigUrl = `${UPDATE_ENDPOINT_URL}/${path.basename(file)}.sig`;
  return verifyLinuxArtifact(file, sigUrl, net.fetch);
}

/** Refuse a Windows installer whose trust could not be re-established this session. */
function refuseUnverifiedWindowsInstaller(verdict: Exclude<WindowsVerification, 'verified'>): void {
  announcedUpdateVersion ??= pendingUpdateVersion;
  pendingUpdateVersion = null;
  pendingDownloadedFile = null;
  pendingGeneration += 1;
  discardPreparedUpdate(requireLogger());
  // The installer was discarded; the user must download again.
  notifyTray(settledTrayState());
  updateSplashError('Update could not be verified');
  sendToRenderer(
    'update:error',
    verdict === 'rejected'
      ? {
          message:
            'Update blocked: Windows signature verification failed. Please download the update again.',
          securityEvent: true,
          subtype: 'publisher-failure',
        }
      : {
          message:
            'Update could not be verified right now. Please download the update again and retry.',
        }
  );
}

/** Report a Linux pre-install refusal with tamper-vs-availability copy. */
function refuseUnverifiedLinuxInstaller(
  result: Extract<LinuxVerifyResult, { verified: false }>
): void {
  if (result.kind === 'tampered') {
    requireLogger().error(`SECURITY: Linux update signature INVALID — ${result.reason}`);
    updateSplashError('Update blocked: signature verification failed');
    sendToRenderer('update:error', {
      message: `Update blocked: ${result.reason}`,
      securityEvent: true,
      subtype: 'signature-failure',
    });
    return;
  }
  requireLogger().warn(`Linux update could not be verified — ${result.reason}`);
  updateSplashError('Update could not be verified — try again later');
  sendToRenderer('update:error', {
    message: `Update could not be verified right now (${result.reason}). Please try again later, or download the latest from the official Releases page.`,
  });
}

/**
 * Platform pre-install signature gate. Returns true only when installation may
 * proceed; every refusal (including a selection that changed during the await)
 * has already been reported to the renderer.
 */
async function passesPlatformInstallGate(
  downloadedFile: string | null,
  abandonStaleSelection: () => boolean
): Promise<boolean> {
  if (process.platform === 'win32') {
    // Cached NSIS bytes bypass the download hook in electron-updater 6.8.9.
    // Re-establish trust in this session using the binary-owned publisher policy.
    const verdict = downloadedFile
      ? await verifyWindowsUpdateSignature(downloadedFile, ALLOWED_WINDOWS_PUBLISHERS)
      : 'unavailable';
    if (abandonStaleSelection()) return false;
    if (verdict !== 'verified') {
      refuseUnverifiedWindowsInstaller(verdict);
      return false;
    }
  }
  if (process.platform === 'linux') {
    const result = await verifyLinuxUpdateBeforeInstall(downloadedFile);
    if (abandonStaleSelection()) return false;
    if (!result.verified) {
      refuseUnverifiedLinuxInstaller(result);
      return false;
    }
  }
  return true;
}

/**
 * Safe quit-and-install: creates a backup + sentinel before handing off
 * to electron-updater's quitAndInstall(). If backup creation fails the
 * install is aborted and the user is notified (#384).
 */
export async function safeQuitAndInstall(): Promise<void> {
  if (installInFlight || downloadsInFlight > 0) {
    sendToRenderer('update:error', {
      message: 'An update operation is in progress. Please try again later.',
    });
    return;
  }
  const currentVersion = app.getVersion();
  const targetVersion = pendingUpdateVersion;
  const downloadedFile = pendingDownloadedFile;
  const generation = pendingGeneration;
  if (!targetVersion) {
    logger?.error('safeQuitAndInstall called but no pending update version');
    sendToRenderer('update:error', { message: 'No update downloaded.' });
    return;
  }
  // Own the install before the first await; public downloads cannot overlap it.
  installInFlight = true;
  const selectionIsCurrent = (): boolean =>
    pendingGeneration === generation &&
    pendingUpdateVersion === targetVersion &&
    pendingDownloadedFile === downloadedFile;
  const abandonStaleSelection = (): boolean => {
    if (selectionIsCurrent()) return false;
    discardPreparedUpdate(requireLogger(), false);
    logger?.warn('Update selection changed; install aborted');
    sendToRenderer('update:error', {
      message: 'The downloaded update changed. Please try installing again.',
    });
    return true;
  };
  try {
    logger?.info(`Preparing safe update: v${currentVersion} → v${targetVersion}`);
    const prepared = await prepareForUpdate(currentVersion, targetVersion, requireLogger());
    if (abandonStaleSelection()) return;
    if (!prepared) {
      logger?.error('Failed to prepare safety backup, aborting update install');
      sendToRenderer('update:error', {
        message: 'Failed to create safety backup. Update aborted — you can retry.',
      });
      return;
    }
    if (!(await passesPlatformInstallGate(downloadedFile, abandonStaleSelection))) return;

    if (abandonStaleSelection()) return;
    logger?.info('Safety sentinel written, proceeding with quitAndInstall()');
    // No await between final selection check, veto release, and installer handoff.
    releaseQuitVetoForUpdate?.();
    autoUpdater.quitAndInstall();
  } catch {
    logger?.error('Update installation aborted: safety operation unavailable');
    sendToRenderer('update:error', {
      message: 'Update installation could not be prepared. Please try again later.',
    });
  } finally {
    installInFlight = false;
  }
}

export function getAllowPrerelease(): boolean {
  return autoUpdater.allowPrerelease;
}

export function setAllowPrerelease(enabled: boolean): void {
  autoUpdater.allowPrerelease = enabled;
  writeUpdatePrefs({ allowPrerelease: enabled });
}

// ─── Helpers ──────────────────────────────────────────────────────────

function formatBytes(bytes: number): string {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/** Compatibility hook after login; packaged builds keep the static recovery feed. */
export function setUpdateFeedUrl(_apiBase: string): void {
  if (!app.isPackaged) {
    console.debug('[updater] setUpdateFeedUrl: no-op in development mode');
    return;
  }
  ensureUpdaterReady();
  configureUpdateFeed();
  startScheduledChecks();
}

/** Stop scheduled update checks (called on app quit). */
export function stopAutoUpdater(): void {
  if (checkInterval) {
    clearInterval(checkInterval);
    checkInterval = null;
  }
}

/** Get the update logger instance (available after initAutoUpdater or ensureUpdaterReady). */
export function getUpdateLogger(): UpdateLogger | null {
  return logger;
}

/** Get the path to today's update log file. */
export function getUpdateLogPath(): string | null {
  return logger?.getLogPath() ?? null;
}
