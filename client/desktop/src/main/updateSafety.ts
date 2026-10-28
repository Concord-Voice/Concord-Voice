import fs from 'node:fs';
import path from 'node:path';
import os from 'node:os';
import { UPDATER_CACHE_DIR_NAME } from '../shared/updaterCache';
import { app } from 'electron';
import type { UpdateLogger } from './updateLogger';

// ─── Types ───────────────────────────────────────────────────────────────

export interface UpdateSentinel {
  fromVersion: string;
  toVersion: string;
  backupPath: string; // empty string if no backup (macOS/Windows)
  timestamp: number; // ms since epoch
  platform: string; // 'win32' | 'darwin' | 'linux'
}

export type SentinelResultType = 'none' | 'success' | 'rollback' | 'stale' | 'unknown';

export interface SentinelResult {
  type: SentinelResultType;
  fromVersion?: string;
  toVersion?: string;
  backupPath?: string;
}

interface CleanupManifest {
  backupPath: string;
  cacheDirPath?: string; // Legacy metadata only; never a deletion target.
  logDir: string;
  timestamp: number;
}

// ─── Constants ───────────────────────────────────────────────────────────

const SENTINEL_FILENAME = 'update-sentinel.json';
const CLEANUP_FILENAME = 'update-cleanup.json';
/** Sentinel older than 1 hour is considered stale (update likely crashed). */
const STALE_THRESHOLD_MS = 60 * 60 * 1000;

// ─── Helpers ─────────────────────────────────────────────────────────────

function sentinelPath(): string {
  return path.join(app.getPath('userData'), SENTINEL_FILENAME);
}

function cleanupPath(): string {
  return path.join(app.getPath('userData'), CLEANUP_FILENAME);
}

function safeUnlink(filePath: string): boolean {
  try {
    fs.unlinkSync(filePath);
    return true;
  } catch {
    return false; // File may not exist or be locked
  }
}

function safeReadJson<T>(filePath: string): T | null {
  try {
    return JSON.parse(fs.readFileSync(filePath, 'utf-8')) as T;
  } catch {
    return null;
  }
}

/**
 * Determine if a log file is trivial (empty or contains only the routine
 * "Application quitting" message). All other content — including short
 * update audit logs like "Update available" or "Downloaded" — is preserved
 * for troubleshooting (#383).
 */
function isLogFileTrivial(filePath: string): boolean {
  try {
    const content = fs.readFileSync(filePath, 'utf-8').trim();
    if (content.length === 0) return true;

    // Only prune if every line is a known routine message
    const lines = content.split('\n');
    return lines.every((line) => line.includes('Application quitting'));
  } catch {
    return false; // Can't read → don't delete
  }
}

/** Match electron-updater 6.8.9's platform cache root, independent of userData. */
export function resolvePendingUpdateCacheDir(
  platform: NodeJS.Platform = process.platform,
  home: string = os.homedir(),
  env: NodeJS.ProcessEnv = process.env
): string {
  let root: string;
  if (platform === 'win32') root = env.LOCALAPPDATA || path.join(home, 'AppData', 'Local');
  else if (platform === 'darwin') root = path.join(home, 'Library', 'Caches');
  else root = env.XDG_CACHE_HOME || path.join(home, '.cache');
  return path.join(root, UPDATER_CACHE_DIR_NAME, 'pending');
}

/** lstat both app-specific boundaries so a redirected parent cannot widen deletion. */
function inspectPendingBoundaries(
  pending: string,
  logger: UpdateLogger
): 'safe' | 'absent' | 'failed' {
  try {
    for (const directory of [path.dirname(pending), pending]) {
      const stat = fs.lstatSync(directory);
      if (stat.isSymbolicLink() || !stat.isDirectory()) {
        logger.warn('Pending update cache cleanup failed: unsafe cache directory');
        return 'failed';
      }
    }
    return 'safe';
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === 'ENOENT') {
      logger.info('Pending update cache absent; nothing to purge');
      return 'absent';
    }
    logger.warn('Pending update cache cleanup failed: directory inspection unavailable');
    return 'failed';
  }
}

/** Remove one pending entry without following symlinks. Returns false on failure. */
function removePendingEntry(child: string, logger: UpdateLogger): boolean {
  try {
    const stat = fs.lstatSync(child);
    if (stat.isDirectory() && !stat.isSymbolicLink()) {
      fs.rmSync(child, { recursive: true, force: true });
    } else {
      fs.unlinkSync(child);
    }
    return true;
  } catch {
    logger.warn('Pending update cache cleanup failed: could not remove an entry');
    return false;
  }
}

/** Only pending download bytes are disposable; cache siblings belong to the updater. */
export function purgePendingUpdateCache(logger: UpdateLogger): 'absent' | 'purged' | 'failed' {
  const pending = resolvePendingUpdateCacheDir();
  if (!path.isAbsolute(pending)) {
    logger.warn('Pending update cache cleanup failed: non-absolute cache root');
    return 'failed';
  }
  const boundaries = inspectPendingBoundaries(pending, logger);
  if (boundaries !== 'safe') return boundaries;

  let failed = false;
  try {
    for (const entry of fs.readdirSync(pending)) {
      if (!removePendingEntry(path.join(pending, entry), logger)) failed = true;
    }
  } catch {
    failed = true;
    logger.warn('Pending update cache cleanup failed: could not read entries');
  }
  if (failed) return 'failed';
  logger.info('Purged pending update cache');
  return 'purged';
}

/** A stale install continuation may remove its sentinel, but not its successor's bytes. */
export function discardPreparedUpdate(logger: UpdateLogger, purgePending = true): void {
  const sentinel = sentinelPath();
  if (!safeUnlink(sentinel) && fs.existsSync(sentinel)) {
    logger.warn('Could not remove aborted update sentinel');
  }
  if (purgePending) {
    purgePendingUpdateCache(logger);
    purgeUpdaterMetadata(logger);
  }
}

/** Remove stale electron-updater metadata files from userData. */
function purgeUpdaterMetadata(logger: UpdateLogger): void {
  const userData = app.getPath('userData');
  try {
    const entries = fs.readdirSync(userData);
    for (const entry of entries) {
      if (entry.startsWith('pending-update-') || entry === 'update-info.json') {
        const fullPath = path.join(userData, entry);
        if (safeUnlink(fullPath)) {
          logger.info(`Removed stale metadata: ${entry}`);
        }
      }
    }
  } catch {
    // Best effort
  }
}

// ─── Public API ──────────────────────────────────────────────────────────

/**
 * Check for an update sentinel file on startup.
 * Determines if the last update succeeded, failed, or is stale.
 */
export function checkUpdateSentinel(logger: UpdateLogger): SentinelResult {
  const sPath = sentinelPath();

  const sentinel = safeReadJson<UpdateSentinel>(sPath);
  if (!sentinel) {
    // No sentinel or corrupted JSON
    if (fs.existsSync(sPath)) {
      // File exists but couldn't parse → corrupted
      logger.error('Corrupted update sentinel file, removing');
      safeUnlink(sPath);
    }
    return { type: 'none' };
  }

  const currentVersion = app.getVersion();

  // Stale sentinel (>1h old) → cleanup regardless
  if (Date.now() - sentinel.timestamp > STALE_THRESHOLD_MS) {
    logger.warn(
      `Stale update sentinel found (>1h old): v${sentinel.fromVersion} → v${sentinel.toVersion}`
    );
    safeUnlink(sPath);
    return {
      type: 'stale',
      fromVersion: sentinel.fromVersion,
      toVersion: sentinel.toVersion,
      backupPath: sentinel.backupPath,
    };
  }

  if (currentVersion === sentinel.toVersion) {
    // New version launched successfully
    return {
      type: 'success',
      fromVersion: sentinel.fromVersion,
      toVersion: sentinel.toVersion,
      backupPath: sentinel.backupPath,
    };
  }

  if (currentVersion === sentinel.fromVersion) {
    // Old version is still running → update failed
    return {
      type: 'rollback',
      fromVersion: sentinel.fromVersion,
      toVersion: sentinel.toVersion,
      backupPath: sentinel.backupPath,
    };
  }

  // Unexpected version
  logger.warn(
    `Unexpected version v${currentVersion} with sentinel v${sentinel.fromVersion} → v${sentinel.toVersion}`
  );
  safeUnlink(sPath);
  return {
    type: 'unknown',
    fromVersion: sentinel.fromVersion,
    toVersion: sentinel.toVersion,
  };
}

/**
 * Prepare for an update: create platform-specific backup and write sentinel.
 * Returns true if preparation succeeded and it's safe to call quitAndInstall().
 */
export async function prepareForUpdate(
  fromVersion: string,
  toVersion: string,
  logger: UpdateLogger
): Promise<boolean> {
  let backupPath = '';

  // Linux AppImage: back up the current binary
  if (process.platform === 'linux' && process.env.APPIMAGE) {
    const appImagePath = process.env.APPIMAGE;
    backupPath = `${appImagePath}.backup`;

    logger.info(`Backing up AppImage: ${appImagePath} → ${backupPath}`);
    try {
      fs.copyFileSync(appImagePath, backupPath);
    } catch (err) {
      logger.error(`Failed to back up AppImage: ${(err as Error).message}`);
      return false;
    }
  }

  // Write sentinel file
  const sentinel: UpdateSentinel = {
    fromVersion,
    toVersion,
    backupPath,
    timestamp: Date.now(),
    platform: process.platform,
  };

  try {
    fs.writeFileSync(sentinelPath(), JSON.stringify(sentinel, null, 2), 'utf-8');
    logger.info(
      `Update sentinel written: v${fromVersion} → v${toVersion} (backup: ${backupPath || 'none'})`
    );
    return true;
  } catch (err) {
    logger.error(`Failed to write update sentinel: ${(err as Error).message}`);
    // Clean up backup if sentinel write failed
    if (backupPath) safeUnlink(backupPath);
    return false;
  }
}

/**
 * Finalize a successful update: thorough cleanup of all update artifacts.
 * Called when the new version launches and the sentinel confirms success.
 */
export async function finalizeUpdate(
  logger: UpdateLogger,
  sentinel: SentinelResult
): Promise<void> {
  logger.info(`Finalizing successful update: v${sentinel.fromVersion} → v${sentinel.toVersion}`);

  // Write deferred cleanup manifest as safety net (in case we crash mid-cleanup)
  const logDir = logger.getLogDir();
  const manifest: CleanupManifest = {
    backupPath: sentinel.backupPath || '',
    logDir,
    timestamp: Date.now(),
  };
  try {
    fs.writeFileSync(cleanupPath(), JSON.stringify(manifest), 'utf-8');
  } catch {
    // Non-fatal — cleanup will still proceed
  }

  // 1. Delete sentinel file
  if (safeUnlink(sentinelPath())) {
    logger.info('Removed update sentinel');
  }

  // 2. Delete backup (Linux AppImage)
  if (sentinel.backupPath) {
    if (safeUnlink(sentinel.backupPath)) {
      logger.info(`Removed backup: ${sentinel.backupPath}`);
    }
  }

  // 3. Purge electron-updater download cache
  const cacheOutcome = purgePendingUpdateCache(logger);

  // 4. Purge empty/trivial log files
  pruneTrivialLogs(logDir, logger);

  // 5. Remove stale electron-updater metadata from userData
  purgeUpdaterMetadata(logger);

  // 6. Remove cleanup manifest (cleanup completed successfully)
  if (cacheOutcome === 'failed') {
    logger.warn('Post-update cleanup incomplete; pending cache cleanup will retry');
  } else {
    safeUnlink(cleanupPath());
    logger.info('Post-update cleanup complete');
  }
}

/**
 * Finalize a failed update (rollback path): partial cleanup, preserve all logs.
 */
export async function finalizeRollback(
  logger: UpdateLogger,
  sentinel: SentinelResult
): Promise<void> {
  logger.warn(
    `Update to v${sentinel.toVersion} failed, cleaning up (preserving logs for forensics)`
  );

  // 1. Delete sentinel file
  safeUnlink(sentinelPath());

  // 2. Purge download cache (the downloaded update was bad)
  const cacheOutcome = purgePendingUpdateCache(logger);

  // 3. Remove stale electron-updater metadata
  purgeUpdaterMetadata(logger);

  // Do NOT delete logs — they're the forensic trail
  // Do NOT delete backup — may still be needed for manual recovery

  if (cacheOutcome === 'failed') logger.warn('Rollback cleanup incomplete (logs preserved)');
  else logger.info('Rollback cleanup complete (logs preserved)');
}

/**
 * Safety net: run deferred cleanup from a previous incomplete finalization.
 * Called on every startup; only acts if a cleanup manifest exists and is stale (>1h).
 */
export async function runDeferredCleanup(logger: UpdateLogger): Promise<void> {
  const manifest = safeReadJson<CleanupManifest>(cleanupPath());
  if (!manifest) return;

  // Don't act on fresh manifests — the current finalization may still be running
  if (Date.now() - manifest.timestamp < STALE_THRESHOLD_MS) return;

  logger.info('Running deferred post-update cleanup from previous session');

  if (manifest.backupPath) safeUnlink(manifest.backupPath);
  const cacheOutcome = purgePendingUpdateCache(logger);
  if (manifest.logDir) {
    pruneTrivialLogs(manifest.logDir, logger);
  }
  purgeUpdaterMetadata(logger);

  if (cacheOutcome === 'failed') {
    logger.warn('Deferred cleanup incomplete; pending cache cleanup will retry');
  } else {
    safeUnlink(cleanupPath());
    logger.info('Deferred cleanup complete');
  }
}

// ─── Internal helpers ────────────────────────────────────────────────────

/** Remove empty or trivial log files from the log directory. */
function pruneTrivialLogs(logDir: string, logger: UpdateLogger): void {
  try {
    const entries = fs.readdirSync(logDir);
    for (const entry of entries) {
      if (!entry.startsWith('update-') || !entry.endsWith('.log')) continue;
      const fullPath = path.join(logDir, entry);
      if (isLogFileTrivial(fullPath)) {
        if (safeUnlink(fullPath)) {
          logger.info(`Pruned trivial log: ${entry}`);
        }
      }
    }
  } catch {
    // Best effort — log directory may not exist
  }
}
