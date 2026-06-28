// @vitest-environment node
import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import path from 'node:path';

const mockGetPath = vi.hoisted(() => vi.fn());
const mockSetPath = vi.hoisted(() => vi.fn());
// `isPackaged` is a getter-backed property on the real `app`; expose it as a
// plain mutable field so each case can set production vs. dev before import.
const mockApp = vi.hoisted(() => ({
  getPath: mockGetPath,
  setPath: mockSetPath,
  isPackaged: false,
}));

vi.mock('electron', () => ({ app: mockApp }));

describe('pinUserDataPath', () => {
  // pinUserDataPath() runs as an IMPORT-TIME side effect, so the environment
  // (process.argv + app.isPackaged) must be set BEFORE the dynamic import, and
  // vi.resetModules() must precede each import so the side effect re-runs under
  // that case's conditions.
  const originalArgv = process.argv;

  beforeEach(() => {
    mockGetPath.mockReset();
    mockSetPath.mockReset();
    mockApp.isPackaged = false;
    mockGetPath.mockImplementation((key: string) =>
      key === 'appData' ? '/fake/AppData' : '/fake/other'
    );
    // Baseline: a dev argv with NO --user-data-dir (the normal case).
    process.argv = ['node', 'main.js'];
  });

  afterEach(() => {
    process.argv = originalArgv;
  });

  it('pins userData to <appData>/ConcordVoice regardless of productName', async () => {
    vi.resetModules();
    const { pinUserDataPath, PINNED_USER_DATA_DIR } =
      await import('../../../src/main/pinUserDataPath');
    pinUserDataPath();
    expect(PINNED_USER_DATA_DIR).toBe('ConcordVoice');
    expect(mockSetPath).toHaveBeenCalledWith(
      'userData',
      path.join('/fake/AppData', 'ConcordVoice')
    );
  });

  it('runs the pin as an import-time side effect (before any consumer reads userData)', async () => {
    vi.resetModules();
    mockSetPath.mockClear();
    await import('../../../src/main/pinUserDataPath');
    // The module-load side effect must have already called setPath exactly once.
    expect(mockSetPath).toHaveBeenCalledWith(
      'userData',
      path.join('/fake/AppData', 'ConcordVoice')
    );
  });

  describe('dev --user-data-dir escape hatch (concord-dev.sh --clients N)', () => {
    // Case A — dev + `--user-data-dir=<value>` (combined `=` form) → pin SKIPPED
    // so each multi-client instance keeps its own isolated userData +
    // SingletonLock; otherwise only one client could launch.
    it('Case A: dev (!isPackaged) + --user-data-dir=/tmp/x → does NOT pin', async () => {
      mockApp.isPackaged = false;
      process.argv = ['node', 'main.js', '--user-data-dir=/tmp/x'];
      vi.resetModules();
      mockSetPath.mockClear();
      await import('../../../src/main/pinUserDataPath');
      expect(mockSetPath).not.toHaveBeenCalled();
    });

    // Case B — dev + `--user-data-dir <value>` (separate token form) → also
    // SKIPPED; the impl matches both the `=` form and the bare flag.
    it('Case B: dev (!isPackaged) + --user-data-dir then value token → does NOT pin', async () => {
      mockApp.isPackaged = false;
      process.argv = ['node', 'main.js', '--user-data-dir', '/tmp/x'];
      vi.resetModules();
      mockSetPath.mockClear();
      await import('../../../src/main/pinUserDataPath');
      expect(mockSetPath).not.toHaveBeenCalled();
    });

    // Case C — packaged production ALWAYS pins, even if an external launcher
    // passes --user-data-dir.
    it('Case C: packaged (isPackaged) + --user-data-dir=/tmp/x → pins userData', async () => {
      mockApp.isPackaged = true;
      process.argv = ['node', 'main.js', '--user-data-dir=/tmp/x'];
      vi.resetModules();
      mockSetPath.mockClear();
      await import('../../../src/main/pinUserDataPath');
      expect(mockSetPath).toHaveBeenCalledWith(
        'userData',
        path.join('/fake/AppData', 'ConcordVoice')
      );
    });

    // Case D — dev with NO --user-data-dir (normal single-instance dev) → pins,
    // same as production.
    it('Case D: dev (!isPackaged) + no --user-data-dir → pins userData', async () => {
      mockApp.isPackaged = false;
      process.argv = ['node', 'main.js'];
      vi.resetModules();
      mockSetPath.mockClear();
      await import('../../../src/main/pinUserDataPath');
      expect(mockSetPath).toHaveBeenCalledWith(
        'userData',
        path.join('/fake/AppData', 'ConcordVoice')
      );
    });
  });
});
