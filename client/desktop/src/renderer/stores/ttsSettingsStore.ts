import { create } from 'zustand';
import { persist } from 'zustand/middleware';
import { wrapStore } from '../utils/createStore';

export interface TTSSettings {
  ttsEnabled: boolean;
  ttsSendEnabled: boolean;
  ttsVoice: string | null;
  ttsRate: number;
  ttsVolume: number;
}

interface TTSSettingsState extends TTSSettings {
  setTtsEnabled: (enabled: boolean) => void;
  setTtsSendEnabled: (enabled: boolean) => void;
  setTtsVoice: (voice: string | null) => void;
  setTtsRate: (rate: number) => void;
  setTtsVolume: (volume: number) => void;
}

export const useTTSSettingsStore = wrapStore(create<TTSSettingsState>()(
  persist(
    (set) => ({
      ttsEnabled: false,
      ttsSendEnabled: false,
      ttsVoice: null,
      ttsRate: 1,
      ttsVolume: 1,

      setTtsEnabled: (ttsEnabled) => set({ ttsEnabled }),
      setTtsSendEnabled: (ttsSendEnabled) => set({ ttsSendEnabled }),
      setTtsVoice: (ttsVoice) => set({ ttsVoice }),
      setTtsRate: (ttsRate) => set({ ttsRate: Math.max(0.5, Math.min(2, ttsRate)) }),
      setTtsVolume: (ttsVolume) => set({ ttsVolume: Math.max(0, Math.min(1, ttsVolume)) }),
    }),
    { name: 'concord:tts-settings' }
  )
));
