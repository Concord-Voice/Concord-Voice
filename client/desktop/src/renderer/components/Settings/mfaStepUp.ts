import type { StepUpSubmitOutcome } from '../../hooks/auth/useStepUpFactor';
import {
  apiFetchInContext,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { classifyStepUpRefusal, type StepUpRefusal } from '../../services/system/stepUpRefusal';

/**
 * Outcome of a step-up-gated MFA settings request. Route on `kind`, never on
 * message text. The refusal kinds come from the shared classifier
 * (`services/system/stepUpRefusal.ts`); the two transport kinds are added here.
 */
export type MfaStepUpResult =
  | { kind: 'accepted'; data: unknown }
  | StepUpRefusal
  /** No response arrived. The server may or may not have acted. */
  | { kind: 'networkError' }
  /**
   * The request never left: `apiFetch` refuses to dispatch once the account or
   * server selection changed under it, or since the `context` it was given was
   * captured. Nothing was sent, so nothing is reported — telling the user the
   * network failed would be false.
   */
  | { kind: 'aborted' };

/**
 * A seam handler (#4–#6, #9, #10): the route's own leading arguments, then the
 * credentials and the capture of the `run` that called it. `mfaCode` is the
 * factor hook's value as it stands — undefined when no method is offered — and
 * `context` is required so no handler can send outside the run's capture (C82).
 * `MFATierSelector`'s props and `PrivacySecuritySection`'s handlers share it.
 */
export type MfaSeamHandler<Lead extends readonly unknown[] = []> = (
  ...args: [
    ...lead: Lead,
    password: string,
    mfaCode: string | undefined,
    context: ApiRequestContext,
  ]
) => Promise<MfaStepUpResult>;

/** Maps a response from a step-up-gated route onto {@link MfaStepUpResult}. */
export async function mapMfaStepUpResponse(res: Response): Promise<MfaStepUpResult> {
  if (res.ok) {
    const data: unknown = await res.json().catch(() => null);
    return { kind: 'accepted', data };
  }
  const body: unknown = await res.json().catch(() => ({}));
  return classifyStepUpRefusal(res.status, body);
}

export interface SubmitMfaStepUpOptions {
  /** `code` for `POST /mfa/totp/disable`, which predates the seam and binds it so. */
  codeField?: 'mfa_code' | 'code';
  /**
   * The operation's capture (`run`'s, or one taken before preparation). The
   * request then refuses to dispatch once the account or server has changed
   * since it, and resolves `aborted`. Omitted, the request is its own operation.
   */
  context?: ApiRequestContext;
}

/**
 * Sends a step-up-gated request. The code is sent only when non-empty — an
 * empty string reads to the server as a supplied-and-wrong code. Never throws.
 */
export async function submitMfaStepUp(
  path: string,
  method: 'POST' | 'PUT' | 'DELETE',
  body: Record<string, unknown>,
  credentials: { password: string; mfaCode: string | undefined },
  opts: SubmitMfaStepUpOptions = {}
): Promise<MfaStepUpResult> {
  const payload: Record<string, unknown> = { ...body, password: credentials.password };
  if (credentials.mfaCode) payload[opts.codeField ?? 'mfa_code'] = credentials.mfaCode;
  try {
    const res = await apiFetchInContext(
      path,
      { method, headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(payload) },
      opts.context
    );
    return await mapMfaStepUpResponse(res);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'networkError' };
  }
}

/**
 * The result in the factor hook's terms (`useStepUpFactor`'s `run`), after
 * `PurgeFenceStepUpDialog`'s `toOutcome`. The refusals the hook acts on go to
 * it as themselves, so `methods` and `tokenExpired` survive:
 *
 * - the four field refusals, and `enrollmentRequired`, whose terminal state the
 *   hook renders (E8);
 * - `rateLimited`, because the budget is charged before any factor is read
 *   (`mfa/settings_stepup.go` `openMFASettingsTx`), so a 429 proves the code
 *   unspent (C30). As `answered` it would be recorded as a TOTP acceptance.
 * - `sessionExpired`, a 401 after `apiFetch`'s refresh retry: the hook ends the
 *   stage in its terminal state, which drops the password and says why. As
 *   `answered` the stage stayed live, holding the password with an active
 *   primary that could only send it again under a dead session.
 *
 * Every other answer may have spent the code and is the surface's to render:
 * `answered`. `deleteRateLimited` is the delete soft-lock's and never a
 * settings route's, so it is a foreign answer here, not an MFA prompt.
 */
export function toStepUpSubmitOutcome(result: MfaStepUpResult): StepUpSubmitOutcome {
  switch (result.kind) {
    case 'accepted':
      return { kind: 'success' };
    case 'passwordRequired':
    case 'mfaRequired':
    case 'invalidPassword':
    case 'invalidMfaCode':
    case 'enrollmentRequired':
    case 'rateLimited':
    case 'sessionExpired':
      return { kind: 'refusal', refusal: result };
    case 'deleteRateLimited':
    case 'inlineFactorRequired':
    case 'unavailable':
    case 'failed':
      return { kind: 'answered' };
    case 'networkError':
      return { kind: 'transport' };
    case 'aborted':
      return { kind: 'aborted' };
  }
}

// ── Shared presentation (action modal + recovery-key replace step) ─────────
//
// The surfaces route one refusal to the same place with the same words. They
// used to hold a copy each; the copies are why the replace step's lock could
// be dropped on reopen while the modal's held. Copy matches the purge-fence
// dialog word for word (handoff §1.1, T10).

/** Password-field error for a refusal, or undefined when it is not the password's. */
export function stepUpPasswordError(result: MfaStepUpResult | null): string | undefined {
  if (result?.kind === 'passwordRequired') return 'Enter your password to continue.';
  if (result?.kind === 'invalidPassword') return 'That password is not correct.';
  return undefined;
}

/** General-banner copy for a refusal, or null when it belongs to a field (or to nothing). */
export function stepUpBanner(result: MfaStepUpResult | null): string | null {
  switch (result?.kind) {
    case 'rateLimited':
      return 'Too many attempts. Try again in a few minutes.';
    case 'unavailable':
      return 'Verification is temporarily unavailable. Try again in a few minutes.';
    case 'inlineFactorRequired':
      return result.message;
    // The factor hook's terminal states say these (StepUpCredentials, E8).
    // ErrorBanner is a `role="alert"` error, which E8 rules out, and a banner
    // beside the state would say it twice. A surface with no stage words the
    // dead session itself (`passwordOnlyBanner`).
    case 'enrollmentRequired':
    case 'sessionExpired':
      return null;
    case 'networkError':
      return "Couldn't reach the server. Check your connection and try again.";
    case 'failed':
      return result.message ?? 'Something went wrong. Try again.';
    default:
      return null;
  }
}

const SESSION_EXPIRED_BANNER = 'Your session has expired. Sign in again to continue.';

/**
 * The dangerous-action step-up's banner for a gate that could not decide (a busy
 * lock, an unavailable budget) and for a request that was never sent: the code
 * is not spent, and the same request may simply be tried again. One sentence
 * for the dialog and the purge stage alike (#3456 §3.3).
 */
export const STEP_UP_RETRY_TEXT = "We couldn't confirm that right now. Try again.";

/**
 * `stepUpBanner` for the one MFA-settings surface with no shared stage, the
 * password-only key revoke: no terminal state renders there, so the banner
 * says the session is gone.
 */
export function passwordOnlyBanner(result: MfaStepUpResult | null): string | null {
  return result?.kind === 'sessionExpired' ? SESSION_EXPIRED_BANNER : stepUpBanner(result);
}

/**
 * True when Confirm must stay disabled until the surface is reopened: the
 * budget is spent, or the session is gone. A speed bump — the server budget
 * is the enforcement.
 */
export function isStepUpLocked(result: MfaStepUpResult | null): boolean {
  return result?.kind === 'rateLimited' || result?.kind === 'sessionExpired';
}
