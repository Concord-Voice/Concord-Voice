import { describe, it, expect, beforeEach } from 'vitest';
import { useChangelogStore } from '../../../src/renderer/stores/ui/changelogStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('changelogStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('defaults lastSeenVersion to null', () => {
    expect(useChangelogStore.getState().lastSeenVersion).toBeNull();
  });

  it('markSeen records the version', () => {
    useChangelogStore.getState().markSeen('0.2.21');
    expect(useChangelogStore.getState().lastSeenVersion).toBe('0.2.21');
  });

  it('persists under the concord-changelog key', () => {
    useChangelogStore.getState().markSeen('0.2.21');
    const raw = localStorage.getItem('concord-changelog');
    expect(raw).not.toBeNull();
    expect(JSON.parse(raw as string).state.lastSeenVersion).toBe('0.2.21');
  });
});
