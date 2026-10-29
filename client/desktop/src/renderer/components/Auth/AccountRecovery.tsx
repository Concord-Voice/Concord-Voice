import React, { useState, useEffect, useRef, useCallback } from 'react';
import { z } from 'zod';
import { useMFAChallengeStore } from '../../stores/auth/mfaChallengeStore';
import { recordChallengeIssuer } from '../../services/system/challengeIssuer';
import { methodsForOptions, webauthnOptionsOrNull } from '../../utils/webauthnOptions';
import {
  unwrapWithRecoveryKey,
  generateRegistrationKeys,
  arrayBufferToBase64,
  generateSalt,
  exportPublicKey,
  rewrapRecoveryAccountKey,
  deriveKeyArgon2id,
} from '../../utils/crypto/crypto';
import {
  apiUrl,
  captureRuntimeServerSelection,
  onRuntimeServerSelectionChange,
  runtimeServerSelectionIsCurrent,
} from '../../services/system/runtimeServerBase';
import {
  RequesterDeviceRecoveryAttempt,
  type DeviceRecoveryView,
} from '../../services/system/deviceRecoveryService';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import { useAuthStore } from '../../stores/auth/authStore';
import { e2eeService } from '../../services/e2ee/e2eeService';
import DeviceRecoveryFingerprint from './DeviceRecoveryFingerprint';
import LoadingSpinner from './LoadingSpinner';
import './Login.css';
import ConcordWordmark from './ConcordWordmark';

type RecoveryStep =
  'email' | 'verify' | 'recovery-key' | 'device-waiting' | 'reset-warning' | 'new-password';

interface AccountRecoveryProps {
  onBack: () => void;
  onComplete: () => void; // Navigate back to login on success
}

const RecoveryResponseSchema = z
  .object({
    recovery_token: z.string().min(1),
    has_recovery_key: z.boolean(),
    has_trusted_devices: z.boolean(),
    has_recovery_circle: z.boolean(),
    recovery_wrapped_private_key: z.string().optional(),
    recovery_key_salt: z.string().optional(),
    recovery_wrapped_prefs_key: z.string().optional(),
    recovery_prefs_key_salt: z.string().optional(),
  })
  .refine(
    (data) =>
      !data.has_recovery_key ||
      Boolean(data.recovery_wrapped_private_key?.trim() && data.recovery_key_salt?.trim())
  );
const RecoveryChallengeSchema = z.object({
  mfa_required: z.literal(true),
  mfa_challenge_token: z.string().min(1),
  methods: z.array(z.enum(['totp', 'webauthn'])).min(1),
  webauthn_options: z.unknown().optional(),
});

const AccountRecovery: React.FC<AccountRecoveryProps> = ({ onBack, onComplete }) => {
  const [step, setStep] = useState<RecoveryStep>('email');
  const [email, setEmail] = useState('');
  const [code, setCode] = useState('');
  const [recoveryToken, setRecoveryToken] = useState('');
  const [hasRecoveryKey, setHasRecoveryKey] = useState(false);
  const [hasTrustedDevices, setHasTrustedDevices] = useState(false);
  const [hasRecoveryCircle, setHasRecoveryCircle] = useState(false);
  const deviceAttemptRef = useRef<RequesterDeviceRecoveryAttempt | null>(null);
  const deviceStartingRef = useRef(false);
  const mountedRef = useRef(true);
  const recoveryChallengeRef = useRef<string | null>(null);
  const issuanceRef = useRef<(() => boolean) | null>(null);
  const plaintextRef = useRef<ArrayBuffer | null>(null);
  const [deviceView, setDeviceView] = useState<DeviceRecoveryView | null>(null);
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
  const loadingAttemptRef = useRef(0);
  const startLoading = () => {
    const attempt = ++loadingAttemptRef.current;
    setLoading(true);
    return attempt;
  };
  const finishLoading = (attempt: number) => {
    if (mountedRef.current && loadingAttemptRef.current === attempt) setLoading(false);
  };

  const retainRecoveredPkcs8 = useCallback((bytes: ArrayBuffer | null) => {
    if (!mountedRef.current) {
      if (bytes) new Uint8Array(bytes).fill(0);
      return;
    }
    if (plaintextRef.current && plaintextRef.current !== bytes)
      new Uint8Array(plaintextRef.current).fill(0);
    plaintextRef.current = bytes;
    setRecoveredPkcs8(bytes);
  }, []);
  const cancelDeviceRecovery = useCallback(() => {
    deviceAttemptRef.current?.dispose();
    deviceAttemptRef.current = null;
    setDeviceView(null);
  }, []);
  const recoveryIsCurrent = useCallback(
    (issued = issuanceRef.current) => {
      if (!mountedRef.current || issued !== issuanceRef.current) return false;
      if (issuanceRef.current?.()) return true;
      issuanceRef.current = null;
      const challengeToken = recoveryChallengeRef.current;
      if (challengeToken) {
        recoveryChallengeRef.current = null;
        useMFAChallengeStore.getState().completeChallenge({ verified: false }, challengeToken);
      }
      cancelDeviceRecovery();
      retainRecoveredPkcs8(null);
      setRecoveryToken('');
      setRecoveryData({});
      setHasRecoveryKey(false);
      setHasTrustedDevices(false);
      setHasRecoveryCircle(false);
      setRecoveryKeyInput('');
      setNewPassword('');
      setConfirmPassword('');
      setAcknowledgeDataLoss(false);
      setCode('');
      setStep('email');
      loadingAttemptRef.current++;
      setLoading(false);
      setError('Recovery context changed. Start again.');
      return false;
    },
    [cancelDeviceRecovery, retainRecoveredPkcs8]
  );
  const continueRecovery = (next: RecoveryStep) => {
    if (recoveryIsCurrent()) setStep(next);
  };
  useEffect(() => {
    mountedRef.current = true;
    const invalidate = () => {
      if (issuanceRef.current) recoveryIsCurrent();
    };
    const unsubscribeAuth = useAuthStore.subscribe(invalidate);
    const unsubscribeServer = onRuntimeServerSelectionChange(invalidate);
    return () => {
      unsubscribeAuth();
      unsubscribeServer();
      mountedRef.current = false;
      issuanceRef.current = null;
      if (recoveryChallengeRef.current) {
        useMFAChallengeStore
          .getState()
          .completeChallenge({ verified: false }, recoveryChallengeRef.current);
      }
      deviceAttemptRef.current?.dispose();
      deviceAttemptRef.current = null;
      if (plaintextRef.current) new Uint8Array(plaintextRef.current).fill(0);
      plaintextRef.current = null;
    };
  }, [recoveryIsCurrent]);

  // Step 1: Send recovery code
  const handleSendCode = async () => {
    const loadingAttempt = startLoading();
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
      finishLoading(loadingAttempt);
    }
  };

  const completeRecoveryChallenge = async (
    challenge: z.infer<typeof RecoveryChallengeSchema>,
    selection: ReturnType<typeof captureRuntimeServerSelection>,
    assertCurrent: () => void
  ) => {
    const { mfa_challenge_token: token, methods, webauthn_options: options } = challenge;
    const webauthn = webauthnOptionsOrNull(options, selection.apiBase);
    const offered = methodsForOptions(methods, webauthn);
    if (offered.length === 0 || (offered.includes('webauthn') && !webauthn)) {
      setCode('');
      setStep('email');
      throw new Error('Recovery verification is unavailable. Request a new email code.');
    }
    recordChallengeIssuer(token, selection);
    recoveryChallengeRef.current = token;
    const result = await useMFAChallengeStore
      .getState()
      .showChallenge(token, offered, 'recovery', [], undefined, webauthn);
    recoveryChallengeRef.current = null;
    assertCurrent();
    return result;
  };

  const handleVerificationError = (err: unknown, issued: () => boolean, proofConsumed: boolean) => {
    if (issuanceRef.current !== issued) return;
    issuanceRef.current = null;
    if (proofConsumed) {
      const challengeToken = recoveryChallengeRef.current;
      if (challengeToken) {
        recoveryChallengeRef.current = null;
        useMFAChallengeStore.getState().completeChallenge({ verified: false }, challengeToken);
      }
      setCode('');
      setStep('email');
    }
    setError(err instanceof Error ? err.message : 'Verification failed');
  };

  // Step 2: Verify code
  const handleVerifyCode = async () => {
    const loadingAttempt = startLoading();
    setError('');
    const selection = captureRuntimeServerSelection();
    const lifecycle = captureAuthLifecycle();
    const issued = () =>
      runtimeServerSelectionIsCurrent(selection) && isSameAuthLifecycle(lifecycle);
    issuanceRef.current = issued;
    let proofConsumed = false;
    const assertCurrent = () => {
      if (!mountedRef.current || !issued()) {
        throw new Error('Recovery context changed. Start again.');
      }
    };
    try {
      const res = await fetch(`${selection.apiBase}/api/v1/auth/recovery/verify-code`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ email, code }),
      });
      proofConsumed = res.ok;
      let payload: unknown = await res.json();
      assertCurrent();
      if (!res.ok) {
        const failure = z.object({ error: z.string() }).safeParse(payload);
        throw new Error(failure.success ? failure.data.error : 'Invalid code');
      }
      const challenge = RecoveryChallengeSchema.safeParse(payload);
      if (challenge.success) {
        const result = await completeRecoveryChallenge(challenge.data, selection, assertCurrent);
        if (!result.verified || !('payload' in result)) {
          issuanceRef.current = null;
          setCode('');
          setStep('email');
          return;
        }
        payload = result.payload;
      }
      const verified = RecoveryResponseSchema.safeParse(payload);
      if (!verified.success) throw new Error('Recovery verification is incomplete. Start again.');
      const data = verified.data;

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
      handleVerificationError(err, issued, proofConsumed);
    } finally {
      finishLoading(loadingAttempt);
    }
  };

  // Step 3a: Verify recovery key
  const handleRecoveryKeySubmit = async () => {
    const issued = issuanceRef.current;
    if (!recoveryIsCurrent(issued)) return;
    const loadingAttempt = startLoading();
    setError('');
    try {
      const wrappedKey = recoveryData.recovery_wrapped_private_key;
      const salt = recoveryData.recovery_key_salt;
      if (!wrappedKey || !salt) {
        throw new Error('Recovery key material missing from server response');
      }
      const pkcs8Bytes = await unwrapWithRecoveryKey(wrappedKey, salt, recoveryKeyInput);
      if (!recoveryIsCurrent(issued)) {
        new Uint8Array(pkcs8Bytes).fill(0);
        return;
      }
      retainRecoveredPkcs8(pkcs8Bytes);
      setStep('new-password');
    } catch {
      if (recoveryIsCurrent(issued)) setError('Invalid recovery key. Please check and try again.');
    } finally {
      finishLoading(loadingAttempt);
    }
  };

  // A service-owned attempt pins local context, the offer, consent, and completion acknowledgement.
  const handleDeviceRecovery = async () => {
    if (!recoveryIsCurrent()) return;
    if (deviceStartingRef.current) return;
    deviceStartingRef.current = true;
    cancelDeviceRecovery();
    const loadingAttempt = startLoading();
    setError('');
    const attempt = new RequesterDeviceRecoveryAttempt(recoveryToken, (view) => {
      if (!mountedRef.current || deviceAttemptRef.current !== attempt) return;
      if (!recoveryIsCurrent()) return;
      setDeviceView(view);
      setError(view.error);
      if (view.status === 'complete') {
        retainRecoveredPkcs8(attempt.recoveredAccountKey());
        setStep('new-password');
      } else if (view.status === 'error') {
        deviceAttemptRef.current = null;
        retainRecoveredPkcs8(null);
        setStep(hasRecoveryKey ? 'recovery-key' : 'reset-warning');
      }
    });
    deviceAttemptRef.current = attempt;
    setStep('device-waiting');
    await attempt.start();
    deviceStartingRef.current = false;
    finishLoading(loadingAttempt);
  };

  // Step 4: Set new password
  const handleSetPassword = async () => {
    const issued = issuanceRef.current;
    if (!recoveryIsCurrent(issued)) return;
    if (newPassword !== confirmPassword) {
      setError('Passwords do not match');
      return;
    }
    if (newPassword.length < 12) {
      setError('Password must be at least 12 characters');
      return;
    }

    const loadingAttempt = startLoading();
    setError('');
    try {
      const auth = captureAuthLifecycle();
      const server = captureRuntimeServerSelection();
      const keyEpoch = e2eeService.captureTeardownEpoch();
      const currentAttempt = deviceAttemptRef.current;
      const assertCurrent = () => {
        if (
          !recoveryIsCurrent(issued) ||
          !isSameAuthLifecycle(auth) ||
          !runtimeServerSelectionIsCurrent(server) ||
          e2eeService.wasTornDownSince(keyEpoch)
        )
          throw new Error('Account, server or keys changed. Restart recovery.');
        currentAttempt?.assertCurrent();
      };
      assertCurrent();
      if (recoveredPkcs8) {
        // Recovery key path — same keypair
        const salt = generateSalt();
        const wrappingKey = await deriveKeyArgon2id(newPassword, salt);
        assertCurrent();
        const wrappedPrivateKey = await rewrapRecoveryAccountKey(
          recoveredPkcs8,
          wrappingKey,
          assertCurrent
        );
        assertCurrent();

        const res = await fetch(apiUrl('/api/v1/auth/recovery/reset-password'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            recovery_token: recoveryToken,
            new_password: newPassword,
            wrapped_private_key: wrappedPrivateKey,
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
        assertCurrent();

        const publicKey = await exportPublicKey(newKeys.publicKey);
        assertCurrent();
        const res = await fetch(apiUrl('/api/v1/auth/recovery/reset-account'), {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            recovery_token: recoveryToken,
            new_password: newPassword,
            wrapped_private_key: newKeys.wrappedPrivateKey,
            key_derivation_salt: newKeys.keyDerivationSalt,
            key_derivation_alg: newKeys.keyDerivationAlg,
            public_key: publicKey,
            acknowledge_data_loss: true,
          }),
        });
        if (!res.ok) {
          const data = await res.json();
          throw new Error(data.error || 'Account reset failed');
        }
      }

      assertCurrent();
      retainRecoveredPkcs8(null);
      cancelDeviceRecovery();
      setSuccess('Password reset successfully. Please sign in with your new password.');
    } catch (err) {
      if (recoveryIsCurrent(issued)) setError(err instanceof Error ? err.message : 'Reset failed');
    } finally {
      finishLoading(loadingAttempt);
    }
  };

  let deviceStatusMessage = 'Open Concord on your trusted device and review this request.';
  if (deviceView?.status === 'approved-locked') {
    deviceStatusMessage =
      'Approval received. Your key remains locked until you confirm the fingerprint.';
  } else if (deviceView?.status === 'completing') {
    deviceStatusMessage = 'Validating the account key and acknowledging completion…';
  }

  // Success screen
  if (success) {
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <ConcordWordmark className="login-logo" />
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
          <ConcordWordmark className="login-logo" />
          <h2 className="login-title">Account Recovery</h2>
          <p className="login-subtitle">
            {step === 'email' && 'Enter your email to receive a recovery code'}
            {step === 'verify' && 'Enter the 6-digit code sent to your email'}
            {step === 'recovery-key' && 'Enter your recovery key to restore your encrypted data'}
            {step === 'device-waiting' && 'Waiting for trusted device approval'}
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
                <div className="form-error-banner" role="alert">
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
                onClick={() => continueRecovery('reset-warning')}
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
                <p>Recovery Circle recovery is temporarily paused. Use another recovery method.</p>
              )}
            </>
          )}

          {step === 'device-waiting' && (
            <div className="device-recovery-ceremony">
              <p role="status" aria-live="polite">
                {deviceStatusMessage}
              </p>
              <p>
                Compare all eight groups directly with your trusted device. Confirm only when every
                character matches. Never send the fingerprint to support. Both devices must run an
                updated version of Concord.
              </p>
              {deviceView?.fingerprint && (
                <DeviceRecoveryFingerprint fingerprint={deviceView.fingerprint} />
              )}
              {error && <p role="alert">{error}</p>}
              <div className="device-recovery-actions">
                <button
                  type="button"
                  className="login-submit-btn"
                  disabled={
                    !deviceView?.fingerprint ||
                    deviceView.confirmed ||
                    deviceView.status === 'completing'
                  }
                  onClick={() => {
                    if (recoveryIsCurrent()) void deviceAttemptRef.current?.confirmMatch();
                  }}
                >
                  {deviceView?.confirmed ? 'Fingerprints confirmed' : 'These fingerprints match'}
                </button>
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={() => {
                    if (!recoveryIsCurrent()) return;
                    cancelDeviceRecovery();
                    retainRecoveredPkcs8(null);
                    setStep(hasRecoveryKey ? 'recovery-key' : 'reset-warning');
                    setError('');
                  }}
                >
                  Try a different recovery method
                </button>
              </div>
            </div>
          )}

          {step === 'reset-warning' && (
            <>
              <div
                className="form-error-banner"
                style={{
                  background: 'rgba(220, 38, 38, 0.15)',
                  border: '1px solid #dc2626',
                  color: 'var(--text-primary)',
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
                <div className="form-error-banner" role="alert">
                  <span>{error}</span>
                </div>
              )}
              <button
                type="button"
                className={`login-submit-btn${acknowledgeDataLoss ? ' login-submit-btn--danger' : ''}`}
                disabled={!acknowledgeDataLoss}
                onClick={() => continueRecovery('new-password')}
              >
                Continue with Account Reset
              </button>
              {hasRecoveryKey && (
                <button
                  type="button"
                  className="mfa-choose-another"
                  onClick={() => continueRecovery('recovery-key')}
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
                <p>Recovery Circle recovery is temporarily paused. Use another recovery method.</p>
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

          <button
            type="button"
            className="login-back-btn"
            onClick={() => {
              cancelDeviceRecovery();
              retainRecoveredPkcs8(null);
              onBack();
            }}
            disabled={loading}
          >
            &larr; Back to login
          </button>
        </div>
      </div>
    </div>
  );
};

export default AccountRecovery;
