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
import { useSSOStore, type SSOState } from '../stores/auth/ssoStore';
import {
  startSSOFlow,
  type SSOProvider,
  type SSOResult,
  type SSOCompletionResult,
} from '../services/ssoService';
import { useAuthStore } from '../stores/auth/authStore';
import { useE2EEStore } from '../stores/auth/e2eeStore';
import { useMFAChallengeStore, type MFAChallengeResult } from '../stores/auth/mfaChallengeStore';
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
  completion: SSOCompletionResult,
  serverSelection: RuntimeServerSelection
): Promise<void> {
  // #2424: main stored the refresh credential under `credentialOwner`, so the
  // renderer discards it owner-scoped (clearTokensIfOwner) and revokes the
  // issued session by its access/session reference. No refresh token exists here.
  await Promise.allSettled([
    globalThis.electron?.clearTokensIfOwner?.(completion.credentialOwner),
    revokeAbortedSession({
      accessToken: completion.accessToken,
      sessionId: completion.sessionId,
      apiBase: serverSelection.apiBase,
    }),
  ]);
}

/**
 * AC-8: roll back the SSO eager-unlock gate ONLY while it still belongs to this
 * flow's owner. A superseding flow may have re-armed the gate under its own
 * owner between our arming and a failed auth publish; clearing it unconditionally
 * would strip the successor's gate. Compares the live gate owner before clearing.
 */
function rollbackSSOUnlockGate(credentialOwner: CredentialOwner): void {
  const e2ee = useE2EEStore.getState();
  if (e2ee.needsSSOUnlock && e2ee.ssoCredentialOwner === credentialOwner) {
    e2ee.setNeedsSSOUnlock(false);
  }
}

/**
 * Admit the SSO MFA session main already verified and stored (#2424).
 *
 * The refresh credential was persisted in main under `completion.credentialOwner`
 * via `sso:completeMFA` (storeRefreshTokenIfOwner) — the renderer holds only the
 * sanitized access/session/owner tuple and never a refresh token, so there is NO
 * unrestricted `storeRefreshToken` call here. The E2EE unlock gate is armed with
 * that owner BEFORE the auth lifecycle publishes (AC-7); a lost currentness check
 * rolls the gate back only while it still belongs to this owner (AC-8). Split out
 * of `handleSSOMfaResult` to stay under the S3776 cognitive-complexity threshold.
 */
async function admitVerifiedSSOMfaSession(
  completion: SSOCompletionResult,
  reservationGeneration: number,
  serverSelection: RuntimeServerSelection,
  setState: (state: SSOState) => void
): Promise<void> {
  const { accessToken, sessionId, credentialOwner } = completion;
  if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
    await discardMFASession(completion, serverSelection);
    return;
  }

  // AC-7: arm the owner-bound gate before publishing auth, so any synchronous
  // auth-store subscriber that sees a non-null access token also sees
  // needsSSOUnlock=true with this owner (no transient E2EE-bypass window). E2EE
  // unwrap stays passphrase-based via SSOEagerUnlock — never derived from MFA.
  useE2EEStore.getState().setNeedsSSOUnlock(true, credentialOwner);

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
    rollbackSSOUnlockGate(credentialOwner);
    await discardMFASession(completion, serverSelection);
    return;
  }
  setState({ phase: 'idle' });
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
  // The SSO modal branch resolves the main-verified `ssoCompletion` variant
  // (#2424): main already ran /auth/mfa/verify and stored the refresh credential
  // owner-scoped, so the renderer receives only access/session/owner. A
  // verification failure keeps the modal open and does NOT resolve, so `verified`
  // is only ever true-with-ssoCompletion (success) or false (explicit cancel).
  if (mfaResult.verified && 'ssoCompletion' in mfaResult) {
    await admitVerifiedSSOMfaSession(
      mfaResult.ssoCompletion,
      reservationGeneration,
      serverSelection,
      setState
    );
  } else {
    if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;
    // verified:false — the user cancelled (clearChallenge resolves
    // { verified: false }). The verification-failed case never reaches here:
    // MFAChallengeModal stays open on a failed proof and does NOT resolve; the
    // user must cancel explicitly to leave that state.
    setState({ phase: 'idle' });
  }
}

/**
 * Admit a direct (non-MFA) SSO login: begin the auth lifecycle, arm the SSO
 * eager-unlock gate, and settle the store — or discard the session if the
 * reservation was superseded. Split out of `begin`'s switch so `begin` stays
 * under the S3776 cognitive-complexity threshold; the fence ordering is
 * preserved exactly.
 */
async function admitDirectSSOLogin(
  result: Extract<SSOResult, { kind: 'logged_in' }>,
  reservationGeneration: number,
  serverSelection: RuntimeServerSelection,
  setState: (state: SSOState) => void
): Promise<void> {
  // Defense-in-depth: re-check the reservation before ARMING the gate, mirroring
  // admitVerifiedSSOMfaSession. The caller already guards, and there is no await
  // between here and the synchronous beginAuthLifecycleIfCurrent CAS, so this is
  // currently redundant — but it keeps the two admit paths symmetric and robust
  // if a future refactor inserts an await before the arm (#2424 security review).
  if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
    await discardDirectSSOSession(result, serverSelection);
    return;
  }

  // AC-7: arm the SSO eager-unlock gate (#270 Task 21b) BEFORE publishing auth,
  // so a synchronous auth-store subscriber cannot observe a non-null access token
  // with the gate still false and admit authenticated UI against a prior
  // account's ambient `ready`. See e2eeStore.ts for the two-flag MainApp gate.
  useE2EEStore.getState().setNeedsSSOUnlock(true, result.credentialOwner);

  const admittedGeneration = useAuthStore
    .getState()
    .beginAuthLifecycleIfCurrent(reservationGeneration, result.accessToken, result.sessionId);
  if (admittedGeneration === null || !runtimeServerSelectionIsCurrent(serverSelection)) {
    if (
      admittedGeneration !== null &&
      useAuthStore.getState().authGeneration === admittedGeneration
    ) {
      useAuthStore.getState().clearAccessToken();
    }
    rollbackSSOUnlockGate(result.credentialOwner);
    await discardDirectSSOSession(result, serverSelection);
    return;
  }
  setState({ phase: 'idle' });
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
        // Login-side: this runs immediately BEFORE startSSOFlow, so a
        // `concord://invite/CODE` queued while the app was closed must survive
        // it — click-invite-then-sign-in is the flow #2363 exists to repair.
        await globalThis.electron?.clearTokens?.({ keepDeepLinks: true });
        if (!reservationIsCurrent(reservationGeneration, serverSelection)) return;

        const result = await startSSOFlow(provider, serverSelection.apiBase);
        if (!reservationIsCurrent(reservationGeneration, serverSelection)) {
          if (result.kind === 'logged_in') {
            await discardDirectSSOSession(result, serverSelection);
          }
          return;
        }

        switch (result.kind) {
          case 'logged_in':
            await admitDirectSSOLogin(result, reservationGeneration, serverSelection, setState);
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
                result.recoveryOnlyMethods,
                // #2424: carry the reserved owner + provider so the modal can
                // route the MFA proof through sso:completeMFA under this owner.
                { provider, credentialOwner: result.credentialOwner }
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
