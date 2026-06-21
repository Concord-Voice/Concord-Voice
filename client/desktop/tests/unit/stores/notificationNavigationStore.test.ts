import { describe, it, expect, beforeEach } from 'vitest';
import { useNotificationNavigationStore } from '../../../src/renderer/stores/notificationNavigationStore';

describe('notificationNavigationStore', () => {
  beforeEach(() => {
    useNotificationNavigationStore.setState({ pendingNavigation: null });
  });

  it('has null pending navigation by default', () => {
    const state = useNotificationNavigationStore.getState();
    expect(state.pendingNavigation).toBeNull();
  });

  it('sets pending navigation for channel', () => {
    useNotificationNavigationStore
      .getState()
      .setPendingNavigation({ type: 'channel', targetId: 'ch-1', serverId: 'srv-1' });

    const state = useNotificationNavigationStore.getState();
    expect(state.pendingNavigation).toEqual({
      type: 'channel',
      targetId: 'ch-1',
      serverId: 'srv-1',
    });
  });

  it('sets pending navigation for dm', () => {
    useNotificationNavigationStore
      .getState()
      .setPendingNavigation({ type: 'dm', targetId: 'conv-42' });

    const state = useNotificationNavigationStore.getState();
    expect(state.pendingNavigation).toEqual({
      type: 'dm',
      targetId: 'conv-42',
    });
  });

  it('clears pending navigation', () => {
    useNotificationNavigationStore
      .getState()
      .setPendingNavigation({ type: 'dm', targetId: 'conv-1' });

    useNotificationNavigationStore.getState().clearPendingNavigation();

    const state = useNotificationNavigationStore.getState();
    expect(state.pendingNavigation).toBeNull();
  });
});
