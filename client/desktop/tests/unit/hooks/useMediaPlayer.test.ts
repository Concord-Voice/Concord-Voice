import { describe, it, expect, vi, afterEach } from 'vitest';
import { renderHook, act } from '@testing-library/react';
import { useMediaPlayer } from '@/renderer/hooks/useMediaPlayer';

class FakeMedia extends EventTarget {
  paused = true;
  currentTime = 0;
  duration = NaN;
  volume = 1;
  muted = false;
  play = vi.fn(() => {
    this.paused = false;
    this.dispatchEvent(new Event('play'));
    return Promise.resolve();
  });
  pause = vi.fn(() => {
    this.paused = true;
    this.dispatchEvent(new Event('pause'));
  });
}

function makeContainer() {
  return { current: document.createElement('div') };
}

function setup(init: Partial<FakeMedia> = {}) {
  const media = Object.assign(new FakeMedia(), init);
  const mediaRef = { current: media as unknown as HTMLMediaElement };
  const view = renderHook(() => useMediaPlayer(mediaRef, makeContainer()));
  return { media, view };
}

describe('useMediaPlayer', () => {
  afterEach(() => vi.restoreAllMocks());

  it('starts paused at time 0', () => {
    const { view } = setup();
    expect(view.result.current.playing).toBe(false);
    expect(view.result.current.currentTime).toBe(0);
  });

  it('togglePlay plays a paused element and reflects the play event', () => {
    const { media, view } = setup();
    act(() => view.result.current.togglePlay());
    expect(media.play).toHaveBeenCalled();
    expect(view.result.current.playing).toBe(true);
  });

  it('togglePlay pauses a playing element', () => {
    const { media, view } = setup();
    act(() => view.result.current.togglePlay()); // play
    act(() => view.result.current.togglePlay()); // pause
    expect(media.pause).toHaveBeenCalled();
    expect(view.result.current.playing).toBe(false);
  });

  it('reflects timeupdate into currentTime', () => {
    const { media, view } = setup();
    act(() => {
      media.currentTime = 12;
      media.dispatchEvent(new Event('timeupdate'));
    });
    expect(view.result.current.currentTime).toBe(12);
  });

  it('marks ready and stores duration on loadedmetadata', () => {
    const { media, view } = setup();
    act(() => {
      media.duration = 30;
      media.dispatchEvent(new Event('loadedmetadata'));
    });
    expect(view.result.current.duration).toBe(30);
    expect(view.result.current.ready).toBe(true);
  });

  it('reflects duration via the durationchange event', () => {
    const { media, view } = setup();
    act(() => {
      media.duration = 42;
      media.dispatchEvent(new Event('durationchange'));
    });
    expect(view.result.current.duration).toBe(42);
  });

  it('initializes ready when the element already has finite duration', () => {
    const { view } = setup({ duration: 50 });
    expect(view.result.current.ready).toBe(true);
    expect(view.result.current.duration).toBe(50);
  });

  it('sets playing false on ended', () => {
    const { media, view } = setup();
    act(() => view.result.current.togglePlay());
    act(() => media.dispatchEvent(new Event('ended')));
    expect(view.result.current.playing).toBe(false);
  });

  it('reflects external volume/mute changes via volumechange', () => {
    const { media, view } = setup();
    act(() => {
      media.volume = 0.3;
      media.muted = true;
      media.dispatchEvent(new Event('volumechange'));
    });
    expect(view.result.current.volume).toBe(0.3);
    expect(view.result.current.muted).toBe(true);
  });

  it('seek sets currentTime on the element', () => {
    const { media, view } = setup();
    act(() => view.result.current.seek(7));
    expect(media.currentTime).toBe(7);
    expect(view.result.current.currentTime).toBe(7);
  });

  it('nudge advances within duration bounds', () => {
    const { media, view } = setup({ duration: 30, currentTime: 10 });
    act(() => view.result.current.nudge(5));
    expect(media.currentTime).toBe(15);
  });

  it('nudge clamps to 0 and to duration', () => {
    const { media, view } = setup({ duration: 30, currentTime: 5 });
    act(() => view.result.current.nudge(-20));
    expect(media.currentTime).toBe(0);
    act(() => view.result.current.nudge(100));
    expect(media.currentTime).toBe(30);
  });

  it('nudge uses currentTime+delta as cap when duration is unknown', () => {
    const { media, view } = setup({ duration: NaN, currentTime: 8 });
    act(() => view.result.current.nudge(4));
    expect(media.currentTime).toBe(12);
  });

  it('setVolume clamps to [0,1] and unmutes when raised', () => {
    const { media, view } = setup({ muted: true });
    act(() => view.result.current.setVolume(0.4));
    expect(media.volume).toBe(0.4);
    expect(media.muted).toBe(false);
  });

  it('setVolume clamps above 1 down to 1', () => {
    const { media, view } = setup();
    act(() => view.result.current.setVolume(5));
    expect(media.volume).toBe(1);
  });

  it('toggleMute flips muted', () => {
    const { media, view } = setup();
    act(() => view.result.current.toggleMute());
    expect(media.muted).toBe(true);
  });

  it('togglePlay swallows a rejected play() promise', () => {
    const { media, view } = setup();
    media.play = vi.fn(() => Promise.reject(new Error('not allowed')));
    expect(() => act(() => view.result.current.togglePlay())).not.toThrow();
  });

  it('requestFullscreen requests it on the container when not fullscreen', () => {
    const media = new FakeMedia();
    const container = document.createElement('div');
    const fsSpy = vi.fn(() => Promise.resolve());
    (container as unknown as { requestFullscreen: () => Promise<void> }).requestFullscreen = fsSpy;
    const { result } = renderHook(() =>
      useMediaPlayer({ current: media as unknown as HTMLMediaElement }, { current: container })
    );
    act(() => result.current.requestFullscreen());
    expect(fsSpy).toHaveBeenCalled();
  });

  it('requestFullscreen exits when already fullscreen', () => {
    const media = new FakeMedia();
    const container = document.createElement('div');
    const exitSpy = vi.fn(() => Promise.resolve());
    Object.defineProperty(document, 'fullscreenElement', { value: container, configurable: true });
    (document as unknown as { exitFullscreen: () => Promise<void> }).exitFullscreen = exitSpy;
    const { result } = renderHook(() =>
      useMediaPlayer({ current: media as unknown as HTMLMediaElement }, { current: container })
    );
    act(() => result.current.requestFullscreen());
    expect(exitSpy).toHaveBeenCalled();
    Object.defineProperty(document, 'fullscreenElement', { value: null, configurable: true });
  });

  it('togglePiP requests PiP on a video element', () => {
    const video = document.createElement('video');
    const pipSpy = vi.fn(() => Promise.resolve({} as PictureInPictureWindow));
    (
      video as unknown as { requestPictureInPicture: () => Promise<PictureInPictureWindow> }
    ).requestPictureInPicture = pipSpy;
    Object.defineProperty(document, 'pictureInPictureEnabled', { value: true, configurable: true });
    Object.defineProperty(document, 'pictureInPictureElement', { value: null, configurable: true });
    const { result } = renderHook(() => useMediaPlayer({ current: video }, makeContainer()));
    act(() => result.current.togglePiP());
    expect(pipSpy).toHaveBeenCalled();
  });

  it('togglePiP is a no-op for non-video (audio) elements', () => {
    const audio = document.createElement('audio');
    Object.defineProperty(document, 'pictureInPictureEnabled', { value: true, configurable: true });
    const { result } = renderHook(() => useMediaPlayer({ current: audio }, makeContainer()));
    expect(() => act(() => result.current.togglePiP())).not.toThrow();
  });

  it('removes listeners on unmount', () => {
    const { media, view } = setup();
    const spy = vi.spyOn(media, 'removeEventListener');
    view.unmount();
    expect(spy).toHaveBeenCalled();
  });
});
