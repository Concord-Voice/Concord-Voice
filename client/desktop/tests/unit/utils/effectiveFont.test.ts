import { describe, it, expect } from 'vitest';
import {
  resolveEffectiveFont,
  themeBundledFontFor,
  RESOLVER_CONFIG,
  resolveFontLayers,
  APP_FONT_IDS,
  isAppFontId,
  isFontMode,
  type AppFontId,
  type FontMode,
} from '@/renderer/utils/ui/effectiveFont';

const userWins = { themeVsUser: 'user-wins' as const };
const themeWins = { themeVsUser: 'theme-wins' as const };

describe('resolveEffectiveFont', () => {
  it('dyslexic overrides everything (both configs)', () => {
    for (const cfg of [userWins, themeWins]) {
      const r = resolveEffectiveFont(
        { dyslexicSupport: true, appFont: 'inter', themeBundledFont: 'atkinson' },
        cfg
      );
      expect(r).toEqual({ effective: 'opendyslexic', pickerLocked: true, lockReason: 'dyslexic' });
    }
  });

  it('dyslexic outranks a theme-bundled font AND a user pick (#1644)', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: true, appFont: 'inter', themeBundledFont: 'atkinson' },
      RESOLVER_CONFIG
    );
    expect(r).toEqual({ effective: 'opendyslexic', pickerLocked: true, lockReason: 'dyslexic' });
  });

  it('turning dyslexic off falls through to the prior pick (Q2-restore is structural)', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'inter', themeBundledFont: 'atkinson' },
      RESOLVER_CONFIG
    );
    expect(r.effective).toBe('inter'); // appFont preserved; resolver never mutated it
  });

  it('user-wins: explicit pick overrides theme font', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'inter', themeBundledFont: 'atkinson' },
      userWins
    );
    expect(r).toEqual({ effective: 'inter', pickerLocked: false, lockReason: null });
  });

  it('user-wins: no explicit pick keeps theme font (locked)', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'default', themeBundledFont: 'atkinson' },
      userWins
    );
    expect(r).toEqual({ effective: 'atkinson', pickerLocked: true, lockReason: 'theme' });
  });

  it('theme-wins: theme font locks even over an explicit pick', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'inter', themeBundledFont: 'atkinson' },
      themeWins
    );
    expect(r).toEqual({ effective: 'atkinson', pickerLocked: true, lockReason: 'theme' });
  });

  it('no theme font: explicit pick applies, unlocked', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'lexend', themeBundledFont: null },
      userWins
    );
    expect(r).toEqual({ effective: 'lexend', pickerLocked: false, lockReason: null });
  });

  it('no theme font, no pick: app default', () => {
    const r = resolveEffectiveFont(
      { dyslexicSupport: false, appFont: 'default', themeBundledFont: null },
      userWins
    );
    expect(r).toEqual({ effective: 'default', pickerLocked: false, lockReason: null });
  });

  it('ships C1 config as user-wins', () => {
    expect(RESOLVER_CONFIG.themeVsUser).toBe('user-wins');
  });

  it('themeBundledFontFor: Agency maps to atkinson; other schemes are null (#1643)', () => {
    expect(themeBundledFontFor('agency')).toBe('atkinson');
    expect(themeBundledFontFor('concord')).toBeNull();
    expect(themeBundledFontFor('hacker')).toBeNull();
    expect(themeBundledFontFor('custom')).toBeNull();
  });

  it('Agency theme-layer matrix under the shipped user-wins config (#1643)', () => {
    const themeBundledFont = themeBundledFontFor('agency'); // 'atkinson'

    // No explicit pick → theme provides its font, soft-locked.
    expect(
      resolveEffectiveFont(
        { dyslexicSupport: false, appFont: 'default', themeBundledFont },
        RESOLVER_CONFIG
      )
    ).toEqual({ effective: 'atkinson', pickerLocked: true, lockReason: 'theme' });

    // Explicit non-default pick → user wins, unlocked.
    expect(
      resolveEffectiveFont(
        { dyslexicSupport: false, appFont: 'inter', themeBundledFont },
        RESOLVER_CONFIG
      )
    ).toEqual({ effective: 'inter', pickerLocked: false, lockReason: null });

    // Dyslexic support still overrides the theme.
    expect(
      resolveEffectiveFont(
        { dyslexicSupport: true, appFont: 'default', themeBundledFont },
        RESOLVER_CONFIG
      )
    ).toEqual({ effective: 'opendyslexic', pickerLocked: true, lockReason: 'dyslexic' });
  });
});

describe('resolveFontLayers (#2366)', () => {
  const base = {
    dyslexicSupport: false,
    appFont: 'default' as AppFontId,
    themeBundledFont: null as AppFontId | null,
    fontMode: 'one' as FontMode,
    fontHeadings: 'default' as AppFontId,
    fontNavigation: 'default' as AppFontId,
    fontMessages: 'default' as AppFontId,
  };

  it('One Font with no pick is today: every layer at its default', () => {
    expect(resolveFontLayers(base, RESOLVER_CONFIG)).toEqual({
      interface: 'default',
      headings: 'default',
      navigation: 'default',
      messages: 'default',
      brand: 'default',
      pickerLocked: false,
      lockReason: null,
    });
  });

  it('One Font with an explicit pick moves interface AND headings; regions and brand stay default', () => {
    expect(resolveFontLayers({ ...base, appFont: 'inter' }, RESOLVER_CONFIG)).toMatchObject({
      interface: 'inter',
      headings: 'inter',
      navigation: 'default',
      messages: 'default',
      brand: 'default',
    });
  });

  it('One Font keeps per-area picks stored but does not apply them', () => {
    const r = resolveFontLayers(
      { ...base, fontHeadings: 'lexend', fontNavigation: 'lato', fontMessages: 'inter' },
      RESOLVER_CONFIG
    );
    expect(r).toMatchObject({ headings: 'default', navigation: 'default', messages: 'default' });
  });

  it('Font by Area applies each area independently; interface still comes from appFont', () => {
    const r = resolveFontLayers(
      {
        ...base,
        fontMode: 'area',
        appFont: 'atkinson',
        fontHeadings: 'lexend',
        fontNavigation: 'inter',
        fontMessages: 'sourcesans',
      },
      RESOLVER_CONFIG
    );
    expect(r).toMatchObject({
      interface: 'atkinson',
      headings: 'lexend',
      navigation: 'inter',
      messages: 'sourcesans',
      brand: 'default',
    });
  });

  it('Font by Area: a default Headings matches Interface, exactly as One Font does', () => {
    const area = { ...base, fontMode: 'area' as FontMode };
    // Interface on Theme Default → headings keep the theme's display face.
    expect(resolveFontLayers(area, RESOLVER_CONFIG).headings).toBe('default');
    // An Interface pick carries into headings…
    expect(resolveFontLayers({ ...area, appFont: 'inter' }, RESOLVER_CONFIG).headings).toBe(
      'inter'
    );
    // …a theme-bundled BODY font does not…
    expect(
      resolveFontLayers({ ...area, themeBundledFont: 'atkinson' }, RESOLVER_CONFIG).headings
    ).toBe('default');
    // …Concord Voice Default pins the brand heading face, even on a bundling theme…
    expect(
      resolveFontLayers(
        { ...area, appFont: 'sourcesans', themeBundledFont: 'atkinson' },
        RESOLVER_CONFIG
      )
    ).toMatchObject({ interface: 'sourcesans', headings: 'concord' });
    // …and an explicit Headings pick still wins.
    expect(
      resolveFontLayers({ ...area, appFont: 'inter', fontHeadings: 'lexend' }, RESOLVER_CONFIG)
        .headings
    ).toBe('lexend');
  });

  it('a theme-bundled font drives interface only; headings keep the theme face (Agency, no pick)', () => {
    expect(
      resolveFontLayers({ ...base, themeBundledFont: 'atkinson' }, RESOLVER_CONFIG)
    ).toMatchObject({
      interface: 'atkinson',
      headings: 'default',
      pickerLocked: true,
      lockReason: 'theme',
    });
  });

  it('Dyslexic Support makes every layer, brand included, OpenDyslexic in either mode', () => {
    for (const fontMode of ['one', 'area'] as const) {
      expect(
        resolveFontLayers(
          { ...base, fontMode, dyslexicSupport: true, appFont: 'inter', fontHeadings: 'lexend' },
          RESOLVER_CONFIG
        )
      ).toEqual({
        interface: 'opendyslexic',
        headings: 'opendyslexic',
        navigation: 'opendyslexic',
        messages: 'opendyslexic',
        brand: 'opendyslexic',
        pickerLocked: true,
        lockReason: 'dyslexic',
      });
    }
  });

  it('Font by Area keeps a theme-bundled body font on Interface', () => {
    const agency = {
      ...base,
      fontMode: 'area' as FontMode,
      themeBundledFont: 'atkinson' as AppFontId,
    };
    expect(resolveFontLayers(agency, RESOLVER_CONFIG)).toMatchObject({
      interface: 'atkinson',
      headings: 'default',
      lockReason: 'theme',
    });
    expect(resolveFontLayers({ ...agency, appFont: 'inter' }, RESOLVER_CONFIG)).toMatchObject({
      interface: 'inter',
      lockReason: null,
    });
  });

  it('theme-wins: a pick the theme overrode does not reach headings either', () => {
    const input = {
      ...base,
      appFont: 'inter' as AppFontId,
      themeBundledFont: 'atkinson' as AppFontId,
    };
    expect(resolveFontLayers(input, themeWins)).toMatchObject({
      interface: 'atkinson',
      headings: 'default',
      lockReason: 'theme',
    });
    // user-wins is unchanged: the pick drives both.
    expect(resolveFontLayers(input, userWins)).toMatchObject({
      interface: 'inter',
      headings: 'inter',
    });
  });

  it('One Font: Concord Voice Default keeps the base pair on any theme', () => {
    expect(
      resolveFontLayers(
        { ...base, appFont: 'sourcesans', themeBundledFont: 'atkinson' },
        RESOLVER_CONFIG
      )
    ).toMatchObject({ interface: 'sourcesans', headings: 'concord', lockReason: null });
  });

  it('an ordinary OpenDyslexic PICK goes everywhere except the wordmark', () => {
    expect(resolveFontLayers({ ...base, appFont: 'opendyslexic' }, RESOLVER_CONFIG)).toMatchObject({
      interface: 'opendyslexic',
      headings: 'opendyslexic',
      brand: 'default',
    });
  });
});

describe('font id and mode guards (#2366)', () => {
  it('accepts every declared id and rejects anything else', () => {
    expect(APP_FONT_IDS).toContain('sourcesans');
    for (const id of APP_FONT_IDS) expect(isAppFontId(id)).toBe(true);
    for (const bad of ['', 'comic-sans', 'Default', 42, null, undefined]) {
      expect(isAppFontId(bad)).toBe(false);
    }
  });

  it('a scheme named after an Object member has no bundled font', () => {
    for (const key of ['constructor', 'toString', '__proto__', 'hasOwnProperty']) {
      expect(themeBundledFontFor(key as Parameters<typeof themeBundledFontFor>[0])).toBeNull();
    }
    expect(themeBundledFontFor('agency')).toBe('atkinson');
  });

  it("accepts 'one' and 'area' only", () => {
    expect(isFontMode('one')).toBe(true);
    expect(isFontMode('area')).toBe(true);
    for (const bad of ['basic', 'advanced', '', null, 1]) expect(isFontMode(bad)).toBe(false);
  });
});
