import { describe, expect, it } from 'vitest';
import {
  FEC_MAX_HEADROOM_PERCENT,
  calculateFecBitrate,
} from '@/renderer/services/voice/fecHeadroom';

describe('mic FEC headroom (#2153 extraction)', () => {
  it('returns the tier bitrate when headroom is off or nothing was lost', () => {
    expect(calculateFecBitrate(10, 96_000, false)).toBe(96_000);
    expect(calculateFecBitrate(0, 96_000, true)).toBe(96_000);
    expect(calculateFecBitrate(-5, 96_000, true)).toBe(96_000);
  });

  it('scales loss by the tier band, with each band edge in the upper band', () => {
    expect(calculateFecBitrate(5, 32_000, true)).toBe(38_400); // K = 4 -> +20%
    expect(calculateFecBitrate(4, 64_000, true)).toBe(70_400); // K = 2.5 -> +10%
    expect(calculateFecBitrate(10, 96_000, true)).toBe(120_000); // K = 2.5 -> +25%
    expect(calculateFecBitrate(4, 128_000, true)).toBe(135_680); // K = 1.5 -> +6%
    expect(calculateFecBitrate(10, 192_000, true)).toBe(220_800); // K = 1.5 -> +15%
  });

  it('caps the headroom at FEC_MAX_HEADROOM_PERCENT', () => {
    expect(FEC_MAX_HEADROOM_PERCENT).toBe(50);
    expect(calculateFecBitrate(90, 96_000, true)).toBe(144_000);
    expect(calculateFecBitrate(100, 510_000, true)).toBe(765_000);
  });
});
