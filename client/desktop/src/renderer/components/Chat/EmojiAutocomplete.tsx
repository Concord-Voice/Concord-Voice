import React, {
  useState,
  useEffect,
  useRef,
  useCallback,
  useMemo,
  useImperativeHandle,
  forwardRef,
} from 'react';
import { useEmojiData } from '../EmojiPicker/useEmojiData';
import type { EmojiEntry } from '../EmojiPicker/types';
import { extractTriggerToken, handleTypeaheadKeyDown } from './typeaheadAutocomplete';
import './typeaheadAutocomplete.css';
import './EmojiAutocomplete.css';

export interface EmojiAutocompleteProps {
  /** The current text input value */
  text: string;
  /** Cursor position in the text */
  cursorPosition: number;
  /**
   * Called when an emoji is chosen. `replacementText` is the insert value
   * (glyph + trailing space) and `startIndex` is the offset of the opening `:`,
   * so the caller replaces `text[startIndex..cursorPosition]`.
   */
  onSelect: (replacementText: string, startIndex: number) => void;
  /** Called when the popup should close */
  onClose: () => void;
  /** Position reference element (the textarea) — reserved for future positioning */
  anchorRef?: React.RefObject<HTMLElement | null>;
}

export interface EmojiAutocompleteHandle {
  handleKeyDown: (e: React.KeyboardEvent) => boolean;
}

// eslint-disable-next-line @eslint-react/no-forward-ref -- mirrors MentionAutocomplete; the React 19 ref-as-prop migration would force a disproportionate re-indent
const EmojiAutocomplete = forwardRef<EmojiAutocompleteHandle, EmojiAutocompleteProps>(
  ({ text, cursorPosition, onSelect, onClose, anchorRef: _anchorRef }, ref) => {
    const [selectedIndex, setSelectedIndex] = useState(0);
    const listRef = useRef<HTMLDivElement>(null);
    const { search, loadAllForSearch } = useEmojiData();

    // Load every category once so shortcode search covers all emoji, not just
    // the eagerly-seeded smileys. Each category load updates useEmojiData's
    // internal state, re-rendering this component so `options` (computed below
    // from the module cache) reflects the newly loaded entries.
    useEffect(() => {
      loadAllForSearch().catch(() => {
        /* keep the seeded-subset results if a category fails to load */
      });
    }, [loadAllForSearch]);

    // Active `:query` token at the cursor. Memoized so its identity is stable
    // across unrelated re-renders (the selection-reset effect keys on it).
    const query = useMemo(
      () => extractTriggerToken(text, cursorPosition, ':'),
      [text, cursorPosition]
    );

    // Shortcode-prefix matches for the active token. Computed each render (not
    // memoized on a load counter) so category loads that re-render this
    // component are reflected against the now-fuller module cache. The leading
    // ':' scopes useEmojiData.search to shortcodes (not free-text names).
    const options: EmojiEntry[] = query?.text ? search(':' + query.text).slice(0, 15) : [];

    // Mirror the latest options into a ref so the memoized keyDown handler can
    // read the current list WITHOUT taking `options` as a dependency. `options`
    // is a fresh array every render (recomputed above, deliberately un-memoized
    // so streamed category loads are reflected), so depending on it would
    // rebuild handleKeyDown every render and trip @eslint-react/exhaustive-deps.
    // The ref is always current by the time a keydown fires.
    const optionsRef = useRef(options);
    optionsRef.current = options;

    // Reset the highlight to the top when the QUERY TOKEN changes — not when
    // `options` churns. `options` gets a new array each time a category finishes
    // loading, so keying on it would snap the user's selection back to the top
    // mid-navigation as emoji data streams in.
    useEffect(() => {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional centralized derivation, keyed on the query token
      setSelectedIndex(0);
    }, [query]);

    useEffect(() => {
      if (!listRef.current) return;
      const selected = listRef.current.children[selectedIndex] as HTMLElement | undefined;
      selected?.scrollIntoView({ block: 'nearest' });
    }, [selectedIndex]);

    const handleSelect = useCallback(
      (emoji: EmojiEntry) => {
        if (!query) return;
        // Insert the glyph + a trailing space (matches picker-insertion behavior).
        onSelect(emoji.e + ' ', query.startIndex);
      },
      [query, onSelect]
    );

    // Keyboard navigation (called from MessageInput's onKeyDown, returns true
    // when the key was consumed).
    const handleKeyDown = useCallback(
      (e: React.KeyboardEvent): boolean =>
        handleTypeaheadKeyDown(
          e,
          optionsRef.current.length,
          selectedIndex,
          setSelectedIndex,
          (i) => handleSelect(optionsRef.current[i]),
          onClose
        ),
      [selectedIndex, handleSelect, onClose]
    );

    useImperativeHandle(ref, () => ({ handleKeyDown }), [handleKeyDown]);

    if (!query || options.length === 0) return null;

    return (
      <div
        className="emoji-autocomplete typeahead-autocomplete"
        role="listbox"
        aria-label="Emoji suggestions"
        aria-activedescendant={options[selectedIndex] ? `emoji-opt-${selectedIndex}` : undefined}
        tabIndex={-1}
        ref={listRef}
      >
        {options.map((emoji, i) => {
          const code = emoji.c[0] ?? emoji.n;
          return (
            <div
              key={`${emoji.e}-${code}`}
              id={`emoji-opt-${i}`}
              className={`emoji-option typeahead-option ${i === selectedIndex ? 'selected' : ''}`}
              role="option"
              tabIndex={-1}
              aria-selected={i === selectedIndex}
              onMouseDown={(e) => {
                e.preventDefault(); // prevent textarea blur
                handleSelect(emoji);
              }}
              onMouseEnter={() => setSelectedIndex(i)}
            >
              <span className="emoji-option-glyph" aria-hidden="true">
                {emoji.e}
              </span>
              <span className="emoji-option-code">:{code}:</span>
            </div>
          );
        })}
      </div>
    );
  }
);

EmojiAutocomplete.displayName = 'EmojiAutocomplete';

export default EmojiAutocomplete;
