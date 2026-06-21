import type { EmojiEntry } from './types';
// Eagerly seed the most common shortcodes at module import so the markdown
// pipeline doesn't silently miss `:smile:` etc. before the picker opens.
// Only the smileys category — the others are lazy-loaded when the picker
// opens. Uses a synchronous JSON import so it's safe in module-init order.
import smileys from '../../data/emoji/smileys.json';

/**
 * Module-level lazy reverse index mapping `shortcode → emoji`.
 * Populated one category at a time as the picker loads categories.
 * First-insertion wins on duplicate shortcodes across categories.
 */
const shortcodeToEmoji = new Map<string, string>();
const indexedCategories = new Set<string>();

export function indexCategory(categoryId: string, entries: EmojiEntry[]): void {
  if (indexedCategories.has(categoryId)) return;
  indexedCategories.add(categoryId);
  for (const entry of entries) {
    for (const code of entry.c) {
      if (!shortcodeToEmoji.has(code)) {
        shortcodeToEmoji.set(code, entry.e);
      }
    }
  }
}

export function lookupShortcode(code: string): string | undefined {
  return shortcodeToEmoji.get(code);
}

export function isShortcodeKnown(code: string): boolean {
  return shortcodeToEmoji.has(code);
}

export function isCategoryIndexed(categoryId: string): boolean {
  return indexedCategories.has(categoryId);
}

/** Test helper. Not called in production code. */
export function resetShortcodeIndex(): void {
  shortcodeToEmoji.clear();
  indexedCategories.clear();
}

// Module-init side-effect: seed the smileys category synchronously so that
// `:smile:` and friends are known to the markdown pipeline even before the
// user opens the emoji picker. Other categories are still lazy-loaded by
// `useEmojiData.loadCategory()` on picker open.
indexCategory('smileys', smileys);
