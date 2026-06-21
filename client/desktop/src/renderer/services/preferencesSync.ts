/**
 * Preferences Sync Service — encrypted cross-device sync for UI settings and layout.
 *
 * Watches settingsStore and layoutStore for changes, encrypts via e2eeService,
 * and pushes to the server. On login (or WebSocket notification from another device),
 * fetches + decrypts + applies remote preferences to local stores.
 *
 * Dependencies (settingsStore, layoutStore) are injected via .init() to break the
 * cyclic import: settingsStore → userStore → preferencesSync → settingsStore.
 */

import { e2eeService } from './e2eeService';
import { apiFetch } from './apiClient';
import { errorMessage } from '../utils/redactError';
import type { AppearanceSettings } from '../stores/settingsStore';
import type { MemberPanelMode, ServerFolder } from '../stores/layoutStore';

const DEBOUNCE_MS = 3000;

export interface LayoutPersistedState {
  channelPanelPinned: boolean;
  channelPanelWidth: number;
  memberPanelMode: MemberPanelMode;
  memberPanelWidth: number;
  serverBarHeight: number;
  folderBarHeight: number;
  serverFolders: ServerFolder[];
  serverOrder: string[];
}

export interface PreferencesSyncDeps {
  getAppearance: () => AppearanceSettings;
  setAppearance: (patch: AppearanceSettings) => void;
  getLayout: () => LayoutPersistedState;
  setLayout: (patch: Partial<LayoutPersistedState>) => void;
}

interface PreferencesBlob {
  v: 1;
  settings: AppearanceSettings;
  layout: LayoutPersistedState;
}

/** Diff and apply remote appearance settings to the local settings store. */
function applyRemoteAppearance(remote: AppearanceSettings, deps: PreferencesSyncDeps): void {
  const current = deps.getAppearance();
  const patched = { ...current };
  let changed = false;

  if (remote.theme !== current.theme) {
    patched.theme = remote.theme;
    changed = true;
  }
  if (remote.fontSize !== current.fontSize) {
    patched.fontSize = remote.fontSize;
    changed = true;
  }
  if (remote.compactMode !== current.compactMode) {
    patched.compactMode = remote.compactMode;
    changed = true;
  }

  // Color scheme: handle 'custom' with customColors specially
  if (remote.colorScheme === 'custom' && remote.customColors) {
    if (JSON.stringify(remote.customColors) !== JSON.stringify(current.customColors)) {
      patched.customColors = remote.customColors;
      patched.colorScheme = 'custom';
      changed = true;
    } else if (remote.colorScheme !== current.colorScheme) {
      patched.colorScheme = remote.colorScheme;
      changed = true;
    }
  } else if (remote.colorScheme !== current.colorScheme) {
    patched.colorScheme = remote.colorScheme;
    changed = true;
  }

  if (changed) {
    deps.setAppearance(patched);
  }
}

/** Diff and apply remote layout preferences to the local layout store. */
function applyRemoteLayout(remote: PreferencesBlob['layout'], deps: PreferencesSyncDeps): void {
  const current = deps.getLayout();
  const patch: Record<string, unknown> = {};

  // Scalar fields with min/max clamping
  const clampedFields: { key: keyof typeof remote; min: number; max: number }[] = [
    { key: 'channelPanelWidth', min: 180, max: 400 },
    { key: 'memberPanelWidth', min: 160, max: 340 },
    { key: 'serverBarHeight', min: 36, max: 64 },
    { key: 'folderBarHeight', min: 24, max: 48 },
  ];
  for (const { key, min, max } of clampedFields) {
    if (remote[key] !== current[key]) {
      patch[key] = Math.max(min, Math.min(max, remote[key] as number));
    }
  }

  // Boolean / enum fields
  if (remote.channelPanelPinned !== current.channelPanelPinned) {
    patch.channelPanelPinned = remote.channelPanelPinned;
  }
  if (remote.memberPanelMode !== current.memberPanelMode) {
    patch.memberPanelMode = remote.memberPanelMode;
  }

  // Deep-compared fields
  if (JSON.stringify(remote.serverFolders) !== JSON.stringify(current.serverFolders)) {
    patch.serverFolders = remote.serverFolders;
  }
  if (JSON.stringify(remote.serverOrder) !== JSON.stringify(current.serverOrder)) {
    patch.serverOrder = remote.serverOrder;
  }

  if (Object.keys(patch).length > 0) {
    deps.setLayout(patch);
  }
}

class PreferencesSyncService {
  // Bounded retry for a startup 401. A transient auth-bootstrap race can 401 the
  // very first preferences fetch even though the token is valid (proven by the
  // WS connecting moments later on the same token). fetchAndApply has no other
  // re-trigger for a single-device user — the only one is a cross-device
  // 'preferences_updated' WS push — so without this a transient 401 silently
  // skips the remote-preference PULL for the whole session. The precipitating
  // 401's exact server-side cause is unpinned; the bounded retry is the net.
  private static readonly MAX_AUTH_RETRIES = 2;
  private static readonly AUTH_RETRY_DELAY_MS = 1500;
  private authRetryTimer: ReturnType<typeof setTimeout> | null = null;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private unsubscribers: (() => void)[] = [];
  private isApplyingRemote = false;
  private deps: PreferencesSyncDeps | null = null;
  private watchGeneration = 0;

  init(deps: PreferencesSyncDeps): void {
    if (this.deps) return; // idempotent for HMR
    this.deps = deps;
  }

  private requireDeps(): PreferencesSyncDeps {
    if (!this.deps) {
      throw new Error(
        'PreferencesSyncService.init() must be called before use — see App.tsx bootstrap'
      );
    }
    return this.deps;
  }

  /**
   * Fetch preferences from server, decrypt, and apply to local stores.
   * If the server has no preferences yet, pushes current local state as initial sync.
   */
  async fetchAndApply(attempt = 0): Promise<void> {
    if (!e2eeService.isInitialized) return;

    // A fresh fetch supersedes any pending bounded auth-retry — clear it so a
    // stale retry can't fire after a newer trigger (e.g. a cross-device
    // 'preferences_updated' WS push) and re-apply older remote state. Only the
    // in-flight retry chain should own the timer; on the retry path the timer is
    // already null (cleared in scheduleAuthRetry's callback), so this is a no-op.
    if (this.authRetryTimer) {
      clearTimeout(this.authRetryTimer);
      this.authRetryTimer = null;
    }

    const deps = this.requireDeps();

    try {
      const res = await apiFetch('/api/v1/users/me/preferences');
      if (!res.ok) {
        if (res.status === 401 && attempt < PreferencesSyncService.MAX_AUTH_RETRIES) {
          // Transient startup auth race — retry a bounded number of times after
          // a short delay rather than abandoning the session's remote-pref sync.
          // Constant format string (CWE-134); dynamic parts are separate args.
          console.warn(
            '[PrefsSync] preferences fetch 401 (auth not settled?); retrying',
            attempt + 1,
            'of',
            PreferencesSyncService.MAX_AUTH_RETRIES
          );
          this.scheduleAuthRetry(attempt + 1);
          return;
        }
        console.warn('[PrefsSync] Failed to fetch preferences:', res.status);
        return;
      }

      const data = await res.json();

      if (!data.preferences) {
        // First login — push local state as bootstrap
        console.debug('[PrefsSync] No server preferences, pushing local state');
        await this.pushPreferences();
        return;
      }

      let blob: PreferencesBlob;
      try {
        blob = await e2eeService.decryptPreferences<PreferencesBlob>(
          data.preferences.encrypted_data
        );
      } catch {
        // Preferences encrypted with a different key (e.g., after PBKDF2→Argon2id migration).
        // Push current local state to overwrite stale server data.
        console.warn(
          '[PrefsSync] Cannot decrypt server preferences, re-encrypting with current key'
        );
        await this.pushPreferences();
        return;
      }

      if (blob.v !== 1) {
        console.warn('[PrefsSync] Unknown preferences version:', blob.v);
        return;
      }

      // Apply remote preferences to stores with echo guard.
      // Batch all updates into single setState calls to avoid cascading re-renders.
      this.isApplyingRemote = true;
      try {
        applyRemoteAppearance(blob.settings, deps);
        applyRemoteLayout(blob.layout, deps);
      } finally {
        // Clear after microtask so synchronous subscription callbacks see the flag
        setTimeout(() => {
          this.isApplyingRemote = false;
        }, 0);
      }

      console.debug('[PrefsSync] Applied remote preferences v' + data.preferences.version);
    } catch (err) {
      console.warn('[PrefsSync] Failed to fetch/apply preferences:', errorMessage(err));
    }
  }

  /**
   * Schedule a single bounded retry of fetchAndApply after AUTH_RETRY_DELAY_MS.
   * Replaces any pending retry so retries never stack; cleared by stopWatching.
   */
  private scheduleAuthRetry(nextAttempt: number): void {
    if (this.authRetryTimer) clearTimeout(this.authRetryTimer);
    this.authRetryTimer = setTimeout(() => {
      this.authRetryTimer = null;
      void this.fetchAndApply(nextAttempt);
    }, PreferencesSyncService.AUTH_RETRY_DELAY_MS);
  }

  /**
   * Start watching local stores for changes and schedule debounced pushes.
   */
  startWatching(): void {
    this.stopWatching();

    this.requireDeps(); // throws if init() was not called

    // Watch settings store — poll-based via subscribe selector
    // We use a Zustand-compatible subscribe pattern via the deps layer.
    // Since deps abstracts the store, we schedule pushes whenever deps
    // report changes. The actual Zustand subscription is wired by the
    // caller (see App.tsx bootstrap) — here we just need to ensure the
    // stores are importable. For backward compatibility with existing
    // callers that expect startWatching to set up Zustand subscriptions,
    // we dynamically import the stores.

    // Dynamic import to avoid the cyclic dependency at module load time.
    // These are lazy — they resolve from the already-loaded module registry.
    // Capture the generation so stale callbacks (from a stop/restart race) are discarded.
    const gen = this.watchGeneration;

    import('../stores/settingsStore')
      .then(({ useSettingsStore }) => {
        if (gen !== this.watchGeneration) return; // stale — service was stopped/restarted
        const unsubSettings = useSettingsStore.subscribe(
          (state) => state.appearance,
          () => this.schedulePush(),
          { equalityFn: (a, b) => JSON.stringify(a) === JSON.stringify(b) }
        );
        this.unsubscribers.push(unsubSettings);
      })
      .catch(() => {
        /* module already loaded — should not fail */
      });

    import('../stores/layoutStore')
      .then(({ useLayoutStore }) => {
        if (gen !== this.watchGeneration) return; // stale — service was stopped/restarted
        const unsubLayout = useLayoutStore.subscribe(
          (state) => ({
            channelPanelPinned: state.channelPanelPinned,
            channelPanelWidth: state.channelPanelWidth,
            memberPanelMode: state.memberPanelMode,
            memberPanelWidth: state.memberPanelWidth,
            serverBarHeight: state.serverBarHeight,
            folderBarHeight: state.folderBarHeight,
            serverFolders: state.serverFolders,
            serverOrder: state.serverOrder,
          }),
          () => this.schedulePush(),
          { equalityFn: (a, b) => JSON.stringify(a) === JSON.stringify(b) }
        );
        this.unsubscribers.push(unsubLayout);
      })
      .catch(() => {
        /* module already loaded — should not fail */
      });
  }

  /**
   * Stop watching stores and clear pending debounce.
   */
  stopWatching(): void {
    this.watchGeneration++;
    for (const unsub of this.unsubscribers) {
      unsub();
    }
    this.unsubscribers = [];
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    if (this.authRetryTimer) {
      clearTimeout(this.authRetryTimer);
      this.authRetryTimer = null;
    }
  }

  /**
   * Collect current state, encrypt, and push to server.
   */
  async pushPreferences(): Promise<void> {
    if (!e2eeService.isInitialized) return;

    const deps = this.requireDeps();

    try {
      const settings = deps.getAppearance();
      const layout = deps.getLayout();

      const blob: PreferencesBlob = {
        v: 1,
        settings: {
          theme: settings.theme,
          colorScheme: settings.colorScheme,
          fontSize: settings.fontSize,
          compactMode: settings.compactMode,
          reduceAnimations: settings.reduceAnimations,
          uiScale: settings.uiScale,
          highContrast: settings.highContrast,
          customColors: settings.customColors,
          // appFont/dyslexicSupport ride in the encrypted blob (the type requires the
          // full AppearanceSettings) but are NOT yet applied cross-device on pull —
          // matching the existing applyRemoteAppearance omission of uiScale/highContrast/
          // reduceAnimations. Active cross-device font sync is a follow-up (#1642 deferred).
          appFont: settings.appFont,
          dyslexicSupport: settings.dyslexicSupport,
        },
        layout: {
          channelPanelPinned: layout.channelPanelPinned,
          channelPanelWidth: layout.channelPanelWidth,
          memberPanelMode: layout.memberPanelMode,
          memberPanelWidth: layout.memberPanelWidth,
          serverBarHeight: layout.serverBarHeight,
          folderBarHeight: layout.folderBarHeight,
          serverFolders: layout.serverFolders,
          serverOrder: layout.serverOrder,
        },
      };

      const encrypted = await e2eeService.encryptPreferences(blob);

      const res = await apiFetch('/api/v1/users/me/preferences', {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ encrypted_data: encrypted }),
      });

      if (res.ok) {
        const data = await res.json();
        console.debug('[PrefsSync] Pushed preferences v' + data.version);
      } else {
        console.warn('[PrefsSync] Push failed:', res.status);
      }
    } catch (err) {
      console.warn('[PrefsSync] Push error:', errorMessage(err));
    }
  }

  private schedulePush(): void {
    if (this.isApplyingRemote) return;

    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      this.pushPreferences();
    }, DEBOUNCE_MS);
  }
}

// Singleton
export const preferencesSyncService = new PreferencesSyncService();
