/**
 * useSSOFlow (#270)
 *
 * Thin React hook that wraps `startSSOFlow()` and dispatches its
 * `SSOResult` into the global `useSSOStore`. Components call `begin(provider)`
 * and then read `useSSOStore((s) => s.state)` to render the right next step.
 *
 * On `logged_in`, the hook also writes the access token into `useAuthStore`
 * and resets the SSO store back to idle (the user is now authenticated).
 *
 * Errors are caught and surfaced as `{ phase: 'error', message }` so the UI
 * can show a non-blocking error rather than the hook throwing into a
 * component-level error boundary.
 */

import { useCallback } from 'react';
import { useSSOStore } from '../stores/ssoStore';
import { startSSOFlow, type SSOProvider } from '../services/ssoService';
import { useAuthStore } from '../stores/authStore';
import { useE2EEStore } from '../stores/e2eeStore';
import { useMFAChallengeStore } from '../stores/mfaChallengeStore';
import { hydratePostLogin } from '../services/postLoginHydration';

export function useSSOFlow(): { begin: (provider: SSOProvider) => Promise<void> } {
  const setState = useSSOStore((s) => s.setState);
  const setAccessToken = useAuthStore((s) => s.setAccessToken);

  const begin = useCallback(
    async (provider: SSOProvider): Promise<void> => {
      setState({ phase: 'authenticating', provider });
      try {
        const result = await startSSOFlow(provider);
        switch (result.kind) {
          case 'logged_in':
            setAccessToken(result.accessToken);
            // Arm the SSO eager-unlock gate (#270 Task 21b). See
            // e2eeStore.ts file-level doc for the two-flag semantics
            // that combine `needsSSOUnlock` and `ready` to gate MainApp.
            useE2EEStore.getState().setNeedsSSOUnlock(true);
            setState({ phase: 'idle' });
            // Hydrate post-login user state (preferences, saved GIFs,
            // notification prefs, entitlements) so SSO matches the password /
            // session-restore paths (#1297). Guarded so a hydration blip never
            // turns a successful SSO login into an error — the session is
            // already valid at this point.
            try {
              await hydratePostLogin();
            } catch (err) {
              console.warn(
                'SSO post-login hydration failed (non-fatal):',
                err instanceof Error ? err.message : 'hydrate_failed'
              );
            }
            break;
          case 'mfa_required':
            // Bridge to the canonical MFA modal. The store records the SSO-side
            // phase (so AuthFlow / Login can render fall-back UI if needed) AND
            // the global useMFAChallengeStore is loaded so MFAChallengeModal —
            // mounted at the App root — picks up the challenge identically to
            // the password path. Without this bridge, the SSO mfa_required
            // branch sets a phase nothing renders against and the user is
            // stranded.
            setState({ phase: 'mfa_required', mfaChallengeToken: result.mfaChallengeToken });
            useMFAChallengeStore
              .getState()
              .showChallenge(
                result.mfaChallengeToken,
                result.methods ?? [],
                'sso_login',
                result.recoveryOnlyMethods
              )
              .then(async (mfaResult) => {
                if (mfaResult.verified && mfaResult.payload?.access_token) {
                  // Hydrate auth store from the MFA verify response. SSO
                  // sessions are cookie-authoritative, so we do NOT persist
                  // the body's refresh_token (matches the existing
                  // 'logged_in' branch above). E2EE unwrap is passphrase-
                  // based via SSOEagerUnlock — do NOT derive crypto material
                  // from the MFA verify payload ([internal]rules/e2ee.md).
                  useAuthStore.getState().setAccessToken(mfaResult.payload.access_token);
                  if (mfaResult.payload.session_id) {
                    useAuthStore.getState().setSessionId(mfaResult.payload.session_id);
                  }
                  setState({ phase: 'idle' });
                  useE2EEStore.getState().setNeedsSSOUnlock(true);
                  // Hydrate post-login user state so the SSO-MFA path matches
                  // the password / session-restore paths (#1297). Guarded so a
                  // hydration blip never turns a successful login into an error.
                  try {
                    await hydratePostLogin();
                  } catch (err) {
                    console.warn(
                      'SSO post-login hydration failed (non-fatal):',
                      err instanceof Error ? err.message : 'hydrate_failed'
                    );
                  }
                } else if (mfaResult.verified) {
                  // verified:true but payload missing or access_token absent /
                  // empty. In production this should not happen — SSO
                  // challenges encode PurposeLogin on the wire which always
                  // returns a full login payload. Surface as an explicit
                  // error rather than silently dropping the user at idle so
                  // the UI shows something rather than a no-op.
                  console.error('SSO MFA verify returned verified=true without access_token');
                  setState({ phase: 'error', message: 'mfa_verify_missing_token' });
                } else {
                  // verified:false. The user cancelled (clearChallenge fires
                  // resolve({ verified: false })) or the modal was cleared.
                  // The verification-failed case never reaches this branch:
                  // MFAChallengeModal stays open on a !res.ok response and
                  // does NOT resolve the promise; the user must cancel
                  // explicitly to leave that state.
                  setState({ phase: 'idle' });
                }
              })
              .catch((err: unknown) => {
                // Defensive: if the .then handler itself throws (e.g., a
                // store mutation throws), this .catch must not throw
                // either — wrap the recovery in try/catch to guarantee
                // no unhandled rejection escapes the floating promise
                // chain.
                try {
                  const message = err instanceof Error ? err.message : 'sso_mfa_failed';
                  setState({ phase: 'error', message });
                } catch (recoveryErr) {
                  console.error('SSO MFA error handler failed:', (recoveryErr as Error).message);
                }
              });
            break;
          case 'register_required':
            setState({
              phase: 'register_required',
              provider,
              ssoToken: result.ssoToken,
              email: result.email,
              name: result.name,
            });
            break;
          case 'link_available':
            setState({
              phase: 'link_required',
              provider,
              ssoToken: result.ssoToken,
              maskedEmail: result.maskedEmail,
            });
            break;
        }
      } catch (e: unknown) {
        const message = e instanceof Error ? e.message : 'sso_failed';
        setState({ phase: 'error', message });
      }
    },
    [setState, setAccessToken]
  );

  return { begin };
}
