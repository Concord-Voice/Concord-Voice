import React, { useState, useCallback, useRef, useEffect, useMemo } from 'react';
import { Save, X, Search, AlertTriangle } from 'lucide-react';
import {
  gifProvider,
  type GifResolved,
  type GifCategory,
  type GifCategoryPreview,
} from '../../services/gifProvider';
import { useSavedGifsStore } from '../../stores/savedGifsStore';
import { useSettingsStore } from '../../stores/settingsStore';
import { usePrivacyStore } from '../../stores/privacyStore';
import './GifPicker.css';

interface GifPickerProps {
  onSelect: (slug: string) => void;
  onClose: () => void;
  position: { x: number; y: number; anchorCenterX: number };
}

type Tab = 'trending' | 'recent' | 'categories' | 'saved';

const PAGE_SIZE = 25;
const SEARCH_DEBOUNCE_MS = 300;

/** Render the animated rendition (video or image) for a GIF.
 *
 *  The picker intentionally IGNORES the Reduce Animations setting — users
 *  need to see the animation to choose a GIF. Reduce Animations only affects
 *  inline chat embeds (GifEmbed), not the picker itself. See QA bug #571
 *  item #6A. */
function GifMedia({ gif }: Readonly<{ gif: GifCategoryPreview }>) {
  if (gif.animatedKind === 'video') {
    return (
      <video
        src={gif.animatedUrl}
        poster={gif.stillUrl}
        autoPlay
        loop
        muted
        playsInline
        tabIndex={-1}
      />
    );
  }
  return <img src={gif.animatedUrl} alt="" draggable={false} />;
}

/** Save / unsave button rendered on hover over each GIF tile. */
function SaveOverlayButton({ slug }: Readonly<{ slug: string }>) {
  const isSaved = useSavedGifsStore((s) => s.isGifSaved(slug));
  const saveGif = useSavedGifsStore((s) => s.saveGif);
  const removeGif = useSavedGifsStore((s) => s.removeGif);

  const handleClick = (e: React.MouseEvent) => {
    e.stopPropagation();
    if (isSaved) {
      removeGif(slug);
    } else {
      saveGif(slug);
    }
  };

  return (
    <button
      className={`gif-save-overlay ${isSaved ? 'saved' : ''}`}
      onClick={handleClick}
      aria-label={isSaved ? 'Remove from saved' : 'Save GIF'}
      title={isSaved ? 'Remove from saved' : 'Save GIF'}
    >
      <Save size={16} />
    </button>
  );
}

/** Individual GIF tile with autoplay video / fallback image and the save overlay. */
function GifTile({ gif, onClick }: Readonly<{ gif: GifResolved; onClick: () => void }>) {
  return (
    <button
      className="gif-tile"
      onClick={(e) => {
        e.preventDefault();
        onClick();
      }}
      aria-label="Send GIF"
    >
      <GifMedia gif={gif} />
      <SaveOverlayButton slug={gif.slug} />
    </button>
  );
}

/** Category tile — clicking it switches the picker into search mode for that category. */
function CategoryTile({
  category,
  onClick,
}: Readonly<{ category: GifCategory; onClick: () => void }>) {
  return (
    <button
      className="gif-tile gif-category-tile"
      onClick={onClick}
      aria-label={`Browse ${category.name}`}
    >
      <GifMedia gif={category.preview} />
      <span className="gif-category-name">{category.name}</span>
    </button>
  );
}

/** Empty state shown when a tab has no content. `<output>` rather than a div
 *  with role="status": it carries the same implicit role, and the native
 *  element is announced reliably across assistive tech where the ARIA role
 *  alone is not (SonarQube S6819). The stylesheet already sets display:flex,
 *  so the element default of inline does not apply. */
function EmptyState({ message }: Readonly<{ message: string }>) {
  return <output className="gif-picker-empty">{message}</output>;
}

/** Failure state. Distinguished from an empty tab by glyph, copy AND an
 *  action — three non-colour signals, so it does not rely on colour alone
 *  (WCAG 1.4.1). Deliberately not red. */
function ErrorState({ message, onRetry }: Readonly<{ message: string; onRetry: () => void }>) {
  return (
    <output className="gif-picker-error">
      <AlertTriangle size={20} aria-hidden="true" />
      <span>{message}</span>
      <button className="gif-picker-retry" onClick={onRetry}>
        Try again
      </button>
    </output>
  );
}

/** Render the body of the picker — depends on active tab + search state. */
function PickerBody({
  loading,
  error,
  isSearching,
  debouncedSearchTerm,
  activeTab,
  items,
  categories,
  onGifClick,
  onCategoryClick,
  onRetry,
}: Readonly<{
  loading: boolean;
  error: string | null;
  isSearching: boolean;
  debouncedSearchTerm: string;
  activeTab: Tab;
  items: GifResolved[];
  categories: GifCategory[];
  onGifClick: (gif: GifResolved) => void;
  onCategoryClick: (query: string) => void;
  onRetry: () => void;
}>) {
  if (loading) {
    return <output className="gif-picker-loading">Loading…</output>;
  }
  if (error) return <ErrorState message={error} onRetry={onRetry} />;

  if (activeTab === 'categories' && !isSearching) {
    if (categories.length === 0) return <EmptyState message="No categories to show right now." />;
    return (
      <div className="gif-picker-grid">
        {categories.map((cat) => (
          <CategoryTile key={cat.query} category={cat} onClick={() => onCategoryClick(cat.query)} />
        ))}
      </div>
    );
  }

  if (items.length === 0) {
    let msg = 'No GIFs to show right now.';
    if (isSearching) msg = `No GIFs found for "${debouncedSearchTerm}"`;
    else if (activeTab === 'saved') msg = 'No saved GIFs yet.';
    else if (activeTab === 'recent')
      msg = "You haven't shared any GIFs yet. Send one to see it here.";
    else if (activeTab === 'trending') msg = 'No trending GIFs to show right now.';
    return <EmptyState message={msg} />;
  }

  return (
    <div className="gif-picker-grid">
      {items.map((gif) => (
        <GifTile key={gif.slug} gif={gif} onClick={() => onGifClick(gif)} />
      ))}
    </div>
  );
}

/** Main GIF picker. Renders four tabs (Trending, Recent, Categories, Saved) plus
 *  a search input that overrides the active tab when text is present. The picker
 *  is rendered in a `<dialog>` element for accessibility. */
const GifPicker: React.FC<GifPickerProps> = ({ onSelect, onClose, position }) => {
  const reduceAnimations = useSettingsStore((s) => s.appearance.reduceAnimations);
  const themeMode = useSettingsStore((s) => s.appearance.theme);
  const savedGifSlugs = useSavedGifsStore((s) => s.gifs);
  const sharePersonalization = usePrivacyStore(
    (s) => s.settings.sharePersonalizationWithGifProvider
  );

  // Match the runtime theme (light/dark) used by document.documentElement —
  // settings.theme can be "system", so resolve via the same media query the
  // settings store does. The picker re-evaluates on theme change because
  // themeMode is in the dep graph above.
  /* eslint-disable @eslint-react/purity -- globalThis.matchMedia read is intentional to resolve 'system' theme to dark/light; read-only, no observable side effect */
  const resolvedDark =
    themeMode === 'dark' ||
    (themeMode === 'system' && globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches);
  /* eslint-enable @eslint-react/purity -- end of matchMedia read block */
  const providerLogo = resolvedDark ? gifProvider.logoAssetDark : gifProvider.logoAssetLight;

  const [activeTab, setActiveTab] = useState<Tab>('trending');
  const [searchTerm, setSearchTerm] = useState('');
  const [debouncedSearchTerm, setDebouncedSearchTerm] = useState('');
  const [items, setItems] = useState<GifResolved[]>([]);
  const [categories, setCategories] = useState<GifCategory[]>([]);
  const [loading, setLoading] = useState(false);
  const [error, setError] = useState<string | null>(null);
  // Component-local retry counter. Bumping it re-runs the fetch effect; a
  // transient, picker-scoped gesture does not warrant a store.
  const [retryNonce, setRetryNonce] = useState(0);
  // One-shot, Recent-specific force flag. It must NOT be derived from
  // `retryNonce`: that counter only increments and is bumped by the retry on
  // ANY tab, so `force: retryNonce > 0` would arm the identity-backoff bypass
  // permanently after a single click anywhere in the picker — including on
  // ordinary tab switches back to Recent. The fetch that uses it consumes it.
  const forceRecentRef = useRef(false);

  const pickerRef = useRef<HTMLDialogElement>(null);

  // Debounce search term
  useEffect(() => {
    const t = setTimeout(() => setDebouncedSearchTerm(searchTerm.trim()), SEARCH_DEBOUNCE_MS);
    return () => clearTimeout(t);
  }, [searchTerm]);

  const isSearching = debouncedSearchTerm.length > 0;

  // Fetch content based on active tab + search state
  useEffect(() => {
    let cancelled = false;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: shows loading state while fetching GIFs based on tab/search changes; not a render loop
    setLoading(true);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when starting a new fetch; not a render loop
    setError(null);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears items when starting a new fetch to avoid stale content flash; not a render loop
    setItems([]);

    // Read-and-clear: exactly one fetch spends an explicit retry gesture.
    const consumeForceRecent = () => {
      const f = forceRecentRef.current;
      forceRecentRef.current = false;
      return f;
    };

    const finish = (gifs: GifResolved[]) => {
      if (!cancelled) {
        setItems(gifs);
        setLoading(false);
      }
    };
    const fail = (msg: string) => {
      if (!cancelled) {
        setError(msg);
        setLoading(false);
      }
    };

    if (isSearching) {
      gifProvider
        .search({ q: debouncedSearchTerm, offset: 0, limit: PAGE_SIZE })
        .then((r) => finish(r.items))
        .catch(() => fail("Couldn't load search results."));
      return () => {
        cancelled = true;
      };
    }

    if (activeTab === 'trending') {
      gifProvider
        .trending({ offset: 0, limit: PAGE_SIZE })
        .then((r) => finish(r.items))
        .catch(() => fail("Couldn't load trending GIFs."));
    } else if (activeTab === 'recent') {
      gifProvider
        .recent({ offset: 0, limit: PAGE_SIZE, force: consumeForceRecent() })
        .then((r) => finish(r.items))
        .catch(() => fail("Couldn't load your recent GIFs."));
    } else if (activeTab === 'categories') {
      gifProvider
        .categories({})
        .then((cats) => {
          if (!cancelled) {
            setCategories(cats);
            setItems([]);
            setLoading(false);
          }
        })
        .catch(() => fail("Couldn't load categories."));
    } else if (activeTab === 'saved') {
      Promise.allSettled(savedGifSlugs.map((sg) => gifProvider.getBySlug(sg.slug)))
        .then((results) => {
          // allSettled FULFILLS even when every request rejects, so the
          // .catch() below never sees individual failures. Without this branch
          // an all-failed load fell through to "No saved GIFs yet." — telling a
          // user who has saved GIFs that they have none, the same
          // failure-disguised-as-empty defect this PR fixes on the Recent tab.
          // Guarded on length so a genuinely empty list stays an empty state.
          if (results.length > 0 && results.every((r) => r.status === 'rejected')) {
            fail("Couldn't load saved GIFs.");
            return;
          }
          const gifs: GifResolved[] = [];
          for (const r of results) {
            if (r.status === 'fulfilled') gifs.push(r.value);
          }
          finish(gifs);
        })
        .catch(() => fail("Couldn't load saved GIFs."));
    }

    return () => {
      cancelled = true;
    };
  }, [activeTab, isSearching, debouncedSearchTerm, savedGifSlugs, retryNonce]);

  const handleGifClick = useCallback(
    (gif: GifResolved) => {
      // Fire-and-forget. notifyShared never rejects and emits its own signal
      // on every failure shape, so there is nothing to catch here.
      void gifProvider.notifyShared?.(
        gif.slug,
        isSearching ? { q: debouncedSearchTerm } : undefined
      );
      onSelect(gif.slug);
      onClose();
    },
    [onSelect, onClose, isSearching, debouncedSearchTerm]
  );

  const handleCategoryClick = useCallback((query: string) => {
    setSearchTerm(query);
  }, []);

  // Click-outside-to-close
  useEffect(() => {
    const handleClickOutside = (e: MouseEvent) => {
      if (e.target instanceof Node && pickerRef.current && !pickerRef.current.contains(e.target)) {
        onClose();
      }
    };
    document.addEventListener('mousedown', handleClickOutside);
    return () => document.removeEventListener('mousedown', handleClickOutside);
  }, [onClose]);

  // Escape to close
  useEffect(() => {
    const handleKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onClose();
    };
    document.addEventListener('keydown', handleKey);
    return () => document.removeEventListener('keydown', handleKey);
  }, [onClose]);

  const visibleTabs = useMemo<Tab[]>(() => {
    const tabs: Tab[] = ['trending'];
    // Recent tab requires personalization (KLIPY's /recent endpoint needs customer_id).
    // When the user has disabled personalization, hide the tab entirely.
    if (gifProvider.supportsRecent && sharePersonalization) tabs.push('recent');
    if (gifProvider.supportsCategories) tabs.push('categories');
    tabs.push('saved');
    return tabs;
  }, [sharePersonalization]);

  const tabLabel = (tab: Tab): string => {
    switch (tab) {
      case 'trending':
        return 'Trending';
      case 'recent':
        return 'Recent';
      case 'categories':
        return 'Categories';
      case 'saved':
        return 'Saved';
    }
  };

  return (
    <dialog
      ref={pickerRef}
      open
      className={`gif-picker ${reduceAnimations ? 'reduce-motion' : ''}`}
      style={
        {
          left: position.x,
          top: position.y,
          // Arrow tail x position relative to the picker's left edge.
          ['--gif-picker-arrow-x' as string]: `${position.anchorCenterX - position.x}px`,
        } as React.CSSProperties
      }
      aria-label="GIF picker"
    >
      <div className="gif-picker-header">
        <div className="gif-picker-tabs">
          {visibleTabs.map((tab) => (
            <button
              key={tab}
              className={`gif-picker-tab ${activeTab === tab && !isSearching ? 'active' : ''}`}
              aria-pressed={activeTab === tab && !isSearching}
              onClick={() => {
                setActiveTab(tab);
                setSearchTerm('');
              }}
            >
              {tabLabel(tab)}
            </button>
          ))}
        </div>
        <button className="gif-picker-close" onClick={onClose} aria-label="Close">
          <X size={16} />
        </button>
      </div>

      <div className="gif-picker-search">
        <Search size={14} className="gif-picker-search-icon" />
        <input
          type="text"
          placeholder={gifProvider.searchPlaceholder}
          value={searchTerm}
          onChange={(e) => setSearchTerm(e.target.value)}
          autoFocus
          aria-label={gifProvider.searchPlaceholder}
        />
      </div>

      <div className="gif-picker-body">
        <PickerBody
          loading={loading}
          error={error}
          isSearching={isSearching}
          debouncedSearchTerm={debouncedSearchTerm}
          activeTab={activeTab}
          items={items}
          categories={categories}
          onGifClick={handleGifClick}
          onCategoryClick={handleCategoryClick}
          onRetry={() => {
            // Only a retry ON the Recent tab may clear the identity backoff.
            if (activeTab === 'recent' && !isSearching) forceRecentRef.current = true;
            setRetryNonce((n) => n + 1);
          }}
        />
      </div>

      <div className="gif-picker-footer">
        <span className="gif-picker-attribution">Powered by</span>
        {providerLogo && (
          <img src={providerLogo} alt={gifProvider.name} className="gif-picker-logo" />
        )}
      </div>
    </dialog>
  );
};

export default GifPicker;
