import { useEffect, useRef, useState } from 'react';
import './KeyRecoveryPrompt.css';

interface KeyRecoveryPromptProps {
  readonly onReset: (mfaCode?: string) => void;
  readonly onCancel: () => void;
  // When true, the destructive reset needs an MFA code (step-up auth, #1293);
  // the prompt switches to MFA-entry mode.
  readonly mfaRequired?: boolean;
}

/**
 * Shown when the login key-recovery path detects that the user's wrapped
 * private key cannot be unwrapped on this device. Resetting publishes a new
 * keypair and permanently discards encrypted history — a consented,
 * acknowledged data-loss action that replaces the prior silent regeneration
 * (#1293). The reset is step-up-authenticated server-side (current password +
 * MFA when enabled); when MFA is required this prompt re-opens to collect the
 * code.
 *
 * Rendered as a native <dialog> opened with showModal(), so the browser
 * provides the focus trap, initial focus, and Escape-to-cancel handling
 * (WCAG 2.1.2 / 2.4.3) instead of hand-rolled key listeners.
 */
export default function KeyRecoveryPrompt({
  onReset,
  onCancel,
  mfaRequired = false,
}: KeyRecoveryPromptProps) {
  const [acknowledged, setAcknowledged] = useState(false);
  const [mfaCode, setMfaCode] = useState('');
  const dialogRef = useRef<HTMLDialogElement>(null);

  useEffect(() => {
    dialogRef.current?.showModal();
  }, []);

  return (
    <dialog
      ref={dialogRef}
      className="key-recovery-prompt"
      aria-labelledby="key-recovery-title"
      onCancel={(e) => {
        // Native Escape fires 'cancel'; route it to our non-destructive action.
        e.preventDefault();
        onCancel();
      }}
    >
      <div className="key-recovery-prompt__card">
        {mfaRequired ? (
          <>
            <h2 id="key-recovery-title">Confirm with your authenticator</h2>
            <p>
              Enter your multi-factor authentication code to confirm resetting your encryption keys.
            </p>
            <label className="key-recovery-prompt__ack" htmlFor="key-recovery-mfa-code">
              MFA code
            </label>
            <input
              id="key-recovery-mfa-code"
              type="text"
              inputMode="numeric"
              autoComplete="one-time-code"
              value={mfaCode}
              onChange={(e) => setMfaCode(e.target.value)}
            />
            <div className="key-recovery-prompt__actions">
              <button type="button" autoFocus onClick={onCancel}>
                Cancel
              </button>
              <button
                type="button"
                className="key-recovery-prompt__danger"
                disabled={mfaCode.trim() === ''}
                onClick={() => onReset(mfaCode.trim())}
              >
                Verify and reset
              </button>
            </div>
          </>
        ) : (
          <>
            <h2 id="key-recovery-title">Encryption keys couldn&rsquo;t be recovered</h2>
            <p>
              Your encrypted message history can&rsquo;t be unlocked on this device. You can reset
              your encryption keys to keep using Concord Voice, but{' '}
              <strong>all previous encrypted messages will be permanently lost</strong>.
            </p>
            <label className="key-recovery-prompt__ack">
              <input
                type="checkbox"
                checked={acknowledged}
                onChange={(e) => setAcknowledged(e.target.checked)}
              />{' '}
              I understand my encrypted message history will be permanently deleted.
            </label>
            <div className="key-recovery-prompt__actions">
              <button type="button" autoFocus onClick={onCancel}>
                Cancel
              </button>
              <button
                type="button"
                className="key-recovery-prompt__danger"
                disabled={!acknowledged}
                onClick={() => onReset()}
              >
                Reset and continue
              </button>
            </div>
          </>
        )}
      </div>
    </dialog>
  );
}
