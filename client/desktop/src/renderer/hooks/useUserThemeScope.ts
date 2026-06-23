import { useMemo, type CSSProperties } from 'react';
import { resolveUserThemeScope } from '../utils/schemeColors';
import { useSettingsStore } from '../stores/settingsStore';

interface ThemeScopeProps {
  'data-scheme': string;
  'data-theme': string;
  style?: CSSProperties;
}

export const EMPTY_USER_THEME_SCOPE: ThemeScopeProps = {
  'data-scheme': '',
  'data-theme': '',
};

/**
 * Returns props to spread on an identity component's root element,
 * scoping all CSS variable inheritance to the viewed user's color scheme
 * instead of the viewer's global theme.
 *
 * For preset schemes, sets data-scheme and data-theme attributes that
 * match existing CSS selectors in index.css.
 *
 * For custom schemes, additionally provides inline CSS variable overrides.
 *
 * **High Contrast short-circuit:** when the VIEWER has High Contrast mode
 * on, this hook returns empty scope props. Per-user theme scoping would
 * otherwise re-set the viewed user's accent palette on the popup, defeating
 * the HCM cascade — viewer turns on HCM, opens a user popup, popup renders
 * in the user's pink/purple/etc. scheme instead of stark HCM. HCM is an
 * accessibility override that intentionally discards per-user color
 * identity, so honoring it across identity components is correct. Custom
 * schemes are doubly affected because their inline `style` CSS-var
 * overrides win against any CSS selector regardless of specificity; the
 * short-circuit handles both preset and custom uniformly.
 */
export function useUserThemeScope(colorSchemeJson: string | null | undefined): {
  scopeProps: ThemeScopeProps;
} {
  const highContrast = useSettingsStore((s) => s.appearance.highContrast);

  const scopeProps = useMemo<ThemeScopeProps>(() => {
    if (highContrast) return EMPTY_USER_THEME_SCOPE;
    const scope = resolveUserThemeScope(colorSchemeJson);
    const props: ThemeScopeProps = {
      'data-scheme': scope.scheme === 'custom' ? '' : scope.scheme,
      'data-theme': scope.themeMode,
    };
    if (scope.customStyles) {
      props.style = scope.customStyles;
    }
    return props;
  }, [colorSchemeJson, highContrast]);

  return { scopeProps };
}
