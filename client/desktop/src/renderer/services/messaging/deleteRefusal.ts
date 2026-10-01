/**
 * Pure mapper from a refused single-message DELETE onto the view
 * `DeleteRefusalModal` renders (#3455 §2.10, D-1).
 *
 * The wire carries several logically distinct refusals behind the SAME HTTP
 * status — a route-limiter 429 and a spent-budget 429; a soft-lock-unavailable
 * 503, a budget-unavailable 503 and a lock-conflict 503 — so every branch here
 * routes by a machine-readable flag or an exact frozen string, never by
 * status alone (design spec §2.8, X11/X17). This module owns that reading in
 * one place; `useChatController` never inspects the body itself.
 */

import { classifyStepUpRefusal } from '../system/stepUpRefusal';
import { STEP_UP_TOKEN_EXPIRED_MESSAGE } from '../system/stepUpToken';

export type DeleteRefusalView =
  | { view: 'confirm'; methods: string[]; error?: string }
  /**
   * D-1's own-rule path: a plain password field, never MFAVerifyPrompt. The
   * password goes to the mint endpoint, never to the delete route (#3509).
   */
  | { view: 'password'; error?: string }
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
    case 'passwordRequired':
      // A refused step-up token (#3509) re-prompts with its own copy; the
      // caller remounts the field empty because the view keeps an error.
      return refusal.tokenExpired
        ? { view: 'password', error: STEP_UP_TOKEN_EXPIRED_MESSAGE }
        : { view: 'password' };
    case 'invalidPassword':
      return { view: 'password', error: 'That password is not correct.' };
    case 'invalidMfaCode':
      return prior?.view === 'confirm'
        ? {
            view: 'confirm',
            methods: prior.methods,
            error: "That didn't work. Try again with a new code.",
          }
        : { view: 'failed', message: "That didn't work. Try again with a new code." };
    default:
      // Includes `mfa_enrollment_required`: the server's own text already
      // says the fix ("Set up an authenticator app or security key to do
      // this."). #3456 turns it into a link (T10) — nothing to build here.
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
 *   its methods) open rather than falling back to a bare failure — the caller
 *   is responsible for bumping the prompt key (#3466) when it sees this
 *   pairing; this function has no component to remount.
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
