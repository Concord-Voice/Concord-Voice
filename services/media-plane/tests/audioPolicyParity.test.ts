/**
 * Client/server audio-policy parity (#2153).
 *
 * The media-rate policer derives each audio slot's limit from two server-side
 * numbers that each mirror a desktop value:
 *
 *   ceiling       AUDIO_TIER_OPUS_BITRATE_CEILING_BPS  <->  AUDIO_QUALITY_TIERS[*].maxBitrate
 *   FEC headroom  FEC_HEADROOM                         <->  1 + FEC_MAX_HEADROOM_PERCENT / 100
 *
 * Before #2153 a drift between the tier maps only meant a rejected fmtp. Now a
 * client tier or FEC cap that rises without the server makes the policer pause
 * a STOCK client, and a second pause evicts it. So the pin imports the desktop
 * modules themselves rather than hardcoding their numbers here, the same shape
 * as cameraLayerPolicyParity.test.ts; crossWorkspaceImports.test.ts then
 * forces the CI media-plane test filter to list both files.
 */
import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { FEC_HEADROOM } from '@/lib/mediaPolicer';
import { AUDIO_TIER_OPUS_BITRATE_CEILING_BPS } from '@/lib/roomManager';
import { AUDIO_QUALITY_TIERS } from '../../../client/desktop/src/renderer/stores/voice/audioQualityTiers';
import {
  FEC_MAX_HEADROOM_PERCENT,
  calculateFecBitrate,
} from '../../../client/desktop/src/renderer/services/voice/fecHeadroom';

const CLIENT_VOICE = resolve(__dirname, '..', '..', '..', 'client', 'desktop', 'src', 'renderer');

describe('audio policy parity (#2153)', () => {
  it('gives every desktop tier a server ceiling equal to its maxBitrate, and no others', () => {
    const clientCeilings = Object.fromEntries(
      Object.entries(AUDIO_QUALITY_TIERS).map(([tier, config]) => [tier, config.maxBitrate])
    );
    expect(AUDIO_TIER_OPUS_BITRATE_CEILING_BPS).toStrictEqual(clientCeilings);
  });

  it('pins FEC_HEADROOM to the desktop FEC headroom cap', () => {
    expect(FEC_HEADROOM).toBe(1 + FEC_MAX_HEADROOM_PERCENT / 100);
  });

  it('never lets stock FEC headroom exceed ceiling x FEC_HEADROOM, at any loss', () => {
    for (const { maxBitrate } of Object.values(AUDIO_QUALITY_TIERS)) {
      for (const lossPercent of [1, 10, 25, 50, 100]) {
        expect(calculateFecBitrate(lossPercent, maxBitrate, true)).toBeLessThanOrEqual(
          maxBitrate * FEC_HEADROOM
        );
      }
      // At total loss the cap binds exactly, so the pin is not vacuously loose.
      expect(calculateFecBitrate(100, maxBitrate, true)).toBe(maxBitrate * FEC_HEADROOM);
    }
  });

  it('imports only modules with no imports of their own', () => {
    // This workspace resolves them without the desktop node_modules, so an
    // import added to either file must fail here rather than in CI.
    for (const file of ['stores/voice/audioQualityTiers.ts', 'services/voice/fecHeadroom.ts']) {
      const source = readFileSync(resolve(CLIENT_VOICE, file), 'utf8');
      expect(source, file).not.toMatch(/^\s*import\s/m);
      expect(source, file).not.toMatch(/\brequire\(/);
    }
  });
});
