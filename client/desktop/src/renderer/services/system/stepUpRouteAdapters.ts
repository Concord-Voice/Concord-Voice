/**
 * Route adapters for the step-up routes that answer outside the seam's refusal
 * shape (design 2026-09-26-mfa-factor-picker §4.1, "Route adapters").
 *
 * `classifyStepUpRefusal` reads the `internal/stepup` bodies. Two routes the
 * factor picker serves predate that seam and write their own:
 *
 * - #7, the session revocations (`authenticateForRevoke`,
 *   `internal/sessions/handlers.go`);
 * - #8, backup-code regeneration (`RegenerateBackupCodes`,
 *   `internal/mfa/handlers.go`).
 *
 * A third family, the dangerous-action gates (#3456 §3.2), does write the seam
 * bodies, but shares its 429 and 503 statuses with the gated routes' own
 * limiters and failures, which the classifier maps by status alone.
 *
 * Each adapter matches its route's strings or flags EXACTLY and returns `null`
 * for every response it does not own (C5), so the surface's own handling runs
 * instead of a guess. Rewording a server string therefore degrades it to
 * `null`, never to a factor error on the wrong field. Never widen these to
 * substring or case-folded matches.
 */

import type { StepUpFactorRefusal } from '../../hooks/auth/useStepUpFactor';
import { classifyStepUpRefusal, type StepUpRefusal } from './stepUpRefusal';

/** The `error` member of a parsed body, without trusting the body's shape. */
function errorOf(body: unknown): unknown {
  return typeof body === 'object' && body !== null && 'error' in body ? body.error : undefined;
}

/** The route's own `error` sentence, when the body has a non-empty one. */
export function serverErrorText(body: unknown): string | null {
  const error = errorOf(body);
  return typeof error === 'string' && error !== '' ? error : null;
}

/**
 * #7: a refusal from `DELETE /sessions/:id`, `POST /sessions/revoke-all` or
 * the revocation-mode change.
 *
 * `auth_required` lists `users.mfa_methods`, not the inline factors the route
 * accepts, so its `methods` are never read: it becomes `mfaRequired` with
 * `methods: null`, and the requirements read's set stands (C2).
 *
 * A 429 is the routes' `RateLimitByUser` limiter, which answers before the
 * handler reads anything, so it is `rateLimited`: the code it carried was not
 * spent (C30). As an unowned `null` it would read as an answer that may have
 * spent it.
 */
export function adaptSessionsRefusal(status: number, body: unknown): StepUpFactorRefusal | null {
  if (status === 401) return { kind: 'sessionExpired' };
  if (status === 429) return { kind: 'rateLimited' };
  if (status !== 403) return null;
  switch (errorOf(body)) {
    case 'auth_required':
      return { kind: 'mfaRequired', methods: null };
    case 'password_required':
      return { kind: 'passwordRequired' };
    case 'Incorrect password':
      return { kind: 'invalidPassword' };
    case 'Invalid MFA code':
      return { kind: 'invalidMfaCode' };
    default:
      return null;
  }
}

/**
 * #8: a refusal from `POST /mfa/backup-codes/regenerate`, which verifies the
 * password and then a TOTP code bound as `code`.
 *
 * The missing-field 400 is `failed`, never a factor error; it is unreachable
 * while TOTP is the route's floor and the activation guard requires both
 * fields. `400 TOTP is not enabled` is not a refusal of what was entered: it
 * is `null`, and the stage ends on it itself (Q7). A 429 is the route's
 * limiter, and a 401 the dead session, as on #7.
 */
export function adaptBackupCodeRegenerateRefusal(
  status: number,
  body: unknown
): StepUpFactorRefusal | null {
  const error = errorOf(body);
  if (status === 401) return { kind: 'sessionExpired' };
  if (status === 429) return { kind: 'rateLimited' };
  if (status === 403) {
    if (error === 'Incorrect password') return { kind: 'invalidPassword' };
    if (error === 'Invalid TOTP code') return { kind: 'invalidMfaCode' };
    return null;
  }
  if (status === 400 && error === 'Password and TOTP code are required') return { kind: 'failed' };
  return null;
}

/**
 * The flags a dangerous-action gate puts on its 429 and 503 (#3456 V15):
 * `stepup.Budget`'s two refusals and `mfaenforce.WriteBusy`. Each is read as
 * `=== true` only.
 */
interface DangerousActionFlags {
  step_up_budget_exhausted?: unknown;
  step_up_budget_unavailable?: unknown;
  lock_conflict?: unknown;
}

/** Narrows a parsed body to its flags without trusting the body's shape. */
function flagsOf(body: unknown): DangerousActionFlags {
  return typeof body === 'object' && body !== null ? body : {};
}

/**
 * The 403 kinds a D1 gate writes. No D1 gate reads a password (#3456 V18), so
 * `passwordRequired` and `invalidPassword` are not its answers, and
 * `deleteRateLimited` is the soft-lock's: each is `null`, and so is `failed`.
 */
function dangerousActionForbidden(refusal: StepUpRefusal): StepUpFactorRefusal | null {
  switch (refusal.kind) {
    case 'enrollmentRequired':
    case 'mfaRequired':
    case 'invalidMfaCode':
      return refusal;
    default:
      return null;
  }
}

/**
 * A refusal from a dangerous-action (D1) gate, `mfaenforce.Require`, or the
 * MFA-enforcement toggle's OFF confirmation, `mfaenforce.ConfirmTx` (#3456
 * §3.2).
 *
 * The 429 and 503 are owned only when flagged: the budget's 429 is charged
 * before the gate's transaction, so the code it carried was not spent; the
 * budget's 503 and the gate's lock conflict are `unavailable`. An unflagged
 * 429 or 503 is the gated route's own limiter or failure, so it is `null` and
 * the host's own mapping runs. A 403 goes through `classifyStepUpRefusal`,
 * which keeps its flag-first order (`enrollmentRequired` first).
 */
export function adaptDangerousActionRefusal(
  status: number,
  body: unknown
): StepUpFactorRefusal | null {
  const flags = flagsOf(body);
  if (status === 429) {
    return flags.step_up_budget_exhausted === true ? { kind: 'rateLimited' } : null;
  }
  if (status === 503) {
    const owned = flags.step_up_budget_unavailable === true || flags.lock_conflict === true;
    return owned ? { kind: 'unavailable' } : null;
  }
  if (status === 403) return dangerousActionForbidden(classifyStepUpRefusal(403, body));
  if (status === 401) return { kind: 'sessionExpired' };
  return null;
}
