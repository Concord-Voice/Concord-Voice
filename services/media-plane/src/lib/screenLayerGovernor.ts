import type { StoredCameraLayerDemand } from './cameraLayerGovernor.js';

/**
 * Screen layer demand carries BOTH the owning viewer's `userId` and the watched
 * screen producer's owner `sharerUserId`, on top of the source-agnostic
 * `StoredCameraLayerDemand` shape (consumer id, visibility, smallest-sufficient
 * spatial layer, pressure flag).
 *
 * The `userId` (viewer) is load-bearing for the gate: it must count DISTINCT
 * viewers, not consumer entries. A single participant can own multiple screen
 * consumers of one producer (mediasoup permits it; `consume()` does not dedup),
 * so counting `screenLayerDemands` entries let one misbehaving client trip the
 * gate and force honest publishers into 3× simulcast egress (#1924 adversarial
 * review). Deduping by `userId` closes that: one client = one viewer = at most
 * one toward the threshold, regardless of how many consumers it opens.
 *
 * The `sharerUserId` (screen producer owner) is load-bearing for the per-stream
 * grouping (#1924 review fix "B"): the gate is PER-SHARER, not room-global, so
 * the CALLER pre-filters demands to a single sharer before invoking
 * `computeScreenLayeringGate`. Without this grouping, two viewers watching two
 * DIFFERENT screen shares would flip a room-wide gate and force EVERY sharer
 * into simulcast even though no single stream has heterogeneous viewers. Keyed
 * per SHARER (not per producerId) so it is stable across the sharer's
 * reproduce (codec/layer swap), which mints a new producerId.
 */
export interface StoredScreenLayerDemand extends StoredCameraLayerDemand {
  userId: string;
  sharerUserId: string;
}

/**
 * Pure screen-layering gate (#1924). Demand-driven ONLY: engages when receiver
 * demand shows viewer-size heterogeneity across DISTINCT viewers. Unlike
 * computeCameraLayeringGate there is NO producer-count arm and NO svc
 * short-circuit — SVC screen is handled client-side (ungated, cost-neutral);
 * this gate governs the H.264/VP8 3-encode simulcast path exclusively. 2-on /
 * 1-off distinct-viewer hysteresis (mirrors camera's visible hysteresis).
 *
 * Server-authoritative: the count is over distinct owning `userId`s among
 * visible demands, so a single client cannot inflate it by owning multiple
 * screen consumers (each ownership-validated, but all sharing one userId).
 *
 * Per-sharer (#1924 fix "B"): this function is UNCHANGED and sharer-agnostic —
 * the CALLER pre-filters `demands` to a single sharer's viewers (by
 * `sharerUserId`) before invoking it, so the distinct-viewer count is over that
 * one stream's viewers only, never the whole room.
 */
export function computeScreenLayeringGate(input: {
  demands: StoredScreenLayerDemand[];
  previouslyEnabled?: boolean;
}): boolean {
  const visible = input.demands.filter((d) => d.visible);
  const distinctViewers = new Set(visible.map((d) => d.userId)).size;
  const viewerThreshold = input.previouslyEnabled ? 1 : 2;
  return (
    distinctViewers >= viewerThreshold &&
    visible.some((d) => d.maxUsefulSpatialLayer < 2 || d.pressureStepDown)
  );
}
