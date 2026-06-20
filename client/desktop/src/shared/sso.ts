// Shared SSO sign-in result + error-code union for client-driven OAuth flows
// (Apple #974, Google #975). Provider-specific error codes coexist; the
// discriminated-union SHAPE is identical across providers because the server
// `/session` endpoint returns the same response variants for every provider.
//
// Renamed from AppleSignInResult (#975 §Design): appleSso.ts re-exports these
// under the legacy names during migration.

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
  | { kind: 'tokens'; accessToken: string }
  | {
      kind: 'mfa_challenge';
      mfaChallengeToken: string;
      methods: string[];
      recoveryOnlyMethods?: string[];
      webauthnOptions?: unknown;
    }
  | { kind: 'sso_token'; branch: 'new_user'; ssoToken: string; email: string; name?: string }
  | { kind: 'sso_token'; branch: 'account_link'; ssoToken: string; maskedEmail: string }
  | { kind: 'error'; code: SSOSignInErrorCode };
