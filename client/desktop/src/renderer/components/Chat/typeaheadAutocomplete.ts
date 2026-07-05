import type React from 'react';

/**
 * An active typeahead trigger token at the cursor — e.g. the `@name` of a
 * mention or the `:code` of an emoji shortcode.
 */
export interface TriggerToken {
  /** The lowercased query text between the trigger char and the cursor. */
  text: string;
  /** Index of the trigger character in the source string. */
  startIndex: number;
}

/** Token boundary: space, newline, or tab. */
const isWhitespace = (ch: string | undefined): boolean => ch === ' ' || ch === '\n' || ch === '\t';

/**
 * Extract the active `<trigger><query>` token ending at `cursorPosition`.
 *
 * Walks backwards from the cursor to the nearest trigger char, stopping at
 * whitespace (space, newline, or tab). The trigger only counts when it sits at
 * the start of the input or immediately after whitespace, so `3:30`, `http://`,
 * and `email@host` never trigger. Returns `null` when there is no active token.
 *
 * Shared by MentionAutocomplete (`@`), EmojiAutocomplete (`:`), and the
 * MessageInput detection effects so the trigger semantics stay identical.
 */
export function extractTriggerToken(
  text: string,
  cursorPosition: number,
  trigger: string
): TriggerToken | null {
  let i = cursorPosition - 1;
  while (i >= 0 && text[i] !== trigger && !isWhitespace(text[i])) {
    i--;
  }
  if (i < 0 || text[i] !== trigger) return null;
  // Trigger must be at start-of-input or preceded by whitespace.
  if (i > 0 && !isWhitespace(text[i - 1])) return null;
  return {
    text: text.slice(i + 1, cursorPosition).toLowerCase(),
    startIndex: i,
  };
}

/**
 * Shared keyboard navigation for a typeahead popover. Returns `true` when the
 * key was consumed (so the caller stops further handling). Arrows move the
 * highlight with wraparound, Enter/Tab commit the current index, Escape closes.
 *
 * Shared by MentionAutocomplete and EmojiAutocomplete so both popovers honor
 * the same nav-key contract.
 */
export function handleTypeaheadKeyDown(
  e: React.KeyboardEvent,
  optionCount: number,
  selectedIndex: number,
  setSelectedIndex: React.Dispatch<React.SetStateAction<number>>,
  onCommit: (index: number) => void,
  onClose: () => void
): boolean {
  if (optionCount === 0) return false;
  switch (e.key) {
    case 'ArrowDown':
      e.preventDefault();
      setSelectedIndex((i) => (i + 1) % optionCount);
      return true;
    case 'ArrowUp':
      e.preventDefault();
      setSelectedIndex((i) => (i - 1 + optionCount) % optionCount);
      return true;
    case 'Enter':
    case 'Tab':
      e.preventDefault();
      onCommit(selectedIndex);
      return true;
    case 'Escape':
      e.preventDefault();
      onClose();
      return true;
    default:
      return false;
  }
}
