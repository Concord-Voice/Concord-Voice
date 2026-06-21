import { describe, it, expect, beforeEach } from 'vitest';
import { renderHook } from '@testing-library/react';
import { useUserThemeScope } from '@/renderer/hooks/useUserThemeScope';
import { useSettingsStore } from '@/renderer/stores/settingsStore';

describe('useUserThemeScope', () => {
  beforeEach(() => {
    useSettingsStore.getState().setHighContrast(false);
  });

  it('returns the viewed user scheme/theme attributes for a preset scheme', () => {
    const colorSchemeJson = JSON.stringify({ scheme: 'concord', themeMode: 'dark' });
    const { result } = renderHook(() => useUserThemeScope(colorSchemeJson));
    expect(result.current.scopeProps['data-scheme']).toBe('concord');
    expect(result.current.scopeProps['data-theme']).toBe('dark');
  });

  it('emits inline CSS-var overrides for a custom scheme', () => {
    const colorSchemeJson = JSON.stringify({
      scheme: 'custom',
      themeMode: 'dark',
      accentPrimary: '#abcdef',
      accentSecondary: '#fedcba',
    });
    const { result } = renderHook(() => useUserThemeScope(colorSchemeJson));
    expect(result.current.scopeProps['data-scheme']).toBe('');
    expect(result.current.scopeProps.style).toBeDefined();
  });

  // Regression: HCM is a global accessibility override that intentionally
  // discards per-user color identity. Without this short-circuit, identity
  // popups (UserPopover / MemberProfileCard / etc.) re-set the viewed
  // user's color scheme on themselves, defeating HCM for that subtree.
  // Custom schemes are especially load-bearing because their inline `style`
  // CSS-var overrides win against any CSS selector regardless of specificity.
  it('returns empty scope props when the viewer has HCM enabled (preset)', () => {
    useSettingsStore.getState().setHighContrast(true);
    const colorSchemeJson = JSON.stringify({ scheme: 'concord', themeMode: 'dark' });
    const { result } = renderHook(() => useUserThemeScope(colorSchemeJson));
    expect(result.current.scopeProps['data-scheme']).toBe('');
    expect(result.current.scopeProps['data-theme']).toBe('');
    expect(result.current.scopeProps.style).toBeUndefined();
  });

  it('returns empty scope props when the viewer has HCM enabled (custom)', () => {
    useSettingsStore.getState().setHighContrast(true);
    const colorSchemeJson = JSON.stringify({
      scheme: 'custom',
      themeMode: 'dark',
      accentPrimary: '#abcdef',
      accentSecondary: '#fedcba',
    });
    const { result } = renderHook(() => useUserThemeScope(colorSchemeJson));
    // Critical: inline style must NOT leak through — it would beat every
    // CSS selector including HCM otherwise.
    expect(result.current.scopeProps.style).toBeUndefined();
  });

  it('re-evaluates when the viewer toggles HCM', () => {
    const colorSchemeJson = JSON.stringify({ scheme: 'foxden', themeMode: 'dark' });
    const { result, rerender } = renderHook(() => useUserThemeScope(colorSchemeJson));

    expect(result.current.scopeProps['data-scheme']).toBe('foxden');

    useSettingsStore.getState().setHighContrast(true);
    rerender();
    expect(result.current.scopeProps['data-scheme']).toBe('');

    useSettingsStore.getState().setHighContrast(false);
    rerender();
    expect(result.current.scopeProps['data-scheme']).toBe('foxden');
  });
});
