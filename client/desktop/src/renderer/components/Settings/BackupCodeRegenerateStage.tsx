import React, { useLayoutEffect, useRef, useState } from 'react';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  useStepUpFactor,
  type StepUpFactorRefusal,
  type StepUpPhase,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import {
  adaptBackupCodeRegenerateRefusal,
  serverErrorText,
} from '../../services/system/stepUpRouteAdapters';
import ErrorBanner from './ErrorBanner';

export interface BackupCodeRegenerateStageProps {
  /** The server issued new codes. They are shown once, by the host. */
  onRegenerated: (codes: string[]) => void;
  /**
   * TOTP was turned off after the stage opened (`400 TOTP is not enabled`).
   * The stage says so and offers only Close; the host re-reads the account's
   * MFA status, because what it shows is now stale (Q7).
   */
  onTotpRemoved: () => void;
  onCancel: () => void;
  /** The host's heading (`tabIndex={-1}`): where focus goes on a terminal state. */
  headingRef?: React.RefObject<HTMLElement | null>;
}

/**
 * TOTP is this route's floor and its only factor: `RegenerateBackupCodes`
 * binds a TOTP `code` and checks it with `MatchCodeStep`, never a backup code
 * and never an inline token. With no purpose there is no read, so nothing can
 * add to it (C4).
 */
const FLOOR = ['totp'] as const;

const GENERIC_FAILURE = 'Something went wrong. Try again.';
const NETWORK_TEXT = "Couldn't reach the server. Check your connection and try again.";
const RATE_LIMITED_TEXT = 'Too many attempts. Try again in a few minutes.';
const UNREADABLE_CODES_TEXT = "We couldn't read your new backup codes. Generate them again.";
const TOTP_REMOVED_TEXT =
  'Your authenticator app was turned off. Close this and check your security settings.';

/**
 * The route's 400 when TOTP was turned off after the stage opened. Matched
 * exactly, as the adapter's strings are (C5): a reworded one is a plain
 * failure, never an ended stage.
 */
const TOTP_NOT_ENABLED = 'TOTP is not enabled';

/**
 * What the request came to. The two refusals of what the user entered, a rate
 * limit (which proves the code unspent) and a 401's dead session go to the
 * hook; `totpRemoved` ends the stage; everything else is the stage's to say.
 */
type RegenerateResult =
  | { kind: 'regenerated'; codes: string[] }
  | { kind: 'refusal'; refusal: StepUpFactorRefusal }
  | { kind: 'totpRemoved' }
  | { kind: 'failed'; message: string }
  | { kind: 'transport' }
  | { kind: 'aborted' };

function backupCodesIn(body: unknown): string[] {
  if (typeof body !== 'object' || body === null || !('backup_codes' in body)) return [];
  const codes = body.backup_codes;
  return Array.isArray(codes) ? codes.filter((c): c is string => typeof c === 'string') : [];
}

/**
 * A 2xx. The server has replaced the old codes by now, so an answer that
 * carries none the stage can show is a failure, never an empty list: the host
 * would show the user nothing to save.
 */
function acceptedResult(body: unknown): RegenerateResult {
  const codes = backupCodesIn(body);
  return codes.length > 0
    ? { kind: 'regenerated', codes }
    : { kind: 'failed', message: UNREADABLE_CODES_TEXT };
}

/**
 * A non-2xx response. TOTP turned off ends the stage; everything else goes
 * through the route adapter, which reads exact strings only (C5): a response
 * it does not own (`null`) shows the server's own sentence.
 */
function refusedResult(status: number, body: unknown): RegenerateResult {
  if (status === 400 && serverErrorText(body) === TOTP_NOT_ENABLED) return { kind: 'totpRemoved' };
  const refusal = adaptBackupCodeRegenerateRefusal(status, body);
  if (refusal === null) {
    return {
      kind: 'failed',
      message: serverErrorText(body) ?? 'Failed to regenerate backup codes',
    };
  }
  if (refusal.kind === 'failed') return { kind: 'failed', message: GENERIC_FAILURE };
  return { kind: 'refusal', refusal };
}

/**
 * Sends the regeneration once, under `context`. The route binds the code as
 * `code`, not the seam's `mfa_code`. Never throws.
 */
async function regenerate(
  password: string,
  code: string | undefined,
  context: ApiRequestContext
): Promise<RegenerateResult> {
  try {
    const res = await apiFetchInContext(
      '/api/v1/mfa/backup-codes/regenerate',
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password, code: code ?? '' }),
      },
      context
    );
    const body: unknown = await res.json().catch(() => null);
    return res.ok ? acceptedResult(body) : refusedResult(res.status, body);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'transport' };
  }
}

function toOutcome(result: RegenerateResult): StepUpSubmitOutcome {
  switch (result.kind) {
    case 'regenerated':
      return { kind: 'success' };
    case 'refusal':
      return { kind: 'refusal', refusal: result.refusal };
    case 'totpRemoved':
    case 'failed':
      return { kind: 'answered' };
    case 'transport':
      return { kind: 'transport' };
    case 'aborted':
      return { kind: 'aborted' };
  }
}

/** The banner sentence for what the credentials do not render, or '' when there is none. */
function bannerFor(result: RegenerateResult): string {
  switch (result.kind) {
    case 'totpRemoved':
      return TOTP_REMOVED_TEXT;
    case 'failed':
      return result.message;
    case 'transport':
      return NETWORK_TEXT;
    case 'refusal':
      // The hook renders the field refusals; a rate limit belongs to no field.
      return result.refusal.kind === 'rateLimited' ? RATE_LIMITED_TEXT : '';
    default:
      return '';
  }
}

const PRIMARY_LABEL: Record<StepUpPhase, string> = {
  idle: 'Regenerate Codes',
  ceremony: 'Waiting…',
  submitting: 'Regenerating...',
};

/**
 * Credentials and actions for backup-code regeneration (#8): the password and
 * an authenticator-app code, then Regenerate and Cancel. The host owns the
 * frame and the heading, and shows the new codes when `onRegenerated` fires.
 * Extracted so the exhaustion prompt (PR 6) mounts the same stage.
 *
 * TOTP only: a backup code cannot regenerate codes, and the route takes no
 * inline token, so `allowBackup` is false and the hook has no purpose to read
 * for. The password and the code are never stored or echoed into copy
 * (`[internal]rules/observability.md`).
 */
const BackupCodeRegenerateStage: React.FC<BackupCodeRegenerateStageProps> = ({
  onRegenerated,
  onTotpRemoved,
  onCancel,
  headingRef,
}) => {
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const [totpRemoved, setTotpRemoved] = useState(false);
  const primaryRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);

  const factor = useStepUpFactor({
    enabled: true,
    purpose: null,
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: false,
    floorMethods: FLOOR,
  });
  const submitting = factor.phase === 'submitting';

  // The ended stage unmounts the primary that held focus; Close takes it.
  useLayoutEffect(() => {
    if (totpRemoved) cancelRef.current?.focus();
  }, [totpRemoved]);

  const submit: StepUpSubmit = async (mfa, context) => {
    const result = await regenerate(password, mfa, context);
    // An answer for an account or server that is no longer current belongs to
    // the old one: the hook ends the attempt, and this stage must neither show
    // it nor hand its codes to the host.
    if (!apiRequestContextIsCurrent(context)) return toOutcome(result);
    setError(bannerFor(result));
    // Only the password the server rejected is dropped; the hook keeps the code
    // through a password refusal, so a wrong password does not cost a fresh one.
    if (result.kind === 'refusal' && result.refusal.kind === 'invalidPassword') setPassword('');
    if (result.kind === 'totpRemoved') {
      setPassword('');
      setTotpRemoved(true);
      onTotpRemoved();
    }
    if (result.kind === 'regenerated') onRegenerated(result.codes);
    return toOutcome(result);
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);

  return (
    <div className="mfa-action-body">
      {!totpRemoved && (
        <StepUpCredentials
          factor={factor}
          password={password}
          onPasswordChange={setPassword}
          primaryRef={primaryRef}
          headingRef={headingRef}
        />
      )}
      <ErrorBanner error={error} />
      <div className="mfa-setup-actions">
        {!totpRemoved && (
          <button
            ref={primaryRef}
            type="button"
            className="btn btn-sm btn-primary"
            aria-disabled={ariaDisabled || undefined}
            onClick={activate}
          >
            {PRIMARY_LABEL[factor.phase]}
          </button>
        )}
        <button
          ref={cancelRef}
          type="button"
          className="btn btn-sm btn-secondary"
          disabled={submitting}
          onClick={onCancel}
        >
          {totpRemoved ? 'Close' : 'Cancel'}
        </button>
      </div>
    </div>
  );
};

export default BackupCodeRegenerateStage;
