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
import { useE2EEStore } from '../../stores/e2eeStore';
import { completeSSORegistration, SSOServiceError } from '../../services/ssoService';
import { revokeAbortedSession, type AbortedSessionRef } from '../../services/apiClient';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
} from '../../services/runtimeServerBase';
import { useSSOFlow } from '../../hooks/useSSOFlow';
import { generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { E2EEInitTeardownError } from '../../services/e2eeErrors';
import { errorMessage } from '../../utils/redactError';
import { persistE2EESessionKeys } from '../../utils/persistE2EESessionKeys';
import type { CredentialOwner } from '../../../main/ipcContract';
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
function mapServerErrorToMessage(
  status: number,
  body: CompleteRegistrationErrorBody,
  providerName: string
): string {
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
  if (status === 401 && body.error_code === 'sso_token_invalid') {
    // The short-lived sso_token expired (or was otherwise consumed) before the
    // user finished this screen. Actionable copy + a re-initiate affordance
    // (see the component) replace the old generic dead-end (#2045). Provider-aware
    // so an Apple registrant is not told to "sign in with Google."
    return `Your sign-in session expired. Please sign in with ${providerName} again.`;
  }
  return body.error ?? body.detail ?? 'Registration failed. Please try again.';
}

const SSOPassphraseSetup: React.FC = () => {
  const state = useSSOStore((s) => s.state);
  const setSSOState = useSSOStore((s) => s.setState);
  // begin(provider) re-mints a fresh sso_token via the full OAuth round-trip —
  // the recovery path when the current token has expired (#2045).
  const { begin } = useSSOFlow();

  const [username, setUsername] = useState('');
  const [passphrase, setPassphrase] = useState('');
  const [confirm, setConfirm] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const [tokenExpired, setTokenExpired] = useState(false);

  // Defensive: only render in the register_required phase. Routing into this
  // component happens in AuthFlow (Task 21); a wrong-state render would imply
  // a bug, but null-render is the safest fallback.
  if (state.phase !== 'register_required') {
    return null;
  }

  // Display name for the expiry copy + re-initiate button + intro line. This
  // screen serves BOTH Google and Apple SSO (SSOProvider = 'google' | 'apple'),
  // so the recovery/intro copy must not hardcode a provider (#2045 review).
  const providerName = state.provider === 'apple' ? 'Apple' : 'Google';

  const passphraseStrong = passphrase.length >= 12;
  const matches = passphrase === confirm && passphrase.length > 0;
  const usernameValid = username.length >= 3;
  const valid = passphraseStrong && matches && usernameValid;

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!valid || submitting) {
      return;
    }
    const requestSelection = captureRuntimeServerSelection();
    const requestAuthGeneration = useAuthStore.getState().authGeneration;
    const requestIsCurrent = () =>
      runtimeServerSelectionIsCurrent(requestSelection) &&
      useAuthStore.getState().authGeneration === requestAuthGeneration;
    setSubmitting(true);
    setError(null);
    setTokenExpired(false);

    let teardownEpoch = e2eeService.captureTeardownEpoch();
    let keyMaterialPrepared = false;
    let issuedSession: AbortedSessionRef | null = null;
    let credentialOwner: CredentialOwner | null = null;
    let needsSSOUnlock = false;
    let admittedGeneration: number | null = null;
    let flowInitializationReceipt: ReturnType<typeof e2eeService.captureInitializationReceipt> =
      null;

    const clearFlowSessionKeys = () => {
      e2eeService.clearKeysIfInitializationCurrent(flowInitializationReceipt);
    };
    const cleanupIssuedSession = async () => {
      clearFlowSessionKeys();
      const auth = useAuthStore.getState();
      if (admittedGeneration !== null && auth.authGeneration === admittedGeneration) {
        auth.clearAccessToken();
      }
      if (credentialOwner !== null) {
        const e2ee = useE2EEStore.getState();
        if (e2ee.ssoCredentialOwner === credentialOwner) {
          e2ee.setNeedsSSOUnlock(false);
        }
        try {
          await globalThis.electron?.clearTokensIfOwner?.(credentialOwner);
        } catch {
          // Explicit server revocation below is the cleanup backstop.
        }
      }
      if (issuedSession) await revokeAbortedSession(issuedSession);
    };
    const staleAttemptCleanup = (): Promise<void> | null =>
      requestIsCurrent() ? null : cleanupIssuedSession();
    const showInterrupted = () => {
      const auth = useAuthStore.getState();
      if (
        runtimeServerSelectionIsCurrent(requestSelection) &&
        auth.authGeneration === requestAuthGeneration &&
        auth.accessToken === null
      ) {
        setError('Your session ended during setup. Please sign in again to continue.');
        setTokenExpired(true);
        setSubmitting(false);
      }
    };

    try {
      const keys = await generateRegistrationKeys(passphrase);
      let staleCleanup = staleAttemptCleanup();
      if (staleCleanup) {
        await staleCleanup;
        return;
      }
      const publicKeyBase64 = await exportPublicKey(keys.publicKey);
      staleCleanup = staleAttemptCleanup();
      if (staleCleanup) {
        await staleCleanup;
        return;
      }
      keyMaterialPrepared = true;

      const result = await completeSSORegistration(
        {
          provider: state.provider,
          ssoToken: state.ssoToken,
          username,
          passphrase,
          wrappedPrivateKey: keys.wrappedPrivateKey,
          keyDerivationSalt: keys.keyDerivationSalt,
          publicKey: publicKeyBase64,
        },
        requestSelection
      );
      issuedSession = {
        accessToken: result.accessToken,
        sessionId: result.sessionId,
        apiBase: requestSelection.apiBase,
      };
      credentialOwner = result.credentialOwner;
      staleCleanup = staleAttemptCleanup();
      if (staleCleanup) {
        await staleCleanup;
        return;
      }

      try {
        await e2eeService.initialize(
          passphrase,
          keys.wrappedPrivateKey,
          keys.keyDerivationSalt,
          keys.keyDerivationAlg,
          { signal: new AbortController().signal, isCurrent: requestIsCurrent },
          teardownEpoch
        );
        // Capture the receipt the instant initialize() resolves — BEFORE the
        // stale-attempt cleanup below. Otherwise a request that goes stale in the
        // microtask window between initialize()'s synchronous key commit and this
        // check runs cleanupIssuedSession() -> clearFlowSessionKeys() against a
        // still-null receipt, leaving the just-committed keyset resident (CWE-212).
        // captureInitializationReceipt() returns null when no keyset committed, and
        // clearKeysIfInitializationCurrent is identity+attempt-guarded, so an early
        // capture can never erase a successor's keys.
        flowInitializationReceipt = e2eeService.captureInitializationReceipt();
        staleCleanup = staleAttemptCleanup();
        if (staleCleanup) {
          await staleCleanup;
          return;
        }
      } catch (initError) {
        if (initError instanceof E2EEInitTeardownError || !requestIsCurrent()) {
          await cleanupIssuedSession();
          showInterrupted();
          return;
        }
        e2eeService.clearKeys();
        teardownEpoch = e2eeService.captureTeardownEpoch();
        needsSSOUnlock = true;
        console.warn(
          'E2EE init after SSO passphrase setup failed; gating admission on SSO eager unlock:',
          errorMessage(initError)
        );
      }

      await persistE2EESessionKeys(flowInitializationReceipt?.sessionKeys ?? null, credentialOwner);
      staleCleanup = staleAttemptCleanup();
      if (staleCleanup) {
        await staleCleanup;
        return;
      }
      if (e2eeService.wasTornDownSince(teardownEpoch)) {
        await cleanupIssuedSession();
        showInterrupted();
        return;
      }

      if (needsSSOUnlock) {
        useE2EEStore.getState().setNeedsSSOUnlock(true, credentialOwner);
      }
      admittedGeneration = useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(requestAuthGeneration, result.accessToken, result.sessionId);
      if (admittedGeneration === null) {
        await cleanupIssuedSession();
        return;
      }
      if (
        runtimeServerSelectionIsCurrent(requestSelection) &&
        useAuthStore.getState().authGeneration === admittedGeneration
      ) {
        setSSOState({ phase: 'idle' });
      } else {
        await cleanupIssuedSession();
      }
    } catch (err) {
      const mayReport = requestIsCurrent();
      await cleanupIssuedSession();
      if (!mayReport || !requestIsCurrent()) return;
      if (err instanceof SSOServiceError) {
        const body = (err.body ?? {}) as CompleteRegistrationErrorBody;
        setError(mapServerErrorToMessage(err.status, body, providerName));
        if (err.status === 401 && body.error_code === 'sso_token_invalid') {
          setTokenExpired(true);
        }
      } else if (!keyMaterialPrepared) {
        setError('Could not prepare encryption keys. Please try again.');
      } else {
        setError('Registration failed. Please try again.');
      }
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
          encrypted messages — even we can&apos;t read them without it. You can still sign in with{' '}
          {providerName} anytime.
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
            This will be your identity: @{username || 'username'}
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

        {tokenExpired ? (
          <button
            type="button"
            className="sso-passphrase-setup__submit"
            onClick={() => {
              void begin(state.provider);
            }}
          >
            Sign in with {providerName} again
          </button>
        ) : (
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
        )}
      </form>
    </div>
  );
};

export default SSOPassphraseSetup;
