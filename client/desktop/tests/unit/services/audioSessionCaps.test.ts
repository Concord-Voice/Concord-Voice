import { describe, expect, it } from 'vitest';
import { FREE_ENTITLEMENT } from '@/renderer/stores/auth/subscriptionStore';
import {
  capAudioPtime,
  capAudioTier,
  effectiveAudioCaps,
  resolveSessionAudioCaps,
} from '@/renderer/services/voice/audioSessionCaps';

const premium = {
  ...FREE_ENTITLEMENT,
  tier: 'premium' as const,
  allowedAudioTiers: ['minimum', 'low', 'moderate', 'standard', 'high', 'hifi', 'studio'],
  minPtimeMs: 10,
};

describe('audio session caps', () => {
  it('pins the SFU-admitted Free policy across a later Premium store upgrade', () => {
    const caps = resolveSessionAudioCaps(
      { allowedAudioTiers: FREE_ENTITLEMENT.allowedAudioTiers, minPtimeMs: 20 },
      { tier: 'premium', allowed_audio_tiers: premium.allowedAudioTiers, min_ptime_ms: 10 },
      null
    );
    expect(effectiveAudioCaps(caps, premium)).toEqual({ maxBitrate: 96_000, minPtimeMs: 20 });
    expect(capAudioTier('studio', effectiveAudioCaps(caps, premium).maxBitrate)).toBe('standard');
  });

  it('lowers a Premium admission when the live personal entitlement drops to Free', () => {
    const caps = resolveSessionAudioCaps(
      { allowedAudioTiers: premium.allowedAudioTiers, minPtimeMs: 10 },
      null,
      null
    );
    expect(effectiveAudioCaps(caps, premium)).toEqual({ maxBitrate: 510_000, minPtimeMs: 10 });
    expect(effectiveAudioCaps(caps, FREE_ENTITLEMENT)).toEqual({
      maxBitrate: 96_000,
      minPtimeMs: 20,
    });
  });

  it('keeps a fixed channel standard even when the personal entitlement is Free', () => {
    const caps = resolveSessionAudioCaps(
      { allowedAudioTiers: premium.allowedAudioTiers, minPtimeMs: 10 },
      {
        tier: 'free',
        channel_audio_uplift: true,
        allowed_audio_tiers: premium.allowedAudioTiers,
        min_ptime_ms: 10,
      },
      'studio'
    );
    expect(effectiveAudioCaps(caps, FREE_ENTITLEMENT)).toEqual({
      maxBitrate: 510_000,
      minPtimeMs: 10,
    });
  });

  it('uses a newly enabled A2 channel standard when the earlier join had Personal mode', () => {
    const caps = resolveSessionAudioCaps(
      {
        allowedAudioTiers: premium.allowedAudioTiers,
        minPtimeMs: 10,
        channelAudioUpliftTier: 'high',
      },
      {
        tier: 'free',
        allowed_audio_tiers: FREE_ENTITLEMENT.allowedAudioTiers,
        min_ptime_ms: 20,
      },
      null
    );

    expect(caps.channelUpliftTier).toBe('high');
    expect(effectiveAudioCaps(caps, FREE_ENTITLEMENT)).toEqual({
      maxBitrate: 192_000,
      minPtimeMs: 10,
    });
  });

  it('removes a stale join uplift when the A2 admission says Personal mode', () => {
    const caps = resolveSessionAudioCaps(
      {
        allowedAudioTiers: FREE_ENTITLEMENT.allowedAudioTiers,
        minPtimeMs: 20,
        channelAudioUpliftTier: null,
      },
      {
        tier: 'free',
        channel_audio_uplift: true,
        allowed_audio_tiers: premium.allowedAudioTiers,
        min_ptime_ms: 10,
      },
      'studio'
    );

    expect(caps.channelUpliftTier).toBeNull();
    expect(effectiveAudioCaps(caps, FREE_ENTITLEMENT)).toEqual({
      maxBitrate: 96_000,
      minPtimeMs: 20,
    });
  });

  it('uses join authorization on an older SFU, and rejects malformed grant fields', () => {
    const caps = resolveSessionAudioCaps(
      { allowedAudioTiers: ['nonsense'], minPtimeMs: -1 },
      {
        tier: 'premium',
        allowed_audio_tiers: premium.allowedAudioTiers,
        min_ptime_ms: 10,
      },
      null
    );
    expect(effectiveAudioCaps(caps, premium)).toEqual({ maxBitrate: 510_000, minPtimeMs: 10 });
    expect(
      resolveSessionAudioCaps(
        {},
        { tier: 'free', allowed_audio_tiers: ['studio'], min_ptime_ms: 10 },
        null
      )
    ).toMatchObject({ allowedTiers: FREE_ENTITLEMENT.allowedAudioTiers, minPtimeMs: 20 });
    expect(capAudioPtime(10, 20)).toBe(20);
  });
});
