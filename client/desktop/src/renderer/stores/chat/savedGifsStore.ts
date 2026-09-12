import { createStore } from '../../utils/runtime/createStore';

export interface SavedGif {
  slug: string;
  savedAt: number;
}

interface SavedGifsState {
  gifs: SavedGif[];
  saveGif: (gifSlug: string) => void;
  removeGif: (gifSlug: string) => void;
  isGifSaved: (gifSlug: string) => boolean;
  /** Clear account-scoped state during logout/account switching. */
  reset: () => void;
  /** Internal: called by sync service to replace local state with decrypted remote data. */
  _setGifs: (gifs: SavedGif[]) => void;
}

export const useSavedGifsStore = createStore<SavedGifsState>()((set, get) => ({
  gifs: [],

  saveGif: (gifSlug) =>
    set((state) => {
      if (state.gifs.some((g) => g.slug === gifSlug)) return state;
      return { gifs: [{ slug: gifSlug, savedAt: Date.now() }, ...state.gifs] };
    }),

  removeGif: (gifSlug) =>
    set((state) => {
      // filter() returns a new array even when nothing matched, and to every
      // subscriber a new array identity IS a change. Mirror saveGif's guard so
      // a no-op stays a no-op (#2370).
      if (!state.gifs.some((g) => g.slug === gifSlug)) return state;
      return { gifs: state.gifs.filter((g) => g.slug !== gifSlug) };
    }),

  isGifSaved: (gifSlug) => get().gifs.some((g) => g.slug === gifSlug),

  reset: () => set({ gifs: [] }),

  _setGifs: (gifs) =>
    set((state) => {
      // The decrypted preferences blob is cast, never schema-checked, and this
      // is its only consumer. Without this guard a malformed blob throws a
      // TypeError straight out of fetchAndApply — which postLoginHydration
      // awaits with no catch, so one bad field would abort five unrelated
      // hydration steps, and the WebSocket echo path would raise an unhandled
      // rejection.
      // Array.isArray alone validates only the CONTAINER. A null or non-object
      // entry passes it, falls through the comparison below (a real slug never
      // equals undefined), and gets STORED — after which GifPicker's
      // savedSlugKey selector dereferences g.slug during render and throws in
      // the component tree. That is strictly worse than the rejected promise
      // this guard was added to prevent, so validate every entry.
      if (
        !Array.isArray(gifs) ||
        !gifs.every((g) => g !== null && typeof g === 'object' && typeof g.slug === 'string')
      ) {
        return state;
      }
      // The server echoes the user's own write back, so this is routinely
      // called with content we already hold. Zustand's set() bails before it
      // merges OR notifies when the updater returns the same reference, so
      // returning `state` stops an echo waking every subscriber's selector.
      // That is the whole of what this buys (#2370): savedGifsSync's push
      // watcher does compare by identity, but it is already covered by its own
      // isApplyingRemote flag, which is set before this call and read
      // synchronously within it.
      // savedAt is local bookkeeping that nothing reads and the server does not
      // own, so two lists with the same slugs in the same order are the same
      // list. The comparison is index-wise ON PURPOSE — saveGif prepends, so
      // array order is the Saved tab's display order; a set-wise or sorted
      // comparison would silently freeze that order.
      if (
        state.gifs.length === gifs.length &&
        state.gifs.every((g, i) => g.slug === gifs[i]?.slug)
      ) {
        return state;
      }
      return { gifs };
    }),
}));
