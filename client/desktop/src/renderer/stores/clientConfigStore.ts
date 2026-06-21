import { createStore } from '../utils/createStore';

// Mirrors the backend ClientConfigResponse from services/control-plane/internal/clientconfig/handlers.go

interface FeatureFlags {
  // Server-side flag: true when the control-plane has a KLIPY app key configured
  // and the GIF proxy routes are mounted. The key itself is NEVER sent to the
  // client — all KLIPY traffic goes through /api/v1/klipy/* on the control-plane.
  gifsEnabled?: boolean;
}

// Backend uses omitempty on host/realm (may send `turn: {}`), but the
// clientConfigService always defaults missing fields to '' before calling
// setConfig, so these are guaranteed strings in the store.
interface TURNConfig {
  host: string;
  realm: string;
}

interface ClientConfigState {
  minVersion: string;
  featureFlags: FeatureFlags;
  mediaPlaneUrl: string;
  turn: TURNConfig;
  spaUrl: string;
  spaIpcContract: number;
  lastFetchedAt: number | null;
  setConfig: (config: Omit<ClientConfigState, 'lastFetchedAt' | 'setConfig'>) => void;
}

export const useClientConfigStore = createStore<ClientConfigState>()((set) => ({
  minVersion: '',
  featureFlags: {},
  mediaPlaneUrl: '',
  turn: { host: '', realm: '' },
  spaUrl: '',
  spaIpcContract: 0,
  lastFetchedAt: null,
  setConfig: (config) => set({ ...config, lastFetchedAt: Date.now() }),
}));
