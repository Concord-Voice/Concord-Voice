import { createStore } from '../utils/createStore';

export interface SavedGif {
  slug: string;
  savedAt: number;
}

interface SavedGifsState {
  gifs: SavedGif[];
  saveGif: (gifSlug: string) => void;
  removeGif: (gifSlug: string) => void;
  isGifSaved: (gifSlug: string) => boolean;
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
    set((state) => ({
      gifs: state.gifs.filter((g) => g.slug !== gifSlug),
    })),

  isGifSaved: (gifSlug) => get().gifs.some((g) => g.slug === gifSlug),

  _setGifs: (gifs) => set({ gifs }),
}));
