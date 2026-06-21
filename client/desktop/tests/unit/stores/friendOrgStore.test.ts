import { describe, it, expect, beforeEach } from 'vitest';
import { useFriendOrgStore } from '@/renderer/stores/friendOrgStore';

const reset = () =>
  useFriendOrgStore.getState()._hydrate({ v: 1, categories: [], sectionOrder: [] });

describe('friendOrgStore', () => {
  beforeEach(reset);

  it('createCategory adds a category and appends it to sectionOrder', () => {
    const id = useFriendOrgStore.getState().createCategory('Close Friends', '💜', '#fa709a');
    const s = useFriendOrgStore.getState();
    expect(s.categories).toHaveLength(1);
    expect(s.categories[0]).toMatchObject({
      id,
      name: 'Close Friends',
      emoji: '💜',
      color: '#fa709a',
      memberIds: [],
    });
    expect(s.sectionOrder).toContain(id);
  });

  it('assignFriend enforces one-per-friend (moves from A to B)', () => {
    const a = useFriendOrgStore.getState().createCategory('A', '', null);
    const b = useFriendOrgStore.getState().createCategory('B', '', null);
    useFriendOrgStore.getState().assignFriend('u1', a);
    useFriendOrgStore.getState().assignFriend('u1', b);
    const cats = useFriendOrgStore.getState().categories;
    expect(cats.find((c) => c.id === a)!.memberIds).not.toContain('u1');
    expect(cats.find((c) => c.id === b)!.memberIds).toContain('u1');
  });

  it('assignFriend(null) unassigns the friend from all categories', () => {
    const a = useFriendOrgStore.getState().createCategory('A', '', null);
    useFriendOrgStore.getState().assignFriend('u1', a);
    useFriendOrgStore.getState().assignFriend('u1', null);
    expect(useFriendOrgStore.getState().categories[0].memberIds).not.toContain('u1');
  });

  it('deleteCategory removes it from categories AND sectionOrder atomically; members become uncategorized', () => {
    const a = useFriendOrgStore.getState().createCategory('A', '', null);
    useFriendOrgStore.getState().assignFriend('u1', a);
    useFriendOrgStore.getState().deleteCategory(a);
    const s = useFriendOrgStore.getState();
    expect(s.categories.find((c) => c.id === a)).toBeUndefined();
    expect(s.sectionOrder).not.toContain(a);
    // u1 is now in no category (uncategorized)
    expect(s.categories.some((c) => c.memberIds.includes('u1'))).toBe(false);
  });

  it('pruneFriends drops stale memberIds for non-friends', () => {
    const a = useFriendOrgStore.getState().createCategory('A', '', null);
    useFriendOrgStore.getState().assignFriend('u1', a);
    useFriendOrgStore.getState().assignFriend('u2', a);
    useFriendOrgStore.getState().pruneFriends(['u1']); // u2 unfriended
    expect(useFriendOrgStore.getState().categories[0].memberIds).toEqual(['u1']);
  });
});
