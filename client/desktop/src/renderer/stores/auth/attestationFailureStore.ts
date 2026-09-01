import { createStore } from '../../utils/runtime/createStore';

/**
 * Terminal attestation codes that trigger the failure modal.
 * Mirrors ATTESTATION_TERMINAL_CODES in apiClient.ts — these are the codes
 * for which this build is permanently rejected (no retry path). Narrowed to
 * a string-literal union so the modal cannot be opened with an unrecognized
 * code that would render the "Update Required" UX inappropriately.
 *
 * Server source of truth: ErrorCode constants in
 * services/control-plane/internal/attestation/types.go (ErrUnknownRelease,
 * ErrRevoked, ErrVersionTooOld).
 */
export type TerminalAttestationCode =
  'ATTESTATION_UNKNOWN_RELEASE' | 'ATTESTATION_REVOKED' | 'CLIENT_VERSION_TOO_OLD';

interface AttestationFailureInfo {
  code: TerminalAttestationCode;
  requiredMinVersion?: string;
  downloadHelpUrl?: string;
  observedConfigRequestRevision?: number;
}

interface AttestationFailureState {
  visible: boolean;
  code: TerminalAttestationCode | null;
  requiredMinVersion?: string;
  downloadHelpUrl?: string;
  failureRevision: number;
  observedConfigRequestRevision?: number;
  /**
   * Surface the attestation failure modal with the given info.
   * Actions live on the store (Concord frontend rule).
   */
  showFailure: (info: AttestationFailureInfo) => void;
  /** Dismiss the modal and reset all state. */
  dismiss: () => void;
  clearVersionFloorIfCurrent: (
    failureRevision: number,
    observedConfigRequestRevision: number
  ) => void;
}

export const useAttestationFailureStore = createStore<AttestationFailureState>()((set) => ({
  visible: false,
  code: null,
  requiredMinVersion: undefined,
  downloadHelpUrl: undefined,
  failureRevision: 0,
  observedConfigRequestRevision: undefined,

  showFailure: (info) =>
    set((state) => ({
      visible: true,
      code: info.code,
      requiredMinVersion: info.requiredMinVersion,
      downloadHelpUrl: info.downloadHelpUrl,
      observedConfigRequestRevision: info.observedConfigRequestRevision,
      failureRevision: state.failureRevision + 1,
    })),

  dismiss: () =>
    set({
      visible: false,
      code: null,
      requiredMinVersion: undefined,
      downloadHelpUrl: undefined,
      observedConfigRequestRevision: undefined,
    }),
  clearVersionFloorIfCurrent: (failureRevision, observedConfigRequestRevision) =>
    set((state) => {
      if (
        !state.visible ||
        state.code !== 'CLIENT_VERSION_TOO_OLD' ||
        state.failureRevision !== failureRevision ||
        state.observedConfigRequestRevision !== observedConfigRequestRevision
      ) {
        return {};
      }
      return {
        visible: false,
        code: null,
        requiredMinVersion: undefined,
        downloadHelpUrl: undefined,
        observedConfigRequestRevision: undefined,
      };
    }),
}));
