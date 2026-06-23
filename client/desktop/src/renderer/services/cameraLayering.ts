import type { types as mediasoupTypes } from 'mediasoup-client';

export type CameraScalabilitySetting = 'auto' | 'L1T3' | 'L2T3' | 'L3T3';
export type CameraLayeringKind = 'svc' | 'simulcast' | 'single';
export type CameraLayeringPriority = Pick<
  mediasoupTypes.RtpEncodingParameters,
  'priority' | 'networkPriority'
>;

export interface CameraEncodingPlan {
  kind: CameraLayeringKind;
  encodings: mediasoupTypes.RtpEncodingParameters[];
}

export interface BuildCameraEncodingPlanInput {
  codec?: mediasoupTypes.RtpCodecCapability;
  maxBitrate: number;
  scalabilityMode: CameraScalabilitySetting;
  priority: CameraLayeringPriority;
}

export function resolveCameraScalabilityMode(mode: CameraScalabilitySetting): string {
  if (mode === 'auto') return 'L3T3_KEY';
  if (mode === 'L2T3' || mode === 'L3T3') return `${mode}_KEY`;
  return mode;
}

function codecMime(codec?: mediasoupTypes.RtpCodecCapability): string {
  return codec?.mimeType?.toLowerCase() ?? '';
}

function lowBitrate(maxBitrate: number): number {
  return Math.min(250_000, Math.max(120_000, Math.round(maxBitrate * 0.1)));
}

function midBitrate(maxBitrate: number): number {
  return Math.min(800_000, Math.max(300_000, Math.round(maxBitrate * 0.32)));
}

export function buildCameraEncodingPlan(input: BuildCameraEncodingPlanInput): CameraEncodingPlan {
  const mime = codecMime(input.codec);
  const base = { ...input.priority };

  if (mime === 'video/av1' || mime === 'video/vp9') {
    return {
      kind: 'svc',
      encodings: [
        {
          ...base,
          maxBitrate: input.maxBitrate,
          scalabilityMode: resolveCameraScalabilityMode(input.scalabilityMode),
        },
      ],
    };
  }

  if (mime === 'video/h264' || mime === 'video/vp8') {
    return {
      kind: 'simulcast',
      encodings: [
        { ...base, rid: 'q', scaleResolutionDownBy: 4, maxBitrate: lowBitrate(input.maxBitrate) },
        { ...base, rid: 'h', scaleResolutionDownBy: 2, maxBitrate: midBitrate(input.maxBitrate) },
        { ...base, rid: 'f', scaleResolutionDownBy: 1, maxBitrate: input.maxBitrate },
      ],
    };
  }

  return { kind: 'single', encodings: [{ ...base, maxBitrate: input.maxBitrate }] };
}
