import type { Entitlement } from '../stores/subscriptionStore';
import type { AppearanceSettings } from '../stores/settingsStore';
import { AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../stores/voiceStore';
import { VIDEO_QUALITY_PRESETS } from '../stores/videoSettingsStore';

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
 *  - `musicMode`          → audioSettingsStore.musicMode
 */
export interface ClampableSettings {
  colorScheme: AppearanceSettings['colorScheme'];
  qualityTier: AudioQualityTier;
  cameraPreset: string;
  screenShareBitrate: number;
  musicMode: boolean;
}

/** The default color scheme an over-cap custom scheme is reset to. */
export const DEFAULT_COLOR_SCHEME: AppearanceSettings['colorScheme'] = 'concord';

/** The highest free audio tier a premium tier is clamped down to. */
const FREE_AUDIO_TIER_FLOOR: AudioQualityTier = 'standard';

/**
 * Resolve the highest free camera preset whose resolution AND frame rate sit at
 * or below the free entitlement ceilings. We pick the preset with the largest
 * pixel-rate (height × fps) that still fits — the closest free equivalent of an
 * over-cap pick — falling back to System Default if none qualifies (System
 * Default is height/fps 0 → always within caps, the camera/driver decides).
 */
function highestFreeCameraPreset(entitlement: Entitlement): string {
  let best = 'system';
  let bestScore = -1;
  for (const [key, preset] of Object.entries(VIDEO_QUALITY_PRESETS)) {
    const withinCaps =
      preset.height <= entitlement.maxVideoHeight && preset.frameRate <= entitlement.maxVideoFps;
    if (!withinCaps) continue;
    const score = preset.height * preset.width * preset.frameRate;
    if (score > bestScore) {
      bestScore = score;
      best = key;
    }
  }
  return best;
}

/** Does the camera preset exceed the free video caps (height / fps / pixel-rate)? */
function cameraPresetExceedsCaps(cameraPreset: string, entitlement: Entitlement): boolean {
  const preset = VIDEO_QUALITY_PRESETS[cameraPreset];
  // Unknown / System Default presets never exceed — height/fps are 0 (driver decides).
  if (!preset) return false;
  const pixelRate = preset.width * preset.height * preset.frameRate;
  return (
    preset.height > entitlement.maxVideoHeight ||
    preset.frameRate > entitlement.maxVideoFps ||
    pixelRate > entitlement.maxVideoPixelRate
  );
}

/**
 * Pure free-tier clamp (#1301 Decision 4 / spec §4.2). Given the current client
 * settings + the (free) entitlement, return the clamped settings and whether
 * ANYTHING changed. Caller (useLaunchReset) writes the result back through the
 * real store setters only when `changed === true`, and surfaces the one-time
 * reset explainer.
 *
 * Clamp targets:
 *  - custom color scheme            → default scheme
 *  - premium audio tier             → 'standard'
 *  - camera preset over video caps  → highest free preset (height/fps/pixelRate)
 *  - manual screen-share bitrate    → entitlement.maxManualBitrateBps (0 = auto, never clamped)
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
  let changed = false;

  // 1. Custom color scheme → default (gated by allowCustomScheme).
  if (settings.colorScheme === 'custom' && !entitlement.allowCustomScheme) {
    next.colorScheme = DEFAULT_COLOR_SCHEME;
    changed = true;
  }

  // 2. Premium audio tier → 'standard'. A tier the entitlement no longer allows
  //    is clamped to the highest free tier (Standard).
  if (
    AUDIO_QUALITY_TIERS[settings.qualityTier]?.premium &&
    !entitlement.allowedAudioTiers.includes(settings.qualityTier)
  ) {
    next.qualityTier = FREE_AUDIO_TIER_FLOOR;
    changed = true;
  }

  // 3. Camera preset over the free video ceiling → highest free preset.
  if (cameraPresetExceedsCaps(settings.cameraPreset, entitlement)) {
    next.cameraPreset = highestFreeCameraPreset(entitlement);
    changed = true;
  }

  // 4. Manual screen-share bitrate over the free cap → cap. 0 (= auto) is never
  //    a manual value, so it is left untouched.
  if (
    settings.screenShareBitrate > 0 &&
    settings.screenShareBitrate > entitlement.maxManualBitrateBps
  ) {
    next.screenShareBitrate = entitlement.maxManualBitrateBps;
    changed = true;
  }

  // 5. Music mode → off (gated by allowMusicMode).
  if (settings.musicMode && !entitlement.allowMusicMode) {
    next.musicMode = false;
    changed = true;
  }

  return { settings: next, changed };
}
