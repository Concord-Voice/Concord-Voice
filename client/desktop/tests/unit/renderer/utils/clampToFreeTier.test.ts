import { describe, it, expect } from 'vitest';
import {
  clampToFreeTier,
  DEFAULT_COLOR_SCHEME,
  type ClampableSettings,
} from '@/renderer/utils/clampToFreeTier';
import { FREE_ENTITLEMENT, type Entitlement } from '@/renderer/stores/subscriptionStore';

/** A premium entitlement that allows everything the free floor blocks. */
const PREMIUM_ENTITLEMENT: Entitlement = {
  ...FREE_ENTITLEMENT,
  tier: 'premium',
  allowCustomScheme: true,
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
  allowMusicMode: true,
  maxVideoHeight: 2160,
  maxVideoFps: 120,
  maxVideoPixelRate: 3840 * 2160 * 120,
  maxManualBitrateBps: 30_000_000,
};

/** A settings snapshot that exceeds every free cap. */
const OVER_CAP_SETTINGS: ClampableSettings = {
  colorScheme: 'custom',
  qualityTier: 'studio',
  cameraPreset: '4K60',
  screenShareBitrate: 16_000_000,
  musicMode: true,
};

/** A settings snapshot already at/below the free floor. */
const FREE_SETTINGS: ClampableSettings = {
  colorScheme: 'concord',
  qualityTier: 'standard',
  cameraPreset: '720p30',
  screenShareBitrate: 0,
  musicMode: false,
};

describe('clampToFreeTier — free user clamps', () => {
  it('clamps every over-cap field and reports changed', () => {
    const { settings, changed } = clampToFreeTier(OVER_CAP_SETTINGS, FREE_ENTITLEMENT);
    expect(changed).toBe(true);
    expect(settings.colorScheme).toBe(DEFAULT_COLOR_SCHEME);
    expect(settings.qualityTier).toBe('standard');
    expect(settings.musicMode).toBe(false);
    // 16 Mbps → 5 Mbps free cap
    expect(settings.screenShareBitrate).toBe(FREE_ENTITLEMENT.maxManualBitrateBps);
    // 4K60 exceeds 1080/60 → clamped to highest free preset (≤1080, ≤60fps)
    expect(settings.cameraPreset).not.toBe('4K60');
  });

  it('clamps a custom scheme to the default', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, colorScheme: 'custom' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.colorScheme).toBe(DEFAULT_COLOR_SCHEME);
  });

  it('clamps a premium audio tier (studio) to standard', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, qualityTier: 'studio' },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.qualityTier).toBe('standard');
  });

  it('clamps a manual bitrate over 5 Mbps down to the free cap', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenShareBitrate: 12_000_000 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(true);
    expect(settings.screenShareBitrate).toBe(5_000_000);
  });

  it('does NOT clamp auto (0) bitrate', () => {
    const { settings, changed } = clampToFreeTier(
      { ...FREE_SETTINGS, screenShareBitrate: 0 },
      FREE_ENTITLEMENT
    );
    expect(changed).toBe(false);
    expect(settings.screenShareBitrate).toBe(0);
  });

  it('clamps an over-cap camera preset to a free-fitting preset', () => {
    const { settings } = clampToFreeTier(
      { ...FREE_SETTINGS, cameraPreset: '1440p60' },
      FREE_ENTITLEMENT
    );
    // The clamped preset must be within 1080p / 60fps free caps.
    expect(['system', '360p30', '480p30', '720p30', '720p60', '1080p30', '1080p60']).toContain(
      settings.cameraPreset
    );
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
