import type { Entitlement } from '../stores/auth/subscriptionStore';
import type { AppearanceSettings } from '../stores/ui/settingsStore';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../stores/voice/voiceStore';
import { VIDEO_QUALITY_PRESETS } from '../stores/voice/videoSettingsStore';
import {
  videoLimitsFromEntitlement,
  maxFpsForResolution,
  type VideoAxisLimit,
} from './videoLimits';
import { SCREEN_RES_DIMS, highestFreeScreenResolution } from './screenResolution';

/**
 * A pure snapshot of the client settings that the launch-reset clamp touches —
 * pulled from three different stores (settingsStore appearance, voiceStore,
 * audioSettingsStore, videoSettingsStore). Kept as a flat plain object so
 * `clampToFreeTier` is unit-testable without any store.
 *
 * Field names mirror the real store fields exactly:
 *  - `colorScheme`        → settingsStore.appearance.colorScheme
 *  - `qualityTier`        → voiceStore.qualityTier
 *  - `cameraPreset`       → videoSettingsStore.cameraPreset (key into VIDEO_QUALITY_PRESETS)
 *  - `screenShareBitrate` → videoSettingsStore.screenShareBitrate (bps; 0 = auto)
 *  - `cameraBitrate`      → videoSettingsStore.cameraBitrate (bps; 0 = auto)
 *  - `musicMode`          → audioSettingsStore.musicMode
 */
export interface ClampableSettings {
  colorScheme: AppearanceSettings['colorScheme'];
  qualityTier: AudioQualityTier;
  cameraPreset: string;
  screenShareBitrate: number;
  cameraBitrate: number;
  musicMode: boolean;
  screenResolution: string; // videoSettingsStore.screenResolution (#2163)
  screenFrameRate: number; // videoSettingsStore.screenFrameRate; 0 = native (#2163)
}

/** The highest free audio tier a premium tier is clamped down to. */
const FREE_AUDIO_TIER_FLOOR: AudioQualityTier = 'standard';

/**
 * Resolve the highest free camera preset whose resolution AND frame rate sit at
 * or below the CAMERA-axis entitlement ceilings (#1602). We pick the preset with
 * the largest pixel-rate (height × width × fps) that still fits — the closest
 * free equivalent of an over-cap pick — falling back to System Default if none
 * qualifies (System Default is height/fps 0 → always within caps, driver decides).
 */
function highestFreeCameraPreset(camera: VideoAxisLimit): string {
  let best = 'system';
  let bestScore = -1;
  for (const [key, preset] of Object.entries(VIDEO_QUALITY_PRESETS)) {
    const withinCaps = preset.height <= camera.height && preset.frameRate <= camera.fps;
    if (!withinCaps) continue;
    const score = preset.height * preset.width * preset.frameRate;
    if (score > bestScore) {
      bestScore = score;
      best = key;
    }
  }
  return best;
}

/** Does the camera preset exceed the CAMERA-axis video caps (height / fps)? */
function cameraPresetExceedsCaps(cameraPreset: string, camera: VideoAxisLimit): boolean {
  const preset = VIDEO_QUALITY_PRESETS[cameraPreset];
  // Unknown / System Default presets never exceed — height/fps are 0 (driver decides).
  if (!preset) return false;
  return preset.height > camera.height || preset.frameRate > camera.fps;
}

/**
 * Dims for a screen-share resolution the reset clamp can resolve WITHOUT a live
 * display probe: a fixed preset (720p/1080p/…) or a custom `WxH` string (the dims
 * are encoded in the value). 'source'/native returns undefined — it genuinely
 * needs display dims this pure snapshot lacks, so it is left to the produce
 * boundary (#2163).
 */
function fixedOrCustomScreenDims(res: string): { w: number; h: number } | undefined {
  const fixed = SCREEN_RES_DIMS[res];
  if (fixed) return fixed;
  const custom = /^(\d+)x(\d+)$/.exec(res);
  if (custom) return { w: Number(custom[1]), h: Number(custom[2]) };
  return undefined;
}

/**
 * Pure free-tier clamp (#1301 Decision 4 / spec §4.2). Given the current client
 * settings + the (free) entitlement, return the clamped settings and whether
 * ANYTHING changed. Caller (useLaunchReset) writes the result back through the
 * real store setters only when `changed === true`, and surfaces the one-time
 * reset explainer.
 *
 * Clamp targets (split video axes, #1602):
 *  - premium audio tier             → 'standard'
 *  - camera preset over camera caps → highest free preset (CAMERA axis height/fps)
 *  - manual screen-share bitrate    → STREAM-axis bitrate ceiling (0 = auto, never clamped)
 *  - manual camera bitrate          → CAMERA-axis bitrate ceiling (0 = auto, never clamped)
 *  - music mode                     → off
 *
 * Idempotent: a settings snapshot already at/below the free floor returns
 * `changed: false` with the input untouched.
 */
export function clampToFreeTier(
  settings: ClampableSettings,
  entitlement: Entitlement
): { settings: ClampableSettings; changed: boolean } {
  const next: ClampableSettings = { ...settings };
  const limits = videoLimitsFromEntitlement(entitlement);
  let changed = false;

  // 1. Premium audio tier -> 'standard'. A tier the entitlement no longer allows
  //    is clamped to the highest free tier (Standard).
  if (
    AUDIO_QUALITY_TIERS[settings.qualityTier]?.premium &&
    !entitlement.allowedAudioTiers.includes(settings.qualityTier)
  ) {
    next.qualityTier = FREE_AUDIO_TIER_FLOOR;
    changed = true;
  }

  // 2. Camera preset over the CAMERA-axis ceiling -> highest free preset.
  if (cameraPresetExceedsCaps(settings.cameraPreset, limits.camera)) {
    next.cameraPreset = highestFreeCameraPreset(limits.camera);
    changed = true;
  }

  // 3. Manual screen-share bitrate over the STREAM-axis cap -> cap. 0 (= auto) is
  //    never a manual value, so it is left untouched.
  if (settings.screenShareBitrate > 0 && settings.screenShareBitrate > limits.stream.bitrate) {
    next.screenShareBitrate = limits.stream.bitrate;
    changed = true;
  }

  // 4. Manual camera bitrate over the CAMERA-axis cap -> cap. 0 (= auto) untouched.
  if (settings.cameraBitrate > 0 && settings.cameraBitrate > limits.camera.bitrate) {
    next.cameraBitrate = limits.camera.bitrate;
    changed = true;
  }

  // 5. Music mode -> off (gated by allowMusicMode).
  if (settings.musicMode && !entitlement.allowMusicMode) {
    next.musicMode = false;
    changed = true;
  }

  // 6. Screen-share resolution over the STREAM-axis height ceiling -> highest
  //    permitted fixed resolution (#2163). Fixed presets AND custom WxH picks are
  //    clamped here (both encode their dims); only 'source'/native is left to the
  //    produce boundary (it needs display dims this pure snapshot lacks).
  const screenDims = fixedOrCustomScreenDims(settings.screenResolution);
  if (screenDims && screenDims.h > limits.stream.height) {
    next.screenResolution = highestFreeScreenResolution(limits.stream.height);
    changed = true;
  }

  // 7. Screen-share frame rate over the tiered max for the (possibly just-clamped)
  //    resolution -> tiered max (#2163). Only explicit fps (>0) is clamped; 0
  //    (native) is enforced at the produce boundary. A custom WxH that fits the
  //    height ceiling (e.g. an ultrawide 2560x1080) still tiers its fps here.
  const effDims = fixedOrCustomScreenDims(next.screenResolution);
  if (settings.screenFrameRate > 0 && effDims) {
    const capped = maxFpsForResolution(effDims.w, effDims.h, limits.stream);
    if (settings.screenFrameRate > capped) {
      next.screenFrameRate = capped;
      changed = true;
    }
  }

  return { settings: next, changed };
}
