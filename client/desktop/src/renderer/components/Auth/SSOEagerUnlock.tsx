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
 * unwraps the private key, and persists session keys in safeStorage through
 * the owner-scoped IPC bridge — the same credential-ownership boundary used
 * by the SSO callback. The `onUnlock` callback signals the parent (Task 21
 * AuthFlow wiring) to transition the user into the app.
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
import {
  apiFetch,
  safeJson,
  revokeAbortedSession,
  captureAuthSession,
} from '../../services/apiClient';
import { type KeyDerivationAlgorithm } from '../../utils/crypto';
import { e2eeService, type E2EEInitializationReceipt } from '../../services/e2eeService';
import { E2EEInitTeardownError } from '../../services/e2eeErrors';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
} from '../../services/runtimeServerBase';
import { useAuthStore } from '../../stores/auth/authStore';
import { useE2EEStore } from '../../stores/auth/e2eeStore';
import { persistE2EESessionKeys } from '../../utils/persistE2EESessionKeys';
import type { CredentialOwner } from '../../../main/ipcContract';
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

/**
 * Wipe main-process/disk credentials that a pending persist IPC re-wrote
 * AFTER a teardown wiped them — but ONLY for a nuclear-class (!rememberMe)
 * teardown and only when no successor session owns the store: a rememberMe
 * (graceful) teardown deliberately PRESERVES disk credentials so the session
 * can restore next launch (#1768), and this account's re-persisted keys are
 * part of that restorable state — wiping would delete the preserved refresh
 * token with them (Codex P1, PR #2337). File-private so handleSubmit stays
 * under the S3776 cognitive-complexity ceiling.
 */
async function clearCredentialsIfOwner(credentialOwner: CredentialOwner | null): Promise<void> {
  if (credentialOwner === null) return;
  try {
    await globalThis.electron?.clearTokensIfOwner?.(credentialOwner);
  } catch {
    // Explicit server revocation is the cleanup backstop.
  }
}

async function wipeRepersistedCredentialsForNuclearTeardown(
  credentialOwner: CredentialOwner | null
): Promise<void> {
  const live = useAuthStore.getState();
  if (live.rememberMe) return;

  // SSO callbacks persist refresh credentials in the main process before this
  // gate mounts. Keep cleanup in that same ownership domain so a stale unlock
  // cannot erase a successor login that completed at an await boundary.
  await clearCredentialsIfOwner(credentialOwner);
}

const SSOEagerUnlock: React.FC<Props> = ({ onUnlock, onSocialRecovery }) => {
  const [passphrase, setPassphrase] = useState('');
  const [attemptCount, setAttemptCount] = useState(0);
  const [error, setError] = useState<string | null>(null);
  const [submitting, setSubmitting] = useState(false);
  const ssoCredentialOwner = useE2EEStore((state) => state.ssoCredentialOwner);

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (submitting || passphrase.length === 0) {
      return;
    }
    setSubmitting(true);
    setError(null);
    // One-shot ownership token installed by useSSOFlow alongside the auth
    // lifecycle. Capture it for this attempt so a later SSO flow cannot make
    // this continuation persist or clear credentials under the new owner.
    const credentialOwner = ssoCredentialOwner;
    const requestSelection = captureRuntimeServerSelection();

    // Teardown epoch for the whole unlock attempt (keys fetch + Argon2id):
    // a 401 -> nuclearReset landing anywhere in this span makes initialize()
    // reject with E2EEInitTeardownError instead of committing keys for a
    // dead session (PR #2337 P1-B analogue for the SSO unlock surface).
    const teardownEpoch = e2eeService.captureTeardownEpoch();
    // Bind the abort-revoke to the session this unlock belongs to (set by the
    // SSO callback before this gate mounted), so a concurrent newer login is
    // not revoked by mistake (Codex P1, #2337). `let`, not `const`: the keys
    // fetch below can transparently refresh a 401, rotating the token (and
    // installing a session ID the SSO bootstrap never had) — the snapshot is
    // re-captured after that refresh-capable call so the ownership gate and a
    // later revoke track the CURRENT identity, not the stale bearer.
    let abortedSession = captureAuthSession();
    const requestAuthGeneration = abortedSession.authGeneration;
    const requestIsCurrent = () =>
      requestAuthGeneration !== undefined &&
      useAuthStore.getState().authGeneration === requestAuthGeneration &&
      runtimeServerSelectionIsCurrent(requestSelection);
    let flowInitializationReceipt: E2EEInitializationReceipt | null = null;

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

    // Re-capture after the refresh-capable keys fetch (Codex P1, #2337): a
    // 401 that apiFetch recovered installed a rotated token + session ID for
    // the SAME session. Without this, the ownership gate below would reject
    // the valid refreshed session, and an abort would revoke the stale bearer.
    // Adopt the post-fetch identity ONLY when it is live: a token-only
    // teardown landing during the fetch/parse leaves an EMPTY store, and
    // adopting {null, null} would make the ownership gate vacuously pass
    // (null === null) and admit a dead session — keeping the pre-fetch owner
    // makes the gate fail closed instead (Codex P1, #2337).
    const postFetchSession = captureAuthSession();
    if (
      postFetchSession.accessToken !== null &&
      postFetchSession.authGeneration === requestAuthGeneration &&
      runtimeServerSelectionIsCurrent(requestSelection)
    ) {
      abortedSession = postFetchSession;
    }

    // The ONLY step that counts toward the wrong-passphrase lockout.
    try {
      // #2423: initialize() returns THIS invocation's commit receipt (or null if
      // its guard declined the commit), so the receipt records exactly the keys
      // this call committed — never a successor's ambient state. A later cleanup
      // via clearKeysIfInitializationCurrent therefore cannot clear a successor.
      flowInitializationReceipt = await e2eeService.initialize(
        passphrase,
        wrappedPrivateKey,
        salt,
        alg,
        { signal: new AbortController().signal, isCurrent: requestIsCurrent },
        teardownEpoch
      );
      if (!requestIsCurrent()) {
        await clearCredentialsIfOwner(credentialOwner);
        await revokeAbortedSession(abortedSession);
        setError('Your session expired. Please sign in again.');
        setSubmitting(false);
        return;
      }
    } catch (err) {
      // A teardown mid-unlock (401 -> nuclearReset) is a dead-session signal,
      // NOT a wrong passphrase — never charge it against the lockout counter
      // (contract on E2EEInitTeardownError; PR #2337). Mirrors the 401 branch
      // of the keys fetch above.
      if (err instanceof E2EEInitTeardownError) {
        await revokeAbortedSession(abortedSession);
        setError('Your session expired. Please sign in again.');
        setSubmitting(false);
        return;
      }
      // Never log the underlying error — its `cause` chain can carry
      // ciphertext fragments. AES-GCM authentication failure is the
      // intended signal for "wrong passphrase".
      setAttemptCount((n) => n + 1);
      setError('Incorrect passphrase');
      setPassphrase('');
      setSubmitting(false);
      return;
    }

    // Best-effort persist for restart-survival; the shared helper warns only on
    // failure and never clears the valid in-session keys (#1278/#1288). This does
    // NOT count toward the lockout, and is a no-op if getSessionKeys() is null.
    await persistE2EESessionKeys(
      flowInitializationReceipt?.sessionKeys ?? null,
      credentialOwner ?? undefined
    );

    // Pre-admit epoch check: a teardown landing after initialize() resolved
    // (e.g. during the persist await) must not admit the user (PR #2337).
    if (e2eeService.wasTornDownSince(teardownEpoch)) {
      // The persist above may have re-written E2EE keys to the main process /
      // safeStorage AFTER the teardown wiped them (Codex P1, #2337) — see the
      // helper for the rememberMe (#1768) and successor-identity guards.
      e2eeService.clearKeysIfInitializationCurrent(flowInitializationReceipt);
      await wipeRepersistedCredentialsForNuclearTeardown(credentialOwner);
      await revokeAbortedSession(abortedSession);
      setError('Your session expired. Please sign in again.');
      setSubmitting(false);
      return;
    }

    // Token-lifecycle admit gate (Codex P1, PR #2337): the epoch check proves
    // key custody only at that instant, so separately verify that the captured
    // session still owns the auth store before admitting.
    // authGeneration is the renderer-owned lifecycle identity: access tokens
    // and server session IDs can both rotate during a legitimate refresh, but
    // the generation stays stable. The token/session fallback only supports
    // legacy snapshots that predate generation tracking.
    const liveAuth = useAuthStore.getState();
    const stillOwnsStore = requestIsCurrent() && liveAuth.accessToken !== null;
    if (!stillOwnsStore) {
      e2eeService.clearKeysIfInitializationCurrent(flowInitializationReceipt);
      await clearCredentialsIfOwner(credentialOwner);
      await revokeAbortedSession(abortedSession);
      setError('Your session expired. Please sign in again.');
      setSubmitting(false);
      return;
    }

    onUnlock();
  };

  const handleSocialRecovery = async () => {
    if (submitting) return;
    setSubmitting(true);
    const abortedSession = captureAuthSession();
    const credentialOwner = ssoCredentialOwner;
    await Promise.allSettled([
      clearCredentialsIfOwner(credentialOwner),
      revokeAbortedSession(abortedSession),
    ]);
    onSocialRecovery();
  };

  if (attemptCount >= MAX_ATTEMPTS) {
    return (
      <div className="sso-eager-unlock sso-eager-unlock--locked">
        <h2 className="sso-eager-unlock__title">Can&apos;t unlock encrypted messages</h2>
        <p className="sso-eager-unlock__intro">
          If you&apos;ve forgotten your passphrase, you can recover access through your trustees.
        </p>
        <div className="sso-eager-unlock__actions">
          <button
            type="button"
            className="sso-eager-unlock__submit"
            onClick={() => void handleSocialRecovery()}
            disabled={submitting}
          >
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
