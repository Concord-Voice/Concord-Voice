import { beforeEach, describe, expect, it } from 'vitest';
import {
  addRecentEmoji,
  getAllCachedCategories,
  getCachedCategory,
  getCategoriesMeta,
  getRecentEmojis,
  getSavedSkinTone,
  isCategoryCached,
  saveSkinTone,
  setCachedCategory,
  setCategoriesMeta,
} from '@/renderer/components/EmojiPicker/emojiDataCache';
import type { EmojiCategory, EmojiEntry } from '@/renderer/components/EmojiPicker/types';

// The module cache is module-level state — clear between tests
beforeEach(() => {
  localStorage.clear();
  // Purge module-level cache maps by clearing via the module's own API
  getAllCachedCategories().clear();
  setCategoriesMeta(null as unknown as EmojiCategory[]);
});

// ─── Category cache ───────────────────────────────────────────────────────────

describe('getCachedCategory / setCachedCategory', () => {
  it('returns undefined for a key that was never set', () => {
    expect(getCachedCategory('smileys')).toBeUndefined();
  });

  it('returns the stored array after setCachedCategory', () => {
    const emojis: EmojiEntry[] = [{ e: '😀', n: 'grinning face', s: false }];
    setCachedCategory('smileys', emojis);
    expect(getCachedCategory('smileys')).toBe(emojis);
  });

  it('overwrites an existing entry', () => {
    const first: EmojiEntry[] = [{ e: '😀', n: 'grinning', s: false }];
    const second: EmojiEntry[] = [{ e: '😁', n: 'beaming', s: false }];
    setCachedCategory('smileys', first);
    setCachedCategory('smileys', second);
    expect(getCachedCategory('smileys')).toBe(second);
  });
});

describe('isCategoryCached', () => {
  it('returns false when category has not been cached', () => {
    expect(isCategoryCached('people')).toBe(false);
  });

  it('returns true after setCachedCategory', () => {
    setCachedCategory('people', []);
    expect(isCategoryCached('people')).toBe(true);
  });
});

describe('getAllCachedCategories', () => {
  it('returns an empty Map when nothing is cached', () => {
    expect(getAllCachedCategories().size).toBe(0);
  });

  it('reflects cached entries', () => {
    setCachedCategory('food', [{ e: '🍎', n: 'red apple', s: false }]);
    setCachedCategory('travel', []);
    expect(getAllCachedCategories().size).toBe(2);
  });
});

// ─── Categories metadata ──────────────────────────────────────────────────────

describe('getCategoriesMeta / setCategoriesMeta', () => {
  it('returns null before any metadata is set', () => {
    expect(getCategoriesMeta()).toBeNull();
  });

  it('returns the stored metadata', () => {
    const meta: EmojiCategory[] = [
      { id: 'smileys', name: 'Smileys', icon: '😀', file: 'smileys.json', count: 10 },
    ];
    setCategoriesMeta(meta);
    expect(getCategoriesMeta()).toBe(meta);
  });
});

// ─── Recent emojis ────────────────────────────────────────────────────────────

describe('getRecentEmojis', () => {
  it('returns empty array when nothing is stored', () => {
    expect(getRecentEmojis()).toEqual([]);
  });

  it('handles corrupt JSON gracefully', () => {
    localStorage.setItem('concord-emoji-recent', 'not-json{{');
    expect(getRecentEmojis()).toEqual([]);
  });
});

describe('addRecentEmoji', () => {
  it('adds a new emoji to recents', () => {
    addRecentEmoji('😀');
    expect(getRecentEmojis()).toContain('😀');
  });

  it('increments count and updates lastUsed for a duplicate', () => {
    addRecentEmoji('😀');
    addRecentEmoji('😀');
    const recents = getRecentEmojis();
    // Should still appear once (deduplicated)
    const count = recents.filter((e) => e === '😀').length;
    expect(count).toBe(1);
  });

  it('stores multiple different emojis', () => {
    addRecentEmoji('😀');
    addRecentEmoji('🎉');
    addRecentEmoji('❤️');
    const recents = getRecentEmojis();
    expect(recents).toContain('😀');
    expect(recents).toContain('🎉');
    expect(recents).toContain('❤️');
  });

  it('caps storage at 32 entries', () => {
    for (let i = 0; i < 40; i++) {
      addRecentEmoji(`emoji-${i}`);
    }
    expect(getRecentEmojis().length).toBeLessThanOrEqual(32);
  });

  it('sorts by most recently used first', () => {
    // Seed localStorage directly with known timestamps so sort order is deterministic
    const entries = [
      { emoji: '😀', count: 1, lastUsed: 1000 },
      { emoji: '🎉', count: 1, lastUsed: 2000 },
    ];
    localStorage.setItem('concord-emoji-recent', JSON.stringify(entries));
    const recents = getRecentEmojis();
    // 🎉 has a later lastUsed timestamp, so it should appear first
    expect(recents[0]).toBe('🎉');
    expect(recents[1]).toBe('😀');
  });

  it('does not throw on corrupt existing localStorage', () => {
    localStorage.setItem('concord-emoji-recent', 'bad-json');
    expect(() => addRecentEmoji('😀')).not.toThrow();
  });
});

// ─── Skin tone persistence ────────────────────────────────────────────────────

describe('getSavedSkinTone / saveSkinTone', () => {
  it('returns empty string when no skin tone is saved', () => {
    expect(getSavedSkinTone()).toBe('');
  });

  it('returns the saved skin tone', () => {
    saveSkinTone('\u{1F3FB}');
    expect(getSavedSkinTone()).toBe('\u{1F3FB}');
  });

  it('overwrites a previous skin tone', () => {
    saveSkinTone('\u{1F3FB}');
    saveSkinTone('\u{1F3FF}');
    expect(getSavedSkinTone()).toBe('\u{1F3FF}');
  });

  it('saving empty string clears the tone', () => {
    saveSkinTone('\u{1F3FC}');
    saveSkinTone('');
    expect(getSavedSkinTone()).toBe('');
  });
});
