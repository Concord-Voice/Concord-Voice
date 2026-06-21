/**
 * SSOPassphraseSetup — post-OAuth wizard for first-time SSO users (#270, Task 17).
 *
 * Reached when the SSO callback returns `sso_registration_required`. This
 * component collects the user's chosen username + passphrase, generates an
 * RSA-4096 keypair, derives an Argon2id wrap-key from the passphrase, wraps
 * the private key with AES-256-GCM, and submits the wrapped key material to
 * `POST /api/v1/auth/sso/{provider}/complete-registration`.
 *
 * The user-facing copy uses the word **"passphrase"** (not "password") to
 * nudge the vault-unlock mental model: this string protects the encrypted
 * private key, distinct from the SSO identity provider's authentication.
 *
 * Crypto composition uses the same `generateRegistrationKeys` /
 * `exportPublicKey` primitives that traditional password-based registration
 * uses (see `Register.tsx`) — SSO and email registration produce identical
 * E2EE key material; only the auth path differs.
 *
 * Defensive render: returns null when ssoStore phase !== 'register_required'
 * so this component is safe to mount unconditionally inside an AuthFlow that
 * hasn't yet routed to it (Task 21 wires AuthFlow).
 */

import React, { useState } from 'react';
import PasswordStrength from './PasswordStrength';
import LoadingSpinner from './LoadingSpinner';
import { useSSOStore } from '../../stores/ssoStore';
import { useAuthStore } from '../../stores/authStore';
import { completeSSORegistration, SSOServiceError } from '../../services/ssoService';
import { generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { errorMessage } from '../../utils/redactError';
import './SSOPassphraseSetup.css';

interface CompleteRegistrationErrorBody {
  error_code?: string;
  error?: string;
  detail?: string;
}

/**
 * Map a server error response to a friendly inline message. Specifically
 * handles 409 conflict cases (username/email already taken) so users see
 * actionable copy instead of a generic "registration failed."
 */
function mapServerErrorToMessage(status: number, body: CompleteRegistrationErrorBody): string {
  if (status === 409) {
    switch (body.error_code) {
      case 'username_taken':
        return 'This username is already taken. Try another.';
      case 'email_taken':
        return 'An account with this email already exists. Try linking instead.';
      default:
        return body.detail ?? body.error ?? 'Registration conflict. Please try again.';
    }
  }
  if (status === 400) {
    if (body.error_code === 'invalid_username' || body.error_code === 'invalid_password') {
      return body.detail ?? 'The username or passphrase is invalid.';
    }
  }
  return body.error ?? body.detail ?? 'Registration failed. Please try again.';
}

const SSOPassphraseSetup: React.FC = () => {
  const state = useSSOStore((s) => s.state);
  const setSSOState = useSSOStore((s) => s.setState);
  const setAccessToken = useAuthStore((s) => s.setAccessToken);

  const [username, setUsername] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Defensive: only render in the register_required phase. Routing into this
  // component happens in AuthFlow (Task 21); a wrong-state render would imply
  // a bug, but null-render is the safest fallback.
  if (state.phase !== 'register_required') {
    return null;
  }

  const passphraseStrong = passphrase.length >= 12;
  const matches = passphrase === confirm && passphrase.length > 0;
  const usernameValid = username.length >= 3;
  const valid = passphraseStrong && matches && usernameValid;

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!valid || submitting) {
      return;
    }
    setSubmitting(true);
    setError(null);

    try {
      // Generate E2EE key material from the user's passphrase.
      // Reuses the same primitives traditional Register.tsx uses — the SSO
      // path produces identical key material; only the auth flow differs.
      const keys = await generateRegistrationKeys(passphrase);
      const publicKeyBase64 = await exportPublicKey(keys.publicKey);

      try {
        const { accessToken } = await completeSSORegistration({
          provider: state.provider,
          ssoToken: state.ssoToken,
          username,
          passphrase,
          wrappedPrivateKey: keys.wrappedPrivateKey,
          keyDerivationSalt: keys.keyDerivationSalt,
          publicKey: publicKeyBase64,
        });
        setAccessToken(accessToken);

        // Initialize e2eeService so E2EE is live for THIS session — mirrors the
        // post-registration init added to Register.tsx in #1278. Without this, an
        // SSO user who just set their passphrase has e2eeService.isInitialized ===
        // false and is blocked from creating channels / sending messages ("Setting
        // up secure messaging — try again in a moment.") until a logout→login. The
        // keys were generated above and the server now holds the wrapped private
        // key, so it is safe to establish the session here.
        //
        // The two failure modes are handled SEPARATELY (per #1278 review):
        //   1. initialize() failure → clearKeys(). finalizeKeys assigns wrappingKey
        //      before later steps, so a mid-init throw could leave isInitialized
        //      === true on a half-initialized service; rolling back makes it
        //      honestly false. Non-fatal: SSO registration already succeeded
        //      server-side, so a client-side hiccup must not fail the flow.
        try {
          await e2eeService.initialize(
            passphrase,
            keys.wrappedPrivateKey,
            keys.keyDerivationSalt,
            keys.keyDerivationAlg
          );
        } catch (initError) {
          e2eeService.clearKeys();
          console.warn(
            'E2EE init after SSO passphrase setup failed; secure messaging will require a manual re-login:',
            errorMessage(initError)
          );
        }

        //   2. storeE2EEKeys() failure → warn only, NO clearKeys. Persisting to the
        //      OS keychain lets E2EE survive an app restart (App.tsx session-restore
        //      rehydrates only from stored keys). A persistence failure must NOT
        //      destroy the valid in-memory session from (1). (If (1) failed,
        //      getSessionKeys returns null after clearKeys, so this block no-ops.)
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

        // Returning to phase 'idle' lets AuthFlow re-evaluate based on the
        // new accessToken in authStore (the user is now logged in).
        setSSOState({ phase: 'idle' });
      } catch (err) {
        // ssoService throws SSOServiceError carrying the parsed response body.
        // Use it to map error_code → user-facing copy. Falling back to a
        // synthetic body lets older paths (or unparseable responses) still
        // surface a coherent message rather than going silent.
        if (err instanceof SSOServiceError) {
          const body = (err.body ?? {}) as CompleteRegistrationErrorBody;
          setError(mapServerErrorToMessage(err.status, body));
        } else {
          setError('Registration failed. Please try again.');
        }
        setSubmitting(false);
      }
    } catch {
      // Crypto-error catch only — never log key material.
      setError('Could not prepare encryption keys. Please try again.');
      setSubmitting(false);
    }
  };

  const firstName = state.name?.split(' ')[0];

  return (
    <div className="sso-passphrase-setup">
      <form onSubmit={handleSubmit} className="sso-passphrase-setup__form" noValidate>
        <h2 className="sso-passphrase-setup__title">
          {firstName ? `Welcome to Concord, ${firstName}` : 'Welcome to Concord'}
        </h2>
        <p className="sso-passphrase-setup__intro">
          Signing in as <strong>{state.email}</strong>. Create a passphrase to protect your
          encrypted messages — even we can&apos;t read them without it. You can still sign in with
          Google anytime.
        </p>

        <div className="sso-passphrase-setup__field">
          <label htmlFor="sso-username" className="sso-passphrase-setup__label">
            Username
          </label>
          <input
            id="sso-username"
            type="text"
            className="sso-passphrase-setup__input"
            value={username}
            onChange={(ev) => setUsername(ev.target.value)}
            autoComplete="username"
            disabled={submitting}
            placeholder="your_username"
          />
          <span className="sso-passphrase-setup__hint">
            This will be your identity: {username || 'username'}@example.com
          </span>
        </div>

        <div className="sso-passphrase-setup__field">
          <label htmlFor="sso-passphrase" className="sso-passphrase-setup__label">
            Passphrase
          </label>
          <input
            id="sso-passphrase"
            type="password"
            className="sso-passphrase-setup__input"
            value={passphrase}
            onChange={(ev) => setPassphrase(ev.target.value)}
            autoComplete="new-password"
            disabled={submitting}
            placeholder="At least 12 characters"
          />
          <PasswordStrength password={passphrase} />
        </div>

        <div className="sso-passphrase-setup__field">
          <label htmlFor="sso-confirm-passphrase" className="sso-passphrase-setup__label">
            Confirm passphrase
          </label>
          <input
            id="sso-confirm-passphrase"
            type="password"
            className="sso-passphrase-setup__input"
            value={confirm}
            onChange={(ev) => setConfirm(ev.target.value)}
            autoComplete="new-password"
            disabled={submitting}
            placeholder="Re-enter your passphrase"
          />
          {confirm.length > 0 && matches && (
            <span className="sso-passphrase-setup__success">✓ Passphrases match</span>
          )}
          {confirm.length > 0 && !matches && (
            <span className="sso-passphrase-setup__field-error">Passphrases do not match</span>
          )}
        </div>

        {error && (
          <div className="sso-passphrase-setup__error" role="alert">
            {error}
          </div>
        )}

        <button
          type="submit"
          className="sso-passphrase-setup__submit"
          disabled={!valid || submitting}
        >
          {submitting ? (
            <>
              Creating account…
              <LoadingSpinner size="small" inline />
            </>
          ) : (
            'Create account'
          )}
        </button>
      </form>
    </div>
  );
};

export default SSOPassphraseSetup;
