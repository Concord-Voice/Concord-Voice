import { create } from 'zustand';
import { devtools } from 'zustand/middleware';
import { wrapStore } from '../../utils/runtime/createStore';
import { onRuntimeServerSelectionChange } from '../../services/system/runtimeServerBase';
import type { VoiceConnectionState } from '../voice/voiceStore';
import type { CustomTextPresencePayload, RichPresenceEntry } from '../../types/ws-events';

export type CustomTextStatus = CustomTextPresencePayload;
export type RichPresenceCategory = RichPresenceEntry['category'];

type StoredCustomTextEntry = Omit<
  Extract<RichPresenceEntry, { category: 'custom_text' }>,
  'updated_at'
> & {
  updated_at?: number;
};
export type StoredRichPresenceEntry =
  Exclude<RichPresenceEntry, { category: 'custom_text' }> | StoredCustomTextEntry;

export type OtherPresenceByUser = Record<
  string,
  Partial<Record<RichPresenceCategory, StoredRichPresenceEntry>>
>;

export interface SelfPresence {
  tier: number;
  customText?: string;
  customTextEmoji?: string;
}

interface RichPresenceState {
  otherByUser: OtherPresenceByUser;
  self: SelfPresence;
  setOtherPresence: (userId: string, entry: StoredRichPresenceEntry) => void;
  clearOtherPresence: (userId: string, category: RichPresenceCategory) => void;
  replaceOtherPresence: (next: OtherPresenceByUser) => void;
  clearAllOtherPresence: () => void;
  getPresence: (
    userId: string,
    category: RichPresenceCategory
  ) => StoredRichPresenceEntry | undefined;
  setCustomText: (userId: string, status: CustomTextStatus) => void;
  clearCustomText: (userId: string) => void;
  getCustomText: (userId: string) => CustomTextStatus | undefined;
  setSelfPresence: (updates: Partial<SelfPresence>) => void;
  reset: () => void;
}

const INITIAL_SELF: SelfPresence = { tier: 0 };

export interface LocalRichPresenceVoiceState {
  activeChannelId: string | null;
  activeChannelName: string | null;
  activeServerId: string | null;
  connectionState: VoiceConnectionState;
  isDMCall: boolean;
  isGroupDM: boolean;
  callState: { kind: string };
  participants: Record<string, unknown>;
}

export type LocalRichPresenceActivity =
  | {
      category: 'server_voice';
      channelId: string;
      channelName: string;
      serverId: string;
    }
  | {
      category: 'private_call';
      callType: 'dm' | 'group';
      participantCount?: number;
    };

export function selectLocalRichPresenceActivity(
  voiceState: LocalRichPresenceVoiceState
): LocalRichPresenceActivity | null {
  if (voiceState.connectionState !== 'connected') return null;

  if (voiceState.isDMCall) {
    if (voiceState.callState.kind !== 'in-call') return null;
    const activity: LocalRichPresenceActivity = {
      category: 'private_call',
      callType: voiceState.isGroupDM ? 'group' : 'dm',
    };
    const participantCount = Object.keys(voiceState.participants).length;
    return voiceState.isGroupDM && participantCount > 0
      ? { ...activity, participantCount }
      : activity;
  }

  if (voiceState.callState.kind !== 'idle' && voiceState.callState.kind !== 'in-call') return null;
  if (!voiceState.activeChannelId || !voiceState.activeChannelName || !voiceState.activeServerId) {
    return null;
  }
  return {
    category: 'server_voice',
    channelId: voiceState.activeChannelId,
    channelName: voiceState.activeChannelName,
    serverId: voiceState.activeServerId,
  };
}

export const selectCustomText =
  (userId: string) =>
  (state: RichPresenceState): CustomTextStatus | undefined => {
    const entry = state.otherByUser[userId]?.custom_text;
    return entry?.category === 'custom_text' ? entry.payload : undefined;
  };

export const useRichPresenceStore = wrapStore(
  create<RichPresenceState>()(
    devtools(
      (set, get) => ({
        otherByUser: {},
        self: { ...INITIAL_SELF },

        setOtherPresence: (userId, entry) => {
          set((state) => ({
            otherByUser: {
              ...state.otherByUser,
              [userId]: { ...state.otherByUser[userId], [entry.category]: entry },
            },
          }));
        },

        clearOtherPresence: (userId, category) => {
          set((state) => {
            const current = state.otherByUser[userId];
            if (!current || !(category in current)) return state;
            const nextUser = { ...current };
            delete nextUser[category];
            const next = { ...state.otherByUser };
            if (Object.keys(nextUser).length === 0) delete next[userId];
            else next[userId] = nextUser;
            return { otherByUser: next };
          });
        },

        replaceOtherPresence: (next) => set({ otherByUser: next }),
        clearAllOtherPresence: () => set({ otherByUser: {} }),

        getPresence: (userId, category) => get().otherByUser[userId]?.[category],

        setCustomText: (userId, status) =>
          get().setOtherPresence(userId, { category: 'custom_text', payload: status }),

        clearCustomText: (userId) => get().clearOtherPresence(userId, 'custom_text'),

        getCustomText: (userId) => selectCustomText(userId)(get()),

        setSelfPresence: (updates) => {
          set((state) => ({ self: { ...state.self, ...updates } }));
        },

        reset: () => set({ otherByUser: {}, self: { ...INITIAL_SELF } }),
      }),
      { name: 'RichPresenceStore' }
    )
  )
);

onRuntimeServerSelectionChange(() => {
  useRichPresenceStore.getState().clearAllOtherPresence();
});
