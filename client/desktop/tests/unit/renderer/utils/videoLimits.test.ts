import { describe, it, expect } from 'vitest';
import {
  videoLimitsFromEntitlement,
  maxFpsForResolution,
  clampScreenCapture,
  clampScreenForSubscription,
  effectiveStreamAxis,
  shouldEnforceForSubscription,
  cameraSpatialCapFromJoin,
  cameraSpatialCapFromAck,
  resolveSessionCameraCap,
} from '@/renderer/utils/policy/videoLimits';
import { FREE_ENTITLEMENT, type Entitlement } from '@/renderer/stores/auth/subscriptionStore';

const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  streamMaxHeight: -1,
  streamMaxFps: -1,
  streamMaxPixelRate: -1,
};

describe('#2163 tiered screenshare limits — videoLimits', () => {
  it('derives pixelRate on the stream axis; camera axis is Infinity', () => {
    const l = videoLimitsFromEntitlement(FREE_ENTITLEMENT);
    expect(l.stream.pixelRate).toBe(62208000);
    expect(l.camera.pixelRate).toBe(Infinity);
  });

  it('normalizes native (-1) stream pixelRate to Infinity', () => {
    const l = videoLimitsFromEntitlement(PREMIUM_ENTITLEMENT);
    expect(l.stream.pixelRate).toBe(Infinity);
  });

  describe('server video floor (#1522/#2163) lifts the pixel-rate budget', () => {
    it('raises pixelRate alongside height/fps so the floor is not self-rejecting', () => {
      // A 1080p60 floor implies a 16:9 budget of 1920*1080*60 = 124,416,000 px/s,
      // which is above the free 1080p30 ceiling (62,208,000), so it wins.
      const l = videoLimitsFromEntitlement(FREE_ENTITLEMENT, { height: 1080, fps: 60 });
      expect(l.stream.height).toBe(1080);
      expect(l.stream.fps).toBe(60);
      expect(l.stream.pixelRate).toBe(124416000);
      // Without the pixel-rate lift the tiered clamp would still cap 1080p at 30.
      expect(maxFpsForResolution(1920, 1080, l.stream)).toBe(60);
    });

    it('keeps the higher personal pixelRate when the floor budget is lower', () => {
      // A 720p30 floor (1280*720*30 = 27,648,000) is below the free ceiling, so
      // the personal pixel-rate cap is retained.
      const l = videoLimitsFromEntitlement(FREE_ENTITLEMENT, { height: 720, fps: 30 });
      expect(l.stream.pixelRate).toBe(62208000);
    });

    it('leaves pixelRate untouched when no floor is supplied', () => {
      const l = videoLimitsFromEntitlement(FREE_ENTITLEMENT);
      expect(l.stream.pixelRate).toBe(62208000);
    });
  });

  describe('maxFpsForResolution', () => {
    const s = videoLimitsFromEntitlement(FREE_ENTITLEMENT).stream;
    it('caps 1080p at 30fps (pixel-rate binds)', () => {
      expect(maxFpsForResolution(1920, 1080, s)).toBe(30);
    });
    it('allows 720p at 60fps (fps ceiling binds)', () => {
      expect(maxFpsForResolution(1280, 720, s)).toBe(60);
    });
    it('returns the fps ceiling for a native (Infinity pixelRate) axis', () => {
      const p = videoLimitsFromEntitlement(PREMIUM_ENTITLEMENT).stream;
      expect(maxFpsForResolution(1920, 1080, p)).toBe(Infinity);
    });
    it('returns the fps ceiling for a zero/negative area (defensive)', () => {
      expect(maxFpsForResolution(0, 0, s)).toBe(s.fps);
    });
    it('returns the fps ceiling for a NaN area, never a NaN ceiling (#2172)', () => {
      expect(maxFpsForResolution(Number.NaN, 1080, s)).toBe(s.fps);
      expect(maxFpsForResolution(1920, Number.NaN, s)).toBe(s.fps);
    });
  });

  describe('clampScreenCapture', () => {
    const s = videoLimitsFromEntitlement(FREE_ENTITLEMENT).stream;
    it('clamps 1080p60 fps to 30 (resolution fits)', () => {
      expect(clampScreenCapture(1920, 1080, 60, s)).toEqual({ width: 1920, height: 1080, fps: 30 });
    });
    it('admits 720p60 unchanged', () => {
      expect(clampScreenCapture(1280, 720, 60, s)).toEqual({ width: 1280, height: 720, fps: 60 });
    });
    it('clamps 4K/60 to 1080p/30 (proportional width)', () => {
      expect(clampScreenCapture(3840, 2160, 60, s)).toEqual({ width: 1920, height: 1080, fps: 30 });
    });
    it('clamps a 21:9 ultrawide height, scaling width proportionally to even px', () => {
      // 3440x1440 → height→1080, width = round(3440*1080/1440 /2)*2 = 2580
      const r = clampScreenCapture(3440, 1440, 60, s);
      expect(r.height).toBe(1080);
      expect(r.width).toBe(2580);
      expect(r.width % 2).toBe(0);
    });
    it('is a no-op for a premium (native) axis', () => {
      const p = videoLimitsFromEntitlement(PREMIUM_ENTITLEMENT).stream;
      expect(clampScreenCapture(3840, 2160, 60, p)).toEqual({ width: 3840, height: 2160, fps: 60 });
    });
  });

  describe('clampScreenForSubscription', () => {
    it('is a no-op until hydrated', () => {
      const sub = { hydrated: false, degraded: false, entitlement: FREE_ENTITLEMENT };
      expect(clampScreenForSubscription(3840, 2160, 60, sub)).toEqual({
        width: 3840,
        height: 2160,
        fps: 60,
      });
    });
    it('clamps a FREE user even when degraded — a failed /entitlements fetch does not open the escape (#2172)', () => {
      const sub = { hydrated: false, degraded: true, entitlement: FREE_ENTITLEMENT };
      expect(clampScreenForSubscription(3840, 2160, 60, sub)).toEqual({
        width: 1920,
        height: 1080,
        fps: 30,
      });
    });
    it('is a no-op for a degraded NON-free tier (defensive fail-open — never clamp a user still showing premium)', () => {
      const sub = { hydrated: true, degraded: true, entitlement: PREMIUM_ENTITLEMENT };
      expect(clampScreenForSubscription(3840, 2160, 60, sub)).toEqual({
        width: 3840,
        height: 2160,
        fps: 60,
      });
    });
    it('clamps a free user once authoritative', () => {
      const sub = { hydrated: true, degraded: false, entitlement: FREE_ENTITLEMENT };
      expect(clampScreenForSubscription(3840, 2160, 60, sub)).toEqual({
        width: 1920,
        height: 1080,
        fps: 30,
      });
    });
    it('is a no-op for an authoritative premium user', () => {
      const sub = { hydrated: true, degraded: false, entitlement: PREMIUM_ENTITLEMENT };
      expect(clampScreenForSubscription(3840, 2160, 60, sub)).toEqual({
        width: 3840,
        height: 2160,
        fps: 60,
      });
    });
  });

  describe('effectiveStreamAxis — the shared authoritative-vs-fail-open gate (#2172)', () => {
    const NATIVE = { height: Infinity, fps: Infinity, bitrate: Infinity, pixelRate: Infinity };
    it('pre-hydrate → native (fail open, tier not yet known)', () => {
      expect(
        effectiveStreamAxis({ hydrated: false, degraded: false, entitlement: FREE_ENTITLEMENT })
      ).toEqual(NATIVE);
    });
    it('authoritative free → the free stream axis (clamps)', () => {
      const a = effectiveStreamAxis({
        hydrated: true,
        degraded: false,
        entitlement: FREE_ENTITLEMENT,
      });
      expect(a.height).toBe(1080);
      expect(a.fps).toBe(60);
      expect(a.pixelRate).toBe(62208000);
    });
    it('authoritative premium → native (uncapped) axis', () => {
      const a = effectiveStreamAxis({
        hydrated: true,
        degraded: false,
        entitlement: PREMIUM_ENTITLEMENT,
      });
      expect(a.fps).toBe(Infinity);
      expect(a.pixelRate).toBe(Infinity);
    });
    it('degraded FREE → the free stream axis (monetization escape stays closed)', () => {
      const a = effectiveStreamAxis({
        hydrated: false,
        degraded: true,
        entitlement: FREE_ENTITLEMENT,
      });
      expect(a.pixelRate).toBe(62208000);
    });
    it('degraded PREMIUM → native (fail open; preserved premium tier is not clamped)', () => {
      expect(
        effectiveStreamAxis({ hydrated: true, degraded: true, entitlement: PREMIUM_ENTITLEMENT })
      ).toEqual(NATIVE);
    });
  });

  describe('shouldEnforceForSubscription: the axis-independent fail-open predicate (#2172)', () => {
    it('pre-hydrate (not degraded) → false (fail open, tier not yet known)', () => {
      expect(
        shouldEnforceForSubscription({
          hydrated: false,
          degraded: false,
          entitlement: FREE_ENTITLEMENT,
        })
      ).toBe(false);
    });
    it('authoritative free → true (enforce)', () => {
      expect(
        shouldEnforceForSubscription({
          hydrated: true,
          degraded: false,
          entitlement: FREE_ENTITLEMENT,
        })
      ).toBe(true);
    });
    it('degraded FREE (first-load failure) → true (enforce; monetization escape stays closed)', () => {
      expect(
        shouldEnforceForSubscription({
          hydrated: false,
          degraded: true,
          entitlement: FREE_ENTITLEMENT,
        })
      ).toBe(true);
    });
    it('degraded PREMIUM (reconnect failure) → false (fail open; preserved tier not clamped)', () => {
      expect(
        shouldEnforceForSubscription({
          hydrated: true,
          degraded: true,
          entitlement: PREMIUM_ENTITLEMENT,
        })
      ).toBe(false);
    });
  });
});

// The camera cap read from the SERVER's own join payload (#3279).
//
// This is not a client-side guess at the SFU's decision: `media_entitlements`
// rides the renderer join response, and `maxCameraSpatialLayerForParticipant`
// (roomManager.ts) derives the participant cap from this exact field. Reading it
// here makes client and SFU agree by construction, which neither a live store
// read nor a store snapshot taken at join can do -- the first is wrong on a
// mid-call upgrade, the second on a recovery rejoin that deliberately preserves a
// stale subscription snapshot (Codex, #3279).
describe('#3279 camera spatial cap from the join payload', () => {
  const PREMIUM_BPS = FREE_ENTITLEMENT.maxManualBitrateBps * 2;

  it('mirrors the SFU rule for a premium session', () => {
    expect(cameraSpatialCapFromJoin({ tier: 'premium', max_manual_bitrate_bps: PREMIUM_BPS })).toBe(
      2
    );
  });

  it('caps a free session at layer 1', () => {
    expect(
      cameraSpatialCapFromJoin({
        tier: 'free',
        max_manual_bitrate_bps: FREE_ENTITLEMENT.maxManualBitrateBps,
      })
    ).toBe(1);
  });

  // The media plane's atomic-free clamp: only an explicit premium tier may carry a
  // premium cap, so a cross-field-inconsistent response (a control-plane bug, or a
  // downgrade race) cannot raise the cap on its bitrate alone.
  it('refuses a premium bitrate that arrives on a non-premium tier', () => {
    expect(cameraSpatialCapFromJoin({ tier: 'free', max_manual_bitrate_bps: PREMIUM_BPS })).toBe(1);
    expect(cameraSpatialCapFromJoin({ max_manual_bitrate_bps: PREMIUM_BPS })).toBe(1);
  });

  // null means "no answer" and is distinct from 1 -- the caller falls back to the
  // store on null, and would wrongly clamp a premium viewer if this returned 1.
  it.each([
    ['undefined', undefined],
    ['null', null],
    ['a non-object', 'premium'],
    ['a missing bitrate', { tier: 'premium' }],
    ['a non-numeric bitrate', { tier: 'premium', max_manual_bitrate_bps: '9999999' }],
    ['a non-finite bitrate', { tier: 'premium', max_manual_bitrate_bps: Number.NaN }],
  ])('returns null for %s so the caller can fall back', (_label, payload) => {
    expect(cameraSpatialCapFromJoin(payload)).toBeNull();
  });

  // Total over unknown: a malformed field must degrade, never throw inside the
  // join path -- same contract as normalizeIceServers.
  // The bitrate is VALID here on purpose. Without it the function returns null at
  // the bitrate guard and never reads `tier`, so the case passes with no try/catch
  // anywhere -- it asserted totality while testing an early return.
  it('does not throw on a hostile payload, and still declines to answer', () => {
    const hostile = {
      max_manual_bitrate_bps: PREMIUM_BPS,
      get tier() {
        throw new Error('boom');
      },
    };
    expect(() => cameraSpatialCapFromJoin(hostile)).not.toThrow();
    expect(cameraSpatialCapFromJoin(hostile)).toBeNull();
  });
});

// Precedence for the session cap (#3279).
//
// The wiring line inside establishMediaSession had no test and could not easily
// get one -- that function needs a socket, a device and transports (Gitar,
// #3279). The decision was therefore extracted here, which is the part worth
// pinning; what remains at the call site is a single assignment.
describe('#3279 session camera cap precedence', () => {
  const PREMIUM_BPS = FREE_ENTITLEMENT.maxManualBitrateBps * 2;
  const freeStore = { hydrated: true, degraded: false, entitlement: { ...FREE_ENTITLEMENT } };
  const premiumStore = {
    hydrated: true,
    degraded: false,
    entitlement: { ...FREE_ENTITLEMENT, maxManualBitrateBps: PREMIUM_BPS },
  };
  const premiumJoin = { tier: 'premium', max_manual_bitrate_bps: PREMIUM_BPS };

  it('prefers the SFU admitted cap over both the REST join payload and the store', () => {
    expect(
      resolveSessionCameraCap({
        ackCap: 1,
        joinMediaEntitlements: premiumJoin,
        subscription: premiumStore,
      })
    ).toBe(1);
  });

  it('falls back to the REST join payload when the media plane omits the field', () => {
    expect(
      resolveSessionCameraCap({
        ackCap: undefined,
        joinMediaEntitlements: premiumJoin,
        subscription: freeStore,
      })
    ).toBe(2);
  });

  it('falls back to the store when neither wire source carries it', () => {
    expect(
      resolveSessionCameraCap({
        ackCap: undefined,
        joinMediaEntitlements: undefined,
        subscription: premiumStore,
      })
    ).toBe(2);
  });

  // 0 is a legitimate cap and is FALSY, so a `||` chain silently discards it and
  // falls through to a lower-authority source. The guard has to be `??`.
  it('respects an admitted cap of 0 rather than treating it as absent', () => {
    expect(cameraSpatialCapFromAck(0)).toBe(0);
    expect(
      resolveSessionCameraCap({
        ackCap: 0,
        joinMediaEntitlements: premiumJoin,
        subscription: premiumStore,
      })
    ).toBe(0);
  });

  it.each([
    ['a missing field', undefined],
    ['null', null],
    ['an out-of-range layer', 3],
    ['a numeric string', '1'],
  ])('treats %s as no answer so a lower-authority source is used', (_l, cap) => {
    expect(cameraSpatialCapFromAck(cap)).toBeNull();
  });
});
