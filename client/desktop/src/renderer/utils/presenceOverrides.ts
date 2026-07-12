import { z } from 'zod';

const MAX_EXCLUDED_USERS = 1_000;
const INVALID_DOCUMENT_ERROR = 'Invalid presence override document';

const PresenceOverridesSchema = z.strictObject({
  v: z.literal(1),
  excludedUserIds: z.array(z.string().uuid()).max(MAX_EXCLUDED_USERS),
});

export interface PresenceOverridesDocument {
  v: 1;
  excludedUserIds: string[];
}

export function comparePresenceOverrideUserIds(left: string, right: string): number {
  if (left === right) return 0;
  return left < right ? -1 : 1;
}

export function parsePresenceOverrides(raw: unknown): PresenceOverridesDocument {
  const parsed = PresenceOverridesSchema.safeParse(raw);
  if (!parsed.success) {
    throw new Error(INVALID_DOCUMENT_ERROR);
  }

  const excludedUserIds = [...new Set(parsed.data.excludedUserIds.map((id) => id.toLowerCase()))];
  excludedUserIds.sort(comparePresenceOverrideUserIds);
  return { v: 1, excludedUserIds };
}
