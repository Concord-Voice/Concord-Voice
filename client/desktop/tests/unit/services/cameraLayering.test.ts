import { describe, expect, it } from 'vitest';
import type { types as mediasoupTypes } from 'mediasoup-client';
import {
  buildCameraEncodingPlan,
  castingKindForCodec,
  firstEligibleLayeringKey,
  resolveCameraScalabilityMode,
  type CameraLayeringPriority,
  type CastingEligibility,
} from '../../../src/renderer/services/cameraLayering';

function codec(mimeType: string): mediasoupTypes.RtpCodecCapability {
  return {
    kind: 'video',
    mimeType,
    clockRate: 90000,
    parameters: {},
  } as mediasoupTypes.RtpCodecCapability;
}

const priority: CameraLayeringPriority = { priority: 'medium', networkPriority: 'medium' };
const ALL_ELIGIBLE: CastingEligibility = { svc: true, simulcast: true };

describe('resolveCameraScalabilityMode', () => {
  it('maps auto to L3T3_KEY', () => {
    expect(resolveCameraScalabilityMode('auto')).toBe('L3T3_KEY');
  });

  it('adds KEY to multi-spatial camera modes', () => {
    expect(resolveCameraScalabilityMode('L2T3')).toBe('L2T3_KEY');
    expect(resolveCameraScalabilityMode('L3T3')).toBe('L3T3_KEY');
  });

  it('leaves L1T3 unchanged', () => {
    expect(resolveCameraScalabilityMode('L1T3')).toBe('L1T3');
  });
});

describe('buildCameraEncodingPlan', () => {
  it('uses one SVC encoding for AV1', () => {
    const plan = buildCameraEncodingPlan({
      codec: codec('video/AV1'),
      maxBitrate: 2_500_000,
      scalabilityMode: 'auto',
      priority,
      eligibility: ALL_ELIGIBLE,
    });
    expect(plan.kind).toBe('svc');
    expect(plan.encodings).toEqual([
      { maxBitrate: 2_500_000, scalabilityMode: 'L3T3_KEY', ...priority },
    ]);
  });

  it('uses one SVC encoding for VP9', () => {
    const plan = buildCameraEncodingPlan({
      codec: codec('video/VP9'),
      maxBitrate: 2_500_000,
      scalabilityMode: 'L2T3',
      priority,
      eligibility: ALL_ELIGIBLE,
    });
    expect(plan.kind).toBe('svc');
    expect(plan.encodings[0]).toMatchObject({
      maxBitrate: 2_500_000,
      scalabilityMode: 'L2T3_KEY',
    });
  });

  it('uses low/mid/high RID simulcast for H264', () => {
    const plan = buildCameraEncodingPlan({
      codec: codec('video/H264'),
      maxBitrate: 2_500_000,
      scalabilityMode: 'auto',
      priority,
      eligibility: ALL_ELIGIBLE,
    });
    expect(plan.kind).toBe('simulcast');
    expect(plan.encodings).toEqual([
      { rid: 'q', scaleResolutionDownBy: 4, maxBitrate: 250_000, ...priority },
      { rid: 'h', scaleResolutionDownBy: 2, maxBitrate: 800_000, ...priority },
      { rid: 'f', scaleResolutionDownBy: 1, maxBitrate: 2_500_000, ...priority },
    ]);
  });

  it('uses VP8 as simulcast fallback', () => {
    const plan = buildCameraEncodingPlan({
      codec: codec('video/VP8'),
      maxBitrate: 900_000,
      scalabilityMode: 'auto',
      priority,
      eligibility: ALL_ELIGIBLE,
    });
    expect(plan.kind).toBe('simulcast');
    expect(plan.encodings.map((e) => e.rid)).toEqual(['q', 'h', 'f']);
    expect(plan.encodings[2].maxBitrate).toBe(900_000);
  });

  it('returns single-layer fallback for an unsupported codec', () => {
    const plan = buildCameraEncodingPlan({
      codec: codec('video/unknown'),
      maxBitrate: 1_500_000,
      scalabilityMode: 'auto',
      priority,
      eligibility: ALL_ELIGIBLE,
    });
    expect(plan.kind).toBe('single');
    expect(plan.encodings).toEqual([{ maxBitrate: 1_500_000, ...priority }]);
  });
});

const planBase = { maxBitrate: 1_000_000, scalabilityMode: 'auto' as const, priority };

describe('castingKindForCodec (#1921)', () => {
  it.each([
    ['video/av1', 'svc'],
    ['video/vp9', 'svc'],
    ['video/h264', 'simulcast'],
    ['video/vp8', 'simulcast'],
    ['video/foo', 'single'],
  ])('%s → %s', (mime, kind) => expect(castingKindForCodec(mime)).toBe(kind));

  it('is case-insensitive', () => {
    expect(castingKindForCodec('video/AV1')).toBe('svc');
    expect(castingKindForCodec('video/H264')).toBe('simulcast');
  });
});

describe('buildCameraEncodingPlan eligibility (#1921)', () => {
  it('AV1 with svc eligible → svc', () =>
    expect(
      buildCameraEncodingPlan({ ...planBase, codec: codec('video/av1'), eligibility: ALL_ELIGIBLE })
        .kind
    ).toBe('svc'));
  it('AV1 with svc INELIGIBLE → single', () =>
    expect(
      buildCameraEncodingPlan({
        ...planBase,
        codec: codec('video/av1'),
        eligibility: { svc: false, simulcast: true },
      }).kind
    ).toBe('single'));
  it('H264 with simulcast eligible → simulcast', () =>
    expect(
      buildCameraEncodingPlan({
        ...planBase,
        codec: codec('video/h264'),
        eligibility: ALL_ELIGIBLE,
      }).kind
    ).toBe('simulcast'));
  it('H264 with simulcast INELIGIBLE → single', () =>
    expect(
      buildCameraEncodingPlan({
        ...planBase,
        codec: codec('video/h264'),
        eligibility: { svc: true, simulcast: false },
      }).kind
    ).toBe('single'));
  it('AV1 is NEVER simulcast under any eligibility (E2EE invariant)', () => {
    for (const e of [
      { svc: true, simulcast: true },
      { svc: false, simulcast: true },
      { svc: true, simulcast: false },
      { svc: false, simulcast: false },
    ])
      expect(
        buildCameraEncodingPlan({ ...planBase, codec: codec('video/av1'), eligibility: e }).kind
      ).not.toBe('simulcast');
  });
});

describe('firstEligibleLayeringKey (#1921)', () => {
  const FLOOR = ['video/AV1', 'video/VP9', 'video/H264', 'video/VP8'];
  it('prefers AV1 (svc) when svc eligible', () =>
    expect(firstEligibleLayeringKey(FLOOR, { svc: true, simulcast: true })).toBe('video/AV1'));
  it('skips svc codecs to first simulcast codec when svc off', () =>
    expect(firstEligibleLayeringKey(FLOOR, { svc: false, simulcast: true })).toBe('video/H264'));
  it('returns undefined when both off (caller falls back → single)', () =>
    expect(firstEligibleLayeringKey(FLOOR, { svc: false, simulcast: false })).toBeUndefined());
  it('classifies on the mime prefix, ignoring a :profile suffix', () =>
    expect(firstEligibleLayeringKey(['video/H264:640034'], { svc: false, simulcast: true })).toBe(
      'video/H264:640034'
    ));
});

// Screenshare is SVC-ONLY in v1 (#1921): pickScreenCodec calls buildCameraEncodingPlan
// with eligibility.simulcast hard-false. These lock the shared plan's screen contract.
describe('screenshare SVC-only plan (#1921)', () => {
  const SCREEN_ELIGIBILITY = { svc: true, simulcast: false };
  it('AV1 + supportSvc → svc (1 encoding)', () => {
    const p = buildCameraEncodingPlan({
      ...planBase,
      codec: codec('video/av1'),
      eligibility: SCREEN_ELIGIBILITY,
    });
    expect(p.kind).toBe('svc');
    expect(p.encodings).toHaveLength(1);
  });
  it('H264 never simulcasts (simulcast forced off for screen)', () => {
    const p = buildCameraEncodingPlan({
      ...planBase,
      codec: codec('video/h264'),
      eligibility: SCREEN_ELIGIBILITY,
    });
    expect(p.kind).toBe('single');
  });
  it('AV1 + supportSvc OFF → single (forced)', () => {
    const p = buildCameraEncodingPlan({
      ...planBase,
      codec: codec('video/av1'),
      eligibility: { svc: false, simulcast: false },
    });
    expect(p.kind).toBe('single');
  });
});
