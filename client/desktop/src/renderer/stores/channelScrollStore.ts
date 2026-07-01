/**
 * channelScrollStore — in-memory scroll position cache for MessageList.
 *
 * Used by both the server-channel and DM MessageList mounts to preserve the
 * user's scroll position across channel switches. Keyed on channelId OR
 * dmConversationId (callers decide — the store treats them as opaque strings).
 *
 * NOT persisted across app restarts: positions are session-scoped, since
 * saved scroll offsets only make sense against a live message cache.
 */
import { createStore } from '../utils/createStore';

export interface ChannelScrollState {
  positions: Record<string, number>;
  latestMessageIds: Record<string, string>;
  saveScroll: (id: string, scrollTop: number, latestMessageId?: string) => void;
  getScroll: (id: string) => number | undefined;
  getScrollLatestMessageId: (id: string) => string | undefined;
  clearScroll: (id: string) => void;
}

export const useChannelScrollStore = createStore<ChannelScrollState>()((set, get) => ({
  positions: {},
  latestMessageIds: {},
  saveScroll: (id, scrollTop, latestMessageId) =>
    set((state) => {
      const nextLatest = { ...state.latestMessageIds };
      if (latestMessageId) nextLatest[id] = latestMessageId;
      else delete nextLatest[id];
      return {
        positions: { ...state.positions, [id]: scrollTop },
        latestMessageIds: nextLatest,
      };
    }),
  getScroll: (id) => get().positions[id],
  getScrollLatestMessageId: (id) => get().latestMessageIds[id],
  clearScroll: (id) =>
    set((state) => {
      if (!(id in state.positions)) return state;
      const next = { ...state.positions };
      const nextLatest = { ...state.latestMessageIds };
      delete next[id];
      delete nextLatest[id];
      return { positions: next, latestMessageIds: nextLatest };
    }),
}));
