import { render, screen, fireEvent, act, waitFor } from '../../../test-utils';
import GifEmbed from '@/renderer/components/Chat/GifEmbed';
import { resetAllStores } from '../../../helpers/store-helpers';
import { __resetWindowFocusForTests } from '@/renderer/hooks/ui/useWindowFocus';

// Capture IntersectionObserver callbacks so we can trigger visibility manually
let observerCallback: (entries: { isIntersecting: boolean }[]) => void;

beforeAll(() => {
  // Override the setup.ts mock with one that captures the callback (writable: true in setup)
  (globalThis as Record<string, unknown>).IntersectionObserver = class {
    constructor(callback: (entries: { isIntersecting: boolean }[]) => void) {
      observerCallback = callback;
    }
    observe = vi.fn();
    disconnect = vi.fn();
    unobserve = vi.fn();
  };
});

// Mock the gifProvider so we don't make any network calls.
const mockResolvedVideo = {
  slug: 'mp4-test-slug',
  width: 480,
  height: 270,
  animatedUrl: 'https://media.klipy.com/abc.mp4',
  animatedKind: 'video' as const,
  stillUrl: 'https://media.klipy.com/abc.jpg',
};

const mockResolvedImage = {
  slug: 'gif-test-slug',
  width: 200,
  height: 200,
  animatedUrl: 'https://media.klipy.com/abc.gif',
  animatedKind: 'image' as const,
  stillUrl: 'https://media.klipy.com/abc.jpg',
};

const getBySlugMock = vi.fn();
vi.mock('@/renderer/services/messaging/gifProvider', () => ({
  gifProvider: {
    name: 'KLIPY',
    searchPlaceholder: 'Search KLIPY',
    poweredByText: 'Powered by KLIPY',
    logoAssetLight: './branding/KLIPY/klipy-logo-light.svg',
    logoAssetDark: './branding/KLIPY/klipy-logo-dark.svg',
    independenceDisclaimer: 'Independent disclaimer',
    supportsRecent: true,
    supportsCategories: true,
    trending: vi.fn(),
    search: vi.fn(),
    recent: vi.fn(),
    categories: vi.fn(),
    getBySlug: (slug: string) => getBySlugMock(slug),
    notifyShared: vi.fn(),
    report: vi.fn(),
    setPersonalizationEnabled: vi.fn(),
  },
}));

function triggerVisible() {
  act(() => {
    observerCallback([{ isIntersecting: true }]);
  });
}

describe('GifEmbed', () => {
  beforeEach(() => {
    resetAllStores();
    getBySlugMock.mockReset();
  });

  it('shows click-to-load placeholder when loadAutomatically is false', () => {
    render(<GifEmbed slug="abc" mode="auto" reduceAnimations={false} loadAutomatically={false} />);
    expect(screen.getByText('Click to load GIF')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Click to load GIF' })).toBeInTheDocument();
  });

  it('does not call gifProvider.getBySlug while click-to-load placeholder is shown', () => {
    render(<GifEmbed slug="abc" mode="auto" reduceAnimations={false} loadAutomatically={false} />);
    triggerVisible();
    expect(getBySlugMock).not.toHaveBeenCalled();
  });

  it('fetches via gifProvider when user clicks the placeholder', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    render(<GifEmbed slug="abc" mode="auto" reduceAnimations={false} loadAutomatically={false} />);
    fireEvent.click(screen.getByText('Click to load GIF'));
    triggerVisible();
    await waitFor(() => expect(getBySlugMock).toHaveBeenCalledWith('abc'));
  });

  it('renders <video> for MP4 renditions when loadAutomatically and not reduceMotion', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed
        slug="mp4-test-slug"
        mode="auto"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    await waitFor(() => {
      const video = container.querySelector('video');
      expect(video).not.toBeNull();
      expect(video?.getAttribute('src')).toBe('https://media.klipy.com/abc.mp4');
      expect(video?.getAttribute('poster')).toBe('https://media.klipy.com/abc.jpg');
    });
  });

  it('renders <img> for GIF/WEBP renditions', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);
    const { container } = render(
      <GifEmbed
        slug="gif-test-slug"
        mode="auto"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    await waitFor(() => {
      const img = container.querySelector('img');
      expect(img).not.toBeNull();
      expect(img?.getAttribute('src')).toBe('https://media.klipy.com/abc.gif');
    });
  });

  it('renders the still rendition when reduceMotion is true', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed slug="mp4-test-slug" mode="auto" reduceAnimations={true} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const img = container.querySelector('img');
      expect(img).not.toBeNull();
      expect(img?.getAttribute('src')).toBe('https://media.klipy.com/abc.jpg');
      // No video element when reduce-motion is on
      expect(container.querySelector('video')).toBeNull();
    });
  });

  it('renders error state when gifProvider rejects', async () => {
    getBySlugMock.mockRejectedValue(new Error('not found'));
    render(
      <GifEmbed slug="missing" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      expect(screen.getByText('GIF unavailable')).toBeInTheDocument();
    });
  });

  it('shows the Powered by KLIPY attribution when a GIF is rendered', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    render(
      <GifEmbed
        slug="mp4-test-slug"
        mode="auto"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    await waitFor(() => {
      expect(screen.getByText('Powered by KLIPY')).toBeInTheDocument();
    });
  });

  it('the rendered <video> has accessible aria-label', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed
        slug="mp4-test-slug"
        mode="auto"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    await waitFor(() => {
      const video = container.querySelector('video');
      expect(video?.getAttribute('aria-label')).toBe('GIF from KLIPY');
    });
  });

  // ---------- Layout-shift fixes (bug #1: vertical expand on send) ----------

  it('clamps oversized GIFs into the 400×300 display box on the container', async () => {
    // 1600×900 → max-width 400 with 16:9 aspect → 400×225
    getBySlugMock.mockResolvedValue({
      ...mockResolvedVideo,
      width: 1600,
      height: 900,
    });
    const { container } = render(
      <GifEmbed slug="big" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const embed = container.querySelector('.gif-embed') as HTMLElement;
      expect(embed.style.width).toBe('400px');
      expect(embed.style.height).toBe('225px');
    });
  });

  it('clamps tall GIFs by max-height while preserving aspect ratio', async () => {
    // 600×1200 → max-height 300 with 1:2 aspect → 150×300
    getBySlugMock.mockResolvedValue({
      ...mockResolvedImage,
      width: 600,
      height: 1200,
    });
    const { container } = render(
      <GifEmbed slug="tall" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const embed = container.querySelector('.gif-embed') as HTMLElement;
      expect(embed.style.width).toBe('150px');
      expect(embed.style.height).toBe('300px');
    });
  });

  it('does not upscale small GIFs beyond their natural size', async () => {
    getBySlugMock.mockResolvedValue({
      ...mockResolvedImage,
      width: 120,
      height: 80,
    });
    const { container } = render(
      <GifEmbed slug="small" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const embed = container.querySelector('.gif-embed') as HTMLElement;
      expect(embed.style.width).toBe('120px');
      expect(embed.style.height).toBe('80px');
    });
  });

  it('passes the clamped width/height to the rendered <video> element', async () => {
    getBySlugMock.mockResolvedValue({
      ...mockResolvedVideo,
      width: 1600,
      height: 900,
    });
    const { container } = render(
      <GifEmbed slug="big-vid" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const video = container.querySelector('video');
      expect(video?.getAttribute('width')).toBe('400');
      expect(video?.getAttribute('height')).toBe('225');
    });
  });

  it('does not lock the container size while showing the click-to-load placeholder', () => {
    const { container } = render(
      <GifEmbed slug="lazy" mode="auto" reduceAnimations={false} loadAutomatically={false} />
    );
    const embed = container.querySelector('.gif-embed') as HTMLElement;
    // No inline width/height — placeholder is intrinsically sized so it
    // doesn't get stretched into a 250×180 box.
    expect(embed.style.width).toBe('');
    expect(embed.style.height).toBe('');
  });

  // ---------- Reduce Animations hover-to-play (#571 item #6B) ----------

  it('reduce-motion: shows the still frame by default (no video)', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed slug="mp4-test-slug" mode="auto" reduceAnimations={true} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      const img = container.querySelector('img');
      expect(img?.getAttribute('src')).toBe('https://media.klipy.com/abc.jpg');
    });
    expect(container.querySelector('video')).toBeNull();
  });

  it('reduce-motion: mouseenter swaps to the animated video, mouseleave reverts', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed slug="mp4-test-slug" mode="auto" reduceAnimations={true} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(
        'https://media.klipy.com/abc.jpg'
      )
    );
    const embed = container.querySelector('.gif-embed') as HTMLElement;
    fireEvent.mouseEnter(embed);
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());
    fireEvent.mouseLeave(embed);
    await waitFor(() => expect(container.querySelector('video')).toBeNull());
  });

  it('reduce-motion: focus + blur also swap between still and animation', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);
    const { container } = render(
      <GifEmbed slug="gif-test-slug" mode="auto" reduceAnimations={true} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(
        'https://media.klipy.com/abc.jpg'
      )
    );
    const embed = container.querySelector('.gif-embed') as HTMLElement;
    fireEvent.focus(embed);
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(
        'https://media.klipy.com/abc.gif'
      )
    );
    fireEvent.blur(embed);
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(
        'https://media.klipy.com/abc.jpg'
      )
    );
  });

  it('motion allowed: autoplays without needing hover', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed
        slug="mp4-test-slug"
        mode="auto"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    await waitFor(() => expect(container.querySelector('video')).not.toBeNull());
    // No tabIndex — we don't put reduce-motion focus handlers on the embed
    const embed = container.querySelector('.gif-embed') as HTMLElement;
    expect(embed.getAttribute('tabindex')).toBeNull();
  });

  it('does not lock the container size in the error state', async () => {
    getBySlugMock.mockRejectedValue(new Error('boom'));
    const { container } = render(
      <GifEmbed slug="bad" mode="auto" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => {
      expect(screen.getByText('GIF unavailable')).toBeInTheDocument();
    });
    const embed = container.querySelector('.gif-embed') as HTMLElement;
    expect(embed.style.width).toBe('');
    expect(embed.style.height).toBe('');
  });
});

// ---------------------------------------------------------------------------
// #3291 Codex review fix (fb529eb5d) — mount-after-blur ordering.
//
// The pause effect's deps were `[verdict.playing]`; the <video> mounts only
// when `gifProvider.getBySlug()` resolves. If the window blurs WHILE that
// promise is pending, `playing` is false for the whole resolution, so an
// effect keyed only on it never re-runs against the newly-mounted element,
// and `<video autoPlay>` animated in the background. Fixed by adding
// `resolved` to the deps AND making `autoPlay={verdict.playing}`.
// ---------------------------------------------------------------------------

// Codex review, PR #3291. Two independent findings on the hover wrapper.
describe('GifEmbed — hover wrapper semantics (#3291)', () => {
  beforeEach(() => {
    resetAllStores();
    getBySlugMock.mockReset();
  });

  it('keyboard focus keeps it playing when the pointer leaves', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);
    const { container } = render(
      <GifEmbed slug="gif-test-slug" mode="hover" reduceAnimations={false} loadAutomatically />
    );
    triggerVisible();

    const img = await waitFor(() => {
      const el = container.querySelector('img') as HTMLImageElement;
      expect(el).not.toBeNull();
      return el;
    });
    // Hover-gated and untouched: the still.
    expect(img.getAttribute('src')).toBe(mockResolvedImage.stillUrl);

    const embed = container.querySelector('.gif-embed') as HTMLElement;
    fireEvent.focus(embed);
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(
        mockResolvedImage.animatedUrl
      )
    );

    // THE CASE: pointer leaves while the keyboard still holds focus. One shared
    // boolean would stop playback here; two independent ones keep it running.
    fireEvent.mouseLeave(embed);
    expect(container.querySelector('img')?.getAttribute('src')).toBe(mockResolvedImage.animatedUrl);

    // And blurring with the pointer away does stop it — the other direction.
    fireEvent.blur(embed);
    await waitFor(() =>
      expect(container.querySelector('img')?.getAttribute('src')).toBe(mockResolvedImage.stillUrl)
    );
  });

  it('an embed with nothing loaded yet is not a tab stop; a loaded one is', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);
    const { container: unloaded } = render(
      <GifEmbed
        slug="gif-test-slug"
        mode="hover"
        reduceAnimations={false}
        loadAutomatically={false}
      />
    );
    triggerVisible();
    // The "Click to load GIF" button is already focusable, so the wrapper
    // must not add a second stop on a non-actionable div.
    await waitFor(() => expect(unloaded.querySelector('.gif-embed-placeholder')).not.toBeNull());
    expect((unloaded.querySelector('.gif-embed') as HTMLElement).hasAttribute('tabindex')).toBe(
      false
    );

    const { container: loaded } = render(
      <GifEmbed slug="gif-test-slug" mode="hover" reduceAnimations={false} loadAutomatically />
    );
    triggerVisible();
    await waitFor(() => expect(loaded.querySelector('img')).not.toBeNull());
    expect((loaded.querySelector('.gif-embed') as HTMLElement).getAttribute('tabindex')).toBe('0');
  });
});

// Gitar review on PR #3291 asked for the still-frame <img>'s `onError` to be
// exercised at all; Codex then asked the sharper question of WHAT it should do.
// The first answer routed it to the terminal error state, which `renderGifBody`
// checks before every playback branch — so a still that 404s made "GIF
// unavailable" permanent for an embed whose ANIMATION was fine, and the
// blur/refocus resume cycle could never recover. The still is the PAUSE surface
// only. Failing it retires the still alone and falls back to the same stopped
// box an aliased still already uses.
describe('GifEmbed — a failed STILL is recoverable, a failed ANIMATION is not (#3291)', () => {
  const originalHasFocus = document.hasFocus;

  beforeEach(() => {
    resetAllStores();
    getBySlugMock.mockReset();
    __resetWindowFocusForTests();
    document.hasFocus = () => true;
  });

  afterEach(() => {
    __resetWindowFocusForTests();
    document.hasFocus = originalHasFocus;
  });

  function blurWindow() {
    document.hasFocus = () => false;
    fireEvent(window, new Event('blur'));
  }

  function focusWindow() {
    document.hasFocus = () => true;
    fireEvent(window, new Event('focus'));
  }

  async function loadedImg(container: HTMLElement) {
    return waitFor(() => {
      const el = container.querySelector('img') as HTMLImageElement;
      expect(el).not.toBeNull();
      return el;
    });
  }

  it('hover gate: a failed still shows the stopped box, and hovering still plays', async () => {
    // Distinct still, so the hover-gated branch renders an <img> rather than
    // the no-still placeholder — this test is about the <img>'s error path.
    getBySlugMock.mockResolvedValue(mockResolvedImage);

    const { container } = render(
      <GifEmbed
        slug="gif-test-slug"
        mode="hover"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();

    const still = await loadedImg(container);
    expect(still.getAttribute('src')).toBe(mockResolvedImage.stillUrl);

    fireEvent.error(still);

    // Not a broken-image box (the original finding) and not "GIF unavailable"
    // either (the original remedy, which was terminal).
    await waitFor(() => expect(container.querySelector('.gif-embed-stopped')).not.toBeNull());
    expect(container.querySelector('img')).toBeNull();
    expect(screen.queryByText('GIF unavailable')).toBeNull();

    // The animation was never the thing that failed, so hover still reaches it.
    fireEvent.mouseEnter(container.querySelector('.gif-embed') as HTMLElement);
    const animated = await loadedImg(container);
    expect(animated.getAttribute('src')).toBe(mockResolvedImage.animatedUrl);
  });

  it('THE LOAD-BEARING CASE: a still that fails on blur still resumes on refocus', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);

    const { container } = render(
      <GifEmbed
        slug="gif-test-slug"
        mode="always"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    const playing = await loadedImg(container);
    expect(playing.getAttribute('src')).toBe(mockResolvedImage.animatedUrl);

    // Blur swaps this ONE element's src to the still, which then fails to load.
    blurWindow();
    await waitFor(() =>
      expect((container.querySelector('img') as HTMLImageElement)?.getAttribute('src')).toBe(
        mockResolvedImage.stillUrl
      )
    );
    fireEvent.error(container.querySelector('img') as HTMLImageElement);
    await waitFor(() => expect(container.querySelector('.gif-embed-stopped')).not.toBeNull());

    focusWindow();

    const resumed = await loadedImg(container);
    expect(resumed.getAttribute('src')).toBe(mockResolvedImage.animatedUrl);
    expect(screen.queryByText('GIF unavailable')).toBeNull();
  });

  // The control. Without it, making EVERY image error recoverable would pass
  // both cases above — the point is that the two sources are distinguished,
  // not that failures stopped mattering.
  it('an ANIMATED source that fails is still terminal', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);

    const { container } = render(
      <GifEmbed
        slug="gif-test-slug"
        mode="always"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();
    const playing = await loadedImg(container);
    expect(playing.getAttribute('src')).toBe(mockResolvedImage.animatedUrl);

    fireEvent.error(playing);

    expect(await screen.findByText('GIF unavailable')).toBeInTheDocument();
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('.gif-embed-stopped')).toBeNull();
  });
});

describe('GifEmbed — video mounted while already unfocused (#3291 fix)', () => {
  const originalHasFocus = document.hasFocus;

  beforeEach(() => {
    resetAllStores();
    getBySlugMock.mockReset();
    __resetWindowFocusForTests();
    document.hasFocus = () => true;
  });

  afterEach(() => {
    __resetWindowFocusForTests();
    document.hasFocus = originalHasFocus;
  });

  function blurWindow() {
    document.hasFocus = () => false;
    fireEvent(window, new Event('blur'));
  }

  function focusWindow() {
    document.hasFocus = () => true;
    fireEvent(window, new Event('focus'));
  }

  // Same aliasing defect on the embed: `toResolved` falls through
  // `mp4 ?? webp ?? gif` when `item.still` is absent, so stillUrl === animatedUrl
  // and the blur-time swap stops nothing. Codex review, PR #3291.
  it('an img-kind embed whose still IS its animated url unmounts on blur rather than swapping to itself', async () => {
    const aliased = {
      ...mockResolvedImage,
      stillUrl: mockResolvedImage.animatedUrl,
    };
    getBySlugMock.mockResolvedValue(aliased);

    const { container } = render(
      <GifEmbed slug="aliased" mode="always" reduceAnimations={false} loadAutomatically={true} />
    );
    triggerVisible();
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());

    blurWindow();

    await waitFor(() => expect(container.querySelector('.gif-embed-stopped')).not.toBeNull());
    expect(container.querySelector('img')).toBeNull();

    focusWindow();
    await waitFor(() => expect(container.querySelector('img')).not.toBeNull());
    expect(container.querySelector('.gif-embed-stopped')).toBeNull();
  });

  it('a video that mounts while the window is already unfocused does not play', async () => {
    let resolveGetBySlug!: (v: typeof mockResolvedVideo) => void;
    getBySlugMock.mockReturnValue(
      new Promise((resolve) => {
        resolveGetBySlug = resolve;
      })
    );
    const playSpy = vi
      .spyOn(HTMLMediaElement.prototype, 'play')
      .mockImplementation(() => Promise.resolve());
    const pauseSpy = vi.spyOn(HTMLMediaElement.prototype, 'pause').mockImplementation(() => {});

    const { container } = render(
      <GifEmbed
        slug="mp4-test-slug"
        mode="always"
        reduceAnimations={false}
        loadAutomatically={true}
      />
    );
    triggerVisible();

    // getBySlug is still in flight — blur BEFORE it resolves.
    blurWindow();

    // Now resolve while the window is still unfocused, and let the mount
    // effects settle.
    await act(async () => {
      resolveGetBySlug(mockResolvedVideo);
      await Promise.resolve();
    });

    const video = await waitFor(() => {
      const v = container.querySelector('video');
      expect(v).not.toBeNull();
      return v as HTMLVideoElement;
    });

    // The fix's second half: `autoPlay` is conditional on the verdict, not bare.
    expect(video.hasAttribute('autoplay')).toBe(false);
    // The fix's first half: the pause effect re-ran against the newly-mounted
    // element (deps include `resolved`), so `play` was never invoked for it.
    expect(playSpy).not.toHaveBeenCalled();
    expect(pauseSpy).toHaveBeenCalled();

    focusWindow();
    await waitFor(() => expect(playSpy).toHaveBeenCalled());
  });
});

// CodeRabbit review, PR #3291: hoverHandlers used to be attached only while the
// resolved gate was 'hover', so a pointer that left during an 'always' spell
// never fired onMouseLeave and `pointerOver` stayed stuck true.
describe('GifEmbed — hover state survives a gate change without going stale (#3291)', () => {
  beforeEach(() => {
    resetAllStores();
    getBySlugMock.mockReset();
  });

  it('a pointer that leaves while the gate is OFF is still noticed', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedImage);

    const { container, rerender } = render(
      <GifEmbed slug="gif-test-slug" mode="hover" reduceAnimations={false} loadAutomatically />
    );
    triggerVisible();
    const stillSrc = () =>
      (container.querySelector('img') as HTMLImageElement | null)?.getAttribute('src');
    await waitFor(() => expect(stillSrc()).toBe(mockResolvedImage.stillUrl));

    const embed = container.querySelector('.gif-embed') as HTMLElement;
    fireEvent.mouseEnter(embed);
    await waitFor(() =>
      expect((container.querySelector('img') as HTMLImageElement)?.getAttribute('src')).toBe(
        mockResolvedImage.animatedUrl
      )
    );

    // Gate off, pointer leaves during the gap, gate back on.
    rerender(
      <GifEmbed slug="gif-test-slug" mode="always" reduceAnimations={false} loadAutomatically />
    );
    fireEvent.mouseLeave(embed);
    rerender(
      <GifEmbed slug="gif-test-slug" mode="hover" reduceAnimations={false} loadAutomatically />
    );

    await waitFor(() => expect(stillSrc()).toBe(mockResolvedImage.stillUrl));
  });
});
