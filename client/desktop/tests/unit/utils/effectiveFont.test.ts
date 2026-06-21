import { describe, it, expect } from 'vitest';
import {
  resolveEffectiveFont,
  themeBundledFontFor,
  RESOLVER_CONFIG,
} from '@/renderer/utils/effectiveFont';

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
