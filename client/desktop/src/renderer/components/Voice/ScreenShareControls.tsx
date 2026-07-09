import React, { useCallback } from 'react';
import { Tv } from 'lucide-react';
import { useVoiceStore, ActiveScreenShare, MAX_TUNED_SCREEN_SHARES } from '../../stores/voiceStore';
import { errorMessage } from '../../utils/redactError';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import './ScreenShareControls.css';

const shareLabel = (share: ActiveScreenShare): string =>
  share.displayName || share.username || 'Unknown';

const ShareRow: React.FC<{
  share: ActiveScreenShare;
  tunedIn: boolean;
  atCap: boolean;
}> = ({ share, tunedIn, atCap }) => {
  const name = shareLabel(share);

  const handleClick = useCallback(async () => {
    try {
      const { voiceService } = await import('../../services/voiceService');
      if (tunedIn) {
        await voiceService.tuneOutOfScreenShare(share.producerId, { suppressAutoTune: true });
      } else {
        await voiceService.tuneInToScreenShare(share.producerId, share.userId);
      }
    } catch (err) {
      console.error('Screen-share tune action failed:', errorMessage(err));
    }
  }, [share.producerId, share.userId, tunedIn]);

  if (share.isLocal) {
    return (
      <div className="share-controls__row share-controls__row--local">
        <Tv size={14} className="share-controls__icon" />
        <span className="share-controls__name">{name} (You)</span>
        <span className="share-controls__chip">Sharing</span>
      </div>
    );
  }

  const disabled = !tunedIn && atCap;
  return (
    <div className="share-controls__row">
      <Tv size={14} className="share-controls__icon" />
      <span className="share-controls__name">{name}&apos;s screen</span>
      <button
        type="button"
        className={`share-controls__btn${tunedIn ? ' share-controls__btn--out' : ''}`}
        onClick={handleClick}
        disabled={disabled}
        title={disabled ? `Maximum ${MAX_TUNED_SCREEN_SHARES} screen shares` : undefined}
        aria-label={tunedIn ? `Tune out of ${name}'s screen` : `Tune in to ${name}'s screen`}
      >
        {tunedIn ? 'Tune Out' : 'Tune In'}
      </button>
    </div>
  );
};

/**
 * Unified screen-share control dock (#2088): every active share listed once,
 * per-stream Tune In/Tune Out, plus global Tune In All / Tune Out All.
 * Replaces the Tune-In-only overlay.
 */
const ScreenShareControls: React.FC = () => {
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const availableScreenShares = useVoiceStore((s) => s.availableScreenShares);

  const shares: ActiveScreenShare[] =
    Object.keys(activeScreenShares).length > 0
      ? Object.values(activeScreenShares)
      : // Migration fallback: derive rows from the legacy available list
        availableScreenShares.map((s) => ({ ...s, isLocal: false }));

  if (shares.length === 0) return null;

  const tunedInCount = Object.keys(tunedInScreenShares).length;
  const atCap = tunedInCount >= MAX_TUNED_SCREEN_SHARES;
  const remoteAvailable = shares.filter(
    (s) => !s.isLocal && !(s.producerId in tunedInScreenShares)
  );
  const remoteTunedIn = shares.filter((s) => !s.isLocal && s.producerId in tunedInScreenShares);

  const handleTuneInAll = async () => {
    try {
      const { voiceService } = await import('../../services/voiceService');
      await voiceService.tuneInAllScreenShares();
    } catch (err) {
      console.error('Tune In All failed:', errorMessage(err));
    }
  };

  const handleTuneOutAll = async () => {
    try {
      const { voiceService } = await import('../../services/voiceService');
      await voiceService.tuneOutAllScreenShares();
    } catch (err) {
      console.error('Tune Out All failed:', errorMessage(err));
    }
  };

  return (
    <fieldset className="share-controls">
      <legend className="share-controls__legend">Screen share controls</legend>
      {shares.map((share) => (
        <ShareRow
          key={share.producerId}
          share={share}
          tunedIn={share.producerId in tunedInScreenShares}
          atCap={atCap}
        />
      ))}
      {(remoteAvailable.length > 0 || remoteTunedIn.length > 0) && (
        <div className="share-controls__global">
          {remoteAvailable.length > 0 && (
            <button
              type="button"
              className="share-controls__btn share-controls__btn--global"
              onClick={handleTuneInAll}
              disabled={atCap}
              title={atCap ? `Maximum ${MAX_TUNED_SCREEN_SHARES} screen shares` : undefined}
            >
              Tune In All
            </button>
          )}
          {remoteTunedIn.length > 0 && (
            <button
              type="button"
              className="share-controls__btn share-controls__btn--global share-controls__btn--out"
              onClick={handleTuneOutAll}
            >
              Tune Out All
            </button>
          )}
        </div>
      )}
    </fieldset>
  );
};

export default ScreenShareControls;
