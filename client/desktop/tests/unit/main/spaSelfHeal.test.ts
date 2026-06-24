import { describe, it, expect, vi, beforeEach } from 'vitest';

// Mock the spaLoader module so we can control resolveSpaSource() per test.
vi.mock('@/main/spaLoader', () => ({
  resolveSpaSource: vi.fn(),
  SPA_NO_CACHE_LOAD_OPTIONS: {
    extraHeaders: 'Cache-Control: no-cache\nPragma: no-cache\n',
  },
}));

// Mock electron's BrowserWindow / webContents.
//
// `vi.hoisted` runs BEFORE the `vi.mock` factory below is hoisted, ensuring
// the mocks are initialized when the factory references them. Without
// hoisting, the factory hits `ReferenceError: Cannot access 'mockGetAllWindows'
// before initialization` because vi.mock is itself hoisted to the top of file.
// `mockGetAllWindows` is a vi.fn() so individual tests can override its return
// value (e.g., to simulate the no-window race where every window has been
// destroyed mid-attempt).
const { mockLoadURL, mockLoadFile, mockWebContents, mockGetAllWindows } = vi.hoisted(() => {
  const loadURL = vi.fn();
  const loadFile = vi.fn();
  const webContents = { loadURL, loadFile };
  const getAllWindows = vi.fn(() => [{ webContents, isDestroyed: () => false }]);
  return {
    mockLoadURL: loadURL,
    mockLoadFile: loadFile,
    mockWebContents: webContents,
    mockGetAllWindows: getAllWindows,
  };
});

vi.mock('electron', () => ({
  // #830: spaSelfHeal's loadBundledFallback now consults app.isPackaged
  // to gate the app:// loadURL call (dev mode noops because the scheme
  // is unregistered there). Existing tests assume the packaged branch
  // executes (mockLoadURL is called), so the mock returns isPackaged: true.
  // Tests that need the dev branch (none currently) would override via
  // a per-test vi.mocked() spy.
  app: {
    isPackaged: true,
  },
  BrowserWindow: {
    getAllWindows: mockGetAllWindows,
  },
}));

// Module under test — imported AFTER mocks are registered.
import { resolveSpaSource } from '@/main/spaLoader';
import { attemptSelfHeal, __resetSelfHealState } from '@/main/spaSelfHeal';

const mockResolve = resolveSpaSource as unknown as ReturnType<typeof vi.fn>;

describe('spaSelfHeal — R2 retry state machine', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    __resetSelfHealState();
    // Restore the default getAllWindows mock — individual tests can override.
    mockGetAllWindows.mockReturnValue([{ webContents: mockWebContents, isDestroyed: () => false }]);
  });

  it('retryCount=0: waits 500ms, resolves, loadURLs the result', async () => {
    mockResolve.mockResolvedValue({
      mode: 'remote',
      url: 'https://api.concordvoice.chat/spa/def5678/index.html',
      reason: 'remote SPA compatible',
    });

    const promise = attemptSelfHeal({ reason: 'chunk-load' });
    // Before 500ms elapses, no work has been done.
    expect(mockResolve).not.toHaveBeenCalled();
    await vi.advanceTimersByTimeAsync(500);
    const outcome = await promise;

    expect(mockResolve).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledWith(
      'https://api.concordvoice.chat/spa/def5678/index.html',
      expect.objectContaining({
        extraHeaders: expect.stringContaining('Cache-Control: no-cache'),
      })
    );
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(outcome).toEqual({ mode: 'recovered', retryCount: 1 });
  });

  it('retryCount=0 with mode=bundled: falls back to bundled, sets exhausted', async () => {
    mockResolve.mockResolvedValue({ mode: 'bundled', reason: 'spaUrl rejected' });

    const promise = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    const outcome = await promise;

    // #830: bundled fallback now loads via app:// scheme (Task 4 conversion).
    // The mockLoadFile.not.toHaveBeenCalled() assertion is a regression-lock —
    // if a future change reintroduces loadFile for bundled, this test fails loudly.
    expect(mockLoadURL).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledWith('app://concord/index.html');
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(outcome).toEqual({ mode: 'fellBackToBundled', retryCount: 1 });
  });

  it('retryCount=1: skips refetch, falls back to bundled immediately', async () => {
    // First call gets us to retryCount=1.
    mockResolve.mockResolvedValue({
      mode: 'remote',
      url: 'https://api.concordvoice.chat/spa/def5678/index.html',
      reason: 'remote SPA compatible',
    });
    const first = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    await first;
    vi.clearAllMocks();

    // Second call hits the retryCount=1 branch.
    const second = await attemptSelfHeal({ reason: 'chunk-load' });

    // #830: bundled fallback now loads via app:// scheme (Task 4 conversion).
    expect(mockResolve).not.toHaveBeenCalled();
    expect(mockLoadURL).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledWith('app://concord/index.html');
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(second).toEqual({ mode: 'fellBackToBundled', retryCount: 2 });
  });

  it('after exhausted: subsequent calls log-and-noop', async () => {
    // Drive the state machine past exhaustion.
    mockResolve.mockResolvedValue({ mode: 'bundled', reason: 'forced' });
    const first = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    await first;
    vi.clearAllMocks();

    // Third trigger after exhaustion.
    const third = await attemptSelfHeal({ reason: 'chunk-load' });

    expect(mockResolve).not.toHaveBeenCalled();
    expect(mockLoadURL).not.toHaveBeenCalled();
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(third).toEqual({ mode: 'noop', retryCount: 1 });
  });

  it('in-flight idempotency: simultaneous triggers collapse to one attempt', async () => {
    mockResolve.mockResolvedValue({
      mode: 'remote',
      url: 'https://api.concordvoice.chat/spa/def5678/index.html',
      reason: 'remote SPA compatible',
    });

    const a = attemptSelfHeal({ reason: 'chunk-load' });
    const b = attemptSelfHeal({ reason: 'main-frame-load' });
    const c = attemptSelfHeal({ reason: 'chunk-import-rejected' });

    await vi.advanceTimersByTimeAsync(500);
    const [outcomeA, outcomeB, outcomeC] = await Promise.all([a, b, c]);

    // All three trigger functions return the same outcome from the single attempt.
    expect(mockResolve).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledOnce();
    expect(outcomeA).toEqual(outcomeB);
    expect(outcomeB).toEqual(outcomeC);
  });

  it('resolveSpaSource throws: falls back to bundled, no crash', async () => {
    mockResolve.mockRejectedValue(new Error('config fetch failed: timeout'));

    const promise = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    const outcome = await promise;

    // #830: bundled fallback now loads via app:// scheme (Task 4 conversion).
    expect(mockLoadURL).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledWith('app://concord/index.html');
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(outcome).toEqual({ mode: 'fellBackToBundled', retryCount: 1 });
  });

  it('no window: noops WITHOUT mutating retry budget — next valid trigger still recovers', async () => {
    // Simulate a transient race where every window was destroyed mid-attempt.
    mockGetAllWindows.mockReturnValueOnce([]);

    const noopOutcome = await attemptSelfHeal({ reason: 'chunk-load' });

    // No work done, no state change. retryCount stays 0; selfHealExhausted stays false.
    expect(noopOutcome).toEqual({ mode: 'noop', retryCount: 0 });
    expect(mockResolve).not.toHaveBeenCalled();
    expect(mockLoadURL).not.toHaveBeenCalled();
    expect(mockLoadFile).not.toHaveBeenCalled();

    // Critical regression-lock: a subsequent valid trigger MUST still recover —
    // the no-window path must not have permanently disabled self-heal.
    mockResolve.mockResolvedValueOnce({
      mode: 'remote',
      url: 'https://api.concordvoice.chat/spa/abc1234/index.html',
      reason: 'remote SPA compatible',
    });
    const followup = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    const followupOutcome = await followup;

    expect(followupOutcome).toEqual({ mode: 'recovered', retryCount: 1 });
    expect(mockLoadURL).toHaveBeenCalledOnce();
  });

  it('loadURL throws after successful resolveSpaSource: bundled fallback consumes the retry slot', async () => {
    // Simulates a network drop after we resolved a fresh remote URL but
    // before/during webContents.loadURL — recovery must fall back to bundled
    // and consume the retry slot so a subsequent trigger lands at exhaustion.
    mockResolve.mockResolvedValue({
      mode: 'remote',
      url: 'https://api.concordvoice.chat/spa/def5678/index.html',
      reason: 'remote SPA compatible',
    });
    mockLoadURL.mockRejectedValueOnce(new Error('net::ERR_CONNECTION_RESET'));

    const promise = attemptSelfHeal({ reason: 'chunk-load' });
    await vi.advanceTimersByTimeAsync(500);
    const outcome = await promise;

    // #830: bundled fallback now loads via app:// scheme (Task 4 conversion).
    // mockLoadURL is called TWICE — first with the remote URL (rejected by
    // mockRejectedValueOnce above), then with 'app://concord/index.html' for
    // the bundled fallback path. The first rejection is the trigger; the
    // second call is the bundled-fallback recovery.
    expect(mockResolve).toHaveBeenCalledOnce();
    expect(mockLoadURL).toHaveBeenCalledTimes(2);
    expect(mockLoadURL).toHaveBeenNthCalledWith(
      1,
      'https://api.concordvoice.chat/spa/def5678/index.html',
      expect.objectContaining({
        extraHeaders: expect.stringContaining('Cache-Control: no-cache'),
      })
    );
    expect(mockLoadURL).toHaveBeenNthCalledWith(2, 'app://concord/index.html');
    expect(mockLoadFile).not.toHaveBeenCalled();
    expect(outcome).toEqual({ mode: 'fellBackToBundled', retryCount: 1 });
  });
});
