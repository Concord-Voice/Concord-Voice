// Pins Electron's `userData` directory to a STABLE, spaceless "ConcordVoice"
// name, decoupled from `productName` (the display name stays "Concord Voice").
//
// WHY A SEPARATE MODULE IMPORTED FIRST: app.getPath('userData') is read at
// MODULE-LOAD time by tokenManager.ts (secure-token.dat / secure-e2ee.dat),
// machineId.ts, and updater.ts. In ESM, those imports fully evaluate before
// main.ts's body executes, so a setPath() call in main.ts's body would be too
// late — they'd capture the unpinned path. This module's side effect runs as
// the FIRST import in main.ts, before any userData-reading module.
//
// Generalizes the precedent in updateLogger.ts, which already hardcodes the log
// dir to "ConcordVoice" independent of app.getName(). See ADR-0020 D2 and
// [internal]rules/electron.md "Path-identity pinning".
import { app } from 'electron';
import path from 'node:path';

/** The canonical, immutable userData/cache/logs directory name. */
export const PINNED_USER_DATA_DIR = 'ConcordVoice';

/** Pin userData to <appData>/ConcordVoice. Idempotent; safe before app ready. */
export function pinUserDataPath(): void {
  // Honor an explicit --user-data-dir (multi-instance dev, e.g. concord-dev.sh
  // --clients N). Electron applies that switch to `userData` before app code
  // runs; pinning over it would collapse every instance onto one userData (and
  // one single-instance lock), so only one client could launch. macOS resolves
  // app.getPath('appData') via the Cocoa API and ignores $HOME, so --user-data-dir
  // is the ONLY reliable per-instance isolation. Production never passes the
  // switch, so the pin below still applies there unchanged (ADR-0020).
  const hasExplicitUserDataDir = process.argv.some(
    (arg) => arg === '--user-data-dir' || arg.startsWith('--user-data-dir=')
  );
  if (hasExplicitUserDataDir) return;

  app.setPath('userData', path.join(app.getPath('appData'), PINNED_USER_DATA_DIR));
}

// Load-bearing side effect: runs the instant this module is imported, which —
// because it is main.ts's first import — is before any userData-reading module
// is evaluated.
pinUserDataPath();
