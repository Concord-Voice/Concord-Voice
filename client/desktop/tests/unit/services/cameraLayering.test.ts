import { describe, expect, it } from 'vitest';
import type { types as mediasoupTypes } from 'mediasoup-client';
import {
  buildCameraEncodingPlan,
  resolveCameraScalabilityMode,
  type CameraLayeringPriority,
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
    });
    expect(plan.kind).toBe('single');
    expect(plan.encodings).toEqual([{ maxBitrate: 1_500_000, ...priority }]);
  });
});
