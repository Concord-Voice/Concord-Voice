import { describe, it, expect, vi, beforeEach, afterEach } from 'vitest';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import ThemedMediaPlayer from '@/renderer/components/Chat/ThemedMediaPlayer';

/** Player-level keyboard shortcuts fire while focus is on a control inside the
 *  player (the container itself is not a tab stop — it has role="group", no
 *  tabIndex). Focusing the Play button lets the keydown bubble to the
 *  container's listener for the arrow / m / f shortcuts. */
function focusPlayButton() {
  screen.getByRole('button', { name: 'Play' }).focus();
}

describe('ThemedMediaPlayer', () => {
  beforeEach(() => {
    // jsdom does not implement these — stub so the hook's calls don't throw.
    vi.spyOn(HTMLMediaElement.prototype, 'play').mockImplementation(function (
      this: HTMLMediaElement
    ) {
      this.dispatchEvent(new Event('play'));
      return Promise.resolve();
    });
    vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(function (
      this: HTMLMediaElement
    ) {
      this.dispatchEvent(new Event('pause'));
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders a video element WITHOUT native controls for video variant', () => {
    const { container } = render(
      <ThemedMediaPlayer src="blob:x" variant="video" className="attachment-video" />
    );
    const video = container.querySelector('video');
    expect(video).toBeInTheDocument();
    expect(video?.hasAttribute('controls')).toBe(false);
  });

  it('renders an audio element for audio variant', () => {
    const { container } = render(
      <ThemedMediaPlayer src="blob:x" variant="audio" className="attachment-audio" />
    );
    expect(container.querySelector('audio')).toBeInTheDocument();
    expect(container.querySelector('video')).toBeNull();
  });

  it('uses a plain non-interactive container (no role, no tabIndex)', () => {
    const { container } = render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    const player = container.querySelector('.themed-media-player') as HTMLElement;
    // Deliberately no role/tabIndex: keyboard shortcuts are a progressive
    // enhancement on the already-labelled controls, and a group/region role
    // would clutter landmark navigation for inline media in a message feed.
    expect(player.hasAttribute('role')).toBe(false);
    expect(player.hasAttribute('tabindex')).toBe(false);
  });

  it('renders the themed controls bar with a Play button', () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    expect(screen.getByRole('button', { name: 'Play' })).toBeInTheDocument();
  });

  it('applies the sizing className to the native element', () => {
    const { container } = render(
      <ThemedMediaPlayer src="blob:x" variant="video" className="attachment-video" />
    );
    expect(container.querySelector('video')?.classList.contains('attachment-video')).toBe(true);
  });

  it('Space (via a focused control) toggles play', async () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    focusPlayButton();
    await userEvent.keyboard(' ');
    expect(HTMLMediaElement.prototype.play).toHaveBeenCalled();
  });

  it('Space on a focused range slider does NOT toggle play (defers to the control)', async () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    screen.getByRole('slider', { name: 'Seek' }).focus();
    await userEvent.keyboard(' ');
    expect(HTMLMediaElement.prototype.play).not.toHaveBeenCalled();
  });

  it('ArrowRight nudges the current time forward', async () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    focusPlayButton();
    await userEvent.keyboard('{ArrowRight}');
    // duration is NaN in jsdom, so nudge caps at currentTime+delta = 0+5 = 5 → "0:05".
    expect(await screen.findByText('0:05')).toBeInTheDocument();
  });

  it('ArrowLeft nudges the current time backward (clamped at 0)', async () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    focusPlayButton();
    await userEvent.keyboard('{ArrowRight}'); // → 0:05
    expect(await screen.findByText('0:05')).toBeInTheDocument();
    await userEvent.keyboard('{ArrowLeft}'); // 5 - 5 = 0 → back to 0:00
    expect(await screen.findAllByText('0:00')).not.toHaveLength(0);
  });

  it('ArrowDown lowers the volume', async () => {
    const { container } = render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    const video = container.querySelector('video') as HTMLVideoElement;
    focusPlayButton();
    await userEvent.keyboard('{ArrowDown}');
    expect(video.volume).toBeCloseTo(0.9, 5);
  });

  it('ArrowUp raises the volume (clamped at 1)', async () => {
    const { container } = render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    const video = container.querySelector('video') as HTMLVideoElement;
    focusPlayButton();
    await userEvent.keyboard('{ArrowDown}'); // 1 → 0.9
    await userEvent.keyboard('{ArrowUp}'); // 0.9 → 1.0
    expect(video.volume).toBeCloseTo(1, 5);
  });

  it('ignores an unhandled key without throwing', async () => {
    render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    focusPlayButton();
    await expect(userEvent.keyboard('x')).resolves.not.toThrow();
  });

  it('the m key mutes the element', async () => {
    const { container } = render(<ThemedMediaPlayer src="blob:x" variant="video" />);
    const video = container.querySelector('video') as HTMLVideoElement;
    focusPlayButton();
    await userEvent.keyboard('m');
    expect(video.muted).toBe(true);
  });

  it('the f key requests fullscreen for video', async () => {
    const fsSpy = vi.fn(() => Promise.resolve());
    (
      HTMLElement.prototype as unknown as { requestFullscreen: () => Promise<void> }
    ).requestFullscreen = fsSpy;
    try {
      render(<ThemedMediaPlayer src="blob:x" variant="video" />);
      focusPlayButton();
      await userEvent.keyboard('f');
      expect(fsSpy).toHaveBeenCalled();
    } finally {
      delete (HTMLElement.prototype as unknown as { requestFullscreen?: unknown })
        .requestFullscreen;
    }
  });

  it('the f key does NOT request fullscreen for audio', async () => {
    const fsSpy = vi.fn(() => Promise.resolve());
    (
      HTMLElement.prototype as unknown as { requestFullscreen: () => Promise<void> }
    ).requestFullscreen = fsSpy;
    try {
      render(<ThemedMediaPlayer src="blob:x" variant="audio" />);
      focusPlayButton();
      await userEvent.keyboard('f');
      expect(fsSpy).not.toHaveBeenCalled();
    } finally {
      delete (HTMLElement.prototype as unknown as { requestFullscreen?: unknown })
        .requestFullscreen;
    }
  });
});
