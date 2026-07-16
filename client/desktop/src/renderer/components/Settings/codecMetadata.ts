/**
 * Codec metadata for Settings > Video Codec display.
 *
 * Two concerns live here:
 *   1. `PROFILE_LEVEL_ID_LABELS` — translates raw H.264 profile-level-id hex
 *      strings (as produced by `detectCodecCapabilities`) into human-readable
 *      labels. The raw hex (e.g. `42001f`) must never be shown in the UI.
 *   2. `CODEC_METADATA` — per base-codec descriptive metadata (quality,
 *      efficiency, HDR, description) rendered as a structured list next to
 *      the codec select.
 */

import { h264ProfileClass, type H264ProfileClass } from '../../services/voiceCodecSelection';

// ---------------------------------------------------------------------------
// H.264 profile-level-id → human label
// ---------------------------------------------------------------------------

export const PROFILE_LEVEL_ID_LABELS: Record<string, string> = {
  '42001f': 'Baseline 3.1',
  '42e01f': 'Constrained Baseline 3.1',
  '4d0032': 'Main 5.0',
  '640034': 'High 5.2',
  '4d401f': 'Main 3.1',
  '64001f': 'High 3.1',
  '640c1f': 'Constrained High 3.1',
  '42000a': 'Baseline 1.0',
  '4d400a': 'Main 1.0',
  f4000a: 'Predictive High 4:4:4 1.0',
};

const ROUTER_H264_PROFILE_IDS: Partial<Record<H264ProfileClass, string>> = {
  'constrained-baseline': '42e01f',
  main: '4d0032',
  high: '640034',
};
const ROUTER_VIDEO_MIME_TYPES = new Set(['video/av1', 'video/vp8']);

/**
 * Map a client codec capability to the equivalent codec key configured on the
 * mediasoup router. H.264 levels may differ when level asymmetry is allowed,
 * so compatibility is determined by profile class rather than exact hex.
 */
export function canonicalRouterCodecKey(
  mimeType: string,
  profileId: string | null | undefined
): string | null {
  const mime = mimeType.toLowerCase();

  if (mime === 'video/h264') {
    if (!profileId) return null;
    const profileClass = h264ProfileClass(profileId.toLowerCase());
    const routerProfileId = profileClass ? ROUTER_H264_PROFILE_IDS[profileClass] : undefined;
    return routerProfileId ? `${mime}:${routerProfileId}` : null;
  }

  if (mime === 'video/vp9') {
    const normalized = profileId?.toLowerCase() ?? '0';
    return normalized === '0' || normalized === '2' ? `${mime}:${normalized}` : null;
  }

  return ROUTER_VIDEO_MIME_TYPES.has(mime) ? mime : null;
}

/** Whether Concord's mediasoup router can negotiate this client codec profile. */
export function isRouterSupportedCodecProfile(
  mimeType: string,
  profileId: string | null | undefined
): boolean {
  return canonicalRouterCodecKey(mimeType, profileId) !== null;
}

/**
 * Return a humanized profile label for a raw profile-level-id hex string.
 * Falls back to the provided existing label when the hex is not in the map,
 * and finally to `null` so callers can drop the parenthetical altogether.
 */
export function humanizeProfileLabel(
  profileId: string | null,
  fallbackLabel: string | null
): string | null {
  if (profileId) {
    const key = profileId.toLowerCase();
    const mapped = PROFILE_LEVEL_ID_LABELS[key];
    if (mapped) return mapped;
    // If the existing label looks like a raw hex profile id (6 hex chars),
    // suppress it — we never want to show raw hex.
    if (fallbackLabel && /^[0-9a-f]{6}$/i.test(fallbackLabel)) return null;
  }
  return fallbackLabel;
}

export interface CodecProfileGuideRow {
  key: string;
  label: string;
  standard: string;
  signal: string | null;
  meaning: string;
}

/** Shared menu labels and guide copy for every selectable codec/profile. */
export const CODEC_PROFILE_GUIDE: readonly CodecProfileGuideRow[] = [
  {
    key: 'video/av1:hdr',
    label: 'AV1 (10-bit HDR target)',
    standard: 'AV1 Main profile (profile 0), 10-bit application target',
    signal: 'WebCodecs probe av01.0.05M.10',
    meaning:
      'Targets 10-bit HDR-capable encoding. WebRTC does not expose definitive encoded AV1 bit depth.',
  },
  {
    key: 'video/av1:sdr',
    label: 'AV1 (8-bit SDR target)',
    standard: 'AV1 Main profile (profile 0), 8-bit application target',
    signal: 'WebCodecs probe av01.0.05M.08',
    meaning: 'Targets 8-bit SDR encoding.',
  },
  {
    key: 'video/av1',
    label: 'AV1',
    standard: 'AV1 Main profile (profile 0)',
    signal: 'profile=0',
    meaning:
      'Supports 8- and 10-bit coding. HDR-capable, but Concord does not guarantee a 10-bit HDR stream.',
  },
  {
    key: 'video/vp9:2',
    label: 'VP9 (Profile 2 — HDR)',
    standard: 'VP9 Profile 2',
    signal: 'profile-id=2',
    meaning: '10-bit HDR-capable profile with SVC.',
  },
  {
    key: 'video/vp9:0',
    label: 'VP9 (Profile 0 — SVC)',
    standard: 'VP9 Profile 0',
    signal: 'profile-id=0',
    meaning: '8-bit SDR profile with SVC.',
  },
  {
    key: 'video/h264:640034',
    label: 'H.264 (High 5.2 — Best H.264 quality)',
    standard: 'AVC High Profile, Level 5.2',
    signal: 'profile-level-id=640034',
    meaning: 'Best H.264 compression available in Concord; 8-bit SDR.',
  },
  {
    key: 'video/h264:4d0032',
    label: 'H.264 (Main 5.0 — Balanced)',
    standard: 'AVC Main Profile, Level 5.0',
    signal: 'profile-level-id=4d0032',
    meaning: 'Balances H.264 compression efficiency and compatibility; 8-bit SDR.',
  },
  {
    key: 'video/h264:42e01f',
    label: 'H.264 (Constrained Baseline 3.1 — Compatibility)',
    standard: 'AVC Constrained Baseline Profile, Level 3.1',
    signal: 'profile-level-id=42e01f',
    meaning: 'Broadest WebRTC compatibility among Concord’s H.264 profiles; 8-bit SDR.',
  },
  {
    key: 'video/vp8',
    label: 'VP8',
    standard: 'VP8 (no selectable profile)',
    signal: null,
    meaning: 'Legacy 8-bit SDR fallback.',
  },
] as const;

export function codecProfileMenuLabel(mimeType: string, profileId: string | null): string {
  const mime = mimeType.toLowerCase();
  const normalizedProfile = profileId?.toLowerCase() ?? (mime === 'video/vp9' ? '0' : null);
  const appTargetKey =
    mime === 'video/av1' && (normalizedProfile === 'hdr' || normalizedProfile === 'sdr')
      ? `${mime}:${normalizedProfile}`
      : null;
  const key =
    appTargetKey ??
    canonicalRouterCodecKey(mimeType, profileId) ??
    (normalizedProfile ? `${mime}:${normalizedProfile}` : mime);
  const row = CODEC_PROFILE_GUIDE.find((candidate) => candidate.key === key);
  return row?.label ?? mimeType.replace(/^video\//i, '');
}

// ---------------------------------------------------------------------------
// Codec metadata for structured codec description
// ---------------------------------------------------------------------------

export type CodecRating = 'Ultra' | 'High' | 'Mid' | 'Low';

export interface CodecMetadata {
  quality: CodecRating;
  efficiency: CodecRating;
  /** Compression factor versus H.264 (e.g. "~50% better than H.264") */
  compression: string;
  hdrCapable: boolean;
  description: string;
}

export const CODEC_METADATA: Record<string, CodecMetadata> = {
  av1: {
    quality: 'Ultra',
    efficiency: 'Ultra',
    compression: '~50% better than H.264',
    hdrCapable: true,
    description: 'Modern royalty-free codec. Best for screen sharing on capable hardware.',
  },
  vp9: {
    quality: 'High',
    efficiency: 'High',
    compression: '~40% better than H.264',
    hdrCapable: false,
    description: 'Google codec with strong SVC support and broad browser coverage.',
  },
  h264: {
    quality: 'Mid',
    efficiency: 'Mid',
    compression: 'Reference',
    hdrCapable: false,
    description: 'Universal compatibility. Works on essentially every device.',
  },
  h265: {
    quality: 'High',
    efficiency: 'High',
    compression: '~40% better than H.264',
    hdrCapable: true,
    description: 'HEVC. Great efficiency but patent-encumbered and not yet routed by Concord.',
  },
  hevc: {
    quality: 'High',
    efficiency: 'High',
    compression: '~40% better than H.264',
    hdrCapable: true,
    description: 'HEVC. Great efficiency but patent-encumbered and not yet routed by Concord.',
  },
  vp8: {
    quality: 'Low',
    efficiency: 'Low',
    compression: 'Similar to H.264 Baseline',
    hdrCapable: false,
    description: 'Legacy fallback. Always works when nothing else is available.',
  },
};

const VP9_HDR_METADATA: CodecMetadata = {
  ...CODEC_METADATA.vp9,
  hdrCapable: true,
};

/** Lookup metadata for a codec key or mimeType; returns null when unknown. */
export function getCodecMetadata(keyOrMime: string): CodecMetadata | null {
  const [codec, profileId] = keyOrMime
    .toLowerCase()
    .replace(/^video\//, '')
    .split(':');
  if (codec === 'vp9' && profileId === '2') return VP9_HDR_METADATA;
  return CODEC_METADATA[codec] ?? null;
}
