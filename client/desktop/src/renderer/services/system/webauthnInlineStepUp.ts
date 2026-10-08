/**
 * The inline WebAuthn step-up: begin, the browser ceremony, and finish.
 *
 * Begin asks the server for an assertion challenge bound to one step-up
 * `purpose`; finish exchanges the signed assertion for a single-use inline
 * token the server accepts only on that purpose's route (#3453 RS11). The
 * token is a credential: it is never logged or stored, and the caller sends it
 * with the one request it was minted for.
 *
 * The factor picker's `useStepUpFactor` uses these (design
 * 2026-09-26-mfa-factor-picker §4.1, D14). `context` is required: it admits
 * begin and finish against the caller's capture (`captureApiRequestContext`),
 * so both go to the server whose relying party begin was checked against, and
 * a switch of account or server between them is refused before dispatch.
 * `signal` cancels a request still in flight.
 */

import type { StepUpPurpose } from '../../components/Auth/stepUpPurpose';
import { bufferToBase64url } from '../../utils/crypto/base64url';
import { parseWebAuthnOptions } from '../../utils/webauthnOptions';
import { apiFetchInContext, type ApiRequestContext } from './requestContext';

export const WEBAUTHN_INLINE_BEGIN_PATH = '/api/v1/mfa/webauthn/verify-inline/begin';
export const WEBAUTHN_INLINE_FINISH_PATH = '/api/v1/mfa/webauthn/verify-inline/finish';

/** Begin's 400 for an account with no security key left (`mfa/handlers.go`). */
export const NO_WEBAUTHN_CREDENTIALS = 'No WebAuthn credentials registered';

/** Finish's 400 when begin's session expired or was replaced (`errMsgNoInlineSession`). */
export const NO_INLINE_SESSION = 'No verification session found. Start a new verification.';

/**
 * Begin or finish answered with a non-2xx status. `message` is the server's
 * `error` text, or the step's generic copy when it sent none; `serverError` is
 * that text alone, for matching the frozen server strings above exactly.
 */
export class WebAuthnInlineError extends Error {
  readonly step: 'begin' | 'finish';
  readonly status: number;
  readonly serverError: string | null;

  constructor(step: 'begin' | 'finish', status: number, body: unknown, fallback: string) {
    const serverError = errorText(body);
    super(serverError ?? fallback);
    this.name = 'WebAuthnInlineError';
    this.step = step;
    this.status = status;
    this.serverError = serverError;
  }
}

/** The `error` string of a JSON body, when it is a non-empty string. */
function errorText(body: unknown): string | null {
  if (typeof body !== 'object' || body === null) return null;
  const error = (body as { error?: unknown }).error;
  return typeof error === 'string' && error !== '' ? error : null;
}

/** A JSON POST, carrying `signal` only when there is one. */
function postJson(body: unknown, signal: AbortSignal | undefined): RequestInit {
  const init: RequestInit = {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  };
  if (signal) init.signal = signal;
  return init;
}

/**
 * Asks the server for an assertion challenge for `purpose`. The token finish
 * mints is spendable on that purpose's route only, so `purpose` must name the
 * request the token will be sent with. The server refuses a null purpose.
 */
export async function beginWebAuthnInlineVerification(
  purpose: StepUpPurpose | null,
  context: ApiRequestContext,
  signal?: AbortSignal
): Promise<PublicKeyCredentialRequestOptions> {
  // The server apiFetch sends begin and finish to.
  const apiBase = context.serverSelection.apiBase;
  const res = await apiFetchInContext(
    WEBAUTHN_INLINE_BEGIN_PATH,
    postJson({ purpose }, signal),
    context
  );
  // A non-JSON body (a proxy's HTML 502) must still reach the status checks,
  // or a 401 loses its session-expired meaning.
  const data: unknown = await res.json().catch(() => null);
  if (!res.ok) {
    throw new WebAuthnInlineError('begin', res.status, data, 'Failed to start verification');
  }
  // Every server shares one renderer origin, so the browser's own rpId check
  // cannot tell them apart. The rpId must be the relying party of the server
  // the proof goes to, or a server could name another server's relying party
  // and relay the assertion. Unknown keys (extensions, hints) are dropped. A
  // refusal throws, so the ceremony never runs.
  return parseWebAuthnOptions((data as { publicKey?: unknown } | null)?.publicKey, apiBase);
}

/** Performs the browser WebAuthn assertion ceremony. */
export async function performWebAuthnAssertion(
  options: PublicKeyCredentialRequestOptions,
  signal: AbortSignal
): Promise<PublicKeyCredential> {
  const credential = (await navigator.credentials.get({
    publicKey: options,
    signal,
  })) as PublicKeyCredential;
  if (!credential) throw new Error('No credential returned');
  return credential;
}

/** Sends the assertion to the server and returns the inline token it mints. */
export async function finishWebAuthnVerification(
  credential: PublicKeyCredential,
  context: ApiRequestContext,
  signal?: AbortSignal
): Promise<string> {
  const assertion = credential.response as AuthenticatorAssertionResponse;
  const res = await apiFetchInContext(
    WEBAUTHN_INLINE_FINISH_PATH,
    postJson(
      {
        id: credential.id,
        rawId: bufferToBase64url(credential.rawId),
        type: credential.type,
        response: {
          authenticatorData: bufferToBase64url(assertion.authenticatorData),
          clientDataJSON: bufferToBase64url(assertion.clientDataJSON),
          signature: bufferToBase64url(assertion.signature),
          userHandle: assertion.userHandle ? bufferToBase64url(assertion.userHandle) : undefined,
        },
      },
      signal
    ),
    context
  );
  const data: unknown = await res.json().catch(() => null);
  if (!res.ok) throw new WebAuthnInlineError('finish', res.status, data, 'Verification failed');
  const token = (data as { mfa_token?: unknown } | null)?.mfa_token;
  // A 2xx without a token proves nothing; never report it as verified.
  if (typeof token !== 'string' || token === '') {
    throw new WebAuthnInlineError('finish', res.status, data, 'Verification failed');
  }
  return token;
}
