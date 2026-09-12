import { create } from 'zustand';
import { useSavedGifsStore, type SavedGif } from '@/renderer/stores/chat/savedGifsStore';
import { resetAllStores } from '../../helpers/store-helpers';

describe('savedGifsStore', () => {
  beforeEach(() => {
    resetAllStores();
  });

  it('starts with an empty gifs array', () => {
    expect(useSavedGifsStore.getState().gifs).toEqual([]);
  });

  it('saves a GIF by prepending to the array', () => {
    useSavedGifsStore.getState().saveGif('abc123');
    const gifs = useSavedGifsStore.getState().gifs;
    expect(gifs).toHaveLength(1);
    expect(gifs[0].slug).toBe('abc123');
    expect(gifs[0].savedAt).toBeGreaterThan(0);
  });

  it('prepends newer GIFs before older ones', () => {
    useSavedGifsStore.getState().saveGif('first');
    useSavedGifsStore.getState().saveGif('second');
    const gifs = useSavedGifsStore.getState().gifs;
    expect(gifs).toHaveLength(2);
    expect(gifs[0].slug).toBe('second');
    expect(gifs[1].slug).toBe('first');
  });

  it('does not duplicate an already-saved GIF', () => {
    useSavedGifsStore.getState().saveGif('abc123');
    useSavedGifsStore.getState().saveGif('abc123');
    expect(useSavedGifsStore.getState().gifs).toHaveLength(1);
  });

  it('removes a GIF by ID', () => {
    useSavedGifsStore.getState().saveGif('keep');
    useSavedGifsStore.getState().saveGif('remove');
    useSavedGifsStore.getState().removeGif('remove');
    const gifs = useSavedGifsStore.getState().gifs;
    expect(gifs).toHaveLength(1);
    expect(gifs[0].slug).toBe('keep');
  });

  it('removeGif is a no-op for non-existent IDs', () => {
    useSavedGifsStore.getState().saveGif('exists');
    useSavedGifsStore.getState().removeGif('nonexistent');
    expect(useSavedGifsStore.getState().gifs).toHaveLength(1);
  });

  it('isGifSaved returns true for saved GIFs', () => {
    useSavedGifsStore.getState().saveGif('abc123');
    expect(useSavedGifsStore.getState().isGifSaved('abc123')).toBe(true);
  });

  it('isGifSaved returns false for unsaved GIFs', () => {
    expect(useSavedGifsStore.getState().isGifSaved('xyz789')).toBe(false);
  });

  it('_setGifs replaces the entire array', () => {
    useSavedGifsStore.getState().saveGif('old');
    const newGifs = [
      { slug: 'new-1', savedAt: 1000 },
      { slug: 'new-2', savedAt: 2000 },
    ];
    useSavedGifsStore.getState()._setGifs(newGifs);
    expect(useSavedGifsStore.getState().gifs).toEqual(newGifs);
  });

  // regression for #2370
  it('_setGifs with content-equal slugs (server echo) preserves array identity', () => {
    useSavedGifsStore.getState()._setGifs([
      { slug: 'alpha', savedAt: 1000 },
      { slug: 'beta', savedAt: 2000 },
    ]);
    const before = useSavedGifsStore.getState().gifs;

    // The server echoes the user's own write back over WebSocket. `savedAt`
    // is local bookkeeping, not part of the server's identity of a saved
    // GIF — the echo carries its OWN timestamps, which legitimately differ
    // from what the client stamped locally. A fixture with matching savedAt
    // values would not exercise that distinction at all: any accidental
    // `savedAt`-inclusive comparison would still pass, which is exactly the
    // gap this fixture is built to close.
    useSavedGifsStore.getState()._setGifs([
      { slug: 'alpha', savedAt: 9999 },
      { slug: 'beta', savedAt: 8888 },
    ]);

    expect(useSavedGifsStore.getState().gifs).toBe(before);
  });

  // regression for #2370
  it('removeGif is a no-op for a slug that was never saved and preserves array identity', () => {
    useSavedGifsStore.getState().saveGif('exists');
    const before = useSavedGifsStore.getState().gifs;

    useSavedGifsStore.getState().removeGif('never-saved-slug');

    expect(useSavedGifsStore.getState().gifs).toBe(before);
  });

  // The decrypted preferences blob _setGifs consumes is cast, never schema-checked (see
  // savedGifsStore.ts:41-49). These two cases are the malformed-input guard's own coverage.
  describe('_setGifs malformed-input guard', () => {
    it('is a no-op (returns the existing state by reference) for a non-array input, and does not throw', () => {
      useSavedGifsStore.getState().saveGif('existing');
      const before = useSavedGifsStore.getState().gifs;

      expect(() => useSavedGifsStore.getState()._setGifs(undefined as never)).not.toThrow();

      expect(useSavedGifsStore.getState().gifs).toBe(before);
    });

    it('rejects an element-shaped fault outright (a null entry never reaches the store)', () => {
      useSavedGifsStore.getState()._setGifs([
        { slug: 'alpha', savedAt: 1000 },
        { slug: 'beta', savedAt: 2000 },
      ]);
      const before = useSavedGifsStore.getState().gifs;

      // Same length as current state (2), so this reaches the index-wise slug
      // comparison rather than short-circuiting on a length mismatch.
      expect(() =>
        useSavedGifsStore.getState()._setGifs([null as never, { slug: 'beta', savedAt: 2000 }])
      ).not.toThrow();

      // not.toThrow() alone observes the WRONG LAYER and is why an earlier version of
      // this test passed over a poisoned store. `Array.isArray` validates only the
      // container: a null entry passed it, failed the slug comparison (a real slug never
      // equals undefined), and was STORED -- moving the TypeError into GifPicker's
      // savedSlugKey selector, where it throws during render instead. Assert the store
      // is untouched, then exercise the selector's own shape against it.
      expect(useSavedGifsStore.getState().gifs).toBe(before);
      expect(() =>
        useSavedGifsStore
          .getState()
          .gifs.map((g) => g.slug)
          .join(String.fromCharCode(0))
      ).not.toThrow();
    });
  });

  // NON-VACUITY CONTROL for the two cases above. This guard already exists in production
  // code, so both cases above pass immediately -- which is not proof it does anything. This
  // reconstructs the PRE-GUARD `_setGifs` semantics (no `Array.isArray` check, no `?.` on the
  // slug read) in a standalone, throwaway zustand store -- never by mutating
  // savedGifsStore.ts -- and demonstrates the identical inputs throw there.
  describe('_setGifs malformed-input guard (control: pre-guard semantics throw)', () => {
    interface UnguardedState {
      gifs: SavedGif[];
      _setGifs: (gifs: SavedGif[]) => void;
    }

    function makeUnguardedStore(initial: SavedGif[]) {
      return create<UnguardedState>((set) => ({
        gifs: initial,
        _setGifs: (gifs) =>
          set((state) => {
            // Mirrors savedGifsStore._setGifs as it existed before the guard: no
            // Array.isArray check, and `gifs[i].slug` with no optional chaining.
            if (
              state.gifs.length === gifs.length &&
              state.gifs.every((g, i) => g.slug === gifs[i].slug)
            ) {
              return state;
            }
            return { gifs };
          }),
      }));
    }

    it('throws on a non-array input', () => {
      const unguarded = makeUnguardedStore([{ slug: 'existing', savedAt: 1 }]);

      expect(() => unguarded.getState()._setGifs(undefined as never)).toThrow();
    });

    it('throws on an element-shaped fault (a null entry in a length-matching list)', () => {
      const unguarded = makeUnguardedStore([
        { slug: 'alpha', savedAt: 1000 },
        { slug: 'beta', savedAt: 2000 },
      ]);

      expect(() =>
        unguarded.getState()._setGifs([null as never, { slug: 'beta', savedAt: 2000 }])
      ).toThrow();
    });
  });
});
