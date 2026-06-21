import { describe, it, expect } from 'vitest';
import { deriveOverlayColors } from '@/renderer/utils/overlayColors';

describe('deriveOverlayColors', () => {
  it('returns dark palette for dark theme', () => {
    expect(deriveOverlayColors('dark')).toEqual({
      color: '#1a1a1a',
      symbolColor: '#ffffff',
    });
  });

  it('returns light palette for light theme', () => {
    expect(deriveOverlayColors('light')).toEqual({
      color: '#f5f5f5',
      symbolColor: '#1a1a1a',
    });
  });

  it('returns OverlayColors with both required fields', () => {
    const result = deriveOverlayColors('dark');
    expect(result).toHaveProperty('color');
    expect(result).toHaveProperty('symbolColor');
  });
});
