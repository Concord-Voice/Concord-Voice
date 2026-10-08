/**
 * Message purge wire mapping.
 *
 * This module is the ONLY place a purge HTTP status is interpreted; components
 * consume the discriminated result union and never see a status code.
 */

import {
  apiFetchInContext,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../system/requestContext';
import { classifyStepUpRefusal, isStepUpFactorRefusal } from '../system/stepUpRefusal';
import {
  mintRefusalView,
  toDeleteRefusalView,
  toDeleteSubmitOutcome,
  type DeleteRefusalView,
} from './deleteRefusal';
import { mintPasswordStepUpToken, type PasswordStepUpMint } from '../system/stepUpToken';
import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import type { PurgeRange } from '../../constants/purgeRanges';

export type PurgeContext = 'channel' | 'server' | 'dm' | 'group';

export interface PurgeArgs {
  context: PurgeContext;
  scopeId: string;
  range: PurgeRange;
  /**
   * The password. Never logged, never stored — component-local state only. On
   * a DM/group purge it is the route's own step-up field. On a channel/server
   * self-purge it never reaches the route: it is exchanged at the mint
   * endpoint for a single-use token, sent as `step_up_token` (#3509).
   */
  currentPassword?: string;
  mfaCode?: string;
  /**
   * Channel/server self-purge soft-lock (#3455): the challenge view already on
   * screen when this is a retry. Read only so an `Invalid MFA code` refusal can
   * keep the confirm view (and its methods) open — see `toDeleteRefusalView`.
   */
  softLockPrior?: DeleteRefusalView;
}

/**
 * The soft-lock views the credential stage hosts: the two that ask for a
 * factor, and enrolment (E8), which asks for nothing because nothing can pass.
 */
export type SoftLockChallengeView = Extract<
  DeleteRefusalView,
  { view: 'confirm' | 'password' | 'enroll' }
>;

export type PurgeResult =
  | { kind: 'success'; deletedCount: number; hiddenCount: number }
  | { kind: 'rateLimited'; retryAfterSeconds?: number }
  /** The shared step-up attempt budget, not the purge limiter (#3455, X17). */
  | { kind: 'verificationLimited'; retryAfterSeconds?: number }
  | { kind: 'unavailable' }
  | { kind: 'notFound' }
  | { kind: 'forbidden' }
  | { kind: 'passwordRequired' }
  | { kind: 'mfaRequired'; methods: string[] }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidMfaCode' }
  | { kind: 'stepUpImpossible' }
  /**
   * #3455: a channel/server SELF-purge tripped the delete-rate soft-lock and
   * needs a factor before it will run. Not a DM step-up kind: the modal routes
   * it to its own stage, with the purge purposes rather than the DM fence's.
   */
  | {
      kind: 'softLockChallenge';
      view: SoftLockChallengeView;
      /**
       * The factor hook's reading of this challenge, classified here from the
       * wire and the mint's own reason, so no consumer re-derives it from the
       * view's copy. `null` when the credential fields cannot answer it: a
       * refused password exchange that is no verdict on the password (a rate
       * limit, a server without the endpoint, a failed lookup, or an MFA
       * requirement naming no method). Typing again answers none of them, so a
       * consumer ends the stage with the view's copy.
       */
      refusal: StepUpFactorRefusal | null;
    }
  /** #3455: any other soft-lock 403. Nothing was purged. */
  | { kind: 'softLockFailed'; message?: string; retryAfterSeconds?: number }
  /**
   * D7: the soft-lock's password exchange never left, because apiFetch refused
   * to dispatch it after an account or server change. Nothing was checked or
   * purged, so nothing is shown and nothing is reported as a network failure.
   */
  | { kind: 'notSent' }
  | { kind: 'sessionExpired' }
  | { kind: 'networkError' }
  | { kind: 'unexpectedError' }
  | { kind: 'partial' };

/**
 * The kinds the DM/group step-up stage owns. They are not terminal: the modal
 * routes them to the credential stage rather than to the result stage.
 */
const STEP_UP_RESULT_KINDS = new Set<PurgeResult['kind']>([
  'passwordRequired',
  'mfaRequired',
  'invalidPassword',
  'invalidMfaCode',
  'stepUpImpossible',
]);

export type StepUpPurgeResult = Extract<
  PurgeResult,
  {
    kind:
      | 'passwordRequired'
      | 'mfaRequired'
      | 'invalidPassword'
      | 'invalidMfaCode'
      | 'stepUpImpossible';
  }
>;

export type SoftLockChallengeResult = Extract<PurgeResult, { kind: 'softLockChallenge' }>;

/** Everything the result stage can render. A purge that was not sent renders nothing. */
export type TerminalPurgeResult = Exclude<
  PurgeResult,
  StepUpPurgeResult | SoftLockChallengeResult | { kind: 'notSent' }
>;

export function isStepUpPurgeResult(result: PurgeResult): result is StepUpPurgeResult {
  return STEP_UP_RESULT_KINDS.has(result.kind);
}

export function isSoftLockChallengeResult(result: PurgeResult): result is SoftLockChallengeResult {
  return result.kind === 'softLockChallenge';
}

function purgePath(context: PurgeContext, scopeId: string): string {
  switch (context) {
    case 'channel':
      return `/api/v1/channels/${scopeId}/messages`;
    case 'server':
      return `/api/v1/servers/${scopeId}/messages`;
    case 'dm':
    case 'group':
      return `/api/v1/dm/conversations/${scopeId}/messages`;
  }
}

/**
 * A route's soft-lock challenge, with the delete soft-lock's own reading of the
 * same refusal (D6) as its `refusal`, so the two soft-locks hand their factor
 * hooks the same kinds. The views that reach here are each answerable.
 */
function routeChallenge(view: SoftLockChallengeView, payload: unknown): SoftLockChallengeResult {
  const outcome = toDeleteSubmitOutcome(403, payload);
  const refusal = outcome.kind === 'refusal' ? outcome.refusal : null;
  return { kind: 'softLockChallenge', view, refusal };
}

/**
 * A 403 from the purge route is either a step-up refusal — read by the shared
 * classifier (`services/system/stepUpRefusal.ts`), which owns the flag-first,
 * exact-string-second contract and its documented brittleness — or a plain
 * authorization refusal.
 *
 * The degradation stays safe rather than wrong: a reworded seam string lands on
 * `forbidden` ("this purge couldn't be completed") instead of a per-field
 * error, so nothing is misreported and nothing is purged. The durable fix is a
 * `code` field on those 403s, raised on PR #2743 for a decision.
 */
function mapForbidden(payload: unknown, args: PurgeArgs, retryAfter: string | null): PurgeResult {
  // #3455: on the channel and server routes a `delete_rate_limited` 403 is the
  // self-purge soft-lock. The DM/group routes never carry it (their own-rule
  // fence is the older step-up below), so the flag is only read here.
  const isSelfPurgeRoute = args.context === 'channel' || args.context === 'server';
  const flagged =
    typeof payload === 'object' &&
    payload !== null &&
    (payload as { delete_rate_limited?: unknown }).delete_rate_limited === true;
  if (isSelfPurgeRoute && flagged) {
    const view = toDeleteRefusalView(403, payload, retryAfter, args.softLockPrior);
    if (view.view === 'confirm' || view.view === 'password' || view.view === 'enroll') {
      return routeChallenge(view, payload);
    }
    if (view.view === 'failed') {
      return {
        kind: 'softLockFailed',
        message: view.message,
        retryAfterSeconds: view.retryAfterSeconds,
      };
    }
  }
  const refusal = classifyStepUpRefusal(403, payload);
  return isStepUpFactorRefusal(refusal) ? refusal : { kind: 'forbidden' };
}

/** A 429 from the purge route: the purge budget, or the shared step-up budget. */
async function mapTooManyRequests(res: Response): Promise<PurgeResult> {
  // The budget itself is operator-tunable (PURGE_RATE_LIMIT), so the countdown
  // is derived from the header and never from a hardcoded allowance.
  const header = res.headers.get('Retry-After');
  const seconds = header ? Number.parseInt(header, 10) : Number.NaN;
  const retryAfterSeconds = Number.isFinite(seconds) && seconds >= 0 ? seconds : undefined;
  // A self-purge past the delete-rate soft-lock charges the shared step-up
  // attempt budget, which also answers 429. Only its flag tells the two apart
  // (#3455, X17), and "purge limit reached" would misstate what ran out.
  const payload = (await res.json().catch(() => ({}))) as { step_up_budget_exhausted?: unknown };
  if (payload.step_up_budget_exhausted === true) {
    return { kind: 'verificationLimited', retryAfterSeconds };
  }
  return { kind: 'rateLimited', retryAfterSeconds };
}

/**
 * True for a refusal flagged `step_up_unavailable`: the account has neither a
 * password nor inline MFA, so no confirmation can pass.
 */
function isStepUpUnavailable(payload: unknown): boolean {
  return (
    typeof payload === 'object' &&
    payload !== null &&
    (payload as { step_up_unavailable?: unknown }).step_up_unavailable === true
  );
}

/** The server's own `error` text, when the body carries one. */
function errorText(payload: unknown): string | undefined {
  const error =
    typeof payload === 'object' && payload !== null
      ? (payload as { error?: unknown }).error
      : undefined;
  return typeof error === 'string' ? error : undefined;
}

/**
 * The challenge a refused password exchange leaves on screen, read from the
 * mint's reason and never from its copy. `mintRefusalView` moves a mint that
 * names its methods to the code prompt, and only that prompt or a wrong
 * password is something the credential fields can answer: the same reading
 * the delete soft-lock gives its exchange (`useChatController`).
 */
function mintChallenge(
  minted: Extract<PasswordStepUpMint, { kind: 'refused' }>
): SoftLockChallengeResult {
  const view = mintRefusalView(minted);
  let refusal: StepUpFactorRefusal | null = null;
  if (view.view === 'confirm') refusal = { kind: 'mfaRequired', methods: view.methods };
  else if (minted.reason === 'invalidPassword') refusal = { kind: 'invalidPassword' };
  return { kind: 'softLockChallenge', view, refusal };
}

/** The self-purge routes' purposes at the mint endpoint, one per route. */
function selfPurgePurpose(context: 'channel' | 'server') {
  return context === 'channel' ? 'messages.channel_purge' : 'messages.server_purge';
}

/**
 * The step-up fields of a purge body, or the result that stopped it first.
 * A DM/group purge keeps its hard step-up: password and code together, in a
 * single shot (spec R-7). A channel/server self-purge is an own-rule route, so
 * its password goes only to the mint endpoint and the route gets the token; a
 * refused exchange lands on the soft-lock stage, and one that never left is
 * `notSent`.
 *
 * `context` is the caller's capture, when it has one; the purge, and any
 * exchange before it, are admitted against it.
 */
async function purgeStepUpFields(
  args: PurgeArgs,
  context: ApiRequestContext | undefined
): Promise<
  { fields: Record<string, string>; context?: ApiRequestContext } | { refused: PurgeResult }
> {
  const fields: Record<string, string> = {};
  if (args.mfaCode) fields.mfa_code = args.mfaCode;
  if (!args.currentPassword) return { fields, context };
  if (args.context === 'dm' || args.context === 'group') {
    fields.current_password = args.currentPassword;
    return { fields, context };
  }
  // The exchange and the purge are one operation: the purge is admitted
  // against this capture, so it refuses to dispatch if another account or
  // server took over after the exchange (#3509 review).
  const operation = context ?? captureApiRequestContext();
  const minted = await mintPasswordStepUpToken(
    args.currentPassword,
    selfPurgePurpose(args.context),
    operation
  );
  if (minted.kind === 'refused') {
    // D7: the exchange never left, so there is nothing to say about the password.
    if (minted.unsent) return { refused: { kind: 'notSent' } };
    return { refused: mintChallenge(minted) };
  }
  fields.step_up_token = minted.token;
  return { fields, context: operation };
}

/**
 * `context` is the operation the purge belongs to (`captureApiRequestContext`).
 * The DM/group step-up passes the capture its factor was proven under, so a
 * security-key token or a password is never sent as another account or to
 * another server. Without one, the purge is its own operation, as before.
 */
export async function purgeMessages(
  args: PurgeArgs,
  context?: ApiRequestContext
): Promise<PurgeResult> {
  // Single-shot: send whichever factors the actor has, together. Probing for
  // requirements costs a request against the same purge budget (spec R-7).
  // A passwordless SSO account with MFA sends the code alone — the server
  // accepts MFA as the whole step-up when there is no password hash.
  const stepUp = await purgeStepUpFields(args, context);
  if ('refused' in stepUp) return stepUp.refused;
  const body: Record<string, unknown> = { range: args.range, ...stepUp.fields };

  const res = await apiFetchInContext(
    purgePath(args.context, args.scopeId),
    {
      method: 'DELETE',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    },
    stepUp.context
  );

  if (res.ok) {
    // The server always emits a body, but a non-JSON 200 injected by an intermediary would
    // reject out of a function whose callers have no catch, and a body missing
    // the counts would render "Purged undefined messages."
    const data = (await res.json().catch(() => ({}))) as Partial<{
      deleted_count: number;
      hidden_count: number;
    }>;
    // deleted_count 0 is a legal success — an authorized purge of an empty scope.
    return {
      kind: 'success',
      deletedCount: Number(data.deleted_count) || 0,
      hiddenCount: Number(data.hidden_count) || 0,
    };
  }

  if (res.status === 429) return mapTooManyRequests(res);

  // 503 is the fail-closed rate-limit backend, NOT a quota outcome
  // (middleware/ratelimit.go:247). Distinct copy; no countdown exists.
  if (res.status === 503) return { kind: 'unavailable' };

  if (res.status === 404) return { kind: 'notFound' };

  // An expired session (or a refresh that failed) is refused before the purge
  // handler runs, so nothing was deleted. `partial` would claim otherwise.
  if (res.status === 401) return { kind: 'sessionExpired' };

  const payload: unknown = await res.json().catch(() => ({}));

  if (res.status === 403) return mapForbidden(payload, args, res.headers.get('Retry-After'));

  // 400 on a DM purge means the account has neither a password nor MFA, so no
  // credential can satisfy the step-up. The only way forward is the setting.
  if (res.status === 400 && (args.context === 'dm' || args.context === 'group')) {
    return { kind: 'stepUpImpossible' };
  }

  // The same on a channel/server self-purge past the soft-lock, told apart
  // from a malformed-body 400 by its flag (#3509 review): the server's own copy
  // says what to set up, so it is the message.
  if (res.status === 400 && isStepUpUnavailable(payload)) {
    return { kind: 'softLockFailed', message: errorText(payload) };
  }

  // 5xx only: the purge may have partially committed before failing, so this
  // one must never render as "nothing was deleted".
  if (res.status >= 500) return { kind: 'partial' };

  // Any other 4xx is a refusal made before the handler could delete anything —
  // an invalid range, an unroutable method. Generic, but truthful about scope.
  return { kind: 'unexpectedError' };
}
