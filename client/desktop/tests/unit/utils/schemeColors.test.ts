import { resolveUserAccentColors, resolveUserThemeScope } from '@/renderer/utils/ui/schemeColors';
import { contrastRatio, sampleFill, toRgb } from '../styles/contrastPairs';

describe('schemeColors', () => {
  // --- initials pair (review of #3514) ---

  describe("initials on another user's colours", () => {
    // Initials drawn on a user's colours took the raw gradient with #fff or the
    // viewer's --on-brand, neither chosen for that gradient: white on Hacker's
    // green read 1.37:1. Every scheme now carries a fill and label pair.
    const worst = (fill?: string, text?: string) => {
      const samples = fill ? sampleFill(fill) : null;
      const rgb = text ? toRgb(text) : null;
      if (!samples || !rgb) return 0; // no pair to measure
      return Math.min(...samples.map((bg) => contrastRatio(rgb, bg)));
    };

    it.each([
      'concord',
      'morky',
      'bardic',
      'hacker',
      'foxden',
      'spooky',
      'leviathan',
      'grassynill',
      'cottoncandy',
      'driftwood',
      'eclipse',
      'midnightsky',
      'agency',
      'defacto',
      'pride',
    ])('holds 4.5:1 at every point of the %s fill', (scheme) => {
      const colors = resolveUserAccentColors(JSON.stringify({ scheme }));
      expect(worst(colors?.fill, colors?.text)).toBeGreaterThanOrEqual(4.5);
    });

    it('keeps two custom pairs that share a colour apart', () => {
      // Custom pairs are cached; a key on one colour would hand the second user
      // the first user's fill.
      const first = resolveUserAccentColors(
        JSON.stringify({ scheme: 'custom', accentPrimary: '#ffff00', accentSecondary: '#0000ff' })
      );
      const second = resolveUserAccentColors(
        JSON.stringify({ scheme: 'custom', accentPrimary: '#ffff00', accentSecondary: '#00ff00' })
      );
      expect(second?.gradient).toContain('#00ff00');
      expect(second?.fill).not.toBe(first?.fill);
    });

    const hex = (i: number) => `#${i.toString(16).padStart(6, '0')}`;
    // The cache is module state shared by every case here. Each timed case starts
    // its clock ten minutes past the last one, so every pair an earlier case left
    // behind is idle rather than stamped by a clock that has since moved back.
    let clock = Date.now() + 86_400_000;
    const freshClock = () => {
      vi.useFakeTimers({ toFake: ['Date'] });
      clock += 10 * 60_000;
      vi.setSystemTime(clock);
    };

    it('drops a pair idle for a minute past the soft limit, never a pair in use', () => {
      // Past the soft limit only an idle pair is dropped. A plain count bound
      // thrashed on a large list (the next case), so idleness is what stops the
      // cache growing with every palette ever seen (review of #3514).
      freshClock();
      try {
        const custom = (primary: string) =>
          resolveUserAccentColors(
            JSON.stringify({ scheme: 'custom', accentPrimary: primary, accentSecondary: '#abcdef' })
          );
        const kept = custom('#123456');
        const idle = custom(hex(1));
        vi.advanceTimersByTime(60_000);
        for (let i = 2; i < 302; i += 1) {
          expect(custom('#123456'), 'a pair in use is never rebuilt').toBe(kept);
          custom(hex(i));
        }
        expect(custom('#123456')).toBe(kept);
        expect(custom(hex(1)), 'a pair idle for a minute is dropped').not.toBe(idle);
      } finally {
        vi.useRealTimers();
      }
    });

    it("restarts a pair's idle clock each time it is used", () => {
      // A pair used 30 seconds ago is not idle, even once every pair ahead of it
      // has been dropped and it comes up for eviction.
      freshClock();
      try {
        const custom = (i: number) =>
          resolveUserAccentColors(
            JSON.stringify({ scheme: 'custom', accentPrimary: hex(i), accentSecondary: '#0d0e0f' })
          );
        for (let i = 1; i <= 300; i += 1) custom(0x300000 + i);
        const kept = custom(0x300000);
        vi.advanceTimersByTime(60_000);
        expect(custom(0x300000)).toBe(kept);
        vi.advanceTimersByTime(30_000);
        for (let i = 301; i <= 600; i += 1) custom(0x300000 + i);
        expect(custom(0x300000), 'used 30 seconds ago, so not idle').toBe(kept);
      } finally {
        vi.useRealTimers();
      }
    });

    it('stays bounded at the hard limit however fast palettes churn', () => {
      // Nothing here is idle, so only the hard limit can drop the oldest pair.
      const custom = (i: number) =>
        resolveUserAccentColors(
          JSON.stringify({ scheme: 'custom', accentPrimary: hex(i), accentSecondary: '#0a0b0c' })
        );
      const first = custom(0x200000);
      for (let i = 1; i <= 4096; i += 1) custom(0x200000 + i);
      expect(custom(0x200000), 'the oldest pair is dropped at the hard limit').not.toBe(first);
    });

    it('keeps every pair of a list larger than the bound across renders', () => {
      // Codex on #3514: least-recently-used eviction thrashes on a list rendered
      // in the same order each time. With 300 palettes and room for 256, each
      // insert evicted the next row's pair just before it was reached, so every
      // render rebuilt all 300. The member and friend lists are not virtualized,
      // so a large server renders every row at once.
      const custom = (i: number) =>
        resolveUserAccentColors(
          JSON.stringify({
            scheme: 'custom',
            accentPrimary: `#${i.toString(16).padStart(6, '0')}`,
            accentSecondary: '#fedcba',
          })
        );
      const rows = Array.from({ length: 300 }, (_, i) => i + 0x100);
      const first = rows.map(custom);
      const second = rows.map(custom);
      const rebuilt = second.filter((colors, i) => colors !== first[i]).length;
      expect(rebuilt, 'pairs rebuilt on the second render').toBe(0);
    });

    it('holds 4.5:1 for a custom pair no plain label colour can hold', () => {
      // White fails on the yellow end and black on the blue one.
      const colors = resolveUserAccentColors(
        JSON.stringify({ scheme: 'custom', accentPrimary: '#ffff00', accentSecondary: '#0000ff' })
      );
      expect(worst(colors?.fill, colors?.text)).toBeGreaterThanOrEqual(4.5);
    });
  });

  // --- resolveUserAccentColors ---

  describe('resolveUserAccentColors', () => {
    it('returns null for null input', () => {
      expect(resolveUserAccentColors(null)).toBeNull();
    });

    it('returns null for undefined input', () => {
      expect(resolveUserAccentColors(undefined)).toBeNull();
    });

    it('returns null for empty string', () => {
      expect(resolveUserAccentColors('')).toBeNull();
    });

    it('returns null for invalid JSON', () => {
      expect(resolveUserAccentColors('not-json')).toBeNull();
    });

    it('returns null when scheme is missing', () => {
      expect(resolveUserAccentColors(JSON.stringify({}))).toBeNull();
    });

    it('returns preset colors for known scheme', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'concord' }));
      expect(result).not.toBeNull();
      expect(result!.accentPrimary).toBe('#fa709a');
      expect(result!.accentSecondary).toBe('#ffe13f');
      expect(result!.gradient).toContain('linear-gradient');
    });

    it('returns preset colors for hacker scheme', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'hacker' }));
      expect(result).not.toBeNull();
      expect(result!.accentPrimary).toBe('#00ff41');
    });

    it('returns preset colors for pride scheme', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'pride' }));
      expect(result).not.toBeNull();
      expect(result!.accentPrimary).toBe('#ff4d9e');
      expect(result!.accentSecondary).toBe('#3b9eff');
      expect(result!.gradient).toContain('linear-gradient');
    });

    it('returns null for unknown scheme name', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'unknown' }));
      expect(result).toBeNull();
    });

    it('returns custom colors for custom scheme', () => {
      const result = resolveUserAccentColors(
        JSON.stringify({
          scheme: 'custom',
          accentPrimary: '#ff0000',
          accentSecondary: '#00ff00',
        })
      );
      expect(result).not.toBeNull();
      expect(result!.accentPrimary).toBe('#ff0000');
      expect(result!.accentSecondary).toBe('#00ff00');
      expect(result!.gradient).toContain('#ff0000');
      expect(result!.gradient).toContain('#00ff00');
    });

    it('returns null for custom scheme without accent colors', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'custom' }));
      expect(result).toBeNull();
    });

    it('returns null for custom scheme with only accentPrimary', () => {
      const result = resolveUserAccentColors(
        JSON.stringify({ scheme: 'custom', accentPrimary: '#ff0000' })
      );
      expect(result).toBeNull();
    });

    it('returns colors for all 14 preset schemes', () => {
      const schemes = [
        'concord',
        'morky',
        'bardic',
        'hacker',
        'foxden',
        'spooky',
        'leviathan',
        'grassynill',
        'cottoncandy',
        'driftwood',
        'eclipse',
        'midnightsky',
        'agency',
        'defacto',
      ];
      for (const scheme of schemes) {
        const result = resolveUserAccentColors(JSON.stringify({ scheme }));
        expect(result).not.toBeNull();
        expect(result!.accentPrimary).toBeTruthy();
        expect(result!.gradient).toContain('linear-gradient');
      }
    });

    it('returns defacto accent colors', () => {
      const result = resolveUserAccentColors(JSON.stringify({ scheme: 'defacto' }));
      expect(result).not.toBeNull();
      expect(result!.accentPrimary).toBe('#58a6ff');
      expect(result!.accentSecondary).toBe('#79c0ff');
    });
  });

  // --- resolveUserThemeScope ---

  describe('member-controlled values (#2366 review)', () => {
    // Another member's colour_scheme JSON reaches inline style custom properties.
    const custom = (a: string, b = '#112233') =>
      JSON.stringify({ scheme: 'custom', accentPrimary: a, accentSecondary: b });

    it('ignores custom accents that are not #rrggbb', () => {
      for (const bad of [
        'url(https://example.com/x.png)',
        'red',
        '#12345',
        '#1234567',
        'var(--x)',
      ]) {
        expect(resolveUserAccentColors(custom(bad))).toBeNull();
        expect(resolveUserAccentColors(custom('#112233', bad))).toBeNull();
        expect(resolveUserThemeScope(custom(bad)).customStyles).toBeUndefined();
      }
      expect(resolveUserAccentColors(custom('#AABBCC'))?.accentPrimary).toBe('#AABBCC');
      expect(resolveUserThemeScope(custom('#aabbcc')).customStyles).toBeDefined();
    });

    it('does not treat an Object member name as a preset scheme', () => {
      for (const scheme of ['constructor', 'toString', '__proto__', 'hasOwnProperty']) {
        expect(resolveUserAccentColors(JSON.stringify({ scheme }))).toBeNull();
      }
    });
  });

  describe('resolveUserThemeScope', () => {
    it('returns concord/dark fallback for null input', () => {
      const result = resolveUserThemeScope(null);
      expect(result.scheme).toBe('concord');
      expect(result.themeMode).toBe('dark');
      expect(result.customStyles).toBeUndefined();
    });

    it('returns concord/dark fallback for undefined input', () => {
      const result = resolveUserThemeScope(undefined);
      expect(result.scheme).toBe('concord');
      expect(result.themeMode).toBe('dark');
    });

    it('returns concord/dark fallback for invalid JSON', () => {
      const result = resolveUserThemeScope('broken');
      expect(result.scheme).toBe('concord');
    });

    it('returns concord/dark fallback when scheme is missing', () => {
      const result = resolveUserThemeScope(JSON.stringify({}));
      expect(result.scheme).toBe('concord');
    });

    it('resolves preset scheme with dark mode', () => {
      const result = resolveUserThemeScope(JSON.stringify({ scheme: 'hacker', themeMode: 'dark' }));
      expect(result.scheme).toBe('hacker');
      expect(result.themeMode).toBe('dark');
      expect(result.customStyles).toBeUndefined();
    });

    it('resolves preset scheme with light mode', () => {
      const result = resolveUserThemeScope(JSON.stringify({ scheme: 'morky', themeMode: 'light' }));
      expect(result.scheme).toBe('morky');
      expect(result.themeMode).toBe('light');
    });

    it('defaults themeMode to dark when not specified', () => {
      const result = resolveUserThemeScope(JSON.stringify({ scheme: 'concord' }));
      expect(result.themeMode).toBe('dark');
    });

    it('returns fallback for unknown preset scheme', () => {
      const result = resolveUserThemeScope(JSON.stringify({ scheme: 'nonexistent' }));
      expect(result.scheme).toBe('concord');
    });

    it('resolves custom scheme with inline CSS variables', () => {
      const result = resolveUserThemeScope(
        JSON.stringify({
          scheme: 'custom',
          themeMode: 'dark',
          accentPrimary: '#ff0000',
          accentSecondary: '#00ff00',
        })
      );
      expect(result.scheme).toBe('custom');
      expect(result.themeMode).toBe('dark');
      expect(result.customStyles).toBeDefined();
      // customStyles should be a CSSProperties object with CSS variable keys
      expect(typeof result.customStyles).toBe('object');
    });

    it('returns fallback for custom scheme without accent colors', () => {
      const result = resolveUserThemeScope(JSON.stringify({ scheme: 'custom' }));
      expect(result.scheme).toBe('concord');
    });

    it('resolves custom scheme with light mode', () => {
      const result = resolveUserThemeScope(
        JSON.stringify({
          scheme: 'custom',
          themeMode: 'light',
          accentPrimary: '#ff0000',
          accentSecondary: '#00ff00',
        })
      );
      expect(result.scheme).toBe('custom');
      expect(result.themeMode).toBe('light');
      expect(result.customStyles).toBeDefined();
    });
  });
});
