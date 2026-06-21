import { useEffect, useState, useCallback, type RefObject } from 'react';

export interface MediaPlayerState {
  playing: boolean;
  currentTime: number;
  duration: number; // NaN until loadedmetadata
  volume: number; // 0..1
  muted: boolean;
  ready: boolean;
}

export interface MediaPlayerActions {
  togglePlay: () => void;
  seek: (time: number) => void;
  nudge: (deltaSeconds: number) => void;
  setVolume: (v: number) => void;
  toggleMute: () => void;
  requestFullscreen: () => void;
  togglePiP: () => void;
}

/** Subscribes to a media element and exposes declarative state + imperative
 *  actions. Fullscreen targets the container (so themed controls stay visible);
 *  PiP targets the video element. Isolates all DOM-event plumbing from the UI. */
export function useMediaPlayer(
  mediaRef: RefObject<HTMLMediaElement | null>,
  containerRef: RefObject<HTMLElement | null>
): MediaPlayerState & MediaPlayerActions {
  const [playing, setPlaying] = useState(false);
  const [currentTime, setCurrentTime] = useState(0);
  const [duration, setDuration] = useState(Number.NaN);
  // Internal state is `volumeLevel` (not `volume`) so the React setter satisfies
  // @eslint-react/use-state naming (`setVolumeLevel`) without colliding with the
  // public `setVolume` action callback below. Exposed as `volume` in the return.
  const [volumeLevel, setVolumeLevel] = useState(1);
  const [muted, setMuted] = useState(false);
  const [ready, setReady] = useState(false);

  // Subscribe to the media element. The dependency is [mediaRef] (a stable ref),
  // so listeners are attached once against the mediaRef.current captured on
  // mount. This is correct under the hook's contract: the consuming
  // <ThemedMediaPlayer> renders a single <video>/<audio> whose DOM instance is
  // stable for the component's lifetime (variant is fixed per attachment; React
  // reuses the node across re-renders). A future caller that swaps the element
  // instance while the hook stays mounted would need to add the element to this
  // effect's dependencies (e.g. via a callback ref).
  useEffect(() => {
    const el = mediaRef.current;
    if (!el) return;

    const onPlay = () => setPlaying(true);
    const onPause = () => setPlaying(false);
    const onTimeUpdate = () => setCurrentTime(el.currentTime);
    const onDuration = () => {
      setDuration(el.duration);
      setReady(Number.isFinite(el.duration));
    };
    const onVolume = () => {
      setVolumeLevel(el.volume);
      setMuted(el.muted);
    };
    const onEnded = () => setPlaying(false);

    el.addEventListener('play', onPlay);
    el.addEventListener('pause', onPause);
    el.addEventListener('timeupdate', onTimeUpdate);
    el.addEventListener('durationchange', onDuration);
    el.addEventListener('loadedmetadata', onDuration);
    el.addEventListener('volumechange', onVolume);
    el.addEventListener('ended', onEnded);

    // Sync initial state (element may already carry metadata).
    setVolumeLevel(el.volume);
    setMuted(el.muted);
    if (Number.isFinite(el.duration)) {
      setDuration(el.duration);
      setReady(true);
    }

    return () => {
      el.removeEventListener('play', onPlay);
      el.removeEventListener('pause', onPause);
      el.removeEventListener('timeupdate', onTimeUpdate);
      el.removeEventListener('durationchange', onDuration);
      el.removeEventListener('loadedmetadata', onDuration);
      el.removeEventListener('volumechange', onVolume);
      el.removeEventListener('ended', onEnded);
    };
  }, [mediaRef]);

  const togglePlay = useCallback(() => {
    const el = mediaRef.current;
    if (!el) return;
    if (el.paused) {
      const p = el.play();
      if (p && typeof p.catch === 'function') {
        p.catch(() => {
          /* autoplay rejected / element detached — stay paused */
        });
      }
    } else {
      el.pause();
    }
  }, [mediaRef]);

  const seek = useCallback(
    (time: number) => {
      const el = mediaRef.current;
      if (!el) return;
      el.currentTime = time;
      setCurrentTime(time);
    },
    [mediaRef]
  );

  const nudge = useCallback(
    (delta: number) => {
      const el = mediaRef.current;
      if (!el) return;
      const cap = Number.isFinite(el.duration) ? el.duration : el.currentTime + delta;
      const next = Math.max(0, Math.min(cap, el.currentTime + delta));
      el.currentTime = next;
      setCurrentTime(next);
    },
    [mediaRef]
  );

  const setVolume = useCallback(
    (v: number) => {
      const el = mediaRef.current;
      if (!el) return;
      const clamped = Math.max(0, Math.min(1, v));
      el.volume = clamped;
      if (clamped > 0 && el.muted) el.muted = false;
      setVolumeLevel(clamped);
      setMuted(el.muted);
    },
    [mediaRef]
  );

  const toggleMute = useCallback(() => {
    const el = mediaRef.current;
    if (!el) return;
    el.muted = !el.muted;
    setMuted(el.muted);
  }, [mediaRef]);

  const requestFullscreen = useCallback(() => {
    const container = containerRef.current;
    if (!container) return;
    if (document.fullscreenElement) {
      document.exitFullscreen().catch(() => {
        /* exit-fullscreen rejected — no-op */
      });
    } else if (typeof container.requestFullscreen === 'function') {
      container.requestFullscreen().catch(() => {
        /* request-fullscreen rejected — no-op */
      });
    }
  }, [containerRef]);

  const togglePiP = useCallback(() => {
    const el = mediaRef.current;
    if (!(el instanceof HTMLVideoElement)) return;
    if (!document.pictureInPictureEnabled) return;
    if (document.pictureInPictureElement) {
      document.exitPictureInPicture().catch(() => {
        /* exit-PiP rejected — no-op */
      });
    } else {
      el.requestPictureInPicture().catch(() => {
        /* PiP rejected — no-op */
      });
    }
  }, [mediaRef]);

  return {
    playing,
    currentTime,
    duration,
    volume: volumeLevel,
    muted,
    ready,
    togglePlay,
    seek,
    nudge,
    setVolume,
    toggleMute,
    requestFullscreen,
    togglePiP,
  };
}
