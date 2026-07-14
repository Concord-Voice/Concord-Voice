import { describe, it, expect } from 'vitest';
import type { types as mediasoupTypes } from 'mediasoup-client';
import {
  buildCodecCascade,
  codecPriority,
  findFirstFloorCompatibleCodec,
  h264ProfileClass,
  h264ProfilesCompatible,
  selectCodecFromCascade,
  type CodecLookup,
} from '../../../src/renderer/services/voiceCodecSelection';

// ─── Helpers ─────────────────────────────────────────────────────────

/** Create a fake RtpCodecCapability. */
function fakeCodec(mimeType: string): mediasoupTypes.RtpCodecCapability {
  return {
    mimeType,
    kind: 'video',
    clockRate: 90000,
    preferredPayloadType: 96,
    parameters: {},
  } as mediasoupTypes.RtpCodecCapability;
}

/** Build a CodecLookup with configurable floor, HW, and available codecs. */
function makeLookup(opts: {
  floor?: string[];
  hwCodecs?: string[];
  available?: Record<string, mediasoupTypes.RtpCodecCapability>;
}): CodecLookup {
  const floor = opts.floor; // undefined = no floor (all pass)
  const hwSet = new Set((opts.hwCodecs ?? []).map((m) => m.toLowerCase()));
  const codecMap = opts.available ?? {};

  return {
    isInCodecFloor: (key: string) => {
      if (!floor) return true;
      return floor.includes(key.split(':')[0].toLowerCase());
    },
    isHwAccelerated: (key: string) => hwSet.has(key.split(':')[0].toLowerCase()),
    findSendCodec: (key: string) => {
      // Try exact key first, then base mime
      return codecMap[key.toLowerCase()] ?? codecMap[key.split(':')[0].toLowerCase()];
    },
  };
}

// ─── buildCodecCascade ───────────────────────────────────────────────

describe('buildCodecCascade', () => {
  it('returns the canonical SDR order', () => {
    const cascade = buildCodecCascade(false);
    const allMimes = cascade.flatMap((e) => e.mimes);
    expect(allMimes).toEqual([
      'video/AV1',
      'video/H265',
      'video/HEVC',
      'video/VP9:0',
      'video/H264:640034',
      'video/H264:4d0032',
      'video/H264:42e01f',
      'video/VP8',
    ]);
  });

  it('returns the canonical HDR-preference order', () => {
    const cascade = buildCodecCascade(true);
    const allMimes = cascade.flatMap((e) => e.mimes);
    expect(allMimes).toEqual([
      'video/AV1',
      'video/H265',
      'video/HEVC',
      'video/VP9:2',
      'video/H264:640034',
      'video/VP9:0',
      'video/H264:4d0032',
      'video/H264:42e01f',
      'video/VP8',
    ]);
  });

  it('includes both H265 and HEVC as alternates', () => {
    const cascade = buildCodecCascade(false);
    const h265Entry = cascade.find((e) => e.mimes.includes('video/H265'));
    expect(h265Entry).toBeDefined();
    expect(h265Entry!.mimes).toContain('video/HEVC');
    expect(h265Entry!.selectable).toBe(false);
  });

  it('ranks detected H264 profiles best-to-worst', () => {
    expect(
      ['640034', '640c1f', '4d0032', '4d401f', '42001f', '42e01f', '4d801f'].map((profile) =>
        codecPriority(`video/H264:${profile}`, false)
      )
    ).toEqual([30, 31, 32, 32, 33, 34, 34]);
  });
});

// ─── H.264 profile identity ─────────────────────────────────────────

describe('H264 profile identity', () => {
  it('classifies every RFC 6184 Constrained Baseline representation', () => {
    expect(['42e01f', '4d801f', '4de01f', '58c01f', '58f01f'].map(h264ProfileClass)).toEqual([
      'constrained-baseline',
      'constrained-baseline',
      'constrained-baseline',
      'constrained-baseline',
      'constrained-baseline',
    ]);
  });

  it('matches profile class while ignoring level and keeps Constrained High distinct', () => {
    expect(h264ProfilesCompatible('640034', '64001f')).toBe(true);
    expect(h264ProfilesCompatible('4d0032', '4d401f')).toBe(true);
    expect(h264ProfilesCompatible('42e01f', '4d801f')).toBe(true);
    expect(h264ProfilesCompatible('42e01f', '58c01f')).toBe(true);
    expect(h264ProfilesCompatible('640034', '640c1f')).toBe(false);
  });
});

// ─── findFirstFloorCompatibleCodec ───────────────────────────────────

describe('findFirstFloorCompatibleCodec', () => {
  it('returns undefined for empty cascade', () => {
    const lookup = makeLookup({ available: {} });
    expect(findFirstFloorCompatibleCodec([], lookup, false)).toBeUndefined();
  });

  it('skips codecs not in floor and keeps VP8 inside the floor check', () => {
    const av1 = fakeCodec('video/AV1');
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      floor: ['video/vp8'],
      available: { 'video/av1': av1, 'video/vp8': vp8 },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, false);
    expect(result).toBe(vp8);
  });

  it('picks first HW codec when requireHw is true', () => {
    const av1 = fakeCodec('video/AV1');
    const h264 = fakeCodec('video/H264');
    const lookup = makeLookup({
      hwCodecs: ['video/h264'],
      available: { 'video/av1': av1, 'video/h264': h264 },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, true);
    expect(result).toBe(h264);
  });

  it('picks first available codec when requireHw is false', () => {
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({
      available: { 'video/av1': av1 },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, false);
    expect(result).toBe(av1);
  });

  it('never returns detected HEVC while it is unavailable in the router', () => {
    const hevc = fakeCodec('video/HEVC');
    const lookup = makeLookup({
      available: { 'video/hevc': hevc },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, false);
    expect(result).toBeUndefined();
  });
});

// ─── selectCodecFromCascade ──────────────────────────────────────────

describe('selectCodecFromCascade', () => {
  it('returns user-preferred codec when floor-compatible', () => {
    const vp9 = fakeCodec('video/VP9');
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({
      available: { 'video/vp9': vp9, 'video/av1': av1 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/VP9',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp9);
  });

  it('skips user preference when not in floor', () => {
    const vp9 = fakeCodec('video/VP9');
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({
      floor: ['video/av1'],
      available: { 'video/vp9': vp9, 'video/av1': av1 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/VP9',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(av1);
  });

  it('prefers HW codec over SW when hwAccel is true', () => {
    const av1 = fakeCodec('video/AV1');
    const h264 = fakeCodec('video/H264');
    const lookup = makeLookup({
      hwCodecs: ['video/h264'],
      available: { 'video/av1': av1, 'video/h264': h264 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: true,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(h264);
  });

  it('prefers hardware VP9 over hardware H264 in Auto mode (regression for #2242)', () => {
    const av1 = fakeCodec('video/AV1');
    const vp9 = fakeCodec('video/VP9');
    const h264 = fakeCodec('video/H264');
    const lookup = makeLookup({
      hwCodecs: ['video/vp9', 'video/h264'],
      available: { 'video/av1': av1, 'video/vp9': vp9, 'video/h264': h264 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: true,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp9);
  });

  it('falls back to SW when no HW codec available', () => {
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({
      hwCodecs: [],
      available: { 'video/av1': av1 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: true,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(av1);
  });

  it('falls back to floor-compatible VP8 when no higher-priority codec matches', () => {
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      floor: ['video/vp8'],
      available: { 'video/vp8': vp8 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp8);
  });

  it('returns undefined when no codecs available at all', () => {
    const lookup = makeLookup({ available: {} });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBeUndefined();
  });

  it('respects cascade order: AV1 before H264 when both available', () => {
    const av1 = fakeCodec('video/AV1');
    const h264 = fakeCodec('video/H264');
    const lookup = makeLookup({
      hwCodecs: ['video/h264'],
      available: { 'video/av1': av1, 'video/h264': h264 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(av1);
  });

  it('selects VP9 Profile 2 ahead of Profile 0 when HDR preference is enabled', () => {
    const vp9_2 = fakeCodec('video/VP9');
    const vp9_0 = fakeCodec('video/VP9');
    const lookup = makeLookup({
      available: { 'video/vp9:2': vp9_2, 'video/vp9:0': vp9_0 },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: true,
      ...lookup,
    });
    expect(result).toBe(vp9_2);
  });

  it('uses the exact profile hardware verdict before falling back to software', () => {
    const vp9Profile2 = fakeCodec('video/VP9');
    const vp9Profile0 = fakeCodec('video/VP9');
    const available: Record<string, mediasoupTypes.RtpCodecCapability> = {
      'video/vp9:2': vp9Profile2,
      'video/vp9:0': vp9Profile0,
    };
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: true,
      hdrEncoding: true,
      isInCodecFloor: () => true,
      isHwAccelerated: (key) => key.toLowerCase() === 'video/vp9:0',
      findSendCodec: (key) => available[key.toLowerCase()],
    });

    expect(result).toBe(vp9Profile0);
  });

  it('never honors a manual HEVC preference while HEVC is disabled', () => {
    const hevc = fakeCodec('video/HEVC');
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      available: { 'video/hevc': hevc, 'video/vp8': vp8 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/HEVC',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp8);
  });

  it('never honors an unavailable H264 profile class as a manual preference', () => {
    const browserBaseline = fakeCodec('video/H264');
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      available: { 'video/h264:42001f': browserBaseline, 'video/vp8': vp8 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/H264:42001f',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp8);
  });

  it('honors a lower-level H264 preference in a configured profile class', () => {
    const localHigh = fakeCodec('video/H264');
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({
      available: { 'video/h264:64001f': localHigh, 'video/av1': av1 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/H264:64001f',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(localHigh);
  });

  it('does not retain a persisted VP9 Profile 2 preference when HDR is off', () => {
    const vp9Hdr = fakeCodec('video/VP9');
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      available: { 'video/vp9:2': vp9Hdr, 'video/vp8': vp8 },
    });
    const result = selectCodecFromCascade({
      preferred: 'video/VP9:2',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(vp8);
  });

  it('filters the shared priority through layering eligibility', () => {
    const av1 = fakeCodec('video/AV1');
    const vp9 = fakeCodec('video/VP9');
    const h264 = fakeCodec('video/H264');
    const lookup = makeLookup({
      available: {
        'video/av1': av1,
        'video/vp9:0': vp9,
        'video/h264:640034': h264,
      },
    });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
      isEligible: (key) => !key.startsWith('video/AV1') && !key.startsWith('video/VP9'),
    });
    expect(result).toBe(h264);
  });

  it('handles null preferred gracefully', () => {
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({ available: { 'video/av1': av1 } });
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(av1);
  });

  it('handles empty string preferred gracefully', () => {
    const av1 = fakeCodec('video/AV1');
    const lookup = makeLookup({ available: { 'video/av1': av1 } });
    const result = selectCodecFromCascade({
      preferred: '',
      hwAccel: false,
      hdrEncoding: false,
      ...lookup,
    });
    expect(result).toBe(av1);
  });
});
