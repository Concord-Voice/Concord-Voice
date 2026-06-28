import { createStore } from '../utils/createStore';
import {
  useSettingsStore,
  syncColorSchemeToServer,
  type AppearanceSettings,
  type CustomColors,
} from './settingsStore';
import { setSyncSuppressed } from './colorSyncSuppression';
import { useAudioSettingsStore, type AudioSettings } from './audioSettingsStore';
import { useVideoSettingsStore, type VideoSettings } from './videoSettingsStore';
import { useTTSSettingsStore, type TTSSettings } from './ttsSettingsStore';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from './voiceStore';

// ---------------------------------------------------------------------------
// Draftable types — exclude UI-only toggles and system-detected fields
// ---------------------------------------------------------------------------

export type DraftableAudioSettings = Omit<AudioSettings, 'advancedMode'>;
export type DraftableVideoSettings = Omit<
  VideoSettings,
  'videoAdvancedMode' | 'codecCapabilities' | 'gpuInfo' | 'systemHdr'
>;
export type DraftableTTSSettings = TTSSettings;
export type DraftableAppearanceSettings = AppearanceSettings;

// ---------------------------------------------------------------------------
// Draftable key lists (used for snapshotting)
// ---------------------------------------------------------------------------

const AUDIO_DRAFTABLE_KEYS: (keyof DraftableAudioSettings)[] = [
  'noiseCancellation',
  'echoCancellation',
  'autoGainControl',
  'noiseGateMode',
  'noiseGateLevel',
  'musicMode',
  'frameSize',
  'silenceDetection',
  'stereoOverride',
  'inlineFec',
  'fecHeadroom',
  'opusNack',
  'adaptivePtime',
  'audioPriority',
  'inputVolume',
  'outputVolume',
  'quietBoost',
  'quietBoostThreshold',
  'networkType',
  'packetLossWarningThreshold',
];

const VIDEO_DRAFTABLE_KEYS: (keyof DraftableVideoSettings)[] = [
  'cameraPreset',
  'preferredVideoCodec',
  'cameraPriority',
  'screenResolution',
  'screenFrameRate',
  'screenContentType',
  'screenSharePriority',
  'screenShareBitrate',
  'degradationPreference',
  'scalabilityMode',
  'hardwareAcceleration',
  'hdrEncoding',
  'supportSvc',
  'supportSimulcast',
];

const TTS_DRAFTABLE_KEYS: (keyof DraftableTTSSettings)[] = [
  'ttsEnabled',
  'ttsSendEnabled',
  'ttsVoice',
  'ttsRate',
  'ttsVolume',
];

// Audio fields that differ between Basic and Advanced modes (tier-controlled in Basic)
const AUDIO_MODE_KEYS: (keyof DraftableAudioSettings)[] = [
  'silenceDetection',
  'inlineFec',
  'fecHeadroom',
  'frameSize',
  'stereoOverride',
];

// ---------------------------------------------------------------------------
// Snapshot type
// ---------------------------------------------------------------------------

interface SettingsSnapshot {
  appearance: AppearanceSettings;
  audio: DraftableAudioSettings;
  video: DraftableVideoSettings;
  tts: DraftableTTSSettings;
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

function deepEqual(a: unknown, b: unknown): boolean {
  return JSON.stringify(a) === JSON.stringify(b);
}

function pickKeys<T extends object>(source: T, keys: (keyof T)[]): Partial<T> {
  const result: Partial<T> = {};
  for (const k of keys) {
    result[k] = source[k];
  }
  return result;
}

/**
 * Merge stashed values back into drafts for mode-dependent keys.
 * If a stashed value matches the snapshot, remove the draft (no change needed).
 * Otherwise, set the draft to the stashed value.
 */
function restoreStashIntoDrafts(
  newDrafts: Partial<DraftableAudioSettings>,
  stash: Partial<DraftableAudioSettings>,
  snapshot: DraftableAudioSettings
): void {
  for (const k of AUDIO_MODE_KEYS) {
    if (!(k in stash)) continue;
    if (deepEqual(stash[k], snapshot[k])) {
      delete newDrafts[k];
    } else {
      (newDrafts as Record<string, unknown>)[k] = stash[k];
    }
  }
}

/**
 * Apply tier defaults into drafts when no basic stash exists.
 */
function applyTierDefaultsToDrafts(
  newDrafts: Partial<DraftableAudioSettings>,
  qualityTier: AudioQualityTier,
  snapshot: DraftableAudioSettings
): void {
  const tc = AUDIO_QUALITY_TIERS[qualityTier];
  const tierDefaults: Partial<DraftableAudioSettings> = {
    silenceDetection: tc.opusDtx,
    inlineFec: tc.opusFec,
    fecHeadroom: tc.opusFec,
    frameSize: 0,
    stereoOverride: null,
  };
  for (const [k, v] of Object.entries(tierDefaults)) {
    const key = k as keyof DraftableAudioSettings;
    if (deepEqual(v, snapshot[key])) {
      delete newDrafts[key];
    } else {
      (newDrafts as Record<string, unknown>)[k] = v;
    }
  }
}

/** Restore appearance settings from a snapshot back to the real store.
 *
 * Iterates over snapshot.appearance keys and dispatches each to the matching
 * `set<Key>` setter via callSetter, rather than hand-listing each setter
 * — that previous shape silently regressed every time a new appearance
 * setting was added (originally missed `highContrast` and `uiScale`,
 * causing Revert to wipe the draft state without undoing the live-preview
 * write-through, leaving the DOM in the modified state).
 *
 * Guard: customColors=null means "no custom theme defined." Calling
 * setCustomColors(null) would push the colorScheme to 'custom' as a side
 * effect (see useSettingsStore.setCustomColors), which is wrong — skip the
 * null case explicitly to match the original snapshot semantics. */
function restoreAppearanceFromSnapshot(snapshot: SettingsSnapshot): void {
  const store = useSettingsStore.getState() as unknown as Record<string, unknown>;
  for (const [key, value] of Object.entries(snapshot.appearance)) {
    if (key === 'customColors' && value === null) continue;
    callSetter(store, key, value);
  }
}

/** Push draft entries to a real store via setter functions. */
function pushDraftsToStore(
  store: Record<string, unknown>,
  drafts: Record<string, unknown>,
  skipKeys?: Set<string>
): void {
  for (const [key, value] of Object.entries(drafts)) {
    if (skipKeys?.has(key)) continue;
    callSetter(store, key, value);
  }
}

function takeSnapshot(): SettingsSnapshot {
  const appearance = structuredClone(useSettingsStore.getState().appearance);
  const audioState = useAudioSettingsStore.getState();
  const videoState = useVideoSettingsStore.getState();
  const ttsState = useTTSSettingsStore.getState();

  return {
    appearance,
    audio: pickKeys(audioState, AUDIO_DRAFTABLE_KEYS) as DraftableAudioSettings,
    video: pickKeys(videoState, VIDEO_DRAFTABLE_KEYS) as DraftableVideoSettings,
    tts: pickKeys(ttsState, TTS_DRAFTABLE_KEYS) as DraftableTTSSettings,
  };
}

/** Derive a setter name from a field name: 'inputVolume' → 'setInputVolume' */
function setterName(field: string): string {
  return `set${field.charAt(0).toUpperCase()}${field.slice(1)}`;
}

/** Call a store's setter by field name */
function callSetter(store: Record<string, unknown>, field: string, value: unknown) {
  const name = setterName(field);
  if (typeof store[name] === 'function') {
    (store[name] as (v: unknown) => void)(value);
  }
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

interface DraftOverlays {
  appearance: Partial<DraftableAppearanceSettings>;
  audio: Partial<DraftableAudioSettings>;
  video: Partial<DraftableVideoSettings>;
  tts: Partial<DraftableTTSSettings>;
}

const emptyDrafts = (): DraftOverlays => ({
  appearance: {},
  audio: {},
  video: {},
  tts: {},
});

interface DraftSettingsState {
  snapshot: SettingsSnapshot | null;
  drafts: DraftOverlays;

  // Mode stashes — saved draft values for the inactive audio mode
  audioBasicStash: Partial<DraftableAudioSettings> | null;
  audioAdvancedStash: Partial<DraftableAudioSettings> | null;

  // Lifecycle
  initialize: () => void;
  teardown: () => void;

  // Draft setters
  setAppearanceDraft: <K extends keyof DraftableAppearanceSettings>(
    key: K,
    value: DraftableAppearanceSettings[K]
  ) => void;
  setAudioDraft: <K extends keyof DraftableAudioSettings>(
    key: K,
    value: DraftableAudioSettings[K]
  ) => void;
  setVideoDraft: <K extends keyof DraftableVideoSettings>(
    key: K,
    value: DraftableVideoSettings[K]
  ) => void;
  setTtsDraft: <K extends keyof DraftableTTSSettings>(
    key: K,
    value: DraftableTTSSettings[K]
  ) => void;

  // Batch setter (for tier slider which sets 5 fields at once)
  batchSetAudioDrafts: (updates: Partial<DraftableAudioSettings>) => void;

  // Mode switch
  stashAndSwapAudioMode: (toAdvanced: boolean, qualityTier: AudioQualityTier) => void;

  // Actions
  apply: () => void;
  revert: () => void;
}

export const useDraftSettingsStore = createStore<DraftSettingsState>()((set, get) => ({
  snapshot: null,
  drafts: emptyDrafts(),
  audioBasicStash: null,
  audioAdvancedStash: null,

  // ── Lifecycle ────────────────────────────────────────────────────────────

  initialize: () => {
    setSyncSuppressed(true);
    set({
      snapshot: takeSnapshot(),
      drafts: emptyDrafts(),
      audioBasicStash: null,
      audioAdvancedStash: null,
    });
  },

  teardown: () => {
    // Restore appearance from snapshot if there are pending appearance drafts
    const { snapshot, drafts } = get();
    if (snapshot && Object.keys(drafts.appearance).length > 0) {
      restoreAppearanceFromSnapshot(snapshot);
    }
    setSyncSuppressed(false);
    set({
      snapshot: null,
      drafts: emptyDrafts(),
      audioBasicStash: null,
      audioAdvancedStash: null,
    });
  },

  // ── Draft setters ────────────────────────────────────────────────────────

  setAppearanceDraft: (key, value) => {
    // 1. Update draft store (smart change detection)
    set((state) => {
      if (!state.snapshot) return state;
      const snapshotValue = state.snapshot.appearance[key];
      const newDrafts = { ...state.drafts.appearance };

      if (deepEqual(value, snapshotValue)) {
        delete newDrafts[key];
      } else {
        (newDrafts as Record<string, unknown>)[key as string] = value;
      }

      return { drafts: { ...state.drafts, appearance: newDrafts } };
    });

    // 2. Write through to real store for live DOM preview
    // Special case: customColors setter also forces colorScheme:'custom'
    if (key === 'customColors') {
      useSettingsStore.getState().setCustomColors(value as CustomColors);
    } else {
      callSetter(
        useSettingsStore.getState() as unknown as Record<string, unknown>,
        key as string,
        value
      );
    }
  },

  setAudioDraft: (key, value) =>
    set((state) => {
      if (!state.snapshot) return state;
      const snapshotValue = state.snapshot.audio[key];
      const newDrafts = { ...state.drafts.audio };

      if (deepEqual(value, snapshotValue)) {
        delete newDrafts[key];
      } else {
        (newDrafts as Record<string, unknown>)[key as string] = value;
      }

      return { drafts: { ...state.drafts, audio: newDrafts } };
    }),

  setVideoDraft: (key, value) =>
    set((state) => {
      if (!state.snapshot) return state;
      const snapshotValue = state.snapshot.video[key];
      const newDrafts = { ...state.drafts.video };

      if (deepEqual(value, snapshotValue)) {
        delete newDrafts[key];
      } else {
        (newDrafts as Record<string, unknown>)[key as string] = value;
      }

      return { drafts: { ...state.drafts, video: newDrafts } };
    }),

  setTtsDraft: (key, value) =>
    set((state) => {
      if (!state.snapshot) return state;
      const snapshotValue = state.snapshot.tts[key];
      const newDrafts = { ...state.drafts.tts };

      if (deepEqual(value, snapshotValue)) {
        delete newDrafts[key];
      } else {
        (newDrafts as Record<string, unknown>)[key as string] = value;
      }

      return { drafts: { ...state.drafts, tts: newDrafts } };
    }),

  batchSetAudioDrafts: (updates) =>
    set((state) => {
      if (!state.snapshot) return state;
      const newDrafts = { ...state.drafts.audio };

      for (const [key, value] of Object.entries(updates)) {
        const k = key as keyof DraftableAudioSettings;
        if (deepEqual(value, state.snapshot.audio[k])) {
          delete newDrafts[k];
        } else {
          (newDrafts as Record<string, unknown>)[key] = value;
        }
      }

      return { drafts: { ...state.drafts, audio: newDrafts } };
    }),

  // ── Mode stashing ───────────────────────────────────────────────────────

  stashAndSwapAudioMode: (toAdvanced, qualityTier) =>
    set((state) => {
      if (!state.snapshot) return state;

      const currentDrafts = state.drafts.audio;
      const snapshot = state.snapshot.audio;

      // Pick current effective values for mode-dependent keys
      const currentModeValues: Partial<DraftableAudioSettings> = {};
      for (const k of AUDIO_MODE_KEYS) {
        currentModeValues[k] = (k in currentDrafts ? currentDrafts[k] : snapshot[k]) as never;
      }

      const newDrafts = { ...currentDrafts };

      if (toAdvanced) {
        // Basic → Advanced: stash basic, restore advanced
        if (state.audioAdvancedStash) {
          restoreStashIntoDrafts(newDrafts, state.audioAdvancedStash, snapshot);
        }
        return {
          audioBasicStash: currentModeValues,
          drafts: { ...state.drafts, audio: newDrafts },
        };
      }

      // Advanced → Basic: stash advanced, restore basic
      if (state.audioBasicStash) {
        restoreStashIntoDrafts(newDrafts, state.audioBasicStash, snapshot);
      } else {
        applyTierDefaultsToDrafts(newDrafts, qualityTier, snapshot);
      }

      return {
        audioAdvancedStash: currentModeValues,
        drafts: { ...state.drafts, audio: newDrafts },
      };
    }),

  // ── Apply ────────────────────────────────────────────────────────────────

  apply: () => {
    const { snapshot, drafts } = get();
    if (!snapshot) return;

    const willRestart = 'hardwareAcceleration' in drafts.video;

    // Appearance — already written through to real store during draft preview.
    // Sync color scheme to server + memberStore now (subscriber was suppressed).
    if (
      'colorScheme' in drafts.appearance ||
      'customColors' in drafts.appearance ||
      'theme' in drafts.appearance
    ) {
      syncColorSchemeToServer();
    }

    // Audio — push to real store
    if (Object.keys(drafts.audio).length > 0) {
      pushDraftsToStore(
        useAudioSettingsStore.getState() as unknown as Record<string, unknown>,
        drafts.audio as Record<string, unknown>
      );
    }

    // Video — push to real store (except hardwareAcceleration IPC handled below)
    if (Object.keys(drafts.video).length > 0) {
      pushDraftsToStore(
        useVideoSettingsStore.getState() as unknown as Record<string, unknown>,
        drafts.video as Record<string, unknown>,
        new Set(['hardwareAcceleration'])
      );
    }

    // TTS — push to real store
    if (Object.keys(drafts.tts).length > 0) {
      pushDraftsToStore(
        useTTSSettingsStore.getState() as unknown as Record<string, unknown>,
        drafts.tts as Record<string, unknown>
      );
    }

    // Re-snapshot and clear drafts
    // Temporarily allow server sync to fire for appearance changes
    setSyncSuppressed(false);

    set({
      snapshot: takeSnapshot(),
      drafts: emptyDrafts(),
      // Keep mode stashes for future toggles
    });

    // Re-enable draft mode (settings page is still open)
    setSyncSuppressed(true);

    // Hardware acceleration restart — must happen AFTER all stores are saved
    if (willRestart) {
      setTimeout(() => {
        const hwValue = drafts.video.hardwareAcceleration;
        if (hwValue !== undefined) globalThis.electron?.setHardwareAcceleration?.(hwValue);
        globalThis.electron?.relaunchApp?.();
      }, 150);
    }
  },

  // ── Revert ──────────────────────────────────────────────────────────────

  revert: () => {
    const { snapshot } = get();

    // Restore appearance from snapshot (undo live preview)
    if (snapshot) {
      restoreAppearanceFromSnapshot(snapshot);
    }

    // Clear all drafts and stashes
    set({
      drafts: emptyDrafts(),
      audioBasicStash: null,
      audioAdvancedStash: null,
    });
  },
}));
