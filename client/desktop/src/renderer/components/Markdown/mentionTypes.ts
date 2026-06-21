// Leaf module for mention-token resolution: the MentionLookup type and the
// resolveMentionDisplay helper, with NO imports from Chat or Markdown
// components. Extracted from Chat/messageUtils.tsx to break the import cycle
// messageUtils -> Markdown/MarkdownContent -> Markdown/MentionChip ->
// messageUtils (module-graph hygiene). Both Markdown components and the Chat
// consumers depend on this leaf instead of each other.

// Token pattern matchers for resolving UUIDs to display names.
const USER_TOKEN_RE = /^<@([\w-]+)>$/;
const ROLE_TOKEN_RE = /^<@&([\w-]+)>$/;

/** Precomputed lookup maps for O(1) mention token resolution. */
export interface MentionLookup {
  users: Map<string, string>; // userId → display name
  roles: Map<string, string>; // roleId → role name
}

/** Resolve a mention token to a display string using precomputed lookup maps. */
export function resolveMentionDisplay(token: string, lookup: MentionLookup): string {
  // User token: <@userId> → @displayName
  const userMatch = USER_TOKEN_RE.exec(token);
  if (userMatch) {
    const display = lookup.users.get(userMatch[1]);
    return display ? `@${display}` : token;
  }
  // Role token: <@&roleId> → @roleName
  const roleMatch = ROLE_TOKEN_RE.exec(token);
  if (roleMatch) {
    const name = lookup.roles.get(roleMatch[1]);
    return name ? `@${name}` : token;
  }
  // Plain format (@username, @all, @here) — return as-is
  return token;
}
