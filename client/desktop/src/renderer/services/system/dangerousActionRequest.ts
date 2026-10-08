/**
 * The request half of a dangerous-action (D1) host (#3456 §3.3, §3.4).
 *
 * A gated destructive route is sent at most twice. The first send is the host's
 * own request with no code, unchanged by this feature: where the server does not
 * enforce, or the actor is not caught by the gate, it is the whole story. A
 * refusal that asks for verification freezes that request, and the step-up
 * dialog re-sends exactly it with `mfa_code` in the JSON body, which is where
 * every D1 route reads it (`stepup.ReadOptionalStepUp`, and the ban and kick
 * request structs). The host never re-reads its form for the second send.
 */
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import { safeJson } from './apiClient';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from './requestContext';
import { adaptDangerousActionRefusal, serverErrorText } from './stepUpRouteAdapters';

/** A request as the host sent it first. `mfa_code` joins its body on the re-send and nowhere else. */
export interface FrozenRequest {
  readonly path: string;
  readonly method: 'DELETE' | 'POST';
  /** Sent as JSON. Absent: no body at all, as the host sent it before #3456. */
  readonly body?: Readonly<Record<string, unknown>>;
}

/**
 * How a request goes out, admitted against `context`. Null means the host's own
 * last-moment check refused it and nothing was sent: a moderation purge that
 * promises to keep pins rechecks that promise right before each send (#3458).
 */
export type Dispatch = (
  path: string,
  init: RequestInit,
  context: ApiRequestContext
) => Promise<Response | null>;

const plainDispatch: Dispatch = (path, init, context) => apiFetchInContext(path, init, context);

function initFor(request: FrozenRequest, mfaCode?: string): RequestInit {
  if (request.body === undefined && mfaCode === undefined) return { method: request.method };
  return {
    method: request.method,
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(
      mfaCode === undefined ? request.body : { ...request.body, mfa_code: mfaCode }
    ),
  };
}

/**
 * A refused response's body, or null when it is not JSON. `safeJson` throws on a
 * non-JSON content type as well as on a parse failure, so a proxy's HTML 502
 * would otherwise put its own message in front of the user in place of ours.
 */
async function readBody(res: Response): Promise<unknown> {
  return safeJson(res).catch(() => null);
}

/**
 * The refusals that open the dialog: a server that wants a verified factor, and
 * an actor with none to give. Every other answer, including the adapter's own
 * 429 and 503, is the host's to word as it always was.
 *
 * `gated` is false for a request the server never gates (a plain kick). It can
 * only be refused for lack of enrolment (the permission mask's answer), never
 * asked for a code, so `mfaRequired` is not read as a prompt there.
 */
function verificationRefusal(
  status: number,
  body: unknown,
  gated: boolean
): StepUpFactorRefusal | null {
  const refusal = adaptDangerousActionRefusal(status, body);
  if (refusal === null) return null;
  if (refusal.kind === 'enrollmentRequired') return refusal;
  return refusal.kind === 'mfaRequired' && gated ? refusal : null;
}

/**
 * `context` is the account and server the first send went out as. The dialog
 * re-sends against it and nothing else (C82): a refusal that asked account A
 * for a code must never re-send A's frozen request as account B.
 */
export type FirstSendResult =
  | { kind: 'ok'; response: Response }
  | { kind: 'stepUp'; refusal: StepUpFactorRefusal; context: ApiRequestContext };

/** `ConfirmActionModal`'s sentence for a refusal that came back to a different session. */
export const FIRST_SEND_SESSION_CHANGED = 'Your session has expired. Sign in again to continue.';

export interface SendFirstOptions {
  /** False for a request the server never gates (a plain kick); see `verificationRefusal`. */
  gated?: boolean;
  dispatch?: Dispatch;
  /** The sentence for a send `dispatch` refused. Omitted, `failureMessage`. */
  unsentMessage?: string;
}

/**
 * Sends the request with no code, as the account and server current now. A
 * success, or a refusal that asks for verification, is returned. Anything else
 * throws an `Error` carrying the server's own sentence, or `failureMessage`
 * when it sent none, which is what `ConfirmActionModal` shows. So does a
 * verification refusal that arrives once that account or server is no longer
 * current: it belongs to the old one, and no dialog opens for it.
 */
export async function sendFirst(
  request: FrozenRequest,
  failureMessage: string,
  { gated = true, dispatch = plainDispatch, unsentMessage = failureMessage }: SendFirstOptions = {}
): Promise<FirstSendResult> {
  const context = captureApiRequestContext();
  const res = await dispatch(request.path, initFor(request), context);
  if (res === null) throw new Error(unsentMessage);
  if (res.ok) return { kind: 'ok', response: res };
  const body = await readBody(res);
  const refusal = verificationRefusal(res.status, body, gated);
  if (refusal === null) throw new Error(serverErrorText(body) ?? failureMessage);
  if (!apiRequestContextIsCurrent(context)) throw new Error(FIRST_SEND_SESSION_CHANGED);
  return { kind: 'stepUp', refusal, context };
}

/**
 * What a re-send came to. Assignable to the dialog's `DangerousActionSendResult`,
 * with the response kept on success for a host that reads it.
 */
export type ResendResult =
  | { kind: 'ok'; response: Response }
  | { kind: 'refused'; status: number; body: unknown }
  | { kind: 'aborted' };

/**
 * Re-sends the frozen request with the proven code, admitted against `context`.
 * A send `dispatch` refused comes back as a refusal carrying `unsentMessage`
 * with no HTTP status, which the dialog shows as its banner.
 */
export async function resendWithCode(
  request: FrozenRequest,
  mfaCode: string | undefined,
  context: ApiRequestContext,
  dispatch: Dispatch = plainDispatch,
  unsentMessage = ''
): Promise<ResendResult> {
  const res = await dispatch(request.path, initFor(request, mfaCode), context);
  if (res === null) return { kind: 'refused', status: 0, body: { error: unsentMessage } };
  if (res.ok) return { kind: 'ok', response: res };
  return { kind: 'refused', status: res.status, body: await readBody(res) };
}

/**
 * The dialog's `send` for `request`. Before a request is frozen the dialog is
 * closed and never calls it, so the null case sends nothing.
 */
export function resender(
  request: FrozenRequest | null
): (mfaCode: string | undefined, context: ApiRequestContext) => Promise<ResendResult> {
  return async (mfaCode, context) =>
    request === null ? { kind: 'aborted' } : resendWithCode(request, mfaCode, context);
}

/** The dialog's sentence for a refusal the dangerous-action adapter does not own. */
export function describeFailureWith(fallback: string): (status: number, body: unknown) => string {
  return (_status, body) => serverErrorText(body) ?? fallback;
}
