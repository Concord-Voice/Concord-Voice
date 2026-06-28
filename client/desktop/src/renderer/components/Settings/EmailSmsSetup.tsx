import React, { useState } from 'react';
import { apiFetch } from '../../services/apiClient';

type Step = 'password' | 'verify' | 'done';

interface EmailSmsSetupProps {
  mfaActive: boolean;
  onComplete: () => void;
  onCancel: () => void;
}

const EmailSmsSetup: React.FC<EmailSmsSetupProps> = ({ mfaActive, onComplete, onCancel }) => {
  const [step, setStep] = useState<Step>('password');
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  const [emailCode, setEmailCode] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  const handleSetup = async () => {
    setLoading(true);
    setError('');
    try {
      const res = await apiFetch('/api/v1/mfa/email-sms/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          password,
          ...(mfaCode ? { mfa_code: mfaCode } : {}),
          methods: ['email'],
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Setup failed');

      setStep('verify');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Setup failed');
    } finally {
      setLoading(false);
    }
  };

  const handleVerify = async () => {
    setLoading(true);
    setError('');
    try {
      const res = await apiFetch('/api/v1/mfa/email-sms/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ codes: { email: emailCode } }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Verification failed');

      setStep('done');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Verification failed');
    } finally {
      setLoading(false);
    }
  };

  return (
    <div className="mfa-setup-wizard">
      <h3>Set Up Email MFA</h3>

      {step === 'password' && (
        <div className="mfa-setup-step">
          <p>Enter your password to send a verification code to your account email.</p>
          <input
            type="password"
            className={`form-input ${error ? 'error' : ''}`}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            placeholder="Your password"
            disabled={loading}
            autoFocus
          />
          {mfaActive && (
            <input
              type="text"
              className="form-input"
              value={mfaCode}
              onChange={(e) => setMfaCode(e.target.value)}
              placeholder="MFA code from your authenticator"
              disabled={loading}
              autoComplete="one-time-code"
            />
          )}
          {error && <p className="mfa-setup-error">{error}</p>}
          <div className="mfa-setup-actions">
            <button
              className="btn btn-primary"
              onClick={handleSetup}
              disabled={loading || !password || (mfaActive && !mfaCode)}
            >
              {loading ? 'Sending...' : 'Send Code'}
            </button>
            <button className="btn btn-secondary" onClick={onCancel}>
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

          {error && <p className="mfa-setup-error">{error}</p>}
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
