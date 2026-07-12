import { createStore } from '../utils/createStore';

export interface PresenceOverrideState {
  excludedUserIds: string[];
  appliedVersion: number;
  loading: boolean;
  saving: boolean;
  conflict: boolean;
  error: string | null;
  apply: (excludedUserIds: readonly string[], version: number) => void;
  setLoading: (loading: boolean) => void;
  setSaving: (saving: boolean) => void;
  setConflict: (conflict: boolean) => void;
  setError: (error: string | null) => void;
  reset: () => void;
}

const initialState = {
  excludedUserIds: [],
  appliedVersion: 0,
  loading: false,
  saving: false,
  conflict: false,
  error: null,
} satisfies Omit<
  PresenceOverrideState,
  'apply' | 'setLoading' | 'setSaving' | 'setConflict' | 'setError' | 'reset'
>;

export const usePresenceOverrideStore = createStore<PresenceOverrideState>()((set) => ({
  ...initialState,
  apply: (excludedUserIds, version) =>
    set({ excludedUserIds: [...excludedUserIds], appliedVersion: version, error: null }),
  setLoading: (loading) => set({ loading }),
  setSaving: (saving) => set({ saving }),
  setConflict: (conflict) => set({ conflict }),
  setError: (error) => set({ error }),
  reset: () => set({ ...initialState, excludedUserIds: [] }),
}));
