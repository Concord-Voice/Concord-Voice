import {
  useSettingsStore,
  clampUiScale,
  UI_SCALE_DEFAULT,
  UI_SCALE_MIN,
  UI_SCALE_MAX,
} from '@/renderer/stores/ui/settingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('settingsStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('has default values', () => {
    const { appearance } = useSettingsStore.getState();
    expect(appearance.theme).toBe('dark');
    expect(appearance.colorScheme).toBe('concord');
    expect(appearance.fontSize).toBe('default');
    expect(appearance.compactMode).toBe(false);
    expect(appearance.reduceAnimations).toBe(false);
    // #489 — new accessibility-display fields
    expect(appearance.uiScale).toBe(UI_SCALE_DEFAULT);
    expect(appearance.highContrast).toBe(false);
  });

  it('defaults NSFW content intent to false', () => {
    expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
  });

  it('sets and persists NSFW content intent', () => {
    useSettingsStore.getState().setAllowNsfwContent(true);
    expect(useSettingsStore.getState().allowNsfwContent).toBe(true);
    expect(localStorage.getItem('concord-settings')).toContain('"allowNsfwContent":true');
  });

  it('setTheme updates theme', () => {
    useSettingsStore.getState().setTheme('light');
    expect(useSettingsStore.getState().appearance.theme).toBe('light');
  });

  it('setColorScheme updates scheme', () => {
    useSettingsStore.getState().setColorScheme('morky');
    expect(useSettingsStore.getState().appearance.colorScheme).toBe('morky');
  });

  it('setFontSize updates size', () => {
    useSettingsStore.getState().setFontSize('large');
    expect(useSettingsStore.getState().appearance.fontSize).toBe('large');
  });

  it('setCompactMode toggles compact mode', () => {
    useSettingsStore.getState().setCompactMode(true);
    expect(useSettingsStore.getState().appearance.compactMode).toBe(true);
  });

  it('setReduceAnimations toggles animations', () => {
    useSettingsStore.getState().setReduceAnimations(true);
    expect(useSettingsStore.getState().appearance.reduceAnimations).toBe(true);
  });

  // ─── #489 — UI scale + high contrast ────────────────────────────────────

  describe('uiScale (#489)', () => {
    it('setUiScale updates the value', () => {
      useSettingsStore.getState().setUiScale(1.2);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(1.2);
    });

    it('clamps below UI_SCALE_MIN', () => {
      useSettingsStore.getState().setUiScale(0.1);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_MIN);
    });

    it('clamps above UI_SCALE_MAX', () => {
      useSettingsStore.getState().setUiScale(3);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_MAX);
    });

    it('stores the widened 50–200% range for every shell (#2367 part 2)', () => {
      expect(UI_SCALE_MIN).toBe(0.5);
      expect(UI_SCALE_MAX).toBe(2);
      // Storage is not capability-dependent: no bridge here, and 2.0 is kept.
      useSettingsStore.getState().setUiScale(2);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(2);
    });

    it('non-finite input falls back to default', () => {
      useSettingsStore.getState().setUiScale(Number.NaN);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_DEFAULT);
    });

    it('exposes clampUiScale as a pure helper', () => {
      expect(clampUiScale(1)).toBe(1);
      expect(clampUiScale(0.1)).toBe(UI_SCALE_MIN);
      expect(clampUiScale(3)).toBe(UI_SCALE_MAX);
      // Non-finite inputs (NaN, Infinity) fall back to the default rather
      // than getting clamped — they're not meaningful scale values.
      expect(clampUiScale(Number.POSITIVE_INFINITY)).toBe(UI_SCALE_DEFAULT);
      expect(clampUiScale(Number.NaN)).toBe(UI_SCALE_DEFAULT);
    });

    it('applies uiScale to --ui-scale CSS custom property on the doc root', () => {
      useSettingsStore.getState().setUiScale(1.25);
      expect(document.documentElement.style.getPropertyValue('--ui-scale')).toBe('1.25');
    });

    it('clamps --ui-scale to the legacy 0.85–1.3 band on a shell without the zoom bridge', () => {
      useSettingsStore.getState().setUiScale(2);
      expect(document.documentElement.style.getPropertyValue('--ui-scale')).toBe('1.3');
      useSettingsStore.getState().setUiScale(0.5);
      expect(document.documentElement.style.getPropertyValue('--ui-scale')).toBe('0.85');
      // No bridge, so nothing was zoomed and nothing claims to have been.
      expect(useSettingsStore.getState().appliedUiZoom).toBeNull();
    });
  });

  // ─── #2367 part 2 — UI Scale on Electron page zoom ─────────────────────

  describe('page-zoom applier (#2367 part 2)', () => {
    type ElectronStub = { window?: { setZoomFactor: (factor: number) => void } };
    const electronStub = (): ElectronStub => globalThis.electron as unknown as ElectronStub;
    let setZoomFactor: ReturnType<typeof vi.fn<(factor: number) => void>>;

    const setInnerWidth = (width: number): void => {
      Object.defineProperty(globalThis, 'innerWidth', {
        configurable: true,
        writable: true,
        value: width,
      });
    };
    const uiScaleVar = (): string => document.documentElement.style.getPropertyValue('--ui-scale');

    beforeEach(() => {
      setZoomFactor = vi.fn<(factor: number) => void>();
      electronStub().window = { setZoomFactor };
      setInnerWidth(1600);
    });

    afterEach(() => {
      delete electronStub().window;
      setInnerWidth(1024);
      globalThis.location.hash = '';
    });

    it('survives a resize delivered re-entrantly from inside the zoom call', () => {
      // Electron fires `resize` asynchronously today. With a synchronous one, a
      // factor recorded AFTER zooming let the re-entrant call see a stale value
      // and zoom again (the red-team PoC measured 51 re-entries).
      setZoomFactor.mockImplementation(() => {
        globalThis.dispatchEvent(new Event('resize'));
      });
      useSettingsStore.getState().setUiScale(1.5);
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
    });

    it('zooms the page to the chosen scale and pins --ui-scale to 1', () => {
      useSettingsStore.getState().setUiScale(1.5);
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(setZoomFactor).toHaveBeenCalledWith(1.5);
      // Pinned, or text would be scaled by page zoom AND by the variable.
      expect(uiScaleVar()).toBe('1');
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.5);
    });

    it('caps zoom-in by window width: 200% in a 1280-wide window applies 160%', () => {
      setInnerWidth(1280);
      useSettingsStore.getState().setUiScale(2);
      expect(setZoomFactor).toHaveBeenCalledWith(1.6);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.6);
      // The CHOICE is kept; only the applied factor is limited.
      expect(useSettingsStore.getState().appearance.uiScale).toBe(2);
    });

    it('never zooms a PiP window, even with the bridge present', () => {
      globalThis.location.hash = '#/pip/abc';
      useSettingsStore.getState().setUiScale(1.5);
      expect(setZoomFactor).not.toHaveBeenCalled();
      expect(uiScaleVar()).toBe('1');
      expect(useSettingsStore.getState().appliedUiZoom).toBeNull();
    });

    it('makes no second bridge call when the resize the zoom itself fires leaves the factor unchanged', () => {
      useSettingsStore.getState().setUiScale(2);
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(setZoomFactor).toHaveBeenCalledWith(2);
      // What real 2x zoom does to a 1600-wide window: innerWidth halves, then
      // `resize` fires. innerWidth × applied = 1600 → still 2.0.
      setInnerWidth(800);
      globalThis.dispatchEvent(new Event('resize'));
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
    });

    it('re-caps on a resize that narrows the window', () => {
      useSettingsStore.getState().setUiScale(2);
      // The window narrows from 1600 to 1280 unzoomed: 640 CSS px at 2x.
      setInnerWidth(640);
      globalThis.dispatchEvent(new Event('resize'));
      expect(setZoomFactor).toHaveBeenCalledTimes(2);
      expect(setZoomFactor).toHaveBeenLastCalledWith(1.6);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.6);
    });

    it('keeps the last factor through a zero-width resize (minimised window)', () => {
      useSettingsStore.getState().setUiScale(2);
      setInnerWidth(0);
      globalThis.dispatchEvent(new Event('resize'));
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(2);
    });

    it('converges to the exact chosen endpoint once widening lifts the cap (96c2395df)', () => {
      // 1275 / 800 = 1.59375 — a cap that never equals 1.6 by exact float
      // arithmetic, which is what exposed the pre-fix stuck-short bug.
      setInnerWidth(1275);
      useSettingsStore.getState().setUiScale(1.6);
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(setZoomFactor).toHaveBeenCalledWith(1.59375);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.59375);

      // Widen enough that the new unzoomed width no longer caps 1.6 at all.
      setInnerWidth(2000);
      globalThis.dispatchEvent(new Event('resize'));
      expect(setZoomFactor).toHaveBeenCalledTimes(2);
      expect(setZoomFactor).toHaveBeenLastCalledWith(1.6);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.6);
    });

    it('skips a second bridge call for a sub-epsilon cap-rounding gap after a resize (96c2395df)', () => {
      setInnerWidth(1280);
      useSettingsStore.getState().setUiScale(2);
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(setZoomFactor).toHaveBeenCalledWith(1.6);

      // 799 * 1.6 / 800 = 1.598 — 0.002 short of the current 1.6, inside the
      // 0.01 epsilon, and not an endpoint (neither the chosen 2 nor 1).
      setInnerWidth(799);
      globalThis.dispatchEvent(new Event('resize'));
      expect(setZoomFactor).toHaveBeenCalledTimes(1);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.6);
    });

    it('does not persist the applied factor', () => {
      useSettingsStore.getState().setUiScale(1.5);
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.5);
      const stored = localStorage.getItem('concord-settings') ?? '';
      expect(stored).toContain('"uiScale":1.5');
      expect(stored).not.toContain('appliedUiZoom');
    });
  });

  describe('highContrast (#489)', () => {
    it('setHighContrast toggles the value', () => {
      useSettingsStore.getState().setHighContrast(true);
      expect(useSettingsStore.getState().appearance.highContrast).toBe(true);
    });

    it('applies highContrast to data-high-contrast attribute', () => {
      useSettingsStore.getState().setHighContrast(true);
      expect(document.documentElement.dataset.highContrast).toBe('true');
      useSettingsStore.getState().setHighContrast(false);
      expect(document.documentElement.dataset.highContrast).toBe('false');
    });
  });

  describe('DOM attribute application', () => {
    it('applies theme to data-theme attribute', () => {
      useSettingsStore.getState().setTheme('light');
      expect(document.documentElement.dataset.theme).toBe('light');
    });

    it('applies color scheme to data-scheme attribute', () => {
      useSettingsStore.getState().setColorScheme('hacker');
      expect(document.documentElement.dataset.scheme).toBe('hacker');
    });

    it('applies defacto scheme to data-scheme attribute', () => {
      useSettingsStore.getState().setColorScheme('defacto');
      expect(document.documentElement.dataset.scheme).toBe('defacto');
    });

    it('applies font size to data-fontsize attribute', () => {
      useSettingsStore.getState().setFontSize('large');
      expect(document.documentElement.dataset.fontsize).toBe('large');
    });

    it('applies compact mode to data-compact attribute', () => {
      useSettingsStore.getState().setCompactMode(true);
      expect(document.documentElement.dataset.compact).toBe('true');
    });

    it('applies reduce animations to data-reduce-animations attribute', () => {
      useSettingsStore.getState().setReduceAnimations(true);
      expect(document.documentElement.dataset.reduceAnimations).toBe('true');
    });
  });

  // setSyncSuppressed / isSyncSuppressed moved to the colorSyncSuppression leaf
  // module; their tests live in tests/unit/stores/ui/colorSyncSuppression.test.ts.

  describe('persistence', () => {
    it('persists settings to localStorage', () => {
      useSettingsStore.getState().setTheme('light');
      useSettingsStore.getState().setColorScheme('morky');
      const stored = JSON.parse(localStorage.getItem('concord-settings') || '{}');
      expect(stored.state?.appearance?.theme).toBe('light');
      expect(stored.state?.appearance?.colorScheme).toBe('morky');
    });

    it('re-clamps a persisted uiScale outside 0.5–2.0 on rehydration', async () => {
      localStorage.setItem(
        'concord-settings',
        JSON.stringify({ state: { appearance: { uiScale: 9 } }, version: 1 })
      );
      await useSettingsStore.persist.rehydrate();
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_MAX);
    });

    it('replaces unknown font ids and modes on rehydration, keeping valid ones (#2366)', async () => {
      localStorage.setItem(
        'concord-settings',
        JSON.stringify({
          state: {
            appearance: {
              appFont: 'comic-sans',
              fontMode: 'advanced',
              fontHeadings: 'lexend',
              fontNavigation: 42,
              fontMessages: 'sourcesans',
            },
          },
          version: 1,
        })
      );
      await useSettingsStore.persist.rehydrate();
      expect(useSettingsStore.getState().appearance).toMatchObject({
        appFont: 'default',
        fontMode: 'one',
        fontHeadings: 'lexend',
        fontNavigation: 'default',
        fontMessages: 'sourcesans',
      });
    });

    it.each(['appFont', 'fontHeadings', 'fontNavigation', 'fontMessages'] as const)(
      'replaces a corrupt %s alone, leaving the other font keys (#2366)',
      async (key) => {
        const valid = {
          appFont: 'lato',
          fontHeadings: 'lato',
          fontNavigation: 'lato',
          fontMessages: 'lato',
        };
        localStorage.setItem(
          'concord-settings',
          JSON.stringify({
            state: { appearance: { ...valid, [key]: 'comic-sans' } },
            version: 1,
          })
        );
        await useSettingsStore.persist.rehydrate();
        expect(useSettingsStore.getState().appearance).toMatchObject({
          ...valid,
          [key]: 'default',
        });
      }
    );

    it('a corrupt pick on a font-bundling theme resolves to the theme font (#2366)', async () => {
      useSettingsStore.getState().setAppFont('inter');
      localStorage.setItem(
        'concord-settings',
        JSON.stringify({
          state: { appearance: { colorScheme: 'agency', appFont: 'comic-sans' } },
          version: 1,
        })
      );
      await useSettingsStore.persist.rehydrate();
      expect(document.documentElement.dataset.appfont).toBe('atkinson');
    });

    it('a pre-#2366 snapshot gets the new font keys from the defaults', async () => {
      localStorage.setItem(
        'concord-settings',
        JSON.stringify({ state: { appearance: { appFont: 'inter' } }, version: 1 })
      );
      await useSettingsStore.persist.rehydrate();
      expect(useSettingsStore.getState().appearance).toMatchObject({
        appFont: 'inter',
        fontMode: 'one',
        fontHeadings: 'default',
        fontNavigation: 'default',
        fontMessages: 'default',
      });
    });

    it('never lets storage replace an action or the live zoom factor', async () => {
      // `partialize` keeps both out of what is written; a hand-edited snapshot
      // must not put them back into what is read.
      useSettingsStore.setState({ appliedUiZoom: 1.25 });
      localStorage.setItem(
        'concord-settings',
        JSON.stringify({ state: { setUiScale: 0, appliedUiZoom: 2 }, version: 1 })
      );
      await useSettingsStore.persist.rehydrate();
      expect(typeof useSettingsStore.getState().setUiScale).toBe('function');
      expect(useSettingsStore.getState().appliedUiZoom).toBe(1.25);
      useSettingsStore.setState({ appliedUiZoom: null });
    });

    it('defaults a version-1 snapshot missing NSFW intent to false', async () => {
      useSettingsStore.getState().setAllowNsfwContent(true);
      localStorage.setItem('concord-settings', JSON.stringify({ state: {}, version: 1 }));
      await useSettingsStore.persist.rehydrate();
      expect(useSettingsStore.getState().allowNsfwContent).toBe(false);
    });
  });
});
