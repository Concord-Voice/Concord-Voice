import { describe, it, expect, beforeEach } from 'vitest';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { resetAllStores } from '../../helpers/store-helpers';

// resetAllStores() does NOT cover the settings store (it only clears storage), and
// the effective-font subscriber has an equality guard — setting an unchanged value
// does not re-fire the sink. So each test must establish a 'default' baseline and
// exercise the sink through real value transitions.
beforeEach(() => {
  resetAllStores();
  useSettingsStore.getState().setDyslexicSupport(false);
  useSettingsStore.getState().setAppFont('default');
  delete document.documentElement.dataset.appfont;
});

describe('settingsStore — application font', () => {
  it('exposes the new fields with their defaults', () => {
    const a = useSettingsStore.getState().appearance;
    expect(a.appFont).toBe('default');
    expect(a.dyslexicSupport).toBe(false);
  });

  it('setAppFont updates state and writes a single data-appfont attribute', () => {
    useSettingsStore.getState().setAppFont('inter');
    expect(useSettingsStore.getState().appearance.appFont).toBe('inter');
    expect(document.documentElement.dataset.appfont).toBe('inter');
    // single sink — no secondary attribute for dyslexic/theme
    expect(document.documentElement.dataset.dyslexic).toBeUndefined();
  });

  it('a "default" pick resolves to data-appfont="default"', () => {
    useSettingsStore.getState().setAppFont('inter'); // transition away…
    useSettingsStore.getState().setAppFont('default'); // …and back (fires the sink)
    expect(document.documentElement.dataset.appfont).toBe('default');
  });

  it('dyslexicSupport overrides the pick; turning it off restores the pick (Q2 restore)', () => {
    useSettingsStore.getState().setAppFont('inter');
    expect(document.documentElement.dataset.appfont).toBe('inter');
    useSettingsStore.getState().setDyslexicSupport(true);
    expect(document.documentElement.dataset.appfont).toBe('opendyslexic');
    useSettingsStore.getState().setDyslexicSupport(false);
    expect(document.documentElement.dataset.appfont).toBe('inter');
  });
});
