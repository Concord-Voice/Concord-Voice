import { resolveUserAccentColors, resolveUserThemeScope } from '@/renderer/utils/schemeColors';

describe('schemeColors', () => {
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
