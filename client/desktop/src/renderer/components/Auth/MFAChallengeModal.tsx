import React, { useState, useCallback, useMemo, useEffect, useId, useRef } from 'react';
import { useMFAChallengeStore, type MFAVerifyResponse } from '../../stores/auth/mfaChallengeStore';
import { useModalStack } from '../ui/ModalContext';
// The challenge registers at the global overlays' depth, above any ui/Modal, so
// the modals under it go inert and their Escape and Tab handlers stand down.
import { TOP_LAYER_DEPTH } from '../../hooks/ui/useTopLayerDialog';
import { ensureMachineId, safeJson } from '../../services/system/apiClient';
import { runtimeServerSelectionIsCurrent } from '../../services/system/runtimeServerBase';
import { challengeIssuerFor } from '../../services/system/challengeIssuer';
import {
  completeSSOMFA,
  SSOServiceError,
  abandonSSOReservation,
} from '../../services/system/ssoService';
import TOTPInput from './TOTPInput';
import BackupCodeInput from './BackupCodeInput';
import WebAuthnPrompt from './WebAuthnPrompt';
import MFAMethodPicker, {
  getDefaultMethod,
  getAvailableCategories,
  MFAMethodCategory,
} from './MFAMethodPicker';
import { useSignInEmailCode, type SignInEmailCode } from './signInEmailCode';
import SendNewCodeButton from './SendNewCodeButton';
import { responseIssuedSessionID, revokeMalformedLoginSession } from './loginSessionRevoke';
import { intersectInline, pickDefaultMethod } from '../../services/system/stepUpRequirements';
import { bufferToBase64url } from '../../utils/crypto/base64url';
import './TOTPInput.css';
// The challenge's own styles. MFA.css otherwise arrives only with the lazy
// Settings chunk, so a challenge raised before Settings was ever opened (login,
// SSO) rendered unstyled and in page flow.
import '../Settings/MFA.css';

// A proof in flight disables Cancel, and a close re-shows the dialog for it, so
// a request that never settles would leave the challenge with no way out.
const MFA_PROOF_TIMEOUT_MS = 30_000;

const GENERIC_PROOF_ERROR = 'Verification failed. Please try again.';
const TIMEOUT_PROOF_ERROR = 'Verification timed out. Please try again.';
// One line: the error slot centres only a message that does not wrap.
const ORIGIN_CHANGED_ERROR = 'The server changed. Cancel and try again.';

// The server selection moved on after the challenge arrived. Its token belongs
// to the server that issued it, so nothing more is sent for it, and a result
// that lands after the move is not acted on.
class ChallengeOriginChangedError extends Error {}

// Codes main's sso:completeMFA raises itself. The server's verify endpoint
// answers with readable `error` text instead, which is shown as it is.
const SSO_PROOF_ERRORS: Record<string, string> = {
  sso_mfa_verify_failed: "Couldn't check your code. Check your connection and try again.",
  sso_cancelled: 'This sign-in was cancelled. Please sign in again.',
  sso_session_rejected: "The sign-in couldn't be completed. Please sign in again.",
};

// Maps a verify failure to a user-facing message (#2424). A non-2xx SSO verify
// throws SSOServiceError carrying the server's text or main's own code; keep
// the modal open for retry and never show a bare code. `timedOut` is the
// request's own signal: a stall after the headers fails the body read with an
// AbortError, not a TimeoutError. Never log token/proof material — only the
// error `.message` per [internal]rules/observability.md.
function resolveMfaProofError(err: unknown, timedOut: boolean): string {
  if (err instanceof ChallengeOriginChangedError) return ORIGIN_CHANGED_ERROR;
  if (timedOut || (err instanceof DOMException && err.name === 'TimeoutError')) {
    return TIMEOUT_PROOF_ERROR;
  }
  if (err instanceof SSOServiceError) {
    if (typeof err.body?.error === 'string' && err.body.error) return err.body.error;
    const code = typeof err.body?.error_code === 'string' ? err.body.error_code : '';
    return SSO_PROOF_ERRORS[code] ?? GENERIC_PROOF_ERROR;
  }
  console.error('MFA verify failed:', (err as Error).message);
  return GENERIC_PROOF_ERROR;
}

// A send can outlive its challenge, as a proof can (see submitMfaProof).
const isLiveChallenge = (token: string) => useMFAChallengeStore.getState().challengeToken === token;

// The email panel for a challenge whose issuer is unknown: nothing is sent,
// and the panel says so. Its token has no server it may go to.
const NO_ISSUER_EMAIL_CODE: SignInEmailCode = {
  sendError: ORIGIN_CHANGED_ERROR,
  failed: false,
  retry: () => {},
};

/** The email panel's state: the refusal when a challenge offering email has no issuer. */
function emailCodeForIssuer(
  challengeToken: string | null,
  issuer: unknown,
  methods: readonly string[],
  sent: SignInEmailCode
): SignInEmailCode {
  const issuerUnknown = Boolean(challengeToken) && issuer === null;
  return issuerUnknown && methods.includes('email') ? NO_ISSUER_EMAIL_CODE : sent;
}

/**
 * The method a challenge opens on: the server's `default_method` (the factor
 * the account used most recently) when the challenge offers it, else the
 * strongest offered method (#3663 review).
 */
function challengeDefaultMethod(
  methods: string[],
  recoveryOnlyMethods: string[],
  serverDefault: string | null
): MFAMethodCategory {
  const offered = intersectInline(getAvailableCategories(methods, recoveryOnlyMethods));
  return (
    pickDefaultMethod(offered, serverDefault) ?? getDefaultMethod(methods, recoveryOnlyMethods)
  );
}

const WEBAUTHN_SPENT = 'This security key request can no longer be used. Cancel and try again.';

/**
 * The security-key method. The prompt runs only with options for a ceremony
 * the server has not yet judged; a refused assertion consumed it, so the panel
 * shows why and how to get a new one instead of waiting forever (#3663 review).
 */
const WebAuthnPanel: React.FC<{
  options: PublicKeyCredentialRequestOptions | null;
  spent: boolean;
  error: string;
  onSuccess: (credential: Credential) => void;
  onError: (message: string) => void;
  onCancel: () => void;
}> = ({ options, spent, error, onSuccess, onError, onCancel }) => {
  if (options && !spent) {
    return (
      <WebAuthnPrompt
        requestOptions={options}
        onSuccess={onSuccess}
        onError={onError}
        onCancel={onCancel}
      />
    );
  }
  return (
    <div style={{ textAlign: 'center' }}>
      {error && <p className="totp-error">{error}</p>}
      <p className="mfa-modal-desc">
        {spent
          ? WEBAUTHN_SPENT
          : 'WebAuthn verification will be triggered by the server challenge.'}
      </p>
    </div>
  );
};

const MFAChallengeModal: React.FC = () => {
  const challengeToken = useMFAChallengeStore((s) => s.challengeToken);
  const methods = useMFAChallengeStore((s) => s.methods);
  const recoveryOnlyMethods = useMFAChallengeStore((s) => s.recoveryOnlyMethods);
  const purpose = useMFAChallengeStore((s) => s.purpose);
  const ssoContext = useMFAChallengeStore((s) => s.ssoContext);
  const completeChallenge = useMFAChallengeStore((s) => s.completeChallenge);
  const clearChallenge = useMFAChallengeStore((s) => s.clearChallenge);
  const serverDefault = useMFAChallengeStore((s) => s.defaultMethod);

  const defaultMethod = useMemo(
    () =>
      methods.length > 0
        ? challengeDefaultMethod(methods, recoveryOnlyMethods, serverDefault)
        : 'totp',
    [methods, recoveryOnlyMethods, serverDefault]
  );

  const [mode, setMode] = useState<MFAMethodCategory | 'method-select'>(defaultMethod);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // Bumped when a code is refused, remounting the code input empty: each code
  // is accepted once, so a refused code is either wrong or spent.
  const [inputKey, setInputKey] = useState(0);
  // The server consumes a ceremony when it judges the assertion, so after a
  // refused one the prompt cannot run again for this challenge (#3663 review).
  const [webauthnSpent, setWebauthnSpent] = useState(false);
  // The caller passes the challenge's parsed WebAuthn options to showChallenge,
  // which stores them with the token. WebAuthnPrompt restarts its ceremony
  // when they change.
  const webauthnOptions = useMFAChallengeStore((s) => s.webauthnOptions);

  // The server selection the challenge was raised under, recorded by the code
  // that raised it before publishing it (see challengeIssuer.ts). Null when
  // none was recorded or the record has outlived the challenge. Nothing is
  // sent for the token then: taking the selection here instead would send it
  // to whichever server is selected when the modal renders.
  const challengeSelection = challengeIssuerFor(challengeToken);

  // The modal stays mounted between challenges, so a new one starts clean: its
  // own default method, no error, and no spinner left from a proof that
  // belonged to the challenge before it.
  const prevTokenRef = React.useRef(challengeToken);
  if (challengeToken !== prevTokenRef.current) {
    prevTokenRef.current = challengeToken;
    if (challengeToken) {
      if (methods.length > 0) setMode(defaultMethod);
      setError('');
      setLoading(false);
      setWebauthnSpent(false);
    }
  }

  const available = useMemo(
    () => getAvailableCategories(methods, recoveryOnlyMethods),
    [methods, recoveryOnlyMethods]
  );
  const hasMultipleMethods = available.length > 1;

  // #2424: single verification path for both TOTP/backup and WebAuthn proofs.
  // For the 'sso_login' purpose it routes the proof through the sso:completeMFA
  // MAIN handler (refresh token never reaches the renderer); every other purpose
  // keeps the existing renderer-side POST to /api/v1/auth/mfa/verify. The
  // completeChallenge `forToken` argument binds the result to this challenge so a
  // late proof cannot settle a superseding challenge (AC-11).
  const submitMfaProof = useCallback(
    async (proof: { method: string; code?: string; assertion?: unknown }): Promise<void> => {
      if (!challengeToken) return;
      // No recorded issuer means no server this token may go to.
      if (!challengeSelection) {
        setError(ORIGIN_CHANGED_ERROR);
        setInputKey((k) => k + 1);
        return;
      }
      // A proof can outlive its challenge (a replacement challenge supersedes
      // it in flight), so its result only touches the challenge it was sent for.
      const isCurrent = () => useMFAChallengeStore.getState().challengeToken === challengeToken;
      const selection = challengeSelection;
      const requireOrigin = () => {
        if (!runtimeServerSelectionIsCurrent(selection)) throw new ChallengeOriginChangedError();
      };
      setLoading(true);
      setError('');
      const timeout = AbortSignal.timeout(MFA_PROOF_TIMEOUT_MS);
      try {
        requireOrigin();
        // `proof` already carries exactly the fields for its method ({method,code}
        // or {method,assertion}), so it spreads directly into the payload/body.
        if (purpose === 'sso_login' && ssoContext) {
          const completion = await completeSSOMFA(
            {
              provider: ssoContext.provider,
              mfaChallengeToken: challengeToken,
              credentialOwner: ssoContext.credentialOwner,
              ...proof,
            },
            selection
          );
          // No origin check here. Main has already stored this completion's
          // refresh credential and the server has issued its session, so it
          // must reach useSSOFlow even after a move: that fence, not this one,
          // clears the stored credential and revokes the session when the
          // selection has moved. Dropping it here would leave both alive.
          completeChallenge({ verified: true, ssoCompletion: completion }, challengeToken);
          return;
        }

        const machineId = await ensureMachineId(selection.apiBase);
        requireOrigin();
        const res = await fetch(`${selection.apiBase}/api/v1/auth/mfa/verify`, {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            ...(machineId ? { 'X-Machine-Id': machineId } : {}),
          },
          credentials: 'include',
          body: JSON.stringify({ mfa_challenge_token: challengeToken, ...proof }),
          signal: timeout,
        });

        const data = await safeJson<MFAVerifyResponse & { error?: string }>(res);
        if (!runtimeServerSelectionIsCurrent(selection)) {
          // An answer accepted after the move is not acted on, so any session
          // it issued is revoked at the server that issued it. This branch's
          // only purpose today (suspicious_refresh) returns no tokens, so this
          // is defence in depth. Not awaited: revokeAbortedSession has no
          // timeout, and Cancel stays disabled while the proof is loading.
          if (res.ok) {
            void revokeMalformedLoginSession(data, selection.apiBase, responseIssuedSessionID(res));
          }
          throw new ChallengeOriginChangedError();
        }
        if (res.ok) {
          completeChallenge({ verified: true, payload: data }, challengeToken);
        } else if (isCurrent()) {
          setError(data.error || 'Verification failed');
          setInputKey((k) => k + 1);
        }
      } catch (err) {
        const message = resolveMfaProofError(err, timeout.aborted);
        if (isCurrent()) {
          setError(message);
          setInputKey((k) => k + 1);
        }
      } finally {
        if (isCurrent()) setLoading(false);
      }
    },
    [challengeToken, challengeSelection, completeChallenge, purpose, ssoContext]
  );

  const handleVerify = useCallback(
    (code: string, method: string) => submitMfaProof({ method, code }),
    [submitMfaProof]
  );

  const handleWebAuthnSuccess = useCallback(
    async (credential: Credential) => {
      const pkc = credential as PublicKeyCredential;
      const response = pkc.response as AuthenticatorAssertionResponse;
      // go-webauthn reads unpadded base64url, as Login sends it (#3663 review).
      const assertion = {
        id: pkc.id,
        rawId: bufferToBase64url(pkc.rawId),
        type: pkc.type,
        response: {
          authenticatorData: bufferToBase64url(response.authenticatorData),
          clientDataJSON: bufferToBase64url(response.clientDataJSON),
          signature: bufferToBase64url(response.signature),
          userHandle: response.userHandle ? bufferToBase64url(response.userHandle) : null,
        },
      };
      await submitMfaProof({ method: 'webauthn', assertion });
      // Still this challenge, so the server refused the assertion: a verified
      // one completes the challenge and clears the token.
      if (useMFAChallengeStore.getState().challengeToken === challengeToken) {
        setWebauthnSpent(true);
      }
    },
    [submitMfaProof, challengeToken]
  );

  const handleWebAuthnError = useCallback((errMsg: string) => {
    setError(errMsg);
  }, []);

  const cancelChallenge = useCallback(() => {
    // #2394: only the 'sso_login' purpose holds an SSO reservation.
    // The other purpose, 'suspicious_refresh', runs mid-session where a
    // published credential already makes the release a structural
    // no-op — the gate states that rather than relying on it.
    // Fire-and-forget: clearChallenge must not wait on an IPC round
    // trip, and the helper never throws.
    if (purpose === 'sso_login') void abandonSSOReservation();
    clearChallenge();
  }, [purpose, clearChallenge]);

  // The challenge can fire while any other overlay is open — including the
  // Settings dialog, which showModal() puts in the browser top layer, where no
  // z-index can reach above it. A request is waiting on this challenge, so it
  // must be the one reachable surface: open it with showModal() too (the last
  // modal dialog shown is topmost and blocks the rest), and register it as the
  // topmost ModalContext entry (el null: the top layer ignores z-index).
  const dialogRef = useRef<HTMLDialogElement>(null);
  const invokerRef = useRef<HTMLElement | null>(null);
  const stackId = useId();
  const { register, unregister } = useModalStack();
  // Keyed on the token, not on "is open": a replacement challenge must be shown
  // even when Escape closed the dialog while the previous proof was in flight,
  // and a token that turns empty unmounts the dialog, so this must clean up.
  useEffect(() => {
    const dlg = dialogRef.current;
    if (!dlg) return;
    // Capture the focused element before register() inerts the background.
    // ??= keeps the first one across a replacement and StrictMode's replay.
    invokerRef.current ??= document.activeElement as HTMLElement | null;
    // The control the user was last in. Read at retake time, activeElement
    // already points into the burying dialog, whose showModal() focused it.
    let lastFocused: HTMLElement | null = null;
    const trackFocus = (e: FocusEvent) => {
      if (e.target instanceof HTMLElement) lastFocused = e.target;
    };
    dlg.addEventListener('focusin', trackFocus);
    if (!dlg.open) dlg.showModal();
    register(stackId, TOP_LAYER_DEPTH, null);

    // The top layer is last-showModal()-wins, so a dialog shown after the
    // challenge (Settings from a shortcut, a network-driven attestation
    // failure) buries it again. close() + showModal() takes the top back. Only
    // the challenge does this: two dialogs doing it would trade places forever.
    // A non-modal show() does not enter the top layer, so it buries nothing.
    const observer = new MutationObserver((records) => {
      const buried = records.some(
        (r) =>
          r.target !== dlg && r.target instanceof HTMLDialogElement && r.target.matches(':modal')
      );
      if (buried && dlg.open) {
        // showModal() refocuses the first control; keep the user where they
        // were typing.
        const restore = lastFocused;
        dlg.close();
        dlg.showModal();
        if (restore && dlg.contains(restore)) restore.focus();
      }
    });
    observer.observe(document.body, { subtree: true, attributeFilter: ['open'] });

    return () => {
      observer.disconnect();
      dlg.removeEventListener('focusin', trackFocus);
      unregister(stackId);
      // Disconnected means the challenge really closed (StrictMode's replay
      // keeps the dialog mounted). unregister() has already lifted the inert
      // background, so focus can return to the element the user was on.
      if (!dlg.isConnected) {
        invokerRef.current?.focus?.();
        invokerRef.current = null;
      }
    };
  }, [challengeToken, stackId, register, unregister]);

  // A proof in flight disables the code inputs, and Chromium drops focus to
  // <body>. When the proof fails, put focus back on an input: otherwise the
  // next digits typed are lost, and Escape and Tab come from outside the
  // dialog, where page-wide listeners behind it can take them.
  const wasLoadingRef = useRef(false);
  useEffect(() => {
    const dlg = dialogRef.current;
    if (wasLoadingRef.current && !loading && dlg?.open && !dlg.contains(document.activeElement)) {
      dlg.querySelector<HTMLElement>('input:not([disabled])')?.focus();
    }
    wasLoadingRef.current = loading;
  }, [loading]);

  // Renderer-direct for every purpose, sso_login included: the route takes
  // only the challenge token. Like a proof, it goes to the server the
  // challenge was raised under, and only while that is still the selection.
  // The hook reads a null selection as no challenge open, so a challenge with
  // no issuer is refused here.
  const sendEmailCode = useSignInEmailCode({
    challengeToken,
    serverSelection: challengeSelection,
    mode,
    methods,
    originChangedError: ORIGIN_CHANGED_ERROR,
    isCurrent: isLiveChallenge,
  });
  const emailCode = emailCodeForIssuer(challengeToken, challengeSelection, methods, sendEmailCode);

  // Early return AFTER all hooks
  if (!challengeToken) return null;

  const handleMethodSelect = (method: MFAMethodCategory) => {
    setMode(method);
    setError('');
  };

  let subtitle: string;
  if (mode === 'totp') subtitle = 'Enter the 6-digit code from your authenticator app';
  else if (mode === 'backup') subtitle = 'Enter one of your backup codes';
  else if (mode === 'webauthn') subtitle = 'Use your security key or biometrics';
  else if (mode === 'email-sms') subtitle = 'Enter the verification code sent to you';
  else subtitle = 'Select a verification method';

  // No portal: #root goes inert while a ui/Modal is open, but a showModal()
  // dialog escapes an inert ancestor (measured, Chromium 152).
  return (
    <dialog
      ref={dialogRef}
      className="mfa-challenge-dialog"
      aria-labelledby={`${stackId}-title`}
      aria-describedby={`${stackId}-desc`}
      onCancel={(e) => {
        // Escape = Cancel, except while a proof is in flight (the Cancel button
        // is disabled then too).
        e.preventDefault();
        if (!loading) cancelChallenge();
      }}
      onClose={(e) => {
        // The close event is queued. If the dialog is open again by the time
        // it runs (the top-layer retake above, or a replacement challenge),
        // it belonged to an earlier close.
        const dlg = e.currentTarget;
        if (dlg.open || !useMFAChallengeStore.getState().challengeToken) return;
        // Escape's `cancel` is cancelable only while the page has user
        // activation (measured both ways, Chromium 152), so preventDefault
        // above cannot always keep the dialog open. A proof in flight settles
        // the challenge itself, as the disabled Cancel button promises, so
        // re-show the dialog for it. Otherwise a closed dialog with a live
        // challenge is the original deadlock, so treat it as Cancel.
        if (loading && dlg.isConnected) {
          dlg.showModal();
          return;
        }
        cancelChallenge();
      }}
    >
      <div className="mfa-modal">
        <h3 id={`${stackId}-title`}>Verify Your Identity</h3>
        <p id={`${stackId}-desc`} className="mfa-modal-desc">
          {subtitle}
        </p>

        {mode === 'method-select' && (
          <MFAMethodPicker
            methods={methods}
            excludeMethods={recoveryOnlyMethods}
            currentMethod={defaultMethod}
            onSelect={handleMethodSelect}
            onCancel={() => {
              setMode(defaultMethod);
              setError('');
            }}
          />
        )}

        {mode === 'totp' && (
          <TOTPInput
            key={inputKey}
            onSubmit={(code) => handleVerify(code, 'totp')}
            disabled={loading}
            error={error}
          />
        )}

        {mode === 'backup' && (
          <BackupCodeInput
            key={inputKey}
            onSubmit={(code) => handleVerify(code, 'backup_code')}
            disabled={loading}
            error={error}
          />
        )}

        {mode === 'webauthn' && (
          <WebAuthnPanel
            options={webauthnOptions}
            spent={webauthnSpent}
            error={error}
            onSuccess={handleWebAuthnSuccess}
            onError={handleWebAuthnError}
            onCancel={() => {
              setMode('method-select');
              setError('');
            }}
          />
        )}

        {mode === 'email-sms' && (
          <>
            <TOTPInput
              key={inputKey}
              onSubmit={(code) => handleVerify(code, methods.includes('email') ? 'email' : 'sms')}
              disabled={loading}
              error={error}
            />
            <SendNewCodeButton emailCode={emailCode} disabled={loading} />
          </>
        )}

        {mode !== 'method-select' && hasMultipleMethods && (
          <button
            type="button"
            className="mfa-choose-another"
            onClick={() => {
              setMode('method-select');
              setError('');
            }}
            disabled={loading}
          >
            Choose another form of verification
          </button>
        )}

        <button
          type="button"
          className="btn btn-secondary mfa-modal-cancel"
          onClick={cancelChallenge}
          disabled={loading}
        >
          Cancel
        </button>
      </div>
    </dialog>
  );
};

export default MFAChallengeModal;
