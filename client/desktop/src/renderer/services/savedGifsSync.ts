/**
 * Saved GIFs Sync Service — encrypted cross-device sync for saved KLIPY GIF slugs.
 *
 * Uses the shared e2eeBlobTransport helpers for encrypt/decrypt/push/fetch so the
 * service body only contains the saved-gifs-specific blob shape and store wiring.
 *
 * Only KLIPY GIF slugs are stored — no image data, no titles, no URLs — so we
 * comply with KLIPY ToS Section 1 (no building a content database).
 * The server stores only AES-256-GCM ciphertext; it cannot see which GIFs are saved.
 */

import { useSavedGifsStore, type SavedGif } from '../stores/savedGifsStore';
import { fetchEncryptedBlob, pushEncryptedBlob } from './e2eeBlobTransport';

const DEBOUNCE_MS = 3000;
const ENDPOINT = '/api/v1/users/me/saved-gifs';
const RESPONSE_KEY = 'saved_gifs';

interface SavedGifsBlob {
  v: 1;
  gifs: SavedGif[];
}

class SavedGifsSyncService {
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private unsubscribers: (() => void)[] = [];
  private isApplyingRemote = false;

  /**
   * Fetch saved GIFs from server, decrypt, and apply to local store.
   * If the server has no data yet, pushes current local state as initial sync.
   */
  async fetchAndApply(): Promise<void> {
    const { blob, pushBootstrap } = await fetchEncryptedBlob<SavedGifsBlob>(ENDPOINT, RESPONSE_KEY);

    if (pushBootstrap) {
      await this.pushSavedGifs();
      return;
    }

    if (blob?.v !== 1) return;

    this.isApplyingRemote = true;
    try {
      useSavedGifsStore.getState()._setGifs(blob.gifs);
    } finally {
      setTimeout(() => {
        this.isApplyingRemote = false;
      }, 0);
    }
  }

  /**
   * Start watching the savedGifsStore for changes and schedule debounced pushes.
   */
  startWatching(): void {
    this.stopWatching();

    let prevGifs = useSavedGifsStore.getState().gifs;
    const unsub = useSavedGifsStore.subscribe((state) => {
      if (state.gifs !== prevGifs) {
        prevGifs = state.gifs;
        this.schedulePush();
      }
    });
    this.unsubscribers.push(unsub);
  }

  /**
   * Stop watching store and clear pending debounce.
   */
  stopWatching(): void {
    for (const unsub of this.unsubscribers) {
      unsub();
    }
    this.unsubscribers = [];
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
  }

  /**
   * Collect current state, encrypt, and push to server.
   */
  async pushSavedGifs(): Promise<void> {
    const blob: SavedGifsBlob = {
      v: 1,
      gifs: useSavedGifsStore.getState().gifs,
    };
    await pushEncryptedBlob(ENDPOINT, blob);
  }

  private schedulePush(): void {
    if (this.isApplyingRemote) return;

    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      this.pushSavedGifs();
    }, DEBOUNCE_MS);
  }
}

// Singleton
export const savedGifsSyncService = new SavedGifsSyncService();
