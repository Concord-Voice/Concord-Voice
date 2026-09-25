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
 * The two boolean flags are the intended contract and are matched first. The
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
}

export type StepUpRefusal =
  | { kind: 'passwordRequired' }
  | { kind: 'mfaRequired'; methods: string[] }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidMfaCode' }
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
  if (body.password_required === true) return { kind: 'passwordRequired' };
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
