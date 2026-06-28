import { castingKindForCodec } from '../../services/cameraLayering';

/**
 * Dynamic helper copy for the SVC / Simulcast casting toggles (#1921). Pure function
 * of (active codec, supportSvc, supportSimulcast). A codec-inert toggle stays ON and
 * shows "applies if you switch" copy — it is never disabled (per the spec state machine).
 */
export interface CastingCopy {
  svc: string;
  simulcast: string;
  notice?: string;
}

function svcLine(isSvcCodec: boolean, supportSvc: boolean): string {
  if (supportSvc) {
    if (isSvcCodec) {
      return 'On — AV1 and VP9 publish layered SVC: one encode, multiple quality layers the server can thin per viewer.';
    }
    return 'On — applies to AV1/VP9. Your current codec uses Simulcast (or single); switch to AV1/VP9 to use SVC.';
  }
  if (isSvcCodec) {
    return 'Off — this codec will publish a single stream.';
  }
  return 'Off — AV1/VP9 SVC disabled.';
}

function simulcastLine(isSimulcastCodec: boolean, supportSimulcast: boolean): string {
  if (supportSimulcast) {
    if (isSimulcastCodec) {
      return 'On — H.264 and VP8 publish 3 Simulcast layers (low / medium / full).';
    }
    return 'On — applies to H.264/VP8. Your current codec uses SVC (or single).';
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
  const kind = mime ? castingKindForCodec(mime) : 'single'; // Auto/unknown → describe generically
  const isSvcCodec = kind === 'svc';
  const isSimulcastCodec = kind === 'simulcast';

  // Notice shown only when BOTH are off (forced single stream). Positive-leading
  // condition: when either is on there is no notice.
  const notice =
    supportSvc || supportSimulcast
      ? undefined
      : 'Layered video off — publishing a single stream to everyone. Applies when the call uses layered video.';

  return {
    svc: svcLine(isSvcCodec, supportSvc),
    simulcast: simulcastLine(isSimulcastCodec, supportSimulcast),
    notice,
  };
}
