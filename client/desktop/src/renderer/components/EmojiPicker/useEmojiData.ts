import { useState, useEffect, useCallback } from 'react';
import { EmojiEntry, EmojiCategory } from './types';
import { errorMessage } from '../../utils/redactError';
import {
  getCachedCategory,
  setCachedCategory,
  isCategoryCached,
  getCategoriesMeta,
  setCategoriesMeta,
  getAllCachedCategories,
} from './emojiDataCache';
import { indexCategory } from './shortcodeIndex';

// Eager import for categories metadata (tiny file, always needed)
import categoriesMeta from '../../data/emoji/categories.json';

// Define lazy importers at module scope so Vite can statically analyze them.
// Each arrow function returns a dynamic import() that Vite will code-split.
const categoryLoaders: Record<string, () => Promise<{ default: EmojiEntry[] }>> = {
  smileys: () => import('../../data/emoji/smileys.json'),
  people: () => import('../../data/emoji/people.json'),
  animals: () => import('../../data/emoji/animals.json'),
  food: () => import('../../data/emoji/food.json'),
  travel: () => import('../../data/emoji/travel.json'),
  activities: () => import('../../data/emoji/activities.json'),
  objects: () => import('../../data/emoji/objects.json'),
  symbols: () => import('../../data/emoji/symbols.json'),
  flags: () => import('../../data/emoji/flags.json'),
};

// Shortcode-only search: every loaded emoji whose `c` array contains a code
// that starts with the (already-lowercased) prefix. Extracted to module scope
// so the `search` callback stays below Sonar's cognitive-complexity budget.
function searchByCodePrefix(cache: Map<string, EmojiEntry[]>, codeQuery: string): EmojiEntry[] {
  const results: EmojiEntry[] = [];
  for (const [, emojis] of cache) {
    for (const emoji of emojis) {
      if (emoji.c.some((code) => code.startsWith(codeQuery))) {
        results.push(emoji);
      }
    }
  }
  return results;
}

// Free-text search: match by `n` substring OR `c` prefix. Both branches
// coexist so bare "thumbsu" still finds 👍 via its "thumbsup" shortcode.
function searchByNameOrCode(cache: Map<string, EmojiEntry[]>, lower: string): EmojiEntry[] {
  const results: EmojiEntry[] = [];
  for (const [, emojis] of cache) {
    for (const emoji of emojis) {
      if (emoji.n.toLowerCase().includes(lower) || emoji.c.some((code) => code.startsWith(lower))) {
        results.push(emoji);
      }
    }
  }
  return results;
}

export function useEmojiData() {
  const [categories, setCategories] = useState<EmojiCategory[]>(
    () => getCategoriesMeta() || categoriesMeta
  );
  const [loadedCategories, setLoadedCategories] = useState<Set<string>>(
    () => new Set(getAllCachedCategories().keys())
  );
  const [loadingCategory, setLoadingCategory] = useState<string | null>(null);

  // Store categories metadata on first mount
  useEffect(() => {
    if (!getCategoriesMeta()) {
      setCategoriesMeta(categoriesMeta);
    }
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: populates categories list on mount from cached metadata; not a render loop
    setCategories(categoriesMeta);
  }, []);

  // Load a specific category's emoji data
  const loadCategory = useCallback(async (categoryId: string): Promise<EmojiEntry[]> => {
    // Return cached if available
    const cached = getCachedCategory(categoryId);
    if (cached) return cached;

    const loader = categoryLoaders[categoryId];
    if (!loader) return [];

    setLoadingCategory(categoryId);
    try {
      const mod = await loader();
      const data: EmojiEntry[] = mod.default;
      setCachedCategory(categoryId, data);
      indexCategory(categoryId, data);
      setLoadedCategories((prev) => new Set(prev).add(categoryId));
      return data;
    } catch (err) {
      console.error('Failed to load emoji category:', categoryId, errorMessage(err));
      return [];
    } finally {
      setLoadingCategory(null);
    }
  }, []);

  // Get emoji data for a category (returns cached or empty)
  const getCategory = useCallback((categoryId: string): EmojiEntry[] => {
    return getCachedCategory(categoryId) || [];
  }, []);

  // Search across all loaded categories.
  // A leading colon scopes the query to shortcode prefixes only (e.g. ":sm"
  // matches emoji whose `c` array contains a code starting with "sm").
  // Otherwise the query matches by free-text name OR shortcode prefix so a
  // bare "thumbsu" still finds 👍 via its "thumbsup" shortcode.
  const search = useCallback(
    (query: string): EmojiEntry[] => {
      if (!query.trim()) return [];
      const lower = query.toLowerCase();
      const cache = getAllCachedCategories();

      if (lower.startsWith(':')) {
        const codeQuery = lower.slice(1);
        if (!codeQuery) return [];
        return searchByCodePrefix(cache, codeQuery);
      }

      return searchByNameOrCode(cache, lower);
    },
    // The callback closes over no React state. `getAllCachedCategories()` is a
    // module-level function that reads module-level cache; `searchByCodePrefix`
    // and `searchByNameOrCode` are module-scope helpers. A fresh call always
    // sees the latest cache, so the callback's identity can stay stable.
    []
  );

  // Load all categories for comprehensive search
  const loadAllForSearch = useCallback(async () => {
    if (!categories.length) return;
    const unloaded = categories.filter((c) => !isCategoryCached(c.id));
    await Promise.all(unloaded.map((c) => loadCategory(c.id)));
  }, [categories, loadCategory]);

  return {
    categories,
    loadedCategories,
    loadingCategory,
    loadCategory,
    getCategory,
    search,
    loadAllForSearch,
  };
}
