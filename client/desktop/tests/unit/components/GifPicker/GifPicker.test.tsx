import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
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
    await waitFor(() =>
      expect(recentMock).toHaveBeenCalledWith(expect.objectContaining({ offset: 0, limit: 25 }))
    );
  });

  it('Recent tab shows empty state when no recent GIFs are available', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() =>
      expect(
        screen.getByText("You haven't shared any GIFs yet. Send one to see it here.")
      ).toBeInTheDocument()
    );
  });

  it('Categories tab calls gifProvider.categories', async () => {
    categoriesMock.mockResolvedValue([
      {
        name: 'Reactions',
        query: 'reaction',
        preview: {
          animatedUrl: sampleImageGif.animatedUrl,
          animatedKind: sampleImageGif.animatedKind,
          stillUrl: sampleImageGif.stillUrl,
        },
      },
      {
        name: 'Animals',
        query: 'animals',
        preview: {
          animatedUrl: sampleVideoGif.animatedUrl,
          animatedKind: sampleVideoGif.animatedKind,
          stillUrl: sampleVideoGif.stillUrl,
        },
      },
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

  it('clicking a category searches by its query, not its display name', async () => {
    categoriesMock.mockResolvedValue([
      {
        name: 'Reactions',
        query: 'reaction',
        preview: {
          animatedUrl: 'https://media.klipy.com/g1.gif',
          animatedKind: 'image' as const,
          stillUrl: 'https://media.klipy.com/g1.jpg',
        },
      },
    ]);
    searchMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Categories'));
    await waitFor(() => expect(screen.getByText('Reactions')).toBeInTheDocument());
    fireEvent.click(screen.getByText('Reactions'));

    // The search box shows the QUERY, because the input is the user's only
    // handle on the search — text that does not drive the search is a lie.
    await waitFor(() =>
      expect(searchMock).toHaveBeenCalledWith(expect.objectContaining({ q: 'reaction' }))
    );
  });

  it('Saved tab shows empty state when no GIFs are saved', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Saved'));
    await waitFor(() => expect(screen.getByText('No saved GIFs yet.')).toBeInTheDocument());
  });

  it('Saved tab shows an error, not an empty state, when every lookup fails', async () => {
    // Promise.allSettled FULFILLS even when every request rejects, so the
    // .catch() is unreachable and an all-failed load fell through to
    // "No saved GIFs yet." — telling a user with saved GIFs that they have
    // none. Same failure-disguised-as-empty class as the Recent tab (#2371 A3).
    useSavedGifsStore.getState().saveGif('saved-slug-1');
    useSavedGifsStore.getState().saveGif('saved-slug-2');
    getBySlugMock.mockRejectedValue(new Error('network down'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Saved'));

    await waitFor(() => expect(screen.getByText("Couldn't load saved GIFs.")).toBeInTheDocument());
    expect(screen.queryByText('No saved GIFs yet.')).toBeNull();
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
  });

  it('Saved tab still shows the empty state for a genuinely empty list', async () => {
    // The fix must not turn a real empty list into an error.
    getBySlugMock.mockRejectedValue(new Error('should not be called'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Saved'));
    await waitFor(() => expect(screen.getByText('No saved GIFs yet.')).toBeInTheDocument());
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

  it('a retry on another tab does not arm force for a later Recent load', async () => {
    // retryNonce is bumped by the retry on ANY tab, so a Trending retry must
    // not permanently clear the Recent identity backoff (Gitar + Codex P2).
    trendingMock.mockRejectedValue(new Error('boom'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
    );
    trendingMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));
    // Gate on a POSITIVE completion, then assert absence synchronously
    // ([internal]rules/tests.md — never wrap a negative assertion in waitFor).
    // Waiting on the button's absence passed the instant the retry entered its
    // loading state, which is BEFORE the retried Trending request settled — so
    // a retry that hung, or that restored the error afterwards, still reached
    // the force assertion below and passed it for the wrong reason.
    await waitFor(() => expect(document.querySelector('.gif-tile')).not.toBeNull());
    expect(screen.queryByRole('button', { name: /try again/i })).toBeNull();

    recentMock.mockResolvedValue({ items: [], hasMore: false });
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() => expect(recentMock).toHaveBeenCalled());
    expect(recentMock).toHaveBeenLastCalledWith(expect.objectContaining({ force: false }));

    // POSITIVE CONTROL — without it the assertion above is an END STATE, and an
    // end state is satisfied by TWO regressions that cancel: one arming
    // forceRecentRef from a non-Recent retry (dropping the activeTab check at
    // its assignment), another consuming it before recent() is called. Either
    // alone fails the `force: false` above; together they restore it, and the
    // test passes on broken code. Falsifying by breaking ONE guard does not
    // reveal this — it only survives when both regress at once.
    //
    // Proving force can STILL be armed makes the second regression observable,
    // so the pair can no longer hide inside each other. This deliberately
    // overlaps 'a Recent retry forces exactly once, then reverts': a positive
    // control has to live in the test it protects, because tests get split,
    // reordered and run under `-t` filters that leave siblings behind.
    recentMock.mockRejectedValue(new Error('identity'));
    fireEvent.click(screen.getByText('Trending'));
    await waitFor(() => expect(document.querySelector('.gif-tile')).not.toBeNull());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
    );
    recentMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));
    await waitFor(() =>
      expect(recentMock).toHaveBeenLastCalledWith(expect.objectContaining({ force: true }))
    );
  });

  it('a Recent retry forces exactly once, then reverts', async () => {
    recentMock.mockRejectedValue(new Error('identity'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
    );

    recentMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));
    await waitFor(() =>
      expect(recentMock).toHaveBeenLastCalledWith(expect.objectContaining({ force: true }))
    );

    // Leave and come back: that one fetch must have consumed the force.
    fireEvent.click(screen.getByText('Trending'));
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() =>
      expect(recentMock).toHaveBeenLastCalledWith(expect.objectContaining({ force: false }))
    );
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

  // ── Error states, retry, and a11y (#2371 A3) ──────────────────────────
  it('shows a distinguishable error with a retry when Recent fails', async () => {
    recentMock.mockRejectedValue(new Error('identity'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));

    await waitFor(() =>
      expect(screen.getByText("Couldn't load your recent GIFs.")).toBeInTheDocument()
    );
    expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument();
  });

  it('an empty Recent list is worded differently from a failure', async () => {
    recentMock.mockResolvedValue({ items: [], hasMore: false });
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));

    await waitFor(() =>
      expect(
        screen.getByText("You haven't shared any GIFs yet. Send one to see it here.")
      ).toBeInTheDocument()
    );
    expect(screen.queryByRole('button', { name: /try again/i })).not.toBeInTheDocument();
  });

  it('retry on the Recent tab refetches with force', async () => {
    recentMock.mockRejectedValue(new Error('identity'));
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Recent'));
    await waitFor(() =>
      expect(screen.getByRole('button', { name: /try again/i })).toBeInTheDocument()
    );

    recentMock.mockResolvedValue({ items: [sampleVideoGif], hasMore: false });
    fireEvent.click(screen.getByRole('button', { name: /try again/i }));

    await waitFor(() =>
      expect(recentMock).toHaveBeenLastCalledWith(expect.objectContaining({ force: true }))
    );
  });

  it('no picker state string says "not found" or "not available"', async () => {
    // The reporter read the existing wording as a fault, not as an empty state.
    categoriesMock.mockResolvedValue([]);
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Categories'));

    await waitFor(() =>
      expect(screen.getByText('No categories to show right now.')).toBeInTheDocument()
    );
    expect(screen.queryByText(/not found|not available/i)).not.toBeInTheDocument();
  });

  it('marks the active tab programmatically, not by colour alone', async () => {
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    expect(screen.getByText('Trending').closest('button')).toHaveAttribute('aria-pressed', 'true');
    expect(screen.getByText('Categories').closest('button')).toHaveAttribute(
      'aria-pressed',
      'false'
    );
  });

  it('announces the empty state to assistive tech', async () => {
    // Assert the announced node IS the empty state. A bare
    // `waitFor(() => getByRole('status'))` is vacuous here: the loading
    // indicator is also a status node, so it satisfies the query during the
    // loading -> empty transition and the test passes whatever markup the
    // empty state ends up with.
    categoriesMock.mockResolvedValue([]);
    render(<GifPicker onSelect={onSelect} onClose={onClose} position={position} />);
    await waitFor(() => expect(trendingMock).toHaveBeenCalled());
    fireEvent.click(screen.getByText('Categories'));

    await waitFor(() =>
      expect(screen.getByText('No categories to show right now.')).toBeInTheDocument()
    );
    expect(screen.getByRole('status')).toHaveTextContent('No categories to show right now.');
  });
});

// ── Layout invariance (#2371 defect B) ────────────────────────────────────
//
// Asserted against the stylesheet SOURCE, not the rendered DOM. Vitest runs
// with `css: false` (no `css:` key in vite.config.ts), so `import './GifPicker.css'`
// is stubbed and getComputedStyle cannot resolve these rules — a toBeVisible()
// or class-presence assertion here would be vacuous. Same approach as
// tests/unit/renderer/components/Titlebar/Titlebar.theme.test.tsx.
describe('GifPicker.css layout invariants (#2371 B)', () => {
  // Strip comments before asserting. These assertions are about DECLARATIONS,
  // and the rules below are commented with prose that names the very
  // declarations being forbidden (e.g. explaining why `flex: 1` must not be
  // present) — matching raw text would fail on the explanation itself.
  const css = readFileSync(
    resolve(__dirname, '../../../../src/renderer/components/GifPicker/GifPicker.css'),
    'utf-8'
  ).replace(/\/\*[\s\S]*?\*\//g, '');

  /** Extract one rule block by exact selector. */
  function ruleBody(selector: string): string {
    const m = new RegExp(`(^|\\n)${selector.replace('.', '\\.')}\\s*\\{([^}]*)\\}`).exec(css);
    if (!m) throw new Error(`selector ${selector} not found in GifPicker.css`);
    return m[2];
  }

  it('.gif-picker-body declares one deterministic height, not a min/max pair', () => {
    const body = ruleBody('.gif-picker-body');
    expect(body).toMatch(/height:\s*360px/);
    expect(body).not.toMatch(/min-height/);
    expect(body).not.toMatch(/max-height/);
  });

  it('.gif-picker-body does NOT declare flex: 1', () => {
    // `flex: 1` sets flex-basis: 0%, which resolves to `content` against
    // .gif-picker's indefinite height — the declared height would not govern
    // the flex base size and the whole fix would silently no-op.
    // EmojiPicker.css declares height + flex-shrink: 0 and no flex shorthand.
    const body = ruleBody('.gif-picker-body');
    expect(body).not.toMatch(/\bflex:\s*1\b/);
    expect(body).toMatch(/flex-shrink:\s*0/);
  });

  it('the loading and empty states fill the body instead of pinning 160px', () => {
    // Loading shared the 160px rule, so the picker also collapsed transiently
    // on every tab switch and on initial open — broader than reported.
    expect(css).not.toMatch(/height:\s*160px/);
  });

  it('does not pin a height on .gif-picker itself (that value belongs to #2370)', () => {
    const picker = ruleBody('.gif-picker');
    expect(picker).not.toMatch(/(^|\s)height:/);
    expect(picker).toMatch(/max-height:\s*520px/);
    expect(picker).toMatch(/width:\s*370px/);
  });
});
