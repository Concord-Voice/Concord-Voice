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
import { revokeAbortedSession } from '../../services/apiClient';
import { getApiBase } from '../../services/runtimeServerBase';
import { useSSOFlow } from '../../hooks/useSSOFlow';
import { generateRegistrationKeys, exportPublicKey } from '../../utils/crypto';
import { e2eeService } from '../../services/e2eeService';
import { E2EEInitTeardownError } from '../../services/e2eeErrors';
import { errorMessage } from '../../utils/redactError';
import { persistE2EESessionKeys } from '../../utils/persistE2EESessionKeys';
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
  const setAccessToken = useAuthStore((s) => s.setAccessToken);
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

  // Fatal-path for a logout-class teardown observed mid-setup: the
  // registration succeeded server-side, but THIS renderer session is dead.
  // Clear the just-published token (AuthFlow routes on the bare token) and
  // surface the re-initiate affordance — the next SSO sign-in lands on
  // eager-unlock against the server-held keys (Codex P1, PR #2337).
  const abortSetupForTeardown = async (abortedSession: {
    accessToken: string | null;
    sessionId: string | null;
  }) => {
    // Revoke the freshly-issued server session (HttpOnly refresh cookie + row)
    // BEFORE clearing the local token — the renderer apiFetch carries the
    // cookie, so this must run while it is still in the jar (Codex P1, #2337).
    await revokeAbortedSession(abortedSession);
    // Identity-guarded local clear (Codex P1, PR #2337): if a successor
    // sign-in completed while the revoke above was awaited, the store now
    // holds the NEW session's token — clearing unconditionally would log that
    // session out of the renderer even though the revoke helper correctly
    // declined it. Strip only this aborted flow's own token (mirrors Login's
    // stripAbortedToken discipline).
    const auth = useAuthStore.getState();
    if (auth.accessToken === abortedSession.accessToken) {
      auth.clearAccessToken();
    }
    // Main-process/disk cleanup (Codex P1, PR #2337): a storeE2EEKeys /
    // storeRefreshToken IPC pending when the teardown landed can repopulate
    // the main-process memory + safeStorage files AFTER the teardown wiped
    // them — leaving the dead registration session's key material resident.
    // This registration session is BRAND NEW (its disk artifacts are its own,
    // never a restorable prior session's), so the wipe mirrors Login's
    // clearAbortedDiskTokens: identity-guarded against a successor only.
    if (auth.accessToken === abortedSession.accessToken || auth.accessToken === null) {
      await globalThis.electron?.clearTokens?.();
    }
    setError('Your session ended during setup. Please sign in again to continue.');
    setTokenExpired(true);
    setSubmitting(false);
  };

  const handleSubmit = async (ev: React.FormEvent) => {
    ev.preventDefault();
    if (!valid || submitting) {
      return;
    }
    setSubmitting(true);
    setError(null);
    setTokenExpired(false);

    // Teardown epoch for the whole setup attempt: a 401 -> nuclearReset
    // landing anywhere in this span makes initialize() below reject with
    // E2EEInitTeardownError instead of committing keys for a dead session
    // (PR #2337 P1-B analogue for the SSO passphrase-setup surface).
    // (let, not const: the non-fatal init-failure rollback below rebases it.)
    let teardownEpoch = e2eeService.captureTeardownEpoch();

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
        // Bind a later teardown-abort revoke to THIS registration session, so a
        // concurrent newer login isn't revoked by mistake — and to the API
        // origin it was minted against (Codex P1, #2337).
        const abortedSession = { accessToken, sessionId: null, apiBase: getApiBase() };

        // Initialize e2eeService so E2EE is live for THIS session — mirrors the
        // post-registration init added to Register.tsx in #1278. Without this, an
        // SSO user who just set their passphrase has e2eeService.isInitialized ===
        // false and is blocked from creating channels / sending messages ("Setting
        // up secure messaging — try again in a moment.") until a logout→login. The
        // keys were generated above and the server now holds the wrapped private
        // key, so it is safe to establish the session here.
        //
        // The failure modes are handled SEPARATELY (per #1278 review + #2337):
        //   1. Ordinary initialize() failure → clearKeys() + continue
        //      NON-FATAL: SSO registration already succeeded server-side, so a
        //      client-side hiccup must not fail the flow. Post-#2337 finalizeKeys
        //      publishes the keyset in one synchronous block (no half-init state
        //      is reachable), so the rollback is defense-in-depth; it also resets
        //      the reactive ready flag.
        //   2. A teardown mid-init (E2EEInitTeardownError) is FATAL — the session
        //      is dead; continuing would admit the user on a resurrected token.
        //      Handled by abortSetupForTeardown below (revoke + clear + abort).
        try {
          await e2eeService.initialize(
            passphrase,
            keys.wrappedPrivateKey,
            keys.keyDerivationSalt,
            keys.keyDerivationAlg,
            undefined,
            teardownEpoch
          );
        } catch (initError) {
          // A logout-class teardown landed during setup: the registration DID
          // succeed server-side, but THIS renderer session is dead —
          // initialize() rejected on the stale teardown epoch. Continuing
          // would resurrect the torn-down session with the token published
          // above and admit the user with E2EE cleared (AuthFlow routes on
          // the bare token). Treat as FATAL: clear the fresh token, surface
          // the re-initiate affordance, abort. The next SSO sign-in lands on
          // eager-unlock against the server-held keys (Codex P1, PR #2337).
          if (initError instanceof E2EEInitTeardownError) {
            await abortSetupForTeardown(abortedSession);
            return;
          }
          e2eeService.clearKeys();
          // The rollback clearKeys() above bumps the teardown epoch — rebase
          // our baseline so this SELF-INFLICTED clear is not misread as an
          // external teardown by the pre-admit check below, which would turn
          // the deliberately non-fatal branch fatal (Codex P2, PR #2337). A
          // real teardown after this still advances the epoch past the new
          // baseline.
          teardownEpoch = e2eeService.captureTeardownEpoch();
          console.warn(
            'E2EE init after SSO passphrase setup failed; secure messaging will require a manual re-login:',
            errorMessage(initError)
          );
        }

        //   2. Persist E2EE keys for restart-survival. Failure warns only and
        //      NEVER clears the valid in-memory session from (1) (#1278/#1288 —
        //      rationale lives in persistE2EESessionKeys). No-op if (1) failed
        //      (getSessionKeys returns null after clearKeys).
        await persistE2EESessionKeys(e2eeService.getSessionKeys());

        // Pre-admit epoch check: a teardown landing after initialize()
        // resolved (e.g. during the persist await) must not admit the user on
        // the resurrected token (PR #2337).
        if (e2eeService.wasTornDownSince(teardownEpoch)) {
          await abortSetupForTeardown(abortedSession);
          return;
        }

        // Token-lifecycle admit gate (Codex P1, PR #2337): a token-only
        // invalidation — handleRefreshFailure's `phase !== 'stable'` path —
        // clears the access token WITHOUT advancing the E2EE epoch, so the
        // check above passes. Verify this registration's token still owns the
        // auth store before completing (the fresh SSO registration session
        // carries no session ID, so token equality is the identity).
        if (useAuthStore.getState().accessToken !== abortedSession.accessToken) {
          await abortSetupForTeardown(abortedSession);
          return;
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
          setError(mapServerErrorToMessage(err.status, body, providerName));
          // An expired/invalid sso_token cannot be retried — flip to the
          // re-initiate affordance instead of leaving the user re-submitting a
          // dead token against the same form (#2045).
          if (err.status === 401 && body.error_code === 'sso_token_invalid') {
            setTokenExpired(true);
          }
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
