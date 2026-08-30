import React, { useCallback } from 'react';
import { Tv } from 'lucide-react';
import { ActiveScreenShare, MAX_TUNED_SCREEN_SHARES } from '../../stores/voice/voiceStore';
import { errorMessage } from '../../utils/redactError';
// voiceService is loaded on-demand via dynamic import() — see voiceService.ts
import './ShareTunePill.css';

interface ShareTunePillProps {
  share: ActiveScreenShare;
  tunedIn: boolean;
  atCap: boolean;
  /** Smaller variant for the Mode B user-frame bar / pill strip. */
  compact?: boolean;
  /** Render the sharer's name inside the pill (for surfaces without the
   *  user frame above it, e.g. the collapsed-bar pill strip). */
  showName?: boolean;
}

/**
 * Per-stream Tune In / Tune Out pill rendered directly below the frame of the
 * participant producing the stream (grid slot + user-frame bar + the
 * collapsed-bar pill strip). Relocated from the retired ScreenShareControls
 * dock; the #2088 semantics are preserved: manual tune-out records auto-tune
 * suppression, and tune-in is blocked at the tuned-in cap. Local shares render
 * nothing — the tile's screen badge and the controls-bar Stop button already
 * cover them.
 *
 * The at-cap state uses aria-disabled + a JS activation guard (the house
 * locked-control pattern) instead of the disabled attribute, so keyboard and
 * screen-reader users can still reach the control and hear why it is blocked.
 */
const ShareTunePill: React.FC<ShareTunePillProps> = ({
  share,
  tunedIn,
  atCap,
  compact = false,
  showName = false,
}) => {
  const name = share.displayName || share.username || 'Unknown';
  const blocked = !tunedIn && atCap;

  const handleClick = useCallback(async () => {
    // aria-disabled (unlike disabled) does not block activation — guard here.
    if (blocked) return;
    try {
      const { voiceService } = await import('../../services/voice/voiceService');
      if (tunedIn) {
        await voiceService.tuneOutOfScreenShare(share.producerId, { suppressAutoTune: true });
      } else {
        await voiceService.tuneInToScreenShare(share.producerId, share.userId);
      }
    } catch (err) {
      console.error('Screen-share tune action failed:', errorMessage(err));
    }
  }, [share.producerId, share.userId, tunedIn, blocked]);

  if (share.isLocal) return null;

  const capNoteId = `share-tune-cap-${share.producerId}`;
  const classes = [
    'share-tune-pill',
    tunedIn ? 'share-tune-pill--out' : '',
    compact ? 'share-tune-pill--compact' : '',
  ]
    .filter(Boolean)
    .join(' ');

  return (
    <>
      <button
        type="button"
        className={classes}
        onClick={handleClick}
        aria-disabled={blocked || undefined}
        aria-describedby={blocked ? capNoteId : undefined}
        title={blocked ? `Maximum ${MAX_TUNED_SCREEN_SHARES} screen shares` : undefined}
        aria-label={tunedIn ? `Tune out of ${name}'s screen` : `Tune in to ${name}'s screen`}
      >
        <Tv size={compact ? 10 : 12} className="share-tune-pill__icon" />
        <span className="share-tune-pill__label">
          {showName ? `${name} — ` : ''}
          {tunedIn ? 'Tune Out' : 'Tune In'}
        </span>
      </button>
      {blocked && (
        <span id={capNoteId} className="share-tune-pill__cap-note">
          Maximum {MAX_TUNED_SCREEN_SHARES} screen shares
        </span>
      )}
    </>
  );
};

export default ShareTunePill;
