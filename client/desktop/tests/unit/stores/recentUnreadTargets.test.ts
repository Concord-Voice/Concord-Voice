import { describe, it, expect, beforeEach } from 'vitest';
import {
  useUnreadStore,
  pushRecentUnreadTarget,
  removeRecentUnreadTarget,
  RECENT_UNREAD_TARGETS_MAX,
} from '@/renderer/stores/chat/unreadStore';
import { useDMStore } from '@/renderer/stores/chat/dmStore';

const ids = () => useUnreadStore.getState().recentUnreadTargets.map((t) => t.targetId);

describe('recentUnreadTargets (tray LIFO stack)', () => {
  beforeEach(() => {
    useUnreadStore.getState().clearAll();
    useDMStore.setState({ conversations: [{ id: 'd1', unreadCount: 0 }] } as never);
  });

  it('pushes the newest to the front', () => {
    useUnreadStore.getState().incrementUnread('a');
    useUnreadStore.getState().bumpChannelUnread('b', 's1');
    useDMStore.getState().incrementUnread('d1');
    expect(ids()).toEqual(['d1', 'b', 'a']);
    expect(useUnreadStore.getState().recentUnreadTargets[0]).toEqual({
      type: 'dm',
      targetId: 'd1',
    });
    expect(useUnreadStore.getState().recentUnreadTargets[1]).toEqual({
      type: 'channel',
      targetId: 'b',
      serverId: 's1',
    });
  });

  it('records the active snapshot server for incrementUnread when known', () => {
    useUnreadStore.getState().setInitialUnreads(new Map(), 's9');
    useUnreadStore.getState().incrementUnread('a');
    expect(useUnreadStore.getState().recentUnreadTargets[0]).toEqual({
      type: 'channel',
      targetId: 'a',
      serverId: 's9',
    });
  });

  it('re-bump moves to the front without duplicating', () => {
    useUnreadStore.getState().incrementUnread('a');
    useUnreadStore.getState().incrementUnread('b');
    useUnreadStore.getState().incrementUnread('a');
    expect(ids()).toEqual(['a', 'b']);
  });

  it('removes a read conversation (channel and DM)', () => {
    useUnreadStore.getState().incrementUnread('a');
    useUnreadStore.getState().incrementUnread('b');
    useDMStore.getState().incrementUnread('d1');
    useUnreadStore.getState().clearUnread('a');
    useDMStore.getState().clearUnread('d1');
    expect(ids()).toEqual(['b']);
  });

  it('clearUnread on an entry with no counts still drops the stack entry', () => {
    useUnreadStore.getState().incrementUnread('a');
    useUnreadStore.setState({ unreadCounts: new Map() });
    useUnreadStore.getState().clearUnread('a');
    expect(ids()).toEqual([]);
  });

  it('clearing something absent leaves the store untouched', () => {
    useUnreadStore.getState().incrementUnread('a');
    const before = useUnreadStore.getState().recentUnreadTargets;
    useDMStore.getState().clearUnread('nope');
    useUnreadStore.getState().clearUnread('nope');
    expect(useUnreadStore.getState().recentUnreadTargets).toBe(before);
  });

  it('caps at 20, dropping the oldest', () => {
    for (let i = 0; i < 25; i++) useUnreadStore.getState().incrementUnread(`c${i}`);
    expect(ids()).toHaveLength(RECENT_UNREAD_TARGETS_MAX);
    expect(ids()[0]).toBe('c24');
    expect(ids().at(-1)).toBe('c5');
  });

  it('clearAll empties it', () => {
    useUnreadStore.getState().incrementUnread('a');
    useUnreadStore.getState().clearAll();
    expect(ids()).toEqual([]);
  });

  it('helpers are pure', () => {
    const list = [{ type: 'dm' as const, targetId: 'x' }];
    expect(pushRecentUnreadTarget(list, { type: 'dm', targetId: 'y' })).toHaveLength(2);
    expect(list).toHaveLength(1);
    expect(removeRecentUnreadTarget(list, 'zzz')).toBe(list);
  });
});
