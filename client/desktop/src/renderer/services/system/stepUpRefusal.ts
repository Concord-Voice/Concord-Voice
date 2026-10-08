/**
 * The one classifier for a refusal from a step-up-gated route.
 *
 * Every route that asks for present-tense proof (password, MFA code, or both)
 * answers with the `internal/stepup` bodies on the control plane. Four clients
 * read them — the MFA settings actions (`mfaStepUp.ts`), the recovery-key
 * first store (`MFASetup.tsx`), the purge-fence toggle (`privacyStore.ts`) and
 * the DM purge (`purgeApi.ts`) — and each used to carry its own copy of this
 * parse. Copies drift: a fix to one (a new status, a new flag) left the others
 * reading the old contract. Each consumer now maps this result onto its own
 * union, so its callers keep their shape while the wire is read in one place.
 *
 * The boolean flags are the intended contract and are matched first. The
 * two exact string comparisons are NOT: `Invalid password` and `Invalid MFA
 * code` carry no machine-readable discriminator, so equality on the frozen seam
 * strings is the only signal (handoff X1). Rewording either server string
 * degrades a per-field error to the generic `failed` arm, which misreports
 * nothing. Never widen these to substring matches.
 */

export interface StepUpRefusalBody {
  error?: unknown;
  password_required?: unknown;
  mfa_required?: unknown;
  methods?: unknown;
  inline_factor_required?: unknown;
  /** #3455: set on the delete-rate soft-lock's three 403 refusals only. */
  delete_rate_limited?: unknown;
  /**
   * #3509: set beside `password_required` when an own-rule route was sent a
   * `step_up_token` that matched nothing (spent, expired, or stranded by a
   * credential change). The remedy is the same password prompt, again.
   */
  step_up_token_invalid?: unknown;
  /**
   * #3464: the action needs an inline confirmation and the actor holds no
   * inline factor (`stepup.EnrollmentRequired`). Carries no `mfa_required`.
   */
  mfa_enrollment_required?: unknown;
}

export type StepUpRefusal =
  /** `tokenExpired`: the route refused a password step-up token (#3509). */
  | { kind: 'passwordRequired'; tokenExpired?: true }
  | { kind: 'mfaRequired'; methods: string[] }
  /**
   * #3455: the delete-rate soft-lock's MFA refusal. Carries the same
   * `methods` shape as `mfaRequired` but is a distinct kind so a delete-path
   * consumer can route it to its own confirm view without a route it does
   * not own (a settings dialog, a purge fence) picking it up by accident —
   * those map it to their generic failure arm instead.
   */
  | { kind: 'deleteRateLimited'; methods: string[] }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidMfaCode' }
  /**
   * #3464: no code this account could send would pass, because it has no
   * authenticator app or security key. Terminal: the remedy is enrolment.
   */
  | { kind: 'enrollmentRequired' }
  | { kind: 'rateLimited' }
  /** 503: the attempt budget could not be evaluated. Nothing was checked or changed. */
  | { kind: 'unavailable' }
  /** 409: the change would leave email/SMS as the only second factor. */
  | { kind: 'inlineFactorRequired'; message: string }
  | { kind: 'sessionExpired' }
  /** Anything else. `message` is the server's own text when it sent one. */
  | { kind: 'failed'; message?: string };

/** Fallback for a 409 whose body carries no usable text. Mirrors the server copy. */
export const INLINE_FACTOR_REQUIRED_MESSAGE =
  'Turn off email and text-message codes before removing your last authenticator app or security key.';

/** Narrows an unknown JSON value to the refusal body without trusting its shape. */
function asBody(body: unknown): StepUpRefusalBody {
  return typeof body === 'object' && body !== null ? body : {};
}

/** The server's `error` string, when it is a non-empty string. */
function serverMessage(body: StepUpRefusalBody): string | undefined {
  if (typeof body.error !== 'string') return undefined;
  const trimmed = body.error.trim();
  return trimmed === '' ? undefined : trimmed;
}

function classifyForbidden(body: StepUpRefusalBody): StepUpRefusal {
  // Matched before everything, with or without the soft-lock's
  // `delete_rate_limited`, which the soft-lock adds to every 403 it writes
  // (E8). Read as anything else it invites a retry no input can complete.
  if (body.mfa_enrollment_required === true) return { kind: 'enrollmentRequired' };
  // Matched next (#3455 §2.10): the delete-rate soft-lock always pairs
  // `delete_rate_limited` with `mfa_required`, and a route that does not know
  // about the soft-lock must never mistake this for its own `mfaRequired`.
  if (body.delete_rate_limited === true && body.mfa_required === true) {
    const methods = Array.isArray(body.methods)
      ? body.methods.filter((m): m is string => typeof m === 'string')
      : [];
    return { kind: 'deleteRateLimited', methods };
  }
  if (body.password_required === true) {
    return body.step_up_token_invalid === true
      ? { kind: 'passwordRequired', tokenExpired: true }
      : { kind: 'passwordRequired' };
  }
  if (body.mfa_required === true) {
    const methods = Array.isArray(body.methods)
      ? body.methods.filter((m): m is string => typeof m === 'string')
      : [];
    return { kind: 'mfaRequired', methods };
  }
  if (body.error === 'Invalid password') return { kind: 'invalidPassword' };
  if (body.error === 'Invalid MFA code') return { kind: 'invalidMfaCode' };
  return { kind: 'failed', message: serverMessage(body) };
}

/**
 * Classifies a NON-2xx response from a step-up-gated route. `body` is the
 * parsed JSON, or anything at all when parsing failed — it is never trusted.
 */
export function classifyStepUpRefusal(status: number, rawBody: unknown): StepUpRefusal {
  const body = asBody(rawBody);
  switch (status) {
    case 401:
      return { kind: 'sessionExpired' };
    case 403:
      return classifyForbidden(body);
    case 409:
      if (body.inline_factor_required === true) {
        return {
          kind: 'inlineFactorRequired',
          message: serverMessage(body) ?? INLINE_FACTOR_REQUIRED_MESSAGE,
        };
      }
      return { kind: 'failed', message: serverMessage(body) };
    case 429:
      return { kind: 'rateLimited' };
    case 503:
      return { kind: 'unavailable' };
    default:
      return { kind: 'failed', message: serverMessage(body) };
  }
}

/** True for the four kinds that belong to a credential field rather than a banner. */
export function isStepUpFactorRefusal(
  refusal: StepUpRefusal
): refusal is Extract<
  StepUpRefusal,
  { kind: 'passwordRequired' | 'mfaRequired' | 'invalidPassword' | 'invalidMfaCode' }
> {
  return (
    refusal.kind === 'passwordRequired' ||
    refusal.kind === 'mfaRequired' ||
    refusal.kind === 'invalidPassword' ||
    refusal.kind === 'invalidMfaCode'
  );
}
