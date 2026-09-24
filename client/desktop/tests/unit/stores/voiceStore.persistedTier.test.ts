import { afterEach, describe, expect, it, vi } from 'vitest';

// voiceStore reads its persisted settings once, at module load, so each case
// seeds storage and then imports a fresh copy of the module.
async function stateWithPersistedTier(tier: unknown) {
  localStorage.setItem('concord:voice-settings', JSON.stringify({ qualityTier: tier }));
  vi.resetModules();
  const { useVoiceStore } = await import('@/renderer/stores/voice/voiceStore');
  return useVoiceStore.getState();
}

afterEach(() => {
  localStorage.clear();
});

describe('voiceStore persisted quality tier', () => {
  // Every AUDIO_QUALITY_TIERS[qualityTier] lookup in Settings throws on an
  // unrecognised tier, so one bad stored value crashed Audio Configuration.
  it.each(['bogus', 42, ''])(
    'an unrecognised stored tier (%j) falls back to standard',
    async (tier) => {
      const state = await stateWithPersistedTier(tier);
      expect(state.qualityTier).toBe('standard');
      expect(state.effectiveQualityTier).toBe('standard');
    }
  );

  it('keeps a recognised stored tier', async () => {
    expect((await stateWithPersistedTier('hifi')).qualityTier).toBe('hifi');
  });

  it("still migrates the legacy 'voice' tier to low", async () => {
    expect((await stateWithPersistedTier('voice')).qualityTier).toBe('low');
  });
});
