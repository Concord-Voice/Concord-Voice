/**
 * SSO Service (#270, #974, #975)
 *
 * Both providers (apple and google) are fully main-process-driven. The
 * renderer calls a single IPC invoke — `sso:appleSignIn` (#974) or
 * `sso:googleSignIn` (#975) — which runs PKCE, loopback, token exchange,
 * local ID-token verification, and the /session POST entirely in the main
 * process. No OAuth material (authorization code, client_secret, ID token)
 * crosses IPC in either direction.
 *
 * `startSSOFlow` receives the discriminated `SSOSignInResult` / `AppleSignInResult`
 * from IPC and maps it to the renderer-local `SSOResult` union so callers (the
 * `useSSOFlow` hook) can dispatch into the right next-step UI: logged in
 * directly, MFA challenge, first-time SSO registration, or link-an-existing-
 * account flow.
 *
 * Privacy: this module never logs the SSO token, MFA token, or access token.
 * Errors propagate as `Error` with stable string codes (e.g.
 * `google_id_token_invalid`) so UI can localize without inspecting payload.
 */

import { apiFetch, safeJson } from './apiClient';

import type { AppleSignInResult } from '@/shared/appleSso';
import type { SSOSignInResult } from '@/shared/sso';

export type SSOProvider = 'google' | 'apple';

export type SSOResult =
  | { kind: 'logged_in'; accessToken: string }
  | {
      kind: 'mfa_required';
      mfaChallengeToken: string;
      /** Login-eligible methods (drives MFAChallengeModal layout) */
      methods: string[];
      /** Methods enrolled but disqualified for login (e.g. backup_code) */
      recoveryOnlyMethods?: string[];
      /** PublicKeyCredentialRequestOptions when "webauthn" is in methods */
      webauthnOptions?: unknown;
    }
  | { kind: 'register_required'; ssoToken: string; email: string; name?: string }
  | { kind: 'link_available'; ssoToken: string; maskedEmail: string };

/**
 * SSOServiceError carries the server response body for callers that need to
 * map error_code values to localized UX. Thrown from the *complete-* helpers
 * when the HTTP response is non-2xx; the body is best-effort parsed JSON.
 *
 * Plain `Error` was previously thrown with a synthetic message
 * (`sso_complete_registration_failed_400`), discarding the server's
 * `error_code` and `detail` fields. Components had to re-fetch or hard-code a
 * mapping. SSOServiceError keeps the structured payload so e.g. the passphrase
 * setup screen can render "username already taken" vs "weak password" vs
 * generic "registration failed".
 */
export class SSOServiceError extends Error {
  status: number;
  body: Record<string, unknown> | null;
  constructor(status: number, message: string, body: Record<string, unknown> | null) {
    super(message);
    this.name = 'SSOServiceError';
    this.status = status;
    this.body = body;
  }
}

/**
 * Maps a discriminated SSOSignInResult / AppleSignInResult (both are the same
 * type after #975 made AppleSignInResult a re-export alias) onto the renderer's
 * SSOResult union. The `error` kind throws the stable taxonomy code so
 * useSSOFlow's catch surfaces `{ phase: 'error', message }`.
 */
function mapSSOResult(result: SSOSignInResult): SSOResult {
  switch (result.kind) {
    case 'tokens':
      return { kind: 'logged_in', accessToken: result.accessToken };
    case 'mfa_challenge':
      return {
        kind: 'mfa_required',
        mfaChallengeToken: result.mfaChallengeToken,
        methods: result.methods,
        recoveryOnlyMethods: result.recoveryOnlyMethods,
        webauthnOptions: result.webauthnOptions,
      };
    case 'sso_token':
      if (result.branch === 'new_user') {
        return {
          kind: 'register_required',
          ssoToken: result.ssoToken,
          email: result.email,
          name: result.name,
        };
      }
      return {
        kind: 'link_available',
        ssoToken: result.ssoToken,
        maskedEmail: result.maskedEmail,
      };
    case 'error':
      throw new Error(result.code);
  }
}

/**
 * Begin an SSO flow for the given provider.
 *
 * Both providers are fully main-process-driven (#974 apple, #975 google).
 * One IPC invoke runs PKCE, loopback, token exchange, local ID-token
 * verification, and the /session POST in the main process; the renderer
 * receives only the final discriminated result mapped onto SSOResult.
 */
export async function startSSOFlow(provider: SSOProvider): Promise<SSOResult> {
  if (provider === 'apple') {
    const result: AppleSignInResult = await globalThis.electron.sso.appleSignIn();
    return mapSSOResult(result);
  }
  const result: SSOSignInResult = await globalThis.electron.sso.googleSignIn();
  return mapSSOResult(result);
}

/**
 * Complete first-time SSO registration. Submits the user's chosen username,
 * passphrase, and E2EE key material (wrapped private key + salt + public key)
 * along with the short-lived `sso_token` returned by the callback step.
 *
 * Returns a fresh access token on success.
 */
export async function completeSSORegistration(params: {
  provider: SSOProvider;
  ssoToken: string;
  username: string;
  passphrase: string;
  /** base64-encoded wrapped private key */
  wrappedPrivateKey: string;
  /** base64-encoded Argon2id salt used to derive the wrapping key */
  keyDerivationSalt: string;
  /** base64-encoded RSA public key */
  publicKey: string;
}): Promise<{ accessToken: string }> {
  const res = await apiFetch(`/api/v1/auth/sso/${params.provider}/complete-registration`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      sso_token: params.ssoToken,
      username: params.username,
      password: params.passphrase,
      wrapped_private_key: params.wrappedPrivateKey,
      key_derivation_salt: params.keyDerivationSalt,
      public_key: params.publicKey,
    }),
  });
  if (!res.ok) {
    // Best-effort body parse so the caller can read error_code / detail.
    // safeJson throws on parse failure → fall back to null body, not a fatal.
    let body: Record<string, unknown> | null = null;
    try {
      body = await safeJson<Record<string, unknown>>(res);
    } catch {
      body = null;
    }
    throw new SSOServiceError(res.status, `sso_complete_registration_failed_${res.status}`, body);
  }
  const data = await safeJson<{ access_token: string }>(res);
  return { accessToken: data.access_token };
}

/**
 * Link an SSO identity to an existing password-authenticated account.
 * The caller must provide the existing account password to authorize the link.
 */
export async function completeSSOLink(params: {
  provider: SSOProvider;
  ssoToken: string;
  password: string;
}): Promise<{ accessToken: string }> {
  const res = await apiFetch(`/api/v1/auth/sso/${params.provider}/complete-link`, {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      sso_token: params.ssoToken,
      password: params.password,
    }),
  });
  if (!res.ok) {
    let body: Record<string, unknown> | null = null;
    try {
      body = await safeJson<Record<string, unknown>>(res);
    } catch {
      body = null;
    }
    throw new SSOServiceError(res.status, `sso_complete_link_failed_${res.status}`, body);
  }
  const data = await safeJson<{ access_token: string }>(res);
  return { accessToken: data.access_token };
}
