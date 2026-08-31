import { describe, it, expect } from 'vitest';
import { nativeExceedsFree } from '@/renderer/utils/policy/nativeExceedsFree';
import { videoLimitsFromEntitlement } from '@/renderer/utils/policy/videoLimits';
import { FREE_ENTITLEMENT, type Entitlement } from '@/renderer/stores/auth/subscriptionStore';

// The screen-share option lists gate on the STREAM axis (#1602). The free stream
// axis raw ceilings are 1080p / 60fps post-#2163 (the pixel-rate cap tiers fps
// separately — nativeExceedsFree only knows the raw axis); premium is native.
const FREE_STREAM = videoLimitsFromEntitlement(FREE_ENTITLEMENT).stream;

const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  streamMaxHeight: -1,
  streamMaxFps: -1,
  streamMaxPixelRate: -1,
};
const PREMIUM_STREAM = videoLimitsFromEntitlement(PREMIUM_ENTITLEMENT).stream;

describe('nativeExceedsFree — free user (stream axis 1080p, raw fps 60)', () => {
  it('reports exceeds when native height beats the free stream cap', () => {
    const r = nativeExceedsFree({ nativeHeight: 2160, nativeFps: 30 }, FREE_STREAM);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(1080);
    expect(r.clampedFps).toBe(30);
  });

  it('reports exceeds when native fps beats the free stream cap', () => {
    const r = nativeExceedsFree({ nativeHeight: 1080, nativeFps: 120 }, FREE_STREAM);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(1080);
    expect(r.clampedFps).toBe(60); // free stream raw fps ceiling (pixel-rate tiers it separately)
  });

  it('does NOT report exceeds when the device is within the free stream caps', () => {
    const r = nativeExceedsFree({ nativeHeight: 720, nativeFps: 30 }, FREE_STREAM);
    expect(r.exceeds).toBe(false);
    expect(r.clampedHeight).toBe(720);
    expect(r.clampedFps).toBe(30);
  });

  it('clamps both ceilings to the free stream caps when both exceed', () => {
    const r = nativeExceedsFree({ nativeHeight: 4320, nativeFps: 240 }, FREE_STREAM);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(1080);
    expect(r.clampedFps).toBe(60); // raw fps ceiling
  });
});

describe('nativeExceedsFree — premium passthrough (native stream axis)', () => {
  it('does NOT report exceeds for a native (uncapped) premium stream axis', () => {
    const r = nativeExceedsFree({ nativeHeight: 2160, nativeFps: 120 }, PREMIUM_STREAM);
    expect(r.exceeds).toBe(false);
    // Native/uncapped axis (Infinity) → clamps to the native value.
    expect(r.clampedHeight).toBe(2160);
    expect(r.clampedFps).toBe(120);
  });
});
