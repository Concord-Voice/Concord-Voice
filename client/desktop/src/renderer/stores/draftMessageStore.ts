import { create } from 'zustand';
import { devtools, persist } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

export interface DraftContent {
  text: string;
  replyToId?: string;
  replyToUserId?: string;
  replyToUsername?: string;
  updatedAt: number;
}

interface DraftMessageState {
  drafts: Record<string, DraftContent>; // keyed by channelId or conversationId

  setDraft: (targetId: string, content: DraftContent) => void;
  clearDraft: (targetId: string) => void;
  getDraft: (targetId: string) => DraftContent | undefined;
  hasDraft: (targetId: string) => boolean;
  clearAllDrafts: () => void;
  clearStaleDrafts: (maxAgeMs?: number) => void;
}

const THIRTY_DAYS_MS = 30 * 24 * 60 * 60 * 1000;

export const useDraftMessageStore = wrapStore(create<DraftMessageState>()(
  devtools(
    persist(
      (set, get) => ({
        drafts: {},

        setDraft: (targetId: string, content: DraftContent) => {
          set({ drafts: { ...get().drafts, [targetId]: content } });
        },

        clearDraft: (targetId: string) => {
          const { [targetId]: _, ...rest } = get().drafts;
          set({ drafts: rest });
        },

        getDraft: (targetId: string) => {
          return get().drafts[targetId];
        },

        hasDraft: (targetId: string) => {
          return targetId in get().drafts;
        },

        clearAllDrafts: () => {
          set({ drafts: {} });
        },

        clearStaleDrafts: (maxAgeMs: number = THIRTY_DAYS_MS) => {
          const now = Date.now();
          const drafts = get().drafts;
          const fresh: Record<string, DraftContent> = {};
          for (const [key, draft] of Object.entries(drafts)) {
            if (now - draft.updatedAt < maxAgeMs) {
              fresh[key] = draft;
            }
          }
          set({ drafts: fresh });
        },
      }),
      {
        name: 'concord:draft-messages',
        partialize: (state) => ({ drafts: state.drafts }),
        onRehydrateStorage: () => {
          return (state) => {
            state?.clearStaleDrafts();
          };
        },
      }
    ),
    { name: 'DraftMessageStore' }
  )
));
