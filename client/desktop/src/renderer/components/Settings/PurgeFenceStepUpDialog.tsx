import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  useStepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import { usePrivacyStore, type PurgeFenceDisableResult } from '../../stores/ui/privacyStore';
import { apiRequestContextIsCurrent } from '../../services/system/requestContext';
// The frame and footer are the shipped #1354 markup, so they need the shipped
// #1354 stylesheet — the Settings pane never loads PurgeMessagesModal.
import '../Purge/purgeMessages.css';

interface PurgeFenceStepUpDialogProps {
  open: boolean;
  /** Cancel, Escape, and a completed disable all land here. */
  onClose: () => void;
}

/**
 * Constant across every stage: ui/Modal binds the title to aria-labelledby, so
 * mutating it renames the dialog mid-interaction (WCAG 4.1.2 / 3.2.2). The
 * words are the #1354 step-up heading — the challenge is the same challenge.
 */
const TITLE = 'Confirm it is you';

/**
 * The shipped #1354 warning, reused verbatim as the framing sentence. It is the
 * reason the transition is gated, so it is the reason to ask. The section
 * renders the same words only once the fence is actually OFF — by then this
 * dialog is closed, so the two never coexist.
 */
const DISABLE_WARNING =
  'Without this, anyone with access to your unlocked account can permanently purge your ' +
  'message history.';

/** The primary's label by phase; the two in-flight ones carry a spinner. */
const PRIMARY_LABEL: Record<StepUpPhase, string> = {
  idle: 'Turn Off',
  ceremony: 'Waiting…',
  submitting: 'Turning off...',
};

/**
 * The two answers the credentials cannot render. The server's own message is
 * shown for these: version skew, a rate limit and an outage each carry an
 * actionable sentence the client must not paraphrase.
 */
type ServerAnswer = Extract<PurgeFenceDisableResult, { kind: 'refused' | 'stepUpImpossible' }>;

function isServerAnswer(result: PurgeFenceDisableResult): result is ServerAnswer {
  return result.kind === 'refused' || result.kind === 'stepUpImpossible';
}

/**
 * The store's result in the hook's terms. The four field refusals go to the
 * hook as the refusals they are. The store folds a 429, a 5xx and a transport
 * failure into `refused`, so none of them can be told apart from an answer
 * that spent the code, and `answered` is the safe reading for both arms.
 */
function toOutcome(result: PurgeFenceDisableResult): StepUpSubmitOutcome {
  switch (result.kind) {
    case 'accepted':
      return { kind: 'success' };
    case 'passwordRequired':
    case 'invalidPassword':
    case 'invalidMfaCode':
      return { kind: 'refusal', refusal: { kind: result.kind } };
    case 'mfaRequired':
      return { kind: 'refusal', refusal: { kind: 'mfaRequired', methods: result.methods } };
    case 'stepUpImpossible':
    case 'refused':
      return { kind: 'answered' };
    case 'aborted':
      return { kind: 'aborted' };
  }
}

/**
 * Step-up for the one gated privacy transition: turning
 * `require_auth_before_purge` OFF (#2765). Extracted rather than inlined —
 * PrivacySecuritySection is already at its S3776 cognitive-complexity ceiling,
 * and this dialog owns a small state machine of its own.
 *
 * The password is component-local state and the code lives in the factor hook.
 * Neither is written to a store, persisted, or echoed into error copy
 * (`[internal]rules/observability.md`).
 */
const PurgeFenceStepUpDialog: React.FC<PurgeFenceStepUpDialogProps> = ({ open, onClose }) => {
  const [password, setPassword] = useState('');
  const [answer, setAnswer] = useState<ServerAnswer | null>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);

  const disablePurgeFence = usePrivacyStore((s) => s.disablePurgeFence);

  // passwordLeg set to always: the password field renders whatever the account
  // holds. `mfa_required` without `password_required` does not identify a
  // passwordless account — the server verifies the password factor first, so an
  // MFA-enabled account that supplies a correct password and no code receives
  // exactly that shape. Deriving visibility from it caused a retry loop
  // (CodeRabbit review, #2792): the field vanished, the next submit sent no
  // password, and the accepted password had to be retyped — two step-up
  // attempts burned per cycle, so an actor holding BOTH correct factors could
  // rate-limit themselves out of their own setting.
  //
  // `readFailure: 'passwordOnly'`: a failed read must not lock the user out of
  // a setting the server would accept. The submit is the check that counts, and
  // its refusal mounts the picker.
  const factor = useStepUpFactor({
    enabled: open,
    purpose: 'privacy.purge_fence_disable',
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: true,
  });

  // Neither factor can satisfy an account that holds neither, so that answer
  // offers no retryable field at all.
  const deadEnd = answer?.kind === 'stepUpImpossible';
  const submitting = factor.phase === 'submitting';

  // The parent keeps this mounted behind a boolean, so closing must reset the
  // machine: otherwise the password — and a stale answer — carry into the next
  // open. The hook drops its own state, the code included, when `open` flips.
  useEffect(() => {
    if (open) return;
    // The rule guards against wasted renders. Here the dialog is already closed,
    // so the extra render is of nothing, and dropping the wire secret the
    // moment it closes outranks that.
    /* eslint-disable @eslint-react/set-state-in-effect -- credential hygiene, see above */
    setPassword('');
    setAnswer(null);
    /* eslint-enable @eslint-react/set-state-in-effect -- reset block ends here */
  }, [open]);

  // The dead end unmounts the credentials and the primary. The enclosing
  // <dialog> takes focus, as it does on the picker's own terminal states.
  useLayoutEffect(() => {
    if (deadEnd) cancelRef.current?.closest('dialog')?.focus();
  }, [deadEnd]);

  // Single-shot: whichever factors the user holds travel in the same request,
  // so a rate-limited budget is not spent discovering which. A WebAuthn token
  // rides as `mfa_code`, exactly as a typed code does.
  const submit: StepUpSubmit = async (mfa, context) => {
    const result = await disablePurgeFence(
      { currentPassword: password || undefined, mfaCode: mfa },
      context
    );
    // An answer for an account or server that is no longer current belongs to
    // the old one: the hook ends the attempt, and this dialog must neither show
    // it nor close on it.
    if (!apiRequestContextIsCurrent(context)) return toOutcome(result);
    setAnswer(isServerAnswer(result) ? result : null);
    // Only the password the server rejected is dropped; the hook keeps the code
    // through a password refusal, so a wrong password does not cost a fresh one.
    if (result.kind === 'invalidPassword') setPassword('');
    // The store now holds the new setting and the switch follows it. The close
    // effect drops the password.
    if (result.kind === 'accepted') onClose();
    return toOutcome(result);
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);

  return (
    <Modal
      isOpen={open}
      onClose={onClose}
      title={TITLE}
      width="small"
      dismissable={!submitting}
      initialFocusRef={passwordRef}
    >
      <div className="purge-modal__body">
        <div className="purge-modal__form">
          <p className="purge-modal__stepup-body">{DISABLE_WARNING}</p>

          {answer !== null && (
            <p className="purge-modal__deadend" role="alert">
              {answer.message}
            </p>
          )}

          {!deadEnd && (
            <StepUpCredentials
              factor={factor}
              password={password}
              onPasswordChange={setPassword}
              primaryRef={primaryRef}
              passwordRef={passwordRef}
            />
          )}

          <div className="purge-modal__actions">
            <button
              ref={cancelRef}
              type="button"
              className="purge-modal__cancel"
              disabled={submitting}
              onClick={onClose}
            >
              Cancel
            </button>
            {!deadEnd && (
              <button
                ref={primaryRef}
                type="button"
                className="purge-modal__confirm"
                aria-disabled={ariaDisabled || undefined}
                onClick={activate}
              >
                {factor.phase === 'idle' ? (
                  PRIMARY_LABEL.idle
                ) : (
                  <>
                    <LoadingSpinner size="small" inline /> {PRIMARY_LABEL[factor.phase]}
                  </>
                )}
              </button>
            )}
          </div>
        </div>
      </div>
    </Modal>
  );
};

export default PurgeFenceStepUpDialog;
