import { render, screen, fireEvent, waitFor } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useSavedGifsStore } from '@/renderer/stores/savedGifsStore';
import { useSettingsStore } from '@/renderer/stores/settingsStore';
import { usePrivacyStore } from '@/renderer/stores/privacyStore';

// Mock the gifProvider entirely. The picker no longer talks to a vendor SDK
// directly — it goes through the abstract gifProvider singleton, which we
// fully control here.
const trendingMock = vi.fn();
const searchMock = vi.fn();
const recentMock = vi.fn();
const categoriesMock = vi.fn();
const getBySlugMock = vi.fn();
const notifySharedMock = vi.fn();

const sampleVideoGif = {
  slug: 'video-1',
  width: 300,
  height: 200,
  animatedUrl: 'https://media.klipy.com/v1.mp4',
  animatedKind: 'video' as const,
  stillUrl: 'https://media.klipy.com/v1.jpg',
};

const sampleImageGif = {
  slug: 'gif-1',
  width: 200,
  height: 200,
  animatedUrl: 'https://media.klipy.com/g1.gif',
  animatedKind: 'image' as const,
  stillUrl: 'https://media.klipy.com/g1.jpg',
};

vi.mock('@/renderer/services/gifProvider', () => ({
  gifProvider: {
    name: 'KLIPY',
    searchPlaceholder: 'Search KLIPY',
    poweredByText: 'Powered by KLIPY',
    logoAssetLight: './branding/KLIPY/klipy-logo-light.svg',
    logoAssetDark: './branding/KLIPY/klipy-logo-dark.svg',
    independenceDisclaimer:
      'Concord is independently developed and not affiliated with or endorsed by KLIPY.',
    supportsRecent: true,
    supportsCategories: true,
    trending: (opts: unknown) => trendingMock(opts),
    search: (opts: unknown) => searchMock(opts),
    recent: (opts: unknown) => recentMock(opts),
    categories: (opts: unknown) => categoriesMock(opts),
    getBySlug: (slug: string) => getBySlugMock(slug),
    notifyShared: (slug: string) => notifySharedMock(slug),
    setPersonalizationEnabled: vi.fn(),
  },
}));

import GifPicker from '@/renderer/components/GifPicker/GifPicker';

describe('GifPicker', () => {
  const onSelect = vi.fn();
  const onClose = vi.fn();
  const position = { x: 100, y: 200, anchorCenterX: 150 };

  beforeEach(() => {
    resetAllStores();
    onSelect.mockClear();
    onClose.mockClear();
    trendingMock.mockReset();
    searchMock.mockReset();
    recentMock.mockReset();
    categoriesMock.mockReset();
    getBySlugMock.mockReset();
    notifySharedMock.mockReset();
    // Enable personalization so the Recent tab is visible in these tests
    usePrivacyStore.setState((s) => ({
      settings: { ...s.settings, sharePersonalizationWithGifProvider: true },
    }));
    // Default trending returns one GIF
    trendingMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    recentMock.mockResolvedValue({ items: [], hasMore: false });
    categoriesMock.mockResolvedValue([]);
    notifySharedMock.mockResolvedValue(undefined);
  });

  it('renders all four tabs (trending, recent, categories, saved)', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    expect(screen.getByText('Trending')).toBeInTheDocument();
    expect(screen.getByText('Recent')).toBeInTheDocument();
    expect(screen.getByText('Categories')).toBeInTheDocument();
    expect(screen.getByText('Saved')).toBeInTheDocument();
    // Wait for the trending fetch to settle so React doesn't warn about unawaited state updates
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
  });

  it('renders the close button and search input with correct placeholder', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    expect(screen.getByRole('button', { name: 'Close' })).toBeInTheDocument();
    expect(screen.getByPlaceholderText('Search KLIPY')).toBeInTheDocument();
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
  });

  it('shows Powered by attribution with the provider logo', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    expect(screen.getByText('Powered by')).toBeInTheDocument();
    expect(screen.getByAltText('KLIPY')).toBeInTheDocument();
    // Independence disclaimer lives in Settings > About, not the picker footer.
    expect(screen.queryByText(/independently developed/i)).not.toBeInTheDocument();
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
  });

  it('loads trending GIFs by default', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => {
      expect(trendingMock).toHaveBeenCalledWith({ offset: 0, limit: 25 });
    });
    // The video element for the sample GIF should appear once loading finishes
    await waitFor(() => {
      expect(document.querySelector('video')).not.toBeNull();
    });
  });

  it('switches to Recent tab and calls gifProvider.recent', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() => expect(recentMock).toHaveBeenCalledWith({ offset: 0, limit: 25 }));
  });

  it('Recent tab shows empty state when no recent GIFs are available', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() => expect(screen.getByText('No recent GIFs')).toBeInTheDocument());
  });

  it('Categories tab calls gifProvider.categories', async () => {
    categoriesMock.mockResolvedValue([
      { name: 'Reactions', preview: sampleImageGif },
      { name: 'Animals', preview: sampleVideoGif },
    ]);
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Categories'));
    await waitFor(() => expect(categoriesMock).toHaveBeenCalled());
    await waitFor(() => {
      expect(screen.getByText('Reactions')).toBeInTheDocument();
      expect(screen.getByText('Animals')).toBeInTheDocument();
    });
  });

  it('clicking a category switches to search mode for that category name', async () => {
    categoriesMock.mockResolvedValue([{ name: 'Reactions', preview: sampleImageGif }]);
    searchMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Categories'));
    await waitFor(() => expect(screen.getByText('Reactions')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Reactions'));
    // Search input should now contain "Reactions" — and search should fire
    await waitFor(() => {
      expect(searchMock).toHaveBeenCalledWith(
        expect.objectContaining({ q: 'Reactions', offset: 0, limit: 25 })
      );
    });
  });

  it('Saved tab shows empty state when no GIFs are saved', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Saved'));
    await waitFor(() => expect(screen.getByText('No saved GIFs yet')).toBeInTheDocument());
  });

  it('Saved tab resolves each saved slug via gifProvider.getBySlug', async () => {
    useSavedGifsStore.getState().saveGif('saved-slug-1');
    getBySlugMock.mockResolvedValue(sampleVideoGif);
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Saved'));
    await waitFor(() => expect(getBySlugMock).toHaveBeenCalledWith('saved-slug-1'));
  });

  it('typing in the search input debounces and calls gifProvider.search', async () => {
    searchMock.mockResolvedValue({ items: [sampleImageGif], hasMore: false });
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.change(screen.getByPlaceholderText('Search KLIPY'), { target: { value: 'cat' } });
    await waitFor(
      () => {
        expect(searchMock).toHaveBeenCalledWith(
          expect.objectContaining({ q: 'cat', offset: 0, limit: 25 })
        );
      },
      { timeout: 1000 }
    );
  });

  it('clicking a GIF tile fires notifyShared, calls onSelect with the slug, and closes', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    await waitFor(() => expect(document.querySelector('.gif-tile')).not.toBeNull());
    const tile = document.querySelector('.gif-tile') as HTMLElement;
    fireEvent.click(tile);
    expect(onSelect).toHaveBeenCalledWith('video-1');
    expect(onClose).toHaveBeenCalled();
    expect(notifySharedMock).toHaveBeenCalledWith('video-1');
  });

  it('clicking the close button calls onClose', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByRole('button', { name: 'Close' }));
    expect(onClose).toHaveBeenCalled();
  });

  it('Escape key closes the picker', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.keyDown(document, { key: 'Escape' });
    expect(onClose).toHaveBeenCalled();
  });

  it('clicking outside the picker closes it', async () => {
    render(
      <div>
        <div data-testid="outside">outside</div>
        <GifPicker onSelect={onSelect} onClose={onClose} position={position} />
      </div>
    );
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.mouseDown(screen.getByTestId('outside'));
    expect(onClose).toHaveBeenCalled();
  });

  it('clicking inside the picker does not close it', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.mouseDown(screen.getByText('Trending'));
    expect(onClose).not.toHaveBeenCalled();
  });

  // --- Reduce Animations: picker must always animate (#571 item #6A) ---

  it('picker renders animated <video> tiles even when Reduce Animations is ON', async () => {
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    // Sample GIF is a video rendition — must appear as <video>, not the still <img>
    await waitFor(() => {
      expect(document.querySelector('video')).not.toBeNull();
    });
  });

  it('picker renders animated <img> tiles (image rendition) regardless of Reduce Animations', async () => {
    trendingMock.mockResolvedValue({ items: [sampleImageGif], hasMore: false });
    useSettingsStore.setState((s) => ({
      appearance: { ...s.appearance, reduceAnimations: true },
    }));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    await waitFor(() => {
      const img = document.querySelector('.gif-tile img') as HTMLImageElement;
      expect(img).not.toBeNull();
      // Uses the animated URL, NOT the still — picker ignores reduceAnimations
      expect(img.getAttribute('src')).toBe('https://media.klipy.com/g1.gif');
    });
  });

  it('save overlay click adds the GIF to saved store without sending it', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    await waitFor(() => expect(document.querySelector('.gif-save-overlay')).not.toBeNull());
    const saveBtn = document.querySelector('.gif-save-overlay') as HTMLElement;
    fireEvent.click(saveBtn);
    expect(useSavedGifsStore.getState().isGifSaved('video-1')).toBe(true);
    expect(onSelect).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });
});
