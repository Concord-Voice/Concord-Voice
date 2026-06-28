import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import type { CodecCapability, GpuInfo } from '../services/mediaCapabilities';

// ---------------------------------------------------------------------------
// Video quality presets
// ---------------------------------------------------------------------------

export interface VideoQualityPreset {
  label: string;
  width: number;
  height: number;
  frameRate: number;
  maxBitrate: number;
}

export const VIDEO_QUALITY_PRESETS: Record<string, VideoQualityPreset> = {
  system: { label: 'System Default', width: 0, height: 0, frameRate: 0, maxBitrate: 2_500_000 },
  '360p30': { label: '360p 30fps', width: 640, height: 360, frameRate: 30, maxBitrate: 400_000 },
  '480p30': { label: '480p 30fps', width: 854, height: 480, frameRate: 30, maxBitrate: 700_000 },
  '720p30': { label: '720p 30fps', width: 1280, height: 720, frameRate: 30, maxBitrate: 1_500_000 },
  '720p60': { label: '720p 60fps', width: 1280, height: 720, frameRate: 60, maxBitrate: 2_500_000 },
  '1080p30': {
    label: '1080p 30fps',
    width: 1920,
    height: 1080,
    frameRate: 30,
    maxBitrate: 3_500_000,
  },
  '1080p60': {
    label: '1080p 60fps',
    width: 1920,
    height: 1080,
    frameRate: 60,
    maxBitrate: 5_000_000,
  },
  '1440p30': {
    label: '1440p 30fps',
    width: 2560,
    height: 1440,
    frameRate: 30,
    maxBitrate: 6_000_000,
  },
  '1440p60': {
    label: '1440p 60fps',
    width: 2560,
    height: 1440,
    frameRate: 60,
    maxBitrate: 8_000_000,
  },
  '4K30': { label: '4K 30fps', width: 3840, height: 2160, frameRate: 30, maxBitrate: 10_000_000 },
  '4K60': { label: '4K 60fps', width: 3840, height: 2160, frameRate: 60, maxBitrate: 16_000_000 },
};

// ---------------------------------------------------------------------------
// Types
// ---------------------------------------------------------------------------

// Screen resolution: 'source' | '720p' | '1080p' | '1440p' | '4K' | 'WxH' (custom display resolution)
export type ScreenContentType = 'auto' | 'motion' | 'detail';
export type DegradationPreference = 'balanced' | 'maintain-framerate' | 'maintain-resolution';
export type VideoPriority = 'off' | 'low' | 'medium' | 'high';

/** Per-share screen share options passed from the picker to voiceService. */
export interface ScreenShareOptions {
  resolution: string;
  frameRate: number;
  contentType: ScreenContentType;
}

// ---------------------------------------------------------------------------
// Store
// ---------------------------------------------------------------------------

export interface VideoSettings {
  // UI mode
  videoAdvancedMode: boolean;

  // Camera
  cameraPreset: string; // Key into VIDEO_QUALITY_PRESETS
  preferredVideoCodec: string | null; // mimeType e.g. "video/VP9"
  cameraPriority: VideoPriority; // DSCP priority for camera traffic

  // Screen share
  screenResolution: string;
  screenFrameRate: number; // 0 = Native (display refresh rate), or specific FPS value
  screenContentType: ScreenContentType;
  screenSharePriority: VideoPriority; // DSCP priority for screen share traffic
  screenShareBitrate: number; // bps, 0 = auto (codec-dependent)

  // Encoding behavior
  degradationPreference: DegradationPreference; // How to handle congestion: drop fps or resolution
  scalabilityMode: 'auto' | 'L1T3' | 'L2T3' | 'L3T3'; // SVC spatial×temporal layers

  // Casting eligibility (codec-derived kind is gated by these allow-lists)
  supportSvc: boolean; // AV1/VP9 may publish layered SVC
  supportSimulcast: boolean; // H264/VP8 may publish simulcast

  // Hardware
  hardwareAcceleration: boolean;

  // HDR
  hdrEncoding: boolean; // Enable HDR encoding (VP9 P2). Auto-detected from display.
  systemHdr: boolean; // Whether any connected display supports HDR (non-persisted, runtime)

  // Cached capabilities (non-persisted in practice, but ok to persist for fast initial render)
  codecCapabilities: CodecCapability[];
  gpuInfo: GpuInfo | null;
}

interface VideoSettingsState extends VideoSettings {
  setCameraPreset: (preset: string) => void;
  setPreferredVideoCodec: (codec: string | null) => void;
  setCameraPriority: (priority: VideoPriority) => void;
  setScreenResolution: (res: string) => void;
  setScreenFrameRate: (fps: number) => void;
  setScreenContentType: (type: ScreenContentType) => void;
  setScreenSharePriority: (priority: VideoPriority) => void;
  setScreenShareBitrate: (bitrate: number) => void;
  setDegradationPreference: (pref: DegradationPreference) => void;
  setScalabilityMode: (mode: 'auto' | 'L1T3' | 'L2T3' | 'L3T3') => void;
  setSupportSvc: (enabled: boolean) => void;
  setSupportSimulcast: (enabled: boolean) => void;
  setHardwareAcceleration: (enabled: boolean) => void;
  setHdrEncoding: (enabled: boolean) => void;
  setVideoAdvancedMode: (enabled: boolean) => void;
  setCodecCapabilities: (caps: CodecCapability[]) => void;
  setGpuInfo: (info: GpuInfo | null) => void;
}

const defaults: VideoSettings = {
  videoAdvancedMode: false,
  cameraPreset: 'system',
  preferredVideoCodec: null,
  cameraPriority: 'medium',
  screenResolution: 'source',
  screenFrameRate: 30,
  screenContentType: 'auto',
  screenSharePriority: 'medium',
  screenShareBitrate: 0, // Auto
  degradationPreference: 'balanced',
  scalabilityMode: 'auto', // Auto = L3T3 for SVC codecs
  supportSvc: true,
  supportSimulcast: true,
  hardwareAcceleration: true,
  hdrEncoding: false,
  systemHdr: false,
  codecCapabilities: [],
  gpuInfo: null,
};

export const useVideoSettingsStore = wrapStore(
  create<VideoSettingsState>()(
    persist(
      (set) => ({
        ...defaults,

        setVideoAdvancedMode: (videoAdvancedMode) => set({ videoAdvancedMode }),
        setCameraPreset: (cameraPreset) => set({ cameraPreset }),
        setPreferredVideoCodec: (preferredVideoCodec) => set({ preferredVideoCodec }),
        setCameraPriority: (cameraPriority) => set({ cameraPriority }),
        setScreenResolution: (screenResolution) => set({ screenResolution }),
        setScreenFrameRate: (screenFrameRate) => set({ screenFrameRate }),
        setScreenContentType: (screenContentType) => set({ screenContentType }),
        setScreenSharePriority: (screenSharePriority) => set({ screenSharePriority }),
        setScreenShareBitrate: (screenShareBitrate) => set({ screenShareBitrate }),
        setDegradationPreference: (degradationPreference) => set({ degradationPreference }),
        setScalabilityMode: (scalabilityMode) => set({ scalabilityMode }),
        setSupportSvc: (supportSvc) => set({ supportSvc }),
        setSupportSimulcast: (supportSimulcast) => set({ supportSimulcast }),
        setHardwareAcceleration: (hardwareAcceleration) => set({ hardwareAcceleration }),
        setHdrEncoding: (hdrEncoding) => set({ hdrEncoding }),
        setCodecCapabilities: (codecCapabilities) => set({ codecCapabilities }),
        setGpuInfo: (gpuInfo) => set({ gpuInfo }),
      }),
      {
        name: 'concord:video-settings',
        partialize: (state) => {
          // Exclude runtime-only fields from persistence
          const { systemHdr: _, ...rest } = state;
          return rest;
        },
      }
    )
  )
);
