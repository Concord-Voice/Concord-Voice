import { createStore } from '../utils/createStore';

export type SectionKey = 'pending' | 'online' | 'offline';
export const BUILTIN_SECTION_ORDER: SectionKey[] = ['pending', 'online', 'offline'];

export interface FriendCategory {
  id: string; // "cat_<uuid>"
  name: string;
  emoji: string; // unicode or ""
  color: string | null; // hex or null
  memberIds: string[]; // friend userIds; disjoint across categories (one-per-friend)
}

export interface FriendOrgBlob {
  v: 1;
  categories: FriendCategory[];
  sectionOrder: string[]; // mix of cat_* ids + built-in keys
}

interface FriendOrgState {
  categories: FriendCategory[];
  sectionOrder: string[];
  createCategory: (name: string, emoji: string, color: string | null) => string;
  renameCategory: (id: string, name: string) => void;
  setCategoryStyle: (id: string, style: { emoji?: string; color?: string | null }) => void;
  deleteCategory: (id: string) => void;
  assignFriend: (friendUserId: string, categoryId: string | null) => void;
  reorderSections: (order: string[]) => void;
  pruneFriends: (validFriendUserIds: string[]) => void;
  _hydrate: (blob: FriendOrgBlob) => void;
}

const newId = (): string => `cat_${crypto.randomUUID()}`;

// Pure category transforms, extracted to module scope so the store's set() updaters
// stay shallow (keeps arrow nesting under the 4-level limit — Sonar S2004) and so the
// one-per-friend / prune logic is independently testable.
function reassignMember(
  categories: FriendCategory[],
  friendUserId: string,
  categoryId: string | null
): FriendCategory[] {
  return categories.map((c) => {
    const without = c.memberIds.filter((m) => m !== friendUserId); // remove from any prior (one-per-friend)
    if (c.id === categoryId) return { ...c, memberIds: [...without, friendUserId] };
    return without.length === c.memberIds.length ? c : { ...c, memberIds: without };
  });
}

function pruneMembers(categories: FriendCategory[], keep: Set<string>): FriendCategory[] {
  return categories.map((c) => ({ ...c, memberIds: c.memberIds.filter((m) => keep.has(m)) }));
}

export const useFriendOrgStore = createStore<FriendOrgState>()((set) => ({
  categories: [],
  sectionOrder: [],

  createCategory: (name, emoji, color) => {
    const id = newId();
    set((s) => ({
      categories: [...s.categories, { id, name, emoji, color, memberIds: [] }],
      sectionOrder: [...s.sectionOrder, id],
    }));
    return id;
  },

  renameCategory: (id, name) =>
    set((s) => ({ categories: s.categories.map((c) => (c.id === id ? { ...c, name } : c)) })),

  setCategoryStyle: (id, style) =>
    set((s) => ({
      categories: s.categories.map((c) =>
        c.id === id
          ? {
              ...c,
              emoji: style.emoji ?? c.emoji,
              color: style.color === undefined ? c.color : style.color,
            }
          : c
      ),
    })),

  deleteCategory: (id) =>
    set((s) => ({
      categories: s.categories.filter((c) => c.id !== id), // members drop with the category → uncategorized
      sectionOrder: s.sectionOrder.filter((k) => k !== id), // atomic: remove its order entry too
    })),

  assignFriend: (friendUserId, categoryId) =>
    set((s) => ({ categories: reassignMember(s.categories, friendUserId, categoryId) })),

  reorderSections: (order) => set({ sectionOrder: order }),

  pruneFriends: (valid) => {
    const keep = new Set(valid);
    set((s) => ({ categories: pruneMembers(s.categories, keep) }));
  },

  _hydrate: (blob) => set({ categories: blob.categories, sectionOrder: blob.sectionOrder }),
}));
