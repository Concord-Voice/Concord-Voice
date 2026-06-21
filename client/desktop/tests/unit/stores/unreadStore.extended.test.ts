import { useUnreadStore } from '@/renderer/stores/unreadStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('unreadStore — extended coverage', () => {
  beforeEach(() => {
    resetAllStores();
  });

  // --- Mention tracking ---

  describe('incrementMention', () => {
    it('increments mention count from zero', () => {
      useUnreadStore.getState().incrementMention('channel-1');
      expect(useUnreadStore.getState().mentionCounts.get('channel-1')).toBe(1);
    });

    it('increments existing mention count', () => {
      useUnreadStore.getState().incrementMention('channel-1');
      useUnreadStore.getState().incrementMention('channel-1');
      expect(useUnreadStore.getState().mentionCounts.get('channel-1')).toBe(2);
    });
  });

  describe('clearMentions', () => {
    it('clears mention count for a channel', () => {
      useUnreadStore.getState().incrementMention('channel-1');
      useUnreadStore.getState().clearMentions('channel-1');
      expect(useUnreadStore.getState().mentionCounts.has('channel-1')).toBe(false);
    });

    it('returns same state when clearing non-existent mention', () => {
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().clearMentions('nonexistent');
      const stateAfter = useUnreadStore.getState();
      // mentionCounts reference should be the same (no unnecessary re-render)
      expect(stateAfter.mentionCounts).toBe(stateBefore.mentionCounts);
    });
  });

  // --- clearUnread also clears mentions ---

  describe('clearUnread with mentions', () => {
    it('clears both unread count and mention count for a channel', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().incrementMention('channel-1');
      useUnreadStore.getState().clearUnread('channel-1');
      expect(useUnreadStore.getState().unreadCounts.has('channel-1')).toBe(false);
      expect(useUnreadStore.getState().mentionCounts.has('channel-1')).toBe(false);
    });

    it('returns same state when clearing channel with no unreads or mentions', () => {
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().clearUnread('nonexistent');
      const stateAfter = useUnreadStore.getState();
      expect(stateAfter).toBe(stateBefore);
    });
  });

  // --- Server mention tracking ---

  describe('markServerMention', () => {
    it('adds server to mention set', () => {
      useUnreadStore.getState().markServerMention('server-1');
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(true);
    });

    it('does not duplicate when already present', () => {
      useUnreadStore.getState().markServerMention('server-1');
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().markServerMention('server-1');
      const stateAfter = useUnreadStore.getState();
      expect(stateAfter.serverMentionSet).toBe(stateBefore.serverMentionSet);
    });
  });

  describe('clearServerMention', () => {
    it('removes server from mention set', () => {
      useUnreadStore.getState().markServerMention('server-1');
      useUnreadStore.getState().clearServerMention('server-1');
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(false);
    });

    it('returns same state when server not in mention set', () => {
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().clearServerMention('nonexistent');
      const stateAfter = useUnreadStore.getState();
      expect(stateAfter.serverMentionSet).toBe(stateBefore.serverMentionSet);
    });
  });

  // --- clearServerUnread also clears server mentions ---

  describe('clearServerUnread with mentions', () => {
    it('clears both server unread and server mention', () => {
      useUnreadStore.getState().markServerUnread('server-1');
      useUnreadStore.getState().markServerMention('server-1');
      useUnreadStore.getState().clearServerUnread('server-1');
      expect(useUnreadStore.getState().serverUnreadSet.has('server-1')).toBe(false);
      expect(useUnreadStore.getState().serverMentionSet.has('server-1')).toBe(false);
    });

    it('returns same state when server has neither unreads nor mentions', () => {
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().clearServerUnread('nonexistent');
      const stateAfter = useUnreadStore.getState();
      expect(stateAfter).toBe(stateBefore);
    });
  });

  // --- markServerUnread idempotency ---

  describe('markServerUnread idempotency', () => {
    it('returns same state when server already in unread set', () => {
      useUnreadStore.getState().markServerUnread('server-1');
      const stateBefore = useUnreadStore.getState();
      useUnreadStore.getState().markServerUnread('server-1');
      const stateAfter = useUnreadStore.getState();
      expect(stateAfter.serverUnreadSet).toBe(stateBefore.serverUnreadSet);
    });
  });

  // --- clearAll clears everything ---

  describe('clearAll with mentions', () => {
    it('clears all channel unreads, mentions, server unreads, and server mentions', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().incrementMention('channel-1');
      useUnreadStore.getState().markServerUnread('server-1');
      useUnreadStore.getState().markServerMention('server-1');
      useUnreadStore.getState().clearAll();

      const state = useUnreadStore.getState();
      expect(state.unreadCounts.size).toBe(0);
      expect(state.mentionCounts.size).toBe(0);
      expect(state.serverUnreadSet.size).toBe(0);
      expect(state.serverMentionSet.size).toBe(0);
    });
  });

  // --- setUnreadCount edge cases ---

  describe('setUnreadCount positive', () => {
    it('sets a positive count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 10);
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(10);
    });

    it('overwrites an existing count', () => {
      useUnreadStore.getState().setUnreadCount('channel-1', 5);
      useUnreadStore.getState().setUnreadCount('channel-1', 10);
      expect(useUnreadStore.getState().unreadCounts.get('channel-1')).toBe(10);
    });
  });
});
