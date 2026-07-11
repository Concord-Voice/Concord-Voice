/**
 * Runtime WebRTC hardware-encode signal (question B).
 *
 * `RTCOutboundRtpStreamStats.powerEfficientEncoder` is libwebrtc's own
 * `is_hardware_accelerated` bit for the encoder actually in use — i.e. whether the
 * live CALL is hardware-encoding a codec. This is distinct from the WebCodecs
 * `isConfigSupported` probe (question A), which reports whether the GPU *silicon* can
 * encode a codec regardless of what Chromium's WebRTC sender does (WebRTC software-
 * encodes AV1 via libaom and often VP9, even on GPUs that have those encoders).
 *
 * The field is runtime-only: it is undefined until the encoder is instantiated and has
 * reported (~1–2s into an active send), so this returns null until then.
 */
export function extractWebrtcHwSignal(
  stats: RTCStatsReport
): { mime: string; powerEfficient: boolean } | null {
  let outbound:
    | { codecId?: string; powerEfficientEncoder?: boolean; kind?: string; mediaType?: string }
    | undefined;
  const codecMimes = new Map<string, string>();

  for (const report of stats.values()) {
    const r = report as {
      type?: string;
      id?: string;
      mimeType?: string;
      kind?: string;
      mediaType?: string;
    };
    if (r.type === 'outbound-rtp' && (r.kind === 'video' || r.mediaType === 'video')) {
      // Simulcast (buildCameraEncodingPlan) emits one outbound-rtp per rid; inactive or
      // not-yet-started layers leave powerEfficientEncoder undefined. Prefer any layer
      // that actually reports the HW bit rather than blindly keeping the last-iterated
      // one — otherwise a trailing inactive layer would defeat the whole B-signal.
      const candidate = report as typeof outbound;
      if (candidate?.powerEfficientEncoder !== undefined) {
        outbound = candidate;
      } else {
        outbound ??= candidate;
      }
    } else if (r.type === 'codec' && r.id && r.mimeType) {
      codecMimes.set(r.id, r.mimeType);
    }
  }

  if (outbound?.powerEfficientEncoder === undefined) return null;
  const mime = outbound.codecId ? codecMimes.get(outbound.codecId) : undefined;
  if (!mime) return null;
  return { mime: mime.toLowerCase(), powerEfficient: outbound.powerEfficientEncoder === true };
}
