// Persisted "last seen changelog version" for the post-update modal (#1857).
// localStorage is disk-backed in Electron and survives full app restarts.
// Accepted residual: storage is per-renderer-origin (remote SPA / app://concord /
// spa-cache://concord are disjoint), so an origin flip may re-show one version
// once — see the design spec D2.
import { create } from 'zustand';
import { persist } from 'zustand/middleware';

interface ChangelogState {
  lastSeenVersion: string | null;
  markSeen: (version: string) => void;
}

export const useChangelogStore = create<ChangelogState>()(
  persist(
    (set) => ({
      lastSeenVersion: null,
      markSeen: (version) => set({ lastSeenVersion: version }),
    }),
    { name: 'concord-changelog', version: 1 }
  )
);
