import { createStore } from '../../utils/runtime/createStore';
import { useDraftSettingsStore } from './draftSettingsStore';

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

export function isSettingsOverlayDismissBlocked(open: SettingsOverlayKind | null): boolean {
  return open === 'app' && useDraftSettingsStore.getState().contentProtectionApplying;
}

/** The Server Settings left-nav sections (`ServerSettingsPage`). */
export type ServerSettingsSection = 'general' | 'roles' | 'members';

export interface SettingsOverlayPayload {
  /** Required when kind === 'server'. */
  serverId?: string;
  /** The Server Settings section to open at; omitted, General. */
  section?: ServerSettingsSection;
}

/**
 * Where the "Back to …" button in Privacy & Security goes once verification
 * setup is done (#3456 §3.6a): the chat that was never navigated away from, or
 * the Server Settings section the setup was opened from.
 */
export type VerificationReturn =
  { kind: 'chat' } | { kind: 'serverSettings'; serverId: string; section: ServerSettingsSection };

interface SettingsOverlayState {
  open: SettingsOverlayKind | null;
  payload: SettingsOverlayPayload | null;
  /**
   * The one pending verification return, or null. In memory only, never
   * persisted. It lasts one Settings excursion: taken once, and dropped when
   * the overlay closes and on every logout-class reset (`gracefulReset`).
   */
  verificationReturn: VerificationReturn | null;
  /**
   * Servers whose "some of your permissions need MFA" notice the user dismissed
   * (#3456 §3.6). In memory only, so it lasts the app session and not a
   * restart. It is deliberately not cleared by `close`: dismissal is not a
   * Settings excursion. Ending an account drops it through
   * `clearMfaNoticeDismissals`.
   */
  dismissedMfaNotices: Record<string, true>;
  openSettings: (kind: SettingsOverlayKind, payload?: SettingsOverlayPayload) => void;
  close: () => void;
  setVerificationReturn: (target: VerificationReturn) => void;
  /** Reads the pending return and clears it, so it is acted on at most once. */
  takeVerificationReturn: () => VerificationReturn | null;
  clearVerificationReturn: () => void;
  dismissMfaNotice: (serverId: string) => void;
  clearMfaNoticeDismissals: () => void;
}

export const useSettingsOverlayStore = createStore<SettingsOverlayState>()((set, get) => ({
  open: null,
  payload: null,
  verificationReturn: null,
  dismissedMfaNotices: {},
  openSettings: (kind, payload) => set({ open: kind, payload: payload ?? null }),
  close: () =>
    set((state) => {
      if (isSettingsOverlayDismissBlocked(state.open)) {
        return state;
      }
      return { open: null, payload: null, verificationReturn: null };
    }),
  setVerificationReturn: (target) => set({ verificationReturn: target }),
  takeVerificationReturn: () => {
    const target = get().verificationReturn;
    if (target !== null) set({ verificationReturn: null });
    return target;
  },
  clearVerificationReturn: () => set({ verificationReturn: null }),
  dismissMfaNotice: (serverId) =>
    set((state) => ({ dismissedMfaNotices: { ...state.dismissedMfaNotices, [serverId]: true } })),
  clearMfaNoticeDismissals: () => set({ dismissedMfaNotices: {} }),
}));
