import { describe, it, expect } from 'vitest';
import { clampToFreeTier, type ClampableSettings } from '@/renderer/utils/policy/clampToFreeTier';
import { FREE_ENTITLEMENT, type Entitlement } from '@/renderer/stores/auth/subscriptionStore';

/** A premium entitlement that allows everything the free floor blocks. Both video
 *  axes are native (uncapped, -1 sentinel) with premium bitrate ceilings (#1602). */
const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  allowCustomScheme: true,
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
  allowMusicMode: true,
  streamMaxHeight: -1,
  streamMaxFps: -1,
  streamMaxPixelRate: -1,
  streamMaxBitrate: 20_000_000,
  cameraMaxHeight: -1,
  cameraMaxFps: -1,
  cameraMaxBitrate: 6_000_000,
  maxManualBitrateBps: 20_000_000,
};

/** A settings snapshot that exceeds every free cap. */
const OVER_CAP_SETTINGS: ClampableSettings = {
  colorScheme: 'custom',
  qualityTier: 'studio',
  cameraPreset: '4K60',
  screenShareBitrate: 16_000_000,
  cameraBitrate: 5_000_000,
  musicMode: true,
  screenResolution: '4K',
  screenFrameRate: 60,
};

/** A settings snapshot already at/below the free floor. */
const FREE_SETTINGS: ClampableSettings = {
  colorScheme: 'concord',
  qualityTier: 'standard',
  cameraPreset: '720p30',
  screenShareBitrate: 0,
  cameraBitrate: 0,
  musicMode: false,
  screenResolution: '1080p',
  screenFrameRate: 30,
};

describe('clampToFreeTier — free user clamps', () => {
  it('clamps every over-cap field and reports changed', () => {
    const { settings, changed } = clampToFreeTier(OVER_CAP_SETTINGS, FREE_ENTITLEMENT);
    expect(changed).toBe(true);
    expect(settings.colorScheme).toBe('custom');
    expect(settings.qualityTier).toBe('standard');
    expect(settings.musicMode).toBe(false);
    // 16 Mbps → 5 Mbps free STREAM cap
    expect(settings.screenShareBitrate).toBe(FREE_ENTITLEMENT.streamMaxBitrate);
    // 5 Mbps → 2.5 Mbps free CAMERA cap
    expect(settings.cameraBitrate).toBe(FREE_ENTITLEMENT.cameraMaxBitrate);
    // 4K60 exceeds the camera axis (720p60) → clamped to highest free camera preset
    expect(settings.cameraPreset).not.toBe('4K60');
  });

  it('leaves a custom scheme untouched for free users', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, colorScheme: 'custom' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.colorScheme).toBe('custom');
  });

  it('clamps a premium audio tier (studio) to standard', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, qualityTier: 'studio' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.qualityTier).toBe('standard');
  });

  it('clamps a screen-share bitrate over 5 Mbps down to the free STREAM cap', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenShareBitrate: 12_000_000 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenShareBitrate).toBe(5_000_000);
  });

  it('clamps a camera bitrate over 2.5 Mbps down to the free CAMERA cap', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, cameraBitrate: 6_000_000 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.cameraBitrate).toBe(2_500_000);
  });

  it('does NOT clamp auto (0) screen-share or camera bitrate', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenShareBitrate: 0, cameraBitrate: 0 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.screenShareBitrate).toBe(0);
    expect(settings.cameraBitrate).toBe(0);
  });

  it('clamps an over-cap camera preset to a free-fitting preset (camera axis 720p60)', () => {
    const { settings } = clampToFreeTier(
      { ...FREE_SETTINGS, cameraPreset: '1440p60' },
      FREE_ENTITLEMENT
    );
    // The clamped preset must be within the free CAMERA axis (720p / 60fps).
    expect(['system', '360p30', '480p30', '720p30', '720p60']).toContain(settings.cameraPreset);
  });

  it('clamps a 1080p30 camera preset (now over the camera axis) down', () => {
    // 1080p30 fit the OLD conflated free cap but exceeds the split camera axis (720p).
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, cameraPreset: '1080p30' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(['system', '360p30', '480p30', '720p30', '720p60']).toContain(settings.cameraPreset);
  });
});

describe('clampToFreeTier — idempotence & premium passthrough', () => {
  it('returns changed:false for already-free settings', () => {
    const { settings, changed } = clampToFreeTier(FREE_SETTINGS, FREE_ENTITLEMENT);
    expect(changed).toBe(false);
    expect(settings).toEqual(FREE_SETTINGS);
  });

  it('passes premium settings through untouched for a premium entitlement', () => {
    const { settings, changed } = clampToFreeTier(OVER_CAP_SETTINGS, PREMIUM_ENTITLEMENT);
    expect(changed).toBe(false);
    expect(settings).toEqual(OVER_CAP_SETTINGS);
  });

  it('does not mutate the input object', () => {
    const input: ClampableSettings = { ...OVER_CAP_SETTINGS };
    clampToFreeTier(input, FREE_ENTITLEMENT);
    expect(input).toEqual(OVER_CAP_SETTINGS);
  });

  it('leaves a free audio tier untouched (no false-positive clamp)', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, qualityTier: 'moderate' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.qualityTier).toBe('moderate');
  });

  it('leaves an unknown / System Default camera preset untouched', () => {
    const { changed } = clampToFreeTier(
      { ...FREE_SETTINGS, cameraPreset: 'system' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
  });
});

describe('clampToFreeTier — #2163 screenshare res/fps tiered clamp', () => {
  it('admits 1080p30 unchanged', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '1080p', screenFrameRate: 30 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.screenResolution).toBe('1080p');
    expect(settings.screenFrameRate).toBe(30);
  });

  it('admits 720p60 unchanged (60fps reserved for 720p and below)', () => {
    const { changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '720p', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
  });

  it('clamps 1080p60 fps to 30 (resolution stays 1080p)', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '1080p', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenResolution).toBe('1080p');
    expect(settings.screenFrameRate).toBe(30);
  });

  it('clamps 1440p60 to 1080p and fps to 30', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '1440p', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenResolution).toBe('1080p');
    expect(settings.screenFrameRate).toBe(30);
  });

  it('clamps 4K resolution to 1080p', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '4K', screenFrameRate: 30 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenResolution).toBe('1080p');
  });

  it('leaves source + native (0) fps for the produce boundary', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: 'source', screenFrameRate: 0 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.screenResolution).toBe('source');
    expect(settings.screenFrameRate).toBe(0);
  });

  it('clamps a custom WxH over the height ceiling to 1080p (dims encoded in value)', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '2560x1440', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenResolution).toBe('1080p');
    expect(settings.screenFrameRate).toBe(30);
  });

  it('tiers a custom ultrawide fps that fits the height ceiling but not the budget', () => {
    // 2560x1080 fits the 1080 height ceiling, so the resolution is kept, but the
    // pixel-rate budget (62.2 Mpx/s / 2,764,800 px) admits only 22fps.
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '2560x1080', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenResolution).toBe('2560x1080');
    expect(settings.screenFrameRate).toBe(22);
  });

  it('admits a custom WxH that fits both height and budget unchanged', () => {
    const { changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '1280x720', screenFrameRate: 60 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
  });

  it('is a no-op for a premium (native) entitlement', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenResolution: '4K', screenFrameRate: 60 },
      PREMIUM_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.screenResolution).toBe('4K');
    expect(settings.screenFrameRate).toBe(60);
  });
});
