// #2367 part 2 — executes the BUILT preload bundle (not the source module) so
// the assertion survives esbuild's minification/bundling, unlike
// `tests/integration/preload-sandbox-contract.test.ts`'s string greps. Loads
// dist/preload/preload.js the same way Node's CJS loader would, with a
// hand-built `require('electron')` stub, and calls the exposed
// `electron.window.setZoomFactor` end to end.
// @vitest-environment node
import { describe, it, expect, beforeAll, vi } from 'vitest';
import { execFileSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const projectRoot = resolve(__dirname, '../..');
const preloadPath = resolve(projectRoot, 'dist/preload/preload.js');

interface ElectronStub {
  contextBridge: { exposeInMainWorld: ReturnType<typeof vi.fn> };
  ipcRenderer: {
    invoke: ReturnType<typeof vi.fn>;
    on: ReturnType<typeof vi.fn>;
    removeListener: ReturnType<typeof vi.fn>;
    send: ReturnType<typeof vi.fn>;
  };
  webFrame: { setZoomFactor: ReturnType<typeof vi.fn> };
}

function makeElectronStub(): ElectronStub {
  return {
    contextBridge: { exposeInMainWorld: vi.fn() },
    ipcRenderer: {
      invoke: vi.fn().mockResolvedValue(undefined),
      on: vi.fn(),
      removeListener: vi.fn(),
      send: vi.fn(),
    },
    webFrame: { setZoomFactor: vi.fn() },
  };
}

/**
 * Evaluate the bundled CJS preload the way Node's own module loader would —
 * a `module`/`exports`/`require` wrapper — WITHOUT going through Node's real
 * module resolution, since `require('electron')` cannot resolve outside a
 * real Electron process. `window` is also injected: the bundle calls
 * `installAudiocapRelay(ipcRenderer, window)` at module scope (a bare global
 * reference, not `globalThis.window`), so this Node-environment test must
 * supply one.
 */
function loadPreloadBundle(electronStub: ElectronStub): void {
  const code = readFileSync(preloadPath, 'utf-8');
  const fakeRequire = (id: string): unknown => {
    if (id === 'electron') return electronStub;
    throw new Error(`preload bundle required unexpected module '${id}'`);
  };
  const fakeWindow = {
    addEventListener: vi.fn(),
    document: { readyState: 'complete' },
  };
  const module = { exports: {} as Record<string, unknown> };
  const wrapper = new Function(
    'exports',
    'require',
    'module',
    '__filename',
    '__dirname',
    'window',
    code
  );
  wrapper(module.exports, fakeRequire, module, preloadPath, dirname(preloadPath), fakeWindow);
}

describe('UI zoom bridge — executable bundle contract (#2367)', () => {
  beforeAll(() => {
    try {
      execFileSync('npm', ['run', 'build:preload'], { cwd: projectRoot, stdio: 'pipe' });
    } catch (err: unknown) {
      const stderr =
        err instanceof Error && 'stderr' in err
          ? String((err as NodeJS.ErrnoException & { stderr: Buffer }).stderr)
          : '';
      throw new Error(`preload build failed:\n${stderr}`);
    }
  });

  it('clamps in-range, rejects invalid, and forwards only sanitized values to webFrame.setZoomFactor', () => {
    const electronStub = makeElectronStub();
    loadPreloadBundle(electronStub);

    expect(electronStub.contextBridge.exposeInMainWorld).toHaveBeenCalledTimes(1);
    const [channel, api] = electronStub.contextBridge.exposeInMainWorld.mock.calls[0] as [
      string,
      { window?: { setZoomFactor?: (factor: unknown) => void } },
    ];
    expect(channel).toBe('electron');
    const setZoomFactor = api.window?.setZoomFactor;
    expect(typeof setZoomFactor).toBe('function');

    // Rejected: not a finite number — no-op, never reaches webFrame.
    setZoomFactor?.(Number.NaN);
    setZoomFactor?.('1.5');

    // Accepted, clamped to [0.5, 2]: 5 -> 2, 0.1 -> 0.5, 1.25 unchanged.
    setZoomFactor?.(5);
    setZoomFactor?.(0.1);
    setZoomFactor?.(1.25);

    expect(electronStub.webFrame.setZoomFactor).toHaveBeenCalledTimes(3);
    expect(electronStub.webFrame.setZoomFactor.mock.calls).toEqual([[2], [0.5], [1.25]]);
  });
});
