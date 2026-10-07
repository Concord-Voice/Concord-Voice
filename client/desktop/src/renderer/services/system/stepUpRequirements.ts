/**
 * What a step-up will ask for, learned before the first submit.
 *
 * `GET /api/v1/mfa/step-up` answers, for the signed-in account only, the
 * inline factors the step-up seam will accept (policy P1), which of them the
 * account used most recently, and whether an unused backup code exists. The
 * picker reads it when a step-up surface opens, so an account is never shown a
 * code box it cannot fill (design 2026-09-26-mfa-factor-picker §1.3, §2).
 *
 * The read is ADVISORY. The server enforces the step-up on the action itself,
 * and an `mfa_required` refusal's `methods` replace whatever was read here. So
 * every failure of the read fails toward asking for more (the blocked state or
 * the password leg alone), never toward `ready`.
 *
 * Nothing this module reads is logged: a default derived from recency is
 * account posture.
 */

import type { StepUpRefusal } from './stepUpRefusal';
import { apiFetch } from './apiClient';
import { isAbortError } from './requestContext';

export const STEP_UP_REQUIREMENTS_PATH = '/api/v1/mfa/step-up';

/** The factors a step-up can verify inline, strongest first. */
const INLINE_STEP_UP_METHODS = ['webauthn', 'totp'] as const;

export type InlineStepUpMethod = (typeof INLINE_STEP_UP_METHODS)[number];

/**
 * Why the read was refused. Each is terminal: the action route sits behind the
 * same middleware chain, so it would refuse the same way (design §2).
 *
 * - `session`: a 401 that survived `apiFetch`'s refresh.
 * - `account`: 403 `error_code: "account_disabled"`.
 * - `emailUnverified`: 403 `code: "EMAIL_NOT_VERIFIED"`.
 * - `client`: 400, an attestation or client-version 403, or any other 4xx
 *   except 404 and 429.
 */
export type StepUpReadRefusalReason = 'session' | 'account' | 'emailUnverified' | 'client';

export type StepUpRequirementsResult =
  | {
      kind: 'ready';
      /** P1, intersected with `{webauthn, totp}`, strongest first. May be empty. */
      methods: readonly InlineStepUpMethod[];
      /** The server's choice when it is a member of `methods`, else null. */
      defaultMethod: InlineStepUpMethod | null;
      backupCodeAvailable: boolean;
    }
  /** 404: the server predates the route. The handler itself never answers 404. */
  | { kind: 'unsupported' }
  /** 429, any 5xx, a transport failure, or a 200 whose body is malformed. */
  | { kind: 'unavailable' }
  | { kind: 'refused'; reason: StepUpReadRefusalReason }
  /** The signal, or `apiFetch`'s pre-dispatch fence, cancelled it. Discard; never render. */
  | { kind: 'aborted' };

/**
 * `value` when it is an inline method and one of the `offered` ones, else null.
 * The answer is always drawn from the inline set, never from `offered`, so
 * nothing else (a backup code) can come back however `offered` was built.
 */
function offeredMethod(
  offered: readonly InlineStepUpMethod[],
  value: unknown
): InlineStepUpMethod | null {
  return (
    INLINE_STEP_UP_METHODS.find((method) => method === value && offered.includes(method)) ?? null
  );
}

/**
 * The inline factors in `methods`, strongest first, each at most once. Email
 * and SMS (and anything else) are dropped: Family A never offers them (G1), so
 * a list naming only those is the no-usable-method state, an empty array.
 */
export function intersectInline(methods: readonly string[]): InlineStepUpMethod[] {
  return INLINE_STEP_UP_METHODS.filter((method) => methods.includes(method));
}

/**
 * The method the picker opens on: `serverDefault` when it is offered, else the
 * strongest offered method, else null. The client computes no recency of its
 * own, and a backup code is never the default because it is never offered as
 * a method.
 */
export function pickDefaultMethod(
  offered: readonly InlineStepUpMethod[],
  serverDefault: string | null | undefined
): InlineStepUpMethod | null {
  // Strength order comes from the inline set, not from `offered`, which a
  // caller may have built in any order.
  return (
    offeredMethod(offered, serverDefault) ??
    INLINE_STEP_UP_METHODS.find((method) => offered.includes(method)) ??
    null
  );
}

/**
 * True when `code`, with spaces and hyphens removed, is exactly six ASCII
 * digits. A TOTP code is six digits and a backup code is eight characters, so
 * the shape alone says which factor a seam success spent (design §4.1, C12).
 * That holds only while the server keeps the two formats disjoint.
 */
export function isTotpShaped(code: string): boolean {
  return /^\d{6}$/.test(code.replace(/[ -]/g, ''));
}

/**
 * True when `code`, trimmed, is eight ASCII letters or digits: the shape of a
 * backup code (`backupLen`/`backupAlphabet`, `mfa/totp.go`). The server
 * upper-cases after `TrimSpace`, so case does not matter here either.
 */
export function isBackupShaped(code: string): boolean {
  return /^[A-Za-z0-9]{8}$/.test(code.trim());
}

/**
 * True for a refusal that proves the submitted code was not spent, so a
 * TOTP-shaped code is not recorded as accepted (C30). Every other answer,
 * including success and a lost response, may have spent it.
 */
export function codeProvenUnspent(refusal: StepUpRefusal): boolean {
  switch (refusal.kind) {
    case 'invalidMfaCode':
    case 'invalidPassword':
    case 'mfaRequired':
    case 'passwordRequired':
    case 'rateLimited':
      return true;
    default:
      return false;
  }
}

const TOTP_PERIOD_MS = 30_000;

/**
 * When the recently-spent TOTP hint (S2a) stops showing: the end of the period
 * after the one `acceptedAt` fell in. `acceptedAt` is the `Date.now()`
 * milliseconds `noteTotpAccepted` stored, so the arithmetic is in milliseconds
 * too (C64).
 */
export function totpHintExpiresAt(acceptedAt: number): number {
  return (Math.floor(acceptedAt / TOTP_PERIOD_MS) + 2) * TOTP_PERIOD_MS;
}

/** True while the S2a hint applies to an acceptance at `acceptedAt`, if any. */
export function isTotpHintActive(acceptedAt: number | undefined, now: number): boolean {
  return acceptedAt !== undefined && now < totpHintExpiresAt(acceptedAt);
}

/** A 200 body, or null when any field is missing or of the wrong type (Q6). */
function parseRequirements(raw: unknown): StepUpRequirementsResult | null {
  if (typeof raw !== 'object' || raw === null || Array.isArray(raw)) return null;
  const body = raw as Record<string, unknown>;
  const methods = body.methods;
  const serverDefault = body.default_method;
  const backupCodeAvailable = body.backup_code_available;
  if (!Array.isArray(methods) || !methods.every((m): m is string => typeof m === 'string')) {
    return null;
  }
  // The key is always present: JSON null when the account has no inline method.
  if (serverDefault !== null && typeof serverDefault !== 'string') return null;
  if (typeof backupCodeAvailable !== 'boolean') return null;
  const offered = intersectInline(methods);
  return {
    kind: 'ready',
    methods: offered,
    defaultMethod: offeredMethod(offered, serverDefault),
    backupCodeAvailable,
  };
}

function forbiddenReason(raw: unknown): StepUpReadRefusalReason {
  const body = typeof raw === 'object' && raw !== null ? (raw as Record<string, unknown>) : {};
  if (body.error_code === 'account_disabled') return 'account';
  if (body.code === 'EMAIL_NOT_VERIFIED') return 'emailUnverified';
  return 'client';
}

async function responseResult(res: Response): Promise<StepUpRequirementsResult> {
  if (res.ok) return parseRequirements(await res.json()) ?? { kind: 'unavailable' };
  if (res.status === 404) return { kind: 'unsupported' };
  if (res.status === 401) return { kind: 'refused', reason: 'session' };
  if (res.status === 403) {
    return { kind: 'refused', reason: forbiddenReason(await res.json().catch(() => null)) };
  }
  if (res.status === 429 || res.status >= 500) return { kind: 'unavailable' };
  if (res.status >= 400) return { kind: 'refused', reason: 'client' };
  // Nothing else is a real answer from this route: fail toward asking for more.
  return { kind: 'unavailable' };
}

/**
 * Reads the signed-in account's step-up requirements. Never throws.
 *
 * `apiFetch` throws a DOMException named AbortError when `signal` aborts, and
 * also before dispatch when the account or server changed under it (D7). Both
 * are `aborted`, which the caller discards. Any other rejection, including an
 * unreadable 200 body, is `unavailable`.
 */
export async function fetchStepUpRequirements(
  signal: AbortSignal
): Promise<StepUpRequirementsResult> {
  try {
    const res = await apiFetch(STEP_UP_REQUIREMENTS_PATH, { method: 'GET', signal });
    const result = await responseResult(res);
    return signal.aborted ? { kind: 'aborted' } : result;
  } catch (err) {
    // `fetch` rejects with the signal's own reason, which need not be an
    // AbortError when the caller aborted with one, so the signal is checked too.
    return signal.aborted || isAbortError(err) ? { kind: 'aborted' } : { kind: 'unavailable' };
  }
}
