import { describe, it, expect } from 'vitest';
import { nativeExceedsFree } from '@/renderer/utils/nativeExceedsFree';
import { FREE_ENTITLEMENT, type Entitlement } from '@/renderer/stores/subscriptionStore';

const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  maxVideoHeight: 2160,
  maxVideoFps: 120,
};

describe('nativeExceedsFree — free user', () => {
  it('reports exceeds when native height beats the free cap', () => {
    const r = nativeExceedsFree({ nativeHeight: 2160, nativeFps: 60 }, FREE_ENTITLEMENT);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(FREE_ENTITLEMENT.maxVideoHeight); // 1080
    expect(r.clampedFps).toBe(60);
  });

  it('reports exceeds when native fps beats the free cap', () => {
    const r = nativeExceedsFree({ nativeHeight: 1080, nativeFps: 144 }, FREE_ENTITLEMENT);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(1080);
    expect(r.clampedFps).toBe(FREE_ENTITLEMENT.maxVideoFps); // 60
  });

  it('does NOT report exceeds when the device is within free caps', () => {
    const r = nativeExceedsFree({ nativeHeight: 720, nativeFps: 30 }, FREE_ENTITLEMENT);
    expect(r.exceeds).toBe(false);
    expect(r.clampedHeight).toBe(720);
    expect(r.clampedFps).toBe(30);
  });

  it('clamps both ceilings to the free caps when both exceed', () => {
    const r = nativeExceedsFree({ nativeHeight: 4320, nativeFps: 240 }, FREE_ENTITLEMENT);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(1080);
    expect(r.clampedFps).toBe(60);
  });
});

describe('nativeExceedsFree — premium passthrough', () => {
  it('does NOT report exceeds for a premium entitlement at 4K120', () => {
    const r = nativeExceedsFree({ nativeHeight: 2160, nativeFps: 120 }, PREMIUM_ENTITLEMENT);
    expect(r.exceeds).toBe(false);
    expect(r.clampedHeight).toBe(2160);
    expect(r.clampedFps).toBe(120);
  });

  it('clamps to the premium ceiling when native exceeds even premium', () => {
    const r = nativeExceedsFree({ nativeHeight: 4320, nativeFps: 240 }, PREMIUM_ENTITLEMENT);
    expect(r.exceeds).toBe(true);
    expect(r.clampedHeight).toBe(2160);
    expect(r.clampedFps).toBe(120);
  });
});
