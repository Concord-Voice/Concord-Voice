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
import { useE2EEStore } from '../../stores/e2eeStore';
import { completeSSOLink, abandonSSOReservation } from '../../services/ssoService';
import { revokeAbortedSession, type AbortedSessionRef } from '../../services/apiClient';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
} from '../../services/runtimeServerBase';
import type { CredentialOwner } from '../../../main/ipcContract';
import './SSOAccountLinkConfirm.css';

/**
 * Map a completeSSOLink failure to the user-facing error copy. ssoService throws
 * Error(`sso_complete_link_failed_${status}`). Pull the status off the error
 * message — 423 means the server applied a brute-force lockout; any other status
 * is treated as a generic mismatch (without leaking the status code). Extracted
 * verbatim from handleSubmit's catch so the status decode does not add to its
 * cognitive complexity (S3776).
 */
function mapCompleteLinkError(err: unknown): string {
  const errMessage = err instanceof Error ? err.message : '';
  const statusMatch = /sso_complete_link_failed_(\d+)/.exec(errMessage);
  const status = statusMatch ? Number.parseInt(statusMatch[1], 10) : 0;

  if (status === 423) {
    return 'Too many failed attempts. Try again in 15 minutes.';
  }
  return 'Wrong password';
}

const SSOAccountLinkConfirm: React.FC = () => {
  const state = useSSOStore((s) => s.state);
  const setSSOState = useSSOStore((s) => s.setState);

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
    const requestSelection = captureRuntimeServerSelection();
    const requestAuthGeneration = useAuthStore.getState().authGeneration;
    const requestIsCurrent = () =>
      runtimeServerSelectionIsCurrent(requestSelection) &&
      useAuthStore.getState().authGeneration === requestAuthGeneration;
    setSubmitting(true);
    setError(null);

    let issuedSession: AbortedSessionRef | null = null;
    let credentialOwner: CredentialOwner | null = null;
    let admittedGeneration: number | null = null;
    const cleanupIssuedSession = async () => {
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

    try {
      const result = await completeSSOLink(
        {
          provider: state.provider,
          ssoToken: state.ssoToken,
          password,
        },
        requestSelection
      );
      issuedSession = {
        accessToken: result.accessToken,
        sessionId: result.sessionId,
        apiBase: requestSelection.apiBase,
      };
      credentialOwner = result.credentialOwner;
      if (!requestIsCurrent()) {
        await cleanupIssuedSession();
        return;
      }

      // Arm the post-auth gate before publishing the access token so React
      // can never observe an admitted SSO session without its owner-bound
      // unlock requirement.
      useE2EEStore.getState().setNeedsSSOUnlock(true, credentialOwner);
      admittedGeneration = useAuthStore
        .getState()
        .beginAuthLifecycleIfCurrent(requestAuthGeneration, result.accessToken, result.sessionId);
      if (admittedGeneration === null) {
        await cleanupIssuedSession();
        return;
      }
      // Returning to phase 'idle' lets AuthFlow re-evaluate based on the
      // new accessToken in authStore (the user is now logged in).
      if (
        runtimeServerSelectionIsCurrent(requestSelection) &&
        useAuthStore.getState().authGeneration === admittedGeneration
      ) {
        setSSOState({ phase: 'idle' });
      } else {
        await cleanupIssuedSession();
      }
    } catch (err) {
      await cleanupIssuedSession();
      if (!requestIsCurrent()) return;
      setError(mapCompleteLinkError(err));
      setSubmitting(false);
    }
  };

  const handleCancel = () => {
    // #2394: backing out here abandons the SSO flow, so retire the orphaned
    // main-process reservation eagerly rather than leaving it to block a later
    // password registration's E2EE key staging. Fire-and-forget: the helper
    // never throws, and nothing below depends on its result.
    void abandonSSOReservation();
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
