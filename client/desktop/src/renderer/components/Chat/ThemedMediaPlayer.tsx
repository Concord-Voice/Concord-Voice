import React, { useRef, useEffect } from 'react';
import { useMediaPlayer } from '../../hooks/useMediaPlayer';
import MediaControlsBar from './MediaControlsBar';
import './ThemedMediaPlayer.css';

interface ThemedMediaPlayerProps {
  readonly src: string;
  readonly variant: 'video' | 'audio';
  readonly className?: string;
}

const FULLSCREEN_SUPPORTED = typeof document !== 'undefined' && document.fullscreenEnabled === true;
const PIP_SUPPORTED = typeof document !== 'undefined' && document.pictureInPictureEnabled === true;

/** Native media element with `controls={false}` plus a theme-following controls
 *  bar. Replaces the bare <video>/<audio> in chat attachments so playback chrome
 *  matches the active theme accent. */
const ThemedMediaPlayer: React.FC<ThemedMediaPlayerProps> = ({ src, variant, className }) => {
  const mediaRef = useRef<HTMLMediaElement>(null);
  const containerRef = useRef<HTMLDivElement>(null);
  const player = useMediaPlayer(mediaRef, containerRef);

  const { togglePlay, seek, nudge, setVolume, toggleMute, requestFullscreen, togglePiP, ...state } =
    player;
  const actions = { togglePlay, seek, nudge, setVolume, toggleMute, requestFullscreen, togglePiP };

  // Player-level keyboard shortcuts. Attached imperatively (not a JSX onKeyDown
  // on a static <div>) so the container needs neither tabIndex nor an
  // interactive role — shortcuts fire while focus is on any control inside the
  // player, via event bubbling. Live media values are read from the element so
  // the listener closure stays stable and is attached once.
  useEffect(() => {
    const el = containerRef.current;
    if (!el) return;
    const onKeyDown = (e: KeyboardEvent) => {
      const target = e.target as HTMLElement;
      const isRange = target instanceof HTMLInputElement && target.type === 'range';
      const isButton = target instanceof HTMLButtonElement;
      const media = mediaRef.current;
      switch (e.key) {
        case ' ':
        case 'k':
          // Defer to a focused control: buttons activate on Space natively, and
          // a focused range slider (scrubber/volume) must not toggle playback —
          // mirrors the isRange guards on the Arrow cases below.
          if (isButton || isRange) return;
          e.preventDefault();
          togglePlay();
          break;
        case 'ArrowLeft':
          if (isRange) return;
          e.preventDefault();
          nudge(-5);
          break;
        case 'ArrowRight':
          if (isRange) return;
          e.preventDefault();
          nudge(5);
          break;
        case 'ArrowUp':
          if (isRange) return;
          e.preventDefault();
          setVolume(Math.min(1, (media?.volume ?? 0) + 0.1));
          break;
        case 'ArrowDown':
          if (isRange) return;
          e.preventDefault();
          setVolume(Math.max(0, (media?.volume ?? 0) - 0.1));
          break;
        // 'm' (mute) and 'f' (fullscreen) are intentionally NOT guarded by
        // isButton/isRange and do not preventDefault: unlike Space (native
        // button activation) and the Arrows (native range adjustment), they
        // are not native keys for any focused control, so they fire as
        // player-wide shortcuts without clobbering a control's own handling.
        case 'm':
          toggleMute();
          break;
        case 'f':
          if (variant === 'video') requestFullscreen();
          break;
        default:
          break;
      }
    };
    el.addEventListener('keydown', onKeyDown);
    return () => el.removeEventListener('keydown', onKeyDown);
  }, [variant, togglePlay, nudge, setVolume, toggleMute, requestFullscreen]);

  // Plain layout container: deliberately no role/tabIndex. Accessibility comes
  // from the individually aria-labelled controls; the container-level keyboard
  // shortcuts (attached via addEventListener above) are a progressive
  // enhancement. A group/region role here would clutter screen-reader landmark
  // navigation for every inline media item in a message feed.
  return (
    <div ref={containerRef} className="themed-media-player" data-variant={variant}>
      {variant === 'video' ? (
        <video
          ref={mediaRef as React.RefObject<HTMLVideoElement>}
          src={src}
          className={className}
          controls={false}
          preload="metadata"
          playsInline
        >
          <track kind="captions" />
        </video>
      ) : (
        <audio
          ref={mediaRef as React.RefObject<HTMLAudioElement>}
          src={src}
          className={className}
          controls={false}
          preload="metadata"
        >
          <track kind="captions" />
        </audio>
      )}
      <MediaControlsBar
        variant={variant}
        state={state}
        actions={actions}
        fullscreenSupported={FULLSCREEN_SUPPORTED}
        pipSupported={PIP_SUPPORTED}
      />
    </div>
  );
};

export default ThemedMediaPlayer;
