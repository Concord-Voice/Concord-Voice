import React, { useState, useEffect, useMemo, useCallback } from 'react';
import { useVoiceStore } from '../../stores/voiceStore';
import {
  useVideoSettingsStore,
  VIDEO_QUALITY_PRESETS,
  type ScreenContentType,
  type DegradationPreference,
  type VideoPriority,
} from '../../stores/videoSettingsStore';
import {
  codecKey,
  codecKeyMime,
  getCodecInfo,
  type CodecCapability,
} from '../../services/mediaCapabilities';
import { humanizeProfileLabel, getCodecMetadata } from './codecMetadata';
import { useDraftVideoSetting, setDraftVideoSetting } from '../../hooks/useDraftSettings';
import { useEntitlement } from '../../hooks/useEntitlement';
import { useGateActivation } from '../../hooks/useGateActivation';
import { nativeExceedsFree } from '../../utils/nativeExceedsFree';
import PremiumChip from '../common/PremiumChip';
import ToggleSwitch from './ToggleSwitch';
import CollapsibleSection from './CollapsibleSection';
import CustomSelect from '../ui/CustomSelect';

// ─── GPU Vendor Icon ────────────────────────────────────────────────────────

const GpuVendorIcon: React.FC<{ vendor: string }> = ({ vendor }) => {
  const v = vendor.toLowerCase();
  // Apple
  if (v.includes('apple'))
    return (
      <svg
        width="13"
        height="16"
        viewBox="0 0 256 315"
        fill="none"
        style={{ display: 'block', flexShrink: 0 }}
      >
        <path
          d="M213.8 167.1c-.4-39.2 32-58.1 33.5-59.1-18.2-26.7-46.6-30.3-56.7-30.7-24.2-2.4-47.2 14.2-59.5 14.2-12.3 0-31.3-13.9-51.4-13.5-26.5.4-50.9 15.4-64.5 39.1-27.5 47.7-7 118.4 19.8 157.1 13.1 18.9 28.7 40.2 49.2 39.4 19.7-.8 27.2-12.8 51-12.8 23.9 0 30.6 12.8 51.5 12.4 21.2-.4 34.7-19.3 47.7-38.3 15.1-22 21.3-43.2 21.7-44.3-.5-.2-41.6-16-42.1-63.5zM175 64.2C185.8 51.1 193.1 33.4 191 15.8c-15.2.6-33.7 10.2-44.6 23-9.8 11.3-18.4 29.5-16.1 46.9 17 1.3 34.3-8.6 44.7-21.5z"
          fill="currentColor"
        />
      </svg>
    );
  // NVIDIA
  if (v.includes('nvidia'))
    return (
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ verticalAlign: 'middle' }}
      >
        <rect x="1" y="3" width="14" height="10" rx="2" fill="#76B900" />
        <text
          x="8"
          y="10.5"
          textAnchor="middle"
          fill="white"
          fontSize="7"
          fontWeight="bold"
          fontFamily="sans-serif"
        >
          N
        </text>
      </svg>
    );
  // Intel
  if (v.includes('intel'))
    return (
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ verticalAlign: 'middle' }}
      >
        <circle cx="8" cy="8" r="7" fill="#0071C5" />
        <text
          x="8"
          y="11"
          textAnchor="middle"
          fill="white"
          fontSize="8"
          fontWeight="bold"
          fontFamily="sans-serif"
        >
          i
        </text>
      </svg>
    );
  // AMD
  if (v.includes('amd'))
    return (
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ verticalAlign: 'middle' }}
      >
        <rect x="1" y="3" width="14" height="10" rx="2" fill="#ED1C24" />
        <text
          x="8"
          y="10.5"
          textAnchor="middle"
          fill="white"
          fontSize="6"
          fontWeight="bold"
          fontFamily="sans-serif"
        >
          AMD
        </text>
      </svg>
    );
  // Qualcomm
  if (v.includes('qualcomm'))
    return (
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ verticalAlign: 'middle' }}
      >
        <circle cx="8" cy="8" r="7" fill="#3253DC" />
        <text
          x="8"
          y="11"
          textAnchor="middle"
          fill="white"
          fontSize="8"
          fontWeight="bold"
          fontFamily="sans-serif"
        >
          Q
        </text>
      </svg>
    );
  // ARM
  if (v.includes('arm'))
    return (
      <svg
        width="16"
        height="16"
        viewBox="0 0 16 16"
        fill="none"
        style={{ verticalAlign: 'middle' }}
      >
        <rect x="1" y="3" width="14" height="10" rx="2" fill="#0091BD" />
        <text
          x="8"
          y="10.5"
          textAnchor="middle"
          fill="white"
          fontSize="6"
          fontWeight="bold"
          fontFamily="sans-serif"
        >
          ARM
        </text>
      </svg>
    );
  return null;
};

// ─── Pure bitrate / preset helpers (extracted to keep VideoConfigSection's ────
//     cognitive complexity ≤ 15; behaviour-preserving, unit-testable in isolation)

/** Codec substrings that signal an efficient (modern) codec for bitrate estimation. */
const EFFICIENT_CODEC_TOKENS = ['AV1', 'H265', 'HEVC', 'VP9'] as const;

/** Whether a single codec id/mime string names an efficient codec. */
function isEfficientCodec(codec: string): boolean {
  return EFFICIENT_CODEC_TOKENS.some((token) => codec.includes(token));
}

/** The free-tier video ceilings consumed by the L2 preset-lock helpers. */
interface FreeVideoCaps {
  maxVideoHeight: number;
  maxVideoFps: number;
  maxVideoPixelRate: number;
}

/** Resolve the pixel dimensions for a screen-share resolution selection. Accepts
 *  either a literal `WIDTHxHEIGHT` string or a named preset key. */
function resolveScreenResolution(
  screenResolution: string,
  bestDisplay: { width: number; height: number }
): { w: number; h: number } {
  const resMap: Record<string, { w: number; h: number }> = {
    '720p': { w: 1280, h: 720 },
    '1080p': { w: 1920, h: 1080 },
    '1440p': { w: 2560, h: 1440 },
    '4K': { w: 3840, h: 2160 },
    source: { w: bestDisplay.width, h: bestDisplay.height },
  };
  const parsed = /^(\d+)x(\d+)$/.exec(screenResolution);
  if (parsed) return { w: Number(parsed[1]), h: Number(parsed[2]) };
  return resMap[screenResolution] || resMap['1080p'];
}

/** Pure recommended-bitrate estimate (bytes-per-pixel heuristic). Efficient
 *  codecs use a lower bpp. Effective codec precedence: active (in-use) >
 *  preferred > auto-pick from capabilities. */
function computeRecommendedBitrate(args: {
  res: { w: number; h: number };
  effectiveFps: number;
  activeScreenCodec: string | null;
  preferredVideoCodec: string | null;
  codecCapabilities: CodecCapability[];
}): number {
  const { res, effectiveFps, activeScreenCodec, preferredVideoCodec, codecCapabilities } = args;
  const effectiveCodec = activeScreenCodec ?? preferredVideoCodec;
  const isEfficient = effectiveCodec
    ? isEfficientCodec(effectiveCodec)
    : codecCapabilities.some((c) => isEfficientCodec(c.mimeType));
  const bpp = isEfficient ? 0.04 : 0.07;
  const bps = res.w * res.h * effectiveFps * bpp;
  return Math.round(bps / 100_000) * 100_000;
}

/** Does a camera preset exceed any free video ceiling (height/fps/pixel-rate)?
 *  System Default (0/0) and unknown presets never exceed. Pure over caps. */
function presetExceedsFreeCaps(key: string, caps: FreeVideoCaps): boolean {
  const preset = VIDEO_QUALITY_PRESETS[key];
  if (!preset) return false;
  const pixelRate = preset.width * preset.height * preset.frameRate;
  return (
    preset.height > caps.maxVideoHeight ||
    preset.frameRate > caps.maxVideoFps ||
    pixelRate > caps.maxVideoPixelRate
  );
}

/** Highest free camera preset (largest pixel-count within caps), falling back to
 *  System Default. Pure over caps; mirrors clampToFreeTier's resolver. */
function resolveHighestFreeCameraPreset(caps: FreeVideoCaps): string {
  let best = 'system';
  let bestScore = -1;
  for (const [key, preset] of Object.entries(VIDEO_QUALITY_PRESETS)) {
    // Reuse the lock predicate so the snap-back can never land on a preset the
    // lock marks premium (e.g. 1080p60, which exceeds the free pixel-rate cap).
    if (presetExceedsFreeCaps(key, caps)) continue;
    const score = preset.height * preset.width * preset.frameRate;
    if (score > bestScore) {
      bestScore = score;
      best = key;
    }
  }
  return best;
}

/** A CustomSelect option row. */
interface SelectOption {
  value: string;
  label: string;
  group?: string;
}

/** Build the camera-preset select options, marking premium presets (above the
 *  free ceilings) with a 🔒 + "Premium" suffix (L2). Pure — extracted from the
 *  component body so the per-option ternary lives here, not in VideoConfigSection. */
function buildCameraPresetOptions(presetExceedsFree: (key: string) => boolean): SelectOption[] {
  return Object.entries(VIDEO_QUALITY_PRESETS).map(([key, preset]) => ({
    value: key,
    label: presetExceedsFree(key) ? `${preset.label} \u{1F512} Premium` : preset.label,
  }));
}

/** Build the screen-share Resolution select options, clamped to the free video
 *  ceiling (`clampedHeight`) per L6. Pure — the device-vs-cap `&&` ladder lives
 *  here, dropping it out of the component body's cognitive complexity. */
function buildResolutionOptions(args: {
  bestDisplayHeight: number;
  clampedHeight: number;
  uniqueDisplayResolutions: { width: number; height: number; isPrimary: boolean }[];
}): SelectOption[] {
  const { bestDisplayHeight, clampedHeight, uniqueDisplayResolutions } = args;
  const offer4K = bestDisplayHeight >= 2160 && clampedHeight >= 2160;
  const offer1440 = bestDisplayHeight >= 1440 && clampedHeight >= 1440;
  return [
    { value: 'source', label: 'Native', group: 'Common Resolutions' },
    ...(offer4K ? [{ value: '4K', label: '4K (3840×2160)', group: 'Common Resolutions' }] : []),
    ...(offer1440
      ? [{ value: '1440p', label: '1440p (2560×1440)', group: 'Common Resolutions' }]
      : []),
    { value: '1080p', label: '1080p (1920×1080)', group: 'Common Resolutions' },
    { value: '720p', label: '720p (1280×720)', group: 'Common Resolutions' },
    ...uniqueDisplayResolutions
      .filter((d) => d.height <= clampedHeight)
      .map((d) => ({
        value: `${d.width}x${d.height}`,
        label: `${d.width}×${d.height}${d.isPrimary ? ' (Primary)' : ''}`,
        group: 'Your Displays',
      })),
  ];
}

/** Build the screen-share Frame Rate select options, clamped to the free fps
 *  ceiling (`clampedFps`) per L6. Pure — the high-refresh `&&` ladder lives here. */
function buildFrameRateOptions(args: {
  maxRefreshRate: number;
  clampedFps: number;
}): SelectOption[] {
  const { maxRefreshRate, clampedFps } = args;
  const offer = (hz: number): boolean => maxRefreshRate >= hz && clampedFps >= hz;
  return [
    { value: '0', label: `Native (${maxRefreshRate} Hz)`, group: 'Common' },
    { value: '60', label: '60 FPS', group: 'Common' },
    { value: '30', label: '30 FPS', group: 'Common' },
    ...(offer(120) ? [{ value: '120', label: '120 FPS', group: 'Additional' }] : []),
    ...(offer(100) ? [{ value: '100', label: '100 FPS', group: 'Additional' }] : []),
    ...(offer(90) ? [{ value: '90', label: '90 FPS', group: 'Additional' }] : []),
    ...(offer(75) ? [{ value: '75', label: '75 FPS', group: 'Additional' }] : []),
    { value: '24', label: '24 FPS (Cinematic)', group: 'Additional' },
    { value: '15', label: '15 FPS', group: 'Additional' },
    { value: '5', label: '5 FPS (Slideshow)', group: 'Additional' },
  ];
}

// ─── Video Configuration Section ────────────────────────────────────────────

const VideoConfigSection: React.FC = () => {
  const activeCameraCodec = useVoiceStore((s) => s.activeCameraCodec);
  const activeScreenCodec = useVoiceStore((s) => s.activeScreenCodec);

  const codecCapabilities = useVideoSettingsStore((s) => s.codecCapabilities);
  const gpuInfo = useVideoSettingsStore((s) => s.gpuInfo);
  const videoAdvancedMode = useVideoSettingsStore((s) => s.videoAdvancedMode);
  const systemHdr = useVideoSettingsStore((s) => s.systemHdr);

  const cameraPreset = useDraftVideoSetting('cameraPreset');
  const screenResolution = useDraftVideoSetting('screenResolution');
  const screenFrameRate = useDraftVideoSetting('screenFrameRate');
  const screenContentType = useDraftVideoSetting('screenContentType');
  const preferredVideoCodec = useDraftVideoSetting('preferredVideoCodec');
  const screenSharePriority = useDraftVideoSetting('screenSharePriority');
  const screenShareBitrate = useDraftVideoSetting('screenShareBitrate');
  const cameraPriority = useDraftVideoSetting('cameraPriority');
  const degradationPreference = useDraftVideoSetting('degradationPreference');
  const hardwareAcceleration = useDraftVideoSetting('hardwareAcceleration');
  const hdrEncoding = useDraftVideoSetting('hdrEncoding');

  // Premium entitlement caps (#1301):
  //  - L2: camera-preset resolution/fps options above the free ceilings carry a
  //    🔒 marker and snap back to the highest free preset.
  //  - L5: the manual screen-share bitrate slider is fenced at the free cap.
  //  - L6: device-derived resolution/fps option lists are clamped to the free
  //    ceiling, with a "your device supports more" note when native exceeds it.
  const entitlement = useEntitlement((e) => e);
  const { maxVideoHeight, maxVideoFps, maxVideoPixelRate, maxManualBitrateBps } = entitlement;
  const cameraPresetGate = useGateActivation('video-quality');
  const bitrateGate = useGateActivation('manual-bitrate');
  const [cameraPresetLockHinted, setCameraPresetLockHinted] = useState(false);

  const [displayInfo, setDisplayInfo] = useState<
    {
      width: number;
      height: number;
      refreshRate: number;
      scaleFactor: number;
      isPrimary: boolean;
    }[]
  >([]);

  useEffect(() => {
    globalThis.electron?.getDisplayInfo?.().then((displays) => {
      if (displays) setDisplayInfo(displays);
    });
  }, []);

  // Display-derived values for screen share options
  const bestDisplay = useMemo(() => {
    if (displayInfo.length === 0) return { width: 1920, height: 1080, refreshRate: 60 };
    return displayInfo.reduce(
      (best, d) => (d.width * d.height > best.width * best.height ? d : best),
      displayInfo[0]
    );
  }, [displayInfo]);

  const maxRefreshRate = useMemo(() => {
    if (displayInfo.length === 0) return 60;
    return Math.round(Math.max(...displayInfo.map((d) => d.refreshRate || 60)));
  }, [displayInfo]);

  // Unique display resolutions for the "Your Displays" optgroup
  const uniqueDisplayResolutions = useMemo(() => {
    const seen = new Set<string>();
    return displayInfo
      .map((d) => ({ width: d.width, height: d.height, isPrimary: d.isPrimary }))
      .filter((d) => {
        const key = `${d.width}x${d.height}`;
        if (seen.has(key)) return false;
        seen.add(key);
        return true;
      })
      .sort((a, b) => b.width * b.height - a.width * a.height);
  }, [displayInfo]);

  // Dynamic bitrate recommendation based on resolution, FPS, and codec.
  // The estimation logic lives in pure module helpers; this memo only wires the
  // current draft state into them (keeps the component's cognitive complexity low).
  const recommendedBitrate = useMemo(() => {
    const res = resolveScreenResolution(screenResolution, bestDisplay);
    const effectiveFps = screenFrameRate === 0 ? maxRefreshRate : screenFrameRate;
    return computeRecommendedBitrate({
      res,
      effectiveFps,
      activeScreenCodec,
      preferredVideoCodec,
      codecCapabilities,
    });
  }, [
    screenResolution,
    screenFrameRate,
    preferredVideoCodec,
    activeScreenCodec,
    codecCapabilities,
    bestDisplay,
    maxRefreshRate,
  ]);

  const clampedRecommended = Math.max(1_500_000, Math.min(30_000_000, recommendedBitrate));

  // ── L2: camera-preset resolution/fps lock ──────────────────────────────
  // The free caps drive both the lock predicate and the snap-back resolver; the
  // pure helpers (presetExceedsFreeCaps / resolveHighestFreeCameraPreset) close
  // over this object so the component body stays flat.
  const freeVideoCaps = useMemo<FreeVideoCaps>(
    () => ({ maxVideoHeight, maxVideoFps, maxVideoPixelRate }),
    [maxVideoHeight, maxVideoFps, maxVideoPixelRate]
  );

  const presetExceedsFree = useCallback(
    (key: string): boolean => presetExceedsFreeCaps(key, freeVideoCaps),
    [freeVideoCaps]
  );

  const handleCameraPresetChange = useCallback(
    (key: string): void => {
      if (presetExceedsFreeCaps(key, freeVideoCaps)) {
        // L2 snap-back: a locked preset never reaches the store. Clamp to the
        // highest free preset and reveal the chip (no mid-action modal).
        setDraftVideoSetting('cameraPreset', resolveHighestFreeCameraPreset(freeVideoCaps));
        setCameraPresetLockHinted(true);
        return;
      }
      setCameraPresetLockHinted(false);
      setDraftVideoSetting('cameraPreset', key);
    },
    [freeVideoCaps]
  );

  // ── L6: device-derived resolution / frame-rate native-exceeds guard ─────
  // The screen-share Resolution + Frame Rate option lists are derived from the
  // detected display. Clamp the offered ceiling to the free caps and surface a
  // "your device supports more" note when the device genuinely exceeds free.
  const nativeGuard = nativeExceedsFree(
    { nativeHeight: bestDisplay.height, nativeFps: maxRefreshRate },
    entitlement
  );

  // ── L5: manual screen-share bitrate clamp ──────────────────────────────
  const FREE_BITRATE_CAP_MBPS = maxManualBitrateBps / 1_000_000; // 5.0 for free
  const ABSOLUTE_BITRATE_MAX_MBPS = 30;
  // The slider's effective ceiling: the free cap when not entitled (i.e. when
  // the free cap is below the absolute max), else the absolute max.
  const bitrateSliderMaxMbps = Math.min(FREE_BITRATE_CAP_MBPS, ABSOLUTE_BITRATE_MAX_MBPS);
  const bitrateIsCapped = FREE_BITRATE_CAP_MBPS < ABSOLUTE_BITRATE_MAX_MBPS;
  // When toggling automatic OFF, seed the manual cap at the recommended value
  // but never above the free ceiling (so a free user starts within range).
  const initialManualBitrate = Math.min(clampedRecommended, maxManualBitrateBps);

  return (
    <CollapsibleSection id="section-video-screen" title="Video Configuration">
      <div className="settings-mode-toggle" role="tablist">
        <span
          className={`settings-mode-pill ${videoAdvancedMode ? '' : 'active'}`}
          role="tab"
          tabIndex={0}
          aria-selected={!videoAdvancedMode}
          onClick={() => useVideoSettingsStore.getState().setVideoAdvancedMode(false)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              useVideoSettingsStore.getState().setVideoAdvancedMode(false);
            }
          }}
        >
          Basic Settings
        </span>
        <span
          className={`settings-mode-pill ${videoAdvancedMode ? 'active' : ''}`}
          role="tab"
          tabIndex={0}
          aria-selected={videoAdvancedMode}
          onClick={() => useVideoSettingsStore.getState().setVideoAdvancedMode(true)}
          onKeyDown={(e) => {
            if (e.key === 'Enter' || e.key === ' ') {
              e.preventDefault();
              useVideoSettingsStore.getState().setVideoAdvancedMode(true);
            }
          }}
        >
          Advanced Settings
        </span>
      </div>

      {/* ── Camera ── */}
      <h3 className="settings-subsection-title">Camera</h3>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Camera Preset</span>
          <span className="settings-row-hint">
            {cameraPreset === 'system'
              ? "Constrains the camera's capture resolution and frame rate. Currently System Default \u2014 the camera and driver decide automatically."
              : `Constrains the camera's capture resolution and frame rate. Currently requesting ${VIDEO_QUALITY_PRESETS[cameraPreset]?.label ?? cameraPreset}.`}
          </span>
          {cameraPresetLockHinted && (
            <span className="settings-row-premium-note">
              <PremiumChip
                label="higher resolutions"
                onActivate={cameraPresetGate.onActivate}
                id={cameraPresetGate.describedById}
              />
            </span>
          )}
        </div>
        <CustomSelect
          className="settings-select"
          // L2: premium presets (above the free height/fps/pixel-rate ceilings)
          // carry a trailing \ud83d\udd12 + "Premium" marker; the select stays usable and
          // selecting one snaps back to the highest free preset. The per-option
          // marker logic lives in the pure buildCameraPresetOptions helper.
          options={buildCameraPresetOptions(presetExceedsFree)}
          value={cameraPreset}
          onChange={handleCameraPresetChange}
        />
      </div>

      {/* ── Screen Share ── */}
      <h3 className="settings-subsection-title">Screen Share</h3>
      <p className="settings-section-description">
        Default settings for screen sharing. These can be changed per-share in the picker.
      </p>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Resolution</span>
          <span className="settings-row-hint">
            {screenResolution === 'source'
              ? "Default capture resolution for screen sharing. Currently Native \u2014 captures at your display's full resolution."
              : `Default capture resolution for screen sharing. Currently ${screenResolution}.`}
          </span>
          {nativeGuard.exceeds && (
            <span className="settings-row-premium-note">
              <PremiumChip label="your device supports more" />
              <span className="settings-native-exceeds-note">
                Your device supports more \u2014 unlock with Premium
              </span>
            </span>
          )}
        </div>
        <CustomSelect
          className="settings-select"
          // L6: device-derived resolution options are clamped to the free video
          // ceiling (nativeGuard.clampedHeight) \u2014 premium resolutions above the
          // free cap are not offered. The "supports more" note explains why. The
          // device-vs-cap option ladder lives in the pure buildResolutionOptions
          // helper (keeps the component body's cognitive complexity \u2264 15).
          options={buildResolutionOptions({
            bestDisplayHeight: bestDisplay.height,
            clampedHeight: nativeGuard.clampedHeight,
            uniqueDisplayResolutions,
          })}
          value={screenResolution}
          onChange={(v) => setDraftVideoSetting('screenResolution', v)}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Frame Rate</span>
          <span className="settings-row-hint">
            {screenFrameRate === 0
              ? `Default capture frame rate for screen sharing. Currently Native (${maxRefreshRate} Hz).`
              : `Default capture frame rate for screen sharing. Currently ${screenFrameRate} FPS.`}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          // L6: high-refresh options are clamped to the free fps ceiling
          // (nativeGuard.clampedFps) — frame rates above the free cap are not
          // offered. The standard 60/30 and slow options always remain. The
          // high-refresh option ladder lives in the pure buildFrameRateOptions helper.
          options={buildFrameRateOptions({
            maxRefreshRate,
            clampedFps: nativeGuard.clampedFps,
          })}
          value={String(screenFrameRate)}
          onChange={(v) => setDraftVideoSetting('screenFrameRate', Number(v))}
        />
      </div>

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Content Type</span>
          <span className="settings-row-hint">
            {(() => {
              if (screenContentType === 'auto')
                return 'Optimizes encoding for the type of content being shared. Currently Auto \u2014 Concord Voice detects motion vs. static content and adjusts accordingly.';
              if (screenContentType === 'motion')
                return 'Optimizes encoding for the type of content being shared. Currently Motion \u2014 prioritizes smooth frame rate for video and animations.';
              return 'Optimizes encoding for the type of content being shared. Currently Detail \u2014 prioritizes sharp text and edges for code and documents.';
            })()}
          </span>
        </div>
        <CustomSelect
          className="settings-select"
          options={[
            { value: 'auto', label: 'Auto' },
            { value: 'motion', label: 'Motion (Video & Animation)' },
            { value: 'detail', label: 'Detail (Text & Code)' },
          ]}
          value={screenContentType}
          onChange={(v) => setDraftVideoSetting('screenContentType', v as ScreenContentType)}
        />
      </div>

      {/* ── Advanced Video Settings ── */}
      {videoAdvancedMode && (
        <>
          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Congestion Priority</span>
              <span className="settings-row-hint">
                {(() => {
                  if (degradationPreference === 'balanced')
                    return 'How Concord adapts your camera during network congestion. Currently Balanced \u2014 Concord decides whether to reduce resolution or framerate.';
                  if (degradationPreference === 'maintain-framerate')
                    return 'How Concord adapts your camera during network congestion. Currently Prefer Smooth Video \u2014 keeps framerate by reducing resolution.';
                  return 'How Concord adapts your camera during network congestion. Currently Prefer Sharp Details \u2014 keeps resolution by reducing framerate.';
                })()}
              </span>
            </div>
            <CustomSelect
              className="settings-select"
              options={[
                { value: 'balanced', label: 'Balanced' },
                { value: 'maintain-framerate', label: 'Prefer Smooth Video' },
                { value: 'maintain-resolution', label: 'Prefer Sharp Details' },
              ]}
              value={degradationPreference}
              onChange={(v) =>
                setDraftVideoSetting('degradationPreference', v as DegradationPreference)
              }
            />
          </div>

          <h3 className="settings-subsection-title">Codec & Hardware</h3>

          {gpuInfo && (
            <p className="settings-section-description" style={{ marginTop: 0, marginBottom: 6 }}>
              <span className="settings-gpu-badge">
                <GpuVendorIcon vendor={gpuInfo.vendor} /> {gpuInfo.vendor}
                {gpuInfo.device ? ` \u00b7 ${gpuInfo.device}` : ''}
              </span>
            </p>
          )}

          {codecCapabilities.length > 0 &&
            (() => {
              // Sort order matches codec cascade: AV1 → HEVC → H264 → VP9 → VP8
              const codecPriority: Record<string, number> = {
                AV1: 0,
                H265: 1,
                HEVC: 1,
                H264: 2,
                VP9: 3,
                VP8: 4,
              };
              const codecDisplayName: Record<string, string> = {
                H264: 'AVC (H.264)',
                H265: 'HEVC (H.265)',
                HEVC: 'HEVC (H.265)',
              };
              // Codecs the mediasoup router supports (H265 not yet supported)
              const routerSupported = new Set([
                'video/vp8',
                'video/vp9',
                'video/h264',
                'video/av1',
              ]);
              const sortByPriority = (a: CodecCapability, b: CodecCapability) =>
                (codecPriority[a.mimeType.replace('video/', '')] ?? 99) -
                (codecPriority[b.mimeType.replace('video/', '')] ?? 99);
              const humanProfile = (c: CodecCapability) =>
                humanizeProfileLabel(c.profileId, c.profileLabel);
              const displayName = (c: CodecCapability) => {
                const raw = c.mimeType.replace('video/', '');
                const base = codecDisplayName[raw] ?? raw;
                const profile = humanProfile(c);
                return profile ? `${base} (${profile})` : base;
              };
              const isSupported = (c: CodecCapability) =>
                routerSupported.has(c.mimeType.toLowerCase());

              // Dedupe entries that resolve to the same (codec, human profile) pair.
              // Raw profile-level-id hex strings that collapse to the same label
              // (e.g. "42001f" and an already-labeled "Constrained Baseline 3.1")
              // must not appear twice.
              const dedupe = (list: CodecCapability[]): CodecCapability[] => {
                const seen = new Set<string>();
                const out: CodecCapability[] = [];
                for (const c of list) {
                  const key = `${c.mimeType.toLowerCase()}|${humanProfile(c) ?? ''}`;
                  if (seen.has(key)) continue;
                  seen.add(key);
                  out.push(c);
                }
                return out;
              };

              // HW column: codecs with GPU acceleration
              // SW column: ALL codecs (every codec has a software encoder fallback)
              const hwCodecs = dedupe(
                codecCapabilities.filter((c) => c.powerEfficient).sort(sortByPriority)
              );
              const swCodecs = dedupe([...codecCapabilities].sort(sortByPriority));

              // Which column is active
              const hwHasSupported = hwCodecs.some((c) => isSupported(c));
              const preferredMime = preferredVideoCodec ? codecKeyMime(preferredVideoCodec) : null;
              const hwActive = preferredMime
                ? // User picked a specific codec → active column is whichever contains its mimeType
                  hardwareAcceleration &&
                  hwCodecs.some(
                    (c) =>
                      c.mimeType.toLowerCase() === preferredMime.toLowerCase() && isSupported(c)
                  )
                : hardwareAcceleration && hwHasSupported;

              // Determine preferred codec key for green highlight
              const activeColumnCodecs = hwActive ? hwCodecs : swCodecs;
              const preferredKey =
                preferredVideoCodec &&
                routerSupported.has(codecKeyMime(preferredVideoCodec).toLowerCase())
                  ? preferredVideoCodec
                  : (() => {
                      const first = activeColumnCodecs.find((c) => isSupported(c));
                      return first ? codecKey(first) : null;
                    })();

              // Active codecs currently in use (from voice producers — now profile-aware keys)
              const inUseKeys = new Set<string>(
                [activeCameraCodec, activeScreenCodec]
                  .filter(Boolean)
                  .map((k) => (k as string).toLowerCase())
              );

              const renderCodecItem = (
                c: CodecCapability,
                columnKey: string,
                isActiveColumn: boolean
              ) => {
                const supported = isSupported(c);
                const cKey = codecKey(c);
                const isPreferred = isActiveColumn && supported && cKey === preferredKey;
                const isInUse =
                  supported &&
                  (inUseKeys.has(cKey.toLowerCase()) || inUseKeys.has(c.mimeType.toLowerCase()));
                let tooltip: string;
                if (!supported) tooltip = 'Not supported';
                else if (isPreferred && isInUse) tooltip = 'Preferred \u00b7 In Use';
                else if (isInUse) tooltip = 'In Use';
                else if (isPreferred) tooltip = 'Preferred';
                else tooltip = 'Available';
                return (
                  <div
                    key={`${columnKey}-${cKey}`}
                    className={`settings-codec-item${isPreferred ? ' preferred' : ''}${isInUse ? ' in-use' : ''}${supported ? '' : ' unsupported'}`}
                    data-tooltip={tooltip}
                  >
                    <span className={`settings-codec-name${supported ? '' : ' strikethrough'}`}>
                      {displayName(c)}
                    </span>
                  </div>
                );
              };

              return (
                <>
                  <div className="settings-codec-grid">
                    <div className={`settings-codec-column${hwActive ? ' active' : ''}`}>
                      <span
                        className={`settings-codec-column-header${hwActive ? ' active' : ''}`}
                        {...(hwActive ? { 'data-tooltip': 'Preferred' } : {})}
                      >
                        Hardware
                      </span>
                      <div className="settings-codec-column-items">
                        {hwCodecs.map((c) => renderCodecItem(c, 'hw', hwActive))}
                        {hwCodecs.length === 0 && (
                          <span className="settings-codec-empty">None detected</span>
                        )}
                      </div>
                    </div>
                    <div className={`settings-codec-column${hwActive ? '' : ' active'}`}>
                      <span
                        className={`settings-codec-column-header${hwActive ? '' : ' active'}`}
                        {...(hwActive ? {} : { 'data-tooltip': 'Preferred' })}
                      >
                        Software
                      </span>
                      <div className="settings-codec-column-items">
                        {swCodecs.map((c) => renderCodecItem(c, 'sw', !hwActive))}
                        {swCodecs.length === 0 && (
                          <span className="settings-codec-empty">None detected</span>
                        )}
                      </div>
                    </div>
                  </div>
                  {hardwareAcceleration && !hwHasSupported && (
                    <div className="settings-hw-fallback-notice">
                      Hardware acceleration is enabled, but none of your GPU&apos;s codecs are
                      currently supported. Concord Voice has automatically failed over to software
                      encoding.
                    </div>
                  )}
                </>
              );
            })()}

          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Enable HDR Encoding</span>
              <span className="settings-row-hint">
                {(() => {
                  if (!systemHdr)
                    return 'No HDR display detected. Connect an HDR-capable display to enable.';
                  if (hdrEncoding)
                    return 'Enabled. HDR codec profiles (VP9 Profile 2) and HDR-capable variants of AV1 and H.264 High will be used when available.';
                  return 'Disabled. SDR codec profiles are preferred. HDR-only profiles such as VP9 Profile 2 will not be selected.';
                })()}
              </span>
            </div>
            <ToggleSwitch
              checked={hdrEncoding}
              onChange={(v) => setDraftVideoSetting('hdrEncoding', v)}
              disabled={!systemHdr}
            />
          </div>

          {(() => {
            const routerSupported = new Set(['video/vp8', 'video/vp9', 'video/h264', 'video/av1']);
            // Sort order matches codec cascade: AV1 → HEVC → H264 → VP9 → VP8
            const codecPriority: Record<string, number> = {
              AV1: 0,
              H265: 1,
              HEVC: 1,
              H264: 2,
              VP9: 3,
              VP8: 4,
            };
            const sortByPriority = (a: CodecCapability, b: CodecCapability) =>
              (codecPriority[a.mimeType.replace('video/', '')] ?? 99) -
              (codecPriority[b.mimeType.replace('video/', '')] ?? 99);
            const supported = codecCapabilities.filter((c) =>
              routerSupported.has(c.mimeType.toLowerCase())
            );
            const sdrCodecs = supported.filter((c) => !c.isHdr).sort(sortByPriority);
            const hdrCodecs = supported.filter((c) => c.isHdr).sort(sortByPriority);

            // Resolve effective codec key for info badge
            const firstSupported = sdrCodecs[0] || supported[0];
            const effectiveKey =
              preferredVideoCodec || (firstSupported ? codecKey(firstSupported) : null);
            const info = effectiveKey ? getCodecInfo(effectiveKey) : null;

            return (
              <>
                <div className="settings-row">
                  <div className="settings-row-info">
                    <span className="settings-row-label">Video Codec</span>
                    <span className="settings-row-hint">
                      {preferredVideoCodec
                        ? `Preferred codec for camera and screen share encoding. Currently ${getCodecInfo(preferredVideoCodec).name}. Concord falls back automatically if a peer cannot decode it.`
                        : 'Preferred codec for camera and screen share encoding. Currently Auto \u2014 Concord selects the best available based on hardware and peer support.'}
                    </span>
                  </div>
                  <CustomSelect
                    className="settings-select"
                    options={[
                      { value: '', label: 'Auto' },
                      ...sdrCodecs.map((c) => {
                        const key = codecKey(c);
                        const name = c.mimeType.replace('video/', '');
                        const profile = humanizeProfileLabel(c.profileId, c.profileLabel);
                        return {
                          value: key,
                          label: profile ? `${name} (${profile})` : name,
                        };
                      }),
                      ...hdrCodecs.map((c) => {
                        const key = codecKey(c);
                        const name = c.mimeType.replace('video/', '');
                        const profile = humanizeProfileLabel(c.profileId, c.profileLabel);
                        return {
                          value: hdrEncoding ? key : `__disabled_${key}`,
                          label: profile ? `${name} (${profile})` : name,
                          group: hdrEncoding ? 'HDR' : 'Requires HDR',
                        };
                      }),
                    ]}
                    value={preferredVideoCodec ?? ''}
                    onChange={(v) =>
                      setDraftVideoSetting(
                        'preferredVideoCodec',
                        v.startsWith('__disabled_') ? preferredVideoCodec : v || null
                      )
                    }
                  />
                </div>

                {info &&
                  (() => {
                    const meta = effectiveKey ? getCodecMetadata(effectiveKey) : null;
                    const quality = meta?.quality ?? info.quality;
                    const efficiency = meta?.efficiency ?? info.efficiency;
                    const compression = meta?.compression ?? info.compressionRatio;
                    const hdrCapable = meta ? meta.hdrCapable : info.hdr;
                    const description = meta?.description ?? info.notes;
                    return (
                      <dl className="settings-codec-info-badge settings-codec-meta">
                        <div className="settings-codec-meta-row">
                          <dt>Quality:</dt>
                          <dd>{quality}</dd>
                        </div>
                        <div className="settings-codec-meta-row">
                          <dt>Efficiency:</dt>
                          <dd>
                            {efficiency} <em>({compression})</em>
                          </dd>
                        </div>
                        <div className="settings-codec-meta-row">
                          <dt>HDR Capable:</dt>
                          <dd>{hdrCapable ? 'Yes' : 'No'}</dd>
                        </div>
                        <div className="settings-codec-meta-row">
                          <dt>Description:</dt>
                          <dd>{description}</dd>
                        </div>
                      </dl>
                    );
                  })()}

                {preferredVideoCodec && (
                  <div className="settings-codec-preference-notice">
                    Your client will prefer this codec, but will fall back to the next best option
                    if another participant can&apos;t decode it. Active codecs are shown above with
                    a{' '}
                    <span
                      className="settings-codec-item in-use"
                      style={{
                        display: 'inline-flex',
                        padding: '1px 6px',
                        margin: '0 3px',
                        fontSize: 'inherit',
                        verticalAlign: 'baseline',
                      }}
                    >
                      highlight
                    </span>{' '}
                    .
                  </div>
                )}
              </>
            );
          })()}

          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Hardware Acceleration</span>
              <span className="settings-row-hint">
                {hardwareAcceleration
                  ? 'Enabled. Your GPU handles video encoding and decoding. Concord Voice falls back to software encoding for unsupported codecs.'
                  : 'Disabled. All video encoding and decoding runs on your CPU. Avoids GPU-specific limitations but increases CPU usage. Requires restart.'}
              </span>
            </div>
            <ToggleSwitch
              checked={hardwareAcceleration}
              onChange={(enabled) => setDraftVideoSetting('hardwareAcceleration', enabled)}
            />
          </div>

          <h3 className="settings-subsection-title">Bandwidth</h3>

          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Automatic Bitrate</span>
              <span className="settings-row-hint">
                {screenShareBitrate === 0
                  ? 'Enabled. Concord Voice adjusts bitrate based on your resolution, frame rate, and codec.'
                  : `Disabled. Using a fixed ${(screenShareBitrate / 1_000_000).toFixed(1)} Mbps cap. Recommended ~${(clampedRecommended / 1_000_000).toFixed(1)} Mbps for current settings.`}
              </span>
              {screenShareBitrate === 0 && (
                <span className="settings-estimated-bitrate">
                  Estimated Bitrate: ~{(clampedRecommended / 1_000_000).toFixed(1)} Mbps
                </span>
              )}
            </div>
            <ToggleSwitch
              checked={screenShareBitrate === 0}
              onChange={(v) =>
                // L5: when disabling automatic, seed the manual cap within the
                // free ceiling so a free user starts inside the live range.
                setDraftVideoSetting('screenShareBitrate', v ? 0 : initialManualBitrate)
              }
            />
          </div>
          {screenShareBitrate !== 0 && (
            <div className="settings-volume-row">
              <div className="settings-row-info">
                <span className="settings-volume-label">Cap</span>
                <span className="settings-row-hint">
                  Sets the maximum bitrate cap for screen sharing. Left (1.5 Mbps) for simple
                  content. Right ({bitrateSliderMaxMbps} Mbps) for high-motion content.
                </span>
              </div>
              <div className="settings-slider-wrapper">
                <span className="settings-slider-value">
                  {(screenShareBitrate / 1_000_000).toFixed(1)} Mbps
                </span>
                {/* L5: the slider stays LIVE up to the free cap. When capped, a
                    decorative ghost-zone past the thumb advertises the premium
                    range; the slider's `max` fences the live value at the cap. */}
                <div className={`settings-bitrate-slider-wrap${bitrateIsCapped ? ' capped' : ''}`}>
                  <input
                    type="range"
                    className="settings-volume-slider"
                    min={1.5}
                    max={bitrateSliderMaxMbps}
                    step={0.5}
                    value={Math.min(screenShareBitrate / 1_000_000, bitrateSliderMaxMbps)}
                    onChange={(e) =>
                      setDraftVideoSetting(
                        'screenShareBitrate',
                        Math.round(
                          Math.min(Number(e.target.value), bitrateSliderMaxMbps) * 1_000_000
                        )
                      )
                    }
                  />
                  {bitrateIsCapped && (
                    <button
                      type="button"
                      className="settings-bitrate-ghost-zone"
                      onClick={bitrateGate.onActivate}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') bitrateGate.onActivate(e);
                      }}
                    >
                      <span aria-hidden="true" className="settings-bitrate-ghost-gradient" />
                      <PremiumChip
                        label={`beyond ${bitrateSliderMaxMbps} Mbps →`}
                        id={bitrateGate.describedById}
                      />
                    </button>
                  )}
                </div>
              </div>
            </div>
          )}

          <h3 className="settings-subsection-title">Transport</h3>

          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Quality of Service for Camera</span>
              <span className="settings-row-hint">
                {
                  'Marks camera traffic with DSCP tags so your network can prioritize it. Not all networks honor DSCP tags; some may ignore or strip them, so this setting may have no effect depending on you or your ISP\u2019s network configurations. '
                }
                {(() => {
                  if (cameraPriority === 'off') return 'Currently off \u2014 no tagging applied.';
                  if (cameraPriority === 'low')
                    return 'Currently Low (DF) \u2014 minimal differentiation. (RFC 2474)';
                  if (cameraPriority === 'medium')
                    return 'Currently Default (AF43) \u2014 recommended for most networks. (RFC 2597)';
                  return 'Currently High (EF) \u2014 highest priority. (RFC 5127)';
                })()}
              </span>
            </div>
            <CustomSelect
              className="settings-select"
              options={[
                { value: 'off', label: 'Off (No Tagging)' },
                { value: 'low', label: 'Low (DF)' },
                { value: 'medium', label: 'Default (AF43)' },
                { value: 'high', label: 'High (EF)' },
              ]}
              value={cameraPriority}
              onChange={(v) => setDraftVideoSetting('cameraPriority', v as VideoPriority)}
            />
          </div>

          <div className="settings-row">
            <div className="settings-row-info">
              <span className="settings-row-label">Quality of Service for Screen Share</span>
              <span className="settings-row-hint">
                {
                  'Marks screen share traffic with DSCP tags so your network can prioritize it. Not all networks honor DSCP tags; some may ignore or strip them, so this setting may have no effect depending on you or your ISP\u2019s network configurations. '
                }
                {(() => {
                  if (screenSharePriority === 'off')
                    return 'Currently off \u2014 no tagging applied.';
                  if (screenSharePriority === 'low')
                    return 'Currently Low (DF) \u2014 minimal differentiation. (RFC 2474)';
                  if (screenSharePriority === 'medium')
                    return 'Currently Default (AF42) \u2014 recommended for most networks. (RFC 2597)';
                  return 'Currently High (EF) \u2014 highest priority. (RFC 5127)';
                })()}
              </span>
            </div>
            <CustomSelect
              className="settings-select"
              options={[
                { value: 'off', label: 'Off (No Tagging)' },
                { value: 'low', label: 'Low (DF)' },
                { value: 'medium', label: 'Default (AF42)' },
                { value: 'high', label: 'High (EF)' },
              ]}
              value={screenSharePriority}
              onChange={(v) => setDraftVideoSetting('screenSharePriority', v as VideoPriority)}
            />
          </div>

          {/* Layer mode is not exposed as a manual screen-share control here;
              camera layering is negotiated by the media-plane gate. The
              scalabilityMode store value is preserved for codec planning. */}
        </>
      )}
    </CollapsibleSection>
  );
};

export default VideoConfigSection;
