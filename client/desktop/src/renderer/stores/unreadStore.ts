import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

interface UnreadState {
  // Channel ID -> unread count (per-channel, active server only)
  unreadCounts: Map<string, number>;

  // Channel or DM conversation ID -> mention count (subset of unreadCounts where user was @mentioned)
  mentionCounts: Map<string, number>;

  // Server IDs that have any unread messages (cross-server)
  serverUnreadSet: Set<string>;

  // Server IDs that have unread mentions (cross-server, for priority badges)
  serverMentionSet: Set<string>;

  setInitialUnreads: (counts: Map<string, number>) => void;
  setUnreadCount: (channelId: string, count: number) => void;
  incrementUnread: (channelId: string) => void;
  clearUnread: (channelId: string) => void;
  clearAll: () => void;

  incrementMention: (channelId: string) => void;
  clearMentions: (channelId: string) => void;

  setInitialServerUnreads: (serverIds: string[]) => void;
  markServerUnread: (serverId: string) => void;
  clearServerUnread: (serverId: string) => void;
  markServerMention: (serverId: string) => void;
  clearServerMention: (serverId: string) => void;
}

export const useUnreadStore = wrapStore(create<UnreadState>()(
  devtools(
    (set) => ({
      unreadCounts: new Map(),
      mentionCounts: new Map(),
      serverUnreadSet: new Set(),
      serverMentionSet: new Set(),

      setInitialUnreads: (counts: Map<string, number>) => set({ unreadCounts: new Map(counts) }),

      setUnreadCount: (channelId: string, count: number) =>
        set((state) => {
          const next = new Map(state.unreadCounts);
          if (count > 0) {
            next.set(channelId, count);
          } else {
            next.delete(channelId);
          }
          return { unreadCounts: next };
        }),

      incrementUnread: (channelId: string) =>
        set((state) => {
          const current = state.unreadCounts.get(channelId) || 0;
          state.unreadCounts.set(channelId, current + 1);
          return { unreadCounts: new Map(state.unreadCounts) };
        }),

      clearUnread: (channelId: string) =>
        set((state) => {
          const hasUnread = state.unreadCounts.has(channelId);
          const hasMention = state.mentionCounts.has(channelId);
          if (!hasUnread && !hasMention) return state;
          const nextUnread = new Map(state.unreadCounts);
          const nextMention = new Map(state.mentionCounts);
          nextUnread.delete(channelId);
          nextMention.delete(channelId);
          return { unreadCounts: nextUnread, mentionCounts: nextMention };
        }),

      clearAll: () =>
        set({
          unreadCounts: new Map(),
          mentionCounts: new Map(),
          serverUnreadSet: new Set(),
          serverMentionSet: new Set(),
        }),

      incrementMention: (channelId: string) =>
        set((state) => {
          const current = state.mentionCounts.get(channelId) || 0;
          const next = new Map(state.mentionCounts);
          next.set(channelId, current + 1);
          return { mentionCounts: next };
        }),

      clearMentions: (channelId: string) =>
        set((state) => {
          if (state.mentionCounts.has(channelId)) {
            const next = new Map(state.mentionCounts);
            next.delete(channelId);
            return { mentionCounts: next };
          }
          return state;
        }),

      setInitialServerUnreads: (serverIds: string[]) =>
        set({ serverUnreadSet: new Set(serverIds) }),

      markServerUnread: (serverId: string) =>
        set((state) => {
          if (state.serverUnreadSet.has(serverId)) return state;
          const next = new Set(state.serverUnreadSet);
          next.add(serverId);
          return { serverUnreadSet: next };
        }),

      clearServerUnread: (serverId: string) =>
        set((state) => {
          const hadUnread = state.serverUnreadSet.has(serverId);
          const hadMention = state.serverMentionSet.has(serverId);
          if (!hadUnread && !hadMention) return state;
          const nextUnread = new Set(state.serverUnreadSet);
          const nextMention = new Set(state.serverMentionSet);
          nextUnread.delete(serverId);
          nextMention.delete(serverId);
          return { serverUnreadSet: nextUnread, serverMentionSet: nextMention };
        }),

      markServerMention: (serverId: string) =>
        set((state) => {
          if (state.serverMentionSet.has(serverId)) return state;
          const next = new Set(state.serverMentionSet);
          next.add(serverId);
          return { serverMentionSet: next };
        }),

      clearServerMention: (serverId: string) =>
        set((state) => {
          if (!state.serverMentionSet.has(serverId)) return state;
          const next = new Set(state.serverMentionSet);
          next.delete(serverId);
          return { serverMentionSet: next };
        }),
    }),
    { name: 'UnreadStore' }
  )
));
