import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('unreadStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  describe('setInitialUnreads', () => {
    it('sets initial counts from a map', () => {
      const counts = new Map([
        ['channel-1', 5],
        ['channel-2', 3],
      ]);
      useUnreadStore.getState().setInitialUnreads(counts);
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(5);
      expect(useUnreadStore.getState().unreadCounts.get('channel-2')).toBe(3);
    });
  });

  describe('incrementUnread', () => {
    it('increments from zero', () => {
      useUnreadStore.getState().incrementUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(1);
    });

    it('increments existing count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().incrementUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(6);
    });
  });

  describe('clearUnread', () => {
    it('removes a channel count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().clearUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });
  });

  describe('setUnreadCount', () => {
    it('removes entry when count is zero', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-1', 0);
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
    });
  });

  describe('serverUnreadSet', () => {
    it('tracks servers with unread messages', () => {
      useUnreadStore.getState().setInitialServerUnreads(['server-1', 'server-2']);
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
    });

    it('marks and clears server unreads', () => {
      useUnreadStore.getState().markServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(true);
      useUnreadStore.getState().clearServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
    });
  });

  describe('clearAll', () => {
    it('clears all channel unreads', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-2', 3);
      useUnreadStore.getState().clearAll();
      expect(useUnreadStore.getState().unreadCounts.size).toBe(0);
    });
  });
});
