import { create } from 'zustand';
import { persist, subscribeWithSelector } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import {
  type CustomColors,
  deriveThemeVariables,
  applyCustomThemeVariables,
  clearCustomThemeVariables,
} from '../utils/colorUtils';
import { DEFAULT_CLIENT_BEHAVIOR, type ClientBehavior } from '../../shared/clientBehavior';
import { deriveOverlayColors } from '../utils/overlayColors';
import { useUserStore } from './userStore';
import { useMemberStore } from './memberStore';
// Sync-suppression flag lives in a dependency-free leaf module so that
// userStore can flip it via a STATIC import without a circular dependency or a
// teardown-racing dynamic import. See colorSyncSuppression.ts.
import { isSyncSuppressed } from './colorSyncSuppression';
import {
  type AppFontId,
  resolveEffectiveFont,
  themeBundledFontFor,
  RESOLVER_CONFIG,
} from '../utils/effectiveFont';

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
   * Continuous UI scale multiplier. Coexists with `fontSize` (discrete) —
   * uiScale compounds into `--ui-scale` and is multiplied by `--sp-base`
   * + `--font-scale` so the two controls stack rather than override each
   * other. Clamped to [0.85, 1.5]; default 1.0 is a no-op.
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
}

/** Lower + upper bound for uiScale; defaults match the slider range.
 *
 * Max is capped at 1.30 (not the original 1.50) so users can't push the UI
 * into a layout-breaking zone — the chat preview tile and a few other
 * narrow surfaces start collapsing past ~1.20 even with a responsive grid.
 * Layout robustness is the right long-term answer; until every surface is
 * known-safe at 1.50, the cap protects the user from a usability cliff.
 * Font Size's discrete "Large" still compounds on top, so the practical
 * upper limit is 1.175 × 1.30 ≈ 1.53 — comparable to the original cap. */
export const UI_SCALE_MIN = 0.85;
export const UI_SCALE_MAX = 1.3;
export const UI_SCALE_DEFAULT = 1;

/** Clamp + sanity-check uiScale on the way in (slider, persisted state). */
export function clampUiScale(value: number): number {
  if (!Number.isFinite(value)) return UI_SCALE_DEFAULT;
  return Math.min(UI_SCALE_MAX, Math.max(UI_SCALE_MIN, value));
}

export type { CustomColors } from '../utils/colorUtils';

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
  setClientBehavior: (value: ClientBehavior) => void;
  setSubscriptionResetAcknowledged: (acknowledged: boolean) => void;
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
};

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

function applyUiScale(value: number) {
  // Apply via custom property so existing var(--sp-base, ...) and
  // var(--font-scale, ...) consumers compound with it without code change.
  // The CSS layer in index.css multiplies --sp-base and --font-scale by
  // --ui-scale via calc().
  document.documentElement.style.setProperty('--ui-scale', String(clampUiScale(value)));
}

function applyHighContrast(enabled: boolean) {
  document.documentElement.dataset.highContrast = enabled ? 'true' : 'false';
}

// Single DOM sink for the resolved application font (see utils/effectiveFont.ts).
// The resolver decides which font wins; this just writes the one attribute CSS
// keys on (`[data-appfont='…']`). Do NOT add a second sink for dyslexic/theme —
// they feed the resolver, not the DOM.
function applyEffectiveFont(id: AppFontId) {
  document.documentElement.dataset.appfont = id;
}

// v0 → v1 (#1099): the #1383 interim default {toTray:'none', toToolbar:'minimize'}
// was snapshotted into localStorage for any user who changed ANY setting while
// it was live (no partialize — the whole store persists, and merge spreads
// persisted over defaults). Map the EXACT interim-default combo back to the
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

        setSubscriptionResetAcknowledged: (subscriptionResetAcknowledged) =>
          set({ subscriptionResetAcknowledged }),

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
        merge: (persisted, current) => {
          const p = persisted as Partial<SettingsState> | undefined;
          return {
            ...current,
            ...p,
            appearance: { ...defaultAppearance, ...p?.appearance },
            clientBehavior: { ...DEFAULT_CLIENT_BEHAVIOR, ...p?.clientBehavior },
            // Default false when a pre-#1301 snapshot has no ack flag (the
            // `...p` spread already carries it forward when present).
            subscriptionResetAcknowledged: p?.subscriptionResetAcknowledged ?? false,
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

// Subscribe to high contrast changes and apply to DOM
useSettingsStore.subscribe(
  (state) => state.appearance.highContrast,
  (highContrast) => applyHighContrast(highContrast),
  { fireImmediately: true }
);

// Subscribe to the effective-font inputs (appFont, dyslexicSupport, colorScheme)
// and apply the resolved font as the single `data-appfont` attribute. This is the
// ONLY DOM write for fonts — the resolver (utils/effectiveFont.ts) owns precedence
// (Dyslexic > user pick > theme-bundled > default). In C1 `themeBundledFontFor`
// returns null; #1643 fills it. `lockReason`/`pickerLocked` are read by the picker.
useSettingsStore.subscribe(
  (state) => ({
    appFont: state.appearance.appFont,
    dyslexicSupport: state.appearance.dyslexicSupport,
    colorScheme: state.appearance.colorScheme,
  }),
  ({ appFont, dyslexicSupport, colorScheme }) => {
    const { effective } = resolveEffectiveFont(
      { dyslexicSupport, appFont, themeBundledFont: themeBundledFontFor(colorScheme) },
      RESOLVER_CONFIG
    );
    applyEffectiveFont(effective);
  },
  {
    equalityFn: (a, b) =>
      a.appFont === b.appFont &&
      a.dyslexicSupport === b.dyslexicSupport &&
      a.colorScheme === b.colorScheme,
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
