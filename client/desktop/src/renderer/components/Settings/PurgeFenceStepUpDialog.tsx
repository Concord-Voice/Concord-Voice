import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import StepUpFields, { type StepUpFieldErrors } from '../Purge/StepUpFields';
import { usePrivacyStore, type PurgeFenceDisableResult } from '../../stores/privacyStore';
// StepUpFields renders the shipped #1354 markup, so it needs the shipped #1354
// stylesheet — the Settings pane never loads PurgeMessagesModal.
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

/**
 * Per-field step-up error copy (#1354 deck §5, byte-identical to
 * PurgeMessagesModal). Held in a switch rather than a lookup object because the
 * pre-commit secret scanner flags a credential-shaped key placed beside a
 * quoted literal — see StepUpFields' `credentialError`.
 */
function stepUpFieldErrors(refusal: PurgeFenceDisableResult | null): StepUpFieldErrors {
  switch (refusal?.kind) {
    case 'invalidPassword':
      return { credentialError: 'That password is not correct.' };
    case 'invalidMfaCode':
      return { codeError: 'That code is not correct, or it has expired. Try the next one.' };
    default:
      return {};
  }
}

/**
 * Whether a refusal belongs above the fields rather than on one of them. The
 * server's own message is rendered for these — a missing factor, a rate limit,
 * and version skew each carry an actionable sentence the client must not
 * paraphrase.
 */
function bannerMessage(refusal: PurgeFenceDisableResult | null): string | null {
  if (refusal === null || refusal.kind === 'accepted') return null;
  if (refusal.kind === 'invalidPassword' || refusal.kind === 'invalidMfaCode') return null;
  return refusal.message;
}

/**
 * Step-up for the one gated privacy transition: turning
 * `require_auth_before_purge` OFF (#2765). Extracted rather than inlined —
 * PrivacySecuritySection is already at its S3776 cognitive-complexity ceiling,
 * and this dialog owns a small state machine of its own.
 *
 * Both wire secrets are component-local state. They are never written to a
 * store, never persisted, and never echoed into error copy
 * (`[internal]rules/observability.md`).
 */
const PurgeFenceStepUpDialog: React.FC<PurgeFenceStepUpDialogProps> = ({ open, onClose }) => {
  const [password, setPassword] = useState('');
  const [code, setCode] = useState('');
  const [busy, setBusy] = useState(false);
  const [refusal, setRefusal] = useState<PurgeFenceDisableResult | null>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const codeRef = useRef<HTMLInputElement>(null);

  const disablePurgeFence = usePrivacyStore((s) => s.disablePurgeFence);

  // The password field ALWAYS renders. `mfa_required` without
  // `password_required` does not identify a passwordless account — the server
  // verifies the password factor first, so an MFA-enabled account that supplies
  // a correct password and no code receives exactly that shape.
  //
  // Deriving visibility from it caused a retry loop (CodeRabbit review, #2792):
  // the field vanished, the next submit therefore sent no password, the server
  // answered `password_required`, and the accepted password had to be retyped —
  // two step-up attempts burned per cycle, so an actor holding BOTH correct
  // factors could rate-limit themselves out of their own setting.
  //
  // An account with no password simply leaves this empty; the server already
  // accepts MFA alone. Let the server's `password_required` and invalid-factor
  // arms drive the copy instead of inferring account shape on the client.
  const showPassword = true;
  // Neither factor can satisfy an account that holds neither, so that refusal
  // offers no retryable field at all.
  const deadEnd = refusal?.kind === 'stepUpImpossible';
  const canSubmit = !busy && (password !== '' || code !== '');
  const banner = bannerMessage(refusal);

  // The parent keeps this mounted behind a boolean, so closing must reset the
  // machine: otherwise the credentials — and a stale refusal — carry into the
  // next open.
  useEffect(() => {
    if (open) return;
    // The rule guards against wasted renders. Here the dialog is already closed,
    // so the extra render is of nothing, and dropping the wire secrets the
    // moment it closes outranks that.
    /* eslint-disable @eslint-react/set-state-in-effect -- credential hygiene, see above */
    setPassword('');
    setCode('');
    setRefusal(null);
    /* eslint-enable @eslint-react/set-state-in-effect -- reset block ends here */
  }, [open]);

  // A rejected factor returns focus to the field that owns it. Keyed on the
  // refusal object, so a second wrong attempt of the same kind re-focuses too.
  useLayoutEffect(() => {
    if (refusal?.kind === 'invalidPassword') passwordRef.current?.focus();
    else if (refusal?.kind === 'invalidMfaCode') codeRef.current?.focus();
  }, [refusal]);

  const handleSubmit = async () => {
    if (!canSubmit) return;
    setBusy(true);
    try {
      // Single-shot: whichever factors the user holds travel in the same
      // request, so a rate-limited budget is not spent discovering which.
      const result = await disablePurgeFence({
        currentPassword: password || undefined,
        mfaCode: code || undefined,
      });
      if (result.kind === 'accepted') {
        // The store now holds the new setting and the switch follows it. The
        // close effect drops the credentials.
        onClose();
        return;
      }
      // Drop only the factor the server rejected, so a wrong password does not
      // cost the user a fresh code they already typed.
      if (result.kind === 'invalidPassword') setPassword('');
      if (result.kind === 'invalidMfaCode') setCode('');
      setRefusal(result);
    } finally {
      // Without this the dialog is unclosable: `busy` disables the fieldset that
      // owns Cancel, and `dismissable={!busy}` gates Escape and the backdrop.
      setBusy(false);
    }
  };

  return (
    <Modal isOpen={open} onClose={onClose} title={TITLE} width="small" dismissable={!busy}>
      <div className="purge-modal__body">
        <fieldset disabled={busy} className="purge-modal__form">
          <p className="purge-modal__stepup-body">{DISABLE_WARNING}</p>

          {banner !== null && (
            <p className="purge-modal__deadend" role="alert">
              {banner}
            </p>
          )}

          {!deadEnd && (
            <StepUpFields
              showPassword={showPassword}
              password={password}
              onPasswordChange={setPassword}
              code={code}
              onCodeChange={setCode}
              errors={stepUpFieldErrors(refusal)}
              passwordRef={passwordRef}
              codeRef={codeRef}
            />
          )}

          <div className="purge-modal__actions">
            <button type="button" className="purge-modal__cancel" onClick={onClose}>
              Cancel
            </button>
            {!deadEnd && (
              <button
                type="button"
                className="purge-modal__confirm"
                disabled={!canSubmit}
                onClick={() => void handleSubmit()}
              >
                {busy ? (
                  <>
                    <LoadingSpinner size="small" inline /> Turning off...
                  </>
                ) : (
                  'Turn Off'
                )}
              </button>
            )}
          </div>
        </fieldset>
      </div>
    </Modal>
  );
};

export default PurgeFenceStepUpDialog;
