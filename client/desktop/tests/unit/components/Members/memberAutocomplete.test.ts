import { memberAutocomplete } from '@/renderer/components/Members/memberAutocomplete';
import type { ServerMember } from '@/renderer/stores/chat/memberStore';

const member = (
  userId: string,
  username: string,
  displayName?: string,
  roleName?: string
): ServerMember => ({
  user_id: userId,
  username,
  display_name: displayName,
  role: 'member',
  joined_at: '2025-01-01T00:00:00Z',
  roles: roleName ? [{ role_id: 'r1', role_name: roleName, position: 1 }] : [],
});

describe('memberAutocomplete', () => {
  const members = [
    member('u1', 'alice', 'Alice'),
    member('u2', 'bob', 'Bob', 'Aliens'),
    member('u3', 'alicez', 'Zed'),
    member('u4', 'other', 'Alicia'),
    member('u5', 'xalice', 'Xalice'),
  ];

  it('keeps the full roster and its order until three characters are typed', () => {
    for (const query of ['', 'a', 'Al', ' al ', '🎉a']) {
      const result = memberAutocomplete(members, query);
      expect(result.isActive).toBe(false);
      expect(result.members).toBe(members);
    }
  });

  it('matches only username or display-name prefixes and sorts by the visible name', () => {
    const result = memberAutocomplete(members, ' ALI ');
    expect(result.isActive).toBe(true);
    expect(result.members.map((entry) => entry.user_id)).toEqual(['u1', 'u4', 'u3']);
    expect(members.map((entry) => entry.user_id)).toEqual(['u1', 'u2', 'u3', 'u4', 'u5']);
  });

  it('returns an empty result once an active prefix has no matches', () => {
    expect(memberAutocomplete(members, 'none').members).toEqual([]);
  });
});
