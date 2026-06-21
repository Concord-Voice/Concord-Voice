import { describe, it, expect } from 'vitest';
import type { types as mediasoupTypes } from 'mediasoup-client';
import {
  buildCodecCascade,
  findFirstFloorCompatibleCodec,
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
  it('returns cascade without VP9:2 when HDR is off', () => {
    const cascade = buildCodecCascade(false);
    const allMimes = cascade.flatMap((e) => e.mimes);
    expect(allMimes).not.toContain('video/VP9:2');
    expect(allMimes).toContain('video/AV1');
    expect(allMimes).toContain('video/VP9');
  });

  it('includes VP9:2 when HDR is on', () => {
    const cascade = buildCodecCascade(true);
    const allMimes = cascade.flatMap((e) => e.mimes);
    expect(allMimes).toContain('video/VP9:2');
  });

  it('includes both H265 and HEVC as alternates', () => {
    const cascade = buildCodecCascade(false);
    const h265Entry = cascade.find((e) => e.mimes.includes('video/H265'));
    expect(h265Entry).toBeDefined();
    expect(h265Entry!.mimes).toContain('video/HEVC');
  });
});

// ─── findFirstFloorCompatibleCodec ───────────────────────────────────

describe('findFirstFloorCompatibleCodec', () => {
  it('returns undefined for empty cascade', () => {
    const lookup = makeLookup({ available: {} });
    expect(findFirstFloorCompatibleCodec([], lookup, false)).toBeUndefined();
  });

  it('skips codecs not in floor', () => {
    const av1 = fakeCodec('video/AV1');
    const vp8 = fakeCodec('video/VP8');
    const lookup = makeLookup({
      floor: ['video/vp8'],
      available: { 'video/av1': av1, 'video/vp8': vp8 },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, false);
    // AV1 not in floor, should skip to something else. VP8 is not in cascade
    // (it's the last-resort), so result should be undefined since VP9 isn't available
    expect(result).toBeUndefined();
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

  it('resolves alternate MIME (HEVC) when primary (H265) not found', () => {
    const hevc = fakeCodec('video/HEVC');
    const lookup = makeLookup({
      available: { 'video/hevc': hevc },
    });
    const cascade = buildCodecCascade(false);
    const result = findFirstFloorCompatibleCodec(cascade, lookup, false);
    // AV1 not available, H265 not available, but HEVC is (alt for H265 entry)
    expect(result).toBe(hevc);
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

  it('falls back to VP8 when nothing in cascade matches', () => {
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

  it('includes VP9:2 in cascade when HDR enabled', () => {
    const vp9_2 = fakeCodec('video/VP9');
    const lookup = makeLookup({
      available: { 'video/vp9': vp9_2 },
    });
    // With HDR off, VP9:2 is before VP9 in cascade — but findSendCodec resolves
    // by base mime, so VP9:2 and VP9 both resolve to the same codec.
    // The important thing is the cascade includes the entry.
    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: true,
      ...lookup,
    });
    expect(result).toBe(vp9_2);
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
