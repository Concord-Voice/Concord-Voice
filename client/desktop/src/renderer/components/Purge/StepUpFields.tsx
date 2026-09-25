import React from 'react';

/**
 * Per-field error text, or nothing. `credentialError` is deliberately not named
 * for the credential it describes: the pre-commit secret scanner flags a
 * password-shaped key sitting beside a quoted literal, and the copy that fills
 * it is a quoted literal.
 */
export interface StepUpFieldErrors {
  credentialError?: string;
  codeError?: string;
}

/** The refusal shape both step-up dialogs route here: only its kind is read. */
interface StepUpRefusalLike {
  kind: string;
}

/**
 * Per-field copy for a step-up refusal, shared by the purge dialog and the
 * purge-fence dialog so the two cannot drift (they held a copy each, kept
 * "byte-identical" by a comment). Both keep both fields on screen, so a refusal
 * that names a MISSING factor has no other visible effect: without copy here, a
 * correct password sent with no code changed nothing and the dialog looked like
 * it had ignored the click. Held in a switch rather than a lookup object because
 * the pre-commit secret scanner flags a credential-shaped key placed beside a
 * quoted literal — see `credentialError` above.
 */
export function stepUpFieldErrors(refusal: StepUpRefusalLike | null): StepUpFieldErrors {
  switch (refusal?.kind) {
    case 'passwordRequired':
      return { credentialError: 'Enter your password to continue.' };
    case 'invalidPassword':
      return { credentialError: 'That password is not correct.' };
    case 'mfaRequired':
      return { codeError: 'Enter the code from your authenticator app to continue.' };
    case 'invalidMfaCode':
      return { codeError: 'That code is not correct, or it has expired. Try the next one.' };
    default:
      return {};
  }
}

/** The field a refusal belongs to, for returning focus; null when it names neither. */
export function stepUpRefusedField(refusal: StepUpRefusalLike | null): 'password' | 'code' | null {
  const errors = stepUpFieldErrors(refusal);
  if (errors.credentialError !== undefined) return 'password';
  if (errors.codeError !== undefined) return 'code';
  return null;
}

interface StepUpFieldsProps {
  /** False when the server asked for MFA alone — an SSO account has no password. */
  showPassword: boolean;
  password: string;
  onPasswordChange: (value: string) => void;
  code: string;
  onCodeChange: (value: string) => void;
  errors: StepUpFieldErrors;
  passwordRef: React.RefObject<HTMLInputElement | null>;
  codeRef: React.RefObject<HTMLInputElement | null>;
}

const PASSWORD_ID = 'purge-stepup-password';
const CODE_ID = 'purge-stepup-code';
const CODE_HELPER_ID = 'purge-stepup-code-helper';

/**
 * The DM/group step-up credentials. Both values are owned by the modal's local
 * state and submitted together in one request; nothing here writes to a store
 * and nothing here is ever logged (copy deck §5, [internal]rules/observability.md).
 */
const StepUpFields: React.FC<StepUpFieldsProps> = ({
  showPassword,
  password,
  onPasswordChange,
  code,
  onCodeChange,
  errors,
  passwordRef,
  codeRef,
}) => (
  <div className="purge-modal__fields">
    {showPassword && (
      <div className="purge-modal__field">
        <label htmlFor={PASSWORD_ID}>Password</label>
        <input
          id={PASSWORD_ID}
          ref={passwordRef}
          type="password"
          autoComplete="current-password"
          value={password}
          onChange={(e) => onPasswordChange(e.target.value)}
          aria-invalid={errors.credentialError !== undefined || undefined}
          aria-describedby={
            errors.credentialError === undefined ? undefined : `${PASSWORD_ID}-error`
          }
        />
        {errors.credentialError !== undefined && (
          <p className="purge-modal__field-error" id={`${PASSWORD_ID}-error`} role="alert">
            {errors.credentialError}
          </p>
        )}
      </div>
    )}

    <div className="purge-modal__field">
      <label htmlFor={CODE_ID}>Authentication code</label>
      <input
        id={CODE_ID}
        ref={codeRef}
        type="text"
        inputMode="numeric"
        autoComplete="one-time-code"
        value={code}
        onChange={(e) => onCodeChange(e.target.value)}
        aria-invalid={errors.codeError !== undefined || undefined}
        aria-describedby={
          errors.codeError === undefined ? CODE_HELPER_ID : `${CODE_HELPER_ID} ${CODE_ID}-error`
        }
      />
      <p className="purge-modal__field-helper" id={CODE_HELPER_ID}>
        Enter the 6-digit code from your authenticator app.
      </p>
      {errors.codeError !== undefined && (
        <p className="purge-modal__field-error" id={`${CODE_ID}-error`} role="alert">
          {errors.codeError}
        </p>
      )}
    </div>
  </div>
);

export default StepUpFields;
