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
  eligibility: CastingEligibility;
}

/** Casting-eligibility allow-list (#1921). Each flag GATES a casting kind; it never
 *  forces one. AV1/VP9 publish SVC, H264/VP8 publish simulcast — so AV1+simulcast is
 *  structurally unreachable: no codec maps to both kinds. */
export interface CastingEligibility {
  svc: boolean;
  simulcast: boolean;
}

/** Pure codec→casting-kind classifier. Codec-derived (not toggle-derived): the toggles
 *  only SUBTRACT eligibility downstream; this never returns simulcast for an SVC codec. */
export function castingKindForCodec(mime: string): CameraLayeringKind {
  const m = mime.toLowerCase();
  if (m === 'video/av1' || m === 'video/vp9') return 'svc';
  if (m === 'video/h264' || m === 'video/vp8') return 'simulcast';
  return 'single';
}

/** Whether a casting kind is permitted by the eligibility allow-list. `single` is always
 *  allowed (it is the forced fallback when a layered kind is ineligible). */
export function isCastingEligible(kind: CameraLayeringKind, e: CastingEligibility): boolean {
  if (kind === 'svc') return e.svc;
  if (kind === 'simulcast') return e.simulcast;
  return true; // single is always allowed
}

/** First codec KEY in `candidates` whose casting kind is eligible. Key may carry a
 *  ':profile' suffix (e.g. 'video/H264:640034') — classify on the mime prefix. */
export function firstEligibleLayeringKey(
  candidates: string[],
  e: CastingEligibility
): string | undefined {
  return candidates.find((key) => isCastingEligible(castingKindForCodec(key.split(':')[0]), e));
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

/**
 * Per-RID caps for a 3-layer simulcast ladder seeded from ONE user-facing ceiling.
 *
 * The ceiling is the `f`-layer cap; `q` and `h` are derived from it. Exported because
 * the live-update path (`voiceService.liveUpdateScreenBitrate`) must produce the same
 * ladder a fresh produce would — #2208 shipped a flat cap that wrote the `f` value to
 * every layer, which raised the aggregate ceiling ~2.5x and, on a LOWERED setting,
 * raised `q` and `h` above what produce-time gives them. Both paths call this, so the
 * ratios cannot drift apart again.
 */
export function simulcastLadderBitrates(maxBitrate: number): Record<'q' | 'h' | 'f', number> {
  return { q: lowBitrate(maxBitrate), h: midBitrate(maxBitrate), f: maxBitrate };
}

export function buildCameraEncodingPlan(input: BuildCameraEncodingPlanInput): CameraEncodingPlan {
  const mime = codecMime(input.codec);
  const base = { ...input.priority };

  // Derive the layering kind from the selected codec, then collapse that plan to
  // single when its mode is disabled. These gates never replace the codec. This is
  // the one chokepoint that makes AV1+simulcast unreachable (#1921).
  const kind = castingKindForCodec(mime);
  if (!isCastingEligible(kind, input.eligibility)) {
    return { kind: 'single', encodings: [{ ...base, maxBitrate: input.maxBitrate }] };
  }

  if (kind === 'svc') {
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

  if (kind === 'simulcast') {
    return {
      kind: 'simulcast',
      encodings: (() => {
        const ladder = simulcastLadderBitrates(input.maxBitrate);
        return [
          { ...base, rid: 'q', scaleResolutionDownBy: 4, maxBitrate: ladder.q },
          { ...base, rid: 'h', scaleResolutionDownBy: 2, maxBitrate: ladder.h },
          { ...base, rid: 'f', scaleResolutionDownBy: 1, maxBitrate: ladder.f },
        ];
      })(),
    };
  }

  return { kind: 'single', encodings: [{ ...base, maxBitrate: input.maxBitrate }] };
}
