import React, { useState, useEffect, useRef } from 'react';
import QRCode from 'qrcode';
import { apiFetch, refreshAccessToken } from '../../services/system/apiClient';
import { errorMessage } from '../../utils/runtime/redactError';
import { base64urlToBuffer, bufferToBase64url } from '../../utils/crypto/base64url';
import TOTPInput from '../Auth/TOTPInput';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import BackupCodeDisplay from './BackupCodeDisplay';
import RecoveryKeyDisplay from './RecoveryKeyDisplay';
import ErrorBanner, { FieldError } from './ErrorBanner';
import {
  generateRecoveryKey,
  wrapWithRecoveryKey,
  wrapPrefsKeyWithRecoveryKey,
} from '../../utils/crypto/crypto';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { classifyStepUpRefusal } from '../../services/system/stepUpRefusal';
import {
  inlineMfaMethods,
  isStepUpLocked,
  stepUpBanner,
  stepUpCodeMayBeSpent,
  stepUpMfaError,
  stepUpPasswordError,
  stepUpPromptMethods,
  submitMfaStepUp,
  type MfaStepUpResult,
} from './mfaStepUp';

// ── Extracted helpers (reduce cognitive complexity) ───────────────────

export type RecoveryKeyOutcome =
  | { kind: 'created'; key: string }
  | { kind: 'kept' }
  | { kind: 'failed' }
  | { kind: 'unavailable' };

/**
 * The E2EE material a prepared upload wraps. Compared by identity, never by
 * value: the wrapping key is a CryptoKey object and the wrapped private key is
 * the server-stored ciphertext, so no key bytes are copied into this record.
 */
interface RecoverySource {
  wrappingKey: CryptoKey;
  wrappedPrivateKey: string;
  hasPrefsKey: boolean;
}

/** One recovery key and the upload body that wraps it, bound to its source. */
interface PreparedRecovery {
  key: string;
  body: Record<string, string>;
  source: RecoverySource;
}

type Preparation =
  | { kind: 'ready'; prepared: PreparedRecovery }
  /** This device has no unlocked E2EE keys to wrap. */
  | { kind: 'unavailable' }
  | { kind: 'failed' }
  /** The holder was cleared (Back, Done, unmount) while this was in flight. */
  | { kind: 'abandoned' };

/** Where a component keeps a preparation. A ref, never state or a store. */
interface PreparationSlot {
  current: Promise<Preparation> | null;
}

/** This device's recovery-wrappable material, read once. Null when locked. */
function readRecoveryMaterial(): { source: RecoverySource; prefsKeyBase64: string | null } | null {
  const wrappingKey = e2eeService.getWrappingKey();
  const wrappedPrivateKey = e2eeService.getWrappedPrivateKey();
  if (!wrappingKey || !wrappedPrivateKey) return null;
  const prefsKeyBase64 = e2eeService.getPreferencesKeyBase64();
  return {
    source: { wrappingKey, wrappedPrivateKey, hasPrefsKey: prefsKeyBase64 !== null },
    prefsKeyBase64,
  };
}

/** True while `prepared` still wraps what this device holds now. */
function wrapsCurrentKeys(prepared: PreparedRecovery): boolean {
  const now = readRecoveryMaterial()?.source ?? null;
  return (
    now !== null &&
    now.wrappingKey === prepared.source.wrappingKey &&
    now.wrappedPrivateKey === prepared.source.wrappedPrivateKey &&
    now.hasPrefsKey === prepared.source.hasPrefsKey
  );
}

/**
 * Wraps the E2EE keys with a fresh recovery key — two Argon2id (64 MiB)
 * derivations, so it runs once per holder and not once per attempt. Never
 * rejects: a failure is logged by message only (never the key) and resolves
 * to `failed`, where it used to be swallowed by `.catch(() => null)`.
 */
async function prepareForCurrentKeys(): Promise<Preparation> {
  const material = readRecoveryMaterial();
  if (!material) return { kind: 'unavailable' };
  const { source, prefsKeyBase64 } = material;
  try {
    const key = generateRecoveryKey();
    const { wrappedKey, salt } = await wrapWithRecoveryKey(
      source.wrappedPrivateKey,
      source.wrappingKey,
      key
    );
    const body: Record<string, string> = {
      recovery_wrapped_private_key: wrappedKey,
      recovery_key_salt: salt,
    };
    if (prefsKeyBase64) {
      const prefs = await wrapPrefsKeyWithRecoveryKey(prefsKeyBase64, key);
      body.recovery_wrapped_prefs_key = prefs.wrappedKey;
      body.recovery_prefs_key_salt = prefs.salt;
    }
    return { kind: 'ready', prepared: { key, body, source } };
  } catch (err) {
    console.warn('Recovery key preparation failed:', errorMessage(err));
    return { kind: 'failed' };
  }
}

/**
 * The preparation held in `slot`, reused while it still wraps this device's
 * current keys, otherwise a fresh one (F1/F2). Reuse is the point: after an
 * ambiguous outcome — the response lost after the server committed — a retry
 * must resend the SAME bytes, so the key the user is finally shown is the key
 * the server holds. A new key per attempt shows one key while storing another.
 */
async function resolvePreparation(slot: PreparationSlot): Promise<Preparation> {
  const held = slot.current;
  if (held) {
    const settled = await held;
    if (slot.current !== held) return { kind: 'abandoned' };
    if (settled.kind === 'ready' && wrapsCurrentKeys(settled.prepared)) return settled;
  }
  const next = prepareForCurrentKeys();
  slot.current = next;
  const settled = await next;
  return slot.current === next ? settled : { kind: 'abandoned' };
}

/**
 * First-time store after TOTP confirm-setup. Needs no credentials: the server
 * inserts only when no key exists, and answers 200 to a resend of the bytes it
 * already holds. A 403 carrying either step-up flag means a DIFFERENT key
 * already existed and was KEPT — never a silent 'done'.
 */
async function storeRecoveryKey(prepared: PreparedRecovery): Promise<RecoveryKeyOutcome> {
  try {
    const res = await apiFetch('/api/v1/mfa/recovery-key', {
      method: 'PUT',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(prepared.body),
    });
    if (res.ok) return { kind: 'created', key: prepared.key };
    const refusal = classifyStepUpRefusal(res.status, await res.json().catch(() => ({})));
    if (refusal.kind === 'passwordRequired' || refusal.kind === 'mfaRequired') {
      return { kind: 'kept' };
    }
    return { kind: 'failed' };
  } catch (err) {
    console.warn('Recovery key upload failed:', errorMessage(err));
    return { kind: 'failed' };
  }
}

/** Replace-step refusal shown when this device cannot make a key at all. */
const REPLACE_KEYS_LOCKED_MESSAGE =
  "Your encryption keys aren't unlocked on this device, so a new recovery key can't be made here.";

/** An outcome after which the server may or may not have stored the new key. */
function isAmbiguousOutcome(result: MfaStepUpResult): boolean {
  return (
    result.kind === 'networkError' || result.kind === 'failed' || result.kind === 'unavailable'
  );
}

/** What the done screen says about the recovery key (F16). */
type RecoveryNote = 'none' | 'uncertain' | null;

/** Copy for the `'recovery-failed'` step (handoff §1.3). `attempts` puts
 * the count in the text so a retry that fails again visibly changes the
 * screen and re-announces the `role="alert"` region — identical text is
 * neither seen nor re-read, and Try again then looks inert. */
function recoveryFailedCopy(outcome: 'failed' | 'unavailable' | null, attempts: number): string {
  if (outcome === 'unavailable') {
    return "We couldn't create your recovery key because your encryption keys aren't unlocked on this device. Without one, you'll lose access to your encrypted message history if you forget your password.";
  }
  const lead =
    attempts > 1
      ? `We still couldn't create your recovery key after ${attempts} attempts.`
      : "We couldn't create your recovery key.";
  return `${lead} Without one, you'll lose access to your encrypted message history if you forget your password.`;
}

/** Classify a WebAuthn error into a user-friendly message. */
function classifyWebAuthnError(err: unknown): string {
  if (err instanceof DOMException && err.name === 'NotAllowedError') {
    return 'Registration cancelled or timed out. Try again.';
  }
  return err instanceof Error ? err.message : 'Registration failed';
}

/** Classify an error message into the appropriate form field. */
function classifyErrorField(message: string): 'password' | 'mfa' | 'general' {
  const lower = message.toLowerCase();
  if (lower.includes('password')) return 'password';
  if (lower.includes('mfa') || lower.includes('code')) return 'mfa';
  return 'general';
}

/** Convert base64url-encoded fields in WebAuthn options to ArrayBuffers. */
function decodeWebAuthnOptions(
  options: PublicKeyCredentialCreationOptions & Record<string, unknown>
): void {
  options.challenge = base64urlToBuffer(options.challenge as unknown as string);
  const user = options.user as unknown as Record<string, unknown>;
  user.id = base64urlToBuffer(user.id as string);
  if (options.excludeCredentials) {
    for (const cred of options.excludeCredentials as Array<{ id: unknown }>) {
      cred.id = base64urlToBuffer(cred.id as string);
    }
  }
}

/** Determine whether a WebAuthn error should return the user to the password step. */
function shouldResetToPasswordStep(err: unknown, msg: string, currentStep: WebAuthnStep): boolean {
  const isNotAllowed = err instanceof DOMException && err.name === 'NotAllowedError';
  const isBeginStepError =
    currentStep === 'password' ||
    msg.toLowerCase().includes('password') ||
    msg.toLowerCase().includes('mfa');
  return isNotAllowed || isBeginStepError;
}

type SetupMethod = 'totp' | 'webauthn';
type TOTPStep =
  | 'password'
  | 'qr'
  | 'verify'
  | 'backup'
  | 'recovery'
  | 'recovery-kept'
  | 'recovery-failed'
  | 'recovery-replace'
  | 'done';
type WebAuthnStep = 'password' | 'registering' | 'done';

interface MFASetupProps {
  method: SetupMethod;
  credentialType?: 'hardware' | 'platform';
  mfaActive?: boolean; // true if user already has MFA enabled
  activeMethods?: string[]; // raw method strings for MFA challenge
  recoveryOnlyMethods?: string[];
  onComplete: () => void;
  onCancel: () => void;
}

const MFASetup: React.FC<MFASetupProps> = ({
  method,
  credentialType = 'hardware',
  mfaActive,
  activeMethods = [],
  recoveryOnlyMethods = [],
  onComplete,
  onCancel,
}) => {
  // Shared state
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  // Bumped to remount the password step's code prompt empty (see dropSetupCode).
  const [setupPromptKey, setSetupPromptKey] = useState(0);
  const [error, setError] = useState('');
  const [errorField, setErrorField] = useState<'password' | 'mfa' | 'general' | ''>('');
  const [loading, setLoading] = useState(false);

  // TOTP state
  const [totpStep, setTotpStep] = useState<TOTPStep>('password');
  const [otpauthUrl, setOtpauthUrl] = useState('');
  const [totpSecret, setTotpSecret] = useState('');
  const [backupCodes, setBackupCodes] = useState<string[]>([]);
  const [recoveryKey, setRecoveryKey] = useState('');
  const [recoveryLoading, setRecoveryLoading] = useState(false);
  // Which copy the 'recovery-failed' step shows (handoff §1.3).
  const [recoveryOutcome, setRecoveryOutcome] = useState<'failed' | 'unavailable' | null>(null);
  const [recoveryFailures, setRecoveryFailures] = useState(0);
  const [recoveryNote, setRecoveryNote] = useState<RecoveryNote>(null);
  // Methods from an mfa_required answer on the password step. The server's
  // present list wins over `activeMethods`, which can be stale (F3's twin).
  const [setupPromptMethods, setSetupPromptMethods] = useState<string[] | null>(null);

  // Prepared recovery-key uploads (F1/F2). Key material lives in these refs
  // and nowhere else — never state, never a store, never persisted, never
  // logged — and each is dropped on accept, Back, Continue, Done and unmount.
  const firstStorePrepRef = useRef<Promise<Preparation> | null>(null);
  const replacePrepRef = useRef<Promise<Preparation> | null>(null);
  useEffect(
    () => () => {
      firstStorePrepRef.current = null;
      replacePrepRef.current = null;
    },
    []
  );

  // Recovery-key replace state (spec §4.6.4, R-9). A fresh password entry —
  // the wizard's own `password` state is deliberately NOT reused here, since
  // replacing destroys the old key and requires present proof. The banner,
  // the field errors and the lock are all derived from `replaceRefusal`.
  const [replacePassword, setReplacePassword] = useState('');
  const [replaceMfaCode, setReplaceMfaCode] = useState('');
  const [replaceLoading, setReplaceLoading] = useState(false);
  const [replaceRefusal, setReplaceRefusal] = useState<MfaStepUpResult | null>(null);
  const [replaceMfaPromptKey, setReplaceMfaPromptKey] = useState(0);
  // True once a replace attempt ended without a definite answer: the old key
  // may already be gone, so nothing may say it was "left in place".
  const [replaceUncertain, setReplaceUncertain] = useState(false);
  const replacePasswordRef = useRef<HTMLInputElement>(null);

  // Focus the password once a password refusal has rendered and the field is
  // enabled again — focusing while it is still disabled does nothing (F4).
  useEffect(() => {
    if (replaceLoading) return;
    if (replaceRefusal?.kind === 'passwordRequired' || replaceRefusal?.kind === 'invalidPassword') {
      replacePasswordRef.current?.focus();
    }
  }, [replaceLoading, replaceRefusal]);

  // WebAuthn state
  const [webauthnStep, setWebauthnStep] = useState<WebAuthnStep>('password');
  const [credentialName, setCredentialName] = useState('');

  // ── TOTP Flow ──────────────────────────────────────────────────────

  const setFieldError = (message: string) => {
    setError(message);
    setErrorField(classifyErrorField(message));
  };

  /**
   * Drops the password step's code once a submission has settled. The server
   * accepts each code once and can accept it yet still fail the request, so a
   * code that was sent is never offered again: the stored copy is cleared and
   * the prompt remounts empty, which keeps the submit disabled until a fresh
   * code is typed.
   */
  const dropSetupCode = () => {
    setMfaCode('');
    setSetupPromptKey((k) => k + 1);
  };

  /**
   * Applies a begin-step step-up refusal (TOTP setup / WebAuthn register
   * begin) to the shared error state, using the same field routing and copy
   * as every other step-up surface (`mfaStepUp.ts`) instead of the server's
   * raw text. An mfa_required answer also names the methods to prompt for,
   * even when the status this wizard opened with said MFA was off.
   */
  const noteSetupRefusal = (status: number, body: unknown) => {
    const refusal = classifyStepUpRefusal(status, body);
    if (refusal.kind === 'mfaRequired') setSetupPromptMethods(refusal.methods);
    const passwordError = stepUpPasswordError(refusal);
    const mfaError = stepUpMfaError(refusal);
    if (passwordError !== undefined) {
      setError(passwordError);
      setErrorField('password');
    } else if (mfaError === undefined) {
      setError(stepUpBanner(refusal) ?? 'Something went wrong. Try again.');
      setErrorField('general');
    } else {
      setError(mfaError);
      setErrorField('mfa');
    }
  };

  const handleTOTPSetup = async () => {
    setLoading(true);
    setError('');
    setErrorField('');
    try {
      const res = await apiFetch('/api/v1/mfa/totp/setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ password, ...(mfaCode ? { mfa_code: mfaCode } : {}) }),
      });
      const data = await res.json();
      if (!res.ok) {
        noteSetupRefusal(res.status, data);
        return;
      }

      setOtpauthUrl(data.otpauth_url);
      setTotpSecret(data.secret);
      setTotpStep('qr');
    } catch (err) {
      setFieldError(err instanceof Error ? err.message : 'Setup failed');
    } finally {
      dropSetupCode();
      setLoading(false);
    }
  };

  const handleTOTPVerify = async (code: string) => {
    setLoading(true);
    setError('');
    try {
      const res = await apiFetch('/api/v1/mfa/totp/verify-setup', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ code }),
      });
      const data = await res.json();
      if (!res.ok) throw new Error(data.error || 'Verification failed');

      setBackupCodes(data.backup_codes || []);
      setTotpStep('backup');
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Invalid code');
    } finally {
      setLoading(false);
    }
  };

  const confirmTOTPSetup = async () => {
    const res = await apiFetch('/api/v1/mfa/totp/confirm-setup', {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
    });
    if (!res.ok) {
      const data = await res.json();
      throw new Error(data.error || 'Confirmation failed');
    }
  };

  /** Dispatches a RecoveryKeyOutcome to its step — shared by the automatic
   * post-confirm upload and by a retry, so both route identically. Never
   * falls silently to `'done'`: every outcome has its own step. */
  const applyRecoveryOutcome = (outcome: RecoveryKeyOutcome) => {
    switch (outcome.kind) {
      case 'created':
        setRecoveryKey(outcome.key);
        setTotpStep('recovery');
        break;
      case 'kept':
        setTotpStep('recovery-kept');
        break;
      case 'failed':
      case 'unavailable':
        setRecoveryOutcome(outcome.kind);
        setRecoveryFailures((n) => n + 1);
        setTotpStep('recovery-failed');
        break;
    }
  };

  /** Prepares once and stores. A failed store keeps the prepared bytes so Try
   * again resends them; every settled outcome drops them. */
  const runFirstStore = async (): Promise<RecoveryKeyOutcome> => {
    const prep = await resolvePreparation(firstStorePrepRef);
    if (prep.kind === 'unavailable') return { kind: 'unavailable' };
    if (prep.kind !== 'ready') return { kind: 'failed' };
    const outcome = await storeRecoveryKey(prep.prepared);
    if (outcome.kind !== 'failed') firstStorePrepRef.current = null;
    return outcome;
  };

  const handleTOTPConfirm = async () => {
    setLoading(true);
    setError('');
    try {
      await confirmTOTPSetup();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Confirmation failed');
      setLoading(false);
      return;
    }
    // The server exempts this session from the pre-MFA challenge for 30 s after
    // a first enrollment. Refresh now to use it, rather than being asked for the
    // code again at the next token refresh. Not awaited: if no exemption was
    // granted, the refresh raises that challenge, which must not block setup.
    // A refresh already in flight is reused instead; it predates the grant, so
    // that session is challenged once at its next refresh, as before this fix.
    void refreshAccessToken().catch(() => console.warn('[mfa] Refresh after enrollment failed'));

    // runFirstStore never throws — every failure is already a
    // RecoveryKeyOutcome — so no surrounding try/catch is needed here.
    setRecoveryLoading(true);
    const outcome = await runFirstStore();
    setRecoveryLoading(false);
    setLoading(false);
    applyRecoveryOutcome(outcome);
  };

  /** Retry after `'recovery-failed'`. Calls only the key upload — confirm-
   * setup has already committed and must never be called again. */
  const handleRecoveryRetry = async () => {
    setRecoveryLoading(true);
    const outcome = await runFirstStore();
    setRecoveryLoading(false);
    applyRecoveryOutcome(outcome);
  };

  /** Leaves the recovery steps for 'done', dropping every copy of key material. */
  const finishRecovery = (note: RecoveryNote) => {
    firstStorePrepRef.current = null;
    replacePrepRef.current = null;
    setRecoveryKey('');
    setRecoveryNote(note);
    setTotpStep('done');
  };

  /** Opens the replace step and starts preparing the new key while the user
   * types their credentials (F1). A lock — a spent budget or a dead session —
   * survives Back and reopen; only a new wizard clears it (F10). */
  const handleOpenReplace = () => {
    setReplacePassword('');
    setReplaceMfaCode('');
    setReplaceRefusal((prev) => (isStepUpLocked(prev) ? prev : null));
    replacePrepRef.current = prepareForCurrentKeys();
    setTotpStep('recovery-replace');
  };

  const handleReplaceBack = () => {
    replacePrepRef.current = null;
    setTotpStep('recovery-kept');
  };

  /** Records a refusal to a SENT replace request, mirroring the action modal:
   * the password is cleared only when it was refused (handoff §1.1), the code
   * whenever the server may have used it up. Display is derived at render. */
  const applyReplaceRefusal = (result: MfaStepUpResult) => {
    setReplaceRefusal(result);
    if (result.kind === 'passwordRequired' || result.kind === 'invalidPassword') {
      setReplacePassword('');
    }
    if (stepUpCodeMayBeSpent(result)) {
      setReplaceMfaPromptKey((k) => k + 1);
      setReplaceMfaCode('');
    }
  };

  const handleReplaceRecoveryKey = async () => {
    setReplaceLoading(true);
    try {
      const prep = await resolvePreparation(replacePrepRef);
      if (prep.kind === 'abandoned') return;
      if (prep.kind !== 'ready') {
        // Nothing was sent, so this is a refusal, not an ambiguous outcome,
        // and the typed code is still unused — it stays.
        setReplaceRefusal(
          prep.kind === 'unavailable'
            ? { kind: 'failed', message: REPLACE_KEYS_LOCKED_MESSAGE }
            : { kind: 'failed' }
        );
        return;
      }
      const result = await submitMfaStepUp('/api/v1/mfa/recovery-key', 'PUT', prep.prepared.body, {
        password: replacePassword,
        mfaCode: replaceMfaCode,
      });
      if (result.kind === 'accepted') {
        replacePrepRef.current = null;
        setReplaceUncertain(false);
        setReplaceRefusal(null);
        setRecoveryKey(prep.prepared.key);
        setTotpStep('recovery');
        return;
      }
      if (isAmbiguousOutcome(result)) setReplaceUncertain(true);
      applyReplaceRefusal(result);
    } finally {
      setReplaceLoading(false);
    }
  };

  // ── WebAuthn Flow ──────────────────────────────────────────────────

  const handleWebAuthnRegister = async () => {
    setLoading(true);
    setError('');
    setErrorField('');
    try {
      // Begin registration — validate password and get challenge from server
      const beginRes = await apiFetch('/api/v1/mfa/webauthn/register/begin', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          password,
          ...(mfaCode ? { mfa_code: mfaCode } : {}),
          credential_name: credentialName || 'Security Key',
          credential_type: credentialType,
        }),
      });
      const beginData = await beginRes.json();
      if (!beginRes.ok) {
        noteSetupRefusal(beginRes.status, beginData);
        return;
      }

      // Convert base64url fields for WebAuthn API
      const options = beginData.publicKey;
      decodeWebAuthnOptions(options);

      // Show the "waiting for key" step before triggering the browser dialog
      setWebauthnStep('registering');

      // Call browser WebAuthn API with timeout protection
      const credential = (await Promise.race([
        navigator.credentials.create({ publicKey: options }),
        new Promise<never>((_, reject) =>
          setTimeout(
            () =>
              reject(
                new Error(
                  'Security key registration timed out. Make sure your key is connected and try again.'
                )
              ),
            60000
          )
        ),
      ])) as PublicKeyCredential;
      if (!credential) throw new Error('No credential returned');

      const attestation = credential.response as AuthenticatorAttestationResponse;

      // Finish registration by sending the attestation to the server
      const finishRes = await apiFetch('/api/v1/mfa/webauthn/register/finish', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          id: credential.id,
          rawId: bufferToBase64url(credential.rawId),
          type: credential.type,
          response: {
            attestationObject: bufferToBase64url(attestation.attestationObject),
            clientDataJSON: bufferToBase64url(attestation.clientDataJSON),
          },
          credential_name: credentialName || 'Security Key',
        }),
      });
      if (!finishRes.ok) {
        const finishData = await finishRes.json();
        throw new Error(finishData.error || 'Registration failed');
      }
      // Uses the enrollment exemption, as after TOTP confirm.
      void refreshAccessToken().catch(() => console.warn('[mfa] Refresh after enrollment failed'));
      setWebauthnStep('done');
    } catch (err) {
      const msg = classifyWebAuthnError(err);
      setFieldError(msg);

      // Go back to password step for cancellation or credential errors
      if (shouldResetToPasswordStep(err, msg, webauthnStep)) {
        setWebauthnStep('password');
      }
      // Otherwise stay on 'registering' step so the error banner is clearly visible
    } finally {
      dropSetupCode();
      setLoading(false);
    }
  };

  // ── Render helpers (reduce cognitive complexity) ────────────────────

  // The password step asks for a code when the account is known to hold MFA,
  // or when the server has just said it does.
  const showSetupPrompt = mfaActive === true || setupPromptMethods !== null;
  const replacePasswordError = stepUpPasswordError(replaceRefusal);

  // Shared password + MFA-verify step used by both the TOTP and WebAuthn
  // flows — they differ only in copy, an optional extra field, and the
  // submit action.
  const renderPasswordVerifyStep = (opts: {
    intro: string;
    activeIntro: string;
    submitLabel: string;
    busyLabel: string;
    onSubmit: () => void;
    extraFields?: React.ReactNode;
  }) => (
    <div className="mfa-setup-step">
      <p>{showSetupPrompt ? opts.activeIntro : opts.intro}</p>
      <input
        type="password"
        className={`form-input ${errorField === 'password' ? 'error' : ''}`}
        value={password}
        onChange={(e) => setPassword(e.target.value)}
        placeholder="Your password"
        disabled={loading}
        autoFocus
      />
      {showSetupPrompt && (
        <MFAVerifyPrompt
          key={setupPromptKey}
          methods={inlineMfaMethods(setupPromptMethods ?? activeMethods)}
          recoveryOnlyMethods={recoveryOnlyMethods}
          onVerify={setMfaCode}
          onCodeChange={setMfaCode}
          disabled={loading}
          error={errorField === 'mfa' ? error : undefined}
          excludeBackupCodes
        />
      )}
      {opts.extraFields}
      <ErrorBanner error={error} errorField={errorField} />
      <div className="mfa-setup-actions">
        <button
          className="btn btn-primary"
          onClick={opts.onSubmit}
          disabled={loading || !password || (showSetupPrompt && !mfaCode)}
        >
          {loading ? opts.busyLabel : opts.submitLabel}
        </button>
        <button className="btn btn-secondary" onClick={onCancel}>
          Cancel
        </button>
      </div>
    </div>
  );

  const renderTOTPPasswordStep = () =>
    renderPasswordVerifyStep({
      intro: 'Enter your password to begin setup.',
      activeIntro: 'Verify your identity to add another method.',
      submitLabel: 'Continue',
      busyLabel: 'Setting up...',
      onSubmit: handleTOTPSetup,
    });

  const renderWebAuthnPasswordStep = () =>
    renderPasswordVerifyStep({
      intro: 'Enter your password and name your key.',
      activeIntro: 'Verify your identity and name your key.',
      submitLabel: 'Register Key',
      busyLabel: 'Registering...',
      onSubmit: handleWebAuthnRegister,
      extraFields: (
        <input
          type="text"
          className="form-input"
          value={credentialName}
          onChange={(e) => setCredentialName(e.target.value)}
          placeholder={
            credentialType === 'platform'
              ? 'Key name (e.g. MacBook Touch ID, Windows Hello)'
              : 'Key name (e.g. YubiKey 5, Google Titan)'
          }
          disabled={loading}
        />
      ),
    });

  const handleResetToPassword = () => {
    setWebauthnStep('password');
    setError('');
  };

  const renderWebAuthnRegisteringStep = () => (
    <div className="mfa-setup-step" style={{ alignItems: 'center' }}>
      <div style={{ textAlign: 'center', padding: '20px 0' }}>
        {error ? <ErrorBanner error={error} size={20} /> : <WebAuthnWaitingPrompt />}
      </div>
      <div className="mfa-setup-actions" style={{ justifyContent: 'center' }}>
        {error ? (
          <button className="btn btn-primary" onClick={handleResetToPassword}>
            Try Again
          </button>
        ) : (
          <button className="btn btn-secondary" onClick={handleResetToPassword}>
            Cancel
          </button>
        )}
      </div>
    </div>
  );

  // ── Render ─────────────────────────────────────────────────────────

  if (method === 'totp') {
    return (
      <div className="mfa-setup-wizard">
        <h3>Set Up Authenticator App</h3>

        {totpStep === 'password' && renderTOTPPasswordStep()}

        {totpStep === 'qr' && (
          <div className="mfa-setup-step">
            <p>Scan this QR code with your authenticator app, then enter the 6-digit code below.</p>
            <QRCodeCanvas data={otpauthUrl} />
            <details className="mfa-manual-entry">
              <summary>Can&apos;t scan? Enter manually</summary>
              <code className="mfa-secret-display">{totpSecret}</code>
            </details>
            <TOTPInput onSubmit={handleTOTPVerify} disabled={loading} error={error} />
            <div className="mfa-setup-actions">
              <button className="btn btn-secondary" onClick={onCancel}>
                Cancel
              </button>
            </div>
          </div>
        )}

        {totpStep === 'backup' && (
          <div className="mfa-setup-step">
            <p>
              Save these backup codes. They&apos;re your safety net if you lose access to your
              authenticator.
            </p>
            <BackupCodeDisplay
              codes={backupCodes}
              onConfirm={handleTOTPConfirm}
              disabled={loading}
            />
            <ErrorBanner error={error} />
          </div>
        )}

        {totpStep === 'recovery' && (
          <div className="mfa-setup-step">
            <p>
              Save your recovery key. This is the <strong>only way</strong> to recover your
              encrypted messages if you lose your password.
            </p>
            <RecoveryKeyDisplay
              recoveryKey={recoveryKey}
              onConfirm={() => finishRecovery(null)}
              onSkip={() => finishRecovery(null)}
              disabled={recoveryLoading}
            />
            <ErrorBanner error={error} />
          </div>
        )}

        {totpStep === 'recovery-kept' && (
          <RecoveryKeptStep
            replaceUncertain={replaceUncertain}
            onOpenReplace={handleOpenReplace}
            onFinish={finishRecovery}
          />
        )}

        {totpStep === 'recovery-failed' && (
          <div className="mfa-setup-step">
            <ErrorBanner error={recoveryFailedCopy(recoveryOutcome, recoveryFailures)} />
            <div className="mfa-setup-actions">
              {recoveryOutcome === 'failed' && (
                <button
                  className="btn btn-primary"
                  autoFocus
                  disabled={recoveryLoading}
                  onClick={() => void handleRecoveryRetry()}
                >
                  {recoveryLoading ? 'Trying again...' : 'Try again'}
                </button>
              )}
              <button
                className={
                  recoveryOutcome === 'unavailable' ? 'btn btn-primary' : 'btn btn-secondary'
                }
                autoFocus={recoveryOutcome === 'unavailable'}
                onClick={() => finishRecovery('none')}
              >
                {recoveryOutcome === 'failed' ? 'Continue without a recovery key' : 'Continue'}
              </button>
            </div>
          </div>
        )}

        {totpStep === 'recovery-replace' && (
          <div className="mfa-setup-step">
            <p>
              Make a new recovery key. Confirm it&apos;s you with your password and a code from your
              authenticator app.
            </p>
            <p className="mfa-modal-desc">Your old recovery key will stop working.</p>
            <div className="mfa-verify-field">
              <label htmlFor="mfa-replace-password">Password</label>
              <input
                id="mfa-replace-password"
                ref={replacePasswordRef}
                type="password"
                autoComplete="current-password"
                value={replacePassword}
                onChange={(e) => setReplacePassword(e.target.value)}
                placeholder="Your password"
                disabled={replaceLoading}
                aria-invalid={replacePasswordError !== undefined}
                aria-describedby={replacePasswordError ? 'mfa-replace-password-error' : undefined}
                autoFocus
              />
              {replacePasswordError && (
                <FieldError id="mfa-replace-password-error">{replacePasswordError}</FieldError>
              )}
            </div>
            <MFAVerifyPrompt
              key={replaceMfaPromptKey}
              methods={stepUpPromptMethods(replaceRefusal, ['totp'])}
              onVerify={setReplaceMfaCode}
              onCodeChange={setReplaceMfaCode}
              disabled={replaceLoading}
              error={stepUpMfaError(replaceRefusal)}
            />
            <ErrorBanner error={stepUpBanner(replaceRefusal) ?? ''} />
            <div className="mfa-setup-actions">
              <button
                className="btn btn-danger"
                onClick={() => void handleReplaceRecoveryKey()}
                disabled={
                  replaceLoading ||
                  isStepUpLocked(replaceRefusal) ||
                  !replacePassword ||
                  !replaceMfaCode
                }
              >
                {replaceLoading ? 'Replacing...' : 'Replace recovery key'}
              </button>
              <button
                className="btn btn-secondary"
                onClick={handleReplaceBack}
                disabled={replaceLoading}
              >
                Back
              </button>
            </div>
          </div>
        )}

        {totpStep === 'done' && (
          <div className="mfa-setup-step mfa-setup-success">
            <h4>MFA Activated!</h4>
            <p>Your authenticator app is now protecting your account.</p>
            {recoveryNote === 'none' && (
              <p>
                This account has no recovery key you can use. Without one, you&apos;ll lose access
                to your encrypted message history if you forget your password.
              </p>
            )}
            {recoveryNote === 'uncertain' && (
              <p>
                We couldn&apos;t confirm your recovery key was replaced, so the one you have may no
                longer work.
              </p>
            )}
            <button className="btn btn-primary" onClick={onComplete}>
              Done
            </button>
          </div>
        )}
      </div>
    );
  }

  // WebAuthn flow
  return (
    <div className="mfa-setup-wizard">
      <h3>
        {credentialType === 'platform' ? 'Set Up Platform Authenticator' : 'Set Up Security Key'}
      </h3>

      {webauthnStep === 'password' && renderWebAuthnPasswordStep()}

      {webauthnStep === 'registering' && renderWebAuthnRegisteringStep()}

      {webauthnStep === 'done' && (
        <div className="mfa-setup-step mfa-setup-success">
          <h4>Security Key Registered!</h4>
          <p>Your security key is now active and protecting your account.</p>
          <button className="btn btn-primary" onClick={onComplete}>
            Done
          </button>
        </div>
      )}
    </div>
  );
};

// ── Extracted sub-components (reduce cognitive complexity) ──────────

interface RecoveryKeptStepProps {
  /** True once a replace attempt ended without a definite answer — the old
   * key may already be gone, so nothing here may say it was left in place. */
  replaceUncertain: boolean;
  onOpenReplace: () => void;
  onFinish: (note: RecoveryNote) => void;
}

/**
 * The 'recovery-kept' TOTP step: an existing recovery key was left in place,
 * or a prior replace attempt ended ambiguously. Extracted from {@link MFASetup}
 * to keep its cognitive complexity down (SonarCloud typescript:S3776).
 */
const RecoveryKeptStep: React.FC<RecoveryKeptStepProps> = ({
  replaceUncertain,
  onOpenReplace,
  onFinish,
}) => (
  <div className="mfa-setup-step">
    <output className="mfa-setup-info">
      <svg
        width={16}
        height={16}
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth="2"
        aria-hidden="true"
        focusable="false"
        style={{ flexShrink: 0 }}
      >
        <circle cx="12" cy="12" r="10" />
        <line x1="12" y1="16" x2="12" y2="12" />
        <line x1="12" y1="8" x2="12.01" y2="8" />
      </svg>
      {replaceUncertain ? (
        <span>
          We couldn&apos;t confirm whether your recovery key was replaced, so the one you have may
          no longer work. Finish replacing it to get a key you know works.
        </span>
      ) : (
        <span>
          A recovery key is already saved for your account, so we left it in place. It&apos;s the
          only way back into your encrypted messages if you forget your password. If you don&apos;t
          have that key, replace it now.
        </span>
      )}
    </output>
    <div className="mfa-setup-actions">
      {replaceUncertain ? (
        <>
          <button className="btn btn-primary" autoFocus onClick={onOpenReplace}>
            Finish replacing
          </button>
          <button className="btn btn-secondary" onClick={() => onFinish('uncertain')}>
            Continue
          </button>
        </>
      ) : (
        <>
          <button className="btn btn-primary" autoFocus onClick={() => onFinish(null)}>
            Continue
          </button>
          <button className="btn btn-secondary" onClick={onOpenReplace}>
            Replace recovery key
          </button>
        </>
      )}
    </div>
  </div>
);

/** Waiting prompt shown during WebAuthn key registration. */
const WebAuthnWaitingPrompt: React.FC = () => (
  <>
    <svg
      width="48"
      height="48"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.5"
      style={{ margin: '0 auto 16px', display: 'block' }}
    >
      <path d="M15 7a2 2 0 012 2m4 0a6 6 0 01-7.743 5.743L11 17H9v2H7v2H4a1 1 0 01-1-1v-2.586a1 1 0 01.293-.707l5.964-5.964A6 6 0 1121 9z" />
    </svg>
    <p>
      <strong>Waiting for your security key...</strong>
    </p>
    <p className="mfa-modal-desc">
      Touch your security key, use your fingerprint reader, or follow your browser&apos;s prompt.
    </p>
  </>
);

// ── QR Code component (renders locally via canvas → data URL) ──────────

const QRCodeCanvas: React.FC<{ data: string }> = ({ data }) => {
  const [dataUrl, setDataUrl] = useState('');
  const mountedRef = useRef(true);

  useEffect(() => {
    mountedRef.current = true;
    QRCode.toDataURL(data, { width: 200, margin: 2 }).then((url) => {
      if (mountedRef.current) setDataUrl(url);
    });
    return () => {
      mountedRef.current = false;
    };
  }, [data]);

  return (
    <div className="mfa-qr-container">
      {dataUrl ? (
        <img src={dataUrl} alt="TOTP QR Code" width={200} height={200} className="mfa-qr-image" />
      ) : (
        <div
          style={{
            width: 200,
            height: 200,
            display: 'flex',
            alignItems: 'center',
            justifyContent: 'center',
          }}
        >
          Generating...
        </div>
      )}
    </div>
  );
};

export default MFASetup;
