import { useCallback, useMemo, useState } from 'react';
import {
  AUDIO_QUALITY_TIERS,
  AUDIO_TIER_ORDER,
  serverAudioCeilingTier,
  type AudioQualityTier,
} from '../../stores/voiceStore';
import '../common/audioQualitySlider.css';

interface ChannelAudioQualitySliderProps {
  /** Current channel standard: a tier name, or null for "Personal". */
  value: string | null;
  /** Emits the new standard: a tier name, or null for "Personal". */
  onChange: (tier: string | null) => void;
  /** Server-axis tier; bounds the selectable ceiling. */
  serverTier?: string;
  disabled?: boolean;
}

// Slider stops: index 0 = "Personal" (null), index i+1 = AUDIO_TIER_ORDER[i].
const PERSONAL_LABEL = 'Personal';

const ChannelAudioQualitySlider: React.FC<ChannelAudioQualitySliderProps> = ({
  value,
  onChange,
  serverTier,
  disabled,
}) => {
  const ceiling = serverAudioCeilingTier(serverTier);
  const ceilingRank = AUDIO_TIER_ORDER.indexOf(ceiling);
  const [lockHinted, setLockHinted] = useState(false);

  // Stop list: Personal + tiers. The selected index.
  const selectedIndex =
    value === null ? 0 : AUDIO_TIER_ORDER.indexOf(value as AudioQualityTier) + 1;
  const stopCount = AUDIO_TIER_ORDER.length + 1;

  const isTierLocked = useCallback(
    (tierIdx: number): boolean => tierIdx > ceilingRank, // tierIdx is the AUDIO_TIER_ORDER index
    [ceilingRank]
  );

  /** Resolve a stop index (0=Personal) to an emit, applying the lock snap-back. */
  const selectStop = useCallback(
    (stopIdx: number) => {
      if (stopIdx === 0) {
        setLockHinted(false);
        onChange(null);
        return;
      }
      const tierIdx = stopIdx - 1;
      if (isTierLocked(tierIdx)) {
        setLockHinted(true);
        return;
      }
      setLockHinted(false);
      onChange(AUDIO_TIER_ORDER[tierIdx]);
    },
    [isTierLocked, onChange]
  );

  const handleSlider = useCallback(
    (e: React.ChangeEvent<HTMLInputElement>) => {
      const idx = Number(e.target.value);
      if (idx >= 0 && idx < stopCount) selectStop(idx);
    },
    [selectStop, stopCount]
  );

  const kbpsLabel = useMemo(() => {
    if (value === null) return 'Personal';
    const cfg = AUDIO_QUALITY_TIERS[value as AudioQualityTier];
    return cfg ? `${Math.round(cfg.maxBitrate / 1000)} kbps` : '';
  }, [value]);

  return (
    <div className="settings-tier-slider-container">
      <div className="settings-tier-labels">
        {/* Personal stop */}
        <button
          type="button"
          className={`settings-tier-label ${selectedIndex === 0 ? 'active' : ''}`}
          aria-pressed={selectedIndex === 0}
          disabled={disabled}
          onClick={() => !disabled && selectStop(0)}
        >
          {PERSONAL_LABEL}
        </button>
        {AUDIO_TIER_ORDER.map((tier, i) => {
          const locked = isTierLocked(i);
          const stopIdx = i + 1;
          const label = AUDIO_QUALITY_TIERS[tier].label;
          return (
            <button
              type="button"
              key={tier}
              className={`settings-tier-label ${selectedIndex === stopIdx ? 'active' : ''} ${locked ? 'settings-tier-label-locked' : ''}`}
              aria-pressed={selectedIndex === stopIdx}
              aria-label={locked ? `${label} locked. Available on Mach-boosted servers.` : label}
              disabled={disabled}
              onClick={() => !disabled && selectStop(stopIdx)}
            >
              {label}
              {locked && (
                <span className="settings-tier-lock-glyph" aria-hidden="true">
                  {'\u{1F512}'}
                </span>
              )}
            </button>
          );
        })}
      </div>
      <div className="settings-tier-track">
        <div className="settings-tier-ticks">
          {Array.from({ length: stopCount }).map((_, i) => {
            const stopKey = i === 0 ? 'personal' : AUDIO_TIER_ORDER[i - 1];
            return (
              <span
                key={stopKey}
                className={`settings-tier-tick ${selectedIndex === i ? 'active' : ''}`}
              />
            );
          })}
        </div>
        <input
          type="range"
          className="settings-tier-slider"
          min={0}
          max={stopCount - 1}
          step={1}
          value={selectedIndex}
          onChange={handleSlider}
          disabled={disabled}
          aria-label="Channel audio quality"
        />
      </div>
      <div className="settings-tier-kbps-container">
        <span
          className="settings-tier-kbps-label"
          style={{
            left: `calc(${100 / (2 * stopCount) + (selectedIndex / (stopCount - 1)) * (100 - 100 / stopCount)}%)`,
          }}
        >
          {kbpsLabel}
        </span>
      </div>
      {lockHinted && (
        <output className="settings-tier-lock-popover">
          <span className="settings-tier-lock-popover-text">
            High-fidelity channel audio is available on Mach-boosted servers.
          </span>
        </output>
      )}
    </div>
  );
};

export default ChannelAudioQualitySlider;
