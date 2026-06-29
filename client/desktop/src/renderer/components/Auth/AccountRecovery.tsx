import React, { useState, useEffect } from 'react';
import {
  unwrapWithRecoveryKey,
  generateRegistrationKeys,
  arrayBufferToBase64,
  generateSalt,
  deriveKeyArgon2id,
  generateECDHKeyPair,
  exportECDHPublicKey,
  importECDHPublicKey,
  deriveSharedSecret,
  decryptWithSharedSecret,
} from '../../utils/crypto';
import { apiUrl } from '../../services/runtimeServerBase';
import { assertValidUUID, isValidUUID } from '../../utils/uuid';
import LoadingSpinner from './LoadingSpinner';
import './Login.css';

type RecoveryStep =
  | 'email'
  | 'verify'
  | 'recovery-key'
  | 'device-waiting'
  | 'social-waiting'
  | 'reset-warning'
  | 'new-password';

interface AccountRecoveryProps {
  onBack: () => void;
  onComplete: () => void; // Navigate back to login on success
}

const AccountRecovery: React.FC<AccountRecoveryProps> = ({ onBack, onComplete }) => {
  const [step, setStep] = useState<RecoveryStep>('email');
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [recoveryToken, setRecoveryToken] = useState('');
  const [hasRecoveryKey, setHasRecoveryKey] = useState(false);
  const [hasTrustedDevices, setHasTrustedDevices] = useState(false);
  const [hasRecoveryCircle, setHasRecoveryCircle] = useState(false);
  const [deviceRequestId, setDeviceRequestId] = useState('');
  const [socialRequestId, setSocialRequestId] = useState('');
  const [socialThreshold, setSocialThreshold] = useState(0);
  const [socialSharesReceived, setSocialSharesReceived] = useState(0);
  const [ecdhKeyPair, setEcdhKeyPair] = useState<CryptoKeyPair | null>(null);
  const [recoveryData, setRecoveryData] = useState<{
    recovery_wrapped_private_key?: string;
    recovery_key_salt?: string;
    recovery_wrapped_prefs_key?: string;
    recovery_prefs_key_salt?: string;
  }>({});
  const [recoveryKeyInput, setRecoveryKeyInput] = useState('');
  const [recoveredPkcs8, setRecoveredPkcs8] = useState<ArrayBuffer | null>(null);
  const [newPassword, setNewPassword] = useState('');
  const [confirmPassword, setConfirmPassword] = useState('');
  const [acknowledgeDataLoss, setAcknowledgeDataLoss] = useState(false);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  const [success, setSuccess] = useState('');

  // Step 1: Send recovery code
  const handleSendCode = async () => {
    setLoading(true);
    setError('');
    try {
      const res = await fetch(apiUrl('/api/v1/auth/recovery/begin'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email }),
      });
      if (!res.ok) {
        const data = await res.json();
        throw new Error(data.error || 'Failed to send recovery code');
      }
      setStep('verify');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to send code');
    } finally {
      setLoading(false);
    }
  };

  // Step 2: Verify code
  const handleVerifyCode = async () => {
    setLoading(true);
    setError('');
    try {
      const res = await fetch(apiUrl('/api/v1/auth/recovery/verify-code'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, code }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Invalid code');

      setRecoveryToken(data.recovery_token);
      setHasRecoveryKey(data.has_recovery_key || false);
      setHasTrustedDevices(data.has_trusted_devices || false);
      setHasRecoveryCircle(data.has_recovery_circle || false);
      setRecoveryData({
        recovery_wrapped_private_key: data.recovery_wrapped_private_key,
        recovery_key_salt: data.recovery_key_salt,
        recovery_wrapped_prefs_key: data.recovery_wrapped_prefs_key,
        recovery_prefs_key_salt: data.recovery_prefs_key_salt,
      });

      if (data.has_recovery_key) {
        setStep('recovery-key');
      } else {
        setStep('reset-warning');
      }
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Verification failed');
    } finally {
      setLoading(false);
    }
  };

  // Step 3a: Verify recovery key
  const handleRecoveryKeySubmit = async () => {
    setLoading(true);
    setError('');
    try {
      const wrappedKey = recoveryData.recovery_wrapped_private_key;
      const salt = recoveryData.recovery_key_salt;
      if (!wrappedKey || !salt) {
        throw new Error('Recovery key material missing from server response');
      }
      const pkcs8Bytes = await unwrapWithRecoveryKey(wrappedKey, salt, recoveryKeyInput);
      setRecoveredPkcs8(pkcs8Bytes);
      setStep('new-password');
    } catch {
      setError('Invalid recovery key. Please check and try again.');
    } finally {
      setLoading(false);
    }
  };

  // Step 3b: Initiate trusted device recovery
  const handleDeviceRecovery = async () => {
    setLoading(true);
    setError('');
    try {
      const keyPair = await generateECDHKeyPair();
      setEcdhKeyPair(keyPair);
      const pubKeyBase64 = await exportECDHPublicKey(keyPair.publicKey);

      const res = await fetch(apiUrl('/api/v1/auth/recovery/device-request'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          recovery_token: recoveryToken,
          ephemeral_public_key: pubKeyBase64,
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Failed to create recovery request');

      // Upfront validation: if the server returns a malformed request_id,
      // fail fast with a user-visible error instead of
      // transitioning to device-waiting and discovering the problem 3s later
      // in the poll loop. The poll-catch fallback (below) remains as
      // defense-in-depth for any future path where a valid ID could become
      // corrupted in React state.
      if (!isValidUUID(data.request_id)) {
        throw new Error('Server returned an invalid recovery request ID. Please try again.');
      }

      setDeviceRequestId(data.request_id);
      setStep('device-waiting');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to initiate device recovery');
    } finally {
      setLoading(false);
    }
  };

  // Poll for trusted device response (handler defined inside useEffect to capture current closure values)
  useEffect(() => {
    if (step !== 'device-waiting' || !deviceRequestId) return;

    const pollDevice = async () => {
      try {
        const safeId = encodeURIComponent(assertValidUUID(deviceRequestId, 'deviceRequestId'));
        const res = await fetch(apiUrl(`/api/v1/auth/recovery/device-request/${safeId}`), {
          headers: { Authorization: `Bearer ${recoveryToken}` },
        });
        const data = await res.json();
        if (!res.ok) return;

        if (data.status === 'approved' && data.encrypted_payload && data.responder_public_key) {
          // Decrypt the payload using ECDH
          if (!ecdhKeyPair) {
            throw new Error('device recovery: ECDH key pair was not generated before polling');
          }
          const responderKey = await importECDHPublicKey(data.responder_public_key);
          const sharedKey = await deriveSharedSecret(ecdhKeyPair.privateKey, responderKey);
          const pkcs8Bytes = await decryptWithSharedSecret(sharedKey, data.encrypted_payload);
          setRecoveredPkcs8(pkcs8Bytes);
          setStep('new-password');
        } else if (data.status === 'rejected') {
          setError('Recovery request was rejected by the trusted device.');
          setStep('recovery-key');
        } else if (data.status === 'expired') {
          setError('Recovery request expired. Please try again.');
          setStep('recovery-key');
        }
        // If still pending, do nothing (will poll again)
      } catch {
        // Ignore poll errors silently — the next poll tick retries. See
        // [internal]rules/frontend.md re: idiomatic fetch-retry polling loops.
        // Malformed request_ids are caught upstream at handleDeviceRecovery
        // before setDeviceRequestId is ever called, so reaching this catch
        // in practice means fetch failure.
        // (The ECDH-invariant throw above is already gated by state-machine
        //  preconditions; also handled by the silent retry.)
      }
    };

    const interval = setInterval(pollDevice, 3000); // Poll every 3 seconds
    return () => clearInterval(interval);
  }, [step, deviceRequestId, recoveryToken, ecdhKeyPair]);

  // Step 3c: Initiate social recovery
  const handleSocialRecovery = async () => {
    setLoading(true);
    setError('');
    try {
      const keyPair = await generateECDHKeyPair();
      setEcdhKeyPair(keyPair);
      const pubKeyBase64 = await exportECDHPublicKey(keyPair.publicKey);

      const res = await fetch(apiUrl('/api/v1/auth/recovery/social-request'), {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          recovery_token: recoveryToken,
          ephemeral_public_key: pubKeyBase64,
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Failed');

      // Upfront validation mirroring handleDeviceRecovery — see that comment
      // for the full rationale (prevents silent infinite-poll trap for
      // malformed server request_ids).
      if (!isValidUUID(data.request_id)) {
        throw new Error('Server returned an invalid recovery request ID. Please try again.');
      }

      setSocialRequestId(data.request_id);
      setSocialThreshold(data.threshold_k);
      setStep('social-waiting');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed');
    } finally {
      setLoading(false);
    }
  };

  // Poll for social recovery shares (handler defined inside useEffect to capture current closure values)
  useEffect(() => {
    if (step !== 'social-waiting' || !socialRequestId) return;

    const pollSocial = async () => {
      try {
        const safeId = encodeURIComponent(assertValidUUID(socialRequestId, 'socialRequestId'));
        const res = await fetch(apiUrl(`/api/v1/auth/recovery/social-request/${safeId}`), {
          headers: { Authorization: `Bearer ${recoveryToken}` },
        });
        const data = await res.json();
        if (!res.ok) return;

        setSocialSharesReceived(data.shares_received || 0);

        if (data.status === 'complete' && data.responses) {
          // Reconstruct from shares
          const { combine } = await import('../../utils/shamir');
          const shares: Array<{ index: number; data: Uint8Array }> = [];

          for (const resp of data.responses) {
            // Each response contains a JSON payload with ephemeral_public_key, encrypted_data, share_index
            const payloadStr = atob(resp.encrypted_share);
            const payload = JSON.parse(payloadStr);

            // Derive shared secret with responder's ECDH key
            if (!ecdhKeyPair) {
              throw new Error('social recovery: ECDH key pair was not generated before polling');
            }
            const responderKey = await importECDHPublicKey(payload.ephemeral_public_key);
            const sharedKey = await deriveSharedSecret(ecdhKeyPair.privateKey, responderKey);
            const shareBytes = await decryptWithSharedSecret(sharedKey, payload.encrypted_data);

            // share_index is embedded in the encrypted payload by the contact
            const shareIndex: number = payload.share_index;
            shares.push({ index: shareIndex, data: new Uint8Array(shareBytes) });
          }

          // Reconstruct PKCS8
          const reconstructed = combine(shares);
          setRecoveredPkcs8(reconstructed.buffer as ArrayBuffer);
          setStep('new-password');
        }
      } catch {
        // Ignore poll errors silently — the next poll tick retries (mirrors
        // pollDevice above). Malformed request_ids are caught upstream at
        // handleSocialRecovery before setSocialRequestId is ever called.
        // The ECDH-invariant throw inside the loop is already gated by
        // state-machine preconditions.
      }
    };

    const interval = setInterval(pollSocial, 5000); // Poll every 5 seconds
    return () => clearInterval(interval);
  }, [step, socialRequestId, recoveryToken, ecdhKeyPair]);

  // Step 4: Set new password
  const handleSetPassword = async () => {
    if (newPassword !== confirmPassword) {
      setError('Passwords do not match');
      return;
    }
    if (newPassword.length < 12) {
      setError('Password must be at least 12 characters');
      return;
    }

    setLoading(true);
    setError('');
    try {
      if (recoveredPkcs8) {
        // Recovery key path — same keypair
        const salt = generateSalt();
        const wrappingKey = await deriveKeyArgon2id(newPassword, salt);

        // Import recovered PKCS8 as extractable CryptoKey for wrapping
        const privateKeyToWrap = await crypto.subtle.importKey(
          'pkcs8',
          recoveredPkcs8,
          { name: 'RSA-OAEP', hash: 'SHA-256' },
          true, // extractable
          ['decrypt']
        );

        // Wrap using wrapKey (which wrappingKey supports)
        const iv = crypto.getRandomValues(new Uint8Array(12));
        const wrapped = await crypto.subtle.wrapKey('pkcs8', privateKeyToWrap, wrappingKey, {
          name: 'AES-GCM',
          iv,
        });
        const wrappedResult = new Uint8Array(12 + wrapped.byteLength);
        wrappedResult.set(iv, 0);
        wrappedResult.set(new Uint8Array(wrapped), 12);

        const res = await fetch(apiUrl('/api/v1/auth/recovery/reset-password'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            recovery_token: recoveryToken,
            new_password: newPassword,
            wrapped_private_key: arrayBufferToBase64(wrappedResult.buffer),
            key_derivation_salt: arrayBufferToBase64(salt.buffer as ArrayBuffer),
            key_derivation_alg: 'argon2id',
          }),
        });
        if (!res.ok) {
          const data = await res.json();
          throw new Error(data.error || 'Password reset failed');
        }
      } else {
        // Account reset path — new keypair, data loss
        const newKeys = await generateRegistrationKeys(newPassword);

        const res = await fetch(apiUrl('/api/v1/auth/recovery/reset-account'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            recovery_token: recoveryToken,
            new_password: newPassword,
            wrapped_private_key: newKeys.wrappedPrivateKey,
            key_derivation_salt: newKeys.keyDerivationSalt,
            key_derivation_alg: newKeys.keyDerivationAlg,
            public_key: arrayBufferToBase64(
              await crypto.subtle.exportKey('spki', newKeys.publicKey)
            ),
            acknowledge_data_loss: true,
          }),
        });
        if (!res.ok) {
          const data = await res.json();
          throw new Error(data.error || 'Account reset failed');
        }
      }

      setSuccess('Password reset successfully. Please sign in with your new password.');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Reset failed');
    } finally {
      setLoading(false);
    }
  };

  // Success screen
  if (success) {
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <img
              src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
              className="login-logo"
              alt="Concord Voice"
            />
            <h2 className="login-title">Password Reset Complete</h2>
            <p className="login-subtitle">{success}</p>
          </div>
          <div className="login-form">
            <button type="button" className="login-submit-btn" onClick={onComplete}>
              Sign In
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="login-container">
      <div className="login-content">
        <div className="login-header">
          <img
            src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
            className="login-logo"
            alt="Concord Voice"
          />
          <h2 className="login-title">Account Recovery</h2>
          <p className="login-subtitle">
            {step === 'email' && 'Enter your email to receive a recovery code'}
            {step === 'verify' && 'Enter the 6-digit code sent to your email'}
            {step === 'recovery-key' && 'Enter your recovery key to restore your encrypted data'}
            {step === 'device-waiting' && 'Waiting for trusted device approval'}
            {step === 'social-waiting' && 'Waiting for Recovery Circle approval'}
            {step === 'reset-warning' && 'No recovery key found'}
            {step === 'new-password' && 'Set your new password'}
          </p>
        </div>

        <div className="login-form">
          {step === 'email' && (
            <>
              <div className="form-group">
                <label htmlFor="recovery-email" className="form-label">
                  Email
                </label>
                <input
                  id="recovery-email"
                  type="email"
                  className="form-input"
                  placeholder="you@example.com"
                  value={email}
                  onChange={(e) => setEmail(e.target.value)}
                  disabled={loading}
                  autoFocus
                />
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="login-submit-btn"
                disabled={loading || !email}
                onClick={handleSendCode}
              >
                {loading ? (
                  <>
                    Sending... <LoadingSpinner size="small" inline />
                  </>
                ) : (
                  'Send Recovery Code'
                )}
              </button>
            </>
          )}

          {step === 'verify' && (
            <>
              <div className="form-group">
                <label htmlFor="recovery-verify-code" className="form-label">
                  Verification Code
                </label>
                <input
                  id="recovery-verify-code"
                  type="text"
                  className="form-input"
                  placeholder="000000"
                  value={code}
                  onChange={(e) => setCode(e.target.value.replaceAll(/\D/g, '').slice(0, 6))}
                  disabled={loading}
                  autoFocus
                  maxLength={6}
                  style={{
                    textAlign: 'center',
                    letterSpacing: 8,
                    fontSize: 24,
                    fontFamily: 'monospace',
                  }}
                />
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="login-submit-btn"
                disabled={loading || code.length !== 6}
                onClick={handleVerifyCode}
              >
                {loading ? (
                  <>
                    Verifying... <LoadingSpinner size="small" inline />
                  </>
                ) : (
                  'Verify Code'
                )}
              </button>
            </>
          )}

          {step === 'recovery-key' && (
            <>
              <div className="form-group">
                <label htmlFor="recovery-key-input" className="form-label">
                  Recovery Key
                </label>
                <textarea
                  id="recovery-key-input"
                  className="form-input"
                  placeholder="Enter your recovery key (with or without dashes)"
                  value={recoveryKeyInput}
                  onChange={(e) => setRecoveryKeyInput(e.target.value)}
                  disabled={loading}
                  rows={3}
                  autoFocus
                  style={{ fontFamily: 'monospace', fontSize: 14, resize: 'none' }}
                />
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="login-submit-btn"
                disabled={loading || !recoveryKeyInput.trim()}
                onClick={handleRecoveryKeySubmit}
              >
                {loading ? (
                  <>
                    Recovering... <LoadingSpinner size="small" inline />
                  </>
                ) : (
                  'Recover Account'
                )}
              </button>
              <button
                type="button"
                className="mfa-choose-another"
                onClick={() => setStep('reset-warning')}
                style={{ marginTop: 8 }}
              >
                I don&apos;t have my recovery key
              </button>
              {hasTrustedDevices && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={handleDeviceRecovery}
                  disabled={loading}
                  style={{ marginTop: 8 }}
                >
                  Recover from trusted device instead
                </button>
              )}
              {hasRecoveryCircle && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={handleSocialRecovery}
                  disabled={loading}
                  style={{ marginTop: 4 }}
                >
                  Recover via Recovery Circle
                </button>
              )}
            </>
          )}

          {step === 'device-waiting' && (
            <>
              <div style={{ textAlign: 'center', padding: '20px 0' }}>
                <LoadingSpinner size="small" inline />
                <p style={{ color: 'var(--text-secondary)', marginTop: 12 }}>
                  Waiting for approval from your trusted device...
                </p>
                <p style={{ color: 'var(--text-tertiary, #718096)', fontSize: 13 }}>
                  Open Concord on your trusted device and approve the recovery request.
                </p>
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="mfa-choose-another"
                onClick={() => {
                  setStep(hasRecoveryKey ? 'recovery-key' : 'reset-warning');
                  setError('');
                }}
                style={{ marginTop: 8 }}
              >
                Try a different recovery method
              </button>
            </>
          )}

          {step === 'social-waiting' && (
            <>
              <div style={{ textAlign: 'center', padding: '20px 0' }}>
                <LoadingSpinner size="small" inline />
                <p style={{ color: 'var(--text-secondary)', marginTop: 12 }}>
                  Waiting for your Recovery Circle to respond...
                </p>
                <p style={{ color: 'var(--text-primary)', fontSize: 18, fontWeight: 600 }}>
                  {socialSharesReceived} / {socialThreshold} shares received
                </p>
                <p style={{ color: 'var(--text-tertiary, #718096)', fontSize: 13 }}>
                  Your contacts need to open Concord and approve your recovery request. This may
                  take up to 24 hours.
                </p>
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="mfa-choose-another"
                onClick={() => {
                  setStep(hasRecoveryKey ? 'recovery-key' : 'reset-warning');
                  setError('');
                }}
              >
                Try a different recovery method
              </button>
            </>
          )}

          {step === 'reset-warning' && (
            <>
              <div
                className="form-error-banner"
                style={{
                  background: 'rgba(220, 38, 38, 0.15)',
                  border: '1px solid #dc2626',
                  marginBottom: 16,
                }}
              >
                <span>
                  <strong>Warning: Permanent Data Loss</strong>
                  <br />
                  Without your recovery key, all encrypted message history will be permanently lost.
                  Your account, servers, friends, and settings will be preserved, but past encrypted
                  messages cannot be recovered.
                </span>
              </div>
              <label className="remember-me-label" style={{ marginBottom: 16 }}>
                <input
                  type="checkbox"
                  checked={acknowledgeDataLoss}
                  onChange={(e) => setAcknowledgeDataLoss(e.target.checked)}
                />
                <span>
                  I understand that all encrypted message history will be permanently lost
                </span>
              </label>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="login-submit-btn"
                disabled={!acknowledgeDataLoss}
                onClick={() => setStep('new-password')}
                style={{ background: acknowledgeDataLoss ? '#dc2626' : undefined }}
              >
                Continue with Account Reset
              </button>
              {hasRecoveryKey && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={() => setStep('recovery-key')}
                  style={{ marginTop: 8 }}
                >
                  I found my recovery key
                </button>
              )}
              {hasTrustedDevices && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={handleDeviceRecovery}
                  disabled={loading}
                  style={{ marginTop: 8 }}
                >
                  Recover from trusted device instead
                </button>
              )}
              {hasRecoveryCircle && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={handleSocialRecovery}
                  disabled={loading}
                  style={{ marginTop: 4 }}
                >
                  Recover via Recovery Circle
                </button>
              )}
            </>
          )}

          {step === 'new-password' && (
            <>
              <div className="form-group">
                <label htmlFor="recovery-new-password" className="form-label">
                  New Password
                </label>
                <input
                  id="recovery-new-password"
                  type="password"
                  className="form-input"
                  placeholder="At least 12 characters"
                  value={newPassword}
                  onChange={(e) => setNewPassword(e.target.value)}
                  disabled={loading}
                  autoFocus
                />
              </div>
              <div className="form-group">
                <label htmlFor="recovery-confirm-password" className="form-label">
                  Confirm Password
                </label>
                <input
                  id="recovery-confirm-password"
                  type="password"
                  className="form-input"
                  placeholder="Confirm your new password"
                  value={confirmPassword}
                  onChange={(e) => setConfirmPassword(e.target.value)}
                  disabled={loading}
                />
              </div>
              {error && (
                <div className="form-error-banner">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className="login-submit-btn"
                disabled={loading || !newPassword || !confirmPassword}
                onClick={handleSetPassword}
              >
                {loading ? (
                  <>
                    Resetting... <LoadingSpinner size="small" inline />
                  </>
                ) : (
                  'Reset Password'
                )}
              </button>
            </>
          )}

          <button type="button" className="login-back-btn" onClick={onBack} disabled={loading}>
            &larr; Back to login
          </button>
        </div>
      </div>
    </div>
  );
};

export default AccountRecovery;
