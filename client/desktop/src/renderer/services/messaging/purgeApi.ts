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
import { toDeleteRefusalView, type DeleteRefusalView } from './deleteRefusal';
import {
  mintMfaMethods,
  mintPasswordStepUpToken,
  passwordStepUpRefusalMessage,
} from '../system/stepUpToken';
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

/** The two soft-lock views that ask the user for a factor. */
export type SoftLockChallengeView = Extract<DeleteRefusalView, { view: 'confirm' | 'password' }>;

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
  | { kind: 'softLockChallenge'; view: SoftLockChallengeView }
  /** #3455: any other soft-lock 403 (e.g. `mfa_enrollment_required`). Nothing was purged. */
  | { kind: 'softLockFailed'; message?: string; retryAfterSeconds?: number }
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

/** Everything the result stage can render. */
export type TerminalPurgeResult = Exclude<PurgeResult, StepUpPurgeResult | SoftLockChallengeResult>;

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
    if (view.view === 'confirm' || view.view === 'password') {
      return { kind: 'softLockChallenge', view };
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
  const retryAfterSeconds = Number.isFinite(seconds) ? seconds : undefined;
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

/** The self-purge routes' purposes at the mint endpoint, one per route. */
function selfPurgePurpose(context: 'channel' | 'server') {
  return context === 'channel' ? 'messages.channel_purge' : 'messages.server_purge';
}

/**
 * The step-up fields of a purge body, or the result that stopped it first.
 * A DM/group purge keeps its hard step-up: password and code together, in a
 * single shot (spec R-7). A channel/server self-purge is an own-rule route, so
 * its password goes only to the mint endpoint and the route gets the token; a
 * refused exchange lands on the password field of the soft-lock stage.
 */
async function purgeStepUpFields(
  args: PurgeArgs
): Promise<
  { fields: Record<string, string>; context?: ApiRequestContext } | { refused: PurgeResult }
> {
  const fields: Record<string, string> = {};
  if (args.mfaCode) fields.mfa_code = args.mfaCode;
  if (!args.currentPassword) return { fields };
  if (args.context === 'dm' || args.context === 'group') {
    fields.current_password = args.currentPassword;
    return { fields };
  }
  // The exchange and the purge are one operation: the purge is admitted
  // against this capture, so it refuses to dispatch if another account or
  // server took over after the exchange (#3509 review).
  const context = captureApiRequestContext();
  const minted = await mintPasswordStepUpToken(
    args.currentPassword,
    selfPurgePurpose(args.context),
    context
  );
  if (minted.kind === 'refused') {
    // An account that enrolled MFA after the prompt opened moves to the code
    // prompt with the methods the mint named.
    const methods = mintMfaMethods(minted);
    return {
      refused: {
        kind: 'softLockChallenge',
        view: methods
          ? { view: 'confirm', methods }
          : { view: 'password', error: passwordStepUpRefusalMessage(minted) },
      },
    };
  }
  fields.step_up_token = minted.token;
  return { fields, context };
}

export async function purgeMessages(args: PurgeArgs): Promise<PurgeResult> {
  // Single-shot: send whichever factors the actor has, together. Probing for
  // requirements costs a request against the same purge budget (spec R-7).
  // A passwordless SSO account with MFA sends the code alone — the server
  // accepts MFA as the whole step-up when there is no password hash.
  const stepUp = await purgeStepUpFields(args);
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
