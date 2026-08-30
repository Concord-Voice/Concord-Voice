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

import { useSavedGifsStore, type SavedGif } from '../../stores/chat/savedGifsStore';
import { fetchEncryptedBlob, pushEncryptedBlob } from '../e2ee/e2eeBlobTransport';
import {
  isHydrationLifecycleCurrent,
  type HydrationLifecycleGuard,
} from './postLoginHydrationLifecycle';

const DEBOUNCE_MS = 3000;
const ENDPOINT = '/api/v1/users/me/saved-gifs';
const RESPONSE_KEY = 'saved_gifs';

interface SavedGifsBlob {
  v: 1;
  gifs: SavedGif[];
}

class SavedGifsSyncService {
  private generation = 0;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private unsubscribers: (() => void)[] = [];
  private readonly activePushControllers = new Set<AbortController>();
  private isApplyingRemote = false;

  /**
   * Fetch saved GIFs from server, decrypt, and apply to local store.
   * If the server has no data yet, pushes current local state as initial sync.
   */
  async fetchAndApply(guard?: HydrationLifecycleGuard): Promise<void> {
    const generation = this.generation;
    const isCurrent = (): boolean =>
      generation === this.generation && isHydrationLifecycleCurrent(guard);
    if (!isCurrent()) return;
    const { blob, pushBootstrap } = await fetchEncryptedBlob<SavedGifsBlob>(
      ENDPOINT,
      RESPONSE_KEY,
      guard?.signal,
      isCurrent
    );
    if (!isCurrent()) return;

    if (pushBootstrap) {
      await this.pushSavedGifs(guard, generation);
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
    this.generation += 1;
    for (const unsub of this.unsubscribers) {
      unsub();
    }
    this.unsubscribers = [];
    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
      this.debounceTimer = null;
    }
    for (const controller of this.activePushControllers) {
      controller.abort();
    }
    this.activePushControllers.clear();
    this.isApplyingRemote = false;
  }

  /**
   * Collect current state, encrypt, and push to server.
   */
  async pushSavedGifs(
    guard?: HydrationLifecycleGuard,
    generation = this.generation,
    snapshot?: readonly SavedGif[]
  ): Promise<void> {
    if (generation !== this.generation || !isHydrationLifecycleCurrent(guard)) return;
    const controller = new AbortController();
    const abortPush = (): void => controller.abort();
    guard?.signal.addEventListener('abort', abortPush, { once: true });
    this.activePushControllers.add(controller);
    const isCurrent = (): boolean =>
      this.activePushControllers.has(controller) &&
      !controller.signal.aborted &&
      generation === this.generation &&
      isHydrationLifecycleCurrent(guard);
    const blob: SavedGifsBlob = {
      v: 1,
      gifs: (snapshot ?? useSavedGifsStore.getState().gifs).map((gif) => ({ ...gif })),
    };
    try {
      await pushEncryptedBlob(ENDPOINT, blob, controller.signal, isCurrent);
    } finally {
      guard?.signal.removeEventListener('abort', abortPush);
      this.activePushControllers.delete(controller);
    }
  }

  private schedulePush(): void {
    if (this.isApplyingRemote) return;

    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
    }
    const generation = this.generation;
    const timer = setTimeout(() => {
      if (this.debounceTimer !== timer) return;
      this.debounceTimer = null;
      this.pushSavedGifs(undefined, generation);
    }, DEBOUNCE_MS);
    this.debounceTimer = timer;
  }
}

// Singleton
export const savedGifsSyncService = new SavedGifsSyncService();
