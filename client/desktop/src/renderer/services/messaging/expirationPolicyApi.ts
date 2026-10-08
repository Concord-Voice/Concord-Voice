import { z } from 'zod';
import { apiFetchInContext, isAbortError, type ApiRequestContext } from '../system/requestContext';
import { adaptDangerousActionRefusal } from '../system/stepUpRouteAdapters';

const expirationWindowSchema = z.union([
  z.literal(3600),
  z.literal(86400),
  z.literal(604800),
  z.literal(2592000),
]);

export type ExpirationWindowSeconds = z.infer<typeof expirationWindowSchema>;

/** The one label per window. Previously duplicated three ways — the zod literals here,
 *  a `labels` record in the (now-deleted) policy summary, and an options array in the
 *  editor — which meant the system-message row would have been a fourth copy. Keyed by
 *  the same literals the schema validates, so a new window cannot be added to one and
 *  forgotten in the other: `Record<ExpirationWindowSeconds, string>` fails to compile
 *  until this map covers it. */
export const EXPIRATION_WINDOW_LABELS: Record<ExpirationWindowSeconds, string> = {
  3600: '1 hour',
  86400: '24 hours',
  604800: '7 days',
  2592000: '30 days',
};

/** Ordered for pickers. Derived from the label map rather than restated, so order is the
 *  only thing this adds. */
export const EXPIRATION_WINDOW_OPTIONS: ReadonlyArray<{
  label: string;
  value: ExpirationWindowSeconds;
}> = [3600, 86400, 604800, 2592000].map((value) => ({
  label: EXPIRATION_WINDOW_LABELS[value as ExpirationWindowSeconds],
  value: value as ExpirationWindowSeconds,
}));

export interface ExpirationPolicy {
  windowSeconds: ExpirationWindowSeconds | null;
  updatedAt: string | null;
  revision: number;
  backfillPending: boolean;
}

export type ExpirationRequest =
  | {
      mode: 'set';
      window_seconds: ExpirationWindowSeconds;
      retroactive: 'apply' | 'new_only';
      /**
       * Confirms a shortening on a server that enforces MFA on dangerous actions (#3456). Sent
       * only on the re-send after a channel's `gated` answer, never on the first request.
       */
      mfa_code?: string;
    }
  | { mode: 'clear'; retroactive: 'clear_pending' | 'leave_pending' }
  | { mode: 'resume'; revision: number };

export type ExpirationScope = { kind: 'channel' | 'dm'; id: string };

export type ExpirationMutationResult =
  | { kind: 'ok'; policy: ExpirationPolicy }
  | { kind: 'conflict'; policy?: ExpirationPolicy }
  | { kind: 'partial'; candidate?: ExpirationPolicy }
  | {
      kind: 'rejected';
      reason:
        | 'invalidRequest'
        | 'sessionExpired'
        | 'forbidden'
        | 'notFound'
        | 'rateLimited'
        | 'unavailable';
      retryAfterSeconds?: number;
    }
  | { kind: 'ambiguous' }
  /**
   * The request never left: `apiFetch`'s pre-dispatch fence refused it because the account or
   * server moved since the `context` it was admitted against. Unlike `ambiguous`, nothing may
   * have been applied, so there is nothing to refresh.
   */
  | { kind: 'aborted' }
  /**
   * A channel's dangerous-action gate answered instead of the policy route (#3456): it asks for
   * a verified code, refuses the one sent, or is busy. `status` and `body` are the response as
   * received, for `adaptDangerousActionRefusal`. Channel scope only: the DM route has no gate.
   */
  | { kind: 'gated'; status: number; body: unknown };

export interface ExpirationPolicyReadRequest {
  targetId: string;
  lifecycle: import('../system/postLoginHydrationLifecycle').AuthLifecycleSnapshot;
}

export type ExpirationPolicyReadResult =
  | { kind: 'fresh'; policy: ExpirationPolicy }
  | { kind: 'missing' }
  | { kind: 'unavailable' }
  | { kind: 'superseded' };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

const policyResponseSchema = z.object({
  window_seconds: expirationWindowSchema.nullable(),
  updated_at: z.string().datetime({ offset: true }).nullable(),
  revision: z.number().int().nonnegative().refine(Number.isSafeInteger),
  backfill_pending: z.boolean(),
});

const expirationRequestSchema = z.discriminatedUnion('mode', [
  z.strictObject({
    mode: z.literal('set'),
    window_seconds: expirationWindowSchema,
    retroactive: z.enum(['apply', 'new_only']),
    mfa_code: z.string().min(1).max(256).optional(),
  }),
  z.strictObject({
    mode: z.literal('clear'),
    retroactive: z.enum(['clear_pending', 'leave_pending']),
  }),
  z.strictObject({
    mode: z.literal('resume'),
    revision: z.number().int().nonnegative().refine(Number.isSafeInteger),
  }),
]);

export function parseExpirationPolicyResponse(raw: unknown): ExpirationPolicy | undefined {
  const parsed = policyResponseSchema.safeParse(raw);
  if (!parsed.success) return undefined;
  return {
    windowSeconds: parsed.data.window_seconds,
    updatedAt: parsed.data.updated_at,
    revision: parsed.data.revision,
    backfillPending: parsed.data.backfill_pending,
  };
}

export function parseExpirationPolicyFromListRow(raw: unknown): ExpirationPolicy | undefined {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return undefined;
  const row = raw as Record<string, unknown>;
  return parseExpirationPolicyResponse({
    window_seconds: row.expiration_window_seconds,
    updated_at: row.expiration_updated_at,
    revision: row.expiration_revision,
    backfill_pending: row.expiration_backfill_pending,
  });
}

export function hasMalformedExpirationPolicyListRow(raw: unknown): boolean {
  if (!isRecord(raw)) return false;
  const hasPolicyField =
    'expiration_window_seconds' in raw ||
    'expiration_updated_at' in raw ||
    'expiration_revision' in raw ||
    'expiration_backfill_pending' in raw;
  return hasPolicyField && !parseExpirationPolicyFromListRow(raw);
}

export function mergeExpirationPolicy(
  current: ExpirationPolicy | undefined,
  incoming: ExpirationPolicy | undefined
): ExpirationPolicy | undefined {
  if (!incoming) return current;
  if (!current || incoming.revision > current.revision) return incoming;
  if (incoming.revision < current.revision) return current;
  return current.backfillPending ? incoming : current;
}

function expirationPath(scope: ExpirationScope): string {
  return scope.kind === 'channel'
    ? `/api/v1/channels/${scope.id}/expiration`
    : `/api/v1/dm/conversations/${scope.id}/expiration`;
}

function retryAfterSeconds(response: Response): number | undefined {
  const value = response.headers.get('Retry-After');
  if (!value || !/^\d+$/.test(value)) return undefined;
  const seconds = Number(value);
  return Number.isSafeInteger(seconds) ? seconds : undefined;
}

function rateLimitedMutationResult(response: Response): ExpirationMutationResult {
  const seconds = retryAfterSeconds(response);
  return seconds === undefined
    ? { kind: 'rejected', reason: 'rateLimited' }
    : { kind: 'rejected', reason: 'rateLimited', retryAfterSeconds: seconds };
}

function rejectedMutationResult(status: number): ExpirationMutationResult | undefined {
  switch (status) {
    case 400:
    case 413:
      return { kind: 'rejected', reason: 'invalidRequest' };
    case 401:
      return { kind: 'rejected', reason: 'sessionExpired' };
    case 403:
      return { kind: 'rejected', reason: 'forbidden' };
    case 404:
      return { kind: 'rejected', reason: 'notFound' };
    case 500:
      return { kind: 'rejected', reason: 'unavailable' };
    default:
      return undefined;
  }
}

/**
 * What a response to the PATCH came to. The gate's answer is read before the status decides
 * what the body is (F11): a 503 whose body is a policy is the ambiguous commit, `partial`; the
 * gate's own 503 carries a flag and is `gated`; any other 503 is a retryable failure. A 401 is
 * the session, whatever the body says, and keeps its own result.
 */
function mutationResult(
  scope: ExpirationScope,
  response: Response,
  body: unknown
): ExpirationMutationResult {
  const { status } = response;
  if (
    scope.kind === 'channel' &&
    status !== 401 &&
    adaptDangerousActionRefusal(status, body) !== null
  ) {
    return { kind: 'gated', status, body };
  }
  const policy = parseExpirationPolicyResponse(body);
  switch (status) {
    case 200:
      return policy ? { kind: 'ok', policy } : { kind: 'ambiguous' };
    case 409:
      return policy ? { kind: 'conflict', policy } : { kind: 'conflict' };
    case 503:
      return policy
        ? { kind: 'partial', candidate: policy }
        : { kind: 'rejected', reason: 'unavailable' };
    case 429:
      return rateLimitedMutationResult(response);
    default:
      return rejectedMutationResult(status) ?? { kind: 'ambiguous' };
  }
}

/**
 * `context` is the capture the PATCH belongs to. The step-up re-send passes the one its factor was
 * proven under, so a code is never sent as another account or to another server; without one the
 * PATCH is its own operation.
 */
export async function updateExpirationPolicy(
  scope: ExpirationScope,
  request: ExpirationRequest,
  context?: ApiRequestContext
): Promise<ExpirationMutationResult> {
  if (!expirationRequestSchema.safeParse(request).success) {
    return { kind: 'rejected', reason: 'invalidRequest' };
  }

  let response: Response;
  try {
    response = await apiFetchInContext(
      expirationPath(scope),
      {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(request),
      },
      context
    );
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'ambiguous' };
  }

  return mutationResult(scope, response, await response.json().catch(() => undefined));
}
