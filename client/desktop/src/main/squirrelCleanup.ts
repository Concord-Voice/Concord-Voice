import fs from 'node:fs';
import path from 'node:path';
import { execFileSync } from 'node:child_process';
import type { UpdateLogger } from './updateLogger';

/** Squirrel's per-user uninstall registry key, written by its Setup.exe. */
const SQUIRREL_UNINSTALL_KEY = String.raw`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\ConcordVoice`;

/**
 * Directories under the ConcordVoice identity root that must NEVER be removed.
 * `app` is the running NSIS install; `pending` is electron-updater's cache, which
 * holds the in-flight installer and current.blockmap.
 */
const PROTECTED = new Set(['app', 'pending']);

/**
 * NTFS is case-INSENSITIVE, and `path.win32.relative` returns the original casing
 * rather than a normalized one — so an exact-case `PROTECTED.has(x)` would let
 * `…\ConcordVoice\App` or `…\Pending` through while naming the very directory the
 * guard exists to protect. Compare folded.
 */
function isProtected(segment: string): boolean {
  return PROTECTED.has(segment.toLowerCase());
}

/**
 * `catch (err)` binds `unknown`. A non-Error throw would render as "undefined" with a
 * bare `(err as Error).message`; observability.md's documented form falls back to a
 * stable identifier instead.
 */
function errText(err: unknown): string {
  return err instanceof Error ? err.message : 'unknown-error';
}

/** Squirrel wrote its unpacked versions as app-N.N.N alongside the execution stub. */
const VERSIONED_APP_DIR = /^app-\d+\.\d+\.\d+/;

/**
 * Validated absolute path to reg.exe, or null if it cannot be trusted.
 *
 * Resolved from %SystemRoot% rather than invoked as bare `reg`, which would search
 * %PATH% — a user-writable directory ahead of System32 could shadow it with a planted
 * reg.exe and get it executed by us (sonar typescript:S4036).
 */
function regExePath(): string | null {
  // %SystemRoot% is itself user-writable via HKCU\Environment, so an attacker who
  // already has user-level execution could point it at a directory they control and
  // have us run their reg.exe every launch. Require a drive-rooted absolute path and
  // an existing target; otherwise skip the key deletion entirely — it is cosmetic and
  // already tolerates failure.
  const root = process.env.SystemRoot ?? String.raw`C:\Windows`;
  if (!/^[A-Za-z]:\\/.test(root)) return null;
  const resolved = path.join(root, 'System32', 'reg.exe');
  return fs.existsSync(resolved) ? resolved : null;
}

/** Fixed-name Squirrel artifacts, independent of what is on disk. */
function fixedTargets(localAppData: string, root: string): string[] {
  const targets = [
    path.join(root, 'Update.exe'),
    path.join(root, 'packages'),
    path.join(root, 'Concord Voice.exe'), // Squirrel execution stub
    path.join(localAppData, 'SquirrelTemp'),
  ];

  // Squirrel's Start Menu entry lives in a company subfolder
  // ($SMPROGRAMS\Concord Voice LLC\Concord Voice.lnk); electron-builder's NSIS writes
  // $SMPROGRAMS\${SHORTCUT_NAME}.lnk with no subfolder (app-builder-lib
  // templates/nsis/include/installer.nsh:106), so the two do NOT collide and the
  // Squirrel folder orphans. Remove the whole company folder.
  //
  // The DESKTOP shortcut is deliberately NOT touched: both installers write
  // $DESKTOP\Concord Voice.lnk, so NSIS overwrites it in place and it self-heals.
  // Deleting it here would race that overwrite and could remove the NEW shortcut.
  const appData = process.env.APPDATA;
  if (appData) {
    targets.push(
      path.join(appData, 'Microsoft', 'Windows', 'Start Menu', 'Programs', 'Concord Voice LLC')
    );
  }

  return targets;
}

/** Versioned app-N.N.N directories discovered under the identity root. */
function versionedAppDirs(root: string, logger: UpdateLogger): string[] {
  try {
    if (!fs.existsSync(root)) return [];
    return fs
      .readdirSync(root)
      .filter((entry) => !isProtected(entry) && VERSIONED_APP_DIR.test(entry))
      .map((entry) => path.join(root, entry));
  } catch (err) {
    logger.warn(`Squirrel cleanup: could not enumerate ${root}: ${errText(err)}`);
    return [];
  }
}

/**
 * Remove each target that exists, skipping anything inside a protected sibling.
 *
 * Exported for tests. The PROTECTED guard below is UNREACHABLE in normal operation —
 * `app` and `pending` are absent from the fixed-target list and cannot match
 * VERSIONED_APP_DIR — so it is a backstop against a future edit adding a protected
 * name to the target list, not a live filter. That makes it exactly the kind of code
 * that rots silently: emptying PROTECTED does not fail any end-to-end test. Hence the
 * direct test (repo precedent: renderAppUpdateYaml in scripts/generate-app-update.mts).
 */
export function removeTargets(root: string, targets: string[], logger: UpdateLogger): number {
  let removed = 0;
  for (const target of targets) {
    // Defense in depth: refuse anything that resolves into a protected sibling, even
    // if a future edit adds it to the allowlist by mistake.
    const rel = path.relative(root, target).split(path.sep)[0];
    if (isProtected(rel)) continue;

    try {
      if (!fs.existsSync(target)) continue;
      fs.rmSync(target, { recursive: true, force: true });
      removed += 1;
    } catch (err) {
      logger.warn(`Squirrel cleanup: failed to remove ${target}: ${errText(err)}`);
    }
  }
  return removed;
}

/** Delete Squirrel's single HKCU uninstall key. Absent key exits non-zero — expected. */
function removeUninstallKey(logger: UpdateLogger): void {
  const reg = regExePath();
  if (!reg) {
    logger.info('Squirrel cleanup: reg.exe not resolvable from %SystemRoot%; skipping key removal');
    return;
  }
  try {
    // execFileSync with a validated absolute path, never shell: true
    // ([internal]rules/electron.md).
    execFileSync(reg, ['delete', SQUIRREL_UNINSTALL_KEY, '/f'], { stdio: 'ignore' });
  } catch (err) {
    logger.info(`Squirrel cleanup: uninstall key not removed: ${errText(err)}`);
  }
}

/**
 * Remove orphaned Squirrel.Windows artifacts left behind by the NSIS migration (#2402).
 *
 * Runs from the INSTALLED APP on a later launch, never from the installer. An
 * installer invoking `Update.exe --uninstall` would recursive-delete ConcordVoice\
 * while itself executing from ConcordVoice\pending\ — reproducing the exact
 * UnauthorizedAccessException this migration exists to fix.
 *
 * Operates on an explicit allowlist, never a directory wipe. Do NOT reuse
 * updateSafety.ts's purgeDirectory, which deletes every entry it finds.
 *
 * Synchronous, idempotent by existence check, and never throws: a cleanup failure
 * must not prevent the app from starting.
 */
export function cleanupSquirrelResidue(logger: UpdateLogger): void {
  if (process.platform !== 'win32') return;

  const localAppData = process.env.LOCALAPPDATA;
  if (!localAppData) {
    // Every other abort path here logs; without this a migration that never ran is
    // indistinguishable from one that ran and found nothing.
    logger.warn('Squirrel cleanup: %LOCALAPPDATA% unset; skipping');
    return;
  }

  const root = path.join(localAppData, 'ConcordVoice');
  const targets = [...fixedTargets(localAppData, root), ...versionedAppDirs(root, logger)];

  const removed = removeTargets(root, targets, logger);

  // Gated on residue actually being found, so this is genuinely one-time rather than
  // a permanent per-launch cost. Ungated, it spawns reg.exe and writes a
  // "key not removed" line on EVERY packaged Windows launch forever, for a migration
  // that completes once. If files were present the key almost certainly was too; if
  // the tree is already clean, the key went with it.
  if (removed > 0) {
    removeUninstallKey(logger);
    logger.info(`Squirrel cleanup: removed ${removed} orphaned artifact(s)`);
  }
}
