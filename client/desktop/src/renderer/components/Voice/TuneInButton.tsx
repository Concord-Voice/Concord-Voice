import React, { useCallback } from 'react';
import { Tv } from 'lucide-react';
import { useVoiceStore, AvailableScreenShare } from '../../stores/voiceStore';
import { errorMessage } from '../../utils/redactError';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import './TuneInButton.css';

interface TuneInButtonProps {
  share: AvailableScreenShare;
}

const MAX_SCREEN_SHARES = 5;

/**
 * Redesigned Tune In button with TV-antenna icon.
 * Shows the sharer's name with a hover tooltip.
 * Disabled when at the 5-stream maximum.
 */
const TuneInButton: React.FC<TuneInButtonProps> = ({ share }) => {
  const tunedInCount = useVoiceStore((s) => Object.keys(s.tunedInScreenShares).length);
  const atLimit = tunedInCount >= MAX_SCREEN_SHARES;

  const handleTuneIn = useCallback(async () => {
    if (atLimit) return;
    try {
      const { voiceService } = await import('../../services/voiceService');
      await voiceService.tuneInToScreenShare(share.producerId, share.userId);
    } catch (err) {
      console.error('Failed to tune in:', errorMessage(err));
    }
  }, [share.producerId, share.userId, atLimit]);

  const displayName = share.displayName || share.username;

  return (
    <button
      className={`tune-in-btn${atLimit ? ' tune-in-btn--disabled' : ''}`}
      onClick={handleTuneIn}
      disabled={atLimit}
      title={
        atLimit
          ? `Maximum ${MAX_SCREEN_SHARES} screen shares`
          : `Tune in to ${displayName}'s screen`
      }
    >
      <Tv size={16} className="tune-in-btn__icon" />
      <span className="tune-in-btn__label">{displayName}&apos;s screen</span>
      <span className="tune-in-btn__hint">Tune In</span>
    </button>
  );
};

/**
 * Overlay showing all available (not yet tuned-in) screen shares.
 */
export const TuneInOverlay: React.FC = () => {
  const availableScreenShares = useVoiceStore((s) => s.availableScreenShares);

  if (availableScreenShares.length === 0) return null;

  return (
    <div className="tune-in-overlay">
      {availableScreenShares.map((share) => (
        <TuneInButton key={share.producerId} share={share} />
      ))}
    </div>
  );
};

export default TuneInButton;
