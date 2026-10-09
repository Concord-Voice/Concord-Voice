import type { ServerMember } from '../../stores/chat/memberStore';

export function memberAutocomplete(
  members: ServerMember[],
  query: string
): { isActive: boolean; members: ServerMember[] } {
  const prefix = query.trim().toLowerCase();
  if (Array.from(prefix).length < 3) return { isActive: false, members };

  const visibleName = (member: ServerMember) => member.display_name || member.username;

  return {
    isActive: true,
    members: members
      .filter(
        (member) =>
          member.username.toLowerCase().startsWith(prefix) ||
          (member.display_name?.toLowerCase().startsWith(prefix) ?? false)
      )
      .sort(
        (a, b) =>
          visibleName(a).localeCompare(visibleName(b), undefined, { sensitivity: 'base' }) ||
          a.username.localeCompare(b.username, undefined, { sensitivity: 'base' }) ||
          a.user_id.localeCompare(b.user_id)
      ),
  };
}
