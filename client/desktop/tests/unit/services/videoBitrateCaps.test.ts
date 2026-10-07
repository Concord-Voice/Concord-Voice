import { describe, expect, it } from 'vitest';
import { FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import { resolveSessionVideoBitrateCaps } from '@/renderer/services/voice/videoBitrateCaps';

describe('resolveSessionVideoBitrateCaps', () => {
  const premiumJoin = {
    tier: 'premium',
    camera_max_bitrate_bps: 6_000_000,
    screen_max_bitrate_bps: 20_000_000,
  };

  it('uses the caps the SFU actually admitted when joins disagree', () => {
    expect(
      resolveSessionVideoBitrateCaps(
        { cameraMaxBitrateBps: 2_500_000, screenMaxBitrateBps: 5_000_000 },
        premiumJoin
      )
    ).toEqual({ camera: 2_500_000, screen: 5_000_000 });
  });

  it('uses the control-plane join caps with an older SFU', () => {
    expect(resolveSessionVideoBitrateCaps({}, premiumJoin)).toEqual({
      camera: 6_000_000,
      screen: 20_000_000,
    });
  });

  it('falls back to free for missing or malformed fields', () => {
    expect(
      resolveSessionVideoBitrateCaps(
        { cameraMaxBitrateBps: NaN, screenMaxBitrateBps: -1 },
        {
          tier: 'premium',
          camera_max_bitrate_bps: '6000000',
          screen_max_bitrate_bps: Infinity,
        }
      )
    ).toEqual({
      camera: FREE_ENTITLEMENT.cameraMaxBitrate,
      screen: FREE_ENTITLEMENT.streamMaxBitrate,
    });
  });

  it('does not accept premium-sized fallback caps with a free join tier', () => {
    expect(
      resolveSessionVideoBitrateCaps(
        {},
        {
          tier: 'free',
          camera_max_bitrate_bps: 6_000_000,
          screen_max_bitrate_bps: 20_000_000,
        }
      )
    ).toEqual({
      camera: FREE_ENTITLEMENT.cameraMaxBitrate,
      screen: FREE_ENTITLEMENT.streamMaxBitrate,
    });
  });
});
