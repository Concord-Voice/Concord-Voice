import { describe, it, expect, vi, beforeEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import MediaControlsBar from '@/renderer/components/Chat/MediaControlsBar';
import type { MediaPlayerState, MediaPlayerActions } from '@/renderer/hooks/useMediaPlayer';

function makeState(over: Partial<MediaPlayerState> = {}): MediaPlayerState {
  return {
    playing: false,
    currentTime: 0,
    duration: 60,
    volume: 1,
    muted: false,
    ready: true,
    ...over,
  };
}

function makeActions(): MediaPlayerActions {
  return {
    togglePlay: vi.fn(),
    seek: vi.fn(),
    nudge: vi.fn(),
    setVolume: vi.fn(),
    toggleMute: vi.fn(),
    requestFullscreen: vi.fn(),
    togglePiP: vi.fn(),
  };
}

describe('MediaControlsBar', () => {
  beforeEach(() => vi.clearAllMocks());

  it('renders Play label when paused', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.getByRole('button', { name: 'Play' })).toBeInTheDocument();
  });

  it('renders Pause label when playing', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState({ playing: true })}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.getByRole('button', { name: 'Pause' })).toBeInTheDocument();
  });

  it('clicking play calls togglePlay', async () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Play' }));
    expect(actions.togglePlay).toHaveBeenCalledOnce();
  });

  it('exposes scrubber and volume as sliders', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.getByRole('slider', { name: 'Seek' })).toBeInTheDocument();
    expect(screen.getByRole('slider', { name: 'Volume' })).toBeInTheDocument();
  });

  it('applies accent gradient fill to the scrubber', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState({ currentTime: 30, duration: 60 })}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    const scrubber = screen.getByRole('slider', { name: 'Seek' });
    expect(scrubber.getAttribute('style')).toContain('--accent-color');
  });

  it('disables the scrubber until ready', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState({ ready: false })}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.getByRole('slider', { name: 'Seek' })).toBeDisabled();
  });

  it('hides fullscreen and PiP for audio variant', () => {
    render(
      <MediaControlsBar
        variant="audio"
        state={makeState()}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.queryByRole('button', { name: 'Fullscreen' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Picture-in-Picture' })).not.toBeInTheDocument();
  });

  it('hides fullscreen when unsupported even for video', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={makeActions()}
        fullscreenSupported={false}
        pipSupported
      />
    );
    expect(screen.queryByRole('button', { name: 'Fullscreen' })).not.toBeInTheDocument();
  });

  it('shows Unmute label when muted', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState({ muted: true })}
        actions={makeActions()}
        fullscreenSupported
        pipSupported
      />
    );
    expect(screen.getByRole('button', { name: 'Unmute' })).toBeInTheDocument();
  });

  it('changing the scrubber calls seek with the new value', () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState({ duration: 60 })}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    fireEvent.change(screen.getByRole('slider', { name: 'Seek' }), { target: { value: '15' } });
    expect(actions.seek).toHaveBeenCalledWith(15);
  });

  it('changing the volume slider calls setVolume', () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    fireEvent.change(screen.getByRole('slider', { name: 'Volume' }), { target: { value: '0.5' } });
    expect(actions.setVolume).toHaveBeenCalledWith(0.5);
  });

  it('clicking mute calls toggleMute', async () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Mute' }));
    expect(actions.toggleMute).toHaveBeenCalledOnce();
  });

  it('clicking the PiP button calls togglePiP', async () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Picture-in-Picture' }));
    expect(actions.togglePiP).toHaveBeenCalledOnce();
  });

  it('clicking the fullscreen button calls requestFullscreen', async () => {
    const actions = makeActions();
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={actions}
        fullscreenSupported
        pipSupported
      />
    );
    await userEvent.click(screen.getByRole('button', { name: 'Fullscreen' }));
    expect(actions.requestFullscreen).toHaveBeenCalledOnce();
  });

  it('hides only PiP when PiP is unsupported but fullscreen is', () => {
    render(
      <MediaControlsBar
        variant="video"
        state={makeState()}
        actions={makeActions()}
        fullscreenSupported
        pipSupported={false}
      />
    );
    expect(screen.queryByRole('button', { name: 'Picture-in-Picture' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Fullscreen' })).toBeInTheDocument();
  });
});
