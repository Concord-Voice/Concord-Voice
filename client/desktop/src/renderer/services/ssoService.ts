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

import type { CredentialOwner } from '../../main/ipcContract';
import type { RuntimeServerSelection } from './runtimeServerBase';
import type { AppleSignInResult } from '@/shared/appleSso';
import type {
  SSOCompleteLinkPayload,
  SSOCompleteMFAPayload,
  SSOCompletionResult as IPCSSOCompletionResult,
  SSOCompleteRegistrationPayload,
  SSOSignInResult,
} from '@/shared/sso';

/** Upper bound on the #2394 abandon IPC, so a wedged main process cannot hang a registration submit. */
const ABANDON_RESERVATION_TIMEOUT_MS = 3000;

export type SSOProvider = 'google' | 'apple';

export interface SSOCompletionResult {
  accessToken: string;
  sessionId: string;
  credentialOwner: CredentialOwner;
}

export type SSOResult =
  | {
      kind: 'logged_in';
      accessToken: string;
      sessionId: string;
      credentialOwner: CredentialOwner;
    }
  | {
      kind: 'mfa_required';
      mfaChallengeToken: string;
      /** Login-eligible methods (drives MFAChallengeModal layout) */
      methods: string[];
      /** Methods enrolled but disqualified for login (e.g. backup_code) */
      recoveryOnlyMethods?: string[];
      /** PublicKeyCredentialRequestOptions when "webauthn" is in methods */
      webauthnOptions?: unknown;
      /**
       * #2424: the opaque owner reserved at SSO sign-in, preserved across the MFA
       * challenge. The renderer carries it to `sso:completeMFA` so main stores the
       * resulting refresh credential under this exact owner — the refresh token
       * never returns to the renderer.
       */
      credentialOwner: CredentialOwner;
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

function unwrapCompletionResult(
  result: IPCSSOCompletionResult,
  failurePrefix: string
): SSOCompletionResult {
  if (result.kind === 'error') {
    throw new SSOServiceError(
      result.status,
      `${failurePrefix}_${result.status}`,
      result.body ? { ...result.body } : { error_code: result.code }
    );
  }
  return {
    accessToken: result.accessToken,
    sessionId: result.sessionId,
    credentialOwner: result.credentialOwner,
  };
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
      return {
        kind: 'logged_in',
        accessToken: result.accessToken,
        sessionId: result.sessionId,
        credentialOwner: result.credentialOwner,
      };
    case 'mfa_challenge':
      return {
        kind: 'mfa_required',
        mfaChallengeToken: result.mfaChallengeToken,
        methods: result.methods,
        recoveryOnlyMethods: result.recoveryOnlyMethods,
        webauthnOptions: result.webauthnOptions,
        credentialOwner: result.credentialOwner,
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
export async function startSSOFlow(provider: SSOProvider, apiBase: string): Promise<SSOResult> {
  if (provider === 'apple') {
    const result: AppleSignInResult = await globalThis.electron.sso.appleSignIn(apiBase);
    return mapSSOResult(result);
  }
  const result: SSOSignInResult = await globalThis.electron.sso.googleSignIn(apiBase);
  return mapSSOResult(result);
}

/**
 * Complete first-time SSO registration. Submits the user's chosen username,
 * passphrase, and E2EE key material (wrapped private key + salt + public key)
 * along with the short-lived `sso_token` returned by the callback step.
 *
 * Returns a renderer-safe access/session reference plus the opaque main-process
 * credential owner. The refresh token never crosses the IPC boundary.
 */
export async function completeSSORegistration(
  params: SSOCompleteRegistrationPayload,
  serverSelection: RuntimeServerSelection
): Promise<SSOCompletionResult> {
  return unwrapCompletionResult(
    await globalThis.electron.sso.completeRegistration(serverSelection.apiBase, params),
    'sso_complete_registration_failed'
  );
}

/**
 * Link an SSO identity to an existing password-authenticated account.
 * The caller must provide the existing account password to authorize the link.
 */
export async function completeSSOLink(
  params: SSOCompleteLinkPayload,
  serverSelection: RuntimeServerSelection
): Promise<SSOCompletionResult> {
  return unwrapCompletionResult(
    await globalThis.electron.sso.completeLink(serverSelection.apiBase, params),
    'sso_complete_link_failed'
  );
}

/**
 * Complete an SSO MFA challenge (#2424). Submits the MFA proof (TOTP/backup code
 * or WebAuthn assertion) plus the owner reserved at sign-in to the `sso:completeMFA`
 * main handler, which performs the /auth/mfa/verify exchange in main and stores
 * the refresh credential under that owner. Returns only a renderer-safe
 * access/session reference plus the owner — the refresh token never crosses IPC.
 */
export async function completeSSOMFA(
  params: SSOCompleteMFAPayload,
  serverSelection: RuntimeServerSelection
): Promise<SSOCompletionResult> {
  return unwrapCompletionResult(
    await globalThis.electron.sso.completeMFA(serverSelection.apiBase, params),
    'sso_complete_mfa_failed'
  );
}

/**
 * Release an orphaned main-process SSO credential reservation (#2394).
 *
 * NEVER throws. Callers invoke it on paths where a rejection would abort
 * user-visible work — the password-registration submit, a modal cancel — so a
 * failure here must degrade, not propagate.
 *
 * An absent bridge (a shell older than IPC contract v21) resolves `false`,
 * degrading to the pre-#2394 behaviour: E2EE keys stay session-only and are
 * recovered by unlock or re-login. This deliberately does NOT follow the
 * fail-closed shape in `persistE2EESessionKeys` — there, an unavailable
 * owner-scoped method must never fall through to the generic writer, because
 * that would be a custody downgrade. Here, absence costs restart-survival
 * only, so degrading is correct.
 */
export async function abandonSSOReservation(): Promise<boolean> {
  const invoke = globalThis.electron?.sso?.abandonReservation;
  if (!invoke) return false;
  // The helper cannot reject, but `ipcRenderer.invoke` can fail to SETTLE if the
  // main process is wedged — and Register.handleSubmit awaits this before doing
  // anything else, so an unsettled promise would hang the submit with
  // isSubmitting stuck true and no error surfaced. Bound it: on timeout we
  // report `false`, which is the same "could not release" answer an older shell
  // gives, and registration proceeds.
  let timer: ReturnType<typeof setTimeout> | undefined;
  try {
    return await Promise.race([
      invoke(),
      new Promise<boolean>((resolve) => {
        timer = setTimeout(() => resolve(false), ABANDON_RESERVATION_TIMEOUT_MS);
      }),
    ]);
  } catch {
    return false;
  } finally {
    if (timer !== undefined) clearTimeout(timer);
  }
}
