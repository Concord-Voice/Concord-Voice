import fs from 'node:fs';
import path from 'node:path';
import { app } from 'electron';

// Retention: keep the 10 most recent daily log files
const MAX_LOG_FILES = 10;
const LOG_FILE_PREFIX = 'update-';
const LOG_FILE_EXT = '.log';

/**
 * Logger interface compatible with electron-updater's `autoUpdater.logger`.
 * Also used by updateSafety.ts for rollback/cleanup logging.
 */
export interface UpdateLogger {
  info(message: string): void;
  warn(message: string): void;
  error(message: string): void;
  debug(message: string): void;
  /** Synchronous flush — no-op for sync writer, exists for interface completeness. */
  flush(): void;
  /** Returns the current daily log file path. */
  getLogPath(): string;
  /** Returns the log directory path. */
  getLogDir(): string;
}

/**
 * Resolve the platform-specific log directory per issue #383 spec:
 *   Windows: %LocalAppData%\ConcordVoice\logs\
 *   macOS:   ~/Library/Logs/ConcordVoice/
 *   Linux:   $XDG_DATA_HOME/ConcordVoice/logs/ (fallback ~/.local/share/ConcordVoice/logs/)
 */
export function resolveLogDir(platform: string = process.platform): string {
  switch (platform) {
    case 'win32': {
      const localAppData =
        process.env.LOCALAPPDATA || path.join(app.getPath('home'), 'AppData', 'Local');
      return path.join(localAppData, 'ConcordVoice', 'logs');
    }
    case 'darwin':
      return path.join(app.getPath('home'), 'Library', 'Logs', 'ConcordVoice');
    default: {
      // Linux and other Unix-like
      const xdgData =
        process.env.XDG_DATA_HOME || path.join(app.getPath('home'), '.local', 'share');
      return path.join(xdgData, 'ConcordVoice', 'logs');
    }
  }
}

/** Generate today's log filename: update-YYYY-MM-DD.log */
function todayLogFilename(): string {
  const now = new Date();
  const yyyy = now.getFullYear();
  const mm = String(now.getMonth() + 1).padStart(2, '0');
  const dd = String(now.getDate()).padStart(2, '0');
  return `${LOG_FILE_PREFIX}${yyyy}-${mm}-${dd}${LOG_FILE_EXT}`;
}

/** Prune old log files, keeping only the most recent MAX_LOG_FILES. */
function pruneOldLogs(logDir: string): void {
  try {
    const entries = fs
      .readdirSync(logDir)
      .filter((f) => f.startsWith(LOG_FILE_PREFIX) && f.endsWith(LOG_FILE_EXT))
      .sort(); // Lexicographic sort on YYYY-MM-DD = chronological order

    if (entries.length <= MAX_LOG_FILES) return;

    const toDelete = entries.slice(0, entries.length - MAX_LOG_FILES);
    for (const file of toDelete) {
      try {
        fs.unlinkSync(path.join(logDir, file));
      } catch {
        // Best-effort deletion — don't crash if a file is locked
      }
    }
  } catch {
    // Directory may not exist yet (first run) — pruning is a no-op
  }
}

/** Format a log line: [ISO-8601] [LEVEL] message */
function formatLine(level: string, message: string): string {
  return `[${new Date().toISOString()}] [${level}] ${message}\n`;
}

/**
 * Create a structured file logger for the update lifecycle.
 *
 * - Writes synchronously (appendFileSync) for crash safety
 * - Also writes to console for DevTools/stdout visibility
 * - One file per day, 10-file rotation
 */
export function createUpdateLogger(): UpdateLogger {
  const logDir = resolveLogDir();

  // Ensure log directory exists
  try {
    fs.mkdirSync(logDir, { recursive: true });
  } catch {
    // If we can't create the directory, we'll still work (console-only)
    // eslint-disable-next-line no-restricted-syntax -- logDir is a resolved filesystem path string, not an Error; no err.cause chain to propagate
    console.error('[updateLogger] Failed to create log directory:', logDir);
  }

  // Prune old log files on init
  pruneOldLogs(logDir);

  function writeLog(level: string, message: string): void {
    const line = formatLine(level, message);

    // Always write to console
    switch (level) {
      case 'ERROR':
        console.error(`[updater] ${message}`);
        break;
      case 'WARN':
        console.warn(`[updater] ${message}`);
        break;
      default:
        // eslint-disable-next-line no-console -- updateLogger is the logger module itself; this console.log is the intentional INFO/DEBUG emitter that the module's API is built around (mirrors console.error/.warn branches above)
        console.log(`[updater] ${message}`);
        break;
    }

    // Write to file (sync for crash safety)
    try {
      const filePath = path.join(logDir, todayLogFilename());
      fs.appendFileSync(filePath, line);
    } catch {
      // File write failed — console output is the fallback
      console.error('[updateLogger] Failed to write to log file');
    }
  }

  return {
    info: (message: string) => writeLog('INFO', message),
    warn: (message: string) => writeLog('WARN', message),
    error: (message: string) => writeLog('ERROR', message),
    debug: (message: string) => writeLog('DEBUG', message),
    flush: () => {
      // No-op: appendFileSync is already synchronous, nothing to flush
    },
    getLogPath: () => path.join(logDir, todayLogFilename()),
    getLogDir: () => logDir,
  };
}
