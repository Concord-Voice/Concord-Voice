import type { Entitlement } from '../../stores/auth/subscriptionStore';

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
  pixelRate: number; // px/s (w*h*fps) ceiling, or Infinity for native/uncapped (#2163)
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
 * The pixel-rate budget a server video floor implies (#2163 / #1522). The floor
 * is expressed as {height, fps}; a floor guarantees at least a 16:9 frame at that
 * height and fps, so the budget is `floorWidth * floorHeight * floorFps`. Used to
 * lift the personal pixel-rate cap alongside height/fps so the floor is not
 * silently rejected by the tiered `maxFpsForResolution` clamp.
 */
function floorPixelRate(floor: { height: number; fps: number }): number {
  const width = Math.round((floor.height * 16) / 9);
  return width * floor.height * floor.fps;
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
    | 'streamMaxPixelRate'
    | 'streamMaxBitrate'
    | 'cameraMaxHeight'
    | 'cameraMaxFps'
    | 'cameraMaxBitrate'
  >,
  serverVideoFloor?: { height: number; fps: number }
): VideoLimits {
  const streamHeight = normalizeCeiling(ent.streamMaxHeight);
  const streamFps = normalizeCeiling(ent.streamMaxFps);
  const streamPixelRate = normalizeCeiling(ent.streamMaxPixelRate);
  return {
    stream: {
      // server floor (#1522) lifts the personal ceiling upward when surfaced.
      height: serverVideoFloor ? Math.max(streamHeight, serverVideoFloor.height) : streamHeight,
      fps: serverVideoFloor ? Math.max(streamFps, serverVideoFloor.fps) : streamFps,
      bitrate: ent.streamMaxBitrate,
      // #2163: lift the tiered pixel-rate budget in step with height/fps, otherwise
      // a floor (e.g. a 1080p60 floor) is silently rejected because the unchanged
      // free streamMaxPixelRate (=1080p30) makes maxFpsForResolution(1920,1080)=30.
      pixelRate: serverVideoFloor
        ? Math.max(streamPixelRate, floorPixelRate(serverVideoFloor))
        : streamPixelRate,
    },
    camera: {
      height: normalizeCeiling(ent.cameraMaxHeight),
      fps: normalizeCeiling(ent.cameraMaxFps),
      bitrate: ent.cameraMaxBitrate,
      pixelRate: Infinity, // camera has no pixel-rate cap — 720p60 is a single tier (#2163)
    },
  };
}

/**
 * The maximum fps admissible for a (width × height) resolution under an axis's
 * tiered caps: the smaller of the absolute fps ceiling and the pixel-rate budget
 * (#2163). Pure. A native/uncapped axis (Infinity pixelRate) returns the fps
 * ceiling. A non-positive or non-finite area (zero/negative/NaN) returns the fps
 * ceiling (defensive) — Number.isFinite also rejects NaN, so a NaN area can never
 * fall through to `pixelRate / NaN` and leak a NaN ceiling downstream (#2172).
 */
export function maxFpsForResolution(width: number, height: number, axis: VideoAxisLimit): number {
  const area = width * height;
  if (!Number.isFinite(area) || area <= 0 || !Number.isFinite(axis.pixelRate)) return axis.fps;
  return Math.min(axis.fps, Math.floor(axis.pixelRate / area));
}

/**
 * Clamp a screen-capture (width, height, fps) triple to a video axis: reduce
 * height to the axis ceiling (scaling width proportionally, rounded to even),
 * then reduce fps to the tiered max for the clamped resolution (#2163). Pure.
 * Premium (Infinity ceilings) is a structural no-op.
 */
export function clampScreenCapture(
  width: number,
  height: number,
  fps: number,
  axis: VideoAxisLimit
): { width: number; height: number; fps: number } {
  let w = width;
  let h = height;
  if (Number.isFinite(axis.height) && h > axis.height && h > 0) {
    const scale = axis.height / h;
    h = axis.height;
    w = Math.max(2, Math.round((width * scale) / 2) * 2);
  }
  return { width: w, height: h, fps: Math.min(fps, maxFpsForResolution(w, h, axis)) };
}

/** A fully-open (native/uncapped) stream axis — the fail-open sentinel used when the
 *  entitlement is not authoritative enough to clamp against. Every field is Infinity;
 *  `clampScreenCapture` against it is a structural no-op (no height clamp, fps ceiling
 *  Infinity), and no consumer reads `bitrate` off the effective axis. (#2172) */
const NATIVE_STREAM_AXIS: VideoAxisLimit = {
  height: Infinity,
  fps: Infinity,
  bitrate: Infinity,
  pixelRate: Infinity,
};

/** A subscription snapshot: the entitlement plus whether it is authoritative. */
export interface SubscriptionSnapshot {
  hydrated: boolean;
  degraded: boolean;
  entitlement: Entitlement;
}

/**
 * Whether a subscription snapshot is authoritative enough to ENFORCE an
 * entitlement-derived cap (a destructive snap-back or a produce-boundary clamp),
 * or whether a seam must fail OPEN because the real entitlement has not arrived.
 * The single fail-open predicate every seam shares (the screen-share stream axis,
 * the Settings display gating, and the camera-preset snap-back) so they can never
 * disagree (#2172):
 *
 *  - Pre-hydrate (`!hydrated && !degraded`, entitlement still loading): fail OPEN,
 *    so a premium user whose real entitlement has not arrived is not transiently
 *    clamped.
 *  - Degraded but NOT the free tier: fail OPEN. With the store's
 *    preserve-on-reconnect behaviour (#2172), a degraded PREMIUM user keeps
 *    `tier: 'premium'`, so a transient fetch failure does not clamp them to free.
 *  - Otherwise (authoritative, or a degraded FREE floor): ENFORCE. A FREE user
 *    whose `/entitlements` fetch failed on FIRST load is still enforced, closing
 *    the monetization escape a bare `degraded` fail-open would leave open.
 *
 * Pure (store snapshot passed in).
 */
export function shouldEnforceForSubscription(sub: SubscriptionSnapshot): boolean {
  const failOpen =
    (!sub.hydrated && !sub.degraded) || (sub.degraded && sub.entitlement.tier !== 'free');
  return !failOpen;
}

/**
 * The stream (screen-share) axis to ENFORCE for a subscription snapshot — the single
 * gate every display seam (picker, Settings) and the produce boundary share so they
 * never disagree (#2172). Returns the entitlement's stream axis when it is authoritative
 * enough to clamp, or a fully-open native axis (fail OPEN) otherwise:
 *
 *  - Pre-hydrate (`!hydrated && !degraded`, entitlement still loading): fail OPEN, so a
 *    premium user whose real entitlement hasn't arrived is not transiently clamped.
 *  - Degraded but NOT the free tier: fail OPEN. With the store's preserve-on-reconnect
 *    behaviour (#2172), a degraded PREMIUM user keeps `tier: 'premium'`, so this branch
 *    fires and their screen share is not clamped to free by a transient fetch failure.
 *  - Otherwise (authoritative, or a degraded FREE floor): the entitlement's stream axis.
 *    A FREE user whose `/entitlements` fetch failed is still enforced, closing the
 *    monetization escape a bare `degraded` fail-open would leave open.
 *
 * Pure (store snapshot passed in).
 */
export function effectiveStreamAxis(sub: SubscriptionSnapshot): VideoAxisLimit {
  return shouldEnforceForSubscription(sub)
    ? videoLimitsFromEntitlement(sub.entitlement).stream
    : NATIVE_STREAM_AXIS;
}

/**
 * Produce-boundary screen clamp (#2163). NON-PERSISTENT (shapes one capture), so it can
 * enforce more aggressively than useLaunchReset's persistent clamp without #1301 data-loss
 * risk. Delegates the authoritative-vs-fail-open decision to `effectiveStreamAxis` so it
 * stays in lockstep with the picker + Settings display seams. Pure.
 */
export function clampScreenForSubscription(
  width: number,
  height: number,
  fps: number,
  sub: SubscriptionSnapshot
): { width: number; height: number; fps: number } {
  return clampScreenCapture(width, height, fps, effectiveStreamAxis(sub));
}
