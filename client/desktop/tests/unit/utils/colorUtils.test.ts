import {
  hexToHsl,
  hslToHex,
  lighten,
  darken,
  relativeLuminance,
  contrastColor,
  isValidHex,
  deriveThemeVariables,
  liftToContrast,
  contrastRatio,
  applyCustomThemeVariables,
  clearCustomThemeVariables,
  type CustomColors,
} from '@/renderer/utils/ui/colorUtils';

// ─── hexToHsl ────────────────────────────────────────────────────────────────

describe('hexToHsl', () => {
  it('converts pure red #ff0000', () => {
    const hsl = hexToHsl('#ff0000');
    expect(hsl.h).toBe(0);
    expect(hsl.s).toBe(100);
    expect(hsl.l).toBe(50);
  });

  it('converts pure green #00ff00', () => {
    const hsl = hexToHsl('#00ff00');
    expect(hsl.h).toBe(120);
    expect(hsl.s).toBe(100);
    expect(hsl.l).toBe(50);
  });

  it('converts pure blue #0000ff', () => {
    const hsl = hexToHsl('#0000ff');
    expect(hsl.h).toBe(240);
    expect(hsl.s).toBe(100);
    expect(hsl.l).toBe(50);
  });

  it('converts black #000000', () => {
    const hsl = hexToHsl('#000000');
    expect(hsl.h).toBe(0);
    expect(hsl.s).toBe(0);
    expect(hsl.l).toBe(0);
  });

  it('converts white #ffffff', () => {
    const hsl = hexToHsl('#ffffff');
    expect(hsl.h).toBe(0);
    expect(hsl.s).toBe(0);
    expect(hsl.l).toBe(100);
  });

  it('converts mid-gray #808080', () => {
    const hsl = hexToHsl('#808080');
    expect(hsl.h).toBe(0);
    expect(hsl.s).toBe(0);
    // Allow for rounding: 50 ± 1
    expect(hsl.l).toBeGreaterThanOrEqual(49);
    expect(hsl.l).toBeLessThanOrEqual(51);
  });
});

// ─── hslToHex ────────────────────────────────────────────────────────────────

describe('hslToHex', () => {
  it('converts pure red hsl(0, 100, 50) to #ff0000', () => {
    expect(hslToHex({ h: 0, s: 100, l: 50 })).toBe('#ff0000');
  });

  it('converts black hsl(0, 0, 0) to #000000', () => {
    expect(hslToHex({ h: 0, s: 0, l: 0 })).toBe('#000000');
  });

  it('converts white hsl(0, 0, 100) to #ffffff', () => {
    expect(hslToHex({ h: 0, s: 0, l: 100 })).toBe('#ffffff');
  });

  it('clamps L values above 100', () => {
    const hex = hslToHex({ h: 0, s: 0, l: 150 });
    expect(hex).toBe('#ffffff');
  });

  it('roundtrips with hexToHsl', () => {
    const colors = ['#fa709a', '#ffe13f', '#0ea5e9', '#6b8e23', '#c471ed'];
    for (const original of colors) {
      const hsl = hexToHsl(original);
      const result = hslToHex(hsl);
      // Allow ±3 difference per channel due to HSL rounding
      const origR = parseInt(original.slice(1, 3), 16);
      const origG = parseInt(original.slice(3, 5), 16);
      const origB = parseInt(original.slice(5, 7), 16);
      const resR = parseInt(result.slice(1, 3), 16);
      const resG = parseInt(result.slice(3, 5), 16);
      const resB = parseInt(result.slice(5, 7), 16);
      expect(Math.abs(origR - resR)).toBeLessThanOrEqual(3);
      expect(Math.abs(origG - resG)).toBeLessThanOrEqual(3);
      expect(Math.abs(origB - resB)).toBeLessThanOrEqual(3);
    }
  });
});

// ─── lighten / darken ────────────────────────────────────────────────────────

describe('lighten', () => {
  it('lightens #000000 by 50 to produce L=50', () => {
    const result = hexToHsl(lighten('#000000', 50));
    expect(result.l).toBe(50);
  });

  it('clamps at L=100', () => {
    const result = hexToHsl(lighten('#ffffff', 50));
    expect(result.l).toBe(100);
  });

  it('lighten by 0 returns same color', () => {
    expect(lighten('#808080', 0)).toBe(hslToHex(hexToHsl('#808080')));
  });
});

describe('darken', () => {
  it('darkens #ffffff by 50 to produce L=50', () => {
    const result = hexToHsl(darken('#ffffff', 50));
    expect(result.l).toBe(50);
  });

  it('clamps at L=0', () => {
    const result = hexToHsl(darken('#000000', 50));
    expect(result.l).toBe(0);
  });

  it('darken by 0 returns same color', () => {
    expect(darken('#808080', 0)).toBe(hslToHex(hexToHsl('#808080')));
  });
});

// ─── relativeLuminance ───────────────────────────────────────────────────────

describe('relativeLuminance', () => {
  it('black has luminance ~0', () => {
    expect(relativeLuminance('#000000')).toBeCloseTo(0, 3);
  });

  it('white has luminance ~1', () => {
    expect(relativeLuminance('#ffffff')).toBeCloseTo(1, 3);
  });

  it('mid-gray has luminance ~0.21', () => {
    const lum = relativeLuminance('#808080');
    expect(lum).toBeGreaterThan(0.15);
    expect(lum).toBeLessThan(0.25);
  });
});

// ─── contrastColor ───────────────────────────────────────────────────────────

describe('contrastColor', () => {
  it('returns #ffffff for dark backgrounds', () => {
    expect(contrastColor('#000000')).toBe('#ffffff');
    expect(contrastColor('#0d0821')).toBe('#ffffff');
    expect(contrastColor('#1a1a2e')).toBe('#ffffff');
  });

  it('returns #000000 for light backgrounds', () => {
    expect(contrastColor('#ffffff')).toBe('#000000');
    expect(contrastColor('#f5f5f7')).toBe('#000000');
    expect(contrastColor('#e8e8ec')).toBe('#000000');
  });
});

// ─── isValidHex ──────────────────────────────────────────────────────────────

describe('isValidHex', () => {
  it('accepts valid 6-digit hex colors', () => {
    expect(isValidHex('#fa709a')).toBe(true);
    expect(isValidHex('#000000')).toBe(true);
    expect(isValidHex('#FFFFFF')).toBe(true);
    expect(isValidHex('#AbCdEf')).toBe(true);
  });

  it('rejects invalid formats', () => {
    expect(isValidHex('fa709a')).toBe(false); // no #
    expect(isValidHex('#fff')).toBe(false); // shorthand
    expect(isValidHex('#gggggg')).toBe(false); // invalid chars
    expect(isValidHex('')).toBe(false); // empty
    expect(isValidHex('#12345')).toBe(false); // too short
    expect(isValidHex('#1234567')).toBe(false); // too long
  });
});

// ─── deriveThemeVariables ────────────────────────────────────────────────────

describe('deriveThemeVariables', () => {
  const defaultColors: CustomColors = {
    background: '#0d0821',
    accentPrimary: '#fa709a',
    accentSecondary: '#ffe13f',
  };

  it('returns all 26 expected CSS property keys', () => {
    const vars = deriveThemeVariables(defaultColors, true);
    const keys = Object.keys(vars);
    expect(keys).toHaveLength(26);
    expect(keys).toContain('--bg-primary');
    expect(keys).toContain('--text-primary');
    expect(keys).toContain('--accent-primary');
    expect(keys).toContain('--gradient-brand');
    expect(keys).toContain('--on-accent');
    expect(keys).toContain('--status-connected');
    // A custom theme that derives --danger without --on-danger inherits :root's
    // foreground against a fill :root never saw. Both halves ship together.
    expect(keys).toContain('--on-danger');
    expect(keys).toContain('--on-success');
  });

  it('clears every key it applies', () => {
    // apply writes Object.entries(vars); clear walks the SEPARATE
    // THEME_VARIABLE_KEYS list. A key in one and not the other survives a theme
    // switch as a stale inline style on documentElement, outranking the scheme
    // that replaced it. This fails if the two lists drift apart.
    const vars = deriveThemeVariables(defaultColors, true);
    applyCustomThemeVariables(vars);
    clearCustomThemeVariables();
    for (const key of Object.keys(vars)) {
      expect(document.documentElement.style.getPropertyValue(key)).toBe('');
    }
  });

  describe('dark mode', () => {
    const vars = deriveThemeVariables(defaultColors, true);

    it('bg-primary matches input background', () => {
      expect(vars['--bg-primary']).toBe(defaultColors.background);
    });

    it('bg-secondary is lighter than bg-primary', () => {
      const bgL = hexToHsl(vars['--bg-primary']).l;
      const secL = hexToHsl(vars['--bg-secondary']).l;
      expect(secL).toBeGreaterThan(bgL);
    });

    it('text-primary is white for dark backgrounds', () => {
      expect(vars['--text-primary']).toBe('#ffffff');
    });

    it('accent-primary matches input', () => {
      expect(vars['--accent-primary']).toBe(defaultColors.accentPrimary);
    });

    it('gradient-brand includes both accent colors', () => {
      expect(vars['--gradient-brand']).toContain(defaultColors.accentPrimary);
      expect(vars['--gradient-brand']).toContain(defaultColors.accentSecondary);
    });

    it('on-accent is determined by accent luminance', () => {
      // Default accent #fa709a is a bright pink — luminance > 0.179 → black
      expect(vars['--on-accent']).toBe(contrastColor(defaultColors.accentPrimary));

      // With a very dark accent
      const darkAccentVars = deriveThemeVariables(
        { ...defaultColors, accentPrimary: '#1a0505' },
        true
      );
      expect(darkAccentVars['--on-accent']).toBe('#ffffff');

      // With a very light accent
      const lightAccentVars = deriveThemeVariables(
        { ...defaultColors, accentPrimary: '#ffff00' },
        true
      );
      expect(lightAccentVars['--on-accent']).toBe('#000000');
    });

    it('status colors are fixed', () => {
      expect(vars['--status-connected']).toBe('#1aaa55');
      expect(vars['--status-connecting']).toBe('#e6b432');
      expect(vars['--status-disconnected']).toBe('#fa6464');
    });
  });

  describe('light mode', () => {
    const vars = deriveThemeVariables(defaultColors, false);

    it('bg-primary is very light (L > 90)', () => {
      const bgL = hexToHsl(vars['--bg-primary']).l;
      expect(bgL).toBeGreaterThan(90);
    });

    it('text-primary is very dark (L < 15)', () => {
      const textL = hexToHsl(vars['--text-primary']).l;
      expect(textL).toBeLessThan(15);
    });

    it('status colors are fixed for light mode', () => {
      expect(vars['--status-connected']).toBe('#1a9a4a');
      expect(vars['--status-connecting']).toBe('#c89a20');
      expect(vars['--status-disconnected']).toBe('#e05050');
    });

    it('accents are slightly darker than dark mode accents', () => {
      const darkVars = deriveThemeVariables(defaultColors, true);
      const darkAccentL = hexToHsl(darkVars['--accent-primary']).l;
      const lightAccentL = hexToHsl(vars['--accent-primary']).l;
      expect(lightAccentL).toBeLessThan(darkAccentL);
    });
  });

  describe('liftToContrast — direction and fallback', () => {
    const SURFACES_DARK = ['#0d0821', '#150d35', '#1d124a', '#1a1042', '#201452'];
    const worstAgainst = (c: string, surfaces: string[]) =>
      Math.min(...surfaces.map((s) => contrastRatio(c, s)));

    it('clears the floor searching the preferred direction on a clustered dark ladder', () => {
      const got = liftToContrast('#5a4a9a', SURFACES_DARK, 4.6, true);
      expect(worstAgainst(got, SURFACES_DARK)).toBeGreaterThanOrEqual(4.6);
    });

    it('retries the opposite direction when the preferred one cannot reach the floor', () => {
      // A light background under dark mode: every surface is near-white, so towardsLight
      // is unsatisfiable. Returning '#ffffff' anyway was a 1:1 fail-open (white on white).
      const surfaces = ['#ffffff', '#ffffff', '#ffffff', '#ffffff', '#ffffff'];
      const got = liftToContrast('#ffffff', surfaces, 4.6, true);
      expect(got).not.toBe('#ffffff');
      const w = worstAgainst(got, surfaces);
      expect(w).toBeGreaterThanOrEqual(4.6);
      // Bounded ABOVE deliberately. Without this the test is vacuous: deleting the reverse
      // search still leaves the unsatisfiable-fallback, which returns '#000000' at 21:1 and
      // satisfies every other assertion here. The reverse SEARCH stops at the first candidate
      // that clears the floor, so it lands just above it — 21:1 means the search never ran.
      expect(w).toBeLessThan(8);
      expect(got).not.toBe('#000000');
    });

    it('returns the best candidate SEEN when the floor is unreachable, not a fixed extreme', () => {
      // Surfaces straddling the full luminance range: both extremes score 1:1 while an
      // interior grey scores ~4.5. An extremes-only fallback returns the WORST value here,
      // so the optimum is not always at an extreme — it is only so while surfaces cluster.
      const surfaces = ['#000000', '#ffffff'];
      const got = liftToContrast('#808080', surfaces, 4.6, true);
      const extremesOnly = Math.max(
        worstAgainst('#ffffff', surfaces),
        worstAgainst('#000000', surfaces)
      );
      expect(extremesOnly).toBeCloseTo(1, 1);
      expect(worstAgainst(got, surfaces)).toBeGreaterThan(4);
    });

    it('returns the seed untouched when it already clears the floor', () => {
      const seed = '#ffffff';
      expect(liftToContrast(seed, SURFACES_DARK, 4.6, true)).toBe(seed);
    });
  });
});
