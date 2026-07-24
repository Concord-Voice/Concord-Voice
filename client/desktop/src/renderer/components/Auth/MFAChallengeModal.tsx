import React, { useState, useCallback, useMemo } from 'react';
import { useMFAChallengeStore, type MFAVerifyResponse } from '../../stores/mfaChallengeStore';
import { ensureMachineId, safeJson } from '../../services/apiClient';
import { apiUrl, captureRuntimeServerSelection } from '../../services/runtimeServerBase';
import { completeSSOMFA, SSOServiceError } from '../../services/ssoService';
import TOTPInput from './TOTPInput';
import BackupCodeInput from './BackupCodeInput';
import WebAuthnPrompt from './WebAuthnPrompt';
import MFAMethodPicker, {
  getDefaultMethod,
  getAvailableCategories,
  MFAMethodCategory,
} from './MFAMethodPicker';
import './TOTPInput.css';

// Maps a verify failure to a user-facing message (#2424). A non-2xx SSO verify
// throws SSOServiceError carrying the server's error_code (wrong code / rate
// limit); surface it and keep the modal open for retry. Never log token/proof
// material — only the error `.message` per [internal]rules/observability.md.
function resolveMfaProofError(err: unknown): string {
  if (err instanceof SSOServiceError) {
    const code = typeof err.body?.error_code === 'string' ? err.body.error_code : '';
    return code || 'Verification failed. Please try again.';
  }
  console.error('MFA verify failed:', (err as Error).message);
  return 'Verification failed. Please try again.';
}

const MFAChallengeModal: React.FC = () => {
  const challengeToken = useMFAChallengeStore((s) => s.challengeToken);
  const methods = useMFAChallengeStore((s) => s.methods);
  const recoveryOnlyMethods = useMFAChallengeStore((s) => s.recoveryOnlyMethods);
  const purpose = useMFAChallengeStore((s) => s.purpose);
  const ssoContext = useMFAChallengeStore((s) => s.ssoContext);
  const completeChallenge = useMFAChallengeStore((s) => s.completeChallenge);
  const clearChallenge = useMFAChallengeStore((s) => s.clearChallenge);

  const defaultMethod = useMemo(
    () => (methods.length > 0 ? getDefaultMethod(methods, recoveryOnlyMethods) : 'totp'),
    [methods, recoveryOnlyMethods]
  );

  const [mode, setMode] = useState<MFAMethodCategory | 'method-select'>(defaultMethod);
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // WebAuthn options now flow through the store so callers (or tests) can
  // populate them via setState. The modal subscribes selectively and remounts
  // WebAuthnPrompt when options arrive.
  const webauthnOptions = useMFAChallengeStore((s) => s.webauthnOptions);

  // Reset mode when challenge token changes
  const prevTokenRef = React.useRef(challengeToken);
  if (challengeToken !== prevTokenRef.current) {
    prevTokenRef.current = challengeToken;
    if (challengeToken && methods.length > 0) {
      const newDefault = getDefaultMethod(methods);
      setMode(newDefault);
      setError('');
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
      setLoading(true);
      setError('');
      try {
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
            captureRuntimeServerSelection()
          );
          completeChallenge({ verified: true, ssoCompletion: completion }, challengeToken);
          return;
        }

        const machineId = await ensureMachineId();
        const res = await fetch(apiUrl('/api/v1/auth/mfa/verify'), {
          method: 'POST',
          headers: {
            'Content-Type': 'application/json',
            ...(machineId ? { 'X-Machine-Id': machineId } : {}),
          },
          credentials: 'include',
          body: JSON.stringify({ mfa_challenge_token: challengeToken, ...proof }),
        });

        const data = await safeJson<MFAVerifyResponse & { error?: string }>(res);
        if (res.ok) {
          completeChallenge({ verified: true, payload: data }, challengeToken);
        } else {
          setError(data.error || 'Verification failed');
        }
      } catch (err) {
        setError(resolveMfaProofError(err));
      } finally {
        setLoading(false);
      }
    },
    [challengeToken, completeChallenge, purpose, ssoContext]
  );

  const handleVerify = useCallback(
    (code: string, method: string) => submitMfaProof({ method, code }),
    [submitMfaProof]
  );

  const handleWebAuthnSuccess = useCallback(
    async (credential: Credential) => {
      const pkc = credential as PublicKeyCredential;
      const response = pkc.response as AuthenticatorAssertionResponse;
      const assertion = {
        id: pkc.id,
        rawId: btoa(String.fromCodePoint(...new Uint8Array(pkc.rawId))),
        type: pkc.type,
        response: {
          authenticatorData: btoa(
            String.fromCodePoint(...new Uint8Array(response.authenticatorData))
          ),
          clientDataJSON: btoa(String.fromCodePoint(...new Uint8Array(response.clientDataJSON))),
          signature: btoa(String.fromCodePoint(...new Uint8Array(response.signature))),
          userHandle: response.userHandle
            ? btoa(String.fromCodePoint(...new Uint8Array(response.userHandle)))
            : null,
        },
      };
      await submitMfaProof({ method: 'webauthn', assertion });
    },
    [submitMfaProof]
  );

  const handleWebAuthnError = useCallback((errMsg: string) => {
    setError(errMsg);
  }, []);

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

  return (
    <div className="mfa-modal-overlay">
      <div className="mfa-modal">
        <h3>Verify Your Identity</h3>
        <p className="mfa-modal-desc">{subtitle}</p>

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
            onSubmit={(code) => handleVerify(code, 'totp')}
            disabled={loading}
            error={error}
          />
        )}

        {mode === 'backup' && (
          <BackupCodeInput
            onSubmit={(code) => handleVerify(code, 'backup_code')}
            disabled={loading}
            error={error}
          />
        )}

        {mode === 'webauthn' && (
          <>
            {webauthnOptions ? (
              <WebAuthnPrompt
                requestOptions={webauthnOptions}
                onSuccess={handleWebAuthnSuccess}
                onError={handleWebAuthnError}
                onCancel={() => {
                  setMode('method-select');
                  setError('');
                }}
              />
            ) : (
              <div style={{ textAlign: 'center' }}>
                <p className="mfa-modal-desc">
                  WebAuthn verification will be triggered by the server challenge.
                </p>
                {error && <p className="totp-error">{error}</p>}
              </div>
            )}
          </>
        )}

        {mode === 'email-sms' && (
          <TOTPInput
            onSubmit={(code) => handleVerify(code, methods.includes('email') ? 'email' : 'sms')}
            disabled={loading}
            error={error}
          />
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
          onClick={clearChallenge}
          disabled={loading}
        >
          Cancel
        </button>
      </div>
    </div>
  );
};

export default MFAChallengeModal;
