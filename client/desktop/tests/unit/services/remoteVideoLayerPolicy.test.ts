import { describe, expect, it } from 'vitest';
import { computeRemoteVideoLayerRequest } from '../../../src/renderer/services/remoteVideoLayerPolicy';

describe('computeRemoteVideoLayerRequest', () => {
  it('pauses hidden tiles by returning visible false with low layers', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: false,
      cssWidth: 640,
      cssHeight: 360,
      devicePixelRatio: 2,
      role: 'grid',
      focusedWindow: true,
      pressureStepDown: false,
    });
    expect(result).toEqual({ visible: false, spatialLayer: 0, temporalLayer: 0 });
  });

  it('keeps thumbnails on low layer', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: true,
      cssWidth: 240,
      cssHeight: 135,
      devicePixelRatio: 2,
      role: 'thumbnail',
      focusedWindow: true,
      pressureStepDown: false,
    });
    expect(result.spatialLayer).toBe(0);
  });

  it('caps a 720p focus surface to mid layer', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: true,
      cssWidth: 1280,
      cssHeight: 720,
      devicePixelRatio: 1,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    expect(result.spatialLayer).toBe(1);
    expect(result.temporalLayer).toBe(2);
  });

  it('allows large focused surfaces to request top layer', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 2,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: false,
    });
    expect(result.spatialLayer).toBe(2);
  });

  it('downgrades by one under pressure', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 2,
      role: 'focus',
      focusedWindow: true,
      pressureStepDown: true,
    });
    expect(result.spatialLayer).toBe(1);
  });

  it('downgrades an unfocused app window by one', () => {
    const result = computeRemoteVideoLayerRequest({
      visible: true,
      cssWidth: 1920,
      cssHeight: 1080,
      devicePixelRatio: 2,
      role: 'focus',
      focusedWindow: false,
      pressureStepDown: false,
    });
    expect(result.spatialLayer).toBe(1);
  });
});
