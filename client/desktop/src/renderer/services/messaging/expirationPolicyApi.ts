import { z } from 'zod';
import { createJSONStorage, type PersistStorage } from 'zustand/middleware';
import { apiFetch } from '../system/apiClient';

const expirationWindowSchema = z.union([
  z.literal(3600),
  z.literal(86400),
  z.literal(604800),
  z.literal(2592000),
]);

export type ExpirationWindowSeconds = z.infer<typeof expirationWindowSchema>;

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
  | { kind: 'ambiguous' };

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

export function parseSeenExpirationRevisions(
  value: unknown
): Record<string, Record<string, number>> {
  if (!isRecord(value)) return {};
  const accounts: Record<string, Record<string, number>> = {};
  for (const [accountId, revisions] of Object.entries(value)) {
    if (!accountId || !isRecord(revisions)) continue;
    const validRevisions: Record<string, number> = {};
    for (const [targetId, revision] of Object.entries(revisions)) {
      if (
        targetId &&
        typeof revision === 'number' &&
        Number.isSafeInteger(revision) &&
        revision >= 0
      ) {
        validRevisions[targetId] = revision;
      }
    }
    if (Object.keys(validRevisions).length > 0) accounts[accountId] = validRevisions;
  }
  return accounts;
}

/** Keeps a definitive in-memory policy result truthful when browser storage is unavailable. */
export function createExpirationPolicyStorage<T>(): PersistStorage<T> | undefined {
  return createJSONStorage<T>(() => {
    const storage = localStorage;
    return {
      getItem: storage.getItem.bind(storage),
      setItem: (name, value) => {
        try {
          storage.setItem(name, value);
        } catch {
          console.warn('Unable to persist expiration policy acknowledgement.');
        }
      },
      removeItem: storage.removeItem.bind(storage),
    };
  });
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

async function responsePolicy(response: Response): Promise<ExpirationPolicy | undefined> {
  return parseExpirationPolicyResponse(await response.json().catch(() => undefined));
}

async function policyMutationResult(
  response: Response,
  status: 200 | 409 | 503
): Promise<ExpirationMutationResult> {
  const policy = await responsePolicy(response);
  if (status === 200) return policy ? { kind: 'ok', policy } : { kind: 'ambiguous' };
  if (status === 409) return policy ? { kind: 'conflict', policy } : { kind: 'conflict' };
  return policy ? { kind: 'partial', candidate: policy } : { kind: 'partial' };
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

export async function updateExpirationPolicy(
  scope: ExpirationScope,
  request: ExpirationRequest
): Promise<ExpirationMutationResult> {
  if (!expirationRequestSchema.safeParse(request).success) {
    return { kind: 'rejected', reason: 'invalidRequest' };
  }

  let response: Response;
  try {
    response = await apiFetch(expirationPath(scope), {
      method: 'PATCH',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(request),
    });
  } catch {
    return { kind: 'ambiguous' };
  }

  if (response.status === 200 || response.status === 409 || response.status === 503) {
    return policyMutationResult(response, response.status);
  }
  if (response.status === 429) return rateLimitedMutationResult(response);
  const rejected = rejectedMutationResult(response.status);
  if (rejected) return rejected;
  return { kind: 'ambiguous' };
}
