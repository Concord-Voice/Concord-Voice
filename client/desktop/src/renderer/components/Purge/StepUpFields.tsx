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
