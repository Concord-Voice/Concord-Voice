// #2367 part 2 — pure helpers behind UI Scale on Electron page zoom. The applier
// that calls the bridge is exercised at its caller in settingsStore.test.ts.
import {
  UI_SCALE_LEGACY_MAX,
  UI_SCALE_LEGACY_MIN,
  UI_ZOOM_EPSILON,
  computeAppliedZoom,
  cssUiScaleFor,
  getZoomBridge,
  hasZoomBridge,
  isPipWindow,
  legacyUiScale,
} from '@/renderer/utils/ui/uiZoom';

type ElectronStub = { window?: unknown };
const electronStub = (): ElectronStub => globalThis.electron as unknown as ElectronStub;

describe('computeAppliedZoom', () => {
  it('never caps a zoom-out or 100%, however narrow the window', () => {
    expect(computeAppliedZoom(0.5, 300)).toBe(0.5);
    expect(computeAppliedZoom(0.75, 640)).toBe(0.75);
    expect(computeAppliedZoom(1, 100)).toBe(1);
  });

  it('caps 200% to 160% in a 1280-wide window', () => {
    expect(computeAppliedZoom(2, 1280)).toBe(1.6);
  });

  it('allows the full 200% in a 1600-wide window', () => {
    expect(computeAppliedZoom(2, 1600)).toBe(2);
  });

  it('allows no zoom-in at all in an 800-wide window, and never drops below 100%', () => {
    expect(computeAppliedZoom(2, 800)).toBe(1);
    expect(computeAppliedZoom(1.5, 640)).toBe(1);
  });

  it('keeps a choice below the cap untouched', () => {
    expect(computeAppliedZoom(1.25, 1280)).toBe(1.25);
  });

  it('is idempotent when fed its own zoomed width back (unzoomedWidth = innerWidth × applied)', () => {
    for (const [chosen, width] of [
      [2, 1280],
      [2, 1283],
      [1.75, 1600],
      [1.3, 900],
    ] as const) {
      const applied = computeAppliedZoom(chosen, width);
      // innerWidth under zoom is an integer — the rounding the guard absorbs.
      const innerWidth = Math.round(width / applied);
      const again = computeAppliedZoom(chosen, innerWidth * applied);
      expect(Math.abs(again - applied)).toBeLessThan(UI_ZOOM_EPSILON);
    }
  });

  it('is total: a non-finite choice is 100%, an unusable width allows no zoom-in', () => {
    expect(computeAppliedZoom(Number.NaN, 1600)).toBe(1);
    expect(computeAppliedZoom(Number.POSITIVE_INFINITY, 1600)).toBe(1);
    expect(computeAppliedZoom(2, 0)).toBe(1);
    expect(computeAppliedZoom(2, Number.NaN)).toBe(1);
    expect(computeAppliedZoom(2, -500)).toBe(1);
  });
});

describe('legacyUiScale / cssUiScaleFor', () => {
  it('clamps to the legacy 0.85–1.3 band', () => {
    expect(legacyUiScale(2)).toBe(UI_SCALE_LEGACY_MAX);
    expect(legacyUiScale(0.5)).toBe(UI_SCALE_LEGACY_MIN);
    expect(legacyUiScale(1.1)).toBe(1.1);
    expect(legacyUiScale(Number.NaN)).toBe(1);
  });

  it('pins --ui-scale to 1 with the bridge and applies the legacy clamp without it', () => {
    expect(cssUiScaleFor(2)).toBe(1.3);
    expect(cssUiScaleFor(1.2)).toBe(1.2);
    electronStub().window = { setZoomFactor: vi.fn() };
    try {
      expect(cssUiScaleFor(2)).toBe(1);
      expect(cssUiScaleFor(0.5)).toBe(1);
    } finally {
      delete electronStub().window;
    }
  });
});

describe('getZoomBridge', () => {
  afterEach(() => {
    delete electronStub().window;
  });

  it('is null on a shell whose preload has no window namespace', () => {
    expect(getZoomBridge()).toBeNull();
    expect(hasZoomBridge()).toBe(false);
  });

  it('is null when the namespace exists but lacks setZoomFactor (a v28 shell)', () => {
    electronStub().window = { setClientBehavior: vi.fn() };
    expect(getZoomBridge()).toBeNull();
  });

  it('is null when setZoomFactor is present but not a function', () => {
    electronStub().window = { setZoomFactor: 2 };
    expect(getZoomBridge()).toBeNull();
  });

  it('returns the bridge function when the shell exposes it', () => {
    const setZoomFactor = vi.fn();
    electronStub().window = { setZoomFactor };
    expect(getZoomBridge()).toBe(setZoomFactor);
    expect(hasZoomBridge()).toBe(true);
  });
});

describe('isPipWindow', () => {
  afterEach(() => {
    globalThis.location.hash = '';
  });

  it('matches the #/pip/ route App.tsx uses and nothing else', () => {
    expect(isPipWindow()).toBe(false);
    globalThis.location.hash = '#/pip/abc';
    expect(isPipWindow()).toBe(true);
    globalThis.location.hash = '#/settings';
    expect(isPipWindow()).toBe(false);
  });
});
