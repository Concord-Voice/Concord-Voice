import { create } from 'zustand';
import { persist, subscribeWithSelector } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import {
  type CustomColors,
  deriveThemeVariables,
  applyCustomThemeVariables,
  clearCustomThemeVariables,
  isValidHex,
} from '../../utils/ui/colorUtils';
import { DEFAULT_CLIENT_BEHAVIOR, type ClientBehavior } from '../../../shared/clientBehavior';
import { deriveOverlayColors } from '../../utils/ui/overlayColors';
import { useUserStore } from '../auth/userStore';
import { useMemberStore } from '../chat/memberStore';
// Sync-suppression flag lives in a dependency-free leaf module so that
// userStore can flip it via a STATIC import without a circular dependency or a
// teardown-racing dynamic import. See colorSyncSuppression.ts.
import { isSyncSuppressed } from './colorSyncSuppression';
import {
  type AppFontId,
  type FontLayers,
  type FontMode,
  APP_DEFAULT_FONT,
  isAppFontId,
  isFontMode,
  resolveFontLayers,
  themeBundledFontFor,
  RESOLVER_CONFIG,
} from '../../utils/ui/effectiveFont';
import type { GifPlaybackMode } from '../../utils/ui/gifPlayback';
import {
  type SetZoomFactor,
  UI_ZOOM_EPSILON,
  computeAppliedZoom,
  cssUiScaleFor,
  getZoomBridge,
  isPipWindow,
} from '../../utils/ui/uiZoom';

export interface AppearanceSettings {
  theme: 'dark' | 'light' | 'system';
  colorScheme:
    | 'concord'
    | 'morky'
    | 'bardic'
    | 'hacker'
    | 'foxden'
    | 'spooky'
    | 'leviathan'
    | 'grassynill'
    | 'cottoncandy'
    | 'driftwood'
    | 'eclipse'
    | 'midnightsky'
    | 'agency'
    | 'defacto'
    | 'pride'
    | 'custom';
  fontSize: 'small' | 'default' | 'large';
  compactMode: boolean;
  reduceAnimations: boolean;
  /**
   * The user's CHOSEN UI scale, clamped to [UI_SCALE_MIN, UI_SCALE_MAX]; default
   * 1.0 is a no-op. What it drives depends on the shell (#2367 part 2,
   * `utils/ui/uiZoom.ts`): with the zoom bridge it is Electron page zoom, capped
   * by window width and with `--ui-scale` pinned to 1; without it, `--ui-scale`
   * gets the legacy 0.85–1.3 clamp. Either way it compounds with the discrete
   * `fontSize` rather than overriding it. Never read this as "the current
   * `--ui-scale`" — use `cssUiScaleFor`.
   */
  uiScale: number;
  /**
   * High-contrast mode. Toggles `data-high-contrast` on the document root;
   * a CSS layer in index.css boosts contrast for the highest-impact
   * surfaces (text on tinted backgrounds, borders, focus rings). First-pass
   * — does not perfectly retune every theme but ships a working toggle.
   */
  highContrast: boolean;
  customColors: CustomColors | null;
  /** User-selected application font. 'default' === no explicit pick (brand default). */
  appFont: AppFontId;
  /** Authoritative dyslexia-support overlay (the toggle UI lands in #1644). */
  dyslexicSupport: boolean;
  /**
   * GIF playback gate (#2369). 'auto' === no explicit pick: follow Reduce
   * Animations. REQUIRED rather than optional, deliberately — `preferencesSync`
   * builds its blob from an explicit field literal, so required-ness is what
   * turns "forgot to sync the new key" into a compile error instead of a
   * setting that silently never syncs.
   */
  gifPlayback: GifPlaybackMode;
  /** #2366: 'one' applies `appFont` everywhere; 'area' applies the three keys below. */
  fontMode: FontMode;
  /** #2366 per-area picks, applied only in 'area' mode. 'default' = the area's default. */
  fontHeadings: AppFontId;
  fontNavigation: AppFontId;
  fontMessages: AppFontId;
}

/** Lower + upper bound for the STORED uiScale — the zoom-bridge slider range.
 *
 * Storage is not capability-dependent: every shell stores 0.5–2.0, and the
 * apply path narrows it. With page zoom (#2367 part 2) the whole layout scales
 * together, so 2.0 is safe wherever the window is wide enough — and where it is
 * not, the applier caps the zoom by width instead of the user's choice.
 *
 * The old 1.30 cap ("not 1.50, the chat preview tile and a few other narrow
 * surfaces start collapsing past ~1.20") is still true, but ONLY of the legacy
 * `--ui-scale` engine, which scales text inside unscaled boxes. It now lives on
 * as `UI_SCALE_LEGACY_MAX` (`utils/ui/uiZoom.ts`), applied only on a shell
 * without the zoom bridge, where the slider keeps its 0.85–1.3 range. */
export const UI_SCALE_MIN = 0.5;
export const UI_SCALE_MAX = 2;
export const UI_SCALE_DEFAULT = 1;

/** Clamp + sanity-check uiScale on the way in (slider, persisted state). */
export function clampUiScale(value: number): number {
  if (!Number.isFinite(value)) return UI_SCALE_DEFAULT;
  return Math.min(UI_SCALE_MAX, Math.max(UI_SCALE_MIN, value));
}

export type { CustomColors } from '../../utils/ui/colorUtils';

interface SettingsState {
  appearance: AppearanceSettings;
  clientBehavior: ClientBehavior;
  /**
   * One-time launch-reset explainer acknowledgement (#1301). Set true after the
   * user dismisses `<SubscriptionResetModal>`. Persisted so the modal is shown
   * once, ever — across restarts. Top-level (not an appearance setting); see the
   * persist `merge` below, which must carry it through rehydration.
   */
  subscriptionResetAcknowledged: boolean;
  /**
   * The user's stored *intent* to see NSFW content — NOT permission to show it.
   *
   * `setAllowNsfwContent` is unconditional and this value is persisted, so a stored
   * `true` can outlive the age eligibility that permitted it (a `nsfwAuth` revocation,
   * a downgrading re-verification, or the transient loading window on launch).
   *
   * **Never render NSFW content on this field alone.** Any consumer must conjoin it with
   * live age eligibility — `useAgeStatus()`'s `validAge && nsfwAuth`, the same test
   * `NsfwContentGate` applies before it renders `checked`. Reading the raw boolean is
   * fail-OPEN, which is the wrong direction for an adult-content gate. Writes are guarded
   * at the gate component; reads are the caller's responsibility until a derived selector
   * exists (worth adding when NSFW-marked channels land and the second consumer appears).
   */
  allowNsfwContent: boolean;
  /**
   * The page zoom factor last handed to the zoom bridge, or `null` when none has
   * been (a shell without the bridge, a PiP window, or not yet applied). Runtime
   * only — excluded from persistence by `partialize`, because it describes this
   * window at its current width, not a preference. Read by the UI Scale slider
   * to say when the choice is being limited. Written only by `applyUiZoom`.
   */
  appliedUiZoom: number | null;
  setTheme: (theme: AppearanceSettings['theme']) => void;
  setColorScheme: (scheme: AppearanceSettings['colorScheme']) => void;
  setFontSize: (size: AppearanceSettings['fontSize']) => void;
  setCompactMode: (enabled: boolean) => void;
  setReduceAnimations: (enabled: boolean) => void;
  setUiScale: (value: number) => void;
  setHighContrast: (enabled: boolean) => void;
  setCustomColors: (colors: CustomColors) => void;
  setAppFont: (id: AppFontId) => void;
  setDyslexicSupport: (on: boolean) => void;
  setGifPlayback: (mode: GifPlaybackMode) => void;
  setFontMode: (mode: FontMode) => void;
  setFontHeadings: (id: AppFontId) => void;
  setFontNavigation: (id: AppFontId) => void;
  setFontMessages: (id: AppFontId) => void;
  setClientBehavior: (value: ClientBehavior) => void;
  setSubscriptionResetAcknowledged: (acknowledged: boolean) => void;
  setAllowNsfwContent: (allowed: boolean) => void;
}

const defaultAppearance: AppearanceSettings = {
  theme: 'dark',
  colorScheme: 'concord',
  fontSize: 'default',
  compactMode: false,
  reduceAnimations: false,
  uiScale: UI_SCALE_DEFAULT,
  highContrast: false,
  customColors: null,
  appFont: 'default',
  dyslexicSupport: false,
  gifPlayback: 'auto',
  fontMode: 'one',
  fontHeadings: 'default',
  fontNavigation: 'default',
  fontMessages: 'default',
};

/**
 * Stored custom colours reach documentElement.style.setProperty through
 * deriveThemeVariables. The colour picker validates them; storage is outside its reach,
 * so a palette with any non-#rrggbb field is dropped and the default scheme restored.
 */
function sanitizeCustomColors(appearance: AppearanceSettings): AppearanceSettings {
  const c: unknown = appearance.customColors;
  if (c === null || c === undefined) return appearance;
  const valid =
    typeof c === 'object' &&
    [
      (c as Partial<CustomColors>).background,
      (c as Partial<CustomColors>).accentPrimary,
      (c as Partial<CustomColors>).accentSecondary,
    ].every((v) => typeof v === 'string' && isValidHex(v));
  if (valid) return appearance;
  return {
    ...appearance,
    customColors: null,
    colorScheme:
      appearance.colorScheme === 'custom' ? defaultAppearance.colorScheme : appearance.colorScheme,
  };
}

function resolveTheme(theme: AppearanceSettings['theme']): 'dark' | 'light' {
  if (theme === 'system') {
    return globalThis.matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
  }
  return theme;
}

function applyTheme(theme: AppearanceSettings['theme']) {
  document.documentElement.dataset.theme = resolveTheme(theme);
}

function applyColorScheme(scheme: AppearanceSettings['colorScheme']) {
  if (scheme === 'custom') {
    // Clear data-scheme so no CSS scheme block applies; custom vars handled by subscriber
    delete document.documentElement.dataset.scheme;
  } else {
    // Clear any inline custom theme variables from a previous custom scheme
    clearCustomThemeVariables();
    document.documentElement.dataset.scheme = scheme;
  }
}

function applyFontSize(fontSize: AppearanceSettings['fontSize']) {
  document.documentElement.dataset.fontsize = fontSize;
}

function applyCompactMode(enabled: boolean) {
  document.documentElement.dataset.compact = enabled ? 'true' : 'false';
}

function applyReduceAnimations(enabled: boolean) {
  document.documentElement.dataset.reduceAnimations = enabled ? 'true' : 'false';
}

function applyUiScale(chosen: number) {
  const setZoom = getZoomBridge();
  // The CSS layer in index.css multiplies --sp-base and --font-scale by
  // --ui-scale via calc(). With the zoom bridge it is pinned to 1 — page zoom
  // already scales text, and doing both would scale it twice. Without it this
  // is the pre-#2367 engine, clamped to the legacy band.
  document.documentElement.style.setProperty('--ui-scale', String(cssUiScaleFor(chosen)));
  applyUiZoom(chosen, setZoom);
}

/**
 * Drive Electron page zoom from the chosen scale (#2367 part 2). Runs on every
 * uiScale change and on every window `resize`.
 *
 * INVARIANT: after this returns, the page's zoom factor equals
 * `appliedUiZoom`, and `appliedUiZoom` equals `computeAppliedZoom` for the
 * current chosen scale and unzoomed width — exactly when that value is an
 * endpoint (the chosen scale, or 1), otherwise to within `UI_ZOOM_EPSILON`. What
 * breaks it, and why each is handled:
 *  - A page whose zoom we do not know (first run in this document; a factor
 *    that may have survived a reload). `appliedUiZoom` starts `null`, and the
 *    first call is unconditional, so the page is brought to a known factor. If
 *    the width we measured was itself distorted by an unknown factor, the
 *    `resize` that zooming fires corrects it on the next pass.
 *  - Feedback. Zooming fires `resize`, which calls back in here. The change
 *    guard makes the no-loop property STRUCTURAL rather than arithmetic: the
 *    recompute is idempotent, but only the guard stops a second bridge call (see
 *    `UI_ZOOM_EPSILON` for why rounding noise always lands inside it).
 *  - A zero-width window (minimised, or not yet laid out). Its width says
 *    nothing about the layout, so the last factor is kept; the resize that
 *    restores it recomputes.
 *  - PiP windows share this module and this store, but must stay at 1x.
 */
function applyUiZoom(chosen: number, setZoom: SetZoomFactor | null): void {
  if (setZoom === null || isPipWindow()) return;
  const innerWidth = globalThis.innerWidth;
  if (!Number.isFinite(innerWidth) || innerWidth <= 0) return;
  const current = useSettingsStore.getState().appliedUiZoom;
  const next = computeAppliedZoom(chosen, innerWidth * (current ?? 1));
  // The epsilon absorbs only width-cap rounding noise. An exact endpoint — the
  // chosen scale, or 1 — always goes through: with the guard alone, a 1.59375
  // cap that became an uncapped 1.6 was a 0.006 step, so the page stayed short
  // of the choice for good and the hint read "Limited to 159%" on a wide screen.
  // Still loop-free: at an endpoint the zoom-induced resize recomputes that same
  // value (skipped as equal) or a cap within 0.0025 of it (skipped by epsilon).
  const isEndpoint = next === chosen || next === 1;
  if (
    current !== null &&
    (next === current || (!isEndpoint && Math.abs(next - current) < UI_ZOOM_EPSILON))
  ) {
    return;
  }
  // Record BEFORE calling: a resize delivered re-entrantly must see the factor
  // already in flight, or the change guard compares against a stale value. The
  // guard holds today only because Electron fires `resize` asynchronously.
  useSettingsStore.setState({ appliedUiZoom: next });
  setZoom(next);
}

/** The slice of live state storage may never overwrite on rehydration: every
 *  action, and the runtime-only `appliedUiZoom`. */
function runtimeOnlySettings(state: SettingsState): Partial<SettingsState> {
  const kept: Record<string, unknown> = { appliedUiZoom: state.appliedUiZoom };
  for (const [key, value] of Object.entries(state)) {
    if (typeof value === 'function') kept[key] = value;
  }
  return kept;
}

function applyHighContrast(enabled: boolean) {
  document.documentElement.dataset.highContrast = enabled ? 'true' : 'false';
}

// Single DOM sink for the resolved application fonts: one attribute per layer (#2366).
// The resolver (utils/ui/effectiveFont.ts) decides; this writes. `data-appfont` keeps its
// name because it IS the Interface layer and existing rules and tests key on it. Do not
// add another font write anywhere — dyslexic/theme/mode feed the resolver, not the DOM.
function applyFontLayers(layers: FontLayers) {
  const d = document.documentElement.dataset;
  d.appfont = layers.interface;
  d.fontHeadings = layers.headings;
  d.fontNav = layers.navigation;
  d.fontMessages = layers.messages;
  d.fontBrand = layers.brand;
}

/** Storage and sync are outside the setters' reach: an unknown id or mode falls back. */
function sanitizeFontSettings(a: AppearanceSettings): AppearanceSettings {
  const font = (v: unknown): AppFontId => (isAppFontId(v) ? v : APP_DEFAULT_FONT);
  return {
    ...a,
    appFont: font(a.appFont),
    fontHeadings: font(a.fontHeadings),
    fontNavigation: font(a.fontNavigation),
    fontMessages: font(a.fontMessages),
    fontMode: isFontMode(a.fontMode) ? a.fontMode : 'one',
  };
}

// v0 → v1 (#1099): the #1383 interim default {toTray:'none', toToolbar:'minimize'}
// was snapshotted into localStorage for any user who changed ANY setting while
// it was live (the whole store persists — `partialize` drops only the runtime
// `appliedUiZoom` — and merge spreads persisted over defaults). Map the EXACT interim-default combo back to the
// intended default; any other combo is a deliberate user choice and passes
// through untouched. Exported for unit tests.
export function migratePersistedSettings(persisted: unknown, version: number): unknown {
  const p = persisted as { clientBehavior?: ClientBehavior } | undefined;
  if (
    version < 1 &&
    p?.clientBehavior?.toTray === 'none' &&
    p.clientBehavior.toToolbar === 'minimize'
  ) {
    return { ...p, clientBehavior: { ...DEFAULT_CLIENT_BEHAVIOR } };
  }
  return persisted;
}

export const useSettingsStore = wrapStore(
  create<SettingsState>()(
    persist(
      subscribeWithSelector((set) => ({
        appearance: defaultAppearance,
        clientBehavior: DEFAULT_CLIENT_BEHAVIOR,
        subscriptionResetAcknowledged: false,
        allowNsfwContent: false,
        appliedUiZoom: null,

        setTheme: (theme) =>
          set((state) => ({
            appearance: { ...state.appearance, theme },
          })),

        setColorScheme: (colorScheme) =>
          set((state) => ({
            appearance: { ...state.appearance, colorScheme },
          })),

        setFontSize: (fontSize) =>
          set((state) => ({
            appearance: { ...state.appearance, fontSize },
          })),

        setCompactMode: (compactMode) =>
          set((state) => ({
            appearance: { ...state.appearance, compactMode },
          })),

        setReduceAnimations: (reduceAnimations) =>
          set((state) => ({
            appearance: { ...state.appearance, reduceAnimations },
          })),

        setUiScale: (uiScale) =>
          set((state) => ({
            appearance: { ...state.appearance, uiScale: clampUiScale(uiScale) },
          })),

        setHighContrast: (highContrast) =>
          set((state) => ({
            appearance: { ...state.appearance, highContrast },
          })),

        setCustomColors: (customColors) =>
          set((state) => ({
            appearance: { ...state.appearance, customColors, colorScheme: 'custom' as const },
          })),

        // LOAD-BEARING setter names: the draft layer write-throughs via
        // callSetter → set<Key> (draftSettingsStore.ts) and Revert restores by
        // iterating appearance keys → set<Key>. Renaming breaks live-preview/revert
        // (the regression documented at draftSettingsStore.ts restoreAppearanceFromSnapshot).
        setAppFont: (appFont) => set((state) => ({ appearance: { ...state.appearance, appFont } })),

        setDyslexicSupport: (dyslexicSupport) =>
          set((state) => ({ appearance: { ...state.appearance, dyslexicSupport } })),

        // Name is load-bearing: draftSettingsStore derives `set${Key}` at runtime,
        // so a mismatch fails live-preview/revert SILENTLY. See the note above setAppFont.
        setGifPlayback: (gifPlayback) =>
          set((state) => ({ appearance: { ...state.appearance, gifPlayback } })),

        // #2366 — names are load-bearing (draftSettingsStore derives set<Key>).
        setFontMode: (fontMode) =>
          set((state) => ({ appearance: { ...state.appearance, fontMode } })),
        setFontHeadings: (fontHeadings) =>
          set((state) => ({ appearance: { ...state.appearance, fontHeadings } })),
        setFontNavigation: (fontNavigation) =>
          set((state) => ({ appearance: { ...state.appearance, fontNavigation } })),
        setFontMessages: (fontMessages) =>
          set((state) => ({ appearance: { ...state.appearance, fontMessages } })),

        setSubscriptionResetAcknowledged: (subscriptionResetAcknowledged) =>
          set({ subscriptionResetAcknowledged }),

        setAllowNsfwContent: (allowNsfwContent) => set({ allowNsfwContent }),

        setClientBehavior: (value: ClientBehavior) => {
          set({ clientBehavior: value });
          // Push to main so the close/minimize intercepts see the new value.
          // Fire-and-forget — the main-side cache is best-effort; on a stale
          // value the worst case is one fallback click before the next renderer
          // mount re-pushes. .catch silences unhandled-rejection if the IPC
          // bridge is absent (e.g. in unit tests that mock partial electron).
          void globalThis.electron?.window?.setClientBehavior?.(value)?.catch?.(() => {});
        },
      })),
      {
        name: 'concord-settings',
        version: 1,
        // Cast: zustand's PersistOptions types migrate as returning the full
        // state, but the migrated value is the raw persisted PARTIAL — the
        // custom merge below shapes it over defaults (same reason merge
        // already casts `persisted as Partial<SettingsState>`).
        migrate: (persistedState, version) =>
          migratePersistedSettings(persistedState, version) as SettingsState,
        // `appliedUiZoom` is a fact about this window at its current width, not
        // a preference: persisting it would hand the next launch a stale factor
        // the applier then trusts as the page's zoom.
        partialize: ({ appliedUiZoom: _appliedUiZoom, ...rest }) => rest,
        merge: (persisted, current) => {
          const p = persisted as Partial<SettingsState> | undefined;
          const appearance = { ...defaultAppearance, ...p?.appearance };
          return {
            ...current,
            ...p,
            // Re-clamp on the way in: storage is outside the setter's reach, so
            // a hand-edited or corrupt value must not reach the apply path. A corrupt
            // font id used to count as an explicit pick and displace a theme-bundled
            // font, so font ids and the font mode are validated too (#2366).
            appearance: sanitizeFontSettings({
              ...sanitizeCustomColors(appearance),
              uiScale: clampUiScale(appearance.uiScale),
            }),
            clientBehavior: { ...DEFAULT_CLIENT_BEHAVIOR, ...p?.clientBehavior },
            // Default false when a pre-#1301 snapshot has no ack flag (the
            // `...p` spread already carries it forward when present).
            subscriptionResetAcknowledged: p?.subscriptionResetAcknowledged ?? false,
            allowNsfwContent: p?.allowNsfwContent ?? false,
            // Storage restores PREFERENCES only. `partialize` keeps runtime state
            // out of what is written, but the `...p` spread would carry a
            // hand-edited or corrupt key back in: a stored `appliedUiZoom` stands in
            // for the page's real zoom and suppresses the first, unconditional
            // apply, and a stored action key (`"setUiScale": 0`) replaces the setter
            // so the slider throws. Both always come from the live store.
            ...runtimeOnlySettings(current),
          };
        },
      }
    )
  )
);

// Subscribe to theme changes and apply to DOM
let systemThemeCleanup: (() => void) | null = null;

function pushOverlayColorsForTheme(theme: AppearanceSettings['theme']): void {
  // #806 Task 22: keep the per-platform titleBarOverlay color in sync with
  // the user's theme. Resolve 'system' to the OS-reported effective theme.
  // macOS ignores titleBarOverlay (uses native traffic lights) so the IPC
  // is a no-op there, but the push is unconditional — the main handler
  // tolerates the call. Fire-and-forget per [internal]rules/observability.md.
  const resolved = resolveTheme(theme);
  void globalThis.electron?.window
    ?.setTitleBarOverlayColor?.(deriveOverlayColors(resolved))
    ?.catch?.(() => {});
}

useSettingsStore.subscribe(
  (state) => state.appearance.theme,
  (theme) => {
    // Clean up previous system theme listener
    if (systemThemeCleanup) {
      systemThemeCleanup();
      systemThemeCleanup = null;
    }

    applyTheme(theme);
    pushOverlayColorsForTheme(theme);

    // If 'system', listen for OS theme changes
    if (theme === 'system') {
      const mediaQuery = globalThis.matchMedia('(prefers-color-scheme: dark)');
      const handler = () => {
        applyTheme('system');
        pushOverlayColorsForTheme('system');
      };
      mediaQuery.addEventListener('change', handler);
      systemThemeCleanup = () => mediaQuery.removeEventListener('change', handler);
    }
  },
  { fireImmediately: true }
);

// Subscribe to color scheme changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.colorScheme,
  (colorScheme) => applyColorScheme(colorScheme),
  { fireImmediately: true }
);

// Subscribe to font size changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.fontSize,
  (fontSize) => applyFontSize(fontSize),
  { fireImmediately: true }
);

// Subscribe to compact mode changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.compactMode,
  (compactMode) => applyCompactMode(compactMode),
  { fireImmediately: true }
);

// Subscribe to reduce animations changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.reduceAnimations,
  (reduceAnimations) => applyReduceAnimations(reduceAnimations),
  { fireImmediately: true }
);

// Subscribe to UI scale changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.uiScale,
  (uiScale) => applyUiScale(uiScale),
  { fireImmediately: true }
);

// Re-evaluate the width cap when the window resizes (#2367 part 2). Registered
// unconditionally and cheap when it has nothing to do: `applyUiZoom` returns at
// once without the zoom bridge or in a PiP window.
globalThis.addEventListener?.('resize', () => {
  applyUiZoom(useSettingsStore.getState().appearance.uiScale, getZoomBridge());
});

// Subscribe to high contrast changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.highContrast,
  (highContrast) => applyHighContrast(highContrast),
  { fireImmediately: true }
);

// Subscribe to every font input and apply the resolved layers through the single sink
// (#2366). The resolver owns precedence (Dyslexic > pick > theme-bundled > default, and
// the One Font / Font by Area split); `lockReason`/`pickerLocked` are read by the picker.
const FONT_INPUT_KEYS = [
  'appFont',
  'dyslexicSupport',
  'colorScheme',
  'fontMode',
  'fontHeadings',
  'fontNavigation',
  'fontMessages',
] as const;
useSettingsStore.subscribe(
  (state) => {
    const a = state.appearance;
    return {
      appFont: a.appFont,
      dyslexicSupport: a.dyslexicSupport,
      colorScheme: a.colorScheme,
      fontMode: a.fontMode,
      fontHeadings: a.fontHeadings,
      fontNavigation: a.fontNavigation,
      fontMessages: a.fontMessages,
    };
  },
  (inputs) =>
    applyFontLayers(
      resolveFontLayers(
        { ...inputs, themeBundledFont: themeBundledFontFor(inputs.colorScheme) },
        RESOLVER_CONFIG
      )
    ),
  {
    equalityFn: (a, b) => FONT_INPUT_KEYS.every((k) => a[k] === b[k]),
    fireImmediately: true,
  }
);

// Subscribe to custom colors + theme changes — derive and apply inline CSS variables
useSettingsStore.subscribe(
  (state) => ({
    colorScheme: state.appearance.colorScheme,
    customColors: state.appearance.customColors,
    theme: state.appearance.theme,
  }),
  ({ colorScheme, customColors, theme }) => {
    if (colorScheme === 'custom' && customColors) {
      const isDark = resolveTheme(theme) === 'dark';
      const vars = deriveThemeVariables(customColors, isDark);
      applyCustomThemeVariables(vars);
      // Cache derived vars for flash-free startup
      try {
        localStorage.setItem('concord-custom-theme-vars', JSON.stringify(vars));
      } catch {
        // Ignore storage errors
      }
    }
  },
  {
    equalityFn: (a, b) =>
      a.colorScheme === b.colorScheme &&
      a.theme === b.theme &&
      JSON.stringify(a.customColors) === JSON.stringify(b.customColors),
    fireImmediately: true,
  }
);

// Sync color scheme + theme mode changes to server profile (debounced, skips initial hydration)
let colorSyncTimer: ReturnType<typeof setTimeout> | null = null;
let colorSyncInitial = true;

/**
 * Build the color_scheme JSON payload, optimistically update memberStore,
 * and sync to server. Called from both the settings subscriber and
 * draftSettingsStore.apply() (since the subscriber is suppressed during draft mode).
 */
export function syncColorSchemeToServer() {
  const { colorScheme, customColors, theme } = useSettingsStore.getState().appearance;
  const themeMode = resolveTheme(theme);
  let payload: string;
  if (colorScheme === 'custom' && customColors) {
    payload = JSON.stringify({
      scheme: 'custom',
      themeMode,
      accentPrimary: customColors.accentPrimary,
      accentSecondary: customColors.accentSecondary,
    });
  } else {
    payload = JSON.stringify({ scheme: colorScheme, themeMode });
  }

  // Optimistically update memberStore so identity components reflect the
  // new scheme immediately (without waiting for the server roundtrip)
  const selfId = useUserStore.getState().user?.id;
  if (selfId) {
    useMemberStore.getState().updateMemberProfile(selfId, { color_scheme: payload });
  }

  // Server sync
  useUserStore
    .getState()
    .updateProfile({ color_scheme: payload })
    .catch(() => {
      // Fire-and-forget — local settings still work if server sync fails
    });
}

useSettingsStore.subscribe(
  (state) => ({
    colorScheme: state.appearance.colorScheme,
    customColors: state.appearance.customColors,
    theme: state.appearance.theme,
  }),
  () => {
    // Skip the initial hydration from localStorage
    if (colorSyncInitial) {
      colorSyncInitial = false;
      return;
    }

    // Suppress server sync during draft mode — sync happens on Apply
    if (isSyncSuppressed()) return;

    // Debounced sync
    if (colorSyncTimer) clearTimeout(colorSyncTimer);
    colorSyncTimer = setTimeout(syncColorSchemeToServer, 500);
  },
  {
    equalityFn: (a, b) =>
      a.colorScheme === b.colorScheme &&
      a.theme === b.theme &&
      JSON.stringify(a.customColors) === JSON.stringify(b.customColors),
    fireImmediately: true,
  }
);
