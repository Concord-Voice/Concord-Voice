import React, { useEffect, useRef, useState } from 'react';
import { apiFetch, refreshAccessToken } from '../../services/system/apiClient';
import { apiRequestContextIsCurrent } from '../../services/system/requestContext';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  useStepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
} from '../../hooks/auth/useStepUpFactor';
import ErrorBanner from './ErrorBanner';
import { stepUpBanner, submitMfaStepUp, toStepUpSubmitOutcome } from './mfaStepUp';
import { refusalText } from './mfaResponse';

type Step = 'password' | 'verify' | 'done';

interface EmailSmsSetupProps {
  onComplete: () => void;
  onCancel: () => void;
}

const PRIMARY_LABEL: Record<StepUpPhase, string> = {
  idle: 'Send Code',
  ceremony: 'Waiting…',
  submitting: 'Sending...',
};

const EmailSmsSetup: React.FC<EmailSmsSetupProps> = ({ onComplete, onCancel }) => {
  const [step, setStep] = useState<Step>('password');
  const [password, setPassword] = useState('');
  const [emailCode, setEmailCode] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const passwordRef = useRef<HTMLInputElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);

  // The factor picker serves the credential step only; the emailed code that
  // follows is this wizard's own. The offered set never contains email or SMS
  // (G1), and the request below names email alone: SMS is not offered (D14).
  const factor = useStepUpFactor({
    enabled: step === 'password',
    purpose: 'mfa_settings.email_sms_setup',
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: true,
  });

  useEffect(() => {
    if (step === 'password') passwordRef.current?.focus();
  }, [step]);

  const submit: StepUpSubmit = async (mfa, context) => {
    const result = await submitMfaStepUp(
      '/api/v1/mfa/email-sms/setup',
      'POST',
      { methods: ['email'] },
      { password, mfaCode: mfa },
      { context }
    );
    // An answer for an account or server that is no longer current belongs to
    // the old one: the hook ends the attempt, and this wizard must neither show
    // it nor advance on it.
    if (!apiRequestContextIsCurrent(context)) return toStepUpSubmitOutcome(result);
    setError(stepUpBanner(result) ?? '');
    // The password the server rejected is dropped, and so is the one it accepted:
    // a sent credential is spent, and Back must not return to the field filled.
    // The hook keeps the code through a password refusal, so a wrong password
    // does not cost a fresh one.
    if (result.kind === 'invalidPassword' || result.kind === 'accepted') setPassword('');
    if (result.kind === 'accepted') setStep('verify');
    return toStepUpSubmitOutcome(result);
  };

  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);

  const handleVerify = async () => {
    setLoading(true);
    setError('');
    try {
      const res = await apiFetch('/api/v1/mfa/email-sms/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ codes: { email: emailCode } }),
      });
      if (!res.ok) throw new Error(await refusalText(res, 'Verification failed'));

      // Uses the enrollment exemption, as after TOTP confirm in MFASetup.
      void refreshAccessToken().catch(() => console.warn('[mfa] Refresh after enrollment failed'));
      setStep('done');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Verification failed');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="mfa-setup-wizard">
      <h3 tabIndex={-1} ref={headingRef}>
        Set Up Email MFA
      </h3>

      {step === 'password' && (
        <div className="mfa-setup-step">
          <p>Confirm it&apos;s you to send a verification code to your account email.</p>
          <StepUpCredentials
            factor={factor}
            password={password}
            onPasswordChange={setPassword}
            primaryRef={primaryRef}
            passwordRef={passwordRef}
            headingRef={headingRef}
          />
          <ErrorBanner error={error} />
          <div className="mfa-setup-actions">
            <button
              ref={primaryRef}
              type="button"
              className="btn btn-primary"
              aria-disabled={ariaDisabled || undefined}
              onClick={activate}
            >
              {PRIMARY_LABEL[factor.phase]}
            </button>
            <button
              type="button"
              className="btn btn-secondary"
              disabled={factor.phase === 'submitting'}
              onClick={onCancel}
            >
              Cancel
            </button>
          </div>
        </div>
      )}

      {step === 'verify' && (
        <div className="mfa-setup-step">
          <p>Enter the verification code sent to your email to activate Email MFA.</p>

          <div className="mfa-verify-field">
            <label htmlFor="mfa-email-code">Email code</label>
            <input
              id="mfa-email-code"
              type="text"
              className="form-input"
              value={emailCode}
              onChange={(e) => setEmailCode(e.target.value)}
              placeholder="6-digit email code"
              maxLength={6}
              autoComplete="one-time-code"
              autoFocus
            />
          </div>

          <ErrorBanner error={error} />
          <div className="mfa-setup-actions">
            <button
              className="btn btn-primary"
              onClick={handleVerify}
              disabled={loading || !emailCode}
            >
              {loading ? 'Verifying...' : 'Verify & Activate'}
            </button>
            <button className="btn btn-secondary" onClick={() => setStep('password')}>
              Back
            </button>
          </div>
        </div>
      )}

      {step === 'done' && (
        <div className="mfa-setup-step mfa-setup-success">
          <h4>Email MFA Activated!</h4>
          <p>Email verification is now available for your account.</p>
          <button className="btn btn-primary" onClick={onComplete}>
            Done
          </button>
        </div>
      )}
    </div>
  );
};

export default EmailSmsSetup;
