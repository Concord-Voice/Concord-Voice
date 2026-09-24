import React from 'react';
import { X } from 'lucide-react';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useScreenTileVideo } from '../../hooks/voice/useScreenTileVideo';
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
  /** #2153: a REMOTE share whose producer is paused. Required so every call site states it. */
  remotePaused: boolean;
  onSelect: () => void;
  onTuneOut: () => void;
}> = ({
  producerId: _producerId,
  stream,
  sharerName,
  isPaused = false,
  sharerUserId,
  remotePaused,
  onSelect,
  onTuneOut,
}) => {
  // #1924: reports this thumbnail's rendered size/visibility for the remote
  // screen and owns the srcObject lifecycle (shared tile wiring).
  const videoRef = useScreenTileVideo({
    sharerUserId,
    stream,
    isPaused: isPaused || remotePaused, // no demand report for a paused producer
    role: 'thumbnail',
  });

  let preview: React.ReactNode;
  if (isPaused) {
    preview = (
      <div className="stream-thumbnail__paused">
        <span className="stream-thumbnail__paused-text">Paused</span>
      </div>
    );
  } else if (remotePaused) {
    preview = (
      <div className="stream-thumbnail__paused stream-thumbnail__paused--remote">
        <span className="stream-thumbnail__paused-text">Screen share paused</span>
      </div>
    );
  } else {
    preview = (
      <video ref={videoRef} className="stream-thumbnail__video" autoPlay playsInline muted />
    );
  }

  return (
    <div className="stream-thumbnail">
      <button
        type="button"
        className="stream-thumbnail__select"
        onClick={onSelect}
        title={`View ${sharerName}'s screen`}
      >
        {preview}
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
          const remotePaused = !isLocalSharer && (meta?.paused ?? false);
          const hidden = (isLocalSharer && localStreamPaused) || remotePaused;
          return (
            <StreamThumbnail
              key={producerId}
              producerId={producerId}
              stream={hidden ? undefined : owner?.screenStream}
              sharerName={sharerName}
              isPaused={isLocalSharer && localStreamPaused}
              remotePaused={remotePaused}
              sharerUserId={!isLocalSharer && meta?.userId ? meta.userId : undefined}
              onSelect={() => setDominantScreenShare(producerId)}
              onTuneOut={() => {
                // Import voiceService lazily to avoid circular dependency
                import('../../services/voice/voiceService').then(({ voiceService }) => {
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
