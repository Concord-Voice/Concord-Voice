import { describe, it, expect } from 'vitest';
import {
  SCREEN_RES_DIMS,
  resolveScreenDims,
  highestFreeScreenResolution,
} from '@/renderer/utils/screenResolution';

describe('#2163 screenResolution', () => {
  it('maps the fixed presets', () => {
    expect(SCREEN_RES_DIMS['1080p']).toEqual({ w: 1920, h: 1080 });
    expect(SCREEN_RES_DIMS['720p']).toEqual({ w: 1280, h: 720 });
    expect(SCREEN_RES_DIMS['1440p']).toEqual({ w: 2560, h: 1440 });
    expect(SCREEN_RES_DIMS['4K']).toEqual({ w: 3840, h: 2160 });
  });

  describe('resolveScreenDims', () => {
    const src = { w: 3440, h: 1440 };
    it('resolves fixed presets', () => {
      expect(resolveScreenDims('1080p', src)).toEqual({ w: 1920, h: 1080 });
    });
    it('resolves a custom WxH string', () => {
      expect(resolveScreenDims('2560x1080', src)).toEqual({ w: 2560, h: 1080 });
    });
    it('resolves source to the provided source dims', () => {
      expect(resolveScreenDims('source', src)).toEqual(src);
    });
    it('falls back to the source dims for an unknown string', () => {
      expect(resolveScreenDims('unknown', src)).toEqual(src);
    });
  });

  describe('highestFreeScreenResolution', () => {
    it('picks the highest fixed resolution within a height ceiling', () => {
      expect(highestFreeScreenResolution(1080)).toBe('1080p');
      expect(highestFreeScreenResolution(720)).toBe('720p');
      expect(highestFreeScreenResolution(1440)).toBe('1440p');
    });
    it('returns the largest for a native (Infinity) ceiling', () => {
      expect(highestFreeScreenResolution(Infinity)).toBe('4K');
    });
    it('floors to 720p below the smallest preset', () => {
      expect(highestFreeScreenResolution(500)).toBe('720p');
    });
  });
});
