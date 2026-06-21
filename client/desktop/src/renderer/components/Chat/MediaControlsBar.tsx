import React from 'react';
import { Play, Pause, Volume2, VolumeX, Maximize, PictureInPicture2 } from 'lucide-react';
import type { MediaPlayerState, MediaPlayerActions } from '../../hooks/useMediaPlayer';
import { formatMediaTime } from '../../utils/formatMediaTime';

interface MediaControlsBarProps {
  readonly variant: 'video' | 'audio';
  readonly state: MediaPlayerState;
  readonly actions: MediaPlayerActions;
  readonly fullscreenSupported: boolean;
  readonly pipSupported: boolean;
}

/** Renders an accent-colored elapsed fill for a range input. The native track
 *  (`--bg-tertiary`) shows through to the right of the thumb. */
function accentFill(pct: number): React.CSSProperties {
  const clamped = Math.max(0, Math.min(100, pct));
  return {
    background: `linear-gradient(to right, var(--accent-color) ${clamped}%, var(--bg-tertiary) ${clamped}%)`,
  };
}

const MediaControlsBar: React.FC<MediaControlsBarProps> = ({
  variant,
  state,
  actions,
  fullscreenSupported,
  pipSupported,
}) => {
  const { playing, currentTime, duration, volume, muted, ready } = state;
  const max = Number.isFinite(duration) ? duration : 0;
  const isVideo = variant === 'video';
  const scrubPct = max > 0 ? (Math.min(currentTime, max) / max) * 100 : 0;
  const volPct = (muted ? 0 : volume) * 100;
  const isSilent = muted || volume === 0;

  return (
    <div className="media-controls" data-variant={variant}>
      <button
        type="button"
        className="media-controls-btn media-controls-play"
        onClick={actions.togglePlay}
        aria-label={playing ? 'Pause' : 'Play'}
      >
        {playing ? <Pause size={18} /> : <Play size={18} />}
      </button>

      <span className="media-controls-time" aria-hidden="true">
        {formatMediaTime(currentTime)}
      </span>

      <input
        type="range"
        className="media-controls-range media-controls-scrubber"
        min={0}
        max={max}
        step={0.1}
        value={Math.min(currentTime, max)}
        disabled={!ready}
        style={accentFill(scrubPct)}
        onChange={(e) => actions.seek(Number(e.target.value))}
        aria-label="Seek"
      />

      <span className="media-controls-time" aria-hidden="true">
        {formatMediaTime(duration)}
      </span>

      <button
        type="button"
        className="media-controls-btn media-controls-mute"
        onClick={actions.toggleMute}
        aria-label={isSilent ? 'Unmute' : 'Mute'}
      >
        {isSilent ? <VolumeX size={18} /> : <Volume2 size={18} />}
      </button>

      <input
        type="range"
        className="media-controls-range media-controls-volume"
        min={0}
        max={1}
        step={0.05}
        value={muted ? 0 : volume}
        style={accentFill(volPct)}
        onChange={(e) => actions.setVolume(Number(e.target.value))}
        aria-label="Volume"
      />

      {isVideo && pipSupported && (
        <button
          type="button"
          className="media-controls-btn media-controls-pip"
          onClick={actions.togglePiP}
          aria-label="Picture-in-Picture"
        >
          <PictureInPicture2 size={18} />
        </button>
      )}

      {isVideo && fullscreenSupported && (
        <button
          type="button"
          className="media-controls-btn media-controls-fullscreen"
          onClick={actions.requestFullscreen}
          aria-label="Fullscreen"
        >
          <Maximize size={18} />
        </button>
      )}
    </div>
  );
};

export default MediaControlsBar;
