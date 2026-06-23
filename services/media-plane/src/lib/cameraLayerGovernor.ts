export type CameraLayerRole = 'thumbnail' | 'grid' | 'focus';
export type LayeredCodecKind = 'svc' | 'simulcast';
export type LayerValue = 0 | 1 | 2;

export interface CameraLayerDemand {
  consumerId: string;
  spatialLayer: LayerValue;
  temporalLayer: LayerValue;
  visible: boolean;
  cssWidth: number;
  cssHeight: number;
  devicePixelRatio: number;
  role: CameraLayerRole;
  focusedWindow: boolean;
  pressureStepDown: boolean;
}

export interface StoredCameraLayerDemand {
  consumerId: string;
  visible: boolean;
  maxUsefulSpatialLayer: LayerValue;
  pressureStepDown: boolean;
}

export type ParseDemandResult =
  | { ok: true; value: CameraLayerDemand }
  | { ok: false; error: 'invalid-consumer-id' | 'invalid-layer' | 'invalid-render-state' };

function isRecord(raw: unknown): raw is Record<string, unknown> {
  return typeof raw === 'object' && raw !== null;
}

function isLayer(n: unknown): n is LayerValue {
  return n === 0 || n === 1 || n === 2;
}

function isRole(role: unknown): role is CameraLayerRole {
  return role === 'thumbnail' || role === 'grid' || role === 'focus';
}

function isValidCssDimension(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value >= 0;
}

function isValidDevicePixelRatio(value: unknown): value is number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0 && value <= 8;
}

export function parseCameraLayerDemand(raw: unknown): ParseDemandResult {
  if (!isRecord(raw)) return { ok: false, error: 'invalid-render-state' };

  if (
    typeof raw.consumerId !== 'string' ||
    raw.consumerId.length === 0 ||
    raw.consumerId !== raw.consumerId.trim()
  ) {
    return { ok: false, error: 'invalid-consumer-id' };
  }
  if (!isLayer(raw.spatialLayer) || !isLayer(raw.temporalLayer)) {
    return { ok: false, error: 'invalid-layer' };
  }
  if (typeof raw.visible !== 'boolean') return { ok: false, error: 'invalid-render-state' };
  if (!isValidCssDimension(raw.cssWidth) || !isValidCssDimension(raw.cssHeight)) {
    return { ok: false, error: 'invalid-render-state' };
  }
  if (!isValidDevicePixelRatio(raw.devicePixelRatio)) {
    return { ok: false, error: 'invalid-render-state' };
  }
  if (!isRole(raw.role)) return { ok: false, error: 'invalid-render-state' };
  if (typeof raw.focusedWindow !== 'boolean' || typeof raw.pressureStepDown !== 'boolean') {
    return { ok: false, error: 'invalid-render-state' };
  }

  return {
    ok: true,
    value: {
      consumerId: raw.consumerId,
      spatialLayer: raw.spatialLayer,
      temporalLayer: raw.temporalLayer,
      visible: raw.visible,
      cssWidth: raw.cssWidth,
      cssHeight: raw.cssHeight,
      devicePixelRatio: raw.devicePixelRatio,
      role: raw.role,
      focusedWindow: raw.focusedWindow,
      pressureStepDown: raw.pressureStepDown,
    },
  };
}

function layerForRender(demand: CameraLayerDemand): LayerValue {
  if (!demand.visible) return 0;

  const maxEdge = Math.max(demand.cssWidth, demand.cssHeight) * demand.devicePixelRatio;
  let layer: number = layerForMaxEdge(maxEdge);

  if (demand.role === 'thumbnail') layer = Math.min(layer, 0);
  if (demand.role === 'grid') layer = Math.min(layer, 1);
  if (!demand.focusedWindow) layer -= 1;
  if (demand.pressureStepDown) layer -= 1;

  if (layer <= 0) return 0;
  if (layer >= 2) return 2;
  return 1;
}

function layerForMaxEdge(maxEdge: number): LayerValue {
  if (maxEdge <= 540) return 0;
  if (maxEdge <= 1280) return 1;
  return 2;
}

export function clampCameraLayerDemand(
  demand: CameraLayerDemand,
  maxSpatialLayer: LayerValue = 2
): {
  spatialLayer: LayerValue;
  temporalLayer: LayerValue;
} {
  const spatialLayer = Math.min(demand.spatialLayer, layerForRender(demand), maxSpatialLayer) as LayerValue;
  const temporalLayer = Math.min(demand.temporalLayer, spatialLayer === 0 ? 1 : 2) as LayerValue;
  return { spatialLayer, temporalLayer };
}

export function storedDemand(
  demand: CameraLayerDemand,
  maxSpatialLayer: LayerValue = 2
): StoredCameraLayerDemand {
  return {
    consumerId: demand.consumerId,
    visible: demand.visible,
    maxUsefulSpatialLayer: Math.min(layerForRender(demand), maxSpatialLayer) as LayerValue,
    pressureStepDown: demand.pressureStepDown,
  };
}

export function computeCameraLayeringGate(input: {
  codecKind: LayeredCodecKind;
  cameraProducerCount: number;
  demands: StoredCameraLayerDemand[];
  previouslyEnabled?: boolean;
}): boolean {
  const visible = input.demands.filter((demand) => demand.visible);
  const visibleThreshold = input.previouslyEnabled ? 1 : 2;
  const hasLayerBenefit =
    visible.length >= visibleThreshold &&
    visible.some((demand) => demand.maxUsefulSpatialLayer < 2 || demand.pressureStepDown);

  if (!hasLayerBenefit) return false;
  if (input.codecKind === 'svc') return true;
  return input.cameraProducerCount >= (input.previouslyEnabled ? 2 : 3);
}
