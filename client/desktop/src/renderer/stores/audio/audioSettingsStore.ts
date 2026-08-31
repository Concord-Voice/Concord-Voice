import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';

// ---------------------------------------------------------------------------
// Advanced audio settings — persisted to localStorage
// These override the quality tier's defaults when set.
// ---------------------------------------------------------------------------

export type AudioPriority = 'off' | 'low' | 'medium' | 'high';

export interface AudioSettings {
  // Basic/Advanced mode toggle
  advancedMode: boolean;

  // Audio processing (getUserMedia constraints)
  noiseCancellation: boolean;
  echoCancellation: boolean;
  autoGainControl: boolean;
  noiseGateMode: 'auto' | 'manual'; // Auto = no gate (AGC handles it), Manual = expose slider
  noiseGateLevel: number; // dBFS, range -80 to -20 (only used in manual mode)

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

interface AudioSettingsState extends AudioSettings {
  setAdvancedMode: (enabled: boolean) => void;
  setNoiseCancellation: (enabled: boolean) => void;
  setEchoCancellation: (enabled: boolean) => void;
  setAutoGainControl: (enabled: boolean) => void;
  setNoiseGateMode: (mode: 'auto' | 'manual') => void;
  setNoiseGateLevel: (level: number) => void;
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
  autoGainControl: true,
  noiseGateMode: 'auto',
  noiseGateLevel: -50,
  musicMode: false,
  frameSize: 0, // Default — resolved at runtime to tier's preferredFrameSize
  silenceDetection: false,
  stereoOverride: null, // null = follow tier default
  inlineFec: true,
  fecHeadroom: true,
  opusNack: false,
  adaptivePtime: false,
  audioPriority: 'medium',
  inputVolume: 100,
  outputVolume: 100,
  perParticipantVolume: {},
  perScreenShareVolume: {},
  quietBoost: false,
  quietBoostThreshold: -35,
  networkType: 'auto',
  packetLossWarningThreshold: 3,
};

export const useAudioSettingsStore = wrapStore(
  create<AudioSettingsState>()(
    persist(
      (set) => ({
        ...defaults,

        setAdvancedMode: (advancedMode) => set({ advancedMode }),
        setNoiseCancellation: (noiseCancellation) => set({ noiseCancellation }),
        setEchoCancellation: (echoCancellation) => set({ echoCancellation }),
        setAutoGainControl: (autoGainControl) => set({ autoGainControl }),
        setNoiseGateMode: (noiseGateMode) => set({ noiseGateMode }),
        setNoiseGateLevel: (noiseGateLevel) =>
          set({ noiseGateLevel: Math.max(-80, Math.min(-20, noiseGateLevel)) }),
        setMusicMode: (musicMode) => set({ musicMode }),
        setFrameSize: (frameSize) => set({ frameSize }),
        setSilenceDetection: (silenceDetection) => set({ silenceDetection }),
        setStereoOverride: (stereoOverride) => set({ stereoOverride }),
        setInlineFec: (inlineFec) => set({ inlineFec }),
        setFecHeadroom: (fecHeadroom) => set({ fecHeadroom }),
        setOpusNack: (opusNack) => set({ opusNack }),
        setAdaptivePtime: (adaptivePtime) => set({ adaptivePtime }),
        setAudioPriority: (audioPriority) => set({ audioPriority }),
        setInputVolume: (inputVolume) =>
          set({ inputVolume: Math.max(0, Math.min(200, inputVolume)) }),
        setOutputVolume: (outputVolume) =>
          set({ outputVolume: Math.max(0, Math.min(200, outputVolume)) }),
        setParticipantVolume: (userId, volume) =>
          set((state) => ({
            perParticipantVolume: {
              ...state.perParticipantVolume,
              [userId]: Math.max(0, Math.min(200, volume)),
            },
          })),
        clearParticipantVolume: (userId) =>
          set((state) => {
            if (!(userId in state.perParticipantVolume)) return state;
            const next = { ...state.perParticipantVolume };
            delete next[userId];
            return { perParticipantVolume: next };
          }),
        // Per-participant overrides are keyed by other users' IDs — user-scoped
        // data inside an otherwise device-scoped store. Cleared on logout-class
        // resets (#1603) so a prior account's contact IDs never persist for the
        // next account on this device (#1233 cross-account discipline).
        clearAllParticipantVolumes: () => set({ perParticipantVolume: {} }),
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
        version: 2,
        migrate: (persistedState: unknown, version: number) => {
          const state = persistedState as Record<string, unknown>;
          if (version === 0) {
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
          if (version <= 1) {
            // v1→v2: autoFecMode + fecManualPercent → inlineFec + fecHeadroom
            const mode = state.autoFecMode as string | undefined;
            state.inlineFec = mode !== 'off';
            state.fecHeadroom = mode !== 'off' && mode !== 'manual';
            delete state.autoFecMode;
            delete state.fecManualPercent;
          }
          return state as unknown as AudioSettingsState;
        },
      }
    )
  )
);
