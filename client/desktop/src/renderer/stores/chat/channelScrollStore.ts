/**
 * channelScrollStore — in-memory reading-position cache for MessageList.
 *
 * Used by both the server-channel and DM MessageList mounts to bring the user
 * back to where they were reading when they switch away and return. Keyed on
 * channelId OR dmConversationId (callers decide — the store treats them as
 * opaque strings).
 *
 * A position is a message ANCHOR, never a pixel offset. Message rows change
 * height after mount (GIF embeds and image attachments are skeletons until
 * their bytes resolve), so a scrollTop measured against the settled layout
 * lands somewhere else when replayed against the fresh one. The anchor is the
 * message nearest the top of the viewport plus how far its top sits above the
 * viewport edge, which survives any amount of growth elsewhere in the list.
 *
 * "At the bottom" is represented by the ABSENCE of an anchor: a thread with no
 * entry opens at the latest message and keeps following it as media resolves.
 *
 * NOT persisted across app restarts: positions are session-scoped, since a
 * saved anchor only makes sense against a live message cache.
 */
import { createStore } from '../../utils/runtime/createStore';

export interface ScrollAnchor {
  /** `data-message-id` of the topmost visible message row when the user left. */
  messageId: string;
  /** Pixels of that row hidden above the viewport's top edge (negative when a
   *  date divider sits between the row and the edge). */
  offset: number;
}

export interface ChannelScrollState {
  anchors: Record<string, ScrollAnchor>;
  saveAnchor: (id: string, anchor: ScrollAnchor) => void;
  getAnchor: (id: string) => ScrollAnchor | undefined;
  clearAnchor: (id: string) => void;
}

export const useChannelScrollStore = createStore<ChannelScrollState>()((set, get) => ({
  anchors: {},
  saveAnchor: (id, anchor) => set((state) => ({ anchors: { ...state.anchors, [id]: anchor } })),
  getAnchor: (id) => get().anchors[id],
  clearAnchor: (id) =>
    set((state) => {
      if (!(id in state.anchors)) return state;
      const next = { ...state.anchors };
      delete next[id];
      return { anchors: next };
    }),
}));
