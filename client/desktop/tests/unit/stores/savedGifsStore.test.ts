import { useSavedGifsStore } from '@/renderer/stores/savedGifsStore';
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
});
