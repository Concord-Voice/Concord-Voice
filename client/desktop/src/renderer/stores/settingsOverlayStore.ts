import { createStore } from '../utils/createStore';

/**
 * Settings Overlay Store
 *
 * Drives which "settings" surface (app settings or server settings) is rendered
 * as a fullscreen portal overlay on top of the persistent chat layout.
 *
 * Rendering settings as overlays (instead of routes that replace MainView) keeps
 * the WebSocket-bound chat tree mounted underneath, eliminating subscribe /
 * unsubscribe churn whenever the user opens settings.
 */

export type SettingsOverlayKind = 'app' | 'server';

export interface SettingsOverlayPayload {
  /** Required when kind === 'server'. */
  serverId?: string;
}

interface SettingsOverlayState {
  open: SettingsOverlayKind | null;
  payload: SettingsOverlayPayload | null;
  openSettings: (kind: SettingsOverlayKind, payload?: SettingsOverlayPayload) => void;
  close: () => void;
}

export const useSettingsOverlayStore = createStore<SettingsOverlayState>()((set) => ({
  open: null,
  payload: null,
  openSettings: (kind, payload) => set({ open: kind, payload: payload ?? null }),
  close: () => set({ open: null, payload: null }),
}));
