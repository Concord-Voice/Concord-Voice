/**
 * Message purge wire mapping.
 *
 * This module is the ONLY place a purge HTTP status is interpreted; components
 * consume the discriminated result union and never see a status code.
 */

import { apiFetch } from './apiClient';
import type { PurgeRange } from '../constants/purgeRanges';

export type PurgeContext = 'channel' | 'server' | 'dm' | 'group';

export interface PurgeArgs {
  context: PurgeContext;
  scopeId: string;
  range: PurgeRange;
  /** DM/group step-up. Never logged, never stored — component-local state only. */
  currentPassword?: string;
  mfaCode?: string;
}

export type PurgeResult =
  | { kind: 'success'; deletedCount: number; hiddenCount: number }
  | { kind: 'rateLimited'; retryAfterSeconds?: number }
  | { kind: 'unavailable' }
  | { kind: 'notFound' }
  | { kind: 'forbidden' }
  | { kind: 'passwordRequired' }
  | { kind: 'mfaRequired'; methods: string[] }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidMfaCode' }
  | { kind: 'stepUpImpossible' }
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

/** Everything the result stage can render. */
export type TerminalPurgeResult = Exclude<PurgeResult, StepUpPurgeResult>;

export function isStepUpPurgeResult(result: PurgeResult): result is StepUpPurgeResult {
  return STEP_UP_RESULT_KINDS.has(result.kind);
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

interface PurgeErrorBody {
  error?: string;
  password_required?: boolean;
  mfa_required?: boolean;
  methods?: string[];
}

/**
 * The two boolean flags are the intended contract and are matched first. The two
 * string comparisons below are NOT: they match human-readable prose emitted by
 * `verifyPurgePasswordFactor` / `verifyPurgeMFAFactor`
 * (`services/control-plane/internal/dm/purge.go`), because those two refusals
 * carry no machine-readable discriminator.
 *
 * Known brittleness, deliberately left in place: rewording either server string
 * silently degrades a per-field error into the generic `forbidden` copy — no
 * test on either side fails, because each asserts its own fixture. The durable
 * fix is a `code` field on those 403s, which changes the response contract and
 * is out of this issue's scope; it is raised on PR #2743 for a decision.
 *
 * The degradation is at least safe rather than wrong — the user sees "this purge
 * couldn't be completed" instead of "that password is not correct", so nothing
 * is misreported and nothing is purged. Match on the flags whenever a future
 * server change makes that possible, and delete the string arms.
 */
function mapForbidden(payload: PurgeErrorBody): PurgeResult {
  if (payload.password_required) return { kind: 'passwordRequired' };
  if (payload.mfa_required) return { kind: 'mfaRequired', methods: payload.methods ?? [] };
  if (payload.error === 'Invalid password') return { kind: 'invalidPassword' };
  if (payload.error === 'Invalid MFA code') return { kind: 'invalidMfaCode' };
  return { kind: 'forbidden' };
}

export async function purgeMessages(args: PurgeArgs): Promise<PurgeResult> {
  const body: Record<string, unknown> = { range: args.range };
  // Single-shot: send whichever factors the actor has, together. Probing for
  // requirements costs a request against the same purge budget (spec R-7).
  // A passwordless SSO account with MFA sends the code alone — the server
  // accepts MFA as the whole step-up when there is no password hash.
  if (args.currentPassword) body.current_password = args.currentPassword;
  if (args.mfaCode) body.mfa_code = args.mfaCode;

  const res = await apiFetch(purgePath(args.context, args.scopeId), {
    method: 'DELETE',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(body),
  });

  if (res.ok) {
    // The server always emits a body, but a proxy-injected non-JSON 200 would
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

  if (res.status === 429) {
    // The budget itself is operator-tunable (PURGE_RATE_LIMIT), so the countdown
    // is derived from the header and never from a hardcoded allowance.
    const header = res.headers.get('Retry-After');
    const seconds = header ? Number.parseInt(header, 10) : Number.NaN;
    return {
      kind: 'rateLimited',
      retryAfterSeconds: Number.isFinite(seconds) ? seconds : undefined,
    };
  }

  // 503 is the fail-closed rate-limit backend, NOT a quota outcome
  // (middleware/ratelimit.go:247). Distinct copy; no countdown exists.
  if (res.status === 503) return { kind: 'unavailable' };

  if (res.status === 404) return { kind: 'notFound' };

  // An expired session (or a refresh that failed) is refused before the purge
  // handler runs, so nothing was deleted. `partial` would claim otherwise.
  if (res.status === 401) return { kind: 'sessionExpired' };

  const payload = (await res.json().catch(() => ({}))) as PurgeErrorBody;

  if (res.status === 403) return mapForbidden(payload);

  // 400 on a DM purge means the account has neither a password nor MFA, so no
  // credential can satisfy the step-up. The only way forward is the setting.
  if (res.status === 400 && (args.context === 'dm' || args.context === 'group')) {
    return { kind: 'stepUpImpossible' };
  }

  // 5xx only: the purge may have partially committed before failing, so this
  // one must never render as "nothing was deleted".
  if (res.status >= 500) return { kind: 'partial' };

  // Any other 4xx is a refusal made before the handler could delete anything —
  // an invalid range, an unroutable method. Generic, but truthful about scope.
  return { kind: 'unexpectedError' };
}
