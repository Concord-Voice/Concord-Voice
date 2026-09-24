import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';
import * as voiceStore from '@/renderer/stores/voice/voiceStore';
import { AUDIO_QUALITY_TIERS } from '@/renderer/stores/voice/audioQualityTiers';

describe('audio quality tiers module (#2153)', () => {
  it('is the very object voiceStore re-exports, so no importer changes', () => {
    expect(voiceStore.AUDIO_QUALITY_TIERS).toBe(AUDIO_QUALITY_TIERS);
  });

  it('keeps the seven-tier ladder the media plane polices against', () => {
    const ladder = Object.fromEntries(
      Object.entries(AUDIO_QUALITY_TIERS).map(([tier, c]) => [tier, [c.maxBitrate, c.premium]])
    );
    expect(ladder).toStrictEqual({
      minimum: [16_000, false],
      low: [32_000, false],
      moderate: [64_000, false],
      standard: [96_000, false],
      high: [192_000, true],
      hifi: [256_000, true],
      studio: [510_000, true],
    });
  });

  it('imports nothing, so the media-plane parity test can load it', () => {
    const source = readFileSync(
      join(__dirname, '../../../src/renderer/stores/voice/audioQualityTiers.ts'),
      'utf8'
    );
    expect(source).not.toMatch(/^\s*import\s/m);
    expect(source).not.toMatch(/\brequire\(/);
  });
});
