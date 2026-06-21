import { EmojiEntry, EmojiCategory } from './types';

// Module-level singleton — survives component unmount/remount
const categoryCache = new Map<string, EmojiEntry[]>();
let categoriesMeta: EmojiCategory[] | null = null;

export function getCachedCategory(id: string): EmojiEntry[] | undefined {
  return categoryCache.get(id);
}

export function setCachedCategory(id: string, data: EmojiEntry[]): void {
  categoryCache.set(id, data);
}

export function isCategoryCached(id: string): boolean {
  return categoryCache.has(id);
}

export function getAllCachedCategories(): Map<string, EmojiEntry[]> {
  return categoryCache;
}

export function getCategoriesMeta(): EmojiCategory[] | null {
  return categoriesMeta;
}

export function setCategoriesMeta(meta: EmojiCategory[]): void {
  categoriesMeta = meta;
}

// Frequently used emoji persistence
const RECENT_KEY = 'concord-emoji-recent';
const MAX_RECENT = 32;

interface RecentEntry {
  emoji: string;
  count: number;
  lastUsed: number;
}

export function getRecentEmojis(): string[] {
  try {
    const raw = localStorage.getItem(RECENT_KEY);
    if (!raw) return [];
    const entries: RecentEntry[] = JSON.parse(raw);
    return [...entries].sort((a, b) => b.lastUsed - a.lastUsed).map((e) => e.emoji);
  } catch {
    return [];
  }
}

export function addRecentEmoji(emoji: string): void {
  try {
    const raw = localStorage.getItem(RECENT_KEY);
    const entries: RecentEntry[] = raw ? JSON.parse(raw) : [];
    const existing = entries.find((e) => e.emoji === emoji);
    if (existing) {
      existing.count++;
      existing.lastUsed = Date.now();
    } else {
      entries.push({ emoji, count: 1, lastUsed: Date.now() });
    }
    // Keep only the most recent entries
    entries.sort((a, b) => b.lastUsed - a.lastUsed);
    localStorage.setItem(RECENT_KEY, JSON.stringify(entries.slice(0, MAX_RECENT)));
  } catch {
    // Ignore localStorage errors
  }
}

// Skin tone persistence
const SKIN_TONE_KEY = 'concord-emoji-skin-tone';

export function getSavedSkinTone(): string {
  try {
    return localStorage.getItem(SKIN_TONE_KEY) || '';
  } catch {
    return '';
  }
}

export function saveSkinTone(tone: string): void {
  try {
    localStorage.setItem(SKIN_TONE_KEY, tone);
  } catch {
    // Ignore localStorage errors
  }
}
