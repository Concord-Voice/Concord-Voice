import { render, screen, fireEvent, act, waitFor } from '../../../test-utils';
import GifEmbed from '@/renderer/components/Chat/GifEmbed';
import { resetAllStores } from '../../../helpers/store-helpers';

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
vi.mock('@/renderer/services/gifProvider', () => ({
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
    render(<GifEmbed slug="abc" reduceMotion={false} loadAutomatically={false} />);
    expect(screen.getByText('Click to load GIF')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Click to load GIF' })).toBeInTheDocument();
  });

  it('does not call gifProvider.getBySlug while click-to-load placeholder is shown', () => {
    render(<GifEmbed slug="abc" reduceMotion={false} loadAutomatically={false} />);
    triggerVisible();
    expect(getBySlugMock).not.toHaveBeenCalled();
  });

  it('fetches via gifProvider when user clicks the placeholder', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    render(<GifEmbed slug="abc" reduceMotion={false} loadAutomatically={false} />);
    fireEvent.click(screen.getByText('Click to load GIF'));
    triggerVisible();
    await waitFor(() => expect(getBySlugMock).toHaveBeenCalledWith('abc'));
  });

  it('renders <video> for MP4 renditions when loadAutomatically and not reduceMotion', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed slug="mp4-test-slug" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="gif-test-slug" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="mp4-test-slug" reduceMotion={true} loadAutomatically={true} />
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
    render(<GifEmbed slug="missing" reduceMotion={false} loadAutomatically={true} />);
    triggerVisible();
    await waitFor(() => {
      expect(screen.getByText('GIF unavailable')).toBeInTheDocument();
    });
  });

  it('shows the Powered by KLIPY attribution when a GIF is rendered', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    render(<GifEmbed slug="mp4-test-slug" reduceMotion={false} loadAutomatically={true} />);
    triggerVisible();
    await waitFor(() => {
      expect(screen.getByText('Powered by KLIPY')).toBeInTheDocument();
    });
  });

  it('the rendered <video> has accessible aria-label', async () => {
    getBySlugMock.mockResolvedValue(mockResolvedVideo);
    const { container } = render(
      <GifEmbed slug="mp4-test-slug" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="big" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="tall" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="small" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="big-vid" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="lazy" reduceMotion={false} loadAutomatically={false} />
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
      <GifEmbed slug="mp4-test-slug" reduceMotion={true} loadAutomatically={true} />
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
      <GifEmbed slug="mp4-test-slug" reduceMotion={true} loadAutomatically={true} />
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
      <GifEmbed slug="gif-test-slug" reduceMotion={true} loadAutomatically={true} />
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
      <GifEmbed slug="mp4-test-slug" reduceMotion={false} loadAutomatically={true} />
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
      <GifEmbed slug="bad" reduceMotion={false} loadAutomatically={true} />
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
