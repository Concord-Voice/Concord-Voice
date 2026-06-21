import { vi, describe, it, expect, beforeEach, afterEach } from 'vitest';

// Mock apiFetch to hand fetchAndApply a 401 DIRECTLY — i.e. simulate the state
// where apiFetch's own 401-recovery already ran and still returned 401. The
// retry under test is the next layer: fetchAndApply must not silently abandon
// the session's preference pull on that 401. (The precipitating 401's exact
// server-side cause is unpinned — see the prefs-401 finding.)
const mockApiFetch = vi.fn();
vi.mock('@/renderer/services/apiClient', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/renderer/services/apiClient')>();
  return { ...actual, apiFetch: (...args: unknown[]) => mockApiFetch(...args) };
});

vi.mock('@/renderer/services/e2eeService', () => ({
  e2eeService: {
    isInitialized: true,
    encryptPreferences: vi.fn().mockResolvedValue('enc'),
    decryptPreferences: vi.fn(),
  },
}));

import { preferencesSyncService } from '@/renderer/services/preferencesSync';

function initStubDeps() {
  (preferencesSyncService as unknown as { deps: unknown }).deps = null;
  preferencesSyncService.init({
    getAppearance: () => ({}) as never,
    setAppearance: () => {},
    getLayout: () => ({}) as never,
    setLayout: () => {},
  });
}

describe('preferencesSync — startup 401 bounded retry', () => {
  beforeEach(() => {
    vi.clearAllMocks();
    vi.useFakeTimers();
    initStubDeps();
  });

  afterEach(() => {
    preferencesSyncService.stopWatching();
    vi.useRealTimers();
  });

  it('retries a transient 401 and is bounded (initial + MAX_AUTH_RETRIES)', async () => {
    mockApiFetch.mockResolvedValue({ ok: false, status: 401 });

    await preferencesSyncService.fetchAndApply();
    expect(mockApiFetch).toHaveBeenCalledTimes(1); // initial 401 → retry scheduled

    await vi.advanceTimersByTimeAsync(1500);
    expect(mockApiFetch).toHaveBeenCalledTimes(2); // retry 1

    await vi.advanceTimersByTimeAsync(1500);
    expect(mockApiFetch).toHaveBeenCalledTimes(3); // retry 2 (the last allowed)

    await vi.advanceTimersByTimeAsync(5000);
    expect(mockApiFetch).toHaveBeenCalledTimes(3); // bounded — no further retries
  });

  it('does NOT retry a non-401 failure', async () => {
    mockApiFetch.mockResolvedValue({ ok: false, status: 500 });

    await preferencesSyncService.fetchAndApply();
    expect(mockApiFetch).toHaveBeenCalledTimes(1);

    await vi.advanceTimersByTimeAsync(5000);
    expect(mockApiFetch).toHaveBeenCalledTimes(1); // 500 is not an auth race
  });

  it('stopWatching cancels a pending retry (clean teardown, e.g. on logout)', async () => {
    mockApiFetch.mockResolvedValue({ ok: false, status: 401 });

    await preferencesSyncService.fetchAndApply();
    expect(mockApiFetch).toHaveBeenCalledTimes(1);

    preferencesSyncService.stopWatching();

    await vi.advanceTimersByTimeAsync(5000);
    expect(mockApiFetch).toHaveBeenCalledTimes(1); // retry was cancelled
  });

  it('a fresh fetchAndApply cancels a pending retry (external trigger supersedes a stale one)', async () => {
    // Call 1 → 401 → schedules a retry. Call 2 is a fresh external trigger (e.g. a
    // 'preferences_updated' WS push); it must cancel that pending retry so it
    // can't fire later and re-apply older remote state. (Gitar finding, PR #1657.)
    mockApiFetch
      .mockResolvedValueOnce({ ok: false, status: 401 }) // call 1 → retry scheduled
      .mockResolvedValueOnce({ ok: false, status: 500 }); // call 2 (fresh) → not retried

    await preferencesSyncService.fetchAndApply();
    expect(mockApiFetch).toHaveBeenCalledTimes(1);

    await preferencesSyncService.fetchAndApply(); // fresh trigger — clears the pending retry
    expect(mockApiFetch).toHaveBeenCalledTimes(2);

    await vi.advanceTimersByTimeAsync(5000);
    expect(mockApiFetch).toHaveBeenCalledTimes(2); // the call-1 retry was cancelled — no 3rd fetch
  });
});
