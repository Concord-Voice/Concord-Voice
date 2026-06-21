/**
 * SSOAccountLinkConfirm — modal for the email-match account-linking flow (#270, Task 18).
 *
 * Reached when the SSO callback returns `account_link_available`: the email
 * returned by the OAuth IdP matches an existing password-authenticated Concord
 * account. To prove ownership of that existing account, the user enters their
 * Concord password; the server then permanently links the SSO identity to it.
 *
 * Submitting calls `POST /api/v1/auth/sso/{provider}/complete-link` via the
 * `completeSSOLink` service helper. On 423 the server has temporarily locked
 * the account after too many failed attempts; we surface a distinct, action-
 * oriented message so the user understands the wait is server-imposed (not
 * "wrong password again"). Other failures collapse to a generic password-
 * mismatch hint without leaking the underlying status code.
 *
 * Cancel resets the SSO store back to `idle`, which causes AuthFlow (Task 21)
 * to re-render the standard login screen — the user can choose to register a
 * new account or restart the SSO flow.
 *
 * Defensive render: returns null when ssoStore phase !== 'link_required' so
 * this component is safe to mount unconditionally inside an AuthFlow that
 * hasn't yet routed to it.
 *
 * Privacy: the secret never enters logs or telemetry — it is held only in
 * component state, passed once to `completeSSOLink`, then released when the
 * component unmounts on phase transition.
 */

import React, { useState } from 'react';
import LoadingSpinner from './LoadingSpinner';
import { useSSOStore } from '../../stores/ssoStore';
import { useAuthStore } from '../../stores/authStore';
import { completeSSOLink } from '../../services/ssoService';
import './SSOAccountLinkConfirm.css';

const SSOAccountLinkConfirm: React.FC = () => {
  const state = useSSOStore((s) => s.state);
  const setSSOState = useSSOStore((s) => s.setState);
  const setAccessToken = useAuthStore((s) => s.setAccessToken);

  const [password, setPassword] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState<string | null>(null);

  // Defensive: only render in the link_required phase. Any other phase
  // routing is the responsibility of AuthFlow (Task 21).
  if (state.phase !== 'link_required') {
    return null;
  }

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (submitting || password.length === 0) {
      return;
    }
    setSubmitting(true);
    setError(null);

    try {
      const { accessToken } = await completeSSOLink({
        provider: state.provider,
        ssoToken: state.ssoToken,
        password,
      });
      setAccessToken(accessToken);
      // Returning to phase 'idle' lets AuthFlow re-evaluate based on the
      // new accessToken in authStore (the user is now logged in).
      setSSOState({ phase: 'idle' });
    } catch (err) {
      // ssoService throws Error(`sso_complete_link_failed_${status}`).
      // Pull the status off the error message — 423 means the server
      // applied a brute-force lockout; any other status is treated as
      // a generic mismatch (without leaking the status code).
      const errMessage = err instanceof Error ? err.message : '';
      const statusMatch = /sso_complete_link_failed_(\d+)/.exec(errMessage);
      const status = statusMatch ? Number.parseInt(statusMatch[1], 10) : 0;

      if (status === 423) {
        setError('Too many failed attempts. Try again in 15 minutes.');
      } else {
        setError('Wrong password');
      }
      setSubmitting(false);
    }
  };

  const handleCancel = () => {
    setSSOState({ phase: 'idle' });
  };

  return (
    <div className="sso-link-confirm">
      <form onSubmit={handleSubmit} className="sso-link-confirm__form" noValidate>
        <h2 className="sso-link-confirm__title">Link your Google account</h2>
        <p className="sso-link-confirm__intro">
          An account with the email <strong>{state.maskedEmail}</strong> already exists. Enter your
          Concord password to link your Google account.
        </p>

        <div className="sso-link-confirm__field">
          <label htmlFor="sso-link-password" className="sso-link-confirm__label">
            Password
          </label>
          <input
            id="sso-link-password"
            type="password"
            className="sso-link-confirm__input"
            value={password}
            onChange={(ev) => setPassword(ev.target.value)}
            autoComplete="current-password"
            disabled={submitting}
          />
        </div>

        {error && (
          <div className="sso-link-confirm__error" role="alert">
            {error}
          </div>
        )}

        <div className="sso-link-confirm__actions">
          <button
            type="button"
            className="sso-link-confirm__cancel"
            onClick={handleCancel}
            disabled={submitting}
          >
            Cancel
          </button>
          <button
            type="submit"
            className="sso-link-confirm__submit"
            disabled={submitting || password.length === 0}
          >
            {submitting ? (
              <>
                Linking…
                <LoadingSpinner size="small" inline />
              </>
            ) : (
              'Link account'
            )}
          </button>
        </div>
      </form>
    </div>
  );
};

export default SSOAccountLinkConfirm;
