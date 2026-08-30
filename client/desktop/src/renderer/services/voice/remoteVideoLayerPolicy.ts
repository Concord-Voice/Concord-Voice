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
  state: RemoteVideoRenderState
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

  const layer = clampLayer(spatialLayer);
  return { visible: true, spatialLayer: layer, temporalLayer: layer === 0 ? 1 : 2 };
}
