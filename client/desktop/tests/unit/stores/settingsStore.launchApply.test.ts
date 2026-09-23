// #2367 part 2 — launch-time zoom apply. Isolated in its own file: it is the
// only suite that needs `vi.resetModules()` + a dynamic import to observe what
// happens at MODULE LOAD (the `fireImmediately: true` uiScale subscriber),
// which is unobservable once `settingsStore.ts` is already imported statically
// elsewhere (see settingsStore.test.ts's `page-zoom applier` describe, which
// exercises the same applier only via already-loaded `setUiScale` calls).
import { vi } from 'vitest';

describe('settingsStore launch-time zoom apply (#2367 part 2)', () => {
  const STORAGE_KEY = 'concord-settings';
  const originalElectron = (globalThis as { electron?: unknown }).electron;
  const originalInnerWidth = globalThis.innerWidth;

  const setInnerWidth = (width: number): void => {
    Object.defineProperty(globalThis, 'innerWidth', {
      configurable: true,
      writable: true,
      value: width,
    });
  };

  const seedPersistedUiScale = (uiScale: number): void => {
    localStorage.setItem(
      STORAGE_KEY,
      JSON.stringify({ state: { appearance: { uiScale } }, version: 1 })
    );
  };

  beforeEach(() => {
    vi.resetModules();
    localStorage.clear();
  });

  afterEach(() => {
    (globalThis as { electron?: unknown }).electron = originalElectron;
    setInnerWidth(originalInnerWidth);
    localStorage.clear();
  });

  it('zooms to the persisted, width-capped scale on load — before any setUiScale call', async () => {
    // 1280 / 800 = 1.6, so a seeded 2.0 choice must arrive capped, exactly as
    // the resize-time applier would compute it — this is the SAME
    // computeAppliedZoom the module-load subscriber must reach.
    setInnerWidth(1280);
    seedPersistedUiScale(2);
    const setZoomFactor = vi.fn();
    (globalThis as { electron?: unknown }).electron = { window: { setZoomFactor } };

    const { useSettingsStore } = await import('@/renderer/stores/ui/settingsStore');

    expect(setZoomFactor).toHaveBeenCalledTimes(1);
    expect(setZoomFactor).toHaveBeenCalledWith(1.6);
    expect(useSettingsStore.getState().appliedUiZoom).toBe(1.6);
  });

  it('makes its first call unconditionally, even for a stored 1 — a factor surviving a reload must be reset', async () => {
    setInnerWidth(1024);
    seedPersistedUiScale(1);
    const setZoomFactor = vi.fn();
    (globalThis as { electron?: unknown }).electron = { window: { setZoomFactor } };

    await import('@/renderer/stores/ui/settingsStore');

    expect(setZoomFactor).toHaveBeenCalledTimes(1);
    expect(setZoomFactor).toHaveBeenCalledWith(1);
  });
});
