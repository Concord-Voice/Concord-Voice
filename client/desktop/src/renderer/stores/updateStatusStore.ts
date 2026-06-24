import { createStore } from '../utils/createStore';

/**
 * State for critical update-channel errors that warrant a persistent
 * in-app banner (see UpdateSecurityBanner). Covers cert-pin and publisher
 * signature failures routed from the updater via the `update:error` IPC
 * channel (#658), plus the media-frame crypto-version floor (#1878): when the
 * media-plane rejects a voice join because the room negotiated a newer frame
 * crypto version than this client can speak, the client is too old to join and
 * must update — surfaced through the same "Download the latest" banner.
 */
export type UpdateCriticalErrorSubtype =
  | 'cert-pin-failure'
  | 'publisher-failure'
  | 'media-crypto-version';

export interface UpdateCriticalError {
  readonly subtype: UpdateCriticalErrorSubtype;
  readonly message: string;
  readonly firstSeenAt: number;
}

interface UpdateStatusState {
  criticalError: UpdateCriticalError | null;
  dismissedForSession: boolean;
  setSecurityError: (subtype: UpdateCriticalErrorSubtype, message: string) => void;
  dismissForSession: () => void;
  reset: () => void;
}

export const useUpdateStatusStore = createStore<UpdateStatusState>()((set) => ({
  criticalError: null,
  dismissedForSession: false,
  setSecurityError: (subtype, message) =>
    // Clear dismissedForSession on a NEW error (different subtype OR message)
    // so the banner re-surfaces. Keep the flag when the same error recurs so
    // we honor the user's dismiss across rapid re-emissions. Issue #658, PR
    // #719 feedback from Copilot.
    set((state) => {
      const isSameError =
        state.criticalError?.subtype === subtype && state.criticalError.message === message;
      return {
        criticalError: { subtype, message, firstSeenAt: Date.now() },
        dismissedForSession: isSameError ? state.dismissedForSession : false,
      };
    }),
  dismissForSession: () => set({ dismissedForSession: true }),
  reset: () => set({ criticalError: null, dismissedForSession: false }),
}));
