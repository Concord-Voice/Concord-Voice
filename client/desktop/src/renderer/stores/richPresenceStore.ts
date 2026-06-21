import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';
import type { CustomTextPresencePayload } from '../types/ws-events';

/**
 * Rich Presence — Custom Text Status (#1233).
 *
 * Holds OTHER users' custom-text presence (the `custom_text` rich-presence
 * category), keyed by user_id, plus the current user's OWN presence settings
 * (tier + draft/applied custom text). The map is populated by the
 * `rich_presence_update` / `rich_presence_clear` WS handlers in
 * useWebSocketMessages; self settings are populated by the presence-settings
 * REST flow + the settings UI.
 *
 * Wire payloads are schema-validated at the dispatch boundary
 * (CustomTextPresencePayloadSchema in types/ws-events.ts), so the value shape
 * stored here (`{ emoji?, text }`) is structurally guaranteed.
 */

/** One other user's custom-text status. Mirrors CustomTextPresencePayload. */
export type CustomTextStatus = CustomTextPresencePayload;

/** The current user's own presence settings. */
export interface SelfPresence {
  /** Subscription tier governing custom-text availability (0 = free). */
  tier: number;
  /** The applied custom-text body, if set. */
  customText?: string;
  /** The applied custom-text emoji, if set. */
  customTextEmoji?: string;
}

interface RichPresenceState {
  /** Other users' custom text, keyed by user_id. */
  customTextByUser: Record<string, CustomTextStatus>;
  /** The current user's own presence settings. */
  self: SelfPresence;

  /** Set (or replace) another user's custom-text status. */
  setCustomText: (userId: string, status: CustomTextStatus) => void;
  /** Remove another user's custom-text status. */
  clearCustomText: (userId: string) => void;
  /** Read another user's custom-text status (undefined if none). */
  getCustomText: (userId: string) => CustomTextStatus | undefined;
  /** Patch the current user's own presence settings. */
  setSelfPresence: (updates: Partial<SelfPresence>) => void;
  /** Reset the store to initial state (test/teardown + sign-out). */
  reset: () => void;
}

const INITIAL_SELF: SelfPresence = { tier: 0 };

export const useRichPresenceStore = wrapStore(
  create<RichPresenceState>()(
    devtools(
      (set, get) => ({
        customTextByUser: {},
        self: { ...INITIAL_SELF },

        setCustomText: (userId, status) => {
          set((state) => ({
            customTextByUser: { ...state.customTextByUser, [userId]: status },
          }));
        },

        clearCustomText: (userId) => {
          set((state) => {
            if (!(userId in state.customTextByUser)) return state;
            const next = { ...state.customTextByUser };
            delete next[userId];
            return { customTextByUser: next };
          });
        },

        getCustomText: (userId) => get().customTextByUser[userId],

        setSelfPresence: (updates) => {
          set((state) => ({ self: { ...state.self, ...updates } }));
        },

        reset: () => {
          set({ customTextByUser: {}, self: { ...INITIAL_SELF } });
        },
      }),
      { name: 'RichPresenceStore' }
    )
  )
);
