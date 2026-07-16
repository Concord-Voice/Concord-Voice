/**
 * Codec cascade selection logic extracted from VoiceService to reduce
 * cognitive complexity. Pure functions — no VoiceService `this` dependency.
 *
 * Canonical codec order shared by runtime selection and Settings prediction.
 * HDR targets lead SDR targets even when that crosses the hardware/software
 * boundary.
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

export type CodecBackendTarget = 'hardware' | 'software';
export type CodecColorTarget = 'hdr' | 'sdr';

export interface CodecCandidate {
  key: string;
  backend: CodecBackendTarget;
  colorTarget: CodecColorTarget;
}

export interface SelectedCodecCandidate<T> {
  codec: T;
  candidate: CodecCandidate;
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

/** Compare an application codec target with the room's RTP codec floor. */
export function isCodecKeyInFloor(key: string, floor: string[] | null): boolean {
  if (!floor) return true;
  const [mime, rawProfile] = key.toLowerCase().split(':');
  const profile = mime === 'video/vp9' ? (rawProfile ?? '0') : rawProfile;

  return floor.some((entry) => {
    const [floorMime, floorProfile] = entry.toLowerCase().split(':');
    if (floorMime !== mime) return false;
    if (mime === 'video/av1') return true;
    if (floorProfile === undefined) {
      if (mime === 'video/vp9') return profile === '0';
      if (mime === 'video/h264') return rawProfile === undefined;
      return true;
    }
    if (mime === 'video/vp9') return profile === floorProfile;
    if (mime === 'video/h264') {
      return profile !== undefined && h264ProfilesCompatible(profile, floorProfile);
    }
    return profile === floorProfile;
  });
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

const HDR_CODEC_KEYS = ['video/AV1:hdr', 'video/VP9:2'] as const;
const H264_CODEC_KEYS = ['video/H264:640034', 'video/H264:4d0032', 'video/H264:42e01f'] as const;
const SDR_CODEC_KEYS = ['video/AV1:sdr', 'video/VP9:0', ...H264_CODEC_KEYS, 'video/VP8'] as const;

function candidates(
  keys: readonly string[],
  backend: CodecBackendTarget,
  colorTarget: CodecColorTarget
): CodecCandidate[] {
  return keys.map((key) => ({ key, backend, colorTarget }));
}

/** Exact Auto policy. Hardware candidates are absent when acceleration is off. */
export function buildCodecCandidates(hdrEncoding: boolean, hwAccel: boolean): CodecCandidate[] {
  const result: CodecCandidate[] = [];
  if (hdrEncoding) {
    if (hwAccel) result.push(...candidates(HDR_CODEC_KEYS, 'hardware', 'hdr'));
    result.push(...candidates(HDR_CODEC_KEYS, 'software', 'hdr'));
  }
  if (hwAccel) result.push(...candidates(SDR_CODEC_KEYS, 'hardware', 'sdr'));
  result.push(...candidates(SDR_CODEC_KEYS, 'software', 'sdr'));
  return result;
}

/** Normalize legacy family-only preferences without changing their stored type. */
export function normalizeCodecTargetKey(key: string, hdrEncoding: boolean): string {
  const [mime, profile] = key.split(':');
  const normalizedMime = mime.toLowerCase();
  if (normalizedMime === 'video/av1') {
    let target = profile?.toLowerCase();
    if (target !== 'hdr' && target !== 'sdr') target = hdrEncoding ? 'hdr' : 'sdr';
    return `video/AV1:${target}`;
  }
  if (normalizedMime === 'video/vp9' && profile === undefined) return 'video/VP9:0';
  return key;
}

function candidateColorTarget(key: string): CodecColorTarget {
  const normalized = key.toLowerCase();
  return normalized === 'video/av1:hdr' || normalized === 'video/vp9:2' ? 'hdr' : 'sdr';
}

function codecAllowedAsPreference(key: string, hdrEncoding: boolean): boolean {
  const [mime, profile] = key.toLowerCase().split(':');
  if (mime === 'video/h265' || mime === 'video/hevc') return false;
  if (candidateColorTarget(key) === 'hdr' && !hdrEncoding) return false;
  if (mime !== 'video/h264' || profile === undefined) return true;
  const profileClass = h264ProfileClass(profile);
  return (
    profileClass === 'high' || profileClass === 'main' || profileClass === 'constrained-baseline'
  );
}

function findCandidate<T>(
  candidate: CodecCandidate,
  lookup: CodecLookup<T>
): SelectedCodecCandidate<T> | undefined {
  if (candidate.backend === 'hardware' && !lookup.isHwAccelerated(candidate.key)) return undefined;
  if (!lookup.isInCodecFloor(candidate.key)) return undefined;
  if (lookup.isEligible && !lookup.isEligible(candidate.key)) return undefined;
  const codec = lookup.findSendCodec(candidate.key);
  return codec ? { codec, candidate } : undefined;
}

function findPreferredCandidate<T>(
  preferred: string,
  hwAccel: boolean,
  hdrEncoding: boolean,
  lookup: CodecLookup<T>
): SelectedCodecCandidate<T> | undefined {
  const normalized = normalizeCodecTargetKey(preferred, hdrEncoding);
  const preferredKeys = normalized.toLowerCase() === 'video/h264' ? H264_CODEC_KEYS : [normalized];
  for (const key of preferredKeys) {
    if (!codecAllowedAsPreference(key, hdrEncoding)) continue;
    const selected = findCandidate(
      {
        key,
        backend: hwAccel ? 'hardware' : 'software',
        colorTarget: candidateColorTarget(key),
      },
      lookup
    );
    if (selected) return selected;
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
export function selectCodecCandidate<T = mediasoupTypes.RtpCodecCapability>(
  config: CodecCascadeConfig<T>
): SelectedCodecCandidate<T> | undefined {
  const { preferred, hwAccel, hdrEncoding, ...lookup } = config;

  if (preferred) {
    const selected = findPreferredCandidate(preferred, hwAccel, hdrEncoding, lookup);
    if (selected) return selected;
  }

  for (const candidate of buildCodecCandidates(hdrEncoding, hwAccel)) {
    const selected = findCandidate(candidate, lookup);
    if (selected) return selected;
  }

  return undefined;
}

export function selectCodecFromCascade<T = mediasoupTypes.RtpCodecCapability>(
  config: CodecCascadeConfig<T>
): T | undefined {
  return selectCodecCandidate(config)?.codec;
}
