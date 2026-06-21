import React, { useRef, useEffect } from 'react';
import { X } from 'lucide-react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useUserStore } from '../../stores/userStore';
import './StreamBar.css';

/**
 * Thumbnail video for a tuned-in screen share (non-dominant).
 * Click to make it the dominant share in VoiceStage.
 */
const StreamThumbnail: React.FC<{
  producerId: string;
  stream?: MediaStream;
  sharerName: string;
  isPaused?: boolean;
  onSelect: () => void;
  onTuneOut: () => void;
}> = ({ producerId: _producerId, stream, sharerName, isPaused = false, onSelect, onTuneOut }) => {
  const videoRef = useRef<HTMLVideoElement>(null);

  useEffect(() => {
    const el = videoRef.current;
    if (!el) return;
    if (stream) {
      el.srcObject = stream;
      el.play().catch(() => {});
    } else {
      el.srcObject = null;
    }
    return () => {
      // Copy ref to local so cleanup acts on the element captured at setup,
      // not whatever videoRef.current might point at by the time cleanup runs.
      el.srcObject = null;
    };
  }, [stream]);

  return (
    <div className="stream-thumbnail">
      <button
        type="button"
        className="stream-thumbnail__select"
        onClick={onSelect}
        title={`View ${sharerName}'s screen`}
      >
        {isPaused ? (
          <div className="stream-thumbnail__paused">
            <span className="stream-thumbnail__paused-text">Paused</span>
          </div>
        ) : (
          <video ref={videoRef} className="stream-thumbnail__video" autoPlay playsInline muted />
        )}
        <div className="stream-thumbnail__label">{sharerName}</div>
      </button>
      <button
        type="button"
        className="stream-thumbnail__close"
        onClick={onTuneOut}
        title="Tune out"
      >
        <X size={12} />
      </button>
    </div>
  );
};

/**
 * Bottom horizontal strip for additional tuned-in screen shares (Mode B).
 * Shows thumbnails of all tuned-in shares except the dominant one.
 * Click a thumbnail to swap it into the center stage.
 */
const StreamBar: React.FC<{ height: number }> = ({ height }) => {
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const dominantScreenShareId = useVoiceStore((s) => s.dominantScreenShareId);
  const participants = useVoiceStore((s) => s.participants);
  const setDominantScreenShare = useVoiceStore((s) => s.setDominantScreenShare);
  const localStreamPaused = useVoiceStore((s) => s.localStreamPaused);
  const localUserId = useUserStore((s) => s.user?.id);

  // Get non-dominant tuned-in shares
  const nonDominantIds = Object.keys(tunedInScreenShares).filter(
    (id) => id !== dominantScreenShareId
  );

  if (nonDominantIds.length === 0) return null;

  return (
    <div className="stream-bar" style={{ height }}>
      <div className="stream-bar__scroll">
        {nonDominantIds.map((producerId) => {
          // Find the participant who owns this screen share
          const sharer = Object.values(participants).find(
            (p) => p.isScreenSharing && p.screenStream
          );
          const sharerName = sharer?.displayName || sharer?.username || 'Unknown';

          const isLocalSharer = sharer?.userId === localUserId;
          return (
            <StreamThumbnail
              key={producerId}
              producerId={producerId}
              stream={isLocalSharer && localStreamPaused ? undefined : sharer?.screenStream}
              sharerName={sharerName}
              isPaused={isLocalSharer && localStreamPaused}
              onSelect={() => setDominantScreenShare(producerId)}
              onTuneOut={() => {
                // Import voiceService lazily to avoid circular dependency
                import('../../services/voiceService').then(({ voiceService }) => {
                  voiceService.tuneOutOfScreenShare(producerId);
                });
              }}
            />
          );
        })}
      </div>
    </div>
  );
};

export default StreamBar;
