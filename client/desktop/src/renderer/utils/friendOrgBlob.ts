import { z } from 'zod';
import {
  BUILTIN_SECTION_ORDER,
  type FriendOrgBlob,
  type FriendCategory,
} from '../stores/friendOrgStore';

export const EMPTY_FRIEND_ORG: FriendOrgBlob = { v: 1, categories: [], sectionOrder: [] };

const CategorySchema = z.object({
  id: z.string().regex(/^cat_/),
  name: z.string(),
  emoji: z.string(),
  color: z.string().nullable(),
  memberIds: z.array(z.string()),
});
const BlobSchema = z.object({
  v: z.literal(1),
  categories: z.array(CategorySchema),
  sectionOrder: z.array(z.string()),
});

const BUILTINS = new Set<string>(BUILTIN_SECTION_ORDER);

/**
 * Decrypt-time trust-boundary guard (NEW vs preferencesSync/savedGifsSync, which JSON.parse-as-T).
 * Cross-device LWW can hydrate a blob authored elsewhere that the local write-path never vetted.
 * Returns a repaired blob, or EMPTY_FRIEND_ORG on a structural / one-per-friend violation.
 */
export function validateFriendOrgBlob(raw: unknown): FriendOrgBlob {
  const parsed = BlobSchema.safeParse(raw);
  if (!parsed.success) return EMPTY_FRIEND_ORG;
  const { categories, sectionOrder } = parsed.data;

  // one-per-friend disjointness: a userId may appear in at most one category.
  const seen = new Set<string>();
  for (const c of categories as FriendCategory[]) {
    for (const m of c.memberIds) {
      if (seen.has(m)) return EMPTY_FRIEND_ORG; // hard violation → safe empty
      seen.add(m);
    }
  }

  // prune orphan cat_* ids (no matching category); keep known cat ids + built-in keys.
  const knownCat = new Set(categories.map((c) => c.id));
  const cleanOrder = sectionOrder.filter((k) => knownCat.has(k) || BUILTINS.has(k));

  return { v: 1, categories, sectionOrder: cleanOrder };
}
