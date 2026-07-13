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

export interface ServerCapabilities {
  auth: {
    oauthProviders: string[];
  };
  features: {
    activityHistorySupported?: boolean;
  };
}

export type ActivityHistoryCapabilityState =
  | { status: 'loading' }
  | { status: 'supported' }
  | { status: 'confirmed-unsupported' }
  | { status: 'error'; lastConfirmedSupported: boolean };

interface ClientConfig {
  minVersion: string;
  featureFlags: FeatureFlags;
  mediaPlaneUrl: string;
  turn: TURNConfig;
  spaUrl: string;
  spaIpcContract: number;
}

interface ClientConfigState extends ClientConfig {
  serverCapabilities: ServerCapabilities | null;
  activityHistoryCapability: ActivityHistoryCapabilityState;
  lastFetchedAt: number | null;
  setConfig: (config: ClientConfig) => void;
  setServerCapabilities: (capabilities: ServerCapabilities | null) => void;
  setActivityHistoryCapability: (capability: ActivityHistoryCapabilityState) => void;
  resetForRuntimeServer: () => void;
}

export const useClientConfigStore = createStore<ClientConfigState>()((set) => ({
  minVersion: '',
  featureFlags: {},
  mediaPlaneUrl: '',
  turn: { host: '', realm: '' },
  spaUrl: '',
  spaIpcContract: 0,
  serverCapabilities: null,
  activityHistoryCapability: { status: 'loading' },
  lastFetchedAt: null,
  setConfig: (config) => set({ ...config, lastFetchedAt: Date.now() }),
  setServerCapabilities: (serverCapabilities) => set({ serverCapabilities }),
  setActivityHistoryCapability: (activityHistoryCapability) => set({ activityHistoryCapability }),
  resetForRuntimeServer: () =>
    set({
      minVersion: '',
      featureFlags: {},
      mediaPlaneUrl: '',
      turn: { host: '', realm: '' },
      spaUrl: '',
      spaIpcContract: 0,
      serverCapabilities: null,
      activityHistoryCapability: { status: 'loading' },
      lastFetchedAt: null,
    }),
}));
