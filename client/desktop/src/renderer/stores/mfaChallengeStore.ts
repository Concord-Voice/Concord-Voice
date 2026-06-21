import { createStore } from '../utils/createStore';
import type { UserProfile } from './userStore';

/**
 * Purpose discriminates which interceptor / flow is awaiting the MFA result.
 * This is a client-side routing label only — the backend's challenge token
 * encodes its own purpose (PurposeLogin, PurposeSuspiciousRefresh,
 * PurposeMFAUpgrade per `services/control-plane/internal/mfa/handlers.go`)
 * and never sees this discriminator. The client uses 'sso_login' to route the
 * SSO post-verify flow even though the underlying challenge is PurposeLogin
 * on the wire.
 *
 * - 'suspicious_refresh': mid-session — apiClient interceptor caught a 401
 *   with mfa_required and is gating a refresh attempt.
 * - 'sso_login': SSO sign-in surfaced an mfa_required response from the
 *   provider callback; useSSOFlow awaits the verified result before flipping
 *   the SSO store to idle and rehydrating E2EE.
 *
 * MFAChallengeModal does NOT branch on purpose — both paths render the same
 * UI and POST to /api/v1/auth/mfa/verify. The discriminator exists so future
 * cancel-flow analytics or per-flow logging can act on it (no telemetry
 * pipeline exists today; Sentry was removed per project memory).
 */
type MFAChallengePurpose = 'suspicious_refresh' | 'sso_login';

/**
 * Shape of the POST /api/v1/auth/mfa/verify response body.
 *
 * IMPORTANT: response shape varies by the backend's challenge purpose
 * (see `completeVerifyPurpose` in `services/control-plane/internal/mfa/
 * handlers.go`):
 * - PurposeLogin → full payload with access_token, refresh_token, session_id,
 *   user, e2ee_keys, remember_me. SSO MFA challenges use this purpose.
 * - PurposeSuspiciousRefresh / PurposeMFAUpgrade → only { verified, purpose,
 *   user_id }. None of the fields below will be present.
 *
 * MFAChallengeModal forwards res.json() to completeChallenge unconditionally,
 * so a suspicious_refresh response WILL land in this type with everything
 * absent. All fields are therefore optional and callers MUST validate
 * before consuming. The Login.tsx password-path consumer at
 * `Login.completeLoginFromResponse` accepts `any` and does its own field
 * access — it is not typed against this interface.
 */
export interface MFAVerifyResponse {
  access_token?: string;
  refresh_token?: string;
  session_id?: string;
  user?: UserProfile;
  e2ee_keys?: {
    wrapped_private_key: string;
    key_derivation_salt: string;
    key_derivation_alg?: string;
    key_version?: number;
  };
  remember_me?: boolean;
}

/**
 * Result returned by showChallenge / passed to completeChallenge.
 *
 * Discriminated union eliminates the verified=true + no-payload phantom state
 * that previously allowed callers to silently drop into a half-authenticated
 * state. completeChallenge with { verified: true } MUST supply payload;
 * clearChallenge resolves with { verified: false }. Callers can narrow with a
 * single `if (result.verified)` check.
 */
export type MFAChallengeResult =
  | { verified: true; payload: MFAVerifyResponse }
  | { verified: false };

interface MFAChallengeState {
  /** The MFA challenge token from the server */
  challengeToken: string | null;
  /** Available MFA methods for this user */
  methods: string[];
  /** Methods restricted to recovery only (excluded from login/verification prompts) */
  recoveryOnlyMethods: string[];
  /** Purpose of the challenge (login handled separately — this is for mid-session or SSO) */
  purpose: MFAChallengePurpose | null;
  /**
   * WebAuthn challenge options when the server has issued a WebAuthn challenge.
   * Null when the WebAuthn flow is not active or the caller has not supplied
   * options. The MFAChallengeModal mounts WebAuthnPrompt only when this is
   * non-null (otherwise it shows the fallback message). Callers that surface
   * a WebAuthn-capable mfa_required response should use setState directly to
   * populate this field; the existing showChallenge signature is left
   * unchanged for source compatibility.
   */
  webauthnOptions: PublicKeyCredentialRequestOptions | null;
  /** Promise resolver — the interceptor awaits this */
  resolve: ((result: MFAChallengeResult) => void) | null;

  /** Present the MFA challenge modal and return a promise that resolves when verified */
  showChallenge: (
    token: string,
    methods: string[],
    purpose: MFAChallengePurpose,
    recoveryOnlyMethods?: string[]
  ) => Promise<MFAChallengeResult>;
  /** Called by the modal after successful MFA verification */
  completeChallenge: (result: MFAChallengeResult) => void;
  /** Clear the challenge state (cancel path) */
  clearChallenge: () => void;
}

export const useMFAChallengeStore = createStore<MFAChallengeState>()((set, get) => ({
  challengeToken: null,
  methods: [],
  recoveryOnlyMethods: [],
  purpose: null,
  webauthnOptions: null,
  resolve: null,

  showChallenge: (token, methods, purpose, recoveryOnlyMethods) => {
    return new Promise<MFAChallengeResult>((resolve) => {
      set({
        challengeToken: token,
        methods,
        recoveryOnlyMethods: recoveryOnlyMethods || [],
        purpose,
        webauthnOptions: null,
        resolve,
      });
    });
  },

  completeChallenge: (result) => {
    const { resolve } = get();
    if (resolve) resolve(result);
    set({
      challengeToken: null,
      methods: [],
      recoveryOnlyMethods: [],
      purpose: null,
      webauthnOptions: null,
      resolve: null,
    });
  },

  clearChallenge: () => {
    const { resolve } = get();
    if (resolve) resolve({ verified: false });
    set({
      challengeToken: null,
      methods: [],
      recoveryOnlyMethods: [],
      purpose: null,
      webauthnOptions: null,
      resolve: null,
    });
  },
}));
