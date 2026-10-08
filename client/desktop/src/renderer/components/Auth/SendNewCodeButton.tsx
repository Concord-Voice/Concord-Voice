import React from 'react';
import type { SignInEmailCode } from './signInEmailCode';

interface SendNewCodeButtonProps {
  emailCode: SignInEmailCode;
  disabled: boolean;
}

/**
 * The email panel's send error, on its own line below the code input, and the
 * way to ask again after a send failed in a way asking again can fix. Renders
 * nothing while there is no send error, so a sent code offers no second
 * request, and neither does a refused one (a 429 without Retry-After means a
 * code is already on its way).
 */
const SendNewCodeButton: React.FC<SendNewCodeButtonProps> = ({ emailCode, disabled }) => {
  if (!emailCode.sendError) return null;
  return (
    <>
      <p className="totp-error mfa-send-error" role="alert">
        {emailCode.sendError}
      </p>
      {emailCode.failed && (
        <button
          type="button"
          className="mfa-choose-another"
          onClick={(e) => {
            // The button unmounts once the new send is out, and focus would
            // fall to <body>. Move it to the code while the button can still
            // find it.
            const input = e.currentTarget.parentElement?.querySelector<HTMLInputElement>(
              'input.totp-digit:not([disabled])'
            );
            emailCode.retry();
            input?.focus();
          }}
          disabled={disabled}
        >
          Send a new code
        </button>
      )}
    </>
  );
};

export default SendNewCodeButton;
