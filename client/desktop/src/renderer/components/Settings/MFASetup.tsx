import React, { useState, useEffect, useId, useRef } from 'react';
import QRCode from 'qrcode';
import { apiFetch, refreshAccessToken } from '../../services/system/apiClient';
import {
  apiFetchInContext,
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { errorMessage } from '../../utils/runtime/redactError';
import { base64urlToBuffer, bufferToBase64url } from '../../utils/crypto/base64url';
import TOTPInput from '../Auth/TOTPInput';
import LoadingSpinner from '../Auth/LoadingSpinner';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import {
  useStepUpFactor,
  type StepUpFactor,
  type StepUpSubmit,
} from '../../hooks/auth/useStepUpFactor';
import BackupCodeDisplay from './BackupCodeDisplay';
import RecoveryKeyDisplay from './RecoveryKeyDisplay';
import ErrorBanner from './ErrorBanner';
import {
  generateRecoveryKey,
  wrapWithRecoveryKey,
  wrapPrefsKeyWithRecoveryKey,
} from '../../utils/crypto/crypto';
import { e2eeService } from '../../services/e2ee/e2eeService';
import { classifyStepUpRefusal } from '../../services/system/stepUpRefusal';
import {
  isStepUpLocked,
  stepUpBanner,
  submitMfaStepUp,
  toStepUpSubmitOutcome,
  type MfaSeamHandler,
  type MfaStepUpResult,
} from './mfaStepUp';
import { refusalText } from './mfaResponse';

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

/**
 * The replace step's preparation, held in a ref so key material never reaches
 * state. `settled` is read synchronously by the send, which therefore awaits
 * nothing: the primary stays down until the preparation has landed (D11, Q6).
 * The holder's identity is how a late result learns it was abandoned.
 */
interface ReplaceHold {
  settled: Preparation | null;
}

/** What the replace step renders from: no key material, only when it may act. */
interface ReplaceSession {
  /** Taken when the step opened, before the preparation read the keys (C82). */
  capture: ApiRequestContext;
  preparing: boolean;
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
 * already existed and was KEPT — never a silent 'done'. Sent under `context`,
 * taken before the keys were read, so a change of account or server since then
 * sends nothing and reads as `failed`.
 */
async function storeRecoveryKey(
  prepared: PreparedRecovery,
  context: ApiRequestContext
): Promise<RecoveryKeyOutcome> {
  try {
    const res = await apiFetchInContext(
      '/api/v1/mfa/recovery-key',
      {
        method: 'PUT',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify(prepared.body),
      },
      context
    );
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

/** What a send that found nothing it could send reports. Nothing left the device. */
function unpreparedRefusal(settled: Preparation | null): MfaStepUpResult {
  return settled?.kind === 'unavailable'
    ? { kind: 'failed', message: REPLACE_KEYS_LOCKED_MESSAGE }
    : { kind: 'failed' };
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

type CreationOptions = PublicKeyCredentialCreationOptions & Record<string, unknown>;

/** The backup codes verify-setup answers with; none for any other body. */
function readBackupCodes(data: unknown): string[] {
  if (typeof data !== 'object' || data === null || !('backup_codes' in data)) return [];
  const { backup_codes: codes } = data;
  return Array.isArray(codes) ? codes.filter((c): c is string => typeof c === 'string') : [];
}

/** The secret and URL TOTP setup answers with, or null for any other body. */
function readTotpSetup(data: unknown): { otpauthUrl: string; secret: string } | null {
  if (typeof data !== 'object' || data === null) return null;
  if (!('otpauth_url' in data) || !('secret' in data)) return null;
  const { otpauth_url: otpauthUrl, secret } = data;
  return typeof otpauthUrl === 'string' && typeof secret === 'string' // pragma: allowlist secret
    ? { otpauthUrl, secret }
    : null;
}

/** The creation options registration begin answers with, or null for any other body. */
function readCreationOptions(data: unknown): CreationOptions | null {
  if (typeof data !== 'object' || data === null || !('publicKey' in data)) return null;
  const { publicKey } = data;
  return typeof publicKey === 'object' && publicKey !== null
    ? (publicKey as CreationOptions)
    : null;
}

/** Convert base64url-encoded fields in WebAuthn options to ArrayBuffers. */
function decodeWebAuthnOptions(options: CreationOptions): void {
  options.challenge = base64urlToBuffer(options.challenge as unknown as string);
  const user = options.user as unknown as Record<string, unknown>;
  user.id = base64urlToBuffer(user.id as string);
  if (options.excludeCredentials) {
    for (const cred of options.excludeCredentials as Array<{ id: unknown }>) {
      cred.id = base64urlToBuffer(cred.id as string);
    }
  }
}

/** How long registration waits for the key before it gives up. */
const REGISTRATION_TIMEOUT_MS = 60000;

/**
 * Creates the credential in the browser, giving up if the key never answers.
 * Aborting `signal` closes the browser's prompt and ends the wait at once. The
 * timeout is cleared however the wait ends, so no timer outlives it.
 */
async function createCredential(
  options: CreationOptions,
  signal: AbortSignal
): Promise<PublicKeyCredential> {
  // The browser's ceremony gets its own controller, so the timeout can end the
  // prompt itself rather than only giving up on it: a prompt left open behind a
  // wizard that has moved on could still mint a credential nothing finishes.
  const ceremony = new AbortController();
  const follow = () => ceremony.abort(signal.reason);
  if (signal.aborted) follow();
  else signal.addEventListener('abort', follow, { once: true });
  let timer: ReturnType<typeof setTimeout> | undefined;
  const givenUp = new Promise<never>((_, reject) => {
    timer = setTimeout(
      () =>
        ceremony.abort(
          new Error(
            'Security key registration timed out. Make sure your key is connected and try again.'
          )
        ),
      REGISTRATION_TIMEOUT_MS
    );
    ceremony.signal.addEventListener('abort', () => reject(ceremony.signal.reason), {
      once: true,
    });
  });
  try {
    const credential = (await Promise.race([
      navigator.credentials.create({ publicKey: options, signal: ceremony.signal }),
      givenUp,
    ])) as PublicKeyCredential | null;
    if (!credential) throw new Error('No credential returned');
    return credential;
  } finally {
    clearTimeout(timer);
    signal.removeEventListener('abort', follow);
  }
}

/**
 * Sends the attestation to the server, bound to the capture the begin request
 * ran under: the registration session it finishes belongs to that account.
 */
async function sendRegistrationFinish(
  credential: PublicKeyCredential,
  credentialName: string,
  context: ApiRequestContext
): Promise<void> {
  const attestation = credential.response as AuthenticatorAttestationResponse;
  const finishRes = await apiFetchInContext(
    '/api/v1/mfa/webauthn/register/finish',
    {
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
        credential_name: credentialName,
      }),
    },
    context
  );
  if (!finishRes.ok) throw new Error(await refusalText(finishRes, 'Registration failed'));
}

/**
 * A seam handler as the factor hook's `submit`. Only the password the server
 * refused is dropped: the hook keeps the code through a password refusal. A
 * refusal for an account or server that is no longer current belongs to the
 * old one, so it changes nothing here.
 */
function toSeamSubmit(
  send: MfaSeamHandler,
  password: string,
  dropPassword: () => void
): StepUpSubmit {
  return async (mfa, context) => {
    const result = await send(password, mfa, context);
    if (result.kind === 'invalidPassword' && apiRequestContextIsCurrent(context)) dropPassword();
    return toStepUpSubmitOutcome(result);
  };
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
  /**
   * True if the user already has MFA enabled. Wording only: which code the
   * step asks for comes from `GET /mfa/step-up`, never from this.
   */
  mfaActive?: boolean;
  onComplete: () => void;
  onCancel: () => void;
}

const MFASetup: React.FC<MFASetupProps> = ({
  method,
  credentialType = 'hardware',
  mfaActive,
  onComplete,
  onCancel,
}) => {
  // Shared state
  const [error, setError] = useState('');
  const [loading, setLoading] = useState(false);
  // The stage heading takes focus when a credential step ends in a terminal
  // state (StepUpCredentials), so the wizard's own heading is that target.
  const headingRef = useRef<HTMLHeadingElement>(null);

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

  // Prepared recovery-key uploads (F1/F2). Key material lives in these refs
  // and nowhere else — never state, never a store, never persisted, never
  // logged — and each is dropped on accept, Back, Continue, Done and unmount.
  const firstStorePrepRef = useRef<Promise<Preparation> | null>(null);
  const replacePrepRef = useRef<ReplaceHold | null>(null);
  // The security-key ceremony in flight, held the same way: Cancel and unmount
  // abort and drop it, and a ceremony that finds itself dropped sends nothing more.
  const ceremonyRef = useRef<AbortController | null>(null);
  useEffect(
    () => () => {
      firstStorePrepRef.current = null;
      replacePrepRef.current = null;
      ceremonyRef.current?.abort();
      ceremonyRef.current = null;
    },
    []
  );

  // Recovery-key replace state (spec §4.6.4, R-9). Replacing destroys the old
  // key and requires present proof, so the step collects its own credentials
  // (RecoveryReplaceStage) and the wizard holds none. The banner and the lock
  // are both derived from `replaceRefusal`.
  const [replaceSession, setReplaceSession] = useState<ReplaceSession | null>(null);
  const [replaceRefusal, setReplaceRefusal] = useState<MfaStepUpResult | null>(null);
  // True once a replace attempt ended without a definite answer: the old key
  // may already be gone, so nothing may say it was "left in place".
  const [replaceUncertain, setReplaceUncertain] = useState(false);

  // WebAuthn state
  const [webauthnStep, setWebauthnStep] = useState<WebAuthnStep>('password');
  const [credentialName, setCredentialName] = useState('');

  // ── TOTP Flow ──────────────────────────────────────────────────────

  /**
   * Shows a begin answer that cannot go on and returns what it came to. An
   * accepted answer without the body the next step needs is a failed begin.
   */
  const refuseBegin = (result: MfaStepUpResult): MfaStepUpResult => {
    const shown: MfaStepUpResult = result.kind === 'accepted' ? { kind: 'failed' } : result;
    setError(stepUpBanner(shown) ?? '');
    return shown;
  };

  /**
   * TOTP setup's step-up (#5): the one request the credentials gate. It runs
   * under `run`'s capture, and everything after its await belongs to the
   * account that sent it.
   */
  const beginTotpSetup: MfaSeamHandler = async (password, mfaCode, context) => {
    setError('');
    const result = await submitMfaStepUp(
      '/api/v1/mfa/totp/setup',
      'POST',
      {},
      { password, mfaCode },
      { context }
    );
    if (!apiRequestContextIsCurrent(context)) return result;
    const setup = result.kind === 'accepted' ? readTotpSetup(result.data) : null;
    if (setup === null) return refuseBegin(result);
    setOtpauthUrl(setup.otpauthUrl);
    setTotpSecret(setup.secret);
    setTotpStep('qr');
    return result;
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
      if (!res.ok) throw new Error(await refusalText(res, 'Verification failed'));
      setBackupCodes(readBackupCodes(await res.json().catch(() => ({}))));
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
    if (!res.ok) throw new Error(await refusalText(res, 'Confirmation failed'));
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
   * again resends them; every settled outcome drops them. The capture comes
   * first, as the replace step's does (C82): bytes wrapped for one account are
   * never sent as another. */
  const runFirstStore = async (): Promise<RecoveryKeyOutcome> => {
    const capture = captureApiRequestContext();
    const prep = await resolvePreparation(firstStorePrepRef);
    if (prep.kind === 'unavailable') return { kind: 'unavailable' };
    if (prep.kind !== 'ready') return { kind: 'failed' };
    const outcome = await storeRecoveryKey(prep.prepared, capture);
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

  /**
   * Wraps a new key and holds it in a ref, so every attempt of this step sends
   * the SAME bytes (F1/F2). The primary stays down until it lands, which is
   * what lets the activation call `run` with nothing awaited in front of it
   * (D11, Q6). `capture` is the one `run` works against: taken before the
   * keys are read, so bytes wrapped for one account are never sent as another.
   */
  const startReplacePreparation = (capture: ApiRequestContext) => {
    const hold: ReplaceHold = { settled: null };
    replacePrepRef.current = hold;
    setReplaceSession({ capture, preparing: true });
    void prepareForCurrentKeys().then((settled) => {
      // Back, Done, unmount, or a newer preparation: nobody is waiting for this.
      if (replacePrepRef.current !== hold) return;
      hold.settled = settled;
      setReplaceSession((session) => session && { ...session, preparing: false });
    });
  };

  /** Opens the replace step and starts preparing the new key while the user
   * types their credentials (F1). A lock — a spent budget or a dead session —
   * survives Back and reopen; only a new wizard clears it (F10). */
  const handleOpenReplace = () => {
    setReplaceRefusal((prev) => (isStepUpLocked(prev) ? prev : null));
    startReplacePreparation(captureApiRequestContext());
    setTotpStep('recovery-replace');
  };

  const handleReplaceBack = () => {
    replacePrepRef.current = null;
    setReplaceSession(null);
    setTotpStep('recovery-kept');
  };

  /**
   * The replace step's one request, and only the request: the preparation
   * finished before the primary came up. Where there is nothing it can send,
   * it reports so and prepares again, so the next attempt can.
   */
  const sendReplace: MfaSeamHandler = async (password, mfaCode, context) => {
    const settled = replacePrepRef.current?.settled ?? null;
    if (settled?.kind !== 'ready' || !wrapsCurrentKeys(settled.prepared)) {
      // Nothing was sent, so this is a refusal, not an ambiguous outcome,
      // and the typed code is still unused — it stays.
      setReplaceRefusal(unpreparedRefusal(settled));
      startReplacePreparation(context);
      return { kind: 'aborted' };
    }
    const { prepared } = settled;
    const result = await submitMfaStepUp(
      '/api/v1/mfa/recovery-key',
      'PUT',
      prepared.body,
      { password, mfaCode },
      { context }
    );
    // An answer for an account or server that is no longer current belongs to
    // the old one: this wizard shows none of it.
    if (!apiRequestContextIsCurrent(context)) return result;
    if (result.kind === 'accepted') {
      replacePrepRef.current = null;
      setReplaceSession(null);
      setReplaceUncertain(false);
      setReplaceRefusal(null);
      setRecoveryKey(prepared.key);
      setTotpStep('recovery');
      return result;
    }
    if (isAmbiguousOutcome(result)) setReplaceUncertain(true);
    setReplaceRefusal(result);
    return result;
  };

  // ── WebAuthn Flow ──────────────────────────────────────────────────

  const keyName = credentialName || 'Security Key';

  /**
   * The browser ceremony and the finish request, once begin accepted. Never
   * rejects. Both belong to the account that began, so a change of account or
   * server since `context` ends it without touching this wizard.
   *
   * Cancel aborts the ceremony and drops it, so a key touched afterwards sends
   * no finish. A finish already under way is not recalled: the key was touched
   * before Cancel and the account now holds it, so its success still lands on
   * 'done', while its failure changes nothing Cancel has not already reset.
   */
  const completeKeyRegistration = async (options: CreationOptions, context: ApiRequestContext) => {
    const ceremony = new AbortController();
    ceremonyRef.current = ceremony;
    try {
      // Convert base64url fields for WebAuthn API
      decodeWebAuthnOptions(options);
      const credential = await createCredential(options, ceremony.signal);
      if (ceremonyRef.current !== ceremony) return;
      await sendRegistrationFinish(credential, keyName, context);
      if (!apiRequestContextIsCurrent(context)) return;
      // Uses the enrollment exemption, as after TOTP confirm.
      void refreshAccessToken().catch(() => console.warn('[mfa] Refresh after enrollment failed'));
      setWebauthnStep('done');
    } catch (err) {
      if (ceremonyRef.current !== ceremony || !apiRequestContextIsCurrent(context)) return;
      // Every failure returns to the credentials step with its banner: begin
      // spent the code or token that gated it, so a retry has to start there.
      setError(classifyWebAuthnError(err));
      setWebauthnStep('password');
    }
  };

  /**
   * Security-key registration's step-up (#5): begin is the one request the
   * credentials gate, so it is all `submit` sends. The ceremony follows it
   * outside the step-up, in the 'registering' step.
   */
  const beginKeyRegistration: MfaSeamHandler = async (password, mfaCode, context) => {
    setError('');
    const result = await submitMfaStepUp(
      '/api/v1/mfa/webauthn/register/begin',
      'POST',
      { credential_name: keyName, credential_type: credentialType },
      { password, mfaCode },
      { context }
    );
    if (!apiRequestContextIsCurrent(context)) return result;
    const options = result.kind === 'accepted' ? readCreationOptions(result.data) : null;
    if (options === null) return refuseBegin(result);
    // Show the "waiting for key" step before triggering the browser dialog
    setWebauthnStep('registering');
    void completeKeyRegistration(options, context);
    return result;
  };

  // ── Render helpers (reduce cognitive complexity) ────────────────────

  // The credential steps (#5) are one stage for both flows — they differ only
  // in copy, the key-name field, and the begin request.
  const renderTOTPPasswordStep = () => (
    <SetupCredentialsStage
      purpose="mfa_settings.totp_setup"
      intro="Enter your password to begin setup."
      activeIntro="Verify your identity to add another method."
      submitLabel="Continue"
      busyLabel="Setting up..."
      mfaActive={mfaActive}
      error={error}
      headingRef={headingRef}
      onBegin={beginTotpSetup}
      onCancel={onCancel}
    />
  );

  const renderWebAuthnPasswordStep = () => (
    <SetupCredentialsStage
      purpose="mfa_settings.webauthn_register"
      intro="Enter your password and name your key."
      activeIntro="Verify your identity and name your key."
      submitLabel="Register Key"
      busyLabel="Registering..."
      mfaActive={mfaActive}
      error={error}
      headingRef={headingRef}
      nameField={{
        value: credentialName,
        onChange: setCredentialName,
        placeholder:
          credentialType === 'platform'
            ? 'Key name (e.g. MacBook Touch ID, Windows Hello)'
            : 'Key name (e.g. YubiKey 5, Google Titan)',
      }}
      onBegin={beginKeyRegistration}
      onCancel={onCancel}
    />
  );

  const handleResetToPassword = () => {
    ceremonyRef.current?.abort();
    ceremonyRef.current = null;
    setWebauthnStep('password');
    setError('');
  };

  const renderWebAuthnRegisteringStep = () => (
    <div className="mfa-setup-step" style={{ alignItems: 'center' }}>
      <div style={{ textAlign: 'center', padding: '20px 0' }}>
        <WebAuthnWaitingPrompt />
      </div>
      <div className="mfa-setup-actions" style={{ justifyContent: 'center' }}>
        <button className="btn btn-secondary" onClick={handleResetToPassword}>
          Cancel
        </button>
      </div>
    </div>
  );

  // ── Render ─────────────────────────────────────────────────────────

  if (method === 'totp') {
    return (
      <div className="mfa-setup-wizard">
        <h3 tabIndex={-1} ref={headingRef}>
          Set Up Authenticator App
        </h3>

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

        {totpStep === 'recovery-replace' && replaceSession !== null && (
          <RecoveryReplaceStage
            session={replaceSession}
            result={replaceRefusal}
            headingRef={headingRef}
            onSend={sendReplace}
            onBack={handleReplaceBack}
          />
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
      <h3 tabIndex={-1} ref={headingRef}>
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

interface StepUpStageFrameProps {
  factor: StepUpFactor;
  password: string;
  onPasswordChange: (value: string) => void;
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  /** The sentences above the credentials. */
  intro: React.ReactNode;
  /** What the action asks for beside the credentials, below them. */
  children?: React.ReactNode;
  /** The answer's general banner (`stepUpBanner`), or an earlier step's error. */
  banner: string;
  primary: {
    className: string;
    label: string;
    busyLabel: string;
    ariaDisabled: boolean;
    onActivate: () => void;
  };
  secondaryLabel: string;
  onSecondary: () => void;
}

/**
 * What the two credential stages share: the intro, `StepUpCredentials`, and a
 * footer whose primary is `aria-disabled` and never natively disabled, so its
 * guard can say what is missing. The password takes focus on entry, as the
 * `autoFocus` it replaces did.
 */
const StepUpStageFrame: React.FC<StepUpStageFrameProps> = ({
  factor,
  password,
  onPasswordChange,
  headingRef,
  intro,
  children,
  banner,
  primary,
  secondaryLabel,
  onSecondary,
}) => {
  const passwordRef = useRef<HTMLInputElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
  useEffect(() => {
    passwordRef.current?.focus();
  }, []);
  const busyLabel = factor.phase === 'ceremony' ? 'Waiting…' : primary.busyLabel;
  return (
    <div className="mfa-setup-step">
      {intro}
      <StepUpCredentials
        factor={factor}
        password={password}
        onPasswordChange={onPasswordChange}
        primaryRef={primaryRef}
        passwordRef={passwordRef}
        headingRef={headingRef}
      />
      {children}
      <ErrorBanner error={banner} />
      <div className="mfa-setup-actions">
        <button
          ref={primaryRef}
          type="button"
          className={primary.className}
          aria-disabled={primary.ariaDisabled || undefined}
          onClick={primary.onActivate}
        >
          {factor.phase === 'idle' ? (
            primary.label
          ) : (
            <>
              <LoadingSpinner size="small" inline /> {busyLabel}
            </>
          )}
        </button>
        <button
          type="button"
          className="btn btn-secondary"
          disabled={factor.phase === 'submitting'}
          onClick={onSecondary}
        >
          {secondaryLabel}
        </button>
      </div>
    </div>
  );
};

interface SetupCredentialsStageProps {
  /** The route the code is sent with: TOTP setup or WebAuthn register begin. */
  purpose: StepUpPurpose;
  intro: string;
  /** The sentence once the account is known to hold MFA. */
  activeIntro: string;
  submitLabel: string;
  busyLabel: string;
  mfaActive?: boolean;
  /** Left by an earlier step of the wizard, e.g. a ceremony that failed. */
  error: string;
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  /** The security-key flow names its key beside the credentials. */
  nameField?: { value: string; placeholder: string; onChange: (value: string) => void };
  /** The gated begin request. Nothing else is sent from this stage. */
  onBegin: MfaSeamHandler;
  onCancel: () => void;
}

/**
 * The enrolment step-up (#5), for TOTP setup and security-key registration.
 * No backup code is offered: that is presentation, keeping enrolment on the
 * real factor — the server would accept one (design §6).
 */
const SetupCredentialsStage: React.FC<SetupCredentialsStageProps> = ({
  purpose,
  intro,
  activeIntro,
  submitLabel,
  busyLabel,
  mfaActive,
  error,
  headingRef,
  nameField,
  onBegin,
  onCancel,
}) => {
  const [password, setPassword] = useState('');
  const nameFieldId = useId();
  const factor = useStepUpFactor({
    enabled: true,
    purpose,
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: false,
  });
  const { ariaDisabled, activate } = stepUpActivation(
    factor,
    password,
    toSeamSubmit(onBegin, password, () => setPassword(''))
  );
  return (
    <StepUpStageFrame
      factor={factor}
      password={password}
      onPasswordChange={setPassword}
      headingRef={headingRef}
      intro={<p>{mfaActive || factor.methods.length > 0 ? activeIntro : intro}</p>}
      banner={error}
      primary={{
        className: 'btn btn-primary',
        label: submitLabel,
        busyLabel,
        ariaDisabled,
        onActivate: activate,
      }}
      secondaryLabel="Cancel"
      onSecondary={onCancel}
    >
      {nameField && (
        <div className="mfa-setup-field">
          <label htmlFor={nameFieldId}>Key name</label>
          <input
            id={nameFieldId}
            type="text"
            className="form-input"
            value={nameField.value}
            onChange={(e) => nameField.onChange(e.target.value)}
            placeholder={nameField.placeholder}
            // The activation captured the name, as it did the password, so an
            // edit while the security-key prompt is open would not be the one
            // registered (§4.2's ceremony row).
            readOnly={factor.phase === 'ceremony'}
            disabled={factor.phase === 'submitting'}
          />
        </div>
      )}
    </StepUpStageFrame>
  );
};

interface RecoveryReplaceStageProps {
  session: ReplaceSession;
  /** The last answer: the banner, and the lock after a spent budget or a dead session. */
  result: MfaStepUpResult | null;
  headingRef: React.RefObject<HTMLHeadingElement | null>;
  /** The gated request. The preparation is the wizard's and has finished by now. */
  onSend: MfaSeamHandler;
  onBack: () => void;
}

/**
 * The recovery-key replace step-up (#6). It prepares on entry, so the primary
 * is held down until the new key is ready and the activation runs `run` with
 * the capture taken then and nothing awaited in front of it (D11, Q6).
 */
const RecoveryReplaceStage: React.FC<RecoveryReplaceStageProps> = ({
  session,
  result,
  headingRef,
  onSend,
  onBack,
}) => {
  const [password, setPassword] = useState('');
  const factor = useStepUpFactor({
    enabled: true,
    purpose: 'mfa_settings.recovery_key_replace',
    passwordLeg: 'always', // pragma: allowlist secret
    readFailure: 'passwordOnly',
    allowBackup: true,
    preparing: session.preparing,
  });
  const { ariaDisabled, activate } = stepUpActivation(
    factor,
    password,
    toSeamSubmit(onSend, password, () => setPassword('')),
    { capture: session.capture }
  );
  // A speed bump: the server budget is the enforcement (isStepUpLocked).
  const locked = isStepUpLocked(result);
  const activateUnlessLocked = () => {
    if (!locked) activate();
  };
  return (
    <StepUpStageFrame
      factor={factor}
      password={password}
      onPasswordChange={setPassword}
      headingRef={headingRef}
      intro={
        <>
          <p>
            Make a new recovery key. Confirm it&apos;s you with your password and your second
            factor.
          </p>
          <p className="mfa-modal-desc">Your old recovery key will stop working.</p>
        </>
      }
      banner={stepUpBanner(result) ?? ''}
      primary={{
        className: 'btn btn-danger',
        label: 'Replace recovery key',
        busyLabel: 'Replacing...',
        ariaDisabled: ariaDisabled || locked,
        onActivate: activateUnlessLocked,
      }}
      secondaryLabel="Back"
      onSecondary={onBack}
    />
  );
};

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
