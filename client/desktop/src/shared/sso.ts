// Shared SSO sign-in result + error-code union for client-driven OAuth flows
// (Apple #974, Google #975). Provider-specific error codes coexist; the
// discriminated-union SHAPE is identical across providers because the server
// `/session` endpoint returns the same response variants for every provider.
//
// Renamed from AppleSignInResult (#975 §Design): appleSso.ts re-exports these
// under the legacy names during migration.

import type { CredentialOwner } from '../main/ipcContract';

export type SSOSignInErrorCode =
  | 'sso_initiate_failed'
  | 'sso_cancelled'
  | `oauth_provider_error:${string}`
  | 'sso_state_expired'
  | 'sso_rate_limited'
  | 'sso_session_rejected'
  // Apple-specific (#974)
  | 'apple_exchange_rejected'
  | 'apple_unavailable'
  | 'apple_verification_unavailable'
  | 'apple_id_token_invalid'
  // Google-specific (#975)
  | 'google_exchange_rejected'
  | 'google_unavailable'
  | 'google_verification_unavailable'
  | 'google_id_token_invalid';

export type SSOSignInResult =
  | {
      kind: 'tokens';
      accessToken: string;
      sessionId: string;
      credentialOwner: CredentialOwner;
    }
  | {
      kind: 'mfa_challenge';
      mfaChallengeToken: string;
      methods: string[];
      recoveryOnlyMethods?: string[];
      webauthnOptions?: unknown;
      // #2424: the SSO CredentialOwner reserved at sign-in is preserved across the
      // MFA challenge and returned as opaque challenge context. The renderer must
      // pass it back to sso:completeMFA so main can conditionally store the MFA
      // credential under that exact owner — the raw refresh token never returns
      // to the renderer.
      credentialOwner: CredentialOwner;
    }
  | { kind: 'sso_token'; branch: 'new_user'; ssoToken: string; email: string; name?: string }
  | { kind: 'sso_token'; branch: 'account_link'; ssoToken: string; maskedEmail: string }
  | { kind: 'error'; code: SSOSignInErrorCode };

export interface SSOCompleteRegistrationPayload {
  provider: 'google' | 'apple';
  ssoToken: string;
  username: string;
  passphrase: string;
  wrappedPrivateKey: string;
  keyDerivationSalt: string;
  publicKey: string;
}

export interface SSOCompleteLinkPayload {
  provider: 'google' | 'apple';
  ssoToken: string;
  password: string;
}

/**
 * #2424: the SSO MFA proof the renderer routes to the `sso:completeMFA` main
 * handler. Main re-validates the reserved owner, submits the proof to
 * `/api/v1/auth/mfa/verify`, and conditionally stores the resulting credential
 * under that owner — no refresh token crosses back to the renderer. `code` is
 * the TOTP/backup value; `assertion` is the WebAuthn assertion collected by the
 * renderer's ceremony (main cannot perform `navigator.credentials.get`).
 */
export interface SSOCompleteMFAPayload {
  provider: 'google' | 'apple';
  mfaChallengeToken: string;
  credentialOwner: CredentialOwner;
  method: string;
  code?: string;
  assertion?: unknown;
}

export interface SSOCompletionErrorBody {
  error_code?: string;
  error?: string;
  detail?: string;
  attempts_remaining?: number;
  retry_after_seconds?: number;
}

/** Renderer-safe result: refresh credentials remain exclusively in main. */
export type SSOCompletionResult =
  | {
      kind: 'tokens';
      accessToken: string;
      sessionId: string;
      credentialOwner: CredentialOwner;
    }
  | {
      kind: 'error';
      status: number;
      code: string;
      body?: SSOCompletionErrorBody;
    };
