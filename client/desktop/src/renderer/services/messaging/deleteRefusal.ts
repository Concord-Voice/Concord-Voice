/**
 * Pure mapper from a refused single-message DELETE onto the view
 * `DeleteRefusalModal` renders (#3455 §2.10, D-1).
 *
 * The wire carries several logically distinct refusals behind the SAME HTTP
 * status — a route-limiter 429 and a spent-budget 429; a soft-lock-unavailable
 * 503, a budget-unavailable 503 and a lock-conflict 503 — so every branch here
 * routes by a machine-readable flag or an exact frozen string, never by
 * status alone (design spec §2.8, X11/X17). This module owns that reading in
 * one place; `useChatController` never inspects the body itself. It also gives
 * the step-up factor hook its seed and its outcome for the same refusal, and
 * maps a refused password exchange onto the view, for delete and purge alike.
 */

import type { StepUpFactorRefusal, StepUpSubmitOutcome } from '../../hooks/auth/useStepUpFactor';
import { classifyStepUpRefusal } from '../system/stepUpRefusal';
import {
  mintMfaMethods,
  passwordStepUpRefusalMessage,
  type PasswordStepUpMint,
} from '../system/stepUpToken';

export type DeleteRefusalView =
  | { view: 'confirm'; methods: string[] }
  /**
   * D-1's own-rule path: a plain password field, never a factor picker. The
   * password goes to the mint endpoint, never to the delete route (#3509).
   * `error` is a refused exchange's words (`mintRefusalView`), which end a
   * challenge the field cannot answer; a wrong password is worded by the
   * credential stage itself.
   */
  | { view: 'password'; error?: string }
  /**
   * E8: the account holds no authenticator app or security key, so nothing it
   * could type would pass. Terminal: no input, no Retry and no countdown, with
   * or without the soft-lock's `Retry-After`. The link is #3456's.
   */
  | { view: 'enroll' }
  | { view: 'wait'; reason: 'requests' | 'verification'; retryAfterSeconds?: number }
  /** Nothing was checked or changed. No countdown: the header is a guess. */
  | { view: 'unavailable' }
  | { view: 'failed'; message?: string; retryAfterSeconds?: number };

/** The frozen budget-exhausted 429 string (`cp/stepup/budget.go`). Exact match only — never widen to a substring test (see stepUpRefusal.ts's own warning about this class of string). */
const TOO_MANY_VERIFICATION_ATTEMPTS = 'Too many verification attempts';

/** `Retry-After` is spec-legal as an HTTP-date, which parseInt turns into NaN.
 *  Mirrors the `Number.isFinite && >= 0` guard at e2eeService.ts:603-604. */
function parseRetryAfter(header: string | null): number | undefined {
  if (header === null) return undefined;
  const parsed = Number.parseInt(header, 10);
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : undefined;
}

function asRecord(value: unknown): Record<string, unknown> {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : {};
}

function bodyErrorText(rawBody: unknown): string | undefined {
  const err = asRecord(rawBody).error;
  if (typeof err !== 'string') return undefined;
  const trimmed = err.trim();
  return trimmed === '' ? undefined : trimmed;
}

/** The view for a 403, from the step-up refusal it carries. */
function fromStepUpRefusal(
  refusal: ReturnType<typeof classifyStepUpRefusal>,
  retryAfterSeconds: number | undefined,
  prior: DeleteRefusalView | undefined
): DeleteRefusalView {
  switch (refusal.kind) {
    case 'deleteRateLimited':
      return { view: 'confirm', methods: refusal.methods };
    case 'mfaRequired':
      // Defensive only: the delete routes always pair `mfa_required` with
      // `delete_rate_limited` (design spec §2.8), so classifyStepUpRefusal
      // should already have matched `deleteRateLimited` above.
      return { view: 'confirm', methods: refusal.methods };
    // The credential stage words a refused password or code in place from the
    // hook's outcome (`toDeleteSubmitOutcome`), so these views carry no copy.
    case 'passwordRequired':
    case 'invalidPassword':
      return { view: 'password' };
    case 'invalidMfaCode':
      return prior?.view === 'confirm'
        ? { view: 'confirm', methods: prior.methods }
        : { view: 'failed', message: "That didn't work. Try again with a new code." };
    case 'enrollmentRequired':
      // Never `failed`: its countdown and Retry invite an attempt that can
      // only be refused the same way.
      return { view: 'enroll' };
    default:
      return {
        view: 'failed',
        message: refusal.kind === 'failed' ? refusal.message : undefined,
        retryAfterSeconds,
      };
  }
}

/**
 * Maps a non-2xx response from `DELETE /channels/:cid/messages/:id` or the DM
 * equivalent onto the view the modal renders.
 *
 * @param prior The view already on screen, when this is a retry. Only read to
 *   decide whether an `Invalid MFA code` refusal keeps the confirm view (with
 *   its methods) open rather than falling back to a bare failure.
 */
export function toDeleteRefusalView(
  status: number,
  rawBody: unknown,
  retryAfterHeader: string | null,
  prior?: DeleteRefusalView
): DeleteRefusalView {
  const retryAfterSeconds = parseRetryAfter(retryAfterHeader);

  if (status === 429) {
    const body = asRecord(rawBody);
    const reason: 'requests' | 'verification' =
      body.step_up_budget_exhausted === true ||
      bodyErrorText(rawBody) === TOO_MANY_VERIFICATION_ATTEMPTS
        ? 'verification'
        : 'requests';
    return { view: 'wait', reason, retryAfterSeconds };
  }

  if (status === 503) {
    // Soft-lock-unavailable, budget-unavailable and lock-conflict (X17) are
    // "nothing happened, try again" from here: none is fixed by typing
    // anything, so all three render the same Close-only body.
    return { view: 'unavailable' };
  }

  if (status === 403) {
    return fromStepUpRefusal(classifyStepUpRefusal(403, rawBody), retryAfterSeconds, prior);
  }

  // 401 (epoch mismatch, or the confirmation path's users row gone), a 404 on
  // the ORIGINAL attempt (a retry's 404 is handled by the caller before this
  // function ever runs), 500, and anything else: the server's own text when
  // it sent one, with a countdown appended only when the header is present.
  return { view: 'failed', message: bodyErrorText(rawBody), retryAfterSeconds };
}

/**
 * The factor hook's reading of the same refusal (D6). The soft-lock's own
 * `deleteRateLimited` is the hook's `mfaRequired`, because the hook knows no
 * delete-specific kind. A refusal no credential can answer is `answered`.
 */
export function toDeleteSubmitOutcome(status: number, rawBody: unknown): StepUpSubmitOutcome {
  const refusal = classifyStepUpRefusal(status, rawBody);
  switch (refusal.kind) {
    case 'deleteRateLimited':
      return { kind: 'refusal', refusal: { kind: 'mfaRequired', methods: refusal.methods } };
    case 'failed':
    case 'unavailable':
    case 'inlineFactorRequired':
      return { kind: 'answered' };
    default:
      return { kind: 'refusal', refusal };
  }
}

/**
 * The challenge a refused password exchange (#3509) leaves on screen. An
 * account that enrolled MFA after the prompt opened can never pass a password
 * prompt, so a mint that names its methods moves to the code prompt; any other
 * refusal stays on the password view with the exchange's words. Only a wrong
 * password is something that view can answer, so a caller ends the challenge
 * with those words for the rest. A caller handles an `unsent` exchange before
 * this (D7): nothing was checked, so there is nothing to say.
 */
export function mintRefusalView(
  refusal: Extract<PasswordStepUpMint, { kind: 'refused' }>
): Extract<DeleteRefusalView, { view: 'confirm' | 'password' }> {
  const methods = mintMfaMethods(refusal);
  return methods
    ? { view: 'confirm', methods }
    : { view: 'password', error: passwordStepUpRefusalMessage(refusal) };
}

/**
 * The factor hook's seed for the view that opened the dialog (G2), or null
 * for a view that hosts no credential. A `deleteRateLimited` seed would seed
 * nothing, so the confirm view seeds the `mfaRequired` its methods came from.
 * The password view is the own rule finding no inline method, which on a
 * `whenNoMfa` leg seeds the empty set. Enrolment ends the instance at once.
 */
export function softLockSeed(view: DeleteRefusalView): StepUpFactorRefusal | null {
  switch (view.view) {
    case 'confirm':
      return { kind: 'mfaRequired', methods: view.methods };
    case 'password':
      return { kind: 'passwordRequired' };
    case 'enroll':
      return { kind: 'enrollmentRequired' };
    default:
      return null;
  }
}
