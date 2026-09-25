import { apiFetch } from '../../services/system/apiClient';
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
   * server selection changed under it. Nothing was sent, so nothing is
   * reported — telling the user the network failed would be false.
   */
  | { kind: 'aborted' };

/** Maps a response from a step-up-gated route onto {@link MfaStepUpResult}. */
export async function mapMfaStepUpResponse(res: Response): Promise<MfaStepUpResult> {
  if (res.ok) {
    const data: unknown = await res.json().catch(() => null);
    return { kind: 'accepted', data };
  }
  const body: unknown = await res.json().catch(() => ({}));
  return classifyStepUpRefusal(res.status, body);
}

/** `apiFetch` throws a DOMException named AbortError; jsdom's is not an Error subclass. */
function isAbortError(err: unknown): boolean {
  return (err instanceof DOMException || err instanceof Error) && err.name === 'AbortError';
}

/**
 * Sends a step-up-gated request. The code is sent only when non-empty — an
 * empty string reads to the server as a supplied-and-wrong code. `codeField`
 * exists for `POST /mfa/totp/disable`, which predates the seam and binds the
 * code as `code` rather than `mfa_code`.
 */
export async function submitMfaStepUp(
  path: string,
  method: 'POST' | 'PUT' | 'DELETE',
  body: Record<string, unknown>,
  credentials: { password: string; mfaCode: string },
  opts: { codeField?: 'mfa_code' | 'code' } = {}
): Promise<MfaStepUpResult> {
  const payload: Record<string, unknown> = { ...body, password: credentials.password };
  if (credentials.mfaCode) payload[opts.codeField ?? 'mfa_code'] = credentials.mfaCode;
  try {
    const res = await apiFetch(path, {
      method,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(payload),
    });
    return await mapMfaStepUpResponse(res);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'networkError' };
  }
}

// ── Shared presentation (action modal + recovery-key replace step) ─────────
//
// Both surfaces route one refusal to the same place with the same words. They
// used to hold a copy each; the copies are why the replace step's lock could
// be dropped on reopen while the modal's held. Copy matches the purge-fence
// dialog word for word (handoff §1.1, T10).

/** Password-field error for a refusal, or undefined when it is not the password's. */
export function stepUpPasswordError(result: MfaStepUpResult | null): string | undefined {
  if (result?.kind === 'passwordRequired') return 'Enter your password to continue.';
  if (result?.kind === 'invalidPassword') return 'That password is not correct.';
  return undefined;
}

/** Code-prompt error for a refusal, or undefined when it is not the code's. */
export function stepUpMfaError(result: MfaStepUpResult | null): string | undefined {
  if (result?.kind === 'mfaRequired') {
    return 'Verify with your authenticator app or security key to continue.';
  }
  if (result?.kind === 'invalidMfaCode') {
    return 'That code is not correct, or it has expired. Try the next one.';
  }
  return undefined;
}

/** General-banner copy for a refusal, or null when it belongs to a field (or to nothing). */
export function stepUpBanner(result: MfaStepUpResult | null): string | null {
  switch (result?.kind) {
    case 'rateLimited':
      return 'Too many attempts. Try again in a few minutes.';
    case 'sessionExpired':
      return 'Your session needs to be verified again. Sign in again to continue.';
    case 'unavailable':
      return 'Verification is temporarily unavailable. Try again in a few minutes.';
    case 'inlineFactorRequired':
      return result.message;
    case 'networkError':
      return "Couldn't reach the server. Check your connection and try again.";
    case 'failed':
      return result.message ?? 'Something went wrong. Try again.';
    default:
      return null;
  }
}

/**
 * True when Confirm must stay disabled until the surface is reopened: the
 * budget is spent, or the session is gone. A speed bump — the server budget
 * is the enforcement.
 */
export function isStepUpLocked(result: MfaStepUpResult | null): boolean {
  return result?.kind === 'rateLimited' || result?.kind === 'sessionExpired';
}

/** The subset of methods the server can verify inline (policy P1). */
export function inlineMfaMethods(methods: readonly string[]): string[] {
  return methods.filter((m) => m === 'totp' || m === 'webauthn');
}

/**
 * Methods the code prompt should offer. An `mfa_required` refusal carries the
 * server's present list, which wins over `fallback` — the last status fetch —
 * because a stale list is exactly how a prompt goes missing (F3).
 */
export function stepUpPromptMethods(
  result: MfaStepUpResult | null,
  fallback: readonly string[]
): string[] {
  if (result?.kind === 'mfaRequired') {
    const fromServer = inlineMfaMethods(result.methods);
    if (fromServer.length > 0) return fromServer;
  }
  return inlineMfaMethods(fallback);
}
