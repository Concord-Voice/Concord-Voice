import {
  useSettingsStore,
  clampUiScale,
  UI_SCALE_DEFAULT,
  UI_SCALE_MIN,
  UI_SCALE_MAX,
} from '@/renderer/stores/settingsStore';
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
      useSettingsStore.getState().setUiScale(0.5);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_MIN);
    });

    it('clamps above UI_SCALE_MAX', () => {
      useSettingsStore.getState().setUiScale(3);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_MAX);
    });

    it('non-finite input falls back to default', () => {
      useSettingsStore.getState().setUiScale(Number.NaN);
      expect(useSettingsStore.getState().appearance.uiScale).toBe(UI_SCALE_DEFAULT);
    });

    it('exposes clampUiScale as a pure helper', () => {
      expect(clampUiScale(1)).toBe(1);
      expect(clampUiScale(0.5)).toBe(UI_SCALE_MIN);
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
  // module; their tests live in tests/unit/stores/colorSyncSuppression.test.ts.

  describe('persistence', () => {
    it('persists settings to localStorage', () => {
      useSettingsStore.getState().setTheme('light');
      useSettingsStore.getState().setColorScheme('morky');
      const stored = JSON.parse(localStorage.getItem('concord-settings') || '{}');
      expect(stored.state?.appearance?.theme).toBe('light');
      expect(stored.state?.appearance?.colorScheme).toBe('morky');
    });
  });
});
