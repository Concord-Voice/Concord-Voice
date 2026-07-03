import type { Entitlement } from '../stores/subscriptionStore';

/**
 * Per-axis video ceiling used for client-side settings gating (#1602). A negative
 * height/fps in the entitlement is the native/uncapped sentinel — normalized to
 * `Infinity` here so comparison sites can `preset.height > limit.height` without a
 * special-case branch. `bitrate` is always a positive bps value.
 */
export interface VideoAxisLimit {
  height: number; // px ceiling, or Infinity for native/uncapped
  fps: number; // fps ceiling, or Infinity for native/uncapped
  bitrate: number; // bps ceiling
}

/** The split screen-share (stream) + webcam (camera) video ceilings. */
export interface VideoLimits {
  stream: VideoAxisLimit;
  camera: VideoAxisLimit;
}

/** A negative height/fps ceiling means native/uncapped → Infinity for comparison. */
function normalizeCeiling(value: number): number {
  return value < 0 ? Infinity : value;
}

/**
 * Derive the split stream/camera video limits from the entitlement (#1602 matrix).
 * Pure, store-free, unit-testable.
 *
 * - The CAMERA axis is personal-tier only (never server-lifted).
 * - The SCREEN-SHARE (stream) axis may be lifted by the server floor
 *   (`max(personal, ServerVideoFloor)`, #1522). When `serverVideoFloor` is not
 *   surfaced client-side, the stream axis is the personal ceiling only — the
 *   media-plane enforces the actual floor regardless (this is display-only UX).
 */
export function videoLimitsFromEntitlement(
  ent: Pick<
    Entitlement,
    | 'streamMaxHeight'
    | 'streamMaxFps'
    | 'streamMaxBitrate'
    | 'cameraMaxHeight'
    | 'cameraMaxFps'
    | 'cameraMaxBitrate'
  >,
  serverVideoFloor?: { height: number; fps: number }
): VideoLimits {
  const streamHeight = normalizeCeiling(ent.streamMaxHeight);
  const streamFps = normalizeCeiling(ent.streamMaxFps);
  return {
    stream: {
      // server floor (#1522) lifts the personal ceiling upward when surfaced.
      height: serverVideoFloor ? Math.max(streamHeight, serverVideoFloor.height) : streamHeight,
      fps: serverVideoFloor ? Math.max(streamFps, serverVideoFloor.fps) : streamFps,
      bitrate: ent.streamMaxBitrate,
    },
    camera: {
      height: normalizeCeiling(ent.cameraMaxHeight),
      fps: normalizeCeiling(ent.cameraMaxFps),
      bitrate: ent.cameraMaxBitrate,
    },
  };
}
