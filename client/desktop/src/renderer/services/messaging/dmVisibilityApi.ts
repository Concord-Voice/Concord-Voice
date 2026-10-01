import { apiFetch } from '../system/apiClient';
import {
  apiFetchInContext,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../system/requestContext';
import {
  mintPasswordStepUpToken,
  passwordStepUpRefusalMessage,
  STEP_UP_TOKEN_EXPIRED_MESSAGE,
} from '../system/stepUpToken';

/**
 * The methods an `mfa_required` refusal named, for Clear's MFA prompt. A
 * refusal that names none still gets a code prompt, as before the methods
 * were read at all.
 */
function namedMfaMethods(value: unknown): string[] {
  const methods = Array.isArray(value)
    ? value.filter((method): method is string => typeof method === 'string')
    : [];
  return methods.length > 0 ? methods : ['totp'];
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
        | 'uncertain';
    }
  | { kind: 'rateLimited'; retryAfterSeconds?: number }
  /**
   * The account confirms with MFA. `methods` are the ones the server named, so
   * an account whose factor is a security key is offered that ceremony (#3509).
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
 */
async function clearRequestBody(
  factor?: ClearFactor
): Promise<{ body: object; context?: ApiRequestContext } | { refused: ClearHistoryResult }> {
  if (factor?.kind === 'mfa') return { body: { mfa_code: factor.value } };
  if (factor?.kind !== 'password') return { body: {} };
  // The exchange and Clear are one operation: Clear is admitted against this
  // capture, so it refuses to dispatch if another account or server took over
  // after the exchange (#3509 review).
  const context = captureApiRequestContext();
  const minted = await mintPasswordStepUpToken(factor.value, 'dm.clear', context);
  if (minted.kind === 'refused') {
    // A wrong password keeps Clear's own arm, and an account that enrolled MFA
    // after the prompt opened moves to Clear's code stage; every other mint
    // refusal carries its copy for the password field.
    if (minted.reason === 'invalidPassword') return { refused: { kind: 'invalidPassword' } };
    if (minted.reason === 'mfaRequired') {
      return { refused: { kind: 'mfaRequired', methods: namedMfaMethods(minted.methods) } };
    }
    return { refused: { kind: 'passwordRefused', message: passwordStepUpRefusalMessage(minted) } };
  }
  return { body: { step_up_token: minted.token }, context };
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

export async function clearDMHistory(
  id: string,
  factor?: ClearFactor
): Promise<ClearHistoryResult> {
  const request = await clearRequestBody(factor);
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
  } catch {
    return { kind: 'uncertain' };
  }

  const raw: unknown = await response.json().catch(() => null);
  const payload =
    raw !== null && typeof raw === 'object' && !Array.isArray(raw)
      ? (raw as Record<string, unknown>)
      : {};
  return clearResponseResult(response, payload, id, factor);
}
