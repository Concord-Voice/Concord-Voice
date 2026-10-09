import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';

// ---------------------------------------------------------------------------
// Advanced audio settings — persisted to localStorage
// These override the quality tier's defaults when set.
// ---------------------------------------------------------------------------

export type AudioPriority = 'off' | 'low' | 'medium' | 'high';
export type NoiseGateMode = 'dynamic' | 'autoCalibrate' | 'manualCalibrate' | 'off';

export interface AudioSettings {
  // Basic/Advanced mode toggle
  advancedMode: boolean;

  // Audio processing (getUserMedia constraints)
  noiseCancellation: boolean;
  echoCancellation: boolean;
  autoGainControl: boolean;
  noiseGateMode: NoiseGateMode;
  noiseGateLevel: number; // dBFS, range -80 to -20 (used in Manual Calibrate)

  // Opus advanced
  musicMode: boolean; // Disables audio processing (echo cancel, noise suppression, AGC) for music fidelity
  frameSize: 0 | 10 | 20 | 40 | 60; // ms (ptime) — 0 = "Default" (use tier's preferredFrameSize)
  silenceDetection: boolean; // DTX override
  stereoOverride: boolean | null; // null = follow tier default, true/false = force

  // FEC & reliability
  inlineFec: boolean; // Allow Opus to embed in-band FEC redundancy
  fecHeadroom: boolean; // Reactively inflate bitrate ceiling when loss detected, giving Opus room for FEC
  opusNack: boolean; // Request retransmission of lost audio packets

  // Transport
  adaptivePtime: boolean; // Let WebRTC dynamically adjust frame size based on network
  audioPriority: AudioPriority; // Encoding + network priority hint (DSCP via RFC 4594)

  // Volume
  inputVolume: number; // 0–200 (percent), default 100. Applied via GainNode in mic pipeline.
  outputVolume: number; // 0–200 (percent), default 100. Applied via GainNode per remote participant.
  /**
   * Per-participant output volume overrides, keyed by userId (percent, 0–200).
   * Multiplied with master `outputVolume` at playback time. Missing keys default
   * to 100 (treated as unity — no adjustment relative to master).
   */
  perParticipantVolume: Record<string, number>;
  /**
   * Volume a participant was at before being muted to 0, so unmuting restores it
   * rather than jumping to 100. Persisted BECAUSE `perParticipantVolume` is: the
   * mute survives a restart, so a restore hint that did not would silently turn
   * "unmute" into "reset to full" on the next launch. Written only by
   * `setParticipantVolume` when it lands on 0, and cleared with its pair.
   */
  previousParticipantVolume: Record<string, number>;
  /**
   * Per-screenshare audio volume overrides, keyed by the SHARER's userId (percent,
   * 0–200). Independent of `perParticipantVolume` (which governs that user's voice/mic
   * audio) so a viewer can tune a screenshare's audio without touching the sharer's
   * voice level. Multiplied with master `outputVolume` at playback time.
   */
  perScreenShareVolume: Record<string, number>;

  // Quiet boost (receiver-side upward compressor)
  quietBoost: boolean; // Dynamically amplify quiet participants
  quietBoostThreshold: number; // dBFS, range -50 to -20. Below this → boost applied.

  // Network
  networkType: 'auto' | 'wifi' | 'wired';
  packetLossWarningThreshold: number; // percent, triggers UI warning
}

const DEFAULT_INPUT_VOLUME = 100;
const DEFAULT_NOISE_GATE_LEVEL = -50;

type CaptureGateSettings = Pick<
  AudioSettings,
  'autoGainControl' | 'musicMode' | 'noiseGateMode' | 'noiseGateLevel'
>;

function normalizeNoiseGateLevel(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value)
    ? Math.max(-80, Math.min(-20, value))
    : DEFAULT_NOISE_GATE_LEVEL;
}

function normalizeNoiseGateMode(
  value: unknown,
  autoGainControl: boolean,
  musicMode: boolean
): Exclude<NoiseGateMode, 'autoCalibrate'> {
  if (value === 'off' || value === 'autoCalibrate') return 'off';
  if (value === 'dynamic') return 'dynamic';
  if (value === 'manualCalibrate') return !musicMode && autoGainControl ? 'dynamic' : value;
  return 'off';
}

export function effectiveNoiseGateMode(
  settings: Pick<AudioSettings, 'musicMode' | 'autoGainControl' | 'noiseGateMode'>
): 'off' | 'dynamic' | 'manualCalibrate' {
  return normalizeNoiseGateMode(
    settings.noiseGateMode,
    settings.autoGainControl === true,
    settings.musicMode === true
  );
}

function normalizeInputVolume(inputVolume: unknown): number {
  if (typeof inputVolume !== 'number' || !Number.isFinite(inputVolume)) {
    return DEFAULT_INPUT_VOLUME;
  }
  return Math.max(0, Math.min(200, inputVolume));
}

function migrateVersionZero(state: Record<string, unknown>): void {
  // v0→v1: Rename fecMode → autoFecMode, add advancedMode + stereoOverride
  if (state.fecMode) {
    state.autoFecMode = state.fecMode === 'auto' ? 'default' : 'manual';
    delete state.fecMode;
  }
  if (typeof state.fecManualPercent === 'number' && state.fecManualPercent > 40) {
    state.fecManualPercent = 40;
  }
  if (state.advancedMode === undefined) state.advancedMode = false;
  if (state.stereoOverride === undefined) state.stereoOverride = null;
}

function migrateVersionOne(state: Record<string, unknown>): void {
  // v1→v2: autoFecMode + fecManualPercent → inlineFec + fecHeadroom
  const mode = state.autoFecMode as string | undefined;
  state.inlineFec = mode !== 'off';
  state.fecHeadroom = mode !== 'off' && mode !== 'manual';
  delete state.autoFecMode;
  delete state.fecManualPercent;
}

function migrateLegacyNoiseGate(state: Record<string, unknown>): void {
  if (state.noiseGateMode === 'auto') {
    state.noiseGateMode = 'off';
    return;
  }
  if (state.noiseGateMode !== 'manual') return;

  state.noiseGateMode =
    state.autoGainControl !== false && state.musicMode !== true ? 'dynamic' : 'manualCalibrate';
}

export function effectiveMicLevelPercent(
  settings: Pick<AudioSettings, 'musicMode' | 'autoGainControl' | 'inputVolume'>
): number {
  const inputVolume = normalizeInputVolume(settings.inputVolume);
  if (inputVolume > 0 && !settings.musicMode && settings.autoGainControl)
    return DEFAULT_INPUT_VOLUME;
  return inputVolume;
}

interface AudioSettingsState extends AudioSettings {
  setAdvancedMode: (enabled: boolean) => void;
  setNoiseCancellation: (enabled: boolean) => void;
  setEchoCancellation: (enabled: boolean) => void;
  setAutoGainControl: (enabled: boolean) => void;
  setNoiseGateMode: (mode: NoiseGateMode) => void;
  setNoiseGateLevel: (level: number) => void;
  setCaptureGateSettings: (patch: Partial<CaptureGateSettings>) => void;
  setMusicMode: (enabled: boolean) => void;
  setFrameSize: (size: 0 | 10 | 20 | 40 | 60) => void;
  setSilenceDetection: (enabled: boolean) => void;
  setStereoOverride: (override: boolean | null) => void;
  setInlineFec: (enabled: boolean) => void;
  setFecHeadroom: (enabled: boolean) => void;
  setOpusNack: (enabled: boolean) => void;
  setAdaptivePtime: (enabled: boolean) => void;
  setAudioPriority: (priority: AudioPriority) => void;
  setInputVolume: (volume: number) => void;
  setOutputVolume: (volume: number) => void;
  setParticipantVolume: (userId: string, volume: number) => void;
  clearParticipantVolume: (userId: string) => void;
  clearAllParticipantVolumes: () => void;
  setScreenShareVolume: (userId: string, volume: number) => void;
  clearScreenShareVolume: (userId: string) => void;
  clearAllScreenShareVolumes: () => void;
  setQuietBoost: (enabled: boolean) => void;
  setQuietBoostThreshold: (threshold: number) => void;
  setNetworkType: (type: 'auto' | 'wifi' | 'wired') => void;
  setPacketLossWarningThreshold: (percent: number) => void;
}

const defaults: AudioSettings = {
  advancedMode: false,
  noiseCancellation: true,
  echoCancellation: true,
  autoGainControl: false,
  noiseGateMode: 'dynamic',
  noiseGateLevel: DEFAULT_NOISE_GATE_LEVEL,
  musicMode: false,
  frameSize: 0, // Default — resolved at runtime to tier's preferredFrameSize
  silenceDetection: false,
  stereoOverride: null, // null = follow tier default
  inlineFec: true,
  fecHeadroom: false,
  opusNack: false,
  adaptivePtime: true,
  audioPriority: 'medium',
  inputVolume: DEFAULT_INPUT_VOLUME,
  outputVolume: 100,
  perParticipantVolume: {},
  previousParticipantVolume: {},
  perScreenShareVolume: {},
  quietBoost: false,
  quietBoostThreshold: -35,
  networkType: 'auto',
  packetLossWarningThreshold: 3,
};

export const useAudioSettingsStore = wrapStore(
  create<AudioSettingsState>()(
    persist(
      (set, get) => ({
        ...defaults,

        setAdvancedMode: (advancedMode) => set({ advancedMode }),
        setNoiseCancellation: (noiseCancellation) => set({ noiseCancellation }),
        setEchoCancellation: (echoCancellation) => set({ echoCancellation }),
        setCaptureGateSettings: (patch) =>
          set((state) => {
            const autoGainControl =
              typeof patch.autoGainControl === 'boolean'
                ? patch.autoGainControl
                : state.autoGainControl;
            const musicMode =
              typeof patch.musicMode === 'boolean' ? patch.musicMode : state.musicMode;
            const noiseGateMode = normalizeNoiseGateMode(
              patch.noiseGateMode ?? state.noiseGateMode,
              autoGainControl,
              musicMode
            );
            return {
              autoGainControl,
              musicMode,
              noiseGateMode,
              noiseGateLevel: normalizeNoiseGateLevel(patch.noiseGateLevel ?? state.noiseGateLevel),
            };
          }),
        setAutoGainControl: (autoGainControl) => get().setCaptureGateSettings({ autoGainControl }),
        setNoiseGateMode: (noiseGateMode) => get().setCaptureGateSettings({ noiseGateMode }),
        setNoiseGateLevel: (noiseGateLevel) => get().setCaptureGateSettings({ noiseGateLevel }),
        setMusicMode: (musicMode) => get().setCaptureGateSettings({ musicMode }),
        setFrameSize: (frameSize) => set({ frameSize }),
        setSilenceDetection: (silenceDetection) => set({ silenceDetection }),
        setStereoOverride: (stereoOverride) => set({ stereoOverride }),
        setInlineFec: (inlineFec) => set({ inlineFec }),
        setFecHeadroom: (fecHeadroom) => set({ fecHeadroom }),
        setOpusNack: (opusNack) => set({ opusNack }),
        setAdaptivePtime: (adaptivePtime) => set({ adaptivePtime }),
        setAudioPriority: (audioPriority) => set({ audioPriority }),
        setInputVolume: (inputVolume) => set({ inputVolume: normalizeInputVolume(inputVolume) }),
        setOutputVolume: (outputVolume) =>
          set({ outputVolume: Math.max(0, Math.min(200, outputVolume)) }),
        setParticipantVolume: (userId, volume) =>
          set((state) => {
            const next = Math.max(0, Math.min(200, volume));
            const current = state.perParticipantVolume[userId];
            // Muting: remember what they were at first. Guarded on > 0 so muting
            // an already-muted participant cannot overwrite the real value with 0.
            const previous =
              next === 0 && typeof current === 'number' && current > 0
                ? { ...state.previousParticipantVolume, [userId]: current }
                : state.previousParticipantVolume;
            return {
              perParticipantVolume: { ...state.perParticipantVolume, [userId]: next },
              previousParticipantVolume: previous,
            };
          }),
        clearParticipantVolume: (userId) =>
          set((state) => {
            const hasVolume = userId in state.perParticipantVolume;
            const hasPrevious = userId in state.previousParticipantVolume;
            if (!hasVolume && !hasPrevious) return state;
            const next = { ...state.perParticipantVolume };
            delete next[userId];
            // The restore hint is meaningless without its volume, and leaving it
            // would resurrect a stale value if an override is set again later.
            const previous = { ...state.previousParticipantVolume };
            delete previous[userId];
            return { perParticipantVolume: next, previousParticipantVolume: previous };
          }),
        // Per-participant overrides are keyed by other users' IDs — user-scoped
        // data inside an otherwise device-scoped store. Cleared on logout-class
        // resets (#1603) so a prior account's contact IDs never persist for the
        // next account on this device (#1233 cross-account discipline).
        clearAllParticipantVolumes: () =>
          set({ perParticipantVolume: {}, previousParticipantVolume: {} }),
        setScreenShareVolume: (userId, volume) =>
          set((state) => ({
            perScreenShareVolume: {
              ...state.perScreenShareVolume,
              [userId]: Math.max(0, Math.min(200, volume)),
            },
          })),
        clearScreenShareVolume: (userId) =>
          set((state) => {
            if (!(userId in state.perScreenShareVolume)) return state;
            const next = { ...state.perScreenShareVolume };
            delete next[userId];
            return { perScreenShareVolume: next };
          }),
        // Keyed by other users' IDs — cleared on logout-class resets (#1603) like
        // perParticipantVolume so a prior account's contact IDs never persist (#1233).
        clearAllScreenShareVolumes: () => set({ perScreenShareVolume: {} }),
        setQuietBoost: (quietBoost) => set({ quietBoost }),
        setQuietBoostThreshold: (quietBoostThreshold) =>
          set({ quietBoostThreshold: Math.max(-50, Math.min(-20, quietBoostThreshold)) }),
        setNetworkType: (networkType) => set({ networkType }),
        setPacketLossWarningThreshold: (packetLossWarningThreshold) =>
          set({ packetLossWarningThreshold }),
      }),
      {
        name: 'concord:audio-advanced',
        version: 3,
        merge: (persistedState, currentState) => {
          const persisted =
            typeof persistedState === 'object' && persistedState !== null
              ? (persistedState as Partial<AudioSettingsState>)
              : {};
          const inputVolume = Object.hasOwn(persisted, 'inputVolume')
            ? persisted.inputVolume
            : currentState.inputVolume;
          const autoGainControl =
            typeof persisted.autoGainControl === 'boolean'
              ? persisted.autoGainControl
              : defaults.autoGainControl;
          const musicMode =
            typeof persisted.musicMode === 'boolean' ? persisted.musicMode : defaults.musicMode;
          return {
            ...currentState,
            ...persisted,
            inputVolume: normalizeInputVolume(inputVolume),
            autoGainControl,
            musicMode,
            noiseGateMode: normalizeNoiseGateMode(
              Object.hasOwn(persisted, 'noiseGateMode')
                ? persisted.noiseGateMode
                : currentState.noiseGateMode,
              autoGainControl,
              musicMode
            ),
            noiseGateLevel: normalizeNoiseGateLevel(
              persisted.noiseGateLevel ?? currentState.noiseGateLevel
            ),
          };
        },
        migrate: (persistedState: unknown, version: number) => {
          const state =
            typeof persistedState === 'object' && persistedState !== null
              ? { ...(persistedState as Record<string, unknown>) }
              : ({} as Record<string, unknown>);
          if (version === 0) migrateVersionZero(state);
          if (version <= 1) migrateVersionOne(state);
          if (version <= 2) migrateLegacyNoiseGate(state);
          return state as unknown as AudioSettingsState;
        },
      }
    )
  )
);
