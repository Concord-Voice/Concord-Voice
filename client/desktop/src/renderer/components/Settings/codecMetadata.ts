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

// ---------------------------------------------------------------------------
// H.264 profile-level-id → human label
// ---------------------------------------------------------------------------

export const PROFILE_LEVEL_ID_LABELS: Record<string, string> = {
  '42001f': 'Constrained Baseline 3.1',
  '42e01f': 'Baseline 3.1',
  '4d401f': 'Main 3.1',
  '64001f': 'High 3.1',
  '640c1f': 'Constrained High 3.1',
  '42000a': 'Constrained Baseline 1.0',
  '4d400a': 'Main 1.0',
  f4000a: 'High 4.0',
};

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
    hdrCapable: true,
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

/** Lookup metadata for a codec key or mimeType; returns null when unknown. */
export function getCodecMetadata(keyOrMime: string): CodecMetadata | null {
  const normalized = keyOrMime
    .toLowerCase()
    .replace(/^video\//, '')
    .split(':')[0];
  return CODEC_METADATA[normalized] ?? null;
}
