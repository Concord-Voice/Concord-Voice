import React, { useState, useMemo, useEffect } from 'react';
import { unwrapLoginKeys, generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { errorMessage } from '../../utils/redactError';
import { hydratePostLogin } from '../../services/postLoginHydration';
import { useAuthStore } from '../../stores/authStore';
import { API_BASE, apiFetch, ensureMachineId } from '../../services/apiClient';
import { UserProfile } from '../../stores/userStore';
import TOTPInput from './TOTPInput';
import BackupCodeInput from './BackupCodeInput';
import WebAuthnPrompt from './WebAuthnPrompt';
import MFAMethodPicker, {
  getDefaultMethod,
  getAvailableCategories,
  MFAMethodCategory,
} from './MFAMethodPicker';
import LoadingSpinner from './LoadingSpinner';
import { SSOButton } from './SSOButton';
import { useSSOFlow } from '../../hooks/useSSOFlow';
import KeyRecoveryPrompt from './KeyRecoveryPrompt';
import './Login.css';
import './TOTPInput.css';

// Convert base64url-encoded WebAuthn options from the server into ArrayBuffer format
// that navigator.credentials.get() expects.
function base64urlToBuffer(base64url: string): ArrayBuffer {
  const base64 = base64url.replaceAll('-', '+').replaceAll('_', '/');
  const pad = base64.length % 4 === 0 ? '' : '='.repeat(4 - (base64.length % 4));
  const binary = atob(base64 + pad);
  const bytes = new Uint8Array(binary.length);
  for (let i = 0; i < binary.length; i++) bytes[i] = binary.codePointAt(i) ?? 0;
  return bytes.buffer;
}

// eslint-disable-next-line @typescript-eslint/no-explicit-any -- server options arrive as JSON with base64url-encoded buffers; the whole purpose of this helper is to decode those into typed `PublicKeyCredentialRequestOptions` — accepting `any` at the boundary and narrowing field-by-field is the right discipline
function parseWebAuthnOptions(serverOptions: any): PublicKeyCredentialRequestOptions {
  const pk = serverOptions.publicKey || serverOptions;
  const opts: PublicKeyCredentialRequestOptions = {
    challenge: base64urlToBuffer(pk.challenge),
    timeout: pk.timeout,
    rpId: pk.rpId,
  };
  if (pk.allowCredentials) {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any -- same rationale as the enclosing `parseWebAuthnOptions` — server-side JSON credentials are narrowed here into typed `PublicKeyCredentialDescriptor` entries
    opts.allowCredentials = pk.allowCredentials.map((cred: any) => ({
      type: cred.type,
      id: base64urlToBuffer(cred.id),
      transports: cred.transports,
    }));
  }
  if (pk.userVerification) {
    opts.userVerification = pk.userVerification;
  }
  return opts;
}

/** Check that Electron safeStorage is available, returning an error message or null */
async function checkSafeStorage(): Promise<string | null> {
  if (!globalThis.electron?.checkPermission) return null;
  try {
    const status = await globalThis.electron.checkPermission('secureStorage');
    if (status !== 'granted') {
      return 'Secure storage is unavailable. Concord requires keychain / credential manager access to safely store authentication tokens and encryption keys. Please enable it and restart the app.';
    }
    return null;
  } catch {
    return 'Secure storage could not be verified. Please try again, and if the problem persists, restart the app.';
  }
}

export interface LoginProps {
  onBack: () => void;
  onSuccess: (data: { accessToken: string; user?: UserProfile; rememberMe: boolean }) => void;
  onSwitchToRegister: () => void;
  onForgotPassword: () => void;
}

interface FormData {
  email: string;
  password: string;
  rememberMe: boolean;
}

interface FormErrors {
  email?: string;
  password?: string;
  general?: string;
}

const Login: React.FC<LoginProps> = ({
  onBack,
  onSuccess,
  onSwitchToRegister,
  onForgotPassword,
}) => {
  const [formData, setFormData] = useState<FormData>({
    email: '',
    password: '',
    rememberMe: false,
  });

  const [errors, setErrors] = useState<FormErrors>({});
  const [isSubmitting, setIsSubmitting] = useState(false);

  // SSO state — populated when the server returns 403 account_uses_sso. The
  // password form is replaced with a list of SSO-only buttons so the user is
  // never stranded with credentials they can't actually use.
  const [ssoOnlyProviders, setSsoOnlyProviders] = useState<string[] | null>(null);
  const { begin: beginSSO } = useSSOFlow();

  // MFA state
  const [mfaRequired, setMfaRequired] = useState(false);
  const [mfaChallengeToken, setMfaChallengeToken] = useState('');
  const [mfaMethods, setMfaMethods] = useState<string[]>([]);
  const [mfaMode, setMfaMode] = useState<MFAMethodCategory | 'method-select'>('totp');
  const [mfaRecoveryOnly, setMfaRecoveryOnly] = useState<string[]>([]);
  const [mfaError, setMfaError] = useState('');
  const [webauthnOptions, setWebauthnOptions] = useState<PublicKeyCredentialRequestOptions | null>(
    null
  );

  // Promise-deferred key-recovery prompt: the login catch awaits the user's
  // decision before resetting keys (consented data loss) or aborting (#1293).
  // The decision carries an optional MFA code for the step-up re-auth retry.
  const [keyRecoveryResolver, setKeyRecoveryResolver] = useState<
    ((decision: { action: 'reset' | 'cancel'; mfaCode?: string }) => void) | null
  >(null);
  // When the server demands MFA for the destructive reset, the prompt re-opens
  // in MFA-entry mode.
  const [keyRecoveryMfaRequired, setKeyRecoveryMfaRequired] = useState(false);

  // The double-arrow is load-bearing: setKeyRecoveryResolver(() => resolve)
  // STORES `resolve` in state. Without it React would CALL `resolve` as a
  // state-updater. Do NOT "simplify" to setKeyRecoveryResolver(resolve).
  const promptKeyRecovery = (): Promise<{ action: 'reset' | 'cancel'; mfaCode?: string }> =>
    new Promise((resolve) => setKeyRecoveryResolver(() => resolve));

  // Non-destructive abort: clear the early-set token so there is no
  // half-authenticated state, and surface guidance on the login screen (#1293).
  const abortKeyRecovery = () => {
    useAuthStore.getState().clearAccessToken();
    setErrors({
      general:
        'Your encryption keys couldn’t be recovered on this device. You can try again, or recover your account on a device that still has your keys.',
    });
    setIsSubmitting(false);
  };

  // If Login unmounts while the prompt is open, settle the pending promise to
  // 'cancel' so the suspended login flow doesn't leak a never-collected closure
  // holding the password + tokens (code-review hardening, #1293).
  useEffect(() => {
    return () => {
      keyRecoveryResolver?.({ action: 'cancel' });
    };
  }, [keyRecoveryResolver]);

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};

    // Email validation
    const emailRegex = /^[^\s@]+@[^\s@.]+(?:\.[^\s@.]+)+$/;
    if (!formData.email) {
      newErrors.email = 'Email is required';
    } else if (!emailRegex.test(formData.email)) {
      newErrors.email = 'Invalid email format';
    }

    // Password validation
    if (!formData.password) {
      newErrors.password = 'Password is required';
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

    // Fail-closed safeStorage enforcement (#197)
    const storageError = await checkSafeStorage();
    if (storageError) {
      setErrors({ general: storageError });
      return;
    }

    setIsSubmitting(true);

    try {
      console.debug('Logging in...');

      const machineId = await ensureMachineId();

      // Login with backend
      const response = await fetch(`${API_BASE}/api/v1/auth/login`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include', // Include cookies for refresh token
        body: JSON.stringify({
          email: formData.email,
          password: formData.password,
          remember_me: formData.rememberMe,
        }),
      });

      const data = await response.json();

      // SSO short-circuits — see helper closures below for the per-status
      // routing (403 account_uses_sso, 500 sso_provider_lookup_failed,
      // 500 sso_account_misconfigured). Extracted so handleSubmit stays under
      // SonarQube's S3776 cognitive-complexity threshold.
      if (tryApplyAccountUsesSSO(response, data)) return;
      if (tryApplySSO500Error(response, data)) return;

      if (!response.ok) {
        throw new Error(data.error || 'Login failed');
      }

      // Check if MFA is required
      if (data.mfa_required) {
        applyMfaRequiredFromResponse(data);
        return;
      }

      await completeLoginFromResponse(data);
    } catch (error) {
      console.error('Login error:', errorMessage(error));
      setErrors({
        general: error instanceof Error ? error.message : 'Login failed. Please try again.',
      });
    } finally {
      setIsSubmitting(false);
    }
  };

  // 403 account_uses_sso short-circuit: backend signals the account is SSO-only
  // (password login disabled, ≥1 identity provider linked). Swap the form for
  // the SSO-only view rather than showing a generic error. Lockout counters
  // are NOT engaged on this branch — see services/control-plane/internal/auth/
  // handlers.go (PasswordLoginDisabled handling). Returns true if handled.
  // Extracted from handleSubmit to stay under S3776 (the inlined ternary on
  // data.providers contributed nesting penalty). Behavior unchanged.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- server payload is loose JSON; providers narrowed via Array.isArray below.
  const tryApplyAccountUsesSSO = (response: Response, data: any): boolean => {
    if (response.status !== 403) return false;
    if (data?.error_code !== 'account_uses_sso') return false;
    const providers = Array.isArray(data.providers) ? (data.providers as string[]) : [];
    setSsoOnlyProviders(providers);
    setIsSubmitting(false);
    return true;
  };

  // Map a 500 SSO short-circuit response to the appropriate user-facing
  // message and apply it. Returns true if the response was handled (caller
  // should `return` immediately) or false if not a known SSO 500 case (caller
  // should fall through). Extracted from handleSubmit so that handler stays
  // under SonarQube's S3776 cognitive-complexity threshold; behavior is
  // unchanged. See the inline comment at the call site for the two error_codes
  // and their UX rationale.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- server error payload is loose JSON; we narrow on error_code field-by-field below.
  const tryApplySSO500Error = (response: Response, data: any): boolean => {
    if (response.status !== 500) return false;
    const code = data?.error_code as string | undefined;
    let message: string | null = null;
    if (code === 'sso_provider_lookup_failed') {
      message =
        "We couldn't load your sign-in options. Please try again in a moment, or contact support if this continues.";
    } else if (code === 'sso_account_misconfigured') {
      message =
        "This account isn't fully set up for sign-in. Please contact support — error code: SSO_MISCONFIG.";
    }
    if (message === null) return false;
    setErrors({ general: message });
    setIsSubmitting(false);
    return true;
  };

  // Surface MFA challenge to the user when the server returns mfa_required.
  // Extracted from handleSubmit so that handler stays under SonarQube's S3776
  // cognitive-complexity threshold; behavior is unchanged.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- server MFA payload shape (methods, recovery_only_methods, webauthn_options) is the same loose JSON the parent handler accepted; narrowing is done field-by-field inside.
  const applyMfaRequiredFromResponse = (data: any) => {
    const serverMethods = data.methods || [];
    const recoveryOnly = data.recovery_only_methods || [];
    setMfaChallengeToken(data.mfa_challenge_token);
    setMfaMethods(serverMethods);
    setMfaRecoveryOnly(recoveryOnly);
    setMfaMode(getDefaultMethod(serverMethods, recoveryOnly));
    if (data.webauthn_options) setWebauthnOptions(parseWebAuthnOptions(data.webauthn_options));
    setMfaRequired(true);
    setIsSubmitting(false);
  };

  // Drives the consented key-recovery flow when the login unwrap fails: prompts
  // for consent, performs the step-up-authenticated reset (current password +
  // MFA retry), and returns true if the keys were reset (continue login) or
  // false if the user aborted. Throws on reset failure (the caller surfaces the
  // error; the token is cleared here first). Extracted from
  // completeLoginFromResponse to keep that function under the cognitive-
  // complexity threshold (#1293).
  const handleKeyRecovery = async (): Promise<boolean> => {
    const decision = await promptKeyRecovery();
    setKeyRecoveryResolver(null);

    if (decision.action === 'cancel') {
      abortKeyRecovery();
      return false;
    }

    // The access token set at login authenticates the reset PUT. The reset is a
    // destructive, step-up-authenticated operation, so it sends the current
    // password (already in hand from the login form) and, when the server
    // demands it, an MFA code. On ANY failure, clear the early-set token then
    // rethrow so a failed reset cannot strand the user in a half-authenticated
    // state (the cancel branch's invariant — spec §3.3.4).
    try {
      const newKeys = await generateRegistrationKeys(formData.password);
      const publicKeyB64 = await exportPublicKey(newKeys.publicKey);
      const sendReset = (mfaCode?: string) =>
        apiFetch('/api/v1/users/me/keys', {
          method: 'PUT',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({
            wrapped_private_key: newKeys.wrappedPrivateKey,
            key_derivation_salt: newKeys.keyDerivationSalt,
            key_derivation_alg: newKeys.keyDerivationAlg,
            public_key: publicKeyB64,
            current_password: formData.password,
            mfa_code: mfaCode,
            acknowledge_data_loss: true,
          }),
        });

      let replaceRes = await sendReset(decision.mfaCode);

      // Step-up MFA: if the server requires an MFA code, re-open the prompt in
      // MFA-entry mode and retry once with the supplied code.
      if (replaceRes.status === 403) {
        const body = await replaceRes.json().catch(() => ({}));
        if (body?.error === 'mfa_required') {
          setKeyRecoveryMfaRequired(true);
          const mfaDecision = await promptKeyRecovery();
          setKeyRecoveryResolver(null);
          setKeyRecoveryMfaRequired(false);
          if (mfaDecision.action === 'cancel') {
            abortKeyRecovery();
            return false;
          }
          replaceRes = await sendReset(mfaDecision.mfaCode);
        }
      }

      if (!replaceRes.ok) throw new Error('Failed to reset encryption keys. Please try again.');

      await e2eeService.initialize(
        formData.password,
        newKeys.wrappedPrivateKey,
        newKeys.keyDerivationSalt,
        newKeys.keyDerivationAlg
      );
      console.debug('E2EE keys reset and service initialized!');
      return true;
    } catch (resetError) {
      useAuthStore.getState().clearAccessToken();
      throw resetError;
    }
  };

  // Complete login after receiving tokens (shared between direct login and MFA verify)
  // eslint-disable-next-line @typescript-eslint/no-explicit-any -- server login payload shape varies across direct-login and MFA-verify endpoints (different e2ee_keys wrapping, optional session_id); field-by-field narrowing inside the function is clearer than a 30-line union type
  const completeLoginFromResponse = async (data: any) => {
    console.debug('Login successful, unwrapping private key...');

    // Set access token early so e2eeService can make authenticated API calls (e.g., key migration)
    useAuthStore.getState().setAccessToken(data.access_token);
    if (data.session_id) useAuthStore.getState().setSessionId(data.session_id);

    const kdAlg = data.e2ee_keys.key_derivation_alg || 'pbkdf2';
    try {
      await unwrapLoginKeys(
        formData.password,
        data.e2ee_keys.wrapped_private_key,
        data.e2ee_keys.key_derivation_salt,
        kdAlg
      );
      // e2eeService.initialize handles PBKDF2→Argon2id migration automatically
      await e2eeService.initialize(
        formData.password,
        data.e2ee_keys.wrapped_private_key,
        data.e2ee_keys.key_derivation_salt,
        kdAlg
      );
      console.debug('Private key unwrapped and E2EE service initialized!');
    } catch (unwrapError) {
      console.warn(
        'Key unwrap failed; prompting for consented key reset',
        errorMessage(unwrapError)
      );
      const recovered = await handleKeyRecovery();
      if (!recovered) return;
    }

    if (globalThis.electron?.storeRefreshToken) {
      await globalThis.electron.storeRefreshToken({
        refreshToken: data.refresh_token,
        rememberMe: data.remember_me ?? formData.rememberMe,
        apiBase: API_BASE,
        accessToken: data.access_token,
      });
    }
    useAuthStore.getState().setAccessToken(data.access_token);
    if (data.session_id) useAuthStore.getState().setSessionId(data.session_id);

    const sessionKeys = e2eeService.getSessionKeys();
    if (sessionKeys && globalThis.electron?.storeE2EEKeys) {
      await globalThis.electron.storeE2EEKeys(sessionKeys);
    }

    // Hydrate all post-login user state — preferences, saved GIFs, notification
    // mute prefs, and the entitlement capability set. Extracted to the shared
    // helper (#1297) so SSO and session-restore hydrate identically; failures
    // of individual steps are non-fatal (each swallows its own network blips).
    await hydratePostLogin();

    onSuccess({
      accessToken: data.access_token,
      user: data.user,
      rememberMe: data.remember_me ?? formData.rememberMe,
    });
  };

  const handleMFAVerify = async (code: string, method: string) => {
    setIsSubmitting(true);
    setMfaError('');
    try {
      const machineId = await ensureMachineId();
      const res = await fetch(`${API_BASE}/api/v1/auth/mfa/verify`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include',
        body: JSON.stringify({
          mfa_challenge_token: mfaChallengeToken,
          method,
          code,
        }),
      });

      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Verification failed');

      // MFA verify returns full login response (tokens + user + keys)
      await completeLoginFromResponse(data);
    } catch (err) {
      setMfaError(err instanceof Error ? err.message : 'Verification failed');
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

  const available = useMemo(
    () => getAvailableCategories(mfaMethods, mfaRecoveryOnly),
    [mfaMethods, mfaRecoveryOnly]
  );
  const hasMultipleMethods = available.length > 1;
  const defaultMethod = useMemo(
    () => getDefaultMethod(mfaMethods, mfaRecoveryOnly),
    [mfaMethods, mfaRecoveryOnly]
  );

  const handleWebAuthnSuccess = async (credential: Credential) => {
    const pkc = credential as PublicKeyCredential;
    const response = pkc.response as AuthenticatorAssertionResponse;

    // Encode ArrayBuffers as base64url (no padding) for the go-webauthn library
    const toBase64url = (buf: ArrayBuffer): string => {
      const bytes = new Uint8Array(buf);
      let binary = '';
      for (const byte of bytes) binary += String.fromCodePoint(byte);
      return btoa(binary).replaceAll('+', '-').replaceAll('/', '_').replaceAll(/=+$/g, '');
    };

    const assertion = {
      id: pkc.id,
      rawId: toBase64url(pkc.rawId),
      type: pkc.type,
      response: {
        authenticatorData: toBase64url(response.authenticatorData),
        clientDataJSON: toBase64url(response.clientDataJSON),
        signature: toBase64url(response.signature),
        userHandle: response.userHandle ? toBase64url(response.userHandle) : null,
      },
    };

    setIsSubmitting(true);
    setMfaError('');
    try {
      const machineId = await ensureMachineId();
      const res = await fetch(`${API_BASE}/api/v1/auth/mfa/verify`, {
        method: 'POST',
        headers: {
          'Content-Type': 'application/json',
          ...(machineId ? { 'X-Machine-Id': machineId } : {}),
        },
        credentials: 'include',
        body: JSON.stringify({
          mfa_challenge_token: mfaChallengeToken,
          method: 'webauthn',
          assertion,
        }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Verification failed');
      await completeLoginFromResponse(data);
    } catch (err) {
      setMfaError(err instanceof Error ? err.message : 'Verification failed');
    } finally {
      setIsSubmitting(false);
    }
  };

  let mfaSubtitle: string;
  if (mfaMode === 'totp') mfaSubtitle = 'Enter the 6-digit code from your authenticator app';
  else if (mfaMode === 'backup') mfaSubtitle = 'Enter one of your backup codes';
  else if (mfaMode === 'webauthn') mfaSubtitle = 'Use your security key or biometrics';
  else if (mfaMode === 'email-sms') mfaSubtitle = 'Enter the verification code sent to you';
  else mfaSubtitle = 'Select a verification method';

  // MFA Step UI
  if (mfaRequired) {
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <img
              src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
              className="login-logo"
              alt="Concord Voice"
            />
            <h2 className="login-title">Two-Factor Authentication</h2>
            <p className="login-subtitle">{mfaSubtitle}</p>
          </div>

          <div className="login-form">
            {mfaMode === 'method-select' && (
              <MFAMethodPicker
                methods={mfaMethods}
                excludeMethods={mfaRecoveryOnly}
                currentMethod={defaultMethod}
                onSelect={(method) => {
                  setMfaMode(method);
                  setMfaError('');
                }}
                onCancel={() => {
                  setMfaMode(defaultMethod);
                  setMfaError('');
                }}
              />
            )}

            {mfaMode === 'totp' && (
              <TOTPInput
                onSubmit={(code) => handleMFAVerify(code, 'totp')}
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {mfaMode === 'backup' && (
              <BackupCodeInput
                onSubmit={(code) => handleMFAVerify(code, 'backup_code')}
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {mfaMode === 'webauthn' && (
              <>
                {webauthnOptions ? (
                  <WebAuthnPrompt
                    requestOptions={webauthnOptions}
                    onSuccess={handleWebAuthnSuccess}
                    onError={(msg) => setMfaError(msg)}
                    onCancel={() => {
                      setMfaMode('method-select');
                      setMfaError('');
                    }}
                  />
                ) : (
                  <div style={{ textAlign: 'center' }}>
                    <p style={{ color: 'var(--text-secondary, #bbb)', fontSize: 14 }}>
                      WebAuthn verification will be triggered by the server challenge.
                    </p>
                    {mfaError && <p className="totp-error">{mfaError}</p>}
                  </div>
                )}
              </>
            )}

            {mfaMode === 'email-sms' && (
              <TOTPInput
                onSubmit={(code) =>
                  handleMFAVerify(code, mfaMethods.includes('email') ? 'email' : 'sms')
                }
                disabled={isSubmitting}
                error={mfaError}
              />
            )}

            {/* "Choose another form" link */}
            {mfaMode !== 'method-select' && hasMultipleMethods && (
              <button
                type="button"
                className="mfa-choose-another"
                onClick={() => {
                  setMfaMode('method-select');
                  setMfaError('');
                }}
                disabled={isSubmitting}
              >
                Choose another form of verification
              </button>
            )}

            {isSubmitting && (
              <div style={{ display: 'flex', justifyContent: 'center', marginTop: 12 }}>
                <LoadingSpinner size="small" inline />
              </div>
            )}

            <button
              type="button"
              className="login-back-btn"
              onClick={() => {
                setMfaRequired(false);
                setMfaError('');
              }}
              disabled={isSubmitting}
            >
              ← Back to login
            </button>
          </div>
        </div>
      </div>
    );
  }

  // SSO-only account: the user logged in with a password some time ago, but
  // their account is now linked to one or more SSO providers and password
  // sign-in is disabled. Show the relevant provider button(s) instead of the
  // password form.
  if (ssoOnlyProviders) {
    // Empty providers array is the impossible-state case: backend says
    // password is disabled, but listSSOProviders returned no rows. Render
    // an explicit error message rather than an empty page that looks like
    // a UI bug.
    const isEmpty = ssoOnlyProviders.length === 0;
    return (
      <div className="login-container">
        <div className="login-content">
          <div className="login-header">
            <img
              src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
              className="login-logo"
              alt="Concord Voice"
            />
            <h2 className="login-title">Welcome Back</h2>
            <p className="login-subtitle">
              {isEmpty
                ? "We couldn't load your sign-in options. Please try again in a moment or contact support."
                : 'This account uses Single Sign-On. Continue with the provider you originally linked.'}
            </p>
          </div>

          <div className="login-form login-form--sso-only">
            {!isEmpty && ssoOnlyProviders.includes('google') && (
              <SSOButton provider="google" onClick={() => beginSSO('google')} />
            )}
            {!isEmpty && ssoOnlyProviders.includes('apple') && (
              <SSOButton provider="apple" onClick={() => beginSSO('apple')} />
            )}

            <button
              type="button"
              className="login-back-btn"
              onClick={() => setSsoOnlyProviders(null)}
            >
              ← Back to login
            </button>
          </div>
        </div>
      </div>
    );
  }

  return (
    <div className="login-container">
      {keyRecoveryResolver && (
        <KeyRecoveryPrompt
          mfaRequired={keyRecoveryMfaRequired}
          onReset={(mfaCode) => keyRecoveryResolver({ action: 'reset', mfaCode })}
          onCancel={() => keyRecoveryResolver({ action: 'cancel' })}
        />
      )}
      <div className="login-content">
        <div className="login-header">
          <img
            src="./branding/Concord-Voice/logos/main-logo-transparent-vector.svg"
            className="login-logo"
            alt="Concord Voice"
          />
          <h2 className="login-title">Welcome Back</h2>
          <p className="login-subtitle">Sign in to your Concord Voice account</p>
        </div>

        {/* SSO entry point — sits above the password form. Clicking begins the
            loopback OAuth flow; subsequent UI is driven by useSSOStore state
            (rendered by AuthFlow once Task 21 wires it). Apple SSO (#271)
            lives next to Google to satisfy App Store policy parity (mobile
            clients #205 require both when either is offered). */}
        <div className="login-sso-row">
          <SSOButton provider="google" onClick={() => beginSSO('google')} disabled={isSubmitting} />
          <SSOButton provider="apple" onClick={() => beginSSO('apple')} disabled={isSubmitting} />
        </div>
        <div className="login-divider" role="separator" aria-label="or sign in with email">
          <span className="login-divider__text">or</span>
        </div>

        <form className="login-form" onSubmit={handleSubmit}>
          {/* Email */}
          <div className="form-group">
            <label htmlFor="login-email" className="form-label">
              Email
            </label>
            <input
              id="login-email"
              type="email"
              className={`form-input ${errors.email ? 'error' : ''}`}
              placeholder="you@example.com"
              value={formData.email}
              onChange={handleChange('email')}
              disabled={isSubmitting}
              autoFocus
            />
            {errors.email && <span className="form-error">{errors.email}</span>}
          </div>

          {/* Password */}
          <div className="form-group">
            <label htmlFor="login-password" className="form-label">
              Password
            </label>
            <input
              id="login-password"
              type="password"
              className={`form-input ${errors.password ? 'error' : ''}`}
              placeholder="Enter your password"
              value={formData.password}
              onChange={handleChange('password')}
              disabled={isSubmitting}
            />
            {errors.password && <span className="form-error">{errors.password}</span>}
          </div>

          {/* Remember Me & Forgot Password */}
          <div className="login-options">
            <label className="remember-me-label">
              <input
                type="checkbox"
                checked={formData.rememberMe}
                onChange={handleChange('rememberMe')}
                disabled={isSubmitting}
              />
              <span>Remember me</span>
            </label>
            <button
              type="button"
              className="forgot-password-link"
              onClick={onForgotPassword}
              disabled={isSubmitting}
            >
              Forgot password?
            </button>
          </div>

          {/* General Error */}
          {errors.general && (
            <div className="form-error-banner">
              <span>{errors.general}</span>
            </div>
          )}

          {/* Submit Button */}
          <button type="submit" className="login-submit-btn" disabled={isSubmitting}>
            {isSubmitting ? (
              <>
                Signing In...
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Sign In'
            )}
          </button>

          {/* Back Button */}
          <button type="button" className="login-back-btn" onClick={onBack} disabled={isSubmitting}>
            ← Back to Connection Options
          </button>
        </form>

        <div className="login-footer">
          <p className="footer-text">
            Don&apos;t have an account?{' '}
            <button
              className="switch-to-register-btn"
              onClick={onSwitchToRegister}
              disabled={isSubmitting}
            >
              Create one
            </button>
          </p>
        </div>
      </div>
    </div>
  );
};

export default Login;
