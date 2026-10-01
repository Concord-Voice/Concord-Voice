/**
 * Static map of preset color scheme names → accent colors.
 * Values extracted from styles/index.css dark-mode scheme definitions.
 *
 * Used to render per-user identity colors (avatar fallback, banner fallback)
 * based on another user's chosen color scheme, without parsing CSS at runtime.
 */

import type { CSSProperties } from 'react';
import {
  deriveBrandPair,
  deriveThemeVariables,
  isValidHex,
  type DerivedThemeVariables,
} from './colorUtils';

export interface SchemeAccentColors {
  accentPrimary: string;
  accentSecondary: string;
  /** The user's gradient, for surfaces that carry no text: banners, swatches. */
  gradient: string;
  /**
   * The fill for initials drawn on the user's colours, and the label colour that
   * holds 4.5:1 at every point of it. The viewer's `--on-brand` is chosen for the
   * viewer's gradient, not this one, so initials take both or neither.
   */
  fill: string;
  text: string;
}

export interface UserThemeScope {
  scheme: string;
  themeMode: 'dark' | 'light';
  /** Only set for custom schemes — inline CSS variables to override the cascade */
  customStyles?: CSSProperties;
}

const SCHEME_ACCENTS: Record<string, [primary: string, secondary: string]> = {
  concord: ['#fa709a', '#ffe13f'],
  morky: ['#e63946', '#ff6b35'],
  bardic: ['#c471ed', '#f64f8e'],
  hacker: ['#00ff41', '#00ee38'],
  foxden: ['#ff6d00', '#ff9100'],
  spooky: ['#ff6a00', '#8b20aa'],
  leviathan: ['#0ea5e9', '#06b6d4'],
  grassynill: ['#6b8e23', '#8b7355'],
  cottoncandy: ['#ff6ea8', '#40c8ff'],
  driftwood: ['#c8a46c', '#a07848'],
  eclipse: ['#cc0000', '#880000'],
  midnightsky: ['#6d8cff', '#a78bfa'],
  agency: ['#e0004e', '#017fa4'],
  defacto: ['#58a6ff', '#79c0ff'],
  pride: ['#ff4d9e', '#3b9eff'],
};

function buildGradient(primary: string, secondary: string): string {
  return `linear-gradient(135deg, ${primary} 0%, ${secondary} 100%)`;
}

function buildColors(primary: string, secondary: string): SchemeAccentColors {
  const gradient = buildGradient(primary, secondary);
  const { fill, text } = deriveBrandPair(primary, secondary, '#ffffff', gradient);
  return { accentPrimary: primary, accentSecondary: secondary, gradient, fill, text };
}

// Pre-build the full SchemeAccentColors objects for each preset
const PRESET_COLORS: Record<string, SchemeAccentColors> = {};
for (const [name, [p, s]] of Object.entries(SCHEME_ACCENTS)) {
  PRESET_COLORS[name] = buildColors(p, s);
}

// Custom colours are resolved on every message render, and deriving a pair
// samples the whole gradient, so each pair is kept. The keys come from other
// members' profiles, so the cache is bounded, but not by a plain count: the
// member and friend lists are not virtualized, and a list rendering more
// palettes than a fixed bound, in the same order each time, evicts the pair
// the next row needs on every insert and rebuilds all of them on every render.
// So past the soft limit only a pair idle for a minute is dropped, which keeps a
// list's whole working set, and the hard limit bounds memory whatever the churn.
// A Map iterates in insertion order and each use re-inserts its key, so the
// least recently used pair is always first.
const CUSTOM_COLORS = new Map<string, { colors: SchemeAccentColors; usedAt: number }>();
const CUSTOM_COLORS_SOFT_LIMIT = 256;
const CUSTOM_COLORS_HARD_LIMIT = 4096;
const CUSTOM_COLORS_IDLE_MS = 60_000;

function customColors(primary: string, secondary: string): SchemeAccentColors {
  const key = `${primary}|${secondary}`;
  const now = Date.now();
  const cached = CUSTOM_COLORS.get(key);
  if (cached) {
    CUSTOM_COLORS.delete(key);
    CUSTOM_COLORS.set(key, { colors: cached.colors, usedAt: now });
    return cached.colors;
  }
  const colors = buildColors(primary, secondary);
  evictCustomColors(now);
  CUSTOM_COLORS.set(key, { colors, usedAt: now });
  return colors;
}

function evictCustomColors(now: number): void {
  for (const [key, entry] of CUSTOM_COLORS) {
    if (CUSTOM_COLORS.size < CUSTOM_COLORS_SOFT_LIMIT) return;
    const idle = now - entry.usedAt >= CUSTOM_COLORS_IDLE_MS;
    if (!idle && CUSTOM_COLORS.size < CUSTOM_COLORS_HARD_LIMIT) return;
    CUSTOM_COLORS.delete(key);
  }
}

/** The inline style for initials on a user's colours, or nothing for the theme's own. */
export function identityInitialStyle(
  colors: Pick<SchemeAccentColors, 'fill' | 'text'> | null | undefined
): CSSProperties | undefined {
  return colors ? { background: colors.fill, color: colors.text } : undefined;
}

/**
 * Resolve a user's server-stored color_scheme JSON into accent colors.
 *
 * @param colorSchemeJson - The raw JSON string from the user profile, or null/undefined.
 * @returns Resolved accent colors, or null if not set / invalid (use global theme fallback).
 */
export function resolveUserAccentColors(
  colorSchemeJson: string | null | undefined
): SchemeAccentColors | null {
  if (!colorSchemeJson) return null;

  try {
    const parsed = JSON.parse(colorSchemeJson) as {
      scheme?: string;
      accentPrimary?: string;
      accentSecondary?: string;
    };

    if (!parsed.scheme) return null;

    // Custom theme — user provided accent colors. Another member controls this JSON,
    // and the values land in inline styles, so only #rrggbb is accepted.
    const primary = parsed.accentPrimary ?? '';
    const secondary = parsed.accentSecondary ?? '';
    if (parsed.scheme === 'custom' && isValidHex(primary) && isValidHex(secondary)) {
      return customColors(primary, secondary);
    }

    // Preset scheme — own-key lookup, so a scheme named 'constructor' is not a colour set
    return Object.hasOwn(PRESET_COLORS, parsed.scheme) ? PRESET_COLORS[parsed.scheme] : null;
  } catch {
    return null;
  }
}

/** Known preset scheme names (used to distinguish preset vs unknown) */
const PRESET_SCHEME_NAMES = new Set(Object.keys(SCHEME_ACCENTS));

/**
 * Convert DerivedThemeVariables to a React CSSProperties object.
 * CSS custom properties are valid React inline style keys when cast.
 */
function themeVarsToCSSProperties(vars: DerivedThemeVariables): CSSProperties {
  const style: Record<string, string> = {};
  for (const [key, value] of Object.entries(vars)) {
    style[key] = value;
  }
  return style as CSSProperties;
}

/**
 * Resolve a user's color_scheme JSON into scoped theme data attributes + optional inline styles.
 *
 * For preset schemes, returns { scheme, themeMode } which map to existing
 * [data-scheme='X'][data-theme='Y'] CSS selectors.
 *
 * For custom schemes, returns inline CSS variables via customStyles since
 * there are no CSS rules to match.
 *
 * @param colorSchemeJson - The raw JSON string from the user profile, or null/undefined.
 * @returns Theme scope data for identity component roots.
 */
export function resolveUserThemeScope(colorSchemeJson: string | null | undefined): UserThemeScope {
  const fallback: UserThemeScope = { scheme: 'concord', themeMode: 'dark' };

  if (!colorSchemeJson) return fallback;

  try {
    const parsed = JSON.parse(colorSchemeJson) as {
      scheme?: string;
      themeMode?: 'dark' | 'light';
      accentPrimary?: string;
      accentSecondary?: string;
    };

    if (!parsed.scheme) return fallback;

    const themeMode = parsed.themeMode === 'light' ? 'light' : 'dark';

    // Custom scheme — generate inline CSS variables (from #rrggbb accents only; see above)
    const primary = parsed.accentPrimary ?? '';
    const secondary = parsed.accentSecondary ?? '';
    if (parsed.scheme === 'custom' && isValidHex(primary) && isValidHex(secondary)) {
      const isDark = themeMode === 'dark';
      const vars = deriveThemeVariables(
        {
          background: isDark ? '#0d0821' : '#f5f5f7',
          accentPrimary: primary,
          accentSecondary: secondary,
        },
        isDark
      );
      return {
        scheme: 'custom',
        themeMode,
        customStyles: themeVarsToCSSProperties(vars),
      };
    }

    // Preset scheme — validate it exists
    if (PRESET_SCHEME_NAMES.has(parsed.scheme)) {
      return { scheme: parsed.scheme, themeMode };
    }

    return fallback;
  } catch {
    return fallback;
  }
}
