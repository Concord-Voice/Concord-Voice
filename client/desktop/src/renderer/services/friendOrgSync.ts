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

import { useFriendOrgStore, type FriendOrgBlob } from '../stores/friendOrgStore';
import { fetchEncryptedBlob, pushEncryptedBlob } from './e2eeBlobTransport';
import { validateFriendOrgBlob } from '../utils/friendOrgBlob';

const DEBOUNCE_MS = 3000;
const ENDPOINT = '/api/v1/users/me/friend-organization';
const RESPONSE_KEY = 'friend_organization';

class FriendOrgSyncService {
  private debounceTimer: ReturnType<typeof setTimeout> | null = null;
  private unsubscribers: (() => void)[] = [];
  private isApplyingRemote = false;

  /**
   * Fetch the encrypted blob, decrypt, structurally validate, and hydrate the store.
   * If the server has no blob yet, push current local state as the initial sync.
   */
  async fetchAndApply(): Promise<void> {
    const { blob, pushBootstrap } = await fetchEncryptedBlob<unknown>(ENDPOINT, RESPONSE_KEY);

    if (pushBootstrap) {
      await this.pushFriendOrg();
      return;
    }

    if (blob == null) return;

    // Decrypt-time trust-boundary guard (NEW vs savedGifsSync/preferencesSync).
    const safe: FriendOrgBlob = validateFriendOrgBlob(blob);

    this.isApplyingRemote = true;
    try {
      useFriendOrgStore.getState()._hydrate(safe);
    } finally {
      setTimeout(() => {
        this.isApplyingRemote = false;
      }, 0);
    }
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

  /**
   * Stop watching the store and clear any pending debounce.
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
   * Encrypt the current blob and push it to the server.
   */
  async pushFriendOrg(): Promise<void> {
    const s = useFriendOrgStore.getState();
    const blob: FriendOrgBlob = { v: 1, categories: s.categories, sectionOrder: s.sectionOrder };
    await pushEncryptedBlob(ENDPOINT, blob);
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
