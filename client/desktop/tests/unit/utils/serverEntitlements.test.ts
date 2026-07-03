import { describe, expect, it } from 'vitest';
import {
  SERVER_TIER_ENTITLEMENTS,
  serverEntitlementsFor,
} from '@/renderer/utils/serverEntitlements';

describe('serverEntitlementsFor', () => {
  it('mirrors the Go ladder values (server_entitlements.go is source of truth)', () => {
    expect(SERVER_TIER_ENTITLEMENTS.groundspeed).toEqual({
      maxCustomEmoji: 75,
      maxStickers: 10,
      maxSoundboards: 15, // founder entitlement matrix baseline; public page omits the line item
      maxUploadBytes: 33_554_432,
      allowAnimatedBanner: false, // animated GIF server banner is Mach 1+ (#1302)
    });
    expect(SERVER_TIER_ENTITLEMENTS.mach1).toEqual({
      maxCustomEmoji: 250,
      maxStickers: 75,
      maxSoundboards: 30,
      maxUploadBytes: 134_217_728,
      allowAnimatedBanner: true,
    });
    expect(SERVER_TIER_ENTITLEMENTS.mach2).toEqual({
      maxCustomEmoji: 350,
      maxStickers: 100,
      maxSoundboards: 40,
      maxUploadBytes: 268_435_456,
      allowAnimatedBanner: true,
    });
    expect(SERVER_TIER_ENTITLEMENTS.mach3).toEqual({
      maxCustomEmoji: 500,
      maxStickers: 150,
      maxSoundboards: 55,
      maxUploadBytes: 536_870_912,
      allowAnimatedBanner: true,
    });
    expect(SERVER_TIER_ENTITLEMENTS.selfhost).toEqual({
      maxCustomEmoji: -1,
      maxStickers: -1,
      maxSoundboards: -1,
      maxUploadBytes: -1,
      allowAnimatedBanner: true,
    });
  });

  it('fails closed to groundspeed for unknown/absent tiers', () => {
    expect(serverEntitlementsFor(undefined)).toEqual(SERVER_TIER_ENTITLEMENTS.groundspeed);
    expect(serverEntitlementsFor('mach')).toEqual(SERVER_TIER_ENTITLEMENTS.groundspeed); // retired binary string
    expect(serverEntitlementsFor('MACH2')).toEqual(SERVER_TIER_ENTITLEMENTS.groundspeed);
  });

  it('rows are frozen — accidental consumer mutation throws instead of corrupting readers', () => {
    expect(Object.isFrozen(SERVER_TIER_ENTITLEMENTS)).toBe(true);
    for (const tier of Object.keys(SERVER_TIER_ENTITLEMENTS)) {
      expect(Object.isFrozen(SERVER_TIER_ENTITLEMENTS[tier])).toBe(true);
    }
    expect(() => {
      (serverEntitlementsFor('mach1') as { maxCustomEmoji: number }).maxCustomEmoji = 9999;
    }).toThrow(TypeError);
    expect(SERVER_TIER_ENTITLEMENTS.mach1.maxCustomEmoji).toBe(250);
  });
});
