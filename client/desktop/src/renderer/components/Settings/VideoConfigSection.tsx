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
import { codecPriority, selectCodecFromCascade } from '../../services/voiceCodecSelection';
import {
  humanizeProfileLabel,
  getCodecMetadata,
  isRouterSupportedCodecProfile,
  codecProfileMenuLabel,
  canonicalRouterCodecKey,
} from './codecMetadata';
import { castingCopy } from './castingCopy';
import { useDraftVideoSetting, setDraftVideoSetting } from '../../hooks/useDraftSettings';
import { useEntitlement } from '../../hooks/useEntitlement';
import { useSubscriptionStore } from '../../stores/subscriptionStore';
import { useGateActivation } from '../../hooks/useGateActivation';
import { nativeExceedsFree } from '../../utils/nativeExceedsFree';
import {
  videoLimitsFromEntitlement,
  clampScreenCapture,
  effectiveStreamAxis,
  shouldEnforceForSubscription,
  type VideoAxisLimit,
} from '../../utils/videoLimits';
import { SCREEN_RES_DIMS } from '../../utils/screenResolution';
import PremiumChip from '../common/PremiumChip';
import ToggleSwitch from './ToggleSwitch';
import CollapsibleSection from './CollapsibleSection';
import CustomSelect from '../ui/CustomSelect';
import CodecProfilesModal from './CodecProfilesModal';

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

/** Resolve the pixel dimensions for a screen-share resolution selection. Accepts
 *  either a literal `WIDTHxHEIGHT` string or a named preset key. */
function resolveScreenResolution(
  screenResolution: string,
  bestDisplay: { width: number; height: number }
): { w: number; h: number } {
  const resMap: Record<string, { w: number; h: number }> = {
    ...SCREEN_RES_DIMS,
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
  targetCodec: string | null;
}): number {
  const { res, effectiveFps, activeScreenCodec, targetCodec } = args;
  const effectiveCodec = activeScreenCodec ?? targetCodec;
  const isEfficient = effectiveCodec ? isEfficientCodec(effectiveCodec.toUpperCase()) : false;
  const bpp = isEfficient ? 0.04 : 0.07;
  const bps = res.w * res.h * effectiveFps * bpp;
  return Math.round(bps / 100_000) * 100_000;
}

function canonicalRequestedCodecKey(
  key: string,
  capabilities: CodecCapability[] = [],
  hdrEncoding = false
): string | null {
  const [mime, requestedProfile] = key.toLowerCase().split(':');
  // Migrate legacy family-only H.264 preferences to the best locally available
  // router-supported profile. Keep High as the pre-detection fallback.
  if (mime === 'video/h264' && requestedProfile === undefined) {
    const available = capabilities
      .map((capability) => canonicalRouterCodecKey(capability.mimeType, capability.profileId))
      .filter((candidate): candidate is string => candidate?.startsWith('video/h264:') === true)
      .sort((left, right) => codecPriority(left, hdrEncoding) - codecPriority(right, hdrEncoding));
    return available[0] ?? canonicalRouterCodecKey(mime, '640034');
  }
  return canonicalRouterCodecKey(mime, requestedProfile);
}

function capabilityMatchesKey(capability: CodecCapability, key: string): boolean {
  const requestedKey = canonicalRequestedCodecKey(key);
  return (
    requestedKey !== null &&
    canonicalRouterCodecKey(capability.mimeType, capability.profileId) === requestedKey
  );
}

function isCapabilityHardware(
  capability: CodecCapability,
  webrtcHwByMime: Record<string, boolean>
): boolean {
  const mime = capability.mimeType.toLowerCase();
  return webrtcHwByMime[mime] ?? capability.hwAvailable === true;
}

/** Hardware-column membership answers whether the machine can hardware encode.
 * A learned positive WebRTC signal can add a missed capability, while a learned
 * software path must not erase a positive WebCodecs hardware capability. */
function isHardwareEncodingAvailable(
  capability: CodecCapability,
  webrtcHwByMime: Record<string, boolean>
): boolean {
  return (
    capability.hwAvailable === true || webrtcHwByMime[capability.mimeType.toLowerCase()] === true
  );
}

function resolveCodecTarget(args: {
  capabilities: CodecCapability[];
  preferred: string | null;
  hardwareAcceleration: boolean;
  hdrEncoding: boolean;
  codecFloor: string[] | null;
  webrtcHwByMime: Record<string, boolean>;
}): CodecCapability | undefined {
  const supported = args.capabilities.filter((capability) =>
    isRouterSupportedCodecProfile(capability.mimeType, capability.profileId)
  );
  const matching = (key: string) =>
    supported.filter((capability) => capabilityMatchesKey(capability, key));
  const find = (key: string) => {
    const candidates = matching(key);
    if (args.hardwareAcceleration) {
      return (
        candidates.find((capability) => isCapabilityHardware(capability, args.webrtcHwByMime)) ??
        candidates[0]
      );
    }
    return candidates[0];
  };
  const canonicalPreferred = args.preferred
    ? canonicalRequestedCodecKey(args.preferred, args.capabilities, args.hdrEncoding)
    : null;
  const preferred = canonicalPreferred && find(canonicalPreferred) ? canonicalPreferred : null;

  return selectCodecFromCascade<CodecCapability>({
    preferred,
    hwAccel: args.hardwareAcceleration,
    hdrEncoding: args.hdrEncoding,
    isInCodecFloor: (key) =>
      !args.codecFloor || args.codecFloor.includes(codecKeyMime(key).toLowerCase()),
    isHwAccelerated: (key) =>
      matching(key).some((capability) => isCapabilityHardware(capability, args.webrtcHwByMime)),
    findSendCodec: find,
  });
}

function activeCodecMatches(capability: CodecCapability, activeKey: string | null): boolean {
  if (!activeKey) return false;
  const normalized = activeKey.toLowerCase();
  if (normalized === codecKey(capability).toLowerCase()) return true;

  // A family-only H.264 runtime key cannot truthfully identify a profile.
  if (normalized === 'video/h264') return false;

  // The grid deduplicates compatible H.264 levels into one canonical profile
  // row, so bridge level-asymmetric runtime keys to that surviving row. The
  // same normalization maps the legacy bare VP9 key to Profile 0.
  const activeCanonical = canonicalRequestedCodecKey(normalized);
  return (
    activeCanonical !== null &&
    canonicalRouterCodecKey(capability.mimeType, capability.profileId) === activeCanonical
  );
}

type CodecColumnKey = 'hw' | 'sw';

function preferredCodecColumn(
  targetCodec: CodecCapability | undefined,
  targetUsesHardware: boolean
): CodecColumnKey | null {
  if (!targetCodec) return null;
  return targetUsesHardware ? 'hw' : 'sw';
}

function codecPreferenceHint(args: {
  preferredVideoCodec: string | null;
  selectedIsTarget: boolean;
  effectiveKey: string | null;
  selectedName: string | null;
  targetName: string | null;
  targetUsesHardware: boolean;
}): string {
  const { preferredVideoCodec, selectedIsTarget, effectiveKey, targetUsesHardware } = args;
  const selectedName = args.selectedName ?? 'codec';
  const targetName = args.targetName ?? 'the next eligible codec';
  const prefix = 'Preferred codec for camera and screen share encoding.';

  if (preferredVideoCodec) {
    if (selectedIsTarget) {
      return `${prefix} Currently ${args.targetName ?? 'unavailable'}. Concord falls back automatically if a peer cannot decode it.`;
    }
    if (effectiveKey) {
      return `Selected ${selectedName} cannot be used with the current room and settings. Concord will try ${targetName} instead.`;
    }
    return `Selected ${selectedName} cannot be used, and no routable local fallback is available.`;
  }

  if (!effectiveKey) {
    return `${prefix} Currently Auto — no routable local codec is available.`;
  }
  const encodingPath = targetUsesHardware ? 'hardware' : 'software';
  return `${prefix} Currently Auto — will try ${targetName} first using ${encodingPath} with current settings.`;
}

function unknownH264ProfileMessage(cameraUnknown: boolean, screenUnknown: boolean): string {
  if (cameraUnknown && screenUnknown) return 'Camera and Screen in use: H.264 profile unknown.';
  if (cameraUnknown) return 'Camera in use: H.264 profile unknown.';
  return 'Screen in use: H.264 profile unknown.';
}

function codecStatuses(args: {
  supported: boolean;
  preferred: boolean;
  cameraInUse: boolean;
  screenInUse: boolean;
}): string[] {
  if (!args.supported) return ['Unavailable'];
  const statuses: string[] = [];
  if (args.preferred) statuses.push('Preferred');
  if (args.cameraInUse) statuses.push('Camera In Use');
  if (args.screenInUse) statuses.push('Screen In Use');
  return statuses;
}

function codecIsInUse(args: {
  supported: boolean;
  columnKey: CodecColumnKey;
  observedColumn: CodecColumnKey;
  capability: CodecCapability;
  activeKey: string | null;
}): boolean {
  if (!args.supported || args.columnKey !== args.observedColumn) return false;
  return activeCodecMatches(args.capability, args.activeKey);
}

function codecItemClassName(supported: boolean, preferred: boolean, inUse: boolean): string {
  const classNames = ['settings-codec-item'];
  if (preferred) classNames.push('preferred');
  if (inUse) classNames.push('in-use');
  if (!supported) classNames.push('unsupported');
  return classNames.join(' ');
}

function codecItemTooltip(supported: boolean, statuses: string[]): string {
  if (!supported) return 'Unavailable in Concord';
  return statuses.join(' · ') || 'Available';
}

function codecNameClassName(supported: boolean): string {
  return supported ? 'settings-codec-name' : 'settings-codec-name strikethrough';
}

function codecStatusClassName(status: string): string {
  return status === 'Unavailable' ? 'settings-codec-status unavailable' : 'settings-codec-status';
}

interface CodecGridItemProps {
  capability: CodecCapability;
  columnKey: CodecColumnKey;
  isActiveColumn: boolean;
  preferredKey: string | null;
  appliedHardwareAcceleration: boolean;
  webrtcHwByMime: Record<string, boolean>;
  activeCameraCodec: string | null;
  activeScreenCodec: string | null;
  label: string;
}

const CodecGridItem: React.FC<CodecGridItemProps> = ({
  capability,
  columnKey,
  isActiveColumn,
  preferredKey,
  appliedHardwareAcceleration,
  webrtcHwByMime,
  activeCameraCodec,
  activeScreenCodec,
  label,
}) => {
  const supported = isRouterSupportedCodecProfile(capability.mimeType, capability.profileId);
  const isPreferred =
    isActiveColumn &&
    supported &&
    canonicalRouterCodecKey(capability.mimeType, capability.profileId) === preferredKey;
  const observedColumn: CodecColumnKey =
    appliedHardwareAcceleration && isCapabilityHardware(capability, webrtcHwByMime) ? 'hw' : 'sw';
  const cameraInUse = codecIsInUse({
    supported,
    columnKey,
    observedColumn,
    capability,
    activeKey: activeCameraCodec,
  });
  const screenInUse = codecIsInUse({
    supported,
    columnKey,
    observedColumn,
    capability,
    activeKey: activeScreenCodec,
  });
  const isInUse = cameraInUse || screenInUse;
  const statuses = codecStatuses({ supported, preferred: isPreferred, cameraInUse, screenInUse });
  const tooltip = codecItemTooltip(supported, statuses);
  const itemClassName = codecItemClassName(supported, isPreferred, isInUse);

  return (
    <div className={itemClassName} data-tooltip={tooltip}>
      <span className={codecNameClassName(supported)}>{label}</span>
      {statuses.map((status) => (
        <span className={codecStatusClassName(status)} key={status}>
          {status}
        </span>
      ))}
    </div>
  );
};

/** Does a camera preset exceed the CAMERA-axis video ceiling (height/fps, #1602)?
 *  System Default (0/0) and unknown presets never exceed. Pure over caps. */
function presetExceedsFreeCaps(key: string, camera: VideoAxisLimit): boolean {
  const preset = VIDEO_QUALITY_PRESETS[key];
  if (!preset) return false;
  return preset.height > camera.height || preset.frameRate > camera.fps;
}

/** Highest free camera preset (largest pixel-count within the CAMERA axis),
 *  falling back to System Default. Pure over caps; mirrors clampToFreeTier's resolver. */
function resolveHighestFreeCameraPreset(camera: VideoAxisLimit): string {
  let best = 'system';
  let bestScore = -1;
  for (const [key, preset] of Object.entries(VIDEO_QUALITY_PRESETS)) {
    // Reuse the lock predicate so the snap-back can never land on a preset the
    // lock marks premium (e.g. 1080p60, which exceeds the free camera height).
    if (presetExceedsFreeCaps(key, camera)) continue;
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
  disabled?: boolean;
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
 *  ceiling (`clampedFps`) per L6. Pure — the high-refresh `&&` ladder lives here.
 *  The persisted explicit fps (`selectedFps`, >0) is always represented: a value
 *  above the tiered ceiling (e.g. a downgraded free user's `screenResolution:
 *  'source'` + 60fps, which the launch-reset clamp leaves for the produce
 *  boundary, #2163) is injected as a 🔒 Premium option. Otherwise the <select>'s
 *  value has no matching <option> and renders a blank/inconsistent selection.
 *  Mirrors buildCameraPresetOptions' "keep + mark premium" pattern; display-only,
 *  so it never mutates the persisted setting (the produce boundary still clamps
 *  the actual capture). */
function buildFrameRateOptions(args: {
  maxRefreshRate: number;
  clampedFps: number;
  selectedFps: number;
}): SelectOption[] {
  const { maxRefreshRate, clampedFps, selectedFps } = args;
  const offer = (hz: number): boolean => maxRefreshRate >= hz && clampedFps >= hz;
  // Native (0) resolves to the display refresh rate at produce time, then the
  // boundary clamps it to the tiered ceiling (#2163). When the display exceeds
  // that ceiling (e.g. free 1080p on a 60Hz+ display captures at 30), mark Native
  // premium so its label matches what is actually captured, mirroring the discrete
  // over-cap fps options and the "your device supports more" resolution note.
  const nativeLabel =
    maxRefreshRate > clampedFps
      ? `Native (${maxRefreshRate} Hz) \u{1F512} Premium`
      : `Native (${maxRefreshRate} Hz)`;
  const options: SelectOption[] = [
    { value: '0', label: nativeLabel, group: 'Common' },
    // 60/30 are gated by the tiered stream fps ceiling (#2163): free 1080p offers
    // no 60, free 720p offers 60. Premium (Infinity ceiling) always offers both.
    ...(offer(60) ? [{ value: '60', label: '60 FPS', group: 'Common' }] : []),
    ...(offer(30) ? [{ value: '30', label: '30 FPS', group: 'Common' }] : []),
    ...(offer(120) ? [{ value: '120', label: '120 FPS', group: 'Additional' }] : []),
    ...(offer(100) ? [{ value: '100', label: '100 FPS', group: 'Additional' }] : []),
    ...(offer(90) ? [{ value: '90', label: '90 FPS', group: 'Additional' }] : []),
    ...(offer(75) ? [{ value: '75', label: '75 FPS', group: 'Additional' }] : []),
    // Every fixed fps is gated by the tiered ceiling, not just the high-refresh
    // ones: on an ultrawide (e.g. free 2560x1080 → pixel-rate admits 22fps) even
    // 24 exceeds the budget, so it must be omitted rather than silently clamped
    // at the produce boundary (#2163).
    ...(offer(24) ? [{ value: '24', label: '24 FPS (Cinematic)', group: 'Additional' }] : []),
    ...(offer(15) ? [{ value: '15', label: '15 FPS', group: 'Additional' }] : []),
    ...(offer(5) ? [{ value: '5', label: '5 FPS (Slideshow)', group: 'Additional' }] : []),
  ];
  // Native (0) is always present; only a positive value absent from the list needs
  // injecting to avoid a blank select. Mark it Premium ONLY when it exceeds the tier
  // ceiling — an in-tier value the clamp just selected (e.g. an ultrawide 22fps
  // ceiling) is allowed and must not carry a paywall marker (#2172 Codex). Keeps a
  // genuine over-cap persisted value intact for a later upgrade.
  if (selectedFps > 0 && !options.some((o) => o.value === String(selectedFps))) {
    options.push({
      value: String(selectedFps),
      label:
        selectedFps > clampedFps ? `${selectedFps} FPS \u{1F512} Premium` : `${selectedFps} FPS`,
      group: 'Common',
    });
  }
  return options;
}

/** Resolve a manual-bitrate slider's UI ceiling from an axis bitrate cap (bps).
 *  A native/uncapped axis (Infinity ceiling) uses the absolute UI max rather than
 *  a ∞ slider. `isCapped` (whether to show the premium upsell ghost-zone) is
 *  driven by tier, NOT by the axis value — a premium user at their finite axis
 *  ceiling must NOT see an upsell. Pure — mirrors the L5 spec (#1602). */
function deriveBitrateSlider(
  axisBitrateBps: number,
  absoluteMaxMbps: number,
  isTopTier: boolean
): { maxMbps: number; isCapped: boolean } {
  const axisMbps = axisBitrateBps / 1_000_000;
  const maxMbps = Math.min(axisMbps, absoluteMaxMbps);
  // Show the ghost-zone only for a non-top tier whose axis sits below the UI max.
  return { maxMbps, isCapped: !isTopTier && axisMbps < absoluteMaxMbps };
}

// ─── Video Configuration Section ────────────────────────────────────────────

const VideoConfigSection: React.FC = () => {
  const activeCameraCodec = useVoiceStore((s) => s.activeCameraCodec);
  const activeScreenCodec = useVoiceStore((s) => s.activeScreenCodec);
  const codecFloor = useVoiceStore((s) => s.codecFloor);

  const codecCapabilities = useVideoSettingsStore((s) => s.codecCapabilities);
  const gpuInfo = useVideoSettingsStore((s) => s.gpuInfo);
  const videoAdvancedMode = useVideoSettingsStore((s) => s.videoAdvancedMode);
  const systemHdr = useVideoSettingsStore((s) => s.systemHdr);
  const webrtcHwByMime = useVideoSettingsStore((s) => s.webrtcHwByMime);
  const appliedHardwareAcceleration = useVideoSettingsStore((s) => s.hardwareAcceleration);

  const cameraPreset = useDraftVideoSetting('cameraPreset');
  const cameraBitrate = useDraftVideoSetting('cameraBitrate');
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
  const supportSvc = useDraftVideoSetting('supportSvc');
  const supportSimulcast = useDraftVideoSetting('supportSimulcast');
  const autoTuneInScreenShares = useDraftVideoSetting('autoTuneInScreenShares');

  // Premium entitlement caps (#1301 / split axes #1602):
  //  - L2: camera-preset resolution/fps options above the CAMERA-axis ceiling
  //    carry a 🔒 marker and snap back to the highest free preset.
  //  - L5: the manual screen-share bitrate slider is fenced at the STREAM-axis
  //    ceiling; the manual camera bitrate slider at the CAMERA-axis ceiling.
  //  - L6: device-derived screen-share resolution/fps option lists are clamped to
  //    the STREAM axis, with a "your device supports more" note when native exceeds.
  // All ceilings derive from the entitlement (no hardcoded tier numbers); see the
  // #1602 matrix in [internal]specs/2026-07-03-1602-av-settings-gating-design.md.
  const entitlement = useEntitlement((e) => e);
  // Authoritative-entitlement flag: the destructive fps/preset snap-backs below gate on
  // this so they never persist a downgrade computed from the pre-hydrate FREE default
  // (#1301/#2172). Degraded is safe — the store preserves the last-known tier on reconnect.
  const hydrated = useSubscriptionStore((s) => s.hydrated);
  const degraded = useSubscriptionStore((s) => s.degraded);
  // Server video floor (#1522) is not surfaced client-side yet; the stream axis is
  // the personal ceiling only and the media-plane enforces the floor regardless.
  const videoLimits = useMemo(() => videoLimitsFromEntitlement(entitlement), [entitlement]);
  const streamLimit = videoLimits.stream;
  const cameraLimit = videoLimits.camera;
  // The stream axis the DISPLAY seams enforce (fps tiering + the native-exceeds note).
  // It fails OPEN through the shared effectiveStreamAxis gate exactly when the picker
  // and produce boundary do (pre-hydrate, or a degraded premium), so Settings never
  // strips a premium user's 60fps options or shows a spurious upsell computed from the
  // pre-hydrate FREE default (#2172). The raw streamLimit above still fences the L5
  // bitrate slider, which reads the authoritative entitlement axis directly.
  const effectiveStream = useMemo(
    () => effectiveStreamAxis({ hydrated, degraded, entitlement }),
    [hydrated, degraded, entitlement]
  );
  // Whether the camera-preset snap-back below may ENFORCE. It shares the same fail-open
  // gate as effectiveStream (shouldEnforceForSubscription): enforce when hydrated OR a
  // degraded FREE floor, fail open pre-hydrate and for a degraded premium. Unlike the
  // stream axis there is NO camera produce-boundary clamp (produceVideo captures the
  // stored preset verbatim) and useLaunchReset is itself hydration-gated, so gating this
  // on `hydrated` alone let a degraded-free user pick a premium preset that produceVideo
  // then captured unclamped (#2172).
  const enforceCameraCaps = useMemo(
    () => shouldEnforceForSubscription({ hydrated, degraded, entitlement }),
    [hydrated, degraded, entitlement]
  );
  const cameraPresetGate = useGateActivation('video-quality');
  const bitrateGate = useGateActivation('manual-bitrate');
  const cameraBitrateGate = useGateActivation('manual-bitrate');
  const [cameraPresetLockHinted, setCameraPresetLockHinted] = useState(false);
  const [screenFpsLockHinted, setScreenFpsLockHinted] = useState(false);
  const [codecProfilesOpen, setCodecProfilesOpen] = useState(false);
  const screenFpsGate = useGateActivation('video-quality');

  const targetCodec = useMemo(
    () =>
      resolveCodecTarget({
        capabilities: codecCapabilities,
        preferred: preferredVideoCodec,
        hardwareAcceleration,
        hdrEncoding,
        codecFloor,
        webrtcHwByMime: webrtcHwByMime ?? {},
      }),
    [
      codecCapabilities,
      preferredVideoCodec,
      hardwareAcceleration,
      hdrEncoding,
      codecFloor,
      webrtcHwByMime,
    ]
  );
  const targetCodecKey = targetCodec
    ? canonicalRouterCodecKey(targetCodec.mimeType, targetCodec.profileId)
    : null;
  const targetUsesHardware =
    targetCodec !== undefined &&
    hardwareAcceleration &&
    isCapabilityHardware(targetCodec, webrtcHwByMime ?? {});

  useEffect(() => {
    if (!preferredVideoCodec || codecCapabilities.length === 0) return;
    const canonicalPreference = canonicalRequestedCodecKey(
      preferredVideoCodec,
      codecCapabilities,
      hdrEncoding
    );
    const capability = canonicalPreference
      ? codecCapabilities.find((candidate) => capabilityMatchesKey(candidate, canonicalPreference))
      : undefined;
    if (
      !capability ||
      !isRouterSupportedCodecProfile(capability.mimeType, capability.profileId) ||
      (canonicalPreference === 'video/vp9:2' && !hdrEncoding)
    ) {
      setDraftVideoSetting('preferredVideoCodec', null);
    } else if (preferredVideoCodec.toLowerCase() !== canonicalPreference) {
      setDraftVideoSetting('preferredVideoCodec', canonicalPreference);
    }
  }, [preferredVideoCodec, codecCapabilities, hdrEncoding]);

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
      targetCodec: targetCodecKey,
    });
  }, [
    screenResolution,
    screenFrameRate,
    activeScreenCodec,
    targetCodecKey,
    bestDisplay,
    maxRefreshRate,
  ]);

  const clampedRecommended = Math.max(1_500_000, Math.min(30_000_000, recommendedBitrate));

  // ── L2: camera-preset resolution/fps lock (CAMERA axis, #1602) ──────────
  // The camera-axis ceiling drives both the lock predicate and the snap-back
  // resolver; the pure helpers close over `cameraLimit` so the body stays flat.
  const presetExceedsFree = useCallback(
    (key: string): boolean => presetExceedsFreeCaps(key, cameraLimit),
    [cameraLimit]
  );

  const handleCameraPresetChange = useCallback(
    (key: string): void => {
      // Snap a locked preset back only when the shared gate says the entitlement is
      // authoritative enough to enforce (hydrated, or a degraded FREE floor). Camera has
      // NO produce-boundary clamp (produceVideo captures the stored preset verbatim) and
      // useLaunchReset is hydration-gated, so this snap-back is the sole enforcement seam.
      // Failing open for pre-hydrate and degraded-premium preserves a premium user's
      // preset (#1301/#2172); a degraded-free user is still clamped, closing the escape.
      if (enforceCameraCaps && presetExceedsFreeCaps(key, cameraLimit)) {
        // L2 snap-back: a locked preset never reaches the store. Clamp to the
        // highest free preset and reveal the chip (no mid-action modal).
        setDraftVideoSetting('cameraPreset', resolveHighestFreeCameraPreset(cameraLimit));
        setCameraPresetLockHinted(true);
        return;
      }
      setCameraPresetLockHinted(false);
      setDraftVideoSetting('cameraPreset', key);
    },
    [cameraLimit, enforceCameraCaps]
  );

  // #2163: the screen-share fps ceiling is resolution-dependent (tiered pixel-rate).
  // It is derived from the resolution that will ACTUALLY be captured, i.e. the
  // selected dims after the produce-boundary height clamp (a free 'source'/4K pick
  // captures at 1080p, so 30fps, not the raw display). It tiers against effectiveStream
  // (the shared fail-open gate), NOT the raw entitlement axis, so pre-hydrate (or for a
  // degraded premium) the full fps ladder is offered instead of the FREE default, keeping
  // the offered fps in lockstep with both the picker and the produce capture (#2172).
  const streamFpsCeilingFor = useCallback(
    (dims: { w: number; h: number }): number =>
      clampScreenCapture(dims.w, dims.h, effectiveStream.fps, effectiveStream).fps,
    [effectiveStream]
  );
  const streamFpsCeiling = useMemo(
    () => streamFpsCeilingFor(resolveScreenResolution(screenResolution, bestDisplay)),
    [streamFpsCeilingFor, screenResolution, bestDisplay]
  );

  // ── L6: device-derived resolution / frame-rate native-exceeds guard ─────
  // The screen-share Resolution + Frame Rate option lists are derived from the
  // detected display. Clamp the offered ceiling to effectiveStream (the shared
  // fail-open axis) and surface a "your device supports more" note when the device
  // genuinely exceeds it. The fps arm uses the RESOLUTION-TIERED ceiling
  // (streamFpsCeiling), NOT the raw axis fps: free streamMaxFps is 60 (the 720p
  // ceiling), but 1080p is pixel-rate-capped to 30, so a free 1080p/60Hz device
  // genuinely exceeds its tier and must show the upsell note, matching the fps
  // dropdown. Because effectiveStream fails open pre-hydrate (or for a degraded
  // premium), a premium user never sees a spurious upsell before hydration (#2163/#2172).
  const nativeGuard = nativeExceedsFree(
    { nativeHeight: bestDisplay.height, nativeFps: maxRefreshRate },
    { ...effectiveStream, fps: streamFpsCeiling }
  );

  // Snap an over-cap fps down when the resolution changes (mirrors the camera-preset
  // snap-back), revealing the premium chip. Native (0) is left for the produce clamp.
  const handleScreenResolutionChange = useCallback(
    (v: string): void => {
      setDraftVideoSetting('screenResolution', v);
      // Only snap the fps down when the entitlement is authoritative (hydrated). Pre-hydrate,
      // streamFpsCeilingFor is derived from the FREE default, so snapping would persist a
      // downgraded fps for a premium user that launch-reset never restores (#1301/#2172 Codex).
      if (!hydrated) {
        setScreenFpsLockHinted(false);
        return;
      }
      const ceiling = streamFpsCeilingFor(resolveScreenResolution(v, bestDisplay));
      if (screenFrameRate > 0 && screenFrameRate > ceiling) {
        setDraftVideoSetting('screenFrameRate', ceiling);
        setScreenFpsLockHinted(true);
      } else {
        setScreenFpsLockHinted(false);
      }
    },
    [streamFpsCeilingFor, bestDisplay, screenFrameRate, hydrated]
  );

  // ── L5: manual bitrate clamps — STREAM (screen-share) + CAMERA sliders ──
  // Each slider fences its live value at its axis ceiling. A native/uncapped
  // premium axis (Infinity) uses the absolute UI max instead of a ∞ slider.
  const ABSOLUTE_BITRATE_MAX_MBPS = 30;
  const isTopTier = entitlement.tier === 'premium';
  const streamBitrate = deriveBitrateSlider(
    streamLimit.bitrate,
    ABSOLUTE_BITRATE_MAX_MBPS,
    isTopTier
  );
  const cameraBitrateSlider = deriveBitrateSlider(
    cameraLimit.bitrate,
    ABSOLUTE_BITRATE_MAX_MBPS,
    isTopTier
  );
  // When toggling automatic OFF, seed the manual cap at the recommended value but
  // never above the axis ceiling (so a user starts within the live range).
  const initialManualBitrate = Math.min(clampedRecommended, streamLimit.bitrate);
  // Camera has no per-resolution recommendation heuristic; seed at its ceiling.
  const initialCameraBitrate = cameraLimit.bitrate;

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
          // L2: premium presets (above the CAMERA-axis height/fps ceiling, #1602)
          // carry a trailing \ud83d\udd12 + "Premium" marker; the select stays usable and
          // selecting one snaps back to the highest free preset. The per-option
          // marker logic lives in the pure buildCameraPresetOptions helper.
          options={buildCameraPresetOptions(presetExceedsFree)}
          value={cameraPreset}
          onChange={handleCameraPresetChange}
        />
      </div>

      {/* ── Camera Bitrate (split camera axis, #1602) ── */}
      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label">Automatic Camera Bitrate</span>
          <span className="settings-row-hint">
            {cameraBitrate === 0
              ? 'Enabled. Concord Voice adjusts your camera bitrate based on resolution, frame rate, and codec.'
              : `Disabled. Using a fixed ${(cameraBitrate / 1_000_000).toFixed(1)} Mbps camera cap.`}
          </span>
        </div>
        <ToggleSwitch
          checked={cameraBitrate === 0}
          onChange={(v) =>
            // Seed the manual camera cap at the camera-axis ceiling when disabling
            // automatic, so a user starts inside the live range.
            setDraftVideoSetting('cameraBitrate', v ? 0 : initialCameraBitrate)
          }
        />
      </div>
      {cameraBitrate !== 0 && (
        <div className="settings-volume-row">
          <div className="settings-row-info">
            <span className="settings-volume-label">Camera Cap</span>
            <span className="settings-row-hint">
              Sets the maximum bitrate cap for your camera (the camera axis). Right (
              {cameraBitrateSlider.maxMbps} Mbps) for high-motion video.
            </span>
          </div>
          <div className="settings-slider-wrapper">
            <span className="settings-slider-value">
              {(cameraBitrate / 1_000_000).toFixed(1)} Mbps
            </span>
            {/* L5: the camera slider stays LIVE up to the camera cap; a capped axis
                shows the premium ghost-zone, and `max` fences the live value. */}
            <div
              className={`settings-bitrate-slider-wrap${cameraBitrateSlider.isCapped ? ' capped' : ''}`}
            >
              <input
                type="range"
                className="settings-volume-slider"
                min={0.5}
                max={cameraBitrateSlider.maxMbps}
                step={0.5}
                value={Math.min(cameraBitrate / 1_000_000, cameraBitrateSlider.maxMbps)}
                onChange={(e) =>
                  setDraftVideoSetting(
                    'cameraBitrate',
                    Math.round(
                      Math.min(Number(e.target.value), cameraBitrateSlider.maxMbps) * 1_000_000
                    )
                  )
                }
              />
              {cameraBitrateSlider.isCapped && (
                <button
                  type="button"
                  className="settings-bitrate-ghost-zone"
                  onClick={cameraBitrateGate.onActivate}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter' || e.key === ' ') cameraBitrateGate.onActivate(e);
                  }}
                >
                  <span aria-hidden="true" className="settings-bitrate-ghost-gradient" />
                  <PremiumChip
                    label={`beyond ${cameraBitrateSlider.maxMbps} Mbps →`}
                    id={cameraBitrateGate.describedById}
                  />
                </button>
              )}
            </div>
          </div>
        </div>
      )}

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
          onChange={handleScreenResolutionChange}
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
          {screenFpsLockHinted && (
            <span className="settings-row-premium-note">
              <PremiumChip
                label="higher frame rates"
                onActivate={screenFpsGate.onActivate}
                id={screenFpsGate.describedById}
              />
            </span>
          )}
        </div>
        <CustomSelect
          className="settings-select"
          // #2163: the fps option ceiling is the resolution-tiered stream max
          // (streamFpsCeiling): free 1080p offers ≤30, free 720p offers ≤60. It is
          // combined with the L6 device/native clamp. The option ladder lives in the
          // pure buildFrameRateOptions helper.
          options={buildFrameRateOptions({
            maxRefreshRate,
            clampedFps: Math.min(nativeGuard.clampedFps, streamFpsCeiling),
            selectedFps: screenFrameRate,
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

      <div className="settings-row">
        <div className="settings-row-info">
          <span className="settings-row-label" id="auto-tune-shares-label">
            Automatically tune in to screen shares
          </span>
          <span className="settings-row-hint" id="auto-tune-shares-hint">
            When enabled, Concord tunes in to screen shares in voice calls you join. You can still
            tune out of any stream manually.
          </span>
        </div>
        <ToggleSwitch
          id="auto-tune-shares"
          ariaLabelledBy="auto-tune-shares-label"
          aria-describedby="auto-tune-shares-hint"
          checked={autoTuneInScreenShares}
          onChange={(v) => setDraftVideoSetting('autoTuneInScreenShares', v)}
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
              const codecDisplayName: Record<string, string> = {
                H264: 'AVC (H.264)',
                H265: 'HEVC (H.265)',
                HEVC: 'HEVC (H.265)',
              };
              const sortByPriority = (a: CodecCapability, b: CodecCapability) =>
                codecPriority(codecKey(a), hdrEncoding) - codecPriority(codecKey(b), hdrEncoding);
              const humanProfile = (c: CodecCapability) =>
                humanizeProfileLabel(c.profileId, c.profileLabel);
              const displayName = (c: CodecCapability) => {
                const raw = c.mimeType.replace('video/', '');
                const base = codecDisplayName[raw] ?? raw;
                const profile = humanProfile(c);
                return profile ? `${base} (${profile})` : base;
              };
              const isSupported = (c: CodecCapability) =>
                isRouterSupportedCodecProfile(c.mimeType, c.profileId);

              // Dedupe entries that resolve to the same (codec, human profile) pair.
              // Raw profile-level-id hex strings that collapse to the same label
              // (e.g. "42001f" and an already-labeled "Constrained Baseline 3.1")
              // must not appear twice.
              const dedupe = (list: CodecCapability[]): CodecCapability[] => {
                const seen = new Set<string>();
                const out: CodecCapability[] = [];
                for (const c of list) {
                  const key =
                    canonicalRouterCodecKey(c.mimeType, c.profileId) ??
                    `${c.mimeType.toLowerCase()}|${humanProfile(c) ?? ''}`;
                  if (seen.has(key)) continue;
                  seen.add(key);
                  out.push(c);
                }
                return out;
              };

              // HW column: codecs with confirmed GPU acceleration
              // SW column: ALL codecs (every codec has a software encoder fallback)
              const hwCodecs = dedupe(
                codecCapabilities
                  .filter((c) => isHardwareEncodingAvailable(c, webrtcHwByMime ?? {}))
                  .sort(sortByPriority)
              );
              const swCodecs = dedupe([...codecCapabilities].sort(sortByPriority));
              // We have a definite HW verdict for at least one codec (WebCodecs probe
              // returned true/false rather than undefined). Drives the "no supported HW
              // codecs" fallback notice below.
              const systemProfilesPopulated =
                codecCapabilities.some((c) => c.hwAvailable !== undefined) ||
                Object.values(webrtcHwByMime ?? {}).some((value) => value !== undefined);

              const hwHasSupported = hwCodecs.some(isSupported);
              const preferredColumn = preferredCodecColumn(targetCodec, targetUsesHardware);
              const hwActive = preferredColumn === 'hw';
              const swActive = preferredColumn === 'sw';
              const preferredKey = targetCodecKey?.toLowerCase() ?? null;
              const cameraProfileUnknown = activeCameraCodec?.toLowerCase() === 'video/h264';
              const screenProfileUnknown = activeScreenCodec?.toLowerCase() === 'video/h264';

              return (
                <>
                  <div className="settings-codec-grid">
                    <div className={`settings-codec-column${hwActive ? ' active' : ''}`}>
                      <span
                        className={`settings-codec-column-header${hwActive ? ' active' : ''}`}
                        title="Codecs your GPU can hardware-encode. Whether a given call uses hardware depends on the negotiated codec."
                        {...(hwActive ? { 'data-tooltip': 'Preferred' } : {})}
                      >
                        Hardware
                      </span>
                      <div className="settings-codec-column-items">
                        {hwCodecs.map((capability) => (
                          <CodecGridItem
                            key={'hw-' + codecKey(capability)}
                            capability={capability}
                            columnKey="hw"
                            isActiveColumn={hwActive}
                            preferredKey={preferredKey}
                            appliedHardwareAcceleration={appliedHardwareAcceleration}
                            webrtcHwByMime={webrtcHwByMime ?? {}}
                            activeCameraCodec={activeCameraCodec}
                            activeScreenCodec={activeScreenCodec}
                            label={displayName(capability)}
                          />
                        ))}
                        {hwCodecs.length === 0 && (
                          <span className="settings-codec-empty">None detected</span>
                        )}
                      </div>
                    </div>
                    <div className={`settings-codec-column${swActive ? ' active' : ''}`}>
                      <span
                        className={`settings-codec-column-header${swActive ? ' active' : ''}`}
                        {...(swActive ? { 'data-tooltip': 'Preferred' } : {})}
                      >
                        Software
                      </span>
                      <div className="settings-codec-column-items">
                        {swCodecs.map((capability) => (
                          <CodecGridItem
                            key={'sw-' + codecKey(capability)}
                            capability={capability}
                            columnKey="sw"
                            isActiveColumn={swActive}
                            preferredKey={preferredKey}
                            appliedHardwareAcceleration={appliedHardwareAcceleration}
                            webrtcHwByMime={webrtcHwByMime ?? {}}
                            activeCameraCodec={activeCameraCodec}
                            activeScreenCodec={activeScreenCodec}
                            label={displayName(capability)}
                          />
                        ))}
                        {swCodecs.length === 0 && (
                          <span className="settings-codec-empty">None detected</span>
                        )}
                      </div>
                    </div>
                  </div>
                  {(cameraProfileUnknown || screenProfileUnknown) && (
                    <output className="settings-codec-active-unknown">
                      {unknownH264ProfileMessage(cameraProfileUnknown, screenProfileUnknown)}
                    </output>
                  )}
                  {hardwareAcceleration && systemProfilesPopulated && !hwHasSupported && (
                    <div className="settings-hw-fallback-notice">
                      Hardware acceleration is enabled, but none of your GPU&apos;s codecs are
                      currently supported by Concord Voice. Software encoding will be used for now.
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
                    return 'Enabled. Concord prefers HDR-capable codec profiles when available. This does not guarantee a 10-bit or HDR stream.';
                  return 'Disabled. Concord prefers SDR codec profiles and will not select VP9 Profile 2.';
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
            const sortByPriority = (a: CodecCapability, b: CodecCapability) =>
              codecPriority(codecKey(a), hdrEncoding) - codecPriority(codecKey(b), hdrEncoding);
            const seenRouterKeys = new Set<string>();
            const supported = codecCapabilities
              .filter((c) => isRouterSupportedCodecProfile(c.mimeType, c.profileId))
              .sort(sortByPriority)
              .filter((capability) => {
                const key = canonicalRouterCodecKey(capability.mimeType, capability.profileId);
                if (!key || seenRouterKeys.has(key)) return false;
                seenRouterKeys.add(key);
                return true;
              });
            const effectiveKey = targetCodecKey;
            const info = effectiveKey ? getCodecInfo(effectiveKey) : null;
            const selectedKey = preferredVideoCodec
              ? canonicalRequestedCodecKey(preferredVideoCodec, codecCapabilities, hdrEncoding)
              : null;
            const selectedInfo = selectedKey ? getCodecInfo(selectedKey) : null;
            const selectedIsTarget = selectedKey !== null && selectedKey === effectiveKey;
            const hint = codecPreferenceHint({
              preferredVideoCodec,
              selectedIsTarget,
              effectiveKey,
              selectedName: selectedInfo?.name ?? null,
              targetName: info?.name ?? null,
              targetUsesHardware,
            });
            return (
              <>
                <div className="settings-row settings-codec-select-row">
                  <div className="settings-row-info">
                    <label className="settings-row-label" htmlFor="video-codec-select">
                      Video Codec
                    </label>
                    <span className="settings-row-hint">{hint}</span>
                    <button
                      type="button"
                      className="settings-codec-profile-help"
                      aria-haspopup="dialog"
                      onClick={() => setCodecProfilesOpen(true)}
                    >
                      What are codec profiles?
                    </button>
                  </div>
                  <CustomSelect
                    id="video-codec-select"
                    className="settings-select"
                    options={[
                      { value: '', label: 'Auto' },
                      ...supported.flatMap((c) => {
                        const key = canonicalRouterCodecKey(c.mimeType, c.profileId);
                        if (!key) return [];
                        const requiresHdr = key === 'video/vp9:2' && !hdrEncoding;
                        return [
                          {
                            value: key,
                            label: `${codecProfileMenuLabel(c.mimeType, c.profileId)}${requiresHdr ? ' — Requires HDR setting' : ''}`,
                            disabled: requiresHdr,
                          },
                        ];
                      }),
                    ]}
                    value={preferredVideoCodec ?? ''}
                    onChange={(v) => setDraftVideoSetting('preferredVideoCodec', v || null)}
                  />
                </div>

                {info &&
                  (() => {
                    // H.264 quality/efficiency varies materially by profile. Keep the
                    // family-level fallback only for an unqualified/unknown H.264 key;
                    // profile-qualified keys use getCodecInfo's profile-aware facts.
                    const meta =
                      effectiveKey && !/^video\/h264:[0-9a-f]{6}$/i.test(effectiveKey)
                        ? getCodecMetadata(effectiveKey)
                        : null;
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

                <div className="settings-codec-preference-notice">
                  <strong>Preferred</strong> is the target Concord will try first.{' '}
                  <strong>Camera In Use</strong> and <strong>Screen In Use</strong> report what each
                  active producer actually uses after room compatibility checks.
                </div>
                <CodecProfilesModal
                  isOpen={codecProfilesOpen}
                  onClose={() => setCodecProfilesOpen(false)}
                />
              </>
            );
          })()}

          {/* SVC / Simulcast casting toggles (#1921). These gate codec-derived
              layering modes, never codec selection. A codec-inert toggle stays ON +
              interactive (never disabled). Helper copy follows the resolved target,
              including when Auto or a floor fallback is active. */}
          {(() => {
            const copy = castingCopy(targetCodecKey, supportSvc, supportSimulcast);
            return (
              <>
                <div className="settings-row">
                  <div className="settings-row-info">
                    <span className="settings-row-label" id="casting-svc-label">
                      Support SVC
                    </span>
                    <span className="settings-row-hint" id="casting-svc-hint">
                      {copy.svc}
                    </span>
                  </div>
                  <ToggleSwitch
                    id="casting-svc"
                    ariaLabelledBy="casting-svc-label"
                    aria-describedby="casting-svc-hint"
                    checked={supportSvc}
                    onChange={(v) => setDraftVideoSetting('supportSvc', v)}
                  />
                </div>
                <div className="settings-row">
                  <div className="settings-row-info">
                    <span className="settings-row-label" id="casting-simulcast-label">
                      Support Simulcast
                    </span>
                    <span className="settings-row-hint" id="casting-simulcast-hint">
                      {copy.simulcast}
                    </span>
                  </div>
                  <ToggleSwitch
                    id="casting-simulcast"
                    ariaLabelledBy="casting-simulcast-label"
                    aria-describedby="casting-simulcast-hint"
                    checked={supportSimulcast}
                    onChange={(v) => setDraftVideoSetting('supportSimulcast', v)}
                  />
                </div>
                {copy.notice && <output className="settings-row-hint">{copy.notice}</output>}
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
                <span className="settings-volume-label">Screen Share Cap</span>
                <span className="settings-row-hint">
                  Sets the maximum bitrate cap for screen sharing (the stream axis). Left (1.5 Mbps)
                  for simple content. Right ({streamBitrate.maxMbps} Mbps) for high-motion content.
                </span>
              </div>
              <div className="settings-slider-wrapper">
                <span className="settings-slider-value">
                  {(screenShareBitrate / 1_000_000).toFixed(1)} Mbps
                </span>
                {/* L5: the slider stays LIVE up to the stream cap. When capped, a
                    decorative ghost-zone past the thumb advertises the premium
                    range; the slider's `max` fences the live value at the cap. */}
                <div
                  className={`settings-bitrate-slider-wrap${streamBitrate.isCapped ? ' capped' : ''}`}
                >
                  <input
                    type="range"
                    className="settings-volume-slider"
                    min={1.5}
                    max={streamBitrate.maxMbps}
                    step={0.5}
                    value={Math.min(screenShareBitrate / 1_000_000, streamBitrate.maxMbps)}
                    onChange={(e) =>
                      setDraftVideoSetting(
                        'screenShareBitrate',
                        Math.round(
                          Math.min(Number(e.target.value), streamBitrate.maxMbps) * 1_000_000
                        )
                      )
                    }
                  />
                  {streamBitrate.isCapped && (
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
                        label={`beyond ${streamBitrate.maxMbps} Mbps →`}
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
        </>
      )}
    </CollapsibleSection>
  );
};

export default VideoConfigSection;
