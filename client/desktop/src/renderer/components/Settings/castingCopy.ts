import { castingKindForCodec, type CameraLayeringKind } from '../../services/cameraLayering';

/**
 * Dynamic helper copy for the SVC / Simulcast casting toggles (#1921). Pure function
 * of (active codec, supportSvc, supportSimulcast). A codec-inert toggle stays ON and
 * shows "applies if you switch" copy — it is never disabled (per the spec state machine).
 *
 * Casting kind is codec-derived and the two schemes are mutually exclusive: AV1/VP9 are
 * SVC-only and can NEVER Simulcast; H.264/VP8 are Simulcast-only and can never SVC (#1924).
 * The copy makes that non-applicability explicit so a user does not expect Support
 * Simulcast to do anything for an AV1/VP9 codec (or Support SVC for an H.264/VP8 codec).
 */
export interface CastingCopy {
  svc: string;
  simulcast: string;
  notice?: string;
}

function svcLine(kind: CameraLayeringKind, supportSvc: boolean): string {
  const isSvcCodec = kind === 'svc';
  const isSimulcastCodec = kind === 'simulcast';
  if (supportSvc) {
    if (isSvcCodec) {
      return 'On — AV1 and VP9 publish layered SVC: one encode, multiple quality layers the server can thin per viewer.';
    }
    if (isSimulcastCodec) {
      return 'On — but SVC does not apply to your current codec: H.264/VP8 are Simulcast-only. Switch to AV1/VP9 to publish SVC.';
    }
    return 'On — applies to AV1/VP9. Switch to AV1 or VP9 to publish layered SVC.';
  }
  if (isSvcCodec) {
    // AV1/VP9 can never Simulcast, so with SVC off there is no layering fallback at all.
    return 'Off — AV1/VP9 cannot Simulcast, so with SVC off this codec publishes a single stream.';
  }
  return 'Off — AV1/VP9 SVC disabled.';
}

function simulcastLine(kind: CameraLayeringKind, supportSimulcast: boolean): string {
  const isSvcCodec = kind === 'svc';
  const isSimulcastCodec = kind === 'simulcast';
  if (supportSimulcast) {
    if (isSimulcastCodec) {
      return 'On — H.264 and VP8 publish 3 Simulcast layers (low / medium / full).';
    }
    if (isSvcCodec) {
      // Load-bearing clarity: Simulcast has NO effect for AV1/VP9 — they are SVC-only.
      return 'On — but has no effect for AV1/VP9: they are SVC-only and can never Simulcast. Their layering depends on Support SVC.';
    }
    return 'On — applies to H.264/VP8. Switch to H.264 or VP8 to publish Simulcast layers.';
  }
  if (isSimulcastCodec) {
    return 'Off — this codec will publish a single stream.';
  }
  return 'Off — H.264/VP8 Simulcast disabled.';
}

export function castingCopy(
  codecKeyOrMime: string | null,
  supportSvc: boolean,
  supportSimulcast: boolean
): CastingCopy {
  const mime = (codecKeyOrMime ?? '').split(':')[0]; // strip ':profile'
  const kind: CameraLayeringKind = mime ? castingKindForCodec(mime) : 'single'; // Auto/unknown → generic

  // Notice shown only when BOTH are off (forced single stream). Positive-leading
  // condition: when either is on there is no notice.
  const notice =
    supportSvc || supportSimulcast
      ? undefined
      : 'Layered video off — publishing a single stream to everyone. Applies when the call uses layered video.';

  return {
    svc: svcLine(kind, supportSvc),
    simulcast: simulcastLine(kind, supportSimulcast),
    notice,
  };
}
