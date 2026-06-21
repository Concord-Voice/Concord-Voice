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
  saveScroll: (id: string, scrollTop: number) => void;
  getScroll: (id: string) => number | undefined;
  clearScroll: (id: string) => void;
}

export const useChannelScrollStore = createStore<ChannelScrollState>()((set, get) => ({
  positions: {},
  saveScroll: (id, scrollTop) =>
    set((state) => ({ positions: { ...state.positions, [id]: scrollTop } })),
  getScroll: (id) => get().positions[id],
  clearScroll: (id) =>
    set((state) => {
      if (!(id in state.positions)) return state;
      const next = { ...state.positions };
      delete next[id];
      return { positions: next };
    }),
}));
