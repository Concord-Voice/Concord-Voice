export type RemoteVideoRole = 'thumbnail' | 'grid' | 'focus';
export type RemoteVideoLayer = 0 | 1 | 2;

export interface RemoteVideoRenderState {
  visible: boolean;
  cssWidth: number;
  cssHeight: number;
  devicePixelRatio: number;
  role: RemoteVideoRole;
  focusedWindow: boolean;
  pressureStepDown: boolean;
}

export interface RemoteVideoLayerRequest {
  visible: boolean;
  spatialLayer: RemoteVideoLayer;
  temporalLayer: RemoteVideoLayer;
}

function clampLayer(layer: number): RemoteVideoLayer {
  if (layer <= 0) return 0;
  if (layer >= 2) return 2;
  return 1;
}

function layerForPixels(width: number, height: number): 0 | 1 | 2 {
  const maxEdge = Math.max(width, height);
  if (maxEdge <= 540) return 0;
  if (maxEdge <= 1280) return 1;
  return 2;
}

export function computeRemoteVideoLayerRequest(
  state: RemoteVideoRenderState,
  // The SFU's own ceiling for THIS viewer. Defaults to the full ladder so every
  // existing caller and the server-side parity sweep are unchanged; the desktop
  // passes the entitlement-derived value. Clamping HERE rather than in the step
  // arithmetic is what makes the first pressure step land: it moves the base the
  // ladder counts down from, instead of merely counting the steps differently.
  maxSpatialLayer: RemoteVideoLayer = 2
): RemoteVideoLayerRequest {
  if (!state.visible) return { visible: false, spatialLayer: 0, temporalLayer: 0 };

  const dpr =
    Number.isFinite(state.devicePixelRatio) && state.devicePixelRatio > 0
      ? state.devicePixelRatio
      : 1;
  const width = Math.max(0, state.cssWidth) * dpr;
  const height = Math.max(0, state.cssHeight) * dpr;

  let spatialLayer: number = layerForPixels(width, height);
  if (state.role === 'thumbnail') spatialLayer = Math.min(spatialLayer, 0);
  if (state.role === 'grid') spatialLayer = Math.min(spatialLayer, 1);
  if (!state.focusedWindow) spatialLayer -= 1;
  if (state.pressureStepDown) spatialLayer -= 1;

  const layer = clampLayer(Math.min(spatialLayer, maxSpatialLayer));
  return { visible: true, spatialLayer: layer, temporalLayer: layer === 0 ? 1 : 2 };
}

/**
 * Apply N steps of decoder pressure to an already-computed request.
 *
 * `computeRemoteVideoLayerRequest` expresses exactly ONE step, because that is
 * what the media-plane's `layerForRender` mirrors and what the wire's boolean
 * `pressureStepDown` can say. Deeper pressure is expressed by asking for a LOWER
 * `spatialLayer` outright: the server clamps with
 * `Math.min(demand.spatialLayer, layerForRender(demand), maxSpatialLayer)`, so a
 * request below its own cap is honoured, and `maxUsefulSpatialLayer` deliberately
 * excludes the request, so stepping deeper cannot flip the room gate off.
 *
 * Keeping the arithmetic here rather than in voiceService is the point: the ladder
 * already exists twice (renderer + governor) and is pinned by a parity suite. A
 * third copy is what that suite exists to prevent.
 */
export function stepDownLayerRequest(
  request: RemoteVideoLayerRequest,
  steps: number
): RemoteVideoLayerRequest {
  if (!request.visible || steps <= 0) return request;
  const layer = clampLayer(request.spatialLayer - steps);
  return { visible: true, spatialLayer: layer, temporalLayer: layer === 0 ? 1 : 2 };
}

/**
 * How many pressure steps this request can still absorb before it would fall
 * below layer 0 — i.e. before a step stops being a step and pausing is the only
 * remaining move. A hidden tile offers none.
 */
export function maxPressureSteps(request: RemoteVideoLayerRequest): number {
  return request.visible ? request.spatialLayer : 0;
}
