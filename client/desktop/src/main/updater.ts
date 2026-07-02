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
import { prepareForUpdate } from './updateSafety';
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
// Captured from the update-downloaded event for the Linux pre-install
// signature gate (#653). The path to the downloaded artifact in the
// electron-updater cache.
let pendingDownloadedFile: string | null = null;

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
      try {
        const chain = await extractChain(filePath);
        const result = verifyChain(chain, publisherNames);
        if (result === null) {
          logger?.info('Runtime chain verification passed');
        } else {
          logger?.error(`SECURITY: chain verification rejected update — ${result}`);
        }
        return result;
      } catch (err) {
        const msg = `extractChain threw: ${(err as Error).message}`;
        logger?.error(`SECURITY: ${msg}`);
        return msg;
      }
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
  });

  autoUpdater.on('update-not-available', (info: UpdateInfo) => {
    requireLogger().info(`Up to date (v${info.version})`);
    updateSplashStatus('All systems go');
    sendToRenderer('update:not-available', { version: info.version });
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
  });

  autoUpdater.on('update-downloaded', (event: UpdateDownloadedEvent) => {
    pendingUpdateVersion = event.version;
    pendingDownloadedFile = event.downloadedFile;
    requireLogger().info(`Downloaded v${event.version} — ready to install`);
    showSplashProgress(100);
    updateSplashStatus('Ready for liftoff');
    sendToRenderer('update:downloaded', { version: event.version });
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
  releaseUpdateQuitVeto?: () => void
): void {
  getWindow = mainWindowGetter;
  releaseQuitVetoForUpdate = releaseUpdateQuitVeto ?? null;

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

export function downloadUpdate(): Promise<string[]> {
  return autoUpdater.downloadUpdate();
}

/**
 * Linux-only pre-install signature gate (#653). Verifies the downloaded
 * artifact's detached Ed25519 signature against the bundled public key.
 * Returns a LinuxVerifyResult — `verified: true` to proceed, otherwise REFUSE.
 * Fail-closed: a missing downloaded path refuses with `kind: 'unavailable'`
 * (a config/availability state, not evidence of tampering). The `.sig` is
 * fetched from the same static public recovery feed the updater already uses.
 */
async function verifyLinuxUpdateBeforeInstall(): Promise<LinuxVerifyResult> {
  if (!pendingDownloadedFile) {
    return { verified: false, reason: 'no downloaded artifact path captured', kind: 'unavailable' };
  }
  const sigUrl = `${UPDATE_ENDPOINT_URL}/${path.basename(pendingDownloadedFile)}.sig`;
  return verifyLinuxArtifact(pendingDownloadedFile, sigUrl, net.fetch);
}

/**
 * Safe quit-and-install: creates a backup + sentinel before handing off
 * to electron-updater's quitAndInstall(). If backup creation fails the
 * install is aborted and the user is notified (#384).
 */
export async function safeQuitAndInstall(): Promise<void> {
  const currentVersion = app.getVersion();
  const targetVersion = pendingUpdateVersion;

  if (!targetVersion) {
    logger?.error('safeQuitAndInstall called but no pending update version');
    sendToRenderer('update:error', { message: 'No update downloaded.' });
    return;
  }

  logger?.info(`Preparing safe update: v${currentVersion} → v${targetVersion}`);

  const prepared = await prepareForUpdate(currentVersion, targetVersion, requireLogger());
  if (!prepared) {
    logger?.error('Failed to prepare safety backup, aborting update install');
    sendToRenderer('update:error', {
      message: 'Failed to create safety backup. Update aborted — you can retry.',
    });
    return;
  }

  // On macOS/Windows no file backup is taken (Squirrel.Mac / NSIS handle the
  // atomic swap); prepareForUpdate() only writes the rollback sentinel. The old
  // wording falsely implied a backup. The sentinel log already reports
  // "(backup: none)" accurately. Linux AppImage is the only path that backs up.

  // Linux has no electron-updater signature hook (verifyUpdateCodeSignature is
  // Windows-only), so verify the downloaded artifact's detached Ed25519
  // signature against the bundled public key here — the single install choke
  // point — BEFORE handing off. Fail-closed: refuse on ANY verification
  // failure. quitAndInstall() follows a successful verify with no intervening
  // await, minimizing the verify->install TOCTOU window (spec §6, #653).
  //
  // The refusal is the same for both failure kinds; the kind only shapes the
  // user message (#653 / Gitar review). A cryptographic verify-false ('tampered')
  // is genuine evidence of an altered artifact → the security banner. An
  // availability failure ('unavailable' — network/IO or missing `.sig`)
  // is retryable → a non-alarming "try again" message, so a transient blip
  // doesn't cry wolf and erode the tamper warning's credibility.
  if (process.platform === 'linux') {
    const result = await verifyLinuxUpdateBeforeInstall();
    if (!result.verified) {
      if (result.kind === 'tampered') {
        requireLogger().error(`SECURITY: Linux update signature INVALID — ${result.reason}`);
        updateSplashError('Update blocked: signature verification failed');
        sendToRenderer('update:error', {
          message: `Update blocked: ${result.reason}`,
          securityEvent: true,
          subtype: 'signature-failure',
        });
      } else {
        requireLogger().warn(`Linux update could not be verified — ${result.reason}`);
        updateSplashError('Update could not be verified — try again later');
        sendToRenderer('update:error', {
          message: `Update could not be verified right now (${result.reason}). Please try again later, or download the latest from the official Releases page.`,
        });
      }
      return;
    }
  }

  logger?.info('Safety sentinel written, proceeding with quitAndInstall()');
  // ponytail: shared install choke point; native before-quit-for-update stays as a macOS fallback.
  releaseQuitVetoForUpdate?.();
  autoUpdater.quitAndInstall();
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
