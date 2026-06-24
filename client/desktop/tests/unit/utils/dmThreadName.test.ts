import { describe, it, expect } from 'vitest';
import { getThreadName } from '@/renderer/utils/dmThreadName';
import type { DMConversation } from '@/renderer/stores/dmStore';

const conv = (over: Partial<DMConversation>): DMConversation =>
  ({
    id: 'c1',
    isGroup: false,
    isPersonal: false,
    name: '',
    participants: [],
    ...over,
  }) as DMConversation;

describe('getThreadName (#1873)', () => {
  it('returns "Conversation" when undefined', () => {
    expect(getThreadName(undefined, 'me')).toBe('Conversation');
  });

  it('returns "Personal Thread" for a personal conversation', () => {
    expect(getThreadName(conv({ isPersonal: true }), 'me')).toBe('Personal Thread');
  });

  it('returns the group name for a named group', () => {
    expect(getThreadName(conv({ isGroup: true, name: 'Squad' }), 'me')).toBe('Squad');
  });

  it('falls back to joined participant names for an unnamed group', () => {
    const c = conv({
      isGroup: true,
      name: '',
      participants: [
        { userId: 'u2', username: 'bob', displayName: 'Bob' },
        { userId: 'u3', username: 'cara' },
      ] as never,
    });
    expect(getThreadName(c, 'me')).toBe('Bob, cara');
  });

  it('returns the other participant display name for a 1:1', () => {
    const c = conv({
      participants: [
        { userId: 'me', username: 'me' },
        { userId: 'u2', username: 'bob', displayName: 'Bob' },
      ] as never,
    });
    expect(getThreadName(c, 'me')).toBe('Bob');
  });
});
