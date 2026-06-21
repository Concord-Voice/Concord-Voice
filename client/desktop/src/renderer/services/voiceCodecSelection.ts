/**
 * Codec cascade selection logic extracted from VoiceService to reduce
 * cognitive complexity. Pure functions — no VoiceService `this` dependency.
 *
 * Cascade order: user pref → AV1 → HEVC → H264 High → VP9:2 (HDR) → H264 → VP9 → VP8
 * Two-pass: HW-accelerated first, then SW fallback.
 */

import type { types as mediasoupTypes } from 'mediasoup-client';

// ─── Types ───────────────────────────────────────────────────────────

/** A codec cascade entry with primary MIME and optional alternates (e.g. H265/HEVC). */
export interface CodecCascadeEntry {
  /** Primary MIME + optional profile (e.g. 'video/H264:6400') */
  mimes: string[];
}

/** Callbacks that the VoiceService instance provides to the pure selection logic. */
export interface CodecLookup {
  isInCodecFloor: (key: string) => boolean;
  isHwAccelerated: (key: string) => boolean;
  findSendCodec: (key: string) => mediasoupTypes.RtpCodecCapability | undefined;
}

export interface CodecCascadeConfig extends CodecLookup {
  preferred: string | null;
  hwAccel: boolean;
  hdrEncoding: boolean;
}

// ─── Cascade Construction ────────────────────────────────────────────

/** Build the ordered codec cascade. HDR adds VP9 profile 2. */
export function buildCodecCascade(hdrEncoding: boolean): CodecCascadeEntry[] {
  return [
    { mimes: ['video/AV1'] },
    { mimes: ['video/H265', 'video/HEVC'] },
    { mimes: ['video/H264:6400'] },
    ...(hdrEncoding ? [{ mimes: ['video/VP9:2'] }] : []),
    { mimes: ['video/H264'] },
    { mimes: ['video/VP9'] },
  ];
}

// ─── Cascade Search ──────────────────────────────────────────────────

/**
 * Search the cascade for the first codec that is floor-compatible and
 * passes an optional HW filter. Returns undefined if nothing matches.
 */
export function findFirstFloorCompatibleCodec(
  cascade: CodecCascadeEntry[],
  lookup: CodecLookup,
  requireHw: boolean
): mediasoupTypes.RtpCodecCapability | undefined {
  for (const entry of cascade) {
    if (requireHw && !entry.mimes.some(lookup.isHwAccelerated)) continue;
    if (!entry.mimes.some(lookup.isInCodecFloor)) continue;

    for (const mime of entry.mimes) {
      const codec = lookup.findSendCodec(mime);
      if (codec) return codec;
    }
  }
  return undefined;
}

// ─── Public API ──────────────────────────────────────────────────────

/**
 * Select the best codec from the cascade.
 *
 * Priority:
 *   1. User-preferred codec (if floor-compatible)
 *   2. HW-accelerated codec from cascade (if HW accel enabled)
 *   3. Any codec from cascade (SW fallback)
 *   4. VP8 last resort (bypasses floor)
 */
export function selectCodecFromCascade(
  config: CodecCascadeConfig
): mediasoupTypes.RtpCodecCapability | undefined {
  const { preferred, hwAccel, hdrEncoding, ...lookup } = config;

  // 1. User preference
  if (preferred && lookup.isInCodecFloor(preferred)) {
    const codec = lookup.findSendCodec(preferred);
    if (codec) return codec;
  }

  const cascade = buildCodecCascade(hdrEncoding);

  // 2. HW-accelerated pass
  if (hwAccel) {
    const hwCodec = findFirstFloorCompatibleCodec(cascade, lookup, true);
    if (hwCodec) return hwCodec;
  }

  // 3. SW fallback pass
  const swCodec = findFirstFloorCompatibleCodec(cascade, lookup, false);
  if (swCodec) return swCodec;

  // 4. VP8 last resort — universal support, bypasses floor
  return lookup.findSendCodec('video/VP8');
}
