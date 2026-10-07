import { apiFetch } from '../system/apiClient';
import {
  apiFetchInContext,
  captureApiRequestContext,
  isAbortError,
  type ApiRequestContext,
} from '../system/requestContext';
import {
  mintPasswordStepUpToken,
  passwordStepUpRefusalMessage,
  STEP_UP_TOKEN_EXPIRED_MESSAGE,
} from '../system/stepUpToken';

/**
 * The methods an `mfa_required` refusal named, for Clear's MFA prompt, exactly
 * as named: a refusal that names none, or none the prompt can verify, is the
 * picker's no-usable-method state. Substituting a code prompt there showed a
 * code box to an account that could not fill it.
 */
function namedMfaMethods(value: unknown): string[] {
  return Array.isArray(value)
    ? value.filter((method): method is string => typeof method === 'string')
    : [];
}

export type ClearFactor = { kind: 'password' | 'mfa'; value: string };

export type ClearHistoryResult =
  | {
      kind:
        | 'success'
        | 'passwordRequired'
        | 'invalidPassword'
        | 'invalidMfaCode'
        | 'stepUpImpossible'
        | 'sessionExpired'
        | 'notFound'
        | 'refused'
        | 'uncertain'
        /**
         * Clear never left: apiFetch refused to dispatch it because the account
         * or server changed after the capture. Nothing was sent, so nothing
         * was cleared; discard it rather than report a transport failure.
         */
        | 'aborted';
    }
  | { kind: 'rateLimited'; retryAfterSeconds?: number }
  /**
   * The account confirms with MFA. `methods` are the ones the server named,
   * unfiltered and possibly empty, so an account whose factor is a security
   * key is offered that ceremony (#3509).
   */
  | { kind: 'mfaRequired'; methods: string[] }
  /**
   * #3509: the password could not be exchanged for a step-up token, or Clear
   * refused the token it was given. Nothing was cleared; `message` belongs on
   * the password field.
   */
  | { kind: 'passwordRefused'; message: string };

export async function hideDMThread(id: string): Promise<boolean> {
  const response = await apiFetch(`/api/v1/dm/conversations/${id}/hide`, { method: 'POST' });
  return response.ok;
}

/**
 * Clear's body, or the refusal that stopped it first. A password never reaches
 * Clear (#3509): it is exchanged at the mint endpoint for a single-use token
 * bound to `dm.clear`, and Clear gets `{ step_up_token }`.
 *
 * `context` is the caller's capture, when it has one; the exchange and Clear
 * are admitted against it.
 */
async function clearRequestBody(
  factor: ClearFactor | undefined,
  context: ApiRequestContext | undefined
): Promise<{ body: object; context?: ApiRequestContext } | { refused: ClearHistoryResult }> {
  if (factor?.kind === 'mfa') return { body: { mfa_code: factor.value }, context };
  if (factor?.kind !== 'password') return { body: {}, context };
  // The exchange and Clear are one operation: Clear is admitted against this
  // capture, so it refuses to dispatch if another account or server took over
  // after the exchange (#3509 review).
  const operation = context ?? captureApiRequestContext();
  const minted = await mintPasswordStepUpToken(factor.value, 'dm.clear', operation);
  if (minted.kind === 'refused') {
    // The exchange never left, so there is nothing to say about the password.
    if (minted.unsent) return { refused: { kind: 'aborted' } };
    // A wrong password keeps Clear's own arm, and an account that enrolled MFA
    // after the prompt opened moves to Clear's code stage; every other mint
    // refusal carries its copy for the password field.
    if (minted.reason === 'invalidPassword') return { refused: { kind: 'invalidPassword' } };
    if (minted.reason === 'mfaRequired') {
      return { refused: { kind: 'mfaRequired', methods: minted.methods ?? [] } };
    }
    return { refused: { kind: 'passwordRefused', message: passwordStepUpRefusalMessage(minted) } };
  }
  return { body: { step_up_token: minted.token }, context: operation };
}

function forbiddenClearResult(
  payload: Record<string, unknown>,
  factor?: ClearFactor
): ClearHistoryResult {
  if (payload.password_required === true && payload.mfa_required === true) {
    return { kind: 'refused' };
  }
  if (payload.password_required === true && payload.step_up_token_invalid === true) {
    return { kind: 'passwordRefused', message: STEP_UP_TOKEN_EXPIRED_MESSAGE };
  }
  if (payload.password_required === true) return { kind: 'passwordRequired' };
  if (payload.mfa_required === true) {
    return { kind: 'mfaRequired', methods: namedMfaMethods(payload.methods) };
  }
  if (factor?.kind === 'password' && payload.error === 'Invalid password') {
    return { kind: 'invalidPassword' };
  }
  if (factor?.kind === 'mfa' && payload.error === 'Invalid MFA code') {
    return { kind: 'invalidMfaCode' };
  }
  return { kind: 'refused' };
}

function clearResponseResult(
  response: Response,
  payload: Record<string, unknown>,
  id: string,
  factor?: ClearFactor
): ClearHistoryResult {
  if (response.ok) {
    const validReceipt =
      payload.conversation_id === id &&
      typeof payload.cleared_at === 'string' &&
      !Number.isNaN(Date.parse(payload.cleared_at));
    return { kind: validReceipt ? 'success' : 'uncertain' };
  }
  if (response.status === 403) return forbiddenClearResult(payload, factor);
  if (response.status === 429) {
    const seconds = Number.parseInt(response.headers.get('Retry-After') ?? '', 10);
    return {
      kind: 'rateLimited',
      retryAfterSeconds: Number.isFinite(seconds) ? seconds : undefined,
    };
  }
  if (response.status === 401) return { kind: 'sessionExpired' };
  if (response.status === 404) return { kind: 'notFound' };
  // The flag says no step-up can pass (#3509 review); the copy's opening words
  // are what a server older than the flag sends.
  if (
    response.status === 400 &&
    (payload.step_up_unavailable === true ||
      (typeof payload.error === 'string' &&
        payload.error.startsWith('Clear history requires verification')))
  ) {
    return { kind: 'stepUpImpossible' };
  }
  return { kind: response.status >= 500 ? 'uncertain' : 'refused' };
}

/**
 * `context` is the operation Clear belongs to (`captureApiRequestContext`): the
 * credential stage passes the capture its factor was proven under, so the
 * proof is never sent as another account or to another server. Without one,
 * Clear is its own operation, as before.
 */
export async function clearDMHistory(
  id: string,
  factor?: ClearFactor,
  context?: ApiRequestContext
): Promise<ClearHistoryResult> {
  const request = await clearRequestBody(factor, context);
  if ('refused' in request) return request.refused;
  let response: Response;
  try {
    response = await apiFetchInContext(
      `/api/v1/dm/conversations/${id}/clear`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request.body),
      },
      request.context
    );
  } catch (err) {
    // Clear is sent with no signal and no dispatch guard, so the only
    // AbortError apiFetch can raise here is its pre-dispatch fence. Any other
    // rejection may have reached the server.
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'uncertain' };
  }

  const raw: unknown = await response.json().catch(() => null);
  const payload =
    raw !== null && typeof raw === 'object' && !Array.isArray(raw)
      ? (raw as Record<string, unknown>)
      : {};
  return clearResponseResult(response, payload, id, factor);
}
