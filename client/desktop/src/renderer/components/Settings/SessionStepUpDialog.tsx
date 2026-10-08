import React, { useRef, useState } from 'react';
import Modal from '../ui/Modal';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import {
  LEG_ONLY_WITHOUT_MFA,
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
import { adaptSessionsRefusal, serverErrorText } from '../../services/system/stepUpRouteAdapters';
import ErrorBanner from './ErrorBanner';

/**
 * The three session actions that ask for a step-up (design
 * 2026-09-26-mfa-factor-picker §4.4 #7): revoking one session, revoking all of
 * them, and changing the revocation mode. They share the route family's
 * `authenticateForRevoke`, so they share one dialog.
 */
export type SessionStepUpAction =
  | { kind: 'revoke'; sessionId: string }
  | { kind: 'revokeAll' }
  | { kind: 'modeChange'; mode: 'simple' | 'secure' };

/**
 * The dialog's React `key`: one instance per action, so a dialog is never
 * re-pointed at another action while its credentials and factor state belong
 * to the first.
 */
export function sessionStepUpKey(action: SessionStepUpAction): string {
  switch (action.kind) {
    case 'revoke':
      return `revoke:${action.sessionId}`;
    case 'revokeAll':
      return 'revokeAll';
    case 'modeChange':
      return `modeChange:${action.mode}`;
  }
}

export interface SessionStepUpDialogProps {
  action: SessionStepUpAction;
  /**
   * The refusal that opened a refusal-triggered dialog: single revoke sends no
   * credential first and opens this on `auth_required` / `password_required`.
   * Revoke-all and the mode change open up front and omit it. Read once, when
   * the dialog mounts (G2), so the parent mounts the dialog in the same commit
   * that sets it.
   */
  seed?: StepUpFactorRefusal | null;
  /** Cancel, Escape and the close button all land here. */
  onClose: () => void;
  /**
   * The server accepted the action. It runs only while the account and server
   * the request was sent for are still current: an answer that lands after a
   * change belongs to the old one, and the parent must not act on it.
   */
  onAccepted: (action: SessionStepUpAction) => void;
}

/** What one action asks for, says and sends. */
interface ActionSpec {
  purpose: StepUpPurpose;
  /** Constant per action: `ui/Modal` binds it to `aria-labelledby`. */
  title: string;
  description: string;
  cancel: string;
  confirm: string;
  confirming: string;
  /** The sentence for an answer that is not a step-up refusal and says nothing itself. */
  failure: string;
  path: string;
  method: 'POST' | 'PUT' | 'DELETE';
  /** The request body before the credentials join it. */
  body: Record<string, unknown>;
}

function specFor(action: SessionStepUpAction): ActionSpec {
  switch (action.kind) {
    case 'revoke':
      return {
        purpose: 'sessions.revoke',
        title: 'Verify Your Identity',
        description: "For your security, confirm it's you to revoke this session.",
        cancel: 'Cancel',
        confirm: 'Confirm & Revoke',
        confirming: 'Revoking...',
        failure: 'Failed to revoke session',
        path: `/api/v1/sessions/${encodeURIComponent(action.sessionId)}`,
        method: 'DELETE',
        body: {},
      };
    case 'revokeAll':
      return {
        purpose: 'sessions.revoke_all',
        title: 'Revoke All Sessions',
        description:
          "You're about to revoke all of your active session tokens, which will log you out of all sessions, including this one. Confirm it's you to continue.",
        cancel: 'No, Cancel',
        confirm: 'Yes, Revoke All Sessions',
        confirming: 'Revoking...',
        failure: 'Failed to revoke sessions',
        path: '/api/v1/sessions/revoke-all',
        method: 'POST',
        body: { include_current: true },
      };
    case 'modeChange':
      return {
        purpose: 'sessions.revocation_mode_set',
        title: 'Change Revocation Mode',
        description:
          action.mode === 'simple'
            ? 'Switching to Simple Revocation. You will be able to authenticate once and freely manage sessions for a short period.'
            : 'Switching to Secure Revocation. Authentication will be required to revoke sessions under certain circumstances.',
        cancel: 'Cancel',
        confirm: 'Confirm',
        confirming: 'Changing...',
        failure: 'Failed to change revocation mode',
        path: '/api/v1/sessions/revocation-mode',
        method: 'PUT',
        body: { mode: action.mode },
      };
  }
}

/**
 * What the request came to. The adapter's refusals go to the hook, a 401's
 * `sessionExpired` included; everything else the dialog says itself.
 */
type SessionResult =
  | { kind: 'accepted' }
  | { kind: 'refusal'; refusal: StepUpFactorRefusal }
  | { kind: 'failed'; message: string }
  | { kind: 'transport' }
  | { kind: 'aborted' };

const SESSION_EXPIRED_TEXT = 'Your session has expired. Sign in again to continue.';
const NETWORK_TEXT = "Couldn't reach the server. Check your connection and try again.";
const RATE_LIMITED_TEXT = 'Too many attempts. Try again in a few minutes.';

/**
 * A non-2xx response through the route adapter. It reads exact strings only
 * (C5), so a response it does not own (`null`) shows the server's own sentence.
 */
function refusedResult(status: number, body: unknown, failure: string): SessionResult {
  const refusal = adaptSessionsRefusal(status, body);
  if (refusal === null) return { kind: 'failed', message: serverErrorText(body) ?? failure };
  // A 401 included: the hook ends the stage in its terminal state, which drops
  // the password and says why. As `failed` the stage stayed live and could only
  // send the credentials again under a dead session.
  return { kind: 'refusal', refusal };
}

/**
 * Sends the action once, under `context`. A WebAuthn token travels as
 * `mfa_code`, exactly as a typed code does. Never throws.
 */
async function sendSessionRequest(
  spec: ActionSpec,
  password: string,
  mfa: string | undefined,
  context: ApiRequestContext
): Promise<SessionResult> {
  const payload: Record<string, unknown> = { ...spec.body };
  if (password) payload.password = password;
  if (mfa) payload.mfa_code = mfa;
  try {
    const res = await apiFetchInContext(
      spec.path,
      {
        method: spec.method,
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(payload),
      },
      context
    );
    if (res.ok) return { kind: 'accepted' };
    const body: unknown = await res.json().catch(() => null);
    return refusedResult(res.status, body, spec.failure);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'transport' };
  }
}

function toOutcome(result: SessionResult): StepUpSubmitOutcome {
  switch (result.kind) {
    case 'accepted':
      return { kind: 'success' };
    case 'refusal':
      return { kind: 'refusal', refusal: result.refusal };
    case 'failed':
      return { kind: 'answered' };
    case 'transport':
      return { kind: 'transport' };
    case 'aborted':
      return { kind: 'aborted' };
  }
}

/** The banner sentence for what the credentials do not render, or '' when there is none. */
function bannerFor(result: SessionResult): string {
  switch (result.kind) {
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

/** The primary's label by phase; the in-flight ones come from the action. */
function primaryLabel(phase: StepUpPhase, spec: ActionSpec): string {
  switch (phase) {
    case 'idle':
      return spec.confirm;
    case 'ceremony':
      return 'Waiting…';
    case 'submitting':
      return spec.confirming;
  }
}

/**
 * Step-up for the session actions (#7). One dialog serves single revoke,
 * revoke-all and the revocation-mode change, so the three do not carry three
 * copies of the same credential markup and request handling.
 *
 * The password leg is `whenNoMfa`: the field appears only when the account
 * holds no inline method, and which methods those are comes from the
 * requirements read or a refusal, never from `users.mfa_methods` (C2). The
 * route also accepts a password alone from an MFA account; offering the code
 * first is a presentation choice, not a server rule.
 *
 * The parent mounts this when the action opens and unmounts it on close, so
 * the password and the factor state die with it. Neither is stored, persisted
 * or echoed into copy (`[internal]rules/observability.md`).
 */
const SessionStepUpDialog: React.FC<SessionStepUpDialogProps> = ({
  action,
  seed = null,
  onClose,
  onAccepted,
}) => {
  const spec = specFor(action);
  const [password, setPassword] = useState('');
  const [error, setError] = useState('');
  const primaryRef = useRef<HTMLButtonElement>(null);

  const factor = useStepUpFactor({
    enabled: true,
    purpose: spec.purpose,
    passwordLeg: LEG_ONLY_WITHOUT_MFA,
    readFailure: 'passwordOnly',
    allowBackup: true,
    seed,
  });
  const submitting = factor.phase === 'submitting';

  const submit: StepUpSubmit = async (mfa, context) => {
    const result = await sendSessionRequest(spec, password, mfa, context);
    // An answer for an account or server that is no longer current belongs to
    // the old one: the hook ends the attempt, and this dialog must neither show
    // it nor act on it.
    if (!apiRequestContextIsCurrent(context)) return toOutcome(result);
    setError(bannerFor(result));
    // Only the password the server rejected is dropped; the hook keeps the code
    // through a password refusal, so a wrong password does not cost a fresh one.
    if (result.kind === 'refusal' && result.refusal.kind === 'invalidPassword') setPassword('');
    if (result.kind === 'accepted') onAccepted(action);
    return toOutcome(result);
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);

  return (
    <Modal isOpen onClose={onClose} title={spec.title} width="small" dismissable={!submitting}>
      <div className="revoke-all-modal-content">
        <p className="revoke-all-modal-description">{spec.description}</p>
        {/* No initialFocusRef: the `whenNoMfa` password field exists only once the read
            has landed, so the stage places focus then. */}
        <StepUpCredentials
          factor={factor}
          password={password}
          onPasswordChange={setPassword}
          primaryRef={primaryRef}
          focusOnReady
          sessionMessage={SESSION_EXPIRED_TEXT}
        />
        <ErrorBanner error={error} />
        <div className="revoke-all-modal-actions">
          <button
            type="button"
            className="revoke-all-modal-cancel-btn"
            disabled={submitting}
            onClick={onClose}
          >
            {spec.cancel}
          </button>
          <button
            ref={primaryRef}
            type="button"
            className="revoke-all-modal-confirm-btn"
            aria-disabled={ariaDisabled || undefined}
            onClick={activate}
          >
            {primaryLabel(factor.phase, spec)}
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default SessionStepUpDialog;
