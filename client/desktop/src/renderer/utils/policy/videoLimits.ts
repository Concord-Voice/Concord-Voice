import { type Entitlement, FREE_ENTITLEMENT } from '../../stores/auth/subscriptionStore';

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
/**
 * The camera SPATIAL-LAYER ceiling the SFU will actually honour for this viewer.
 *
 * Mirrors `maxCameraSpatialLayerForParticipant` in the media plane
 * (`roomManager.ts`), which reads the viewer's own entitlement:
 * `maxManualBitrateBps > free ? 2 : 1`. The client has to know this number
 * because IGNIS predicts, client-side, how many pressure steps remain — and a
 * prediction against the unclamped ladder is wrong for every free viewer.
 * Without it a free viewer on a large focus tile believes it holds two steps,
 * spends the first moving the forwarded layer from 1 to 1, and waits a whole
 * red segment for relief that the first step should have delivered
 * (Codex, #3279).
 *
 * Fails OPEN through the shared `shouldEnforceForSubscription` gate, and the
 * direction matters: clamping is the harmful guess here. A premium viewer whose
 * entitlement has not arrived yet would otherwise be held at layer 1 — a
 * visible quality regression during the pre-hydrate window — whereas failing
 * open merely reproduces today's behaviour until the real value lands.
 *
 * Pure (store snapshot passed in).
 */
export function effectiveCameraSpatialCap(sub: SubscriptionSnapshot): 0 | 1 | 2 {
  if (!shouldEnforceForSubscription(sub)) return 2;
  return sub.entitlement.maxManualBitrateBps > FREE_ENTITLEMENT.maxManualBitrateBps ? 2 : 1;
}

/**
 * The camera spatial cap the SFU itself computed for THIS media session, read
 * from the join response rather than from any client store.
 *
 * `media_entitlements` rides the same renderer join payload as `ice_servers`
 * (`internal/voice/handlers.go`), and the media plane derives the participant's
 * cap from exactly this field — `maxCameraSpatialLayerForParticipant` is
 * `participant.maxManualBitrateBps > FREE.maxManualBitrateBps ? 2 : 1`, where
 * `participant.maxManualBitrateBps` was set from this very number at join. So
 * this is not a client-side approximation of the server's decision; it is the
 * server's own input to it.
 *
 * That closes a defect class the store cannot, in BOTH directions. A live store
 * read is wrong on a mid-call upgrade (the store moves, the SFU session does
 * not). A snapshot taken from the store at join is wrong during connection
 * recovery, where `recoveryReset()` deliberately preserves the previous
 * subscription snapshot while voice rejoins before hydration completes — so a
 * premium subscription that expired during the outage pins a stale cap 2 against
 * a session the SFU freshly admitted at 1 (Codex, #3279).
 *
 * The `tier` gate mirrors the media plane's own atomic-free clamp: only an
 * explicit `premium` tier may carry a premium cap, so a cross-field-inconsistent
 * response cannot raise the cap on its bitrate alone.
 *
 * Total over `unknown`, like `normalizeIceServers`: the field is optional on the
 * wire and a malformed one must degrade to `null` (caller falls back) rather
 * than throw inside the join path.
 */
export function cameraSpatialCapFromJoin(mediaEntitlements: unknown): 0 | 1 | 2 | null {
  if (typeof mediaEntitlements !== 'object' || mediaEntitlements === null) return null;
  try {
    const ent = mediaEntitlements as { tier?: unknown; max_manual_bitrate_bps?: unknown };
    const bitrate = ent.max_manual_bitrate_bps;
    if (typeof bitrate !== 'number' || !Number.isFinite(bitrate)) return null;
    if (ent.tier !== 'premium') return 1;
    return bitrate > FREE_ENTITLEMENT.maxManualBitrateBps ? 2 : 1;
  } catch {
    // A throwing accessor cannot come from `res.json()`, which yields plain data.
    // The guard is here because this runs inside the join path, where the cost of
    // being wrong is a failed join rather than a mis-read cap — and because the
    // doc comment above claims totality, which is worth actually holding.
    return null;
  }
}

/**
 * The camera spatial cap the SFU reports it ADMITTED this participant under,
 * read off the `join-room` acknowledgement (#3279).
 *
 * This is the end of a chain of four. Each earlier source mirrored the SFU's
 * RULE against a different client-visible input and disagreed somewhere: a live
 * store read on a mid-call upgrade; a store snapshot on a recovery rejoin, where
 * `recoveryReset()` deliberately preserves a stale subscription; and even the
 * renderer's own REST join payload, because the media plane performs a SEPARATE
 * `validateChannelAccess` re-authorization at join-room, so a tier change in
 * that window resolves twice to two answers.
 *
 * The class only closes by asking the party that does the clamping. This value
 * is `cameraSpatialCapForParticipant` in roomManager.ts -- the same expression
 * `validateAndClampLayerDemand` clamps against -- evaluated on the participant
 * the SFU actually admitted.
 *
 * Returns null for anything but 0, 1 or 2 so an older media plane (which omits
 * the field) falls back rather than being handed a guess.
 */
export function cameraSpatialCapFromAck(cap: unknown): 0 | 1 | 2 | null {
  return cap === 0 || cap === 1 || cap === 2 ? cap : null;
}

/**
 * The camera spatial cap to pin for a media session, resolved from every source
 * in order of authority (#3279).
 *
 * Extracted as a pure function because the wiring line inside
 * `establishMediaSession` had no test and could not easily get one -- that
 * function needs a socket, a device and transports. A guard that cannot be
 * exercised is a guard nobody can trust (Gitar, #3279). What is worth testing is
 * the PRECEDENCE, not the assignment, so the precedence lives here and
 * `establishMediaSession` keeps a single call.
 *
 *  1. the SFU's admitted cap from the join-room ack -- the party that clamps;
 *  2. the control plane's `media_entitlements` on the renderer REST join;
 *  3. the subscription store, for a control plane that sends neither.
 *
 * Uses `??` and not `||` deliberately: 0 is a legitimate cap and is falsy.
 */
export function resolveSessionCameraCap(input: {
  ackCap: unknown;
  joinMediaEntitlements: unknown;
  subscription: SubscriptionSnapshot;
}): 0 | 1 | 2 {
  return (
    cameraSpatialCapFromAck(input.ackCap) ??
    cameraSpatialCapFromJoin(input.joinMediaEntitlements) ??
    effectiveCameraSpatialCap(input.subscription)
  );
}

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
