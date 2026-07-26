import { describe, it, expect } from 'vitest';
import { readFileSync } from 'node:fs';
import path from 'node:path';

const MAIN = readFileSync(path.resolve(__dirname, '../../../src/main/main.ts'), 'utf8');

/**
 * Source-text assertions rather than behavioral ones: main.ts is the Electron entry
 * point and importing it executes app-lifecycle registration. The behavior of
 * cleanupSquirrelResidue itself is covered in squirrelCleanup.test.ts; what this file
 * guards is that it is actually WIRED, and wired at a safe point (#2402).
 */
describe('squirrelCleanup wiring (#2402)', () => {
  it('imports and calls the cleanup', () => {
    expect(MAIN).toMatch(
      /import\s+\{\s*cleanupSquirrelResidue\s*\}\s+from\s+'\.\/squirrelCleanup'/
    );
    expect(MAIN).toMatch(/cleanupSquirrelResidue\(/);
  });

  it('does not call cleanup before app.whenReady', () => {
    const readyIdx = MAIN.indexOf('app.whenReady()');
    const callIdx = MAIN.indexOf('cleanupSquirrelResidue(');
    expect(readyIdx).toBeGreaterThan(-1);
    expect(callIdx).toBeGreaterThan(readyIdx);
  });

  it('defers cleanup until after the window exists', () => {
    // The sweep is SYNCHRONOUS recursive rmSync over app-* directories that can
    // total hundreds of MB. Running it during whenReady blocks the main-process
    // event loop before first paint — the app appears to hang on launch.
    //
    // Asserting merely "after whenReady" is too weak: the original wiring satisfied
    // that and still ran ~150 lines ahead of createWindow(). Anchor on the window.
    const createWindowIdx = MAIN.indexOf('void createWindow().then(');
    const callIdx = MAIN.indexOf('cleanupSquirrelResidue(');
    expect(createWindowIdx).toBeGreaterThan(-1);
    expect(callIdx).toBeGreaterThan(createWindowIdx);
  });

  it('gates the sweep behind app.isPackaged', () => {
    // Without this, a developer running `npm start` on a real Windows machine has
    // their genuine %LOCALAPPDATA%\ConcordVoice profile swept — Update.exe, packages\,
    // SquirrelTemp\, the Start Menu folder and the uninstall registry key deleted as a
    // side effect of local development. (Gitar finding, PR #2464.)
    const gateIdx = MAIN.indexOf('if (app.isPackaged) {\n      const squirrelLogger');
    const callIdx = MAIN.indexOf('cleanupSquirrelResidue(');
    expect(gateIdx).toBeGreaterThan(-1);
    expect(callIdx).toBeGreaterThan(gateIdx);
  });

  it('reuses the existing update logger rather than constructing a second one', () => {
    // Each createUpdateLogger() re-runs mkdirSync plus a full readdirSync +
    // pruneOldLogs sweep of the log dir, so a second construction doubles that
    // filesystem work on every launch. (Gitar finding, PR #2464.)
    expect(MAIN).toMatch(/const squirrelLogger = getUpdateLogger\(\)/);
    expect(MAIN).not.toMatch(/cleanupSquirrelResidue\(createUpdateLogger\(\)\)/);
  });

  it('yields the event loop before sweeping', () => {
    // Even inside the post-window callback, a bare synchronous call would block the
    // first frame. It must be deferred a tick.
    expect(MAIN).toMatch(/setTimeout\(\s*\(\)\s*=>\s*cleanupSquirrelResidue\(/);
  });

  it('calls cleanup from the app, never from installer/quit paths', () => {
    // The whole point of doing this from the installed app on a later launch is that
    // an INSTALLER running from ConcordVoice\pending\ cannot delete ConcordVoice\ —
    // it would be deleting its own locked image, which is the #2402 bug itself.
    // Guard against a future refactor moving the call into a quit handler, where it
    // would race teardown and could run while an update installer is spawning.
    const callIdx = MAIN.indexOf('cleanupSquirrelResidue(');
    const beforeQuitIdx = MAIN.indexOf("app.on('before-quit'");
    if (beforeQuitIdx > -1) {
      // The call must not live inside the before-quit handler body.
      const handlerEnd = MAIN.indexOf('});', beforeQuitIdx);
      expect(callIdx > beforeQuitIdx && callIdx < handlerEnd).toBe(false);
    }
    expect(callIdx).toBeGreaterThan(-1);
  });
});
