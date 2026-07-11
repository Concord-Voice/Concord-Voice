import React, { useRef, useEffect, useCallback, useId } from 'react';
import { ChevronLeft, ChevronRight, PictureInPicture2, LayoutGrid, Focus } from 'lucide-react';
import { useVoiceStore } from '../../stores/voiceStore';
import { useRenderStateReporter } from '../../hooks/useRenderStateReporter';
import type { RemoteVideoRole } from '../../services/remoteVideoLayerPolicy';
import { ScreenShareAudioControls } from './ScreenShareAudioControls';
import './VoiceStage.css';

/**
 * Single screen share video cell (used in both equal and focus modes).
 */
const StageVideo: React.FC<{
  stream?: MediaStream;
  sharerName: string;
  showOverlay?: boolean;
  label?: string;
  isPaused?: boolean;
  /**
   * Sharer userId whose per-stream audio controls (volume + mute) should render
   * in this cell's overlay. Set for a remote sharer with live screen audio so
   * every equal-weight tile is independently controllable, not just the dominant
   * one (#2162). Undefined = no control (local share, or no screen audio).
   */
  audioControlUserId?: string;
  /**
   * Producing user of a REMOTE screen share this cell renders — set only for a
   * remote share so this cell's rendered size feeds receiver screen layer demand
   * (#1924). Undefined for a local share (we never consume our own screen).
   */
  sharerUserId?: string;
  /** Render-size role bucket for the screen demand report (#1924). */
  renderRole?: RemoteVideoRole;
}> = ({
  stream,
  sharerName,
  showOverlay = true,
  label,
  isPaused = false,
  audioControlUserId,
  sharerUserId,
  renderRole = 'focus',
}) => {
  const videoRef = useRef<HTMLVideoElement>(null);
  const tileId = useId();

  // #1924: report this cell's rendered size/visibility for the remote screen so the
  // SFU can forward each viewer its smallest-sufficient layer and flip the screen
  // layering gate on heterogeneous demand. Inert for a local share (no sharerUserId).
  useRenderStateReporter({
    userId: sharerUserId ?? '',
    tileId,
    source: 'screen',
    elementRef: videoRef,
    role: renderRole,
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
    const videoEl = videoRef.current;
    return () => {
      if (videoEl) {
        videoEl.srcObject = null;
      }
    };
  }, [stream]);

  return (
    <div className="voice-stage__cell">
      {isPaused ? (
        <div className="voice-stage__paused-placeholder">
          <span className="voice-stage__paused-title">Your Screen Is Still Streaming</span>
          <span className="voice-stage__paused-hint">
            If you want it to stay on even when Concord Voice is in the background, use the
            Auto-Pause button in the Controls Panel below.
          </span>
        </div>
      ) : (
        <video ref={videoRef} className="voice-stage__video" autoPlay playsInline muted />
      )}
      {showOverlay && (
        <div className="voice-stage__cell-overlay">
          <span className="voice-stage__sharer-name">{label || `${sharerName}\u2019s screen`}</span>
          {audioControlUserId && <ScreenShareAudioControls sharerUserId={audioControlUserId} />}
        </div>
      )}
    </div>
  );
};

/**
 * Center stage for tuned-in screen shares (Mode B — middle section).
 *
 * Two sub-layouts controlled by `stageLayout`:
 * - **equal**: All tuned-in streams displayed in an equal-weight grid
 * - **focus**: Single dominant stream fills the stage; others go to StreamBar
 */
const VoiceStage: React.FC = () => {
  const dominantScreenShareId = useVoiceStore((s) => s.dominantScreenShareId);
  const tunedInScreenShares = useVoiceStore((s) => s.tunedInScreenShares);
  const participants = useVoiceStore((s) => s.participants);
  const setDominantScreenShare = useVoiceStore((s) => s.setDominantScreenShare);
  const stageLayout = useVoiceStore((s) => s.stageLayout);
  const toggleStageLayout = useVoiceStore((s) => s.toggleStageLayout);
  const localStreamPaused = useVoiceStore((s) => s.localStreamPaused);
  const activeScreenShares = useVoiceStore((s) => s.activeScreenShares);

  const tunedInIds = Object.keys(tunedInScreenShares);
  const hasMultiple = tunedInIds.length > 1;

  // Resolve a producerId to its owner via the metadata seam (#2088) —
  // finally honors the producerId (multi-sharer correct).
  const resolveShare = useCallback(
    (producerId: string) => {
      const meta = activeScreenShares[producerId];
      const participant = meta ? participants[meta.userId] : undefined;
      return {
        name: meta?.displayName || meta?.username || 'Unknown',
        stream: participant?.screenStream,
        isLocal: meta?.isLocal ?? false,
        userId: meta?.userId,
        hasScreenAudio: !!participant?.screenAudioStream,
      };
    },
    [activeScreenShares, participants]
  );

  // ── Focus mode helpers ──────────────────────────────────────────────
  const dominant = dominantScreenShareId ? resolveShare(dominantScreenShareId) : null;

  const cycle = useCallback(
    (direction: 1 | -1) => {
      if (!hasMultiple || !dominantScreenShareId) return;
      const idx = tunedInIds.indexOf(dominantScreenShareId);
      const nextIdx = (idx + direction + tunedInIds.length) % tunedInIds.length;
      setDominantScreenShare(tunedInIds[nextIdx]);
    },
    [hasMultiple, dominantScreenShareId, tunedInIds, setDominantScreenShare]
  );

  // Keyboard navigation (focus mode only)
  useEffect(() => {
    if (stageLayout !== 'focus' || !hasMultiple) return;
    const handler = (e: KeyboardEvent) => {
      if (e.key === 'ArrowLeft') cycle(-1);
      if (e.key === 'ArrowRight') cycle(1);
    };
    globalThis.addEventListener('keydown', handler);
    return () => globalThis.removeEventListener('keydown', handler);
  }, [stageLayout, hasMultiple, cycle]);

  // ── Empty state ─────────────────────────────────────────────────────
  if (tunedInIds.length === 0) {
    return (
      <div className="voice-stage voice-stage--empty">
        <span className="voice-stage__empty-text">No screen share selected</span>
      </div>
    );
  }

  const dominantSharerName = dominant?.name ?? 'Unknown';

  // ── Equal mode: grid of all tuned-in streams ────────────────────────
  if (stageLayout === 'equal') {
    return (
      <div className="voice-stage voice-stage--equal">
        {/* Layout toggle (top-left) */}
        {hasMultiple && (
          <button
            className="voice-stage__layout-toggle"
            onClick={toggleStageLayout}
            title="Switch to focus mode"
          >
            <Focus size={16} />
          </button>
        )}

        <div className="voice-stage__grid" data-count={tunedInIds.length}>
          {tunedInIds.map((producerId) => {
            const { name, stream, isLocal, userId, hasScreenAudio } = resolveShare(producerId);
            return (
              <StageVideo
                key={producerId}
                stream={isLocal && localStreamPaused ? undefined : stream}
                sharerName={name}
                isPaused={isLocal && localStreamPaused}
                audioControlUserId={!isLocal && hasScreenAudio && userId ? userId : undefined}
                sharerUserId={!isLocal && userId ? userId : undefined}
                // A single equal-layout share fills the stage, so it should demand the
                // focus layer (grid caps at layer 1); only ramp down to 'grid' once there
                // are multiple equal-weight cells (#1924).
                renderRole={hasMultiple ? 'grid' : 'focus'}
              />
            );
          })}
        </div>
      </div>
    );
  }

  // ── Focus mode: single dominant stream ──────────────────────────────
  const isDominantLocal = dominant?.isLocal ?? false;
  return (
    <div className="voice-stage">
      <StageVideo
        stream={isDominantLocal && localStreamPaused ? undefined : dominant?.stream}
        sharerName={dominantSharerName}
        showOverlay={false}
        isPaused={isDominantLocal && localStreamPaused}
        sharerUserId={!isDominantLocal ? dominant?.userId : undefined}
        renderRole="focus"
      />

      {/* Bottom overlay */}
      <div className="voice-stage__overlay">
        <span className="voice-stage__sharer-name">{dominantSharerName}&apos;s screen</span>
        {hasMultiple && (
          <span className="voice-stage__count">
            {tunedInIds.indexOf(dominantScreenShareId ?? '') + 1} / {tunedInIds.length}
          </span>
        )}
        {!isDominantLocal && dominant?.userId && dominant.hasScreenAudio && (
          <ScreenShareAudioControls sharerUserId={dominant.userId} />
        )}
      </div>

      {/* Layout toggle (top-left) */}
      {hasMultiple && (
        <button
          className="voice-stage__layout-toggle"
          onClick={toggleStageLayout}
          title="Switch to equal layout"
        >
          <LayoutGrid size={16} />
        </button>
      )}

      {/* PiP button (top-right) — pop out screen share to Electron PiP */}
      {globalThis.electron?.openPipWindow && dominantScreenShareId && (
        <button
          className="voice-stage__pip-btn"
          onClick={async () => {
            await globalThis.electron.openPipWindow({
              id: `screen-${dominantScreenShareId}`,
              width: 400,
              height: 300,
            });
          }}
          title="Pop out to PiP window"
        >
          <PictureInPicture2 size={16} />
        </button>
      )}

      {/* Cycle buttons (only when multiple shares in focus mode) */}
      {hasMultiple && (
        <>
          <button
            className="voice-stage__nav voice-stage__nav--prev"
            onClick={() => cycle(-1)}
            title="Previous screen share (←)"
          >
            <ChevronLeft size={20} />
          </button>
          <button
            className="voice-stage__nav voice-stage__nav--next"
            onClick={() => cycle(1)}
            title="Next screen share (→)"
          >
            <ChevronRight size={20} />
          </button>
        </>
      )}
    </div>
  );
};

export default VoiceStage;
