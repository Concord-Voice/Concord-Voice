/**
 * Codec cascade selection logic extracted from VoiceService to reduce
 * cognitive complexity. Pure functions — no VoiceService `this` dependency.
 *
 * Canonical codec order shared by runtime selection and Settings prediction.
 * Two-pass: HW-accelerated first, then SW fallback.
 */

import type { types as mediasoupTypes } from 'mediasoup-client';

// ─── Types ───────────────────────────────────────────────────────────

/** A codec cascade entry with primary MIME and optional alternates (e.g. H265/HEVC). */
export interface CodecCascadeEntry {
  /** Primary MIME + optional profile (e.g. 'video/H264:6400') */
  mimes: string[];
  /** Keep detected-but-unroutable codecs visible in the order without selecting them. */
  selectable?: boolean;
}

/** Callbacks that the VoiceService instance provides to the pure selection logic. */
export interface CodecLookup<T = mediasoupTypes.RtpCodecCapability> {
  isInCodecFloor: (key: string) => boolean;
  isHwAccelerated: (key: string) => boolean;
  isEligible?: (key: string) => boolean;
  findSendCodec: (key: string) => T | undefined;
}

export interface CodecCascadeConfig<T = mediasoupTypes.RtpCodecCapability> extends CodecLookup<T> {
  preferred: string | null;
  hwAccel: boolean;
  hdrEncoding: boolean;
}

export type H264ProfileClass =
  | 'constrained-baseline'
  | 'baseline'
  | 'main'
  | 'constrained-high'
  | 'high'
  | 'predictive-high-444';

interface H264ProfilePattern {
  profileIdc: number;
  profileIopMask: number;
  profileIopValue: number;
  profileClass: H264ProfileClass;
}

const H264_PROFILE_PATTERNS: readonly H264ProfilePattern[] = [
  {
    profileIdc: 0x42,
    profileIopMask: 0x4f,
    profileIopValue: 0x40,
    profileClass: 'constrained-baseline',
  },
  {
    profileIdc: 0x4d,
    profileIopMask: 0x8f,
    profileIopValue: 0x80,
    profileClass: 'constrained-baseline',
  },
  {
    profileIdc: 0x58,
    profileIopMask: 0xcf,
    profileIopValue: 0xc0,
    profileClass: 'constrained-baseline',
  },
  { profileIdc: 0x42, profileIopMask: 0x4f, profileIopValue: 0, profileClass: 'baseline' },
  { profileIdc: 0x58, profileIopMask: 0xcf, profileIopValue: 0x80, profileClass: 'baseline' },
  { profileIdc: 0x4d, profileIopMask: 0xaf, profileIopValue: 0, profileClass: 'main' },
  {
    profileIdc: 0x64,
    profileIopMask: 0xff,
    profileIopValue: 0x0c,
    profileClass: 'constrained-high',
  },
  { profileIdc: 0x64, profileIopMask: 0xff, profileIopValue: 0, profileClass: 'high' },
  {
    profileIdc: 0xf4,
    profileIopMask: 0xff,
    profileIopValue: 0,
    profileClass: 'predictive-high-444',
  },
];

/**
 * Classify an H.264 profile-level-id with the RFC 6184 profile_idc/profile_iop
 * masks used by mediasoup's negotiator. The level byte does not affect class.
 */
export function h264ProfileClass(profileLevelId: string): H264ProfileClass | null {
  if (!/^[0-9a-f]{6}$/i.test(profileLevelId)) return null;

  const profileIdc = Number.parseInt(profileLevelId.slice(0, 2), 16);
  const profileIop = Number.parseInt(profileLevelId.slice(2, 4), 16);
  const match = H264_PROFILE_PATTERNS.find(
    (pattern) =>
      pattern.profileIdc === profileIdc &&
      (profileIop & pattern.profileIopMask) === pattern.profileIopValue
  );
  return match?.profileClass ?? null;
}

/** H.264 negotiation compatibility under level asymmetry. */
export function h264ProfilesCompatible(left: string, right: string): boolean {
  const leftClass = h264ProfileClass(left);
  return leftClass !== null && leftClass === h264ProfileClass(right);
}

// ─── Cascade Construction ────────────────────────────────────────────

/** Build the ordered routable codec cascade. HEVC stays visible but disabled. */
export function buildCodecCascade(hdrEncoding: boolean): CodecCascadeEntry[] {
  const h264Remainder = [{ mimes: ['video/H264:4d0032'] }, { mimes: ['video/H264:42e01f'] }];

  if (hdrEncoding) {
    return [
      { mimes: ['video/AV1'] },
      { mimes: ['video/H265', 'video/HEVC'], selectable: false },
      { mimes: ['video/VP9:2'] },
      { mimes: ['video/H264:640034'] },
      { mimes: ['video/VP9:0'] },
      ...h264Remainder,
      { mimes: ['video/VP8'] },
    ];
  }

  return [
    { mimes: ['video/AV1'] },
    { mimes: ['video/H265', 'video/HEVC'], selectable: false },
    { mimes: ['video/VP9:0'] },
    { mimes: ['video/H264:640034'] },
    ...h264Remainder,
    { mimes: ['video/VP8'] },
  ];
}

const H264_RANK_BY_CLASS: Partial<Record<H264ProfileClass, number>> = {
  high: 0,
  'constrained-high': 1,
  main: 2,
  baseline: 3,
  'constrained-baseline': 4,
};

type CodecPriorityResolver = (profile: string, hdrEncoding: boolean) => number;

function vp9Priority(profile: string, hdrEncoding: boolean): number {
  if (profile === '2') return hdrEncoding ? 20 : 70;
  return hdrEncoding ? 40 : 20;
}

function h264Priority(profile: string, hdrEncoding: boolean): number {
  const profileClass = h264ProfileClass(profile);
  const rank = profileClass ? (H264_RANK_BY_CLASS[profileClass] ?? 5) : 5;
  if (hdrEncoding) return rank === 0 ? 30 : 50 + rank;
  return 30 + rank;
}

const CODEC_PRIORITY_RESOLVERS: Readonly<Record<string, CodecPriorityResolver>> = {
  'video/av1': () => 0,
  'video/h265': () => 10,
  'video/hevc': () => 10,
  'video/vp9': vp9Priority,
  'video/h264': h264Priority,
  'video/vp8': (_profile, hdrEncoding) => (hdrEncoding ? 70 : 50),
};

/** Stable display rank for detected codecs, including unavailable profiles. */
export function codecPriority(key: string, hdrEncoding: boolean): number {
  const [mime, profile = ''] = key.toLowerCase().split(':');
  return CODEC_PRIORITY_RESOLVERS[mime]?.(profile, hdrEncoding) ?? 99;
}

// ─── Cascade Search ──────────────────────────────────────────────────

/**
 * Search the cascade for the first codec that is floor-compatible and
 * passes an optional HW filter. Returns undefined if nothing matches.
 */
function findCodecInEntry<T>(
  entry: CodecCascadeEntry,
  lookup: CodecLookup<T>,
  requireHw: boolean
): T | undefined {
  for (const mime of entry.mimes) {
    if (requireHw && !lookup.isHwAccelerated(mime)) continue;
    if (!lookup.isInCodecFloor(mime)) continue;
    if (lookup.isEligible && !lookup.isEligible(mime)) continue;
    const codec = lookup.findSendCodec(mime);
    if (codec) return codec;
  }
  return undefined;
}

export function findFirstFloorCompatibleCodec<T>(
  cascade: CodecCascadeEntry[],
  lookup: CodecLookup<T>,
  requireHw: boolean
): T | undefined {
  for (const entry of cascade) {
    if (entry.selectable === false) continue;
    const codec = findCodecInEntry(entry, lookup, requireHw);
    if (codec) return codec;
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
 */
export function selectCodecFromCascade<T = mediasoupTypes.RtpCodecCapability>(
  config: CodecCascadeConfig<T>
): T | undefined {
  const { preferred, hwAccel, hdrEncoding, ...lookup } = config;

  // 1. User preference
  const preferredProfile = preferred?.split(':')[1];
  const preferredMime = preferred?.split(':')[0].toLowerCase();
  const preferredH264Class = preferredProfile ? h264ProfileClass(preferredProfile) : null;
  const h264ProfileAllowed =
    preferredMime !== 'video/h264' ||
    preferredProfile === undefined ||
    preferredH264Class === 'high' ||
    preferredH264Class === 'main' ||
    preferredH264Class === 'constrained-baseline';
  const preferredAllowed =
    h264ProfileAllowed &&
    preferredMime !== 'video/h265' &&
    preferredMime !== 'video/hevc' &&
    (hdrEncoding || !(preferredMime === 'video/vp9' && preferredProfile === '2'));
  if (
    preferred &&
    preferredAllowed &&
    lookup.isInCodecFloor(preferred) &&
    (!lookup.isEligible || lookup.isEligible(preferred))
  ) {
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

  return undefined;
}
