import fs from 'node:fs';
import path from 'node:path';
import { app } from 'electron';

/**
 * Consolidate any legacy userData tree into the pinned canonical directory.
 *
 * Background: userData is now pinned to the spaceless "ConcordVoice" by
 * pinUserDataPath.ts, independent of productName. Earlier builds resolved it to
 * the spaced "Concord Voice" (when productName was spaced) or already to
 * "ConcordVoice" (the #385 convention). The v0.1.38 productName revert split
 * users across both trees. This migration consolidates them onto the pinned dir.
 *
 * Best-effort + non-destructive: the RSA keypair is server-escrowed (ADR-0020),
 * so a wrong guess costs at most one re-login, never data — we archive, never
 * delete. Must run after pinUserDataPath.ts and before any userData read.
 */
const CANONICAL_DIR_NAME = 'ConcordVoice';
const LEGACY_SPACED_NAME = 'Concord Voice';
// Files whose mtime indicates which tree most recently held a live session or
// E2EE key state. secure-e2ee.dat is included so a tree holding key material but
// a stale/absent token still registers as live (Gitar + e2ee-reviewer, #1314).
const LIVENESS_PROBE_FILES = ['secure-token.dat', 'secure-e2ee.dat', 'token-meta.json'];

/** Parent dir that contains the userData folder (e.g. ~/Library/Application Support). */
export function resolveUserDataParent(): string {
  return path.dirname(app.getPath('userData'));
}

/** Narrow an unknown thrown value to a log-safe message (never key material). */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : String(err);
}

function isDirectory(p: string): boolean {
  try {
    return fs.statSync(p).isDirectory();
  } catch {
    return false;
  }
}

/** Newest mtime (ms) among known liveness-probe files in `dir`; 0 if none. */
function livenessMtime(dir: string): number {
  let newest = 0;
  for (const file of LIVENESS_PROBE_FILES) {
    try {
      const m = fs.statSync(path.join(dir, file)).mtimeMs;
      if (m > newest) newest = m;
    } catch {
      // probe file absent — skip
    }
  }
  return newest;
}

/** Archive a dir out of the way (rename, never delete). Returns dest or null. */
function archiveDir(dir: string): string | null {
  const stamp = new Date().toISOString().replace(/[-:]/g, '').replace('T', '-').slice(0, 15); // YYYYMMDD-HHMMSS, mirroring the Chromium Cache.bak-<ts> convention
  const dest = `${dir}.bak-${stamp}`;
  try {
    fs.renameSync(dir, dest);
    return dest;
  } catch (err: unknown) {
    console.warn(`[migration] Failed to archive "${path.basename(dir)}": ${errText(err)}`);
    return null;
  }
}

/** Move a directory, warning (not throwing) on failure. Returns whether it moved. */
function moveDir(from: string, to: string): boolean {
  try {
    fs.renameSync(from, to);
    return true;
  } catch (err: unknown) {
    console.warn(`[migration] Failed to consolidate userData directory: ${errText(err)}`);
    return false;
  }
}

/** Case A — only the legacy spaced tree holds data: move it into the pinned dir. */
function consolidateLoneLegacy(legacy: string, target: string): void {
  if (moveDir(legacy, target)) {
    console.debug(`[migration] Consolidated "${LEGACY_SPACED_NAME}" → "${CANONICAL_DIR_NAME}"`);
  }
}

/** Case B — both trees exist: keep the live one (newest token mtime), archive the stale one. */
function consolidateConflict(legacy: string, target: string): void {
  if (livenessMtime(legacy) > livenessMtime(target)) {
    promoteLegacy(legacy, target);
    return;
  }
  const archived = archiveDir(legacy);
  if (archived !== null) {
    console.debug(
      `[migration] Kept live "${CANONICAL_DIR_NAME}"; archived stale "${path.basename(archived)}"`
    );
  }
}

/**
 * Promote the newer legacy tree into the pinned dir: archive the stale canonical,
 * then move legacy in. If the move fails AFTER the archive succeeded, roll the
 * archive back so the pinned userData path is never left missing (which would
 * force an avoidable re-login). Flagged by Gitar + Copilot on #1314.
 */
function promoteLegacy(legacy: string, target: string): void {
  const archived = archiveDir(target);
  if (archived === null) return; // archive failed → both trees intact, bail
  if (moveDir(legacy, target)) {
    console.debug(
      `[migration] Consolidated newer "${LEGACY_SPACED_NAME}" → "${CANONICAL_DIR_NAME}" ` +
        `(archived stale "${path.basename(archived)}")`
    );
    return;
  }
  // Move failed after a successful archive — restore the canonical dir.
  try {
    fs.renameSync(archived, target);
    console.warn(`[migration] Move failed after archive; restored "${CANONICAL_DIR_NAME}"`);
  } catch (err: unknown) {
    console.warn(
      `[migration] Move + rollback failed; "${CANONICAL_DIR_NAME}" remains archived as ` +
        `"${path.basename(archived)}": ${errText(err)}`
    );
  }
}

export function migrateUserData(): void {
  const target = app.getPath('userData'); // pinned: <parent>/ConcordVoice
  const legacy = path.join(path.dirname(target), LEGACY_SPACED_NAME);

  if (!isDirectory(legacy)) return; // only canonical (or neither) — nothing to consolidate

  if (isDirectory(target)) {
    consolidateConflict(legacy, target); // both exist — the post-revert split
  } else {
    consolidateLoneLegacy(legacy, target); // only the legacy spaced tree has data
  }
}
