/**
 * The password step-up token exchange (#3509).
 *
 * The own-rule routes — DM Clear, the channel and DM message deletes, and the
 * channel and server self-purges — never receive the account password. A
 * password-only account proves it once, here, at
 * `POST /api/v1/auth/step-up/password`, which answers a single-use token bound
 * to the one route's purpose for 60 seconds; the caller then retries that
 * route with `{ step_up_token }`. This module is the only place the renderer
 * sends a password to an own-rule purpose, and the only place a mint refusal
 * is read, so every flow puts the same words on its password field.
 *
 * The password is never logged, stored, or kept past the request.
 */

import { apiFetchInContext, isAbortError, type ApiRequestContext } from './requestContext';
import type { PasswordStepUpPurpose } from '../../components/Auth/stepUpPurpose';

export const STEP_UP_PASSWORD_PATH = '/api/v1/auth/step-up/password';

/** Why the mint refused. Each lands on the password field as a message. */
export type PasswordStepUpRefusal =
  | 'invalidPassword'
  /** The account's shared login lockout (423), or a rate limit (429). */
  | 'tooManyAttempts'
  /**
   * The account gained an authenticator since the route asked for a password.
   * A flow that can confirm with a code moves to its code prompt instead; the
   * message is the fallback for one that cannot.
   */
  | 'mfaRequired'
  /** 404/405: a server older than the step-up endpoint (version skew). */
  | 'unsupported'
  | 'failed';

export type PasswordStepUpMint =
  | { kind: 'minted'; token: string }
  | {
      kind: 'refused';
      reason: PasswordStepUpRefusal;
      retryAfterSeconds?: number;
      /** On `mfaRequired`: the inline methods the mint named (`mfa_methods`). */
      methods?: string[];
      /**
       * The exchange never left: apiFetch refused to dispatch it because the
       * account or server changed after the capture. Nothing was checked, so a
       * flow that can tell a password failure from a request that was not made
       * discards the attempt instead of blaming the password.
       */
      unsent?: true;
    };

/** The exact refusal body `stepup.ErrMsgInvalidPassword` sends. */
const INVALID_PASSWORD = 'Invalid password'; // pragma: allowlist secret

/**
 * The copy for the route's own `step_up_token_invalid` refusal: the token was
 * spent, expired, or stranded by a credential change. Shown on the password
 * field, which the prompt has already emptied for a fresh attempt.
 */
export const STEP_UP_TOKEN_EXPIRED_MESSAGE =
  'Your confirmation expired. Enter your password again.';

function parseRetryAfter(header: string | null): number | undefined {
  if (header === null) return undefined;
  const parsed = Number.parseInt(header, 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined;
}

function refusalReason(status: number, body: Record<string, unknown>): PasswordStepUpRefusal {
  if (status === 404 || status === 405) return 'unsupported';
  if (status === 423 || status === 429) return 'tooManyAttempts';
  if (status === 403 && body.mfa_required === true) return 'mfaRequired';
  if (status === 403 && body.error === INVALID_PASSWORD) return 'invalidPassword';
  return 'failed';
}

/**
 * Exchanges `password` for a single-use token bound to `purpose`. Never
 * throws: a transport failure is a `failed` refusal, so a caller can always
 * put something on the password field. A refusal apiFetch raised before
 * dispatch is the same `failed` refusal marked `unsent`.
 *
 * `context` is the operation the token is minted for (`captureApiRequestContext`):
 * the caller passes the same context to the request that spends the token, so
 * that request is refused before dispatch if another account or server took
 * over in between (#3509 review).
 */
export async function mintPasswordStepUpToken(
  password: string,
  purpose: PasswordStepUpPurpose,
  context?: ApiRequestContext
): Promise<PasswordStepUpMint> {
  let res: Response;
  try {
    res = await apiFetchInContext(
      STEP_UP_PASSWORD_PATH,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ current_password: password, purpose }),
      },
      context
    );
  } catch (err) {
    // The exchange is sent with no signal, so the only AbortError apiFetch can
    // raise here is its pre-dispatch fence: nothing reached the server.
    return isAbortError(err)
      ? { kind: 'refused', reason: 'failed', unsent: true }
      : { kind: 'refused', reason: 'failed' };
  }
  // An unreadable body (a proxy's HTML error page) is an empty one.
  const raw: unknown = await res.json().catch((): unknown => ({}));
  const body = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : {};
  if (res.ok && typeof body.step_up_token === 'string' && body.step_up_token !== '') {
    return { kind: 'minted', token: body.step_up_token };
  }
  const reason = res.ok ? 'failed' : refusalReason(res.status, body);
  const refused: PasswordStepUpMint = {
    kind: 'refused',
    reason,
    retryAfterSeconds: parseRetryAfter(res.headers.get('Retry-After')),
  };
  if (reason === 'mfaRequired' && Array.isArray(body.mfa_methods)) {
    refused.methods = body.mfa_methods.filter((m): m is string => typeof m === 'string');
  }
  return refused;
}

/**
 * The code prompt's methods when the mint refused because the account now
 * confirms with MFA and named its methods, else null.
 */
export function mintMfaMethods(
  refusal: Extract<PasswordStepUpMint, { kind: 'refused' }>
): string[] | null {
  return refusal.reason === 'mfaRequired' && refusal.methods && refusal.methods.length > 0
    ? refusal.methods
    : null;
}

/** The password-field copy for a mint refusal. */
export function passwordStepUpRefusalMessage(
  refusal: Extract<PasswordStepUpMint, { kind: 'refused' }>
): string {
  switch (refusal.reason) {
    case 'invalidPassword':
      return 'That password is not correct.';
    case 'tooManyAttempts':
      return refusal.retryAfterSeconds === undefined
        ? 'Too many attempts. Try again later.'
        : `Too many attempts. Try again in ${refusal.retryAfterSeconds} seconds.`;
    case 'mfaRequired':
      return 'This account now confirms with an authenticator app or security key. Close this and try again.';
    case 'unsupported':
      return "This server doesn't support this confirmation yet.";
    case 'failed':
      return "We couldn't check your password. Try again.";
  }
}
