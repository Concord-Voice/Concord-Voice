// Pure effective-font resolver — the single source of truth for "which font applies".
// No DOM and no *runtime* store imports — unit-testable in isolation. See
// [internal]specs/2026-06-18-1642-appearance-fonts-foundation-design.md §3.1.

// Type-only import: erased by esbuild → zero runtime coupling, no circular dependency
// (the runtime import edge stays one-way settingsStore → effectiveFont).
import type { AppearanceSettings } from '../../stores/ui/settingsStore';

/** Every AppFontId. The type derives from this list, so storage/sync validation
 *  (isAppFontId) can never lag a newly added face (#2366). */
export const APP_FONT_IDS = [
  'default', // "Theme Default" (dynamic) — ALSO the "no explicit pick" sentinel
  'sourcesans', // explicit Source Sans; "Concord Voice Default" in the Interface list (CONCORD_DEFAULT_FONT)
  'system', // OS UI font stack
  'opendyslexic',
  'inter',
  'lexend',
  'lato', // already bundled (Agency body)
  'atkinson', // already bundled — CSS family 'Atkinson Hyperlegible Next'
] as const;

export type AppFontId = (typeof APP_FONT_IDS)[number];

export const DYSLEXIA_FONT: AppFontId = 'opendyslexic';
export const APP_DEFAULT_FONT: AppFontId = 'default';
/**
 * "Concord Voice Default" in the Interface list: the base body face, kept whatever the
 * theme bundles. Its headings are the brand display face (see `HeadingsFontId`), so under
 * the shipped user-wins config the pick keeps the base body and heading faces on every
 * theme — identical to Theme Default on all but a bundling scheme. Two things it does not
 * reach: the titlebar wordmark, which keeps the theme's face (only Dyslexic Support moves
 * it), and a theme-wins config, where the theme's font overrides any pick.
 */
export const CONCORD_DEFAULT_FONT: AppFontId = 'sourcesans';

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
  // Own-key lookup: a corrupted persisted or synced scheme such as
  // 'constructor' must not resolve to an Object.prototype member.
  return Object.hasOwn(SCHEME_FONTS, colorScheme) ? (SCHEME_FONTS[colorScheme] ?? null) : null;
}

/** The shipped C1 configuration: an explicit user pick wins over a theme font (Q1). */
export const RESOLVER_CONFIG: FontResolverConfig = { themeVsUser: 'user-wins' };

export function isAppFontId(value: unknown): value is AppFontId {
  return typeof value === 'string' && (APP_FONT_IDS as readonly string[]).includes(value);
}

/** 'one' applies `appFont` everywhere; 'area' applies the per-area keys (#2366). */
export type FontMode = 'one' | 'area';

export function isFontMode(value: unknown): value is FontMode {
  return value === 'one' || value === 'area';
}

/** The wordmark follows the theme's face unless Dyslexic Support is on. */
export type BrandFontId = 'default' | 'opendyslexic';

export interface FontLayersInput extends FontResolverInput {
  fontMode: FontMode;
  fontHeadings: AppFontId;
  fontNavigation: AppFontId;
  fontMessages: AppFontId;
}

/**
 * The Headings layer adds one value no other layer takes: 'concord', the brand display
 * face (Droidiga) pinned by Concord Voice Default. It is derived, never stored.
 */
export type HeadingsFontId = AppFontId | 'concord';

export interface FontLayers {
  interface: AppFontId;
  headings: HeadingsFontId;
  navigation: AppFontId;
  messages: AppFontId;
  brand: BrandFontId;
  pickerLocked: boolean;
  lockReason: FontResolution['lockReason'];
}

/**
 * What Headings resolve to when they "Match Interface": the Interface pick, the brand
 * display face ('concord') for Concord Voice Default, or the theme's display face
 * (`'default'`) while Interface is Theme Default. A theme-bundled font is a body font —
 * it never reaches headings, and under theme-wins a pick the theme overrode must not
 * reach them either. Exported so the picker previews exactly what the resolver applies.
 */
export function headingsMatchingInterface(
  appFont: AppFontId,
  lockReason: FontResolution['lockReason']
): HeadingsFontId {
  if (lockReason === 'theme') return APP_DEFAULT_FONT;
  return appFont === CONCORD_DEFAULT_FONT ? 'concord' : appFont;
}

/**
 * One resolver call for every font layer (#2366). Interface precedence is exactly
 * `resolveEffectiveFont`'s; the other layers are decided here and nowhere else.
 * 'default' on an area means "Match Interface": Headings follow the Interface pick
 * (see `headingsMatchingInterface`) in BOTH modes; Navigation and Messages inherit, so
 * their text follows Interface and their headers follow Headings.
 */
export function resolveFontLayers(input: FontLayersInput, cfg: FontResolverConfig): FontLayers {
  const base = resolveEffectiveFont(input, cfg);
  if (base.lockReason === 'dyslexic') {
    return {
      interface: DYSLEXIA_FONT,
      headings: DYSLEXIA_FONT,
      navigation: DYSLEXIA_FONT,
      messages: DYSLEXIA_FONT,
      brand: 'opendyslexic',
      pickerLocked: true,
      lockReason: 'dyslexic',
    };
  }
  const shared = {
    interface: base.effective,
    brand: 'default' as const,
    pickerLocked: base.pickerLocked,
    lockReason: base.lockReason,
  };
  const matched = headingsMatchingInterface(input.appFont, base.lockReason);
  if (input.fontMode === 'area') {
    return {
      ...shared,
      headings: input.fontHeadings === APP_DEFAULT_FONT ? matched : input.fontHeadings,
      navigation: input.fontNavigation,
      messages: input.fontMessages,
    };
  }
  // One Font: headings match Interface; the regions inherit, so they need no override.
  return {
    ...shared,
    headings: matched,
    navigation: APP_DEFAULT_FONT,
    messages: APP_DEFAULT_FONT,
  };
}
