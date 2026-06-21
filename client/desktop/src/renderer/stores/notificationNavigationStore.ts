import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

interface PendingNavigation {
  type: 'channel' | 'dm';
  targetId: string;
  serverId?: string;
}

interface NotificationNavigationState {
  pendingNavigation: PendingNavigation | null;
  setPendingNavigation: (nav: PendingNavigation) => void;
  clearPendingNavigation: () => void;
}

export const useNotificationNavigationStore = wrapStore(create<NotificationNavigationState>()(
  devtools(
    (set) => ({
      pendingNavigation: null,
      setPendingNavigation: (nav) => set({ pendingNavigation: nav }),
      clearPendingNavigation: () => set({ pendingNavigation: null }),
    }),
    { name: 'NotificationNavigationStore' }
  )
));
