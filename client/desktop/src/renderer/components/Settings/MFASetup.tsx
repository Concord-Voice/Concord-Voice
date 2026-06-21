import React, { useState, useEffect, useRef } from 'react';
import QRCode from 'qrcode';
import { apiFetch } from '../../services/apiClient';
import { errorMessage } from '../../utils/redactError';
import TOTPInput from '../Auth/TOTPInput';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import BackupCodeDisplay from './BackupCodeDisplay';
import RecoveryKeyDisplay from './RecoveryKeyDisplay';
import {
  generateRecoveryKey,
  wrapWithRecoveryKey,
  wrapPrefsKeyWithRecoveryKey,
} from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';

// ── Extracted helpers (reduce cognitive complexity) ───────────────────

/** Generate a recovery key, wrap E2EE keys with it, and upload to server. */
async function generateAndStoreRecoveryKey(): Promise<string | null> {
  const wrappingKey = e2eeService.getWrappingKey();
  const wrappedPrivateKey = e2eeService.getWrappedPrivateKey();

  if (!wrappingKey || !wrappedPrivateKey) {
    return null; // No wrapping key available, skip recovery
  }

  const newRecoveryKey = generateRecoveryKey();
  const { wrappedKey: recoveryWrappedKey, salt: recoverySalt } = await wrapWithRecoveryKey(
    wrappedPrivateKey,
    wrappingKey,
    newRecoveryKey
  );

  const uploadBody: Record<string, string> = {
    recovery_wrapped_private_key: recoveryWrappedKey,
    recovery_key_salt: recoverySalt,
  };

  const prefsKeyBase64 = e2eeService.getPreferencesKeyBase64();
  if (prefsKeyBase64) {
    const { wrappedKey: recoveryWrappedPrefs, salt: prefsSalt } = await wrapPrefsKeyWithRecoveryKey(
      prefsKeyBase64,
      newRecoveryKey
    );
    uploadBody.recovery_wrapped_prefs_key = recoveryWrappedPrefs;
    uploadBody.recovery_prefs_key_salt = prefsSalt;
  }

  const storeRes = await apiFetch('/api/v1/mfa/recovery-key', {
    method: 'PUT',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify(uploadBody),
  });

  if (!storeRes.ok) {
    return null; // Server rejected, caller decides fallback
  }

  return newRecoveryKey;
}

/** Classify a WebAuthn error into a user-friendly message. */
function classifyWebAuthnError(err: unknown): string {
  if (err instanceof DOMException && err.name === 'NotAllowedError') {
    return 'Registration cancelled or timed out. Try again.';
  }
  return err instanceof Error ? err.message : 'Registration failed';
}

/** Classify an error message into the appropriate form field. */
function classifyErrorField(message: string): 'password' | 'mfa' | 'general' {
  const lower = message.toLowerCase();
  if (lower.includes('password')) return 'password';
  if (lower.includes('mfa') || lower.includes('code')) return 'mfa';
  return 'general';
}

/** Convert base64url-encoded fields in WebAuthn options to ArrayBuffers. */
function decodeWebAuthnOptions(
  options: PublicKeyCredentialCreationOptions & Record<string, unknown>
): void {
  options.challenge = base64urlToBuffer(options.challenge as unknown as string);
  const user = options.user as unknown as Record<string, unknown>;
  user.id = base64urlToBuffer(user.id as string);
  if (options.excludeCredentials) {
    for (const cred of options.excludeCredentials as Array<{ id: unknown }>) {
      cred.id = base64urlToBuffer(cred.id as string);
    }
  }
}

/** Determine whether a WebAuthn error should return the user to the password step. */
function shouldResetToPasswordStep(err: unknown, msg: string, currentStep: WebAuthnStep): boolean {
  const isNotAllowed = err instanceof DOMException && err.name === 'NotAllowedError';
  const isBeginStepError =
    currentStep === 'password' ||
    msg.toLowerCase().includes('password') ||
    msg.toLowerCase().includes('mfa');
  return isNotAllowed || isBeginStepError;
}

type SetupMethod = 'totp' | 'webauthn';
type TOTPStep = 'password' | 'qr' | 'verify' | 'backup' | 'recovery' | 'done';
type WebAuthnStep = 'password' | 'registering' | 'done';

interface MFASetupProps {
  method: SetupMethod;
  credentialType?: 'hardware' | 'platform';
  mfaActive?: boolean; // true if user already has MFA enabled
  activeMethods?: string[]; // raw method strings for MFA challenge
  recoveryOnlyMethods?: string[];
  onComplete: () => void;
  onCancel: () => void;
}

const MFASetup: React.FC<MFASetupProps> = ({
  method,
  credentialType = 'hardware',
  mfaActive,
  activeMethods = [],
  recoveryOnlyMethods = [],
  onComplete,
  onCancel,
}) => {
  // Shared state
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  const [error, setError] = useState('');
  const [errorField, setErrorField] = useState<'password' | 'mfa' | 'general' | ''>('');
  const [loading, setLoading] = useState(false);

  // TOTP state
  const [totpStep, setTotpStep] = useState<TOTPStep>('password');
  const [otpauthUrl, setOtpauthUrl] = useState('');
  const [totpSecret, setTotpSecret] = useState('');
  const [backupCodes, setBackupCodes] = useState<string[]>([]);
  const [recoveryKey, setRecoveryKey] = useState('');
  const [recoveryLoading, setRecoveryLoading] = useState(false);

  // WebAuthn state
  const [webauthnStep, setWebauthnStep] = useState<WebAuthnStep>('password');
  const [credentialName, setCredentialName] = useState('');

  // ── TOTP Flow ──────────────────────────────────────────────────────

  const setFieldError = (message: string) => {
    setError(message);
    setErrorField(classifyErrorField(message));
  };

  const handleTOTPSetup = async () => {
    setLoading(true);
    setError('');
    setErrorField('');
    try {
      const res = await apiFetch('/api/v1/mfa/totp/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password, ...(mfaCode ? { mfa_code: mfaCode } : {}) }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Setup failed');

      setOtpauthUrl(data.otpauth_url);
      setTotpSecret(data.secret);
      setTotpStep('qr');
    } catch (err) {
      setFieldError(err instanceof Error ? err.message : 'Setup failed');
    } finally {
      setLoading(false);
    }
  };

  const handleTOTPVerify = async (code: string) => {
    setLoading(true);
    setError('');
    try {
      const res = await apiFetch('/api/v1/mfa/totp/verify-setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Verification failed');

      setBackupCodes(data.backup_codes || []);
      setTotpStep('backup');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid code');
    } finally {
      setLoading(false);
    }
  };

  const confirmTOTPSetup = async () => {
    const res = await apiFetch('/api/v1/mfa/totp/confirm-setup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
    if (!res.ok) {
      const data = await res.json();
      throw new Error(data.error || 'Confirmation failed');
    }
  };

  const handleTOTPConfirm = async () => {
    setLoading(true);
    setError('');
    try {
      await confirmTOTPSetup();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Confirmation failed');
      setLoading(false);
      return;
    }

    // Generate and store recovery key
    setRecoveryLoading(true);
    try {
      const newRecoveryKey = await generateAndStoreRecoveryKey();
      if (newRecoveryKey) {
        setRecoveryKey(newRecoveryKey);
        setTotpStep('recovery');
      } else {
        setTotpStep('done');
      }
    } catch (recoveryErr) {
      console.warn('Recovery key generation failed, skipping:', errorMessage(recoveryErr));
      setTotpStep('done');
    } finally {
      setRecoveryLoading(false);
      setLoading(false);
    }
  };

  // ── WebAuthn Flow ──────────────────────────────────────────────────

  const handleWebAuthnRegister = async () => {
    setLoading(true);
    setError('');
    setErrorField('');
    try {
      // Begin registration — validate password and get challenge from server
      const beginRes = await apiFetch('/api/v1/mfa/webauthn/register/begin', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          password,
          ...(mfaCode ? { mfa_code: mfaCode } : {}),
          credential_name: credentialName || 'Security Key',
          credential_type: credentialType,
        }),
      });
      const beginData = await beginRes.json();
      if (!beginRes.ok) throw new Error(beginData.error || 'Registration failed');

      // Convert base64url fields for WebAuthn API
      const options = beginData.publicKey;
      decodeWebAuthnOptions(options);

      // Show the "waiting for key" step before triggering the browser dialog
      setWebauthnStep('registering');

      // Call browser WebAuthn API with timeout protection
      const credential = (await Promise.race([
        navigator.credentials.create({ publicKey: options }),
        new Promise<never>((_, reject) =>
          setTimeout(
            () =>
              reject(
                new Error(
                  'Security key registration timed out. Make sure your key is connected and try again.'
                )
              ),
            60000
          )
        ),
      ])) as PublicKeyCredential;
      if (!credential) throw new Error('No credential returned');

      const attestation = credential.response as AuthenticatorAttestationResponse;

      // Finish registration by sending the attestation to the server
      const finishRes = await apiFetch('/api/v1/mfa/webauthn/register/finish', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          id: credential.id,
          rawId: bufferToBase64url(credential.rawId),
          type: credential.type,
          response: {
            attestationObject: bufferToBase64url(attestation.attestationObject),
            clientDataJSON: bufferToBase64url(attestation.clientDataJSON),
          },
          credential_name: credentialName || 'Security Key',
        }),
      });
      if (!finishRes.ok) {
        const finishData = await finishRes.json();
        throw new Error(finishData.error || 'Registration failed');
      }
      setWebauthnStep('done');
    } catch (err) {
      const msg = classifyWebAuthnError(err);
      setFieldError(msg);

      // Go back to password step for cancellation or credential errors
      if (shouldResetToPasswordStep(err, msg, webauthnStep)) {
        setWebauthnStep('password');
      }
      // Otherwise stay on 'registering' step so the error banner is clearly visible
    } finally {
      setLoading(false);
    }
  };

  // ── Render helpers (reduce cognitive complexity) ────────────────────

  const renderTOTPPasswordStep = () => (
    <div className="mfa-setup-step">
      <p>
        {mfaActive
          ? 'Verify your identity to add another method.'
          : 'Enter your password to begin setup.'}
      </p>
      <input
        type="password"
        className={`form-input ${errorField === 'password' ? 'error' : ''}`}
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="Your password"
        disabled={loading}
        autoFocus
      />
      {mfaActive && (
        <MFAVerifyPrompt
          methods={activeMethods}
          recoveryOnlyMethods={recoveryOnlyMethods}
          onVerify={(code) => setMfaCode(code)}
          disabled={loading}
          error={errorField === 'mfa' ? error : undefined}
          excludeBackupCodes
        />
      )}
      <ErrorBanner error={error} errorField={errorField} />
      <div className="mfa-setup-actions">
        <button
          className="btn btn-primary"
          onClick={handleTOTPSetup}
          disabled={loading || !password || (mfaActive && !mfaCode)}
        >
          {loading ? 'Setting up...' : 'Continue'}
        </button>
        <button className="btn btn-secondary" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );

  const renderWebAuthnPasswordStep = () => (
    <div className="mfa-setup-step">
      <p>
        {mfaActive
          ? 'Verify your identity and name your key.'
          : 'Enter your password and name your key.'}
      </p>
      <input
        type="password"
        className={`form-input ${errorField === 'password' ? 'error' : ''}`}
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="Your password"
        disabled={loading}
        autoFocus
      />
      {mfaActive && (
        <MFAVerifyPrompt
          methods={activeMethods}
          recoveryOnlyMethods={recoveryOnlyMethods}
          onVerify={(code) => setMfaCode(code)}
          disabled={loading}
          error={errorField === 'mfa' ? error : undefined}
          excludeBackupCodes
        />
      )}
      <input
        type="text"
        className="form-input"
        value={credentialName}
        onChange={(e) => setCredentialName(e.target.value)}
        placeholder={
          credentialType === 'platform'
            ? 'Key name (e.g. MacBook Touch ID, Windows Hello)'
            : 'Key name (e.g. YubiKey 5, Google Titan)'
        }
        disabled={loading}
      />
      <ErrorBanner error={error} errorField={errorField} />
      <div className="mfa-setup-actions">
        <button
          className="btn btn-primary"
          onClick={handleWebAuthnRegister}
          disabled={loading || !password || (mfaActive && !mfaCode)}
        >
          {loading ? 'Registering...' : 'Register Key'}
        </button>
        <button className="btn btn-secondary" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );

  const handleResetToPassword = () => {
    setWebauthnStep('password');
    setError('');
  };

  const renderWebAuthnRegisteringStep = () => (
    <div className="mfa-setup-step" style={{ alignItems: 'center' }}>
      <div style={{ textAlign: 'center', padding: '20px 0' }}>
        {error ? <ErrorBanner error={error} errorField="" size={20} /> : <WebAuthnWaitingPrompt />}
      </div>
      <div className="mfa-setup-actions" style={{ justifyContent: 'center' }}>
        {error ? (
          <button className="btn btn-primary" onClick={handleResetToPassword}>
            Try Again
          </button>
        ) : (
          <button className="btn btn-secondary" onClick={handleResetToPassword}>
            Cancel
          </button>
        )}
      </div>
    </div>
  );

  // ── Render ─────────────────────────────────────────────────────────

  if (method === 'totp') {
    return (
      <div className="mfa-setup-wizard">
        <h3>Set Up Authenticator App</h3>

        {totpStep === 'password' && renderTOTPPasswordStep()}

        {totpStep === 'qr' && (
          <div className="mfa-setup-step">
            <p>Scan this QR code with your authenticator app, then enter the 6-digit code below.</p>
            <QRCodeCanvas data={otpauthUrl} />
            <details className="mfa-manual-entry">
              <summary>Can&apos;t scan? Enter manually</summary>
              <code className="mfa-secret-display">{totpSecret}</code>
            </details>
            <TOTPInput onSubmit={handleTOTPVerify} disabled={loading} error={error} />
            <div className="mfa-setup-actions">
              <button className="btn btn-secondary" onClick={onCancel}>
                Cancel
              </button>
            </div>
          </div>
        )}

        {totpStep === 'backup' && (
          <div className="mfa-setup-step">
            <p>
              Save these backup codes. They&apos;re your safety net if you lose access to your
              authenticator.
            </p>
            <BackupCodeDisplay
              codes={backupCodes}
              onConfirm={handleTOTPConfirm}
              disabled={loading}
            />
            {error && <p className="mfa-setup-error">{error}</p>}
          </div>
        )}

        {totpStep === 'recovery' && (
          <div className="mfa-setup-step">
            <p>
              Save your recovery key. This is the <strong>only way</strong> to recover your
              encrypted messages if you lose your password.
            </p>
            <RecoveryKeyDisplay
              recoveryKey={recoveryKey}
              onConfirm={() => setTotpStep('done')}
              onSkip={() => setTotpStep('done')}
              disabled={recoveryLoading}
            />
            {error && <p className="mfa-setup-error">{error}</p>}
          </div>
        )}

        {totpStep === 'done' && (
          <div className="mfa-setup-step mfa-setup-success">
            <h4>MFA Activated!</h4>
            <p>Your authenticator app is now protecting your account.</p>
            <button className="btn btn-primary" onClick={onComplete}>
              Done
            </button>
          </div>
        )}
      </div>
    );
  }

  // WebAuthn flow
  return (
    <div className="mfa-setup-wizard">
      <h3>
        {credentialType === 'platform' ? 'Set Up Platform Authenticator' : 'Set Up Security Key'}
      </h3>

      {webauthnStep === 'password' && renderWebAuthnPasswordStep()}

      {webauthnStep === 'registering' && renderWebAuthnRegisteringStep()}

      {webauthnStep === 'done' && (
        <div className="mfa-setup-step mfa-setup-success">
          <h4>Security Key Registered!</h4>
          <p>Your security key is now active and protecting your account.</p>
          <button className="btn btn-primary" onClick={onComplete}>
            Done
          </button>
        </div>
      )}
    </div>
  );
};

// ── Extracted sub-components (reduce cognitive complexity) ──────────

/** Inline error banner shown when a non-MFA error is present. */
const ErrorBanner: React.FC<{
  error: string;
  errorField: string;
  size?: number;
}> = ({ error, errorField, size = 16 }) => {
  if (!error || errorField === 'mfa') return null;
  return (
    <div className="mfa-setup-error-banner">
      <svg
        width={size}
        height={size}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        style={{ flexShrink: 0 }}
      >
        <circle cx="12" cy="12" r="10" />
        <line x1="12" y1="8" x2="12" y2="12" />
        <line x1="12" y1="16" x2="12.01" y2="16" />
      </svg>
      <span>{error}</span>
    </div>
  );
};

/** Waiting prompt shown during WebAuthn key registration. */
const WebAuthnWaitingPrompt: React.FC = () => (
  <>
    <svg
      width="48"
      height="48"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      style={{ margin: '0 auto 16px', display: 'block' }}
    >
      <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
    </svg>
    <p>
      <strong>Waiting for your security key...</strong>
    </p>
    <p className="mfa-modal-desc">
      Touch your security key, use your fingerprint reader, or follow your browser&apos;s prompt.
    </p>
  </>
);

// ── QR Code component (renders locally via canvas → data URL) ──────────

const QRCodeCanvas: React.FC<{ data: string }> = ({ data }) => {
  const [dataUrl, setDataUrl] = useState('');
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    QRCode.toDataURL(data, { width: 200, margin: 2 }).then((url) => {
      if (mountedRef.current) setDataUrl(url);
    });
    return () => {
      mountedRef.current = false;
    };
  }, [data]);

  return (
    <div className="mfa-qr-container">
      {dataUrl ? (
        <img src={dataUrl} alt="TOTP QR Code" width={200} height={200} className="mfa-qr-image" />
      ) : (
        <div
          style={{
            width: 200,
            height: 200,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          Generating...
        </div>
      )}
    </div>
  );
};

// ── base64url helpers ──────────────────────────────────────────────────

function base64urlToBuffer(base64url: string): ArrayBuffer {
  const base64 = base64url.replaceAll('-', '+').replaceAll('_', '/');
  const pad = base64.length % 4 === 0 ? '' : '='.repeat(4 - (base64.length % 4));
  const binary = atob(base64 + pad);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) {
    bytes[i] = binary.codePointAt(i) ?? 0;
  }
  return bytes.buffer;
}

function bufferToBase64url(buffer: ArrayBuffer): string {
  const bytes = new Uint8Array(buffer);
  let binary = '';
  for (const byte of bytes) {
    binary += String.fromCodePoint(byte);
  }
  return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replaceAll(/=+$/g, '');
}

export default MFASetup;
