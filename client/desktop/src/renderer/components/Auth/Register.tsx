import React, { useState } from 'react';
import PasswordStrength from './PasswordStrength';
import InfoTooltip from './InfoTooltip';
import LoadingSpinner from './LoadingSpinner';
import { SSOButton } from './SSOButton';
import { useSSOFlow } from '../../hooks/useSSOFlow';
import { generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import { API_BASE, ensureMachineId } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import { errorMessage } from '../../utils/redactError';
import {
  usePendingRegistrationStore,
  type PendingRegistrationResponse,
} from '../../stores/pendingRegistrationStore';
import './Register.css';

export interface RegisterProps {
  onBack: () => void;
  onSuccess: (data: { pendingId: string; email: string }) => void;
  onSwitchToLogin: () => void;
}

interface FormData {
  email: string;
  username: string;
  password: string;
  confirmPassword: string;
  ageConfirmed: boolean;
}

interface FormErrors {
  email?: string;
  username?: string;
  password?: string;
  confirmPassword?: string;
  ageConfirmed?: string;
  general?: string;
}

const Register: React.FC<RegisterProps> = ({ onBack, onSuccess, onSwitchToLogin }) => {
  const [formData, setFormData] = useState<FormData>({
    email: '',
    username: '',
    password: '',
    confirmPassword: '',
    ageConfirmed: false,
  });

  const [errors, setErrors] = useState<FormErrors>({});
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [passwordsMatch, setPasswordsMatch] = useState(false);
  const { begin: beginSSO } = useSSOFlow();

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};

    // Email validation
    const emailRegex = /^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)+$/;
    if (!formData.email) {
      newErrors.email = 'Email is required';
    } else if (!emailRegex.test(formData.email)) {
      newErrors.email = 'Invalid email format';
    }

    // Username validation
    const usernameRegex = /^[a-zA-Z0-9][a-zA-Z0-9._-]*[a-zA-Z0-9]$/;
    if (!formData.username) {
      newErrors.username = 'Username is required';
    } else if (formData.username.length < 3 || formData.username.length > 32) {
      newErrors.username = 'Username must be 3-32 characters';
    } else if (!usernameRegex.test(formData.username)) {
      newErrors.username =
        'Letters, numbers, periods, underscores, and hyphens only. Must start and end with a letter or number.';
    } else if (/[._-]{2,}/.test(formData.username)) {
      newErrors.username =
        'Username cannot contain consecutive special characters (periods, underscores, hyphens).';
    }

    // Password validation
    if (!formData.password) {
      newErrors.password = 'Password is required';
    } else if (formData.password.length < 12) {
      newErrors.password = 'Password must be at least 12 characters';
    }

    // Confirm password validation
    if (!formData.confirmPassword) {
      newErrors.confirmPassword = 'Please confirm your password';
    } else if (formData.password !== formData.confirmPassword) {
      newErrors.confirmPassword = 'Passwords do not match';
    }

    // Age confirmation
    if (!formData.ageConfirmed) {
      newErrors.ageConfirmed = 'You must confirm you are at least 16 years old';
    }

    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setErrors({});

    if (!validateForm()) {
      return;
    }

    // Fail-closed safeStorage enforcement (#197): block registration if secure storage is unavailable.
    // When Electron IPC is present, any failure (denied or IPC error) blocks registration.
    if (globalThis.electron?.checkPermission) {
      try {
        const storageStatus = await globalThis.electron.checkPermission('secureStorage');
        if (storageStatus !== 'granted') {
          setErrors({
            general:
              'Secure storage is unavailable. Concord requires keychain / credential manager access to safely store authentication tokens and encryption keys. Please enable it and restart the app.',
          });
          return;
        }
      } catch {
        // Electron IPC present but check failed — fail closed
        setErrors({
          general:
            'Secure storage could not be verified. Please try again, and if the problem persists, restart the app.',
        });
        return;
      }
    }

    setIsSubmitting(true);

    try {
      // Generate E2EE keys
      console.debug('Generating E2EE keys...');
      const keys = await generateRegistrationKeys(formData.password);
      const publicKeyBase64 = await exportPublicKey(keys.publicKey);

      console.debug('Keys generated, registering with server...');

      const machineId = await ensureMachineId();

      // Register with backend
      const response = await fetch(`${API_BASE}/api/v1/auth/register`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include', // Include cookies for refresh token
        body: JSON.stringify({
          email: formData.email,
          username: formData.username,
          password: formData.password,
          age_confirmation: formData.ageConfirmed,
          public_key: publicKeyBase64,
          wrapped_private_key: keys.wrappedPrivateKey,
          key_derivation_salt: keys.keyDerivationSalt,
          key_derivation_alg: keys.keyDerivationAlg,
        }),
      });

      const data = (await response.json()) as PendingRegistrationResponse & { error?: string };

      if (!response.ok) {
        throw new Error(data.error ?? 'Registration failed');
      }

      console.debug('Registration successful, pending email verification...');

      // Initialize the E2EE service with the keys we just generated, so the user
      // can use encrypted features (channel creation, messaging) immediately
      // after verifying their email. The registration flow otherwise never calls
      // e2eeService.initialize (only Login.tsx does), leaving
      // e2eeService.isInitialized === false and blocking channel creation with
      // "Setting up secure messaging — try again in a moment". Post-registration
      // the user reaches /app straight from email-verification (no intervening
      // login), so without this they'd be stuck until a manual logout→login.
      //
      // The two failure modes are handled SEPARATELY (per #1278 review):
      //
      //   1. initialize() failure → clearKeys(). finalizeKeys assigns wrappingKey
      //      before later steps, so a mid-init throw could leave isInitialized
      //      === true on a half-initialized service; rolling back makes it
      //      honestly false. Non-fatal: registration already succeeded
      //      server-side; secure messaging falls back to a manual logout→login.
      try {
        await e2eeService.initialize(
          formData.password,
          keys.wrappedPrivateKey,
          keys.keyDerivationSalt,
          keys.keyDerivationAlg
        );
      } catch (initError) {
        e2eeService.clearKeys();
        console.warn(
          'E2EE init after registration failed; secure messaging will require a manual re-login:',
          errorMessage(initError)
        );
      }

      //   2. storeE2EEKeys() failure → warn only, NO clearKeys. Persisting to the
      //      OS keychain lets E2EE survive an app restart (App.tsx session-restore
      //      rehydrates only from stored keys). A persistence failure must NOT
      //      destroy the valid in-memory session from (1) — the current session
      //      still works; only restart-survival is lost. (If (1) failed, getSessionKeys
      //      returns null after clearKeys, so this block no-ops.)
      try {
        const sessionKeys = e2eeService.getSessionKeys();
        if (sessionKeys && globalThis.electron?.storeE2EEKeys) {
          await globalThis.electron.storeE2EEKeys(sessionKeys);
        }
      } catch (storeError) {
        console.warn(
          'Failed to persist E2EE session keys to keychain (E2EE active for this session only):',
          errorMessage(storeError)
        );
      }

      usePendingRegistrationStore.getState().setPending(data);
      onSuccess({ pendingId: data.pending_id, email: data.email });
    } catch (error) {
      console.error('Registration error:', errorMessage(error));
      setErrors({
        general: error instanceof Error ? error.message : 'Registration failed. Please try again.',
      });
    } finally {
      setIsSubmitting(false);
    }
  };

  const handleChange = (field: keyof FormData) => (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.type === 'checkbox' ? e.target.checked : e.target.value;
    setFormData((prev) => ({ ...prev, [field]: value }));
    // Clear error for this field when user starts typing
    if (field in errors && errors[field as keyof FormErrors]) {
      setErrors((prev) => ({ ...prev, [field]: undefined }));
    }
  };

  /** Validate password match when either password field loses focus */
  const validatePasswordMatch = () => {
    if (formData.confirmPassword.length > 0) {
      if (formData.password === formData.confirmPassword) {
        setErrors((prev) => ({ ...prev, confirmPassword: undefined }));
        setPasswordsMatch(true);
      } else {
        setErrors((prev) => ({ ...prev, confirmPassword: 'Passwords do not match' }));
        setPasswordsMatch(false);
      }
    }
  };

  const handleConfirmPasswordBlur = validatePasswordMatch;
  const handlePasswordBlur = validatePasswordMatch;

  return (
    <div className="register-container">
      <div className="register-content">
        <div className="register-header">
          <img
            src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
            className="register-logo"
            alt="Concord Voice"
          />
          <h2 className="register-title">Create Your Account</h2>
          <p className="register-subtitle">
            Join the Concord Voice network with end-to-end encryption
          </p>
        </div>

        {/* SSO entry point — clicking begins the loopback OAuth flow. New
            users coming from Google or Apple (#271) enter the SSO new-user
            wizard via useSSOStore once Task 21 wires AuthFlow. */}
        <div className="register-sso-row">
          <SSOButton provider="google" onClick={() => beginSSO('google')} disabled={isSubmitting} />
          <SSOButton provider="apple" onClick={() => beginSSO('apple')} disabled={isSubmitting} />
        </div>
        <div className="register-divider" role="separator" aria-label="or sign up with email">
          <span className="register-divider__text">or</span>
        </div>

        <form className="register-form" onSubmit={handleSubmit}>
          {/* Email */}
          <div className="form-group">
            <label htmlFor="register-email" className="form-label">
              Email
            </label>
            <input
              id="register-email"
              type="email"
              className={`form-input ${errors.email ? 'error' : ''}`}
              placeholder="you@example.com"
              value={formData.email}
              onChange={handleChange('email')}
              disabled={isSubmitting}
            />
            {errors.email && <span className="form-error">{errors.email}</span>}
          </div>

          {/* Username */}
          <div className="form-group">
            <label htmlFor="register-username" className="form-label">
              Username
            </label>
            <input
              id="register-username"
              type="text"
              className={`form-input ${errors.username ? 'error' : ''}`}
              placeholder="your_username"
              value={formData.username}
              onChange={handleChange('username')}
              disabled={isSubmitting}
            />
            {errors.username && <span className="form-error">{errors.username}</span>}
            <span className="form-hint">
              This will be your identity: {formData.username || 'username'}@concordvoice.chat
            </span>
          </div>

          {/* Password */}
          <div className="form-group">
            <label htmlFor="register-password" className="form-label">
              Password
            </label>
            <input
              id="register-password"
              type="password"
              className={`form-input ${errors.password ? 'error' : ''}`}
              placeholder="Create a strong password"
              value={formData.password}
              onChange={handleChange('password')}
              onBlur={handlePasswordBlur}
              disabled={isSubmitting}
            />
            {errors.password && <span className="form-error">{errors.password}</span>}
            <PasswordStrength password={formData.password} />
          </div>

          {/* Confirm Password */}
          <div className="form-group">
            <label htmlFor="register-confirm-password" className="form-label">
              Confirm Password
            </label>
            <input
              id="register-confirm-password"
              type="password"
              className={`form-input ${errors.confirmPassword ? 'error' : ''} ${passwordsMatch ? 'success' : ''}`}
              placeholder="Confirm your password"
              value={formData.confirmPassword}
              onChange={handleChange('confirmPassword')}
              onBlur={handleConfirmPasswordBlur}
              disabled={isSubmitting}
            />
            {errors.confirmPassword && <span className="form-error">{errors.confirmPassword}</span>}
            {passwordsMatch && formData.confirmPassword.length > 0 && (
              <span className="form-success">✓ Passwords match</span>
            )}
          </div>

          {/* E2EE Info Banner */}
          <div className="form-group">
            <div className="info-banner">
              <div className="info-banner-icon">🔒</div>
              <div className="info-banner-content">
                <div className="info-banner-title">
                  End-to-End Encryption Enabled
                  <InfoTooltip
                    content={
                      <div>
                        <strong>What is E2EE?</strong>
                        <p>
                          End-to-End Encryption means your messages are encrypted on your device
                          before being sent. Only you and the recipient can read them.
                        </p>
                        <strong>How it works:</strong>
                        <ul>
                          <li>
                            <strong>User-to-User & Groups:</strong> Always fully encrypted - maximum
                            protection guaranteed
                          </li>
                          <li>
                            <strong>Servers:</strong> Always fully encrypted - complete privacy
                            protection on every server
                          </li>
                          <li>
                            <strong>Your Keys:</strong> Protected by your password - keep it safe!
                          </li>
                        </ul>
                        <strong>Privacy First:</strong> Not even Concord Voice can read your
                        encrypted messages. We can&apos;t reset your password if you forget it.
                      </div>
                    }
                  />
                </div>
                <p className="info-banner-description">
                  Your account uses DEFCON-grade encryption by default. Your messages are encrypted
                  on your device before sending — not even Concord Voice can read them.
                </p>
              </div>
            </div>
          </div>

          {/* Age Confirmation */}
          <div className="form-group">
            <label className="checkbox-label">
              <input
                type="checkbox"
                checked={formData.ageConfirmed}
                onChange={handleChange('ageConfirmed')}
                disabled={isSubmitting}
              />
              <span className="checkbox-title">I confirm I am at least 16 years old</span>
            </label>
            {errors.ageConfirmed && <span className="form-error">{errors.ageConfirmed}</span>}
          </div>

          {/* General Error */}
          {errors.general && (
            <div className="form-error-banner">
              <span>{errors.general}</span>
            </div>
          )}

          {/* Submit Button */}
          <button type="submit" className="register-submit-btn" disabled={isSubmitting}>
            {isSubmitting ? (
              <>
                Creating Account...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Create Account'
            )}
          </button>

          {/* Back Button */}
          <button
            type="button"
            className="register-back-btn"
            onClick={onBack}
            disabled={isSubmitting}
          >
            ← Back to Connection Options
          </button>
        </form>

        <div className="register-footer">
          <p className="footer-text">
            Already have an account?{' '}
            <button
              className="switch-to-login-btn"
              onClick={onSwitchToLogin}
              disabled={isSubmitting}
            >
              Sign in
            </button>
          </p>
          <p className="footer-text footer-terms">
            By creating an account, you agree to our Terms of Service and Privacy Policy
          </p>
        </div>
      </div>
    </div>
  );
};

export default Register;
