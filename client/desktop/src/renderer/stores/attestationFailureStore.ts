import { createStore } from '../utils/createStore';

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
  | 'ATTESTATION_UNKNOWN_RELEASE'
  | 'ATTESTATION_REVOKED'
  | 'CLIENT_VERSION_TOO_OLD';

interface AttestationFailureInfo {
  code: TerminalAttestationCode;
  requiredMinVersion?: string;
  downloadHelpUrl?: string;
}

interface AttestationFailureState {
  visible: boolean;
  code: TerminalAttestationCode | null;
  requiredMinVersion?: string;
  downloadHelpUrl?: string;
  /**
   * Surface the attestation failure modal with the given info.
   * Actions live on the store (Concord frontend rule).
   */
  showFailure: (info: AttestationFailureInfo) => void;
  /** Dismiss the modal and reset all state. */
  dismiss: () => void;
}

export const useAttestationFailureStore = createStore<AttestationFailureState>()((set) => ({
  visible: false,
  code: null,
  requiredMinVersion: undefined,
  downloadHelpUrl: undefined,

  showFailure: (info) =>
    set({
      visible: true,
      code: info.code,
      requiredMinVersion: info.requiredMinVersion,
      downloadHelpUrl: info.downloadHelpUrl,
    }),

  dismiss: () =>
    set({
      visible: false,
      code: null,
      requiredMinVersion: undefined,
      downloadHelpUrl: undefined,
    }),
}));
