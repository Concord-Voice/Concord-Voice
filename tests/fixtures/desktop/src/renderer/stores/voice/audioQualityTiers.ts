/**
 * The seven-tier audio ladder (must match server-side AUDIO_QUALITY_TIERS).
 *
 * Pure data with NO imports, on purpose: the media plane's
 * audioPolicyParity.test.ts imports this file across workspaces to pin
 * AUDIO_TIER_OPUS_BITRATE_CEILING_BPS to these maxBitrate values, because the
 * #2153 policer derives its audio limit from that server map. An import added
 * here must resolve from services/media-plane too, and the desktop test
 * audioQualityTiers.test.ts fails if one appears. voiceStore.ts re-exports all
 * three names, so no importer changes.
 */
export type AudioQualityTier =
  'minimum' | 'low' | 'moderate' | 'standard' | 'high' | 'hifi' | 'studio';

export interface AudioQualityTierConfig {
  label: string;
  description: string;
  maxBitrate: number;
  opusDtx: boolean;
  opusFec: boolean;
  opusStereo: boolean;
  preferredFrameSize: 10 | 20 | 40 | 60;
  premium: boolean;
}

export const AUDIO_QUALITY_TIERS: Record<AudioQualityTier, AudioQualityTierConfig> = {
  minimum: {
    label: 'Minimum',
    description: 'Optimized for pure survival over quality',
    maxBitrate: 16_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 60,
    premium: false,
  },
  low: {
    label: 'Low',
    description: 'Prioritizes keeping you in the conversation',
    maxBitrate: 32_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 40,
    premium: false,
  },
  moderate: {
    label: 'Moderate',
    description: 'The industry standard sweet spot',
    maxBitrate: 64_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  standard: {
    label: 'Standard',
    description: 'The Concord default, maximum clarity',
    maxBitrate: 96_000,
    opusDtx: true,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 20,
    premium: false,
  },
  high: {
    label: 'High',
    description: 'Virtually transparent clarity',
    maxBitrate: 192_000,
    opusDtx: false,
    opusFec: true,
    opusStereo: false,
    preferredFrameSize: 10,
    premium: true,
  },
  hifi: {
    label: 'Hi-Fi',
    description: 'Maximum fidelity for power users',
    maxBitrate: 256_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
  studio: {
    label: 'Studio',
    description: 'The absolute ceiling, acoustically transparent 48kHz/16-bit',
    maxBitrate: 510_000,
    opusDtx: false,
    opusFec: false,
    opusStereo: true,
    preferredFrameSize: 10,
    premium: true,
  },
};
