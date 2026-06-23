import { describe, expect, it } from 'vitest';
import {
  clampCameraLayerDemand,
  computeCameraLayeringGate,
  parseCameraLayerDemand,
  storedDemand,
} from '../src/lib/cameraLayerGovernor.js';

const validRawDemand = {
  consumerId: 'c1',
  spatialLayer: 2,
  temporalLayer: 2,
  visible: true,
  cssWidth: 1280,
  cssHeight: 720,
  devicePixelRatio: 1,
  role: 'focus',
  focusedWindow: true,
  pressureStepDown: false,
} as const;

describe('parseCameraLayerDemand', () => {
  it('accepts valid render demand', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 2,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1280,
        cssHeight: 720,
        devicePixelRatio: 1,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toMatchObject({ ok: true });
  });

  it('rejects malformed render demand', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 5,
        temporalLayer: 2,
        visible: true,
        cssWidth: 1280,
        cssHeight: 720,
        devicePixelRatio: 1,
        role: 'focus',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-layer' });
  });

  it('rejects blank consumer ids', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: '   ',
        spatialLayer: 1,
        temporalLayer: 1,
        visible: true,
        cssWidth: 640,
        cssHeight: 360,
        devicePixelRatio: 1,
        role: 'grid',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-consumer-id' });
  });

  it('rejects whitespace-padded consumer ids', () => {
    expect(parseCameraLayerDemand({ ...validRawDemand, consumerId: ' c1 ' })).toEqual({
      ok: false,
      error: 'invalid-consumer-id',
    });
  });

  it('rejects invalid render dimensions', () => {
    expect(
      parseCameraLayerDemand({
        consumerId: 'c1',
        spatialLayer: 1,
        temporalLayer: 1,
        visible: true,
        cssWidth: -1,
        cssHeight: 360,
        devicePixelRatio: 1,
        role: 'grid',
        focusedWindow: true,
        pressureStepDown: false,
      })
    ).toEqual({ ok: false, error: 'invalid-render-state' });
  });

  it.each([
    ['null payload', null],
    ['string payload', 'not-demand'],
    ['number payload', 42],
    ['NaN cssWidth', { ...validRawDemand, cssWidth: NaN }],
    ['infinite cssHeight', { ...validRawDemand, cssHeight: Infinity }],
  ])('rejects %s', (_name, raw) => {
    expect(parseCameraLayerDemand(raw)).toEqual({ ok: false, error: 'invalid-render-state' });
  });
});

describe('clampCameraLayerDemand', () => {
  it('caps 720p focus to mid layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 1, temporalLayer: 2 });
  });

  it('caps hidden demand to the lowest spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: false,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 0, temporalLayer: 1 });
  });

  it('applies a server-authoritative spatial layer cap', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value, 1)).toEqual({ spatialLayer: 1, temporalLayer: 2 });
  });

  it('uses physical pixels from css size and devicePixelRatio', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 700,
      cssHeight: 400,
      devicePixelRatio: 2,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(clampCameraLayerDemand(demand.value)).toEqual({ spatialLayer: 2, temporalLayer: 2 });
  });
});

describe('storedDemand', () => {
  it('stores the render-derived max useful spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: true,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(storedDemand(demand.value)).toEqual({
      consumerId: 'c1',
      visible: true,
      maxUsefulSpatialLayer: 0,
      pressureStepDown: true,
    });
  });

  it('stores the server-capped max useful spatial layer', () => {
    const demand = parseCameraLayerDemand({
      consumerId: 'c1',
      spatialLayer: 2,
      temporalLayer: 2,
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    if (!demand.ok) throw new Error('unexpected invalid demand');
    expect(storedDemand(demand.value, 1)).toMatchObject({ maxUsefulSpatialLayer: 1 });
  });
});

describe('computeCameraLayeringGate', () => {
  it('enables SVC when two visible consumers include lower-layer benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false },
          { consumerId: 'c2', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
      })
    ).toBe(true);
  });

  it('keeps simulcast fallback off until there are at least three camera producers', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 2,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false },
          { consumerId: 'c2', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
      })
    ).toBe(false);
  });

  it('enables simulcast fallback with at least three camera producers and layer benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 3,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false },
          { consumerId: 'c2', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
      })
    ).toBe(true);
  });

  it('enables SVC when visible top-layer consumers include pressure-only benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: true },
          { consumerId: 'c2', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
      })
    ).toBe(true);
  });

  it('ignores hidden lower-layer and pressure demand when computing benefit', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
          { consumerId: 'c2', visible: false, maxUsefulSpatialLayer: 1, pressureStepDown: true },
        ],
      })
    ).toBe(false);
  });

  it('keeps layering off when only one consumer is visible', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 3,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false },
          { consumerId: 'c2', visible: false, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
      })
    ).toBe(false);
  });

  it('keeps SVC enabled at the one-visible-consumer hysteresis floor', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'svc',
        cameraProducerCount: 2,
        demands: [{ consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false }],
        previouslyEnabled: true,
      })
    ).toBe(true);
  });

  it('keeps simulcast enabled at the two-producer hysteresis floor', () => {
    expect(
      computeCameraLayeringGate({
        codecKind: 'simulcast',
        cameraProducerCount: 2,
        demands: [
          { consumerId: 'c1', visible: true, maxUsefulSpatialLayer: 1, pressureStepDown: false },
          { consumerId: 'c2', visible: true, maxUsefulSpatialLayer: 2, pressureStepDown: false },
        ],
        previouslyEnabled: true,
      })
    ).toBe(true);
  });
});
