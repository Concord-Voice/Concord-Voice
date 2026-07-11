import React, { useRef, useEffect, useId } from 'react';
import { X } from 'lucide-react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useRenderStateReporter } from '../../hooks/useRenderStateReporter';
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
  /**
   * Producing user of a REMOTE screen share — set only for a remote share so this
   * thumbnail's rendered size feeds receiver screen layer demand (#1924). A small
   * thumbnail viewer is exactly the heterogeneity that lets the SFU forward a lower
   * layer and, across clients, flip the screen-layering gate. Undefined for a local
   * share (we never consume our own screen).
   */
  sharerUserId?: string;
  onSelect: () => void;
  onTuneOut: () => void;
}> = ({
  producerId: _producerId,
  stream,
  sharerName,
  isPaused = false,
  sharerUserId,
  onSelect,
  onTuneOut,
}) => {
  const videoRef = useRef<HTMLVideoElement>(null);
  const tileId = useId();

  // #1924: report this thumbnail's rendered size/visibility for the remote screen.
  useRenderStateReporter({
    userId: sharerUserId ?? '',
    tileId,
    source: 'screen',
    elementRef: videoRef,
    role: 'thumbnail',
    enabled: !!sharerUserId && !!stream && !isPaused,
  });

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
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);

  // Get non-dominant tuned-in shares
  const nonDominantIds = Object.keys(tunedInScreenShares).filter(
    (id) => id !== dominantScreenShareId
  );

  if (nonDominantIds.length === 0) return null;

  return (
    <div className="stream-bar" style={{ height }}>
      <div className="stream-bar__scroll">
        {nonDominantIds.map((producerId) => {
          // Resolve the owner via the producerId → owner metadata seam (#2088)
          const meta = activeScreenShares[producerId];
          const owner = meta ? participants[meta.userId] : undefined;
          const sharerName = meta?.displayName || meta?.username || 'Unknown';
          const isLocalSharer = meta?.isLocal ?? false;
          return (
            <StreamThumbnail
              key={producerId}
              producerId={producerId}
              stream={isLocalSharer && localStreamPaused ? undefined : owner?.screenStream}
              sharerName={sharerName}
              isPaused={isLocalSharer && localStreamPaused}
              sharerUserId={!isLocalSharer && meta?.userId ? meta.userId : undefined}
              onSelect={() => setDominantScreenShare(producerId)}
              onTuneOut={() => {
                // Import voiceService lazily to avoid circular dependency
                import('../../services/voiceService').then(({ voiceService }) => {
                  voiceService.tuneOutOfScreenShare(producerId, { suppressAutoTune: true });
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
