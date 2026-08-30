/**
 * Friend Organization Sync — encrypted cross-device sync for friend categories (#324).
 *
 * Templated on savedGifsSync over the shared e2eeBlobTransport helpers, with ONE
 * addition the reused pattern lacks: a decrypt-time structural validator
 * (validateFriendOrgBlob) that runs at the trust boundary before hydrating the
 * store. Cross-device last-writer-wins can hand this client a blob authored on
 * another device that the local one-per-friend write-path never vetted.
 *
 * The server stores only AES-256-GCM ciphertext + an integer version; it cannot
 * read category names, colors, emoji, OR membership (zero-knowledge).
 */

import { useFriendOrgStore, type FriendOrgBlob } from '../../stores/chat/friendOrgStore';
import { fetchEncryptedBlob, pushEncryptedBlob } from '../e2ee/e2eeBlobTransport';
import { validateFriendOrgBlob } from '../../utils/friendOrgBlob';
import {
  isHydrationLifecycleCurrent,
  type HydrationLifecycleGuard,
} from './postLoginHydrationLifecycle';

const DEBOUNCE_MS = 3000;
const ENDPOINT = '/api/v1/users/me/friend-organization';
const RESPONSE_KEY = 'friend_organization';

class FriendOrgSyncService {
  private generation = 0;
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private remoteApplyResetTimer: ReturnType<typeof setTimeout> | null = null;
  private unsubscribers: (() => void)[] = [];
  private readonly activePushControllers = new Set<AbortController>();
  private isApplyingRemote = false;

  /**
   * Fetch the encrypted blob, decrypt, structurally validate, and hydrate the store.
   * If the server has no blob yet, push current local state as the initial sync.
   */
  async fetchAndApply(): Promise<boolean> {
    const generation = this.generation;
    const { blob, pushBootstrap } = await fetchEncryptedBlob<unknown>(ENDPOINT, RESPONSE_KEY);
    if (generation !== this.generation) return false;

    if (pushBootstrap) {
      await this.pushFriendOrg();
      return generation === this.generation;
    }

    if (blob == null) return true;

    // Decrypt-time trust-boundary guard (NEW vs savedGifsSync/preferencesSync).
    const safe: FriendOrgBlob = validateFriendOrgBlob(blob);

    this.isApplyingRemote = true;
    try {
      useFriendOrgStore.getState()._hydrate(safe);
    } finally {
      if (this.remoteApplyResetTimer) {
        clearTimeout(this.remoteApplyResetTimer);
      }
      this.remoteApplyResetTimer = setTimeout(() => {
        this.remoteApplyResetTimer = null;
        this.isApplyingRemote = false;
      }, 0);
    }
    return generation === this.generation;
  }

  /**
   * Subscribe to friendOrgStore changes and schedule debounced pushes.
   */
  startWatching(): void {
    this.stopWatching();

    const sel = () => {
      const s = useFriendOrgStore.getState();
      return { categories: s.categories, sectionOrder: s.sectionOrder };
    };
    let prev = sel();
    const unsub = useFriendOrgStore.subscribe(() => {
      const next = sel();
      if (next.categories !== prev.categories || next.sectionOrder !== prev.sectionOrder) {
        prev = next;
        this.schedulePush();
      }
    });
    this.unsubscribers.push(unsub);
  }

  /** Stop watching the store and clear both owned timers. */
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
    if (this.remoteApplyResetTimer) {
      clearTimeout(this.remoteApplyResetTimer);
      this.remoteApplyResetTimer = null;
    }
    for (const controller of this.activePushControllers) {
      controller.abort();
    }
    this.activePushControllers.clear();
    this.isApplyingRemote = false;
  }

  /**
   * Encrypt the current blob and push it to the server.
   */
  async pushFriendOrg(snapshot?: FriendOrgBlob, guard?: HydrationLifecycleGuard): Promise<boolean> {
    const generation = this.generation;
    if (!isHydrationLifecycleCurrent(guard)) return false;
    const controller = new AbortController();
    const abortPush = (): void => controller.abort();
    guard?.signal.addEventListener('abort', abortPush, { once: true });
    this.activePushControllers.add(controller);
    const isCurrent = (): boolean =>
      generation === this.generation &&
      !controller.signal.aborted &&
      isHydrationLifecycleCurrent(guard);
    const current = useFriendOrgStore.getState();
    const blob: FriendOrgBlob = snapshot ?? {
      v: 1,
      categories: current.categories,
      sectionOrder: current.sectionOrder,
    };
    try {
      await pushEncryptedBlob(ENDPOINT, blob, controller.signal, isCurrent);
      return isCurrent();
    } finally {
      guard?.signal.removeEventListener('abort', abortPush);
      this.activePushControllers.delete(controller);
    }
  }

  private schedulePush(): void {
    if (this.isApplyingRemote) return; // echo guard — suppresses the apply→push loop

    if (this.debounceTimer) {
      clearTimeout(this.debounceTimer);
    }
    this.debounceTimer = setTimeout(() => {
      this.debounceTimer = null;
      this.pushFriendOrg();
    }, DEBOUNCE_MS);
  }
}

// Singleton
export const friendOrgSyncService = new FriendOrgSyncService();
