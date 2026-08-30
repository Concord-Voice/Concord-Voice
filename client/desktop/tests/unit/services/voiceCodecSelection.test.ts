import { describe, it, expect } from 'vitest';
import type { types as mediasoupTypes } from 'mediasoup-client';
import * as codecSelection from '../../../src/renderer/services/voice/voiceCodecSelection';
import {
  buildCodecCandidates,
  buildCodecCascade,
  codecPriority,
  findFirstFloorCompatibleCodec,
  h264ProfileClass,
  h264ProfilesCompatible,
  isCodecKeyInFloor,
  selectCodecFromCascade,
  type CodecLookup,
} from '../../../src/renderer/services/voice/voiceCodecSelection';

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
  it('enumerates the full HDR-first hardware/software Auto order', () => {
    expect(buildCodecCandidates(true, true).map(({ key, backend }) => `${key}@${backend}`)).toEqual(
      [
        'video/AV1:hdr@hardware',
        'video/VP9:2@hardware',
        'video/AV1:hdr@software',
        'video/VP9:2@software',
        'video/AV1:sdr@hardware',
        'video/VP9:0@hardware',
        'video/H264:640034@hardware',
        'video/H264:4d0032@hardware',
        'video/H264:42e01f@hardware',
        'video/VP8@hardware',
        'video/AV1:sdr@software',
        'video/VP9:0@software',
        'video/H264:640034@software',
        'video/H264:4d0032@software',
        'video/H264:42e01f@software',
        'video/VP8@software',
      ]
    );
  });

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

describe('profile-aware codec floor', () => {
  it('maps AV1 targets, compares VP9 exactly, and compares H264 by profile class', () => {
    expect(isCodecKeyInFloor('video/AV1:hdr', ['video/av1'])).toBe(true);
    expect(isCodecKeyInFloor('video/AV1:sdr', ['video/av1'])).toBe(true);
    expect(isCodecKeyInFloor('video/VP8', ['video/vp8'])).toBe(true);

    expect(isCodecKeyInFloor('video/VP9:0', ['video/vp9:0'])).toBe(true);
    expect(isCodecKeyInFloor('video/VP9', ['video/vp9:0'])).toBe(true);
    expect(isCodecKeyInFloor('video/VP9:2', ['video/vp9:0'])).toBe(false);
    expect(isCodecKeyInFloor('video/VP9:0', ['video/vp9:2'])).toBe(false);

    expect(isCodecKeyInFloor('video/H264:64001f', ['video/h264:640034'])).toBe(true);
    expect(isCodecKeyInFloor('video/H264:640034', ['video/h264:4d0032'])).toBe(false);

    // Conservative compatibility with codec floors emitted before profile-aware servers.
    // A bare VP9 capability means the RFC default Profile 0; a bare H.264 capability
    // cannot prove support for any particular profile-level-id.
    expect(isCodecKeyInFloor('video/VP9:0', ['video/vp9'])).toBe(true);
    expect(isCodecKeyInFloor('video/VP9:2', ['video/vp9'])).toBe(false);
    expect(isCodecKeyInFloor('video/H264:640034', ['video/h264'])).toBe(false);
    expect(isCodecKeyInFloor('video/H264', ['video/h264'])).toBe(true);
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
  it('reports the selected backend and color target as policy metadata', () => {
    const selectCandidate = (
      codecSelection as typeof codecSelection & {
        selectCodecCandidate?: (config: unknown) => {
          codec: mediasoupTypes.RtpCodecCapability;
          candidate: { backend: string; colorTarget: string; key: string };
        };
      }
    ).selectCodecCandidate;
    expect(selectCandidate).toBeTypeOf('function');
    if (!selectCandidate) return;
    const av1Hdr = fakeCodec('video/AV1');

    const selected = selectCandidate({
      preferred: null,
      hwAccel: true,
      hdrEncoding: true,
      isInCodecFloor: () => true,
      isHwAccelerated: () => true,
      findSendCodec: (key: string) => (key.toLowerCase() === 'video/av1:hdr' ? av1Hdr : undefined),
    });

    expect(selected).toEqual({
      codec: av1Hdr,
      candidate: { key: 'video/AV1:hdr', backend: 'hardware', colorTarget: 'hdr' },
    });
  });

  it('uses the exact HDR-first backend order before falling back to SDR', () => {
    const av1Hdr = fakeCodec('video/AV1-hdr');
    const vp9Hdr = fakeCodec('video/VP9-hdr');
    const av1Sdr = fakeCodec('video/AV1-sdr');
    const vp9Sdr = fakeCodec('video/VP9-sdr');
    const available: Record<string, mediasoupTypes.RtpCodecCapability> = {
      'video/av1:hdr': av1Hdr,
      'video/vp9:2': vp9Hdr,
      'video/av1:sdr': av1Sdr,
      'video/vp9:0': vp9Sdr,
    };
    const select = (hardware: string[]) =>
      selectCodecFromCascade({
        preferred: null,
        hwAccel: true,
        hdrEncoding: true,
        isInCodecFloor: () => true,
        isHwAccelerated: (key) => hardware.includes(key.toLowerCase()),
        findSendCodec: (key) => available[key.toLowerCase()],
      });

    expect(select(['video/av1:hdr', 'video/vp9:2', 'video/av1:sdr', 'video/vp9:0'])).toBe(av1Hdr);
    expect(select(['video/vp9:2', 'video/av1:sdr', 'video/vp9:0'])).toBe(vp9Hdr);
    expect(select(['video/av1:sdr', 'video/vp9:0'])).toBe(av1Hdr);
    expect(select(['video/vp9:0'])).toBe(av1Hdr);
  });

  it('omits every hardware candidate when hardware acceleration is off', () => {
    const av1Hdr = fakeCodec('video/AV1-hdr');
    const available = { 'video/av1:hdr': av1Hdr };
    const hardwareChecks: string[] = [];

    const result = selectCodecFromCascade({
      preferred: null,
      hwAccel: false,
      hdrEncoding: true,
      isInCodecFloor: () => true,
      isHwAccelerated: (key) => {
        hardwareChecks.push(key);
        return true;
      },
      findSendCodec: (key) => available[key.toLowerCase() as keyof typeof available],
    });

    expect(result).toBe(av1Hdr);
    expect(hardwareChecks).toEqual([]);
  });

  it('discards a backend-incompatible manual preference and restarts Auto at candidate zero', () => {
    const manualSdr = fakeCodec('video/AV1-sdr');
    const autoHdr = fakeCodec('video/AV1-hdr');
    const available: Record<string, mediasoupTypes.RtpCodecCapability> = {
      'video/av1:sdr': manualSdr,
      'video/av1:hdr': autoHdr,
    };

    const result = selectCodecFromCascade({
      preferred: 'video/AV1:sdr',
      hwAccel: true,
      hdrEncoding: true,
      isInCodecFloor: () => true,
      isHwAccelerated: (key) => key.toLowerCase() === 'video/av1:hdr',
      findSendCodec: (key) => available[key.toLowerCase()],
    });

    expect(result).toBe(autoHdr);
  });

  it('restarts Auto at candidate zero when a bottom-ranked manual codec misses the floor', () => {
    const av1Hdr = fakeCodec('video/AV1-hdr');
    const vp8 = fakeCodec('video/VP8');
    const available: Record<string, mediasoupTypes.RtpCodecCapability> = {
      'video/av1:hdr': av1Hdr,
      'video/vp8': vp8,
    };

    const result = selectCodecFromCascade({
      preferred: 'video/VP8',
      hwAccel: false,
      hdrEncoding: true,
      isInCodecFloor: (key) => key.toLowerCase().startsWith('video/av1:'),
      isHwAccelerated: () => false,
      findSendCodec: (key) => available[key.toLowerCase()],
    });

    expect(result).toBe(av1Hdr);
  });

  it('honors a legacy bare H264 preference through its best eligible hardware profile', () => {
    const av1 = fakeCodec('video/AV1');
    const h264Main = fakeCodec('video/H264');
    const available: Record<string, mediasoupTypes.RtpCodecCapability> = {
      'video/av1:sdr': av1,
      'video/h264:4d0032': h264Main,
    };

    const result = selectCodecFromCascade({
      preferred: 'video/H264',
      hwAccel: true,
      hdrEncoding: false,
      isInCodecFloor: () => true,
      isHwAccelerated: (key) =>
        key.toLowerCase() === 'video/av1:sdr' || key.toLowerCase() === 'video/h264:4d0032',
      findSendCodec: (key) => available[key.toLowerCase()],
    });

    expect(result).toBe(h264Main);
  });

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

  it('keeps an HDR software profile ahead of an SDR hardware profile', () => {
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

    expect(result).toBe(vp9Profile2);
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
