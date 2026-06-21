import React from 'react';
import { Gauge, Crown } from 'lucide-react';
import { useVoiceStore, AUDIO_QUALITY_TIERS, type AudioQualityTier } from '../../stores/voiceStore';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import './QualityTierSelector.css';

const TIER_ORDER: AudioQualityTier[] = [
  'minimum',
  'low',
  'moderate',
  'standard',
  'high',
  'hifi',
  'studio',
];

const QualityTierSelector: React.FC = () => {
  const qualityTier = useVoiceStore((s) => s.qualityTier);

  const handleSelect = async (tier: AudioQualityTier) => {
    const config = AUDIO_QUALITY_TIERS[tier];
    if (config.premium) {
      // Deferred: see #601 — requires subscription system (#211)
    }
    const { voiceService } = await import('../../services/voiceService');
    await voiceService.setQualityTier(tier);
  };

  return (
    <div className="quality-tier">
      <div className="quality-tier__header">
        <Gauge size={14} />
        <span>Audio Quality</span>
      </div>
      <div className="quality-tier__options">
        {TIER_ORDER.map((tier) => {
          const config = AUDIO_QUALITY_TIERS[tier];
          return (
            <button
              key={tier}
              className={`quality-tier__option ${
                qualityTier === tier ? 'quality-tier__option--active' : ''
              }`}
              onClick={() => handleSelect(tier)}
            >
              <span className="quality-tier__option-label">
                {config.label}
                {config.premium && <Crown size={10} className="quality-tier__premium-icon" />}
              </span>
              <span className="quality-tier__option-bitrate">
                {config.maxBitrate >= 1000
                  ? `${(config.maxBitrate / 1000).toFixed(0)} kbps`
                  : `${config.maxBitrate} bps`}
              </span>
            </button>
          );
        })}
      </div>
    </div>
  );
};

export default QualityTierSelector;
