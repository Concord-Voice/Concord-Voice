import { describe, it, expect } from 'vitest';
import { AUDIO_TIER_ORDER, serverAudioCeilingTier } from '@/renderer/stores/voiceStore';

describe('audio tier helpers', () => {
  it('AUDIO_TIER_ORDER is the 7-tier ladder ascending', () => {
    expect(AUDIO_TIER_ORDER).toEqual([
      'minimum',
      'low',
      'moderate',
      'standard',
      'high',
      'hifi',
      'studio',
    ]);
  });

  it('serverAudioCeilingTier mirrors Go: groundspeed→standard, mach→studio', () => {
    expect(serverAudioCeilingTier('groundspeed')).toBe('standard');
    expect(serverAudioCeilingTier('mach')).toBe('studio');
    expect(serverAudioCeilingTier(undefined)).toBe('standard'); // fail-closed
    expect(serverAudioCeilingTier('garbage')).toBe('standard');
  });
});
