import React, { useState } from 'react';
import { apiFetch } from '../../services/apiClient';

type Step = 'password' | 'codes' | 'verify' | 'done';

interface EmailSmsSetupProps {
  mfaActive: boolean;
  onComplete: () => void;
  onCancel: () => void;
}

/**
 * DEV-ONLY Email/SMS MFA setup wizard.
 * Generates verification codes and displays them on screen (no real email/SMS delivery).
 * Production gating is enforced server-side — the API returns 403 in production.
 */
const EmailSmsSetup: React.FC<EmailSmsSetupProps> = ({ mfaActive, onComplete, onCancel }) => {
  const [step, setStep] = useState<Step>('password');
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);

  // Codes from server (dev mode — displayed on screen)
  const [devCodes, setDevCodes] = useState<Record<string, string>>({});

  // User-entered verification codes
  const [emailCode, setEmailCode] = useState('');
  const [smsCode, setSmsCode] = useState('');

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
          methods: ['email', 'sms'],
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Setup failed');

      setDevCodes(data.dev_codes || {});
      setStep('codes');
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
      const codes: Record<string, string> = {};
      if (emailCode) codes.email = emailCode;
      if (smsCode) codes.sms = smsCode;

      const res = await apiFetch('/api/v1/mfa/email-sms/verify', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ codes }),
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
      <h3>Set Up Email / SMS MFA</h3>
      <div className="mfa-dev-banner">
        DEV MODE — Codes will be displayed on screen instead of sent via email/SMS.
      </div>

      {step === 'password' && (
        <div className="mfa-setup-step">
          <p>Enter your password to begin setup. Both email and SMS codes will be generated.</p>
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
              {loading ? 'Generating...' : 'Generate Codes'}
            </button>
            <button className="btn btn-secondary" onClick={onCancel}>
              Cancel
            </button>
          </div>
        </div>
      )}

      {step === 'codes' && (
        <div className="mfa-setup-step">
          <p>
            In production, these codes would be sent to your email and phone. For testing, enter the
            codes shown below into the verification fields.
          </p>

          <div className="mfa-dev-codes">
            {devCodes.email && (
              <div className="mfa-dev-code-row">
                <span className="mfa-dev-code-label">Email code:</span>
                <code className="mfa-dev-code-value">{devCodes.email}</code>
              </div>
            )}
            {devCodes.sms && (
              <div className="mfa-dev-code-row">
                <span className="mfa-dev-code-label">SMS code:</span>
                <code className="mfa-dev-code-value">{devCodes.sms}</code>
              </div>
            )}
          </div>

          <div className="mfa-setup-actions">
            <button className="btn btn-primary" onClick={() => setStep('verify')}>
              Enter Codes
            </button>
            <button className="btn btn-secondary" onClick={onCancel}>
              Cancel
            </button>
          </div>
        </div>
      )}

      {step === 'verify' && (
        <div className="mfa-setup-step">
          <p>Enter the verification codes to activate Email/SMS MFA.</p>

          {devCodes.email !== undefined && (
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
                autoFocus
              />
            </div>
          )}
          {devCodes.sms !== undefined && (
            <div className="mfa-verify-field">
              <label htmlFor="mfa-sms-code">SMS code</label>
              <input
                id="mfa-sms-code"
                type="text"
                className="form-input"
                value={smsCode}
                onChange={(e) => setSmsCode(e.target.value)}
                placeholder="6-digit SMS code"
                maxLength={6}
              />
            </div>
          )}

          {error && <p className="mfa-setup-error">{error}</p>}
          <div className="mfa-setup-actions">
            <button
              className="btn btn-primary"
              onClick={handleVerify}
              disabled={loading || (!emailCode && !smsCode)}
            >
              {loading ? 'Verifying...' : 'Verify & Activate'}
            </button>
            <button className="btn btn-secondary" onClick={() => setStep('codes')}>
              Back
            </button>
          </div>
        </div>
      )}

      {step === 'done' && (
        <div className="mfa-setup-step mfa-setup-success">
          <h4>Email / SMS MFA Activated!</h4>
          <p>Both email and SMS verification are now available for your account.</p>
          <button className="btn btn-primary" onClick={onComplete}>
            Done
          </button>
        </div>
      )}
    </div>
  );
};

export default EmailSmsSetup;
