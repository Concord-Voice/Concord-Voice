import React, { useState, useMemo, useCallback, useRef, useEffect } from 'react';
import TOTPInput from './TOTPInput';
import BackupCodeInput from './BackupCodeInput';
import { apiFetch } from '../../services/system/apiClient';
import { base64urlToBuffer, bufferToBase64url } from '../../utils/crypto/base64url';
import {
  getAvailableCategories,
  getDefaultMethod,
  type MFAMethodCategory,
} from './MFAMethodPicker';
import type { StepUpPurpose } from './stepUpPurpose';

// ── WebAuthn helpers (module-level, outside component) ─────────────────

/** Perform the browser WebAuthn assertion ceremony. */
async function performWebAuthnAssertion(
  options: PublicKeyCredentialRequestOptions,
  signal: AbortSignal
): Promise<PublicKeyCredential> {
  const credential = (await navigator.credentials.get({
    publicKey: options,
    signal,
  })) as PublicKeyCredential;
  if (!credential) throw new Error('No credential returned');
  return credential;
}

/** Send the assertion response to the server and return the MFA token. */
async function finishWebAuthnVerification(
  credential: PublicKeyCredential,
  _challengeToken: string
): Promise<string> {
  const assertion = credential.response as AuthenticatorAssertionResponse;
  const finishRes = await apiFetch('/api/v1/mfa/webauthn/verify-inline/finish', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({
      id: credential.id,
      rawId: bufferToBase64url(credential.rawId),
      type: credential.type,
      response: {
        authenticatorData: bufferToBase64url(assertion.authenticatorData),
        clientDataJSON: bufferToBase64url(assertion.clientDataJSON),
        signature: bufferToBase64url(assertion.signature),
        userHandle: assertion.userHandle ? bufferToBase64url(assertion.userHandle) : undefined,
      },
    }),
  });
  const finishData = await finishRes.json();
  if (!finishRes.ok) throw new Error(finishData.error || 'Verification failed');
  return finishData.mfa_token;
}

/** Classify a WebAuthn error into a user-facing message, or null for silent abort. */
function classifyWebAuthnError(err: unknown): string | null {
  if (err instanceof DOMException && err.name === 'AbortError') return null;
  if (err instanceof DOMException && err.name === 'NotAllowedError') {
    return 'Cancelled or timed out. Try again.';
  }
  return err instanceof Error ? err.message : 'Verification failed';
}

/** Error copy in the security-key branch, for the ceremony's and the parent's. */
const REFUSAL_STYLE: React.CSSProperties = {
  margin: '0 0 8px',
  fontSize: '13px',
  color: 'var(--error-color, #ed4245)',
};

// ── Method-link descriptors ────────────────────────────────────────────

interface MethodLinkDescriptor {
  mode: MFAMethodCategory;
  label: string;
  resetsWebAuthn: boolean;
}

const METHOD_LINK_DEFS: MethodLinkDescriptor[] = [
  { mode: 'webauthn', label: 'Use a security key instead', resetsWebAuthn: true },
  { mode: 'totp', label: 'Use authenticator app instead', resetsWebAuthn: false },
  { mode: 'backup', label: 'Use a backup code instead', resetsWebAuthn: false },
  { mode: 'email-sms', label: 'Use email/SMS code instead', resetsWebAuthn: false },
];

// ── Component ──────────────────────────────────────────────────────────

interface MFAVerifyPromptProps {
  /** Raw method strings from the server (e.g. ['totp', 'webauthn', 'email', 'sms']) */
  methods: string[];
  /** Methods that are recovery-only and should be excluded */
  recoveryOnlyMethods?: string[];
  /** Called with the MFA code (TOTP, backup code, or WebAuthn inline-verify token) */
  onVerify: (code: string) => void;
  /**
   * The step-up purpose of the one request this prompt's code is sent with.
   * A WebAuthn inline-verify token is minted for exactly this purpose and the
   * server accepts it on no other route, so it must name that request, not a
   * neighbour. `null` when the route accepts no inline token at all: the
   * security-key option is then not offered, since a token minted for it
   * could never be spent. Required, so a new mount cannot forget it.
   */
  purpose: StepUpPurpose | null;
  /**
   * Fires on every edit of a typed code — the complete code, or `''` while it
   * is incomplete — and with `''` on a switch to another method. A parent
   * that STORES the code for a later Confirm needs this: `onVerify` reports
   * only completion, so editing a complete code, or switching away from it,
   * would otherwise leave the old code stored behind an enabled Confirm. A
   * WebAuthn token still arrives only through `onVerify`.
   */
  onCodeChange?: (code: string) => void;
  disabled?: boolean;
  error?: string;
  /** If true, backup codes are hidden (use during setup, not revoke/remove) */
  excludeBackupCodes?: boolean;
}

/**
 * Multi-method MFA verification prompt for protected operations (setup, revoke, etc.).
 * Supports TOTP, backup codes, and WebAuthn (via inline-verify token flow).
 */
const MFAVerifyPrompt: React.FC<MFAVerifyPromptProps> = ({
  methods,
  recoveryOnlyMethods = [],
  onVerify,
  purpose,
  onCodeChange,
  disabled = false,
  error,
  excludeBackupCodes = false,
}) => {
  // A route that accepts no inline token (purpose null) is offered no
  // security-key option: excluding the method keeps it out of every list below.
  const excludedMethods = useMemo(
    () => (purpose === null ? [...recoveryOnlyMethods, 'webauthn'] : [...recoveryOnlyMethods]),
    [recoveryOnlyMethods, purpose]
  );

  const available = useMemo(() => {
    const cats = getAvailableCategories(methods, excludedMethods);
    return excludeBackupCodes ? cats.filter((c) => c !== 'backup') : cats;
  }, [methods, excludedMethods, excludeBackupCodes]);

  const defaultMethod = useMemo(() => {
    const def = getDefaultMethod(methods, excludedMethods);
    if (excludeBackupCodes && def === 'backup') {
      return available[0] || 'totp';
    }
    return def;
  }, [methods, excludedMethods, excludeBackupCodes, available]);

  const [mode, setMode] = useState<MFAMethodCategory>(defaultMethod);
  const [webauthnStatus, setWebauthnStatus] = useState<'idle' | 'waiting' | 'verified' | 'error'>(
    'idle'
  );
  const [webauthnError, setWebauthnError] = useState('');
  const abortRef = useRef<AbortController | null>(null);

  // A refusal that arrives after the key answered means the request it
  // confirmed was turned down, and the server may already have spent the
  // token. "Security key verified" no longer holds, so the key is offered
  // again. Only a CHANGE counts: the parents keep a refusal's text until the
  // next submission, so a key re-run under an earlier refusal stays verified.
  const [refusalSeen, setRefusalSeen] = useState(error);
  if (error !== refusalSeen) {
    setRefusalSeen(error);
    if (error && webauthnStatus === 'verified') setWebauthnStatus('idle');
  }
  const showBackupSwitch = !excludeBackupCodes && available.includes('backup');

  // Abort any pending WebAuthn ceremony on unmount
  useEffect(() => {
    return () => {
      abortRef.current?.abort();
      abortRef.current = null;
    };
  }, []);

  // Build switch links for other available methods (data-driven, no branching)
  const otherMethodLinks = useMemo(() => {
    return METHOD_LINK_DEFS.filter((def) => {
      if (def.mode === mode) return false;
      if (def.mode === 'backup') return showBackupSwitch;
      return available.includes(def.mode);
    });
  }, [available, showBackupSwitch, mode]);

  const handleSwitchMode = useCallback(
    (def: MethodLinkDescriptor) => {
      if (def.resetsWebAuthn) {
        setWebauthnStatus('idle');
        setWebauthnError('');
      } else {
        abortRef.current?.abort();
      }
      // The field being left held the reported code; the one being shown is
      // empty, so the parent's copy must be too.
      onCodeChange?.('');
      setMode(def.mode);
    },
    [setMode, setWebauthnStatus, setWebauthnError, onCodeChange]
  );

  const renderMethodLinks = useCallback(
    () =>
      otherMethodLinks.map((def) => (
        <button
          key={def.mode}
          type="button"
          className="totp-backup-link"
          onClick={() => handleSwitchMode(def)}
          disabled={disabled}
        >
          {def.label}
        </button>
      )),
    [otherMethodLinks, handleSwitchMode, disabled]
  );

  // WebAuthn inline-verify flow
  const handleWebAuthnVerify = useCallback(async () => {
    abortRef.current?.abort();
    const controller = new AbortController();
    abortRef.current = controller;

    setWebauthnStatus('waiting');
    setWebauthnError('');
    try {
      // Step 1: Get assertion options from server, for this prompt's purpose
      // only (the token minted at finish is spendable on no other route).
      const beginRes = await apiFetch('/api/v1/mfa/webauthn/verify-inline/begin', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ purpose }),
      });
      const beginData = await beginRes.json();
      if (!beginRes.ok) throw new Error(beginData.error || 'Failed to start verification');

      // Convert base64url fields for WebAuthn API
      const options = beginData.publicKey;
      options.challenge = base64urlToBuffer(options.challenge);
      if (options.allowCredentials) {
        for (const cred of options.allowCredentials) {
          cred.id = base64urlToBuffer(cred.id);
        }
      }

      // Step 2: Browser WebAuthn ceremony
      const credential = await performWebAuthnAssertion(options, controller.signal);

      // Step 3: Send assertion to server and get MFA token
      const mfaToken = await finishWebAuthnVerification(credential, beginData.challengeToken);
      onVerify(mfaToken);
      // Leave the waiting state: the key has answered and Confirm is live.
      setWebauthnStatus('verified');
    } catch (err) {
      const message = classifyWebAuthnError(err);
      if (message === null) {
        setWebauthnStatus('idle');
        return;
      }
      setWebauthnError(message);
      setWebauthnStatus('error');
    }
  }, [onVerify, purpose]);

  return (
    <div className="mfa-verify-prompt">
      <span className="mfa-verify-prompt-label">MFA Verification</span>

      {mode === 'totp' && (
        <>
          <TOTPInput
            onSubmit={onVerify}
            onCodeChange={onCodeChange}
            disabled={disabled}
            error={error}
            autoFocus
          />
          {renderMethodLinks()}
        </>
      )}

      {mode === 'backup' && !excludeBackupCodes && (
        <>
          <BackupCodeInput
            onSubmit={onVerify}
            onCodeChange={onCodeChange}
            disabled={disabled}
            error={error}
          />
          {renderMethodLinks()}
        </>
      )}

      {mode === 'webauthn' && (
        <div className="mfa-webauthn-inline">
          {webauthnStatus === 'idle' && (
            <>
              {/* Every parent routes a code refusal only to this prompt, so
                  security-key mode must show it as the code inputs do. */}
              {error && (
                <p role="alert" style={REFUSAL_STYLE}>
                  {error}
                </p>
              )}
              <button
                type="button"
                className="btn btn-primary"
                onClick={handleWebAuthnVerify}
                disabled={disabled}
              >
                Verify with security key
              </button>
            </>
          )}
          {webauthnStatus === 'waiting' && (
            <div style={{ textAlign: 'center', padding: '8px 0' }}>
              <svg
                width="32"
                height="32"
                viewBox="0 0 24 24"
                fill="none"
                stroke="currentColor"
                strokeWidth="1.5"
                style={{ margin: '0 auto 8px', display: 'block', opacity: 0.7 }}
              >
                <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
              </svg>
              <p style={{ margin: 0, fontSize: '13px', color: 'var(--text-secondary, #bbb)' }}>
                Touch your security key or use biometrics...
              </p>
            </div>
          )}
          {webauthnStatus === 'verified' && (
            // <output> carries the implicit "status" role, so the confirmation
            // is announced without an ARIA role on a paragraph.
            <output
              style={{
                display: 'block',
                margin: '0 0 8px',
                textAlign: 'center',
                fontSize: '13px',
                color: 'var(--text-primary)',
              }}
            >
              Security key verified
            </output>
          )}
          {webauthnStatus === 'error' && (
            <>
              <p style={REFUSAL_STYLE}>{webauthnError}</p>
              <button
                type="button"
                className="btn btn-primary"
                onClick={handleWebAuthnVerify}
                disabled={disabled}
              >
                Try again
              </button>
            </>
          )}
          {renderMethodLinks()}
        </div>
      )}

      {mode === 'email-sms' && (
        <>
          <TOTPInput
            onSubmit={onVerify}
            onCodeChange={onCodeChange}
            disabled={disabled}
            error={error}
            autoFocus
          />
          {renderMethodLinks()}
        </>
      )}
    </div>
  );
};

export default MFAVerifyPrompt;
