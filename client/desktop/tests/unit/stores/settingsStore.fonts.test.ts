import { describe, it, expect, beforeEach } from 'vitest';
import { useSettingsStore } from '@/renderer/stores/ui/settingsStore';
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
  const s = useSettingsStore.getState();
  s.setFontMode('one');
  s.setFontHeadings('default');
  s.setFontNavigation('default');
  s.setFontMessages('default');
  for (const k of ['fontHeadings', 'fontNav', 'fontMessages', 'fontBrand']) {
    delete document.documentElement.dataset[k];
  }
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

describe('settingsStore — layered fonts (#2366)', () => {
  const attrs = () => {
    const d = document.documentElement.dataset;
    return {
      appfont: d.appfont,
      headings: d.fontHeadings,
      nav: d.fontNav,
      messages: d.fontMessages,
      brand: d.fontBrand,
    };
  };

  it('defaults to One Font with every area at default', () => {
    expect(useSettingsStore.getState().appearance).toMatchObject({
      fontMode: 'one',
      fontHeadings: 'default',
      fontNavigation: 'default',
      fontMessages: 'default',
    });
  });

  it('a One Font pick writes interface and headings; regions and brand stay default', () => {
    useSettingsStore.getState().setAppFont('inter');
    expect(attrs()).toEqual({
      appfont: 'inter',
      headings: 'inter',
      nav: 'default',
      messages: 'default',
      brand: 'default',
    });
  });

  it('Font by Area writes each area attribute from its own key', () => {
    const s = useSettingsStore.getState();
    s.setFontMode('area');
    s.setFontHeadings('lexend');
    s.setFontNavigation('inter');
    s.setFontMessages('sourcesans');
    expect(attrs()).toEqual({
      appfont: 'default',
      headings: 'lexend',
      nav: 'inter',
      messages: 'sourcesans',
      brand: 'default',
    });
  });

  it('Dyslexic Support writes opendyslexic to every attribute, brand included', () => {
    const s = useSettingsStore.getState();
    s.setFontMode('area');
    s.setFontHeadings('lexend');
    s.setDyslexicSupport(true);
    expect(attrs()).toEqual({
      appfont: 'opendyslexic',
      headings: 'opendyslexic',
      nav: 'opendyslexic',
      messages: 'opendyslexic',
      brand: 'opendyslexic',
    });
  });

  // One setter at a time: every font input must re-fire the sink on its own, or an
  // area change would not apply until some other input changed.
  it.each([
    ['setFontHeadings', 'fontHeadings'],
    ['setFontNavigation', 'fontNav'],
    ['setFontMessages', 'fontMessages'],
  ] as const)('%s alone re-applies data-%s in Font by Area', (setter, attr) => {
    const s = useSettingsStore.getState();
    s.setFontMode('area');
    s[setter]('lexend');
    expect(document.documentElement.dataset[attr]).toBe('lexend');
  });

  it('a colour-scheme change alone re-resolves the theme-bundled body font', () => {
    const s = useSettingsStore.getState();
    const previous = s.appearance.colorScheme;
    s.setColorScheme('agency');
    expect(attrs().appfont).toBe('atkinson');
    expect(attrs().headings).toBe('default');
    s.setColorScheme('concord');
    expect(attrs().appfont).toBe('default');
    s.setColorScheme(previous);
  });

  it('switching back to One Font keeps the area picks stored but stops applying them', () => {
    const s = useSettingsStore.getState();
    s.setFontMode('area');
    s.setFontNavigation('lato');
    s.setFontMode('one');
    expect(useSettingsStore.getState().appearance.fontNavigation).toBe('lato');
    expect(attrs().nav).toBe('default');
  });
});
