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
import { useSSOStore, type SSOState } from '../stores/ssoStore';
import { startSSOFlow, type SSOProvider, type SSOResult } from '../services/ssoService';
import { useAuthStore } from '../stores/authStore';
import { useE2EEStore } from '../stores/e2eeStore';
import {
  useMFAChallengeStore,
  type MFAVerifyResponse,
  type MFAChallengeResult,
} from '../stores/mfaChallengeStore';
import { revokeAbortedSession } from '../services/apiClient';
import {
  captureRuntimeServerSelection,
  runtimeServerSelectionIsCurrent,
  type RuntimeServerSelection,
} from '../services/runtimeServerBase';
import type { CredentialOwner } from '../../main/ipcContract';

function reservationIsCurrent(
  authGeneration: number,
  serverSelection: RuntimeServerSelection
): boolean {
  return (
    useAuthStore.getState().authGeneration === authGeneration &&
    runtimeServerSelectionIsCurrent(serverSelection)
  );
}

async function discardDirectSSOSession(
  result: Extract<SSOResult, { kind: 'logged_in' }>,
  serverSelection: RuntimeServerSelection
): Promise<void> {
  await Promise.allSettled([
    globalThis.electron?.clearTokensIfOwner?.(result.credentialOwner),
    revokeAbortedSession({
      accessToken: result.accessToken,
      sessionId: result.sessionId,
      apiBase: serverSelection.apiBase,
    }),
  ]);
}

async function discardMFASession(
  payload: MFAVerifyResponse,
  serverSelection: RuntimeServerSelection,
  credentialOwner?: CredentialOwner
): Promise<void> {
  await Promise.allSettled([
    credentialOwner === undefined
      ? undefined
      : globalThis.electron?.clearTokensIfOwner?.(credentialOwner),
    revokeAbortedSession({
      accessToken: payload.access_token ?? null,
      refreshToken: payload.refresh_token ?? null,
      sessionId: payload.session_id ?? null,
      apiBase: serverSelection.apiBase,
    }),
  ]);
}

/**
 * MFA-verify continuation for the SSO `mfa_required` branch. Promoted verbatim
 * out of `begin`'s `.then` callback so neither the begin flow nor this
 * continuation exceeds the S3776 cognitive-complexity threshold. All try/catch,
 * ordering, and `reservationIsCurrent` fencing checks are preserved exactly.
 */
async function handleSSOMfaResult(
  mfaResult: MFAChallengeResult,
  reservationGeneration: number,
  serverSelection: RuntimeServerSelection,
  setState: (state: SSOState) => void
): Promise<void> {
  if (mfaResult.verified) {
    const payload = mfaResult.payload;
    const accessToken = payload.access_token;
    const refreshToken = payload.refresh_token;
    const sessionId = payload.session_id;
    if (!accessToken || !refreshToken || !sessionId) {
      await discardMFASession(payload, serverSelection);
      if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
      console.error('SSO MFA verify returned an incomplete credential tuple');
      setState({ phase: 'error', message: 'mfa_verify_missing_token' });
      return;
    }
    if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
      await discardMFASession(payload, serverSelection);
      return;
    }

    let credentialOwner: CredentialOwner | undefined;
    try {
      const storedOwner = await globalThis.electron?.storeRefreshToken?.({
        refreshToken,
        rememberMe: payload.remember_me ?? true,
        apiBase: serverSelection.apiBase,
        accessToken,
      });
      if (typeof storedOwner !== 'number') {
        throw new TypeError('SSO MFA refresh-token persistence was rejected');
      }
      credentialOwner = storedOwner;
    } catch {
      await discardMFASession(payload, serverSelection, credentialOwner);
      if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
      setState({ phase: 'error', message: 'sso_mfa_persistence_failed' });
      return;
    }
    if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
      await discardMFASession(payload, serverSelection, credentialOwner);
      return;
    }

    // E2EE unwrap is passphrase-based via SSOEagerUnlock — do
    // not derive crypto material from the MFA verify payload.
    const admittedGeneration = useAuthStore
      .getState()
      .beginAuthLifecycleIfCurrent(reservationGeneration, accessToken, sessionId);
    if (admittedGeneration === null || !runtimeServerSelectionIsCurrent(serverSelection)) {
      if (
        admittedGeneration !== null &&
        useAuthStore.getState().authGeneration === admittedGeneration
      ) {
        useAuthStore.getState().clearAccessToken();
      }
      await discardMFASession(payload, serverSelection, credentialOwner);
      return;
    }
    useE2EEStore.getState().setNeedsSSOUnlock(true, credentialOwner);
    setState({ phase: 'idle' });
  } else {
    if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
    // verified:false. The user cancelled (clearChallenge fires
    // resolve({ verified: false })) or the modal was cleared.
    // The verification-failed case never reaches this branch:
    // MFAChallengeModal stays open on a !res.ok response and
    // does NOT resolve the promise; the user must cancel
    // explicitly to leave that state.
    setState({ phase: 'idle' });
  }
}

export function useSSOFlow(): { begin: (provider: SSOProvider) => Promise<void> } {
  const setState = useSSOStore((s) => s.setState);

  const begin = useCallback(
    async (provider: SSOProvider): Promise<void> => {
      // Reserve auth ownership synchronously, before the first await. This
      // fences any password-login continuation that is still unwrapping keys
      // or about to persist its refresh token. Main-process token storage is
      // then cleared before SSO starts: if an older store IPC already landed,
      // this clear follows it; if it has not, Login's generation guard stops it.
      useAuthStore.getState().clearAccessToken();
      const reservationGeneration = useAuthStore.getState().authGeneration;
      const serverSelection = captureRuntimeServerSelection();
      setState({ phase: 'authenticating', provider });
      // A retry supersedes any MFA prompt from the prior reservation. Its
      // promise resolves after the generation changed, so its continuation is
      // fenced by reservationIsCurrent below.
      useMFAChallengeStore.getState().clearChallenge();

      try {
        await globalThis.electron?.clearTokens?.();
        if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;

        const result = await startSSOFlow(provider, serverSelection.apiBase);
        if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
          if (result.kind === 'logged_in') {
            await discardDirectSSOSession(result, serverSelection);
          }
          return;
        }

        switch (result.kind) {
          case 'logged_in': {
            const admittedGeneration = useAuthStore
              .getState()
              .beginAuthLifecycleIfCurrent(
                reservationGeneration,
                result.accessToken,
                result.sessionId
              );
            if (admittedGeneration === null || !runtimeServerSelectionIsCurrent(serverSelection)) {
              if (
                admittedGeneration !== null &&
                useAuthStore.getState().authGeneration === admittedGeneration
              ) {
                useAuthStore.getState().clearAccessToken();
              }
              await discardDirectSSOSession(result, serverSelection);
              return;
            }
            // Arm the SSO eager-unlock gate (#270 Task 21b). See
            // e2eeStore.ts file-level doc for the two-flag semantics
            // that combine `needsSSOUnlock` and `ready` to gate MainApp.
            useE2EEStore.getState().setNeedsSSOUnlock(true, result.credentialOwner);
            setState({ phase: 'idle' });
            break;
          }
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
              .then((mfaResult) =>
                handleSSOMfaResult(mfaResult, reservationGeneration, serverSelection, setState)
              )
              .catch((err: unknown) => {
                // Defensive: if the .then handler itself throws (e.g., a
                // store mutation throws), this .catch must not throw
                // either — wrap the recovery in try/catch to guarantee
                // no unhandled rejection escapes the floating promise
                // chain.
                try {
                  if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
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
        if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
        const message = e instanceof Error ? e.message : 'sso_failed';
        setState({ phase: 'error', message });
      }
    },
    [setState]
  );

  return { begin };
}
