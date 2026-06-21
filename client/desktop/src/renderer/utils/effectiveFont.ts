// Pure effective-font resolver — the single source of truth for "which font applies".
// No DOM and no *runtime* store imports — unit-testable in isolation. See
// [internal]specs/2026-06-18-1642-appearance-fonts-foundation-design.md §3.1.

// Type-only import: erased by esbuild → zero runtime coupling, no circular dependency
// (the runtime import edge stays one-way settingsStore → effectiveFont).
import type { AppearanceSettings } from '../stores/settingsStore';

export type AppFontId =
  | 'default' // Concord Voice Default (brand) — ALSO the "no explicit pick" sentinel
  | 'system' // OS UI font stack
  | 'opendyslexic'
  | 'inter'
  | 'lexend'
  | 'lato' // already bundled (Agency body)
  | 'atkinson'; // already bundled — CSS family 'Atkinson Hyperlegible Next'

export const DYSLEXIA_FONT: AppFontId = 'opendyslexic';
export const APP_DEFAULT_FONT: AppFontId = 'default';

export interface FontResolverConfig {
  // Q1: does an explicit user pick override a font-bundling theme?
  //   'theme-wins' → theme locks its font; 'user-wins' → explicit pick overrides.
  themeVsUser: 'theme-wins' | 'user-wins';
}

export interface FontResolverInput {
  dyslexicSupport: boolean;
  appFont: AppFontId; // 'default' means "no explicit pick"
  themeBundledFont: AppFontId | null; // #1643 fills it via SCHEME_FONTS (Agency → 'atkinson')
}

export interface FontResolution {
  effective: AppFontId;
  pickerLocked: boolean; // #1644 greys the Appearance picker when true
  lockReason: 'dyslexic' | 'theme' | null;
}

export function resolveEffectiveFont(
  input: FontResolverInput,
  cfg: FontResolverConfig
): FontResolution {
  // Layer 1 — Dyslexic Support: authoritative, beats everything.
  if (input.dyslexicSupport) {
    return { effective: DYSLEXIA_FONT, pickerLocked: true, lockReason: 'dyslexic' };
  }

  const userPicked = input.appFont !== APP_DEFAULT_FONT;

  // Layers 2 + 3 — theme vs user pick. The only place Q1 changes behavior.
  if (input.themeBundledFont) {
    if (cfg.themeVsUser === 'user-wins' && userPicked) {
      return { effective: input.appFont, pickerLocked: false, lockReason: null };
    }
    // 'theme-wins', or the user made no explicit pick: theme locks its font.
    return { effective: input.themeBundledFont, pickerLocked: true, lockReason: 'theme' };
  }

  // Layer 3 — user pick (no bundling theme active).
  if (userPicked) {
    return { effective: input.appFont, pickerLocked: false, lockReason: null };
  }

  // Layer 4 — app default.
  return { effective: APP_DEFAULT_FONT, pickerLocked: false, lockReason: null };
}

/**
 * Per-scheme bundled body font. Only schemes that bundle a font appear here;
 * every other scheme inherits the base body stack (returns null). #1643: Agency
 * bundles Atkinson Hyperlegible Next (its already-Atkinson display face + a
 * low-vision-legible body). The `Partial<Record<…>>` annotation validates the
 * keys are real scheme ids AND the values are real AppFontIds, and (unlike
 * `satisfies`, which keeps the narrow `{ agency }` type) lets us index by any
 * scheme id → `AppFontId | undefined`.
 */
const SCHEME_FONTS: Partial<Record<AppearanceSettings['colorScheme'], AppFontId>> = {
  agency: 'atkinson',
};

/** Returns the active scheme's bundled font id, or null. The #1643 seam. */
export function themeBundledFontFor(
  colorScheme: AppearanceSettings['colorScheme']
): AppFontId | null {
  return SCHEME_FONTS[colorScheme] ?? null;
}

/** The shipped C1 configuration: an explicit user pick wins over a theme font (Q1). */
export const RESOLVER_CONFIG: FontResolverConfig = { themeVsUser: 'user-wins' };
