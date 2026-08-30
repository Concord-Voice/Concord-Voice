import { createStore } from '../../utils/createStore';
import type { UserProfile } from './userStore';
import type { CredentialOwner } from '../../../main/ipcContract';
import type { SSOCompletionResult, SSOProvider } from '../../services/ssoService';

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
 * MFAChallengeModal renders the same UI for both purposes but branches on it at
 * verification (#2424): 'suspicious_refresh' POSTs /api/v1/auth/mfa/verify from
 * the renderer (unchanged), while 'sso_login' routes the proof through the
 * `sso:completeMFA` main handler so the SSO refresh token never crosses IPC. The
 * SSO branch requires `ssoContext` (provider + reserved owner) to be present.
 */
type MFAChallengePurpose = 'suspicious_refresh' | 'sso_login';

/**
 * SSO-only routing context (#2424). Present on the store ONLY for the
 * 'sso_login' purpose, so MFAChallengeModal can call `sso:completeMFA` with the
 * owner reserved at sign-in. Absent (null) for 'suspicious_refresh'.
 */
export interface MFASSOContext {
  provider: SSOProvider;
  credentialOwner: CredentialOwner;
}

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
  // #2424: SSO MFA completes in main (sso:completeMFA); the renderer receives
  // only the sanitized access/session/owner tuple, never a refresh token.
  | { verified: true; ssoCompletion: SSOCompletionResult }
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
  /**
   * SSO routing context (#2424). Non-null only for the 'sso_login' purpose;
   * carries the provider + owner the modal needs to call `sso:completeMFA`.
   */
  ssoContext: MFASSOContext | null;
  /** Promise resolver — the interceptor awaits this */
  resolve: ((result: MFAChallengeResult) => void) | null;

  /** Present the MFA challenge modal and return a promise that resolves when verified */
  showChallenge: (
    token: string,
    methods: string[],
    purpose: MFAChallengePurpose,
    recoveryOnlyMethods?: string[],
    ssoContext?: MFASSOContext
  ) => Promise<MFAChallengeResult>;
  /**
   * Called by the modal after successful MFA verification. `forToken` (#2424)
   * binds the completion to the challenge it was collected against: if it no
   * longer matches the active `challengeToken`, the late result is ignored so a
   * stale challenge-A completion cannot resolve or clear a superseding
   * challenge B (AC-11). Omitted → unconditional (back-compat).
   */
  completeChallenge: (result: MFAChallengeResult, forToken?: string) => void;
  /** Clear the challenge state (cancel path) */
  clearChallenge: () => void;
}

export const useMFAChallengeStore = createStore<MFAChallengeState>()((set, get) => ({
  challengeToken: null,
  methods: [],
  recoveryOnlyMethods: [],
  purpose: null,
  webauthnOptions: null,
  ssoContext: null,
  resolve: null,

  showChallenge: (token, methods, purpose, recoveryOnlyMethods, ssoContext) => {
    return new Promise<MFAChallengeResult>((resolve) => {
      set({
        challengeToken: token,
        methods,
        recoveryOnlyMethods: recoveryOnlyMethods || [],
        purpose,
        webauthnOptions: null,
        ssoContext: ssoContext ?? null,
        resolve,
      });
    });
  },

  completeChallenge: (result, forToken) => {
    // AC-11: a completion collected against a superseded challenge must not
    // settle or clear the current one. Ignore it if the token no longer matches.
    if (forToken !== undefined && forToken !== get().challengeToken) return;
    const { resolve } = get();
    if (resolve) resolve(result);
    set({
      challengeToken: null,
      methods: [],
      recoveryOnlyMethods: [],
      purpose: null,
      webauthnOptions: null,
      ssoContext: null,
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
      ssoContext: null,
      resolve: null,
    });
  },
}));
