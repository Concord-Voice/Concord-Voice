import { describe, it, expect } from 'vitest';
import { validateFriendOrgBlob, EMPTY_FRIEND_ORG } from '@/renderer/utils/friendOrgBlob';

describe('validateFriendOrgBlob', () => {
  it('accepts a well-formed blob', () => {
    const blob = {
      v: 1,
      categories: [{ id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: ['u1'] }],
      sectionOrder: ['cat_1', 'online'],
    };
    expect(validateFriendOrgBlob(blob)).toEqual(blob);
  });
  it('falls back to empty on overlapping memberIds (one-per-friend violation)', () => {
    const blob = {
      v: 1,
      categories: [
        { id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: ['u1'] },
        { id: 'cat_2', name: 'B', emoji: '', color: null, memberIds: ['u1'] }, // u1 in two cats
      ],
      sectionOrder: ['cat_1', 'cat_2'],
    };
    expect(validateFriendOrgBlob(blob)).toEqual(EMPTY_FRIEND_ORG);
  });
  it('falls back to empty on a wrong-shaped / oversize / unknown blob', () => {
    expect(validateFriendOrgBlob({ v: 2 })).toEqual(EMPTY_FRIEND_ORG);
    expect(validateFriendOrgBlob(null)).toEqual(EMPTY_FRIEND_ORG);
    expect(validateFriendOrgBlob({ v: 1, categories: 'nope', sectionOrder: [] })).toEqual(
      EMPTY_FRIEND_ORG
    );
  });
  it('drops orphan cat_* ids from sectionOrder', () => {
    const blob = {
      v: 1,
      categories: [{ id: 'cat_1', name: 'A', emoji: '', color: null, memberIds: [] }],
      sectionOrder: ['cat_1', 'cat_GONE', 'online'],
    };
    expect(validateFriendOrgBlob(blob).sectionOrder).toEqual(['cat_1', 'online']);
  });
});
