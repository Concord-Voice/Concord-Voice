import React, { useState, useEffect, useLayoutEffect, useRef, useCallback } from 'react';
import { createPortal } from 'react-dom';
import { EmojiPickerProps, EmojiEntry, SkinTone, SKIN_TONES } from './types';
import { useEmojiData } from './useEmojiData';
import { getRecentEmojis, addRecentEmoji, getSavedSkinTone, saveSkinTone } from './emojiDataCache';
import EmojiSearch from './EmojiSearch';
import EmojiCategoryBar from './EmojiCategoryBar';
import EmojiGrid from './EmojiGrid';
import { resolveAnchoredPlacement } from '../../utils/ui/pickerAnchor';
import './EmojiPicker.css';

/** Border width and corner radius baked into EmojiPicker.css's `.emoji-picker`
 *  rule — kept in sync with the stylesheet, not read from it (#2370). */
const EMOJI_PICKER_BORDER_WIDTH = 1;
const EMOJI_PICKER_CORNER_RADIUS = 8;

interface AnchoredPlacement {
  left: number;
  top: number;
  arrowX: number | null;
  showArrow: boolean;
}

const EmojiPicker: React.FC<EmojiPickerProps> = ({
  onSelect,
  onClose,
  mode = 'popover',
  position,
}) => {
  const { categories, loadingCategory, loadCategory, getCategory, search, loadAllForSearch } =
    useEmojiData();

  const recentEmojis = getRecentEmojis();
  const hasRecent = recentEmojis.length > 0;

  const [activeCategory, setActiveCategory] = useState<string>(hasRecent ? 'recent' : 'smileys');
  const [searchQuery, setSearchQuery] = useState('');
  const [searchResults, setSearchResults] = useState<EmojiEntry[]>([]);
  const [hoveredEmoji, setHoveredEmoji] = useState<EmojiEntry | null>(null);
  const [skinTone, setSkinTone] = useState<SkinTone>(() => getSavedSkinTone() as SkinTone);
  const [showSkinTones, setShowSkinTones] = useState(false);

  const pickerRef = useRef<HTMLDivElement>(null);
  // eslint-disable-next-line @eslint-react/naming-convention-ref-name -- stable ref; rename to the *Ref-suffix convention deferred to avoid churning untested handler lines in this low-coverage component (new-code coverage gate). Cosmetic rule suppressed per [internal]rules conventions.
  const initialLoadDone = useRef(false);

  // Placement + arrow. Electron windows can't render popups beyond the
  // BrowserWindow bounds, so we measure the picker after mount and place it
  // (#2370 §2.1). `anchorCenterX` present (composer only) -> anchored:
  // resolveAnchoredPlacement places ABOVE the anchor with a caret pointing
  // down at it. Absent (the other consumers) -> the pre-#2370 clamp-and-flip
  // below runs byte-identically, with no arrow (A10).
  const [clampedPos, setClampedPos] = useState<AnchoredPlacement | null>(null);

  useLayoutEffect(() => {
    if (mode !== 'popover' || !position || !pickerRef.current) return;
    const rect = pickerRef.current.getBoundingClientRect();
    const vw = globalThis.innerWidth;
    const vh = globalThis.innerHeight;

    if (typeof position.anchorCenterX === 'number') {
      // offsetWidth/offsetHeight, NOT getBoundingClientRect(). This picker
      // carries `animation: emojiPickerIn` whose `from` keyframe is
      // `scale(0.95) translateY(4px)`, and getBoundingClientRect() reports the
      // TRANSFORMED box. This effect runs at mount, on that first keyframe, so
      // the rect is 95% of the real size -- which placed the picker ~20px too
      // low and drove its caret into the button it points at. The offset*
      // properties are layout values and ignore transforms. The GIF picker
      // never showed this because it has no entry animation; do not "unify"
      // the two by reverting this to rect. jsdom runs no animations and
      // applies no transforms, so no unit test can observe this -- the guard
      // is the source pin in the test suite plus the visual pass (#2370).
      const el = pickerRef.current;
      const resolved = resolveAnchoredPlacement({
        anchorTop: position.y,
        anchorRight: position.x,
        anchorCenterX: position.anchorCenterX,
        measuredWidth: el.offsetWidth,
        measuredHeight: el.offsetHeight,
        viewportWidth: vw,
        viewportHeight: vh,
        borderWidth: EMOJI_PICKER_BORDER_WIDTH,
        cornerRadius: EMOJI_PICKER_CORNER_RADIUS,
      });
      setClampedPos(resolved);
      return;
    }

    // Legacy clamp-and-flip — byte-identical to pre-#2370 behavior (R4/A10).
    // `position.x`/`position.y` are the picker's own desired top-left here,
    // not an anchor rect.
    const margin = 8;
    let left = position.x;
    let top = position.y;

    if (left + rect.width + margin > vw) left = vw - rect.width - margin;
    if (left < margin) left = margin;

    if (top + rect.height + margin > vh) {
      // Flip above the anchor point if there's room; otherwise clamp to bottom.
      const flipped = position.y - rect.height - 8;
      top = flipped >= margin ? flipped : Math.max(margin, vh - rect.height - margin);
    }
    if (top < margin) top = margin;

    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clamps picker position to viewport after mount; fires in useLayoutEffect on position/mode change, not on every render
    setClampedPos({ left, top, arrowX: null, showArrow: false });
  }, [mode, position]);

  // Captured once, synchronously, during the FIRST render — before
  // EmojiSearch's `autoFocus` (mode==='popover') can move focus away from
  // whatever triggered this picker. This is who focus returns to on any
  // NON-selection close (Escape, outside-click, resize); selection keeps its
  // own existing focus handling in the caller (#2370 §2.5).
  const [triggerEl] = useState<HTMLElement | null>(() =>
    document.activeElement instanceof HTMLElement ? document.activeElement : null
  );
  const restoreFocus = useCallback(() => {
    if (triggerEl?.isConnected) triggerEl.focus({ preventScroll: true });
  }, [triggerEl]);

  // Load initial category on mount
  useEffect(() => {
    if (categories.length > 0 && !initialLoadDone.current) {
      initialLoadDone.current = true;
      loadCategory('smileys');
    }
  }, [categories, loadCategory]);

  // Load category when tab changes
  useEffect(() => {
    if (activeCategory && activeCategory !== 'recent') {
      loadCategory(activeCategory);
    }
  }, [activeCategory, loadCategory]);

  // Handle search
  useEffect(() => {
    if (searchQuery.trim()) {
      const results = search(searchQuery);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: updates search results when query changes; not a render loop
      setSearchResults(results);
      // If searching and not all categories loaded, load them
      loadAllForSearch();
    } else {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears search results when query is empty; not a render loop
      setSearchResults([]);
    }
  }, [searchQuery, search, loadAllForSearch]);

  // Close on outside click (popover mode only)
  useEffect(() => {
    if (mode !== 'popover') return;

    const handleClickOutside = (e: MouseEvent) => {
      if (e.target instanceof Node && pickerRef.current && !pickerRef.current.contains(e.target)) {
        onClose();
        restoreFocus();
      }
    };

    const handleEscape = (e: KeyboardEvent) => {
      if (e.key === 'Escape') {
        onClose();
        restoreFocus();
      }
    };

    // Delay adding click listener to avoid immediate close from the opening click
    const timer = setTimeout(() => {
      document.addEventListener('mousedown', handleClickOutside);
    }, 0);
    document.addEventListener('keydown', handleEscape);

    return () => {
      clearTimeout(timer);
      document.removeEventListener('mousedown', handleClickOutside);
      document.removeEventListener('keydown', handleEscape);
    };
  }, [mode, onClose, restoreFocus]);

  // Close on resize (R10/T8) — popover mode only, a SEPARATE effect from the
  // click-outside/escape one above and untouched by MessageInput's own
  // resize listener (`:552-557` there), which re-registers every keystroke
  // (`useCallback([content])`) and must not be reused for this. Continuous
  // anchor tracking (scroll/ResizeObserver) is a deliberately recorded
  // residual, not implemented.
  useEffect(() => {
    if (mode !== 'popover') return;
    const handleResize = () => {
      onClose();
      restoreFocus();
    };
    globalThis.addEventListener('resize', handleResize);
    return () => globalThis.removeEventListener('resize', handleResize);
  }, [mode, onClose, restoreFocus]);

  const handleEmojiSelect = useCallback(
    (emoji: string) => {
      addRecentEmoji(emoji);
      onSelect(emoji);
    },
    [onSelect]
  );

  const handleCategorySelect = useCallback((categoryId: string) => {
    setActiveCategory(categoryId);
    setSearchQuery('');
  }, []);

  const handleSkinToneChange = useCallback((tone: SkinTone) => {
    setSkinTone(tone);
    saveSkinTone(tone);
    setShowSkinTones(false);
  }, []);

  // Get current display emojis
  const getDisplayEmojis = (): EmojiEntry[] => {
    if (searchQuery.trim()) {
      return searchResults;
    }
    if (activeCategory === 'recent') {
      return recentEmojis.map((e) => ({
        e,
        n: 'recently used',
        s: false,
        c: [],
      }));
    }
    return getCategory(activeCategory);
  };

  const displayEmojis = getDisplayEmojis();
  const isLoading = loadingCategory === activeCategory;
  // Anchored (composer) mode only: the other consumers never pass
  // anchorCenterX and stay on the legacy clamp-and-flip path, with no arrow.
  const isAnchored = mode === 'popover' && typeof position?.anchorCenterX === 'number';

  const pickerContent = (
    <div
      ref={pickerRef}
      // Array + filter rather than a template literal: the conditional segments
      // left double and trailing spaces, which nothing breaks on today but which
      // an exact `toBe` on className later would trip over.
      className={[
        'emoji-picker',
        mode === 'popover' ? 'emoji-picker--popover' : 'emoji-picker--inline',
        isAnchored && 'emoji-picker--anchored',
        isAnchored && clampedPos && !clampedPos.showArrow && 'emoji-picker--no-arrow',
      ]
        .filter(Boolean)
        .join(' ')}
      style={
        mode === 'popover' && position
          ? ({
              left: clampedPos?.left ?? position.x,
              top: clampedPos?.top ?? position.y,
              // Hide until placed to avoid a one-frame flash off-screen (or,
              // for anchored mode, at the raw anchor coordinates).
              visibility: clampedPos ? 'visible' : 'hidden',
              ['--emoji-picker-arrow-x' as string]: `${clampedPos?.arrowX ?? 0}px`,
            } as React.CSSProperties)
          : undefined
      }
      // The portalled root has no role/name of its own outside this (4.1.2).
      // Only in popover mode — inline instances are embedded in an
      // already-labeled panel, not a floating dialog.
      {...(mode === 'popover' ? { role: 'dialog' as const, 'aria-label': 'Emoji picker' } : {})}
    >
      <EmojiSearch onSearch={setSearchQuery} autoFocus={mode === 'popover'} />

      {!searchQuery && (
        <EmojiCategoryBar
          categories={categories}
          activeCategory={activeCategory}
          hasRecent={hasRecent}
          onSelect={handleCategorySelect}
        />
      )}

      <EmojiGrid
        emojis={displayEmojis}
        onSelect={handleEmojiSelect}
        skinTone={skinTone}
        isLoading={isLoading}
        onHover={setHoveredEmoji}
      />

      <div className="emoji-picker-footer">
        <div className="emoji-picker-preview">
          {hoveredEmoji ? (
            <>
              <span className="emoji-picker-preview-emoji">{hoveredEmoji.e}</span>
              <span className="emoji-picker-preview-name">{hoveredEmoji.n}</span>
            </>
          ) : (
            <span className="emoji-picker-preview-name">
              {(() => {
                if (searchQuery) {
                  const plural = searchResults.length === 1 ? '' : 's';
                  return `${searchResults.length} result${plural}`;
                }
                return categories.find((c) => c.id === activeCategory)?.name || 'Frequently Used';
              })()}
            </span>
          )}
        </div>

        <div className="emoji-picker-skin-tones-wrapper">
          <button
            type="button"
            className="emoji-picker-skin-toggle"
            onClick={() => setShowSkinTones((prev) => !prev)}
            title="Skin tone"
            aria-label="Change skin tone"
          >
            {SKIN_TONES.find((t) => t.tone === skinTone)?.preview || '👋'}
          </button>

          {showSkinTones && (
            <div className="emoji-picker-skin-tones">
              {SKIN_TONES.map((t) => (
                <button
                  type="button"
                  key={t.tone || 'default'}
                  className={`emoji-picker-skin-btn ${skinTone === t.tone ? 'emoji-picker-skin-btn--active' : ''}`}
                  onClick={() => handleSkinToneChange(t.tone)}
                  title={t.label}
                  aria-label={t.label}
                >
                  {t.preview}
                </button>
              ))}
            </div>
          )}
        </div>
      </div>
    </div>
  );

  if (mode === 'popover') {
    return createPortal(pickerContent, document.body);
  }

  return pickerContent;
};

export default EmojiPicker;
