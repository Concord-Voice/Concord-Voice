import { describe, it, expect, beforeEach } from 'vitest';
import {
  resetShortcodeIndex,
  indexCategory,
  lookupShortcode,
  isShortcodeKnown,
} from '@/renderer/components/EmojiPicker/shortcodeIndex';
import type { EmojiEntry } from '@/renderer/components/EmojiPicker/types';

const smileys: EmojiEntry[] = [
  { e: '😄', n: 'grinning face with smiling eyes', s: false, c: ['smile', 'grin'] },
  { e: '😢', n: 'crying face', s: false, c: ['cry'] },
];

describe('shortcodeIndex', () => {
  beforeEach(() => {
    resetShortcodeIndex();
  });

  it('looks up a shortcode after the category is indexed', () => {
    indexCategory('smileys', smileys);
    expect(lookupShortcode('smile')).toBe('😄');
    expect(lookupShortcode('grin')).toBe('😄');
    expect(lookupShortcode('cry')).toBe('😢');
  });

  it('returns undefined for unknown shortcodes', () => {
    indexCategory('smileys', smileys);
    expect(lookupShortcode('does_not_exist')).toBeUndefined();
  });

  it('first category wins on duplicate shortcodes across categories', () => {
    indexCategory('smileys', smileys);
    indexCategory('symbols', [
      { e: '❤️', n: 'red heart', s: false, c: ['smile'] },
    ]);
    expect(lookupShortcode('smile')).toBe('😄');
  });

  it('isShortcodeKnown reports false for unknown, true for known', () => {
    indexCategory('smileys', smileys);
    expect(isShortcodeKnown('smile')).toBe(true);
    expect(isShortcodeKnown('foo')).toBe(false);
  });

  it('resetShortcodeIndex clears all entries', () => {
    indexCategory('smileys', smileys);
    resetShortcodeIndex();
    expect(lookupShortcode('smile')).toBeUndefined();
  });

  it('reindexing the same category is idempotent', () => {
    indexCategory('smileys', smileys);
    indexCategory('smileys', smileys);
    expect(lookupShortcode('smile')).toBe('😄');
  });
});
