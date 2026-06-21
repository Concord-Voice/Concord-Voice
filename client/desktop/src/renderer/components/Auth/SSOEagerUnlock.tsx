/**
 * SSOEagerUnlock — first-device passphrase prompt for SSO sign-in (#270, Task 19).
 *
 * Reached after a successful SSO `logged_in` callback when this device has no
 * cached unwrap key for the user's E2EE private key. SSO authenticates the
 * IdP identity but cannot itself derive the wrap-key (only the user's
 * passphrase can), so we block app entry until the user proves possession of
 * the passphrase by unwrapping the private key locally.
 *
 * On success: calls `e2eeService.initialize`, which derives the wrap-key,
 * unwraps the private key, and persists session keys in safeStorage via the
 * existing `electron.storeE2EEKeys` IPC bridge — the same path traditional
 * password-login uses (see `Login.tsx` `completeLoginFromResponse`). The
 * `onUnlock` callback signals the parent (Task 21 AuthFlow wiring) to
 * transition the user into the app.
 *
 * On 3 wrong attempts: surfaces a Social Recovery offer. Once the lock-out
 * branch renders, it stays rendered for this mount — the user can either
 * accept Social Recovery (parent navigates via `onSocialRecovery`) or reload
 * the app to retry. We do NOT reset the counter mid-session even after Social
 * Recovery is shown; reverting back to the prompt would defeat the rate-limit.
 *
 * Privacy: the passphrase never leaves this component except into
 * `e2eeService.initialize`, which is the canonical, audited entry point.
 * Errors are caught generically — we never log the passphrase, key material,
 * or the underlying decrypt error (which can sometimes carry a chunk of
 * ciphertext in the cause chain). The user-facing copy is fixed:
 * "Incorrect passphrase".
 */

import React, { useState } from 'react';
import { apiFetch, safeJson } from '../../services/apiClient';
import { type KeyDerivationAlgorithm } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import LoadingSpinner from './LoadingSpinner';
import './SSOEagerUnlock.css';

interface Props {
  onUnlock: () => void;
  onSocialRecovery: () => void;
}

interface KeysResponse {
  e2ee_keys: {
    wrapped_private_key: string;
    key_derivation_salt: string;
    key_derivation_alg?: KeyDerivationAlgorithm;
  };
}

const MAX_ATTEMPTS = 3;

const SSOEagerUnlock: React.FC<Props> = ({ onUnlock, onSocialRecovery }) => {
  const [passphrase, setPassphrase] = useState('');
  const [attemptCount, setAttemptCount] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (submitting || passphrase.length === 0) {
      return;
    }
    setSubmitting(true);
    setError(null);

    // Step-isolated error handling so we can distinguish six failure sources
    // and only count true "incorrect passphrase" against the lockout counter.
    // Previously every failure (network blip, JSON parse, IPC failure) bumped
    // the attempt counter and rendered "Incorrect passphrase", which would
    // lock a user out of their own account on a transient connectivity issue.
    //
    // Steps:
    //   1. fetch /users/me/keys (network → 401 → server)
    //   2. parse JSON
    //   3. e2eeService.initialize (passphrase decrypt — the ONLY counter-eligible step)
    //   4. (optional) storeE2EEKeys via IPC — failure should not block this session

    let res: Response;
    try {
      res = await apiFetch('/api/v1/users/me/keys');
    } catch {
      // Network failure — not a passphrase problem, do NOT increment counter.
      setError("Couldn't reach the server. Check your connection and try again.");
      setSubmitting(false);
      return;
    }
    if (!res.ok) {
      if (res.status === 401) {
        setError('Your session expired. Please sign in again.');
      } else {
        setError("Couldn't load your encrypted keys. Please try again.");
      }
      setSubmitting(false);
      return;
    }

    let data: KeysResponse;
    try {
      data = await safeJson<KeysResponse>(res);
    } catch {
      setError("Couldn't read the server's response. Please try again.");
      setSubmitting(false);
      return;
    }
    const {
      wrapped_private_key: wrappedPrivateKey,
      key_derivation_salt: salt,
      key_derivation_alg: alg = 'argon2id',
    } = data.e2ee_keys;

    // The ONLY step that counts toward the wrong-passphrase lockout.
    try {
      await e2eeService.initialize(passphrase, wrappedPrivateKey, salt, alg);
    } catch {
      // Never log the underlying error — its `cause` chain can carry
      // ciphertext fragments. AES-GCM authentication failure is the
      // intended signal for "wrong passphrase".
      setAttemptCount((n) => n + 1);
      setError('Incorrect passphrase');
      setPassphrase('');
      setSubmitting(false);
      return;
    }

    // Best-effort persist; failure leaves us with in-memory keys (this session
    // works, next launch will re-prompt). Do NOT count toward the lockout.
    try {
      const sessionKeys = e2eeService.getSessionKeys();
      if (sessionKeys && globalThis.electron?.storeE2EEKeys) {
        await globalThis.electron.storeE2EEKeys(sessionKeys);
      }
    } catch {
      // intentionally swallowed: in-memory keys make this session work; the
      // user will be asked to unlock again on the next launch.
    }

    onUnlock();
  };

  if (attemptCount >= MAX_ATTEMPTS) {
    return (
      <div className="sso-eager-unlock sso-eager-unlock--locked">
        <h2 className="sso-eager-unlock__title">Can&apos;t unlock encrypted messages</h2>
        <p className="sso-eager-unlock__intro">
          If you&apos;ve forgotten your passphrase, you can recover access through your trustees.
        </p>
        <div className="sso-eager-unlock__actions">
          <button type="button" className="sso-eager-unlock__submit" onClick={onSocialRecovery}>
            Use Social Recovery
          </button>
        </div>
      </div>
    );
  }

  return (
    <form className="sso-eager-unlock" onSubmit={handleSubmit} noValidate>
      <h2 className="sso-eager-unlock__title">Unlock your encrypted messages</h2>
      <p className="sso-eager-unlock__intro">
        Enter your Concord passphrase to decrypt your messages on this device.
      </p>

      <div className="sso-eager-unlock__field">
        <label htmlFor="sso-unlock-passphrase" className="sso-eager-unlock__label">
          Passphrase
        </label>
        <input
          id="sso-unlock-passphrase"
          type="password"
          className="sso-eager-unlock__input"
          value={passphrase}
          onChange={(ev) => setPassphrase(ev.target.value)}
          autoComplete="current-password"
          autoFocus
          disabled={submitting}
        />
      </div>

      {error && (
        <div className="sso-eager-unlock__error" role="alert">
          {error}
        </div>
      )}

      <button
        type="submit"
        className="sso-eager-unlock__submit"
        disabled={submitting || passphrase.length === 0}
      >
        {submitting ? (
          <>
            Unlocking…
            <LoadingSpinner size="small" inline />
          </>
        ) : (
          'Unlock'
        )}
      </button>
    </form>
  );
};

export default SSOEagerUnlock;
