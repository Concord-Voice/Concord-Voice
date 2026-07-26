import { describe, it, expect, vi, beforeEach } from 'vitest';
import path from 'node:path';

// vi.mock factories are hoisted above module scope, so the mock fns must be
// created with vi.hoisted() — bare top-level consts are not yet initialized when
// the factory runs (repo idiom: attestationSignals.test.ts, loadFailureVisibility.test.ts).
const { rmSync, existsSync, execFileSync, readdirSync } = vi.hoisted(() => ({
  rmSync: vi.fn(),
  existsSync: vi.fn(),
  execFileSync: vi.fn(),
  // Omitting readdirSync from the mock makes the versioned app-N.N.N sweep throw
  // into its catch on EVERY test, so the module's core behaviour — actually finding
  // and deleting old Squirrel version directories — would have zero positive
  // coverage while the suite stayed green.
  readdirSync: vi.fn(),
}));

vi.mock('node:fs', () => ({
  default: { rmSync, existsSync, readdirSync },
  rmSync,
  existsSync,
  readdirSync,
}));
vi.mock('node:child_process', () => ({ default: { execFileSync }, execFileSync }));

// NOTE: no electron mock — squirrelCleanup.ts imports no Electron surface. The
// app.isPackaged gate lives in main.ts (see squirrelCleanupWiring.test.ts), not here.

import { cleanupSquirrelResidue, removeTargets } from '../../../src/main/squirrelCleanup';

const logger = { info: vi.fn(), warn: vi.fn(), error: vi.fn() } as never;
const LOCAL = 'C:\\Users\\test\\AppData\\Local';

/**
 * These tests run on macOS/Linux in CI, where path.join emits FORWARD slashes even
 * for Windows-shaped inputs:
 *   path.join('C:\\Users\\x\\AppData\\Local', 'ConcordVoice') === 'C:\\Users\\x\\AppData\\Local/ConcordVoice'
 *
 * Comparing against all-backslash literals would make the `not.toContain` safety
 * assertions below pass VACUOUSLY — green even if the code deleted app/ or pending/.
 * Normalize both sides so those guards actually bite.
 */
const norm = (p: string) => p.replace(/\\/g, '/');
const normAll = (paths: string[]) => paths.map(norm);

beforeEach(() => {
  // resetAllMocks, NOT clearAllMocks: clear wipes recorded calls but KEEPS
  // implementations, so the `rmSync.mockImplementation(() => { throw })` from the
  // error-path test leaked into every subsequent test — silently making `removed` 0
  // and masking whatever those tests believed they were exercising.
  vi.resetAllMocks();
  process.env.LOCALAPPDATA = LOCAL;
  vi.stubGlobal('process', { ...process, platform: 'win32', env: { LOCALAPPDATA: LOCAL } });
  // Default: root enumerates to nothing. Tests that exercise the versioned sweep
  // override this. Without a default, readdirSync returns undefined and the for..of
  // throws — which the catch absorbs, silently skipping the sweep in every test.
  readdirSync.mockReturnValue([]);
});

describe('cleanupSquirrelResidue (#2402)', () => {
  it('removes known Squirrel artifacts when present', () => {
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed).toContain(norm(`${LOCAL}\\ConcordVoice\\Update.exe`));
    expect(removed).toContain(norm(`${LOCAL}\\ConcordVoice\\packages`));
    expect(removed).toContain(norm(`${LOCAL}\\SquirrelTemp`));
  });

  it('removes versioned Squirrel app-N.N.N directories', () => {
    // The module's core behaviour, and the bulk of the reclaimed disk. Previously
    // uncovered: the fs mock omitted readdirSync, so this sweep threw into its catch
    // on every test and only the FAILURE path was exercised.
    existsSync.mockReturnValue(true);
    readdirSync.mockReturnValue(['app-0.2.32', 'app-0.2.33', 'app', 'pending', 'Update.exe']);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed).toContain(norm(`${LOCAL}\\ConcordVoice\\app-0.2.32`));
    expect(removed).toContain(norm(`${LOCAL}\\ConcordVoice\\app-0.2.33`));
    // 'app' and 'pending' were in the SAME listing and must survive it — this is the
    // realistic adversarial case, since both genuinely sit beside the app-* dirs.
    expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice\\app`));
    expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice\\pending`));
  });

  it('ignores non-Squirrel entries in the identity root', () => {
    existsSync.mockReturnValue(true);
    readdirSync.mockReturnValue(['app-notaversion', 'appdata', 'app-1.2', 'user-notes.txt']);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    // The regex is ^app-\d+\.\d+\.\d+ — a two-segment version or a prose name must
    // not match, or an unrelated user directory gets deleted.
    for (const entry of ['app-notaversion', 'appdata', 'app-1.2', 'user-notes.txt']) {
      expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice\\${entry}`));
    }
  });

  it('survives an unreadable identity root', () => {
    existsSync.mockReturnValue(true);
    readdirSync.mockImplementation(() => {
      throw new Error('EACCES');
    });
    expect(() => cleanupSquirrelResidue(logger)).not.toThrow();
    // The fixed-name targets must still be attempted even if enumeration failed.
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed).toContain(norm(`${LOCAL}\\ConcordVoice\\Update.exe`));
  });

  it('NEVER removes the install dir or the updater cache', () => {
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice\\app`));
    expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice\\pending`));
    // and not the identity root itself, which contains both
    expect(removed).not.toContain(norm(`${LOCAL}\\ConcordVoice`));
  });

  it('PROTECTED backstop refuses app/pending even if added to the target list', () => {
    // This is the ONLY test that actually exercises the PROTECTED guard. In normal
    // operation it is unreachable: `app` and `pending` are absent from the fixed
    // targets and cannot match VERSIONED_APP_DIR, so emptying PROTECTED breaks no
    // end-to-end test. The guard exists for the future edit that DOES put a protected
    // name in the list — which is precisely what this simulates.
    // Build with path.join, NOT literal backslashes: the guard compares
    // path.relative(...).split(path.sep), and on macOS/Linux path.sep is '/'. A
    // backslash-literal path is one opaque segment there, so the guard silently never
    // fires and the test would pass whatever PROTECTED contains — the same
    // separator trap the norm() helper above exists for.
    const root = path.join(LOCAL, 'ConcordVoice');
    existsSync.mockReturnValue(true);
    const removed = removeTargets(
      root,
      [
        path.join(root, 'app'),
        path.join(root, 'pending'),
        path.join(root, 'app', 'Concord Voice.exe'), // nested inside a protected dir
        path.join(root, 'Update.exe'), // legitimate — must still be removed
      ],
      logger
    );
    const deleted = rmSync.mock.calls.map((c) => c[0] as string);
    expect(deleted).toEqual([path.join(root, 'Update.exe')]);
    expect(removed).toBe(1);
  });

  it('guard assertions are not vacuous (meta-test)', () => {
    // Proves the not.toContain guards above can actually fail. If the normalizer or
    // the mock wiring drifts so that `removed` is empty or separator-mismatched,
    // those guards go green while the code deletes anything it likes.
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed.length).toBeGreaterThan(0);
    expect(removed.every((p) => p.startsWith(norm(LOCAL)))).toBe(true);
  });

  it('removes the orphaned Squirrel Start Menu folder but not the Desktop shortcut', () => {
    process.env.APPDATA = 'C:\\Users\\test\\AppData\\Roaming';
    vi.stubGlobal('process', {
      ...process,
      platform: 'win32',
      env: { LOCALAPPDATA: LOCAL, APPDATA: 'C:\\Users\\test\\AppData\\Roaming' },
    });
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const removed = normAll(rmSync.mock.calls.map((c) => c[0] as string));
    expect(removed).toContain(
      norm(
        'C:\\Users\\test\\AppData\\Roaming\\Microsoft\\Windows\\Start Menu\\Programs\\Concord Voice LLC'
      )
    );
    // NSIS overwrites $DESKTOP\Concord Voice.lnk in place; deleting it here would
    // race that overwrite and could remove the NEW shortcut.
    expect(removed.some((p) => p.toLowerCase().includes('desktop'))).toBe(false);
  });

  it('does not spawn reg.exe when there was no residue to remove', () => {
    // Ungated, the registry delete runs on EVERY packaged Windows launch forever —
    // a permanent per-launch process spawn and log line for a one-time migration.
    existsSync.mockReturnValue(false);
    cleanupSquirrelResidue(logger);
    expect(rmSync).not.toHaveBeenCalled();
    expect(execFileSync).not.toHaveBeenCalled();
  });

  it('is a no-op when no residue exists', () => {
    existsSync.mockReturnValue(false);
    cleanupSquirrelResidue(logger);
    expect(rmSync).not.toHaveBeenCalled();
  });

  it('does not throw when a removal fails', () => {
    existsSync.mockReturnValue(true);
    rmSync.mockImplementation(() => {
      throw new Error('EBUSY');
    });
    expect(() => cleanupSquirrelResidue(logger)).not.toThrow();
    expect(logger.warn).toHaveBeenCalled();
  });

  it('does not log the raw error object', () => {
    existsSync.mockReturnValue(true);
    rmSync.mockImplementation(() => {
      throw new Error('EBUSY');
    });
    cleanupSquirrelResidue(logger);
    for (const call of (logger.warn as unknown as { mock: { calls: unknown[][] } }).mock.calls) {
      for (const arg of call) expect(arg).not.toBeInstanceOf(Error);
    }
  });

  it('deletes the Squirrel uninstall key without a shell', () => {
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    expect(execFileSync).toHaveBeenCalled();
    const [, args, opts] = execFileSync.mock.calls[0];
    expect(args[0]).toBe('delete');
    // The LEAF key, asserted exactly. Without this, a regression that widened the
    // target to the parent (…\CurrentVersion\Uninstall) would run `reg delete /f` on
    // EVERY installed program's uninstall entry — and pass a suite that only checked
    // args[0].
    expect(args[1]).toBe(
      String.raw`HKCU\Software\Microsoft\Windows\CurrentVersion\Uninstall\ConcordVoice`
    );
    expect(args[2]).toBe('/f');
    expect(args).toHaveLength(3);
    // [internal]rules/electron.md forbids shell: true
    expect(opts?.shell).toBeFalsy();
  });

  it('invokes reg.exe by absolute path, never a bare PATH-resolved name', () => {
    // A bare `reg` searches %PATH%; a user-writable directory ahead of System32
    // could shadow it with a planted reg.exe and get it executed by us
    // (sonar typescript:S4036). Resolve from %SystemRoot% instead.
    vi.stubGlobal('process', {
      ...process,
      platform: 'win32',
      env: { LOCALAPPDATA: LOCAL, SystemRoot: 'C:\\Windows' },
    });
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const [cmd] = execFileSync.mock.calls[0];
    expect(norm(cmd as string)).toBe('C:/Windows/System32/reg.exe');
    expect(cmd).not.toBe('reg');
  });

  it('falls back to a fixed System32 path when SystemRoot is unset', () => {
    vi.stubGlobal('process', {
      ...process,
      platform: 'win32',
      env: { LOCALAPPDATA: LOCAL },
    });
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);
    const [cmd] = execFileSync.mock.calls[0];
    // Still absolute and still under System32 — never a bare name.
    expect(norm(cmd as string)).toMatch(/^C:\/Windows\/System32\/reg\.exe$/);
  });

  // regExePath()'s null return is the fail-closed skip: rather than fall back to a
  // bare `reg` (resolved via %PATH%, which a planted reg.exe could shadow — S4036),
  // it declines the deletion entirely. Both branches need direct coverage: with
  // existsSync globally stubbed true, the skip path is otherwise unreachable under
  // test and the guard could be deleted without a single failure.
  it('skips key removal when %SystemRoot% is not a drive-rooted path', () => {
    vi.stubGlobal('process', {
      ...process,
      platform: 'win32',
      // A UNC root passes a naive "is it absolute" check but is attacker-relocatable
      // via HKCU\Environment — exactly what the drive-letter regex refuses.
      env: { LOCALAPPDATA: LOCAL, SystemRoot: '\\\\evil-share\\Windows' },
    });
    existsSync.mockReturnValue(true);
    cleanupSquirrelResidue(logger);

    // Residue still removed — only the cosmetic key deletion is declined.
    expect(rmSync).toHaveBeenCalled();
    expect(execFileSync).not.toHaveBeenCalled();
    expect(logger.info).toHaveBeenCalledWith(expect.stringContaining('not resolvable'));
  });

  it('skips key removal when reg.exe is absent from System32', () => {
    vi.stubGlobal('process', {
      ...process,
      platform: 'win32',
      env: { LOCALAPPDATA: LOCAL, SystemRoot: 'C:\\Windows' },
    });
    // Targets exist so `removed > 0` and removeUninstallKey is genuinely reached;
    // only reg.exe itself is missing.
    existsSync.mockImplementation((p: unknown) => !norm(String(p)).endsWith('/reg.exe'));
    cleanupSquirrelResidue(logger);

    expect(rmSync).toHaveBeenCalled();
    expect(execFileSync).not.toHaveBeenCalled();
    expect(logger.info).toHaveBeenCalledWith(expect.stringContaining('not resolvable'));
  });

  it('is a no-op on non-win32', () => {
    vi.stubGlobal('process', { ...process, platform: 'darwin', env: {} });
    cleanupSquirrelResidue(logger);
    expect(rmSync).not.toHaveBeenCalled();
    expect(execFileSync).not.toHaveBeenCalled();
  });
});
