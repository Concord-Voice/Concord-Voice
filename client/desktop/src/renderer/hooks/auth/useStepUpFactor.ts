/**
 * The step-up factor picker's state machine (design 2026-09-26-mfa-factor-picker
 * §4.1; plan 2026-10-07 §3).
 *
 * It learns what a step-up will ask for before the first submit, offers only
 * the inline factors the account can use, and runs the one activation that
 * proves a factor and sends the gated request. The surface owns its password
 * field, its primary button and its request; this hook owns the offered set,
 * the active method, the code, the phase, and the ceremony's abort.
 *
 * Invariants, each enforced in this file:
 *
 * - One activation at a time (C6). A synchronous ref is the guard, set before
 *   the first await and released in `finally`; `phase` cannot be, because two
 *   activations can land before React commits it, and a second WebAuthn begin
 *   replaces the first one's server session.
 * - Nothing is sent as another account or to another server (C45, C75, C82).
 *   `run` works against ONE `ApiRequestContext`, its own or the caller's
 *   capture taken before preparation, and checks it before begin, before the
 *   browser ceremony, before finish, and immediately before `submit`. A change ends in the
 *   terminal `sessionExpired` status with nothing further sent. Begin, finish
 *   and the surface's request are also admitted against that context, so
 *   apiFetch refuses a change that lands after the last check.
 * - No proof outlives its attempt (C56). Switching methods, disabling, and
 *   unmounting bump the attempt and abort the ceremony; a token whose finish
 *   resolved anyway is dropped at the pre-`submit` check, unsent.
 * - No read result applies outside the open instance that started it (C57).
 *   Each read carries a sequence number; a result applies only while its
 *   number is current and its signal is not aborted.
 *
 * Nothing here is logged: the offered set and default are account posture.
 */

import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState } from 'react';
import type { StepUpPurpose } from '../../components/Auth/stepUpPurpose';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import {
  codeProvenUnspent,
  fetchStepUpRequirements,
  intersectInline,
  isBackupShaped,
  isTotpShaped,
  pickDefaultMethod,
  type InlineStepUpMethod,
  type StepUpReadRefusalReason,
  type StepUpRequirementsResult,
} from '../../services/system/stepUpRequirements';
import type { StepUpRefusal } from '../../services/system/stepUpRefusal';
import {
  beginWebAuthnInlineVerification,
  finishWebAuthnVerification,
  NO_INLINE_SESSION,
  NO_WEBAUTHN_CREDENTIALS,
  performWebAuthnAssertion,
  WebAuthnInlineError,
} from '../../services/system/webauthnInlineStepUp';
import { useTotpAcceptedStore } from '../../stores/auth/totpAcceptedStore';
import { useUserStore } from '../../stores/auth/userStore';

// ── Public types ─────────────────────────────────────────────────────────

/** A panel the picker can show. A backup code is never a default (§3). */
export type StepUpMethod = InlineStepUpMethod | 'backup';

/** When the surface collects the password. `'none'` waits for its first caller (Q3). */
export type StepUpPasswordLeg = 'always' | 'whenNoMfa'; // pragma: allowlist secret

/** What a failed read does on this route (§2): block with Retry, or the password alone. */
export type StepUpReadFailure = 'block' | 'passwordOnly';

export interface StepUpFactorProps {
  /** The surface is open and its action needs a step-up. The read starts on true. */
  enabled: boolean;
  /** The request the WebAuthn token is minted for. `null` offers no security key. */
  purpose: StepUpPurpose | null;
  passwordLeg: StepUpPasswordLeg;
  readFailure: StepUpReadFailure;
  /** Offer a backup code when the read reports one and TOTP is offered. */
  allowBackup: boolean;
}

/**
 * What is known about the requirement.
 *
 * - `reading`: the read is in flight.
 * - `ready`: the offered set is known. It may be empty: the password alone,
 *   which is also where an `unsupported` read, or an `unavailable` one on a
 *   `passwordOnly` route, lands.
 * - `blocked`: an `unavailable` read on a `block` route. Retry is offered.
 * - `refused`, `noUsableMethod`, `sessionExpired`: terminal for this instance.
 *   `noUsableMethod` is an `mfa_required` naming nothing this app can collect
 *   (G1); `sessionExpired` is an account or server change during `run` (C45).
 */
export type StepUpStatus =
  | { kind: 'reading' }
  | { kind: 'ready' }
  | { kind: 'blocked' }
  | { kind: 'refused'; reason: StepUpReadRefusalReason }
  | { kind: 'noUsableMethod' }
  | { kind: 'sessionExpired' };

/** `ceremony`: the WebAuthn exchange is running. `submitting`: the request is out. */
export type StepUpPhase = 'idle' | 'ceremony' | 'submitting';

/**
 * The one status line the credential UI shows, by kind (copy is §4.2's):
 *
 * - `checking`: activation while the read is in flight.
 * - `missing`: an empty required field, from the activation guard or from an
 *   `mfa_required` / `password_required` refusal (C2, C6).
 * - `invalidPassword`, `invalidFactor`: a refusal of what was entered.
 * - `webauthnCancelled`: cancelled, timed out, or the server's session expired.
 * - `methodsChanged`: a re-read moved the active panel (C13).
 */
export type StepUpNotice =
  | { kind: 'checking' }
  | { kind: 'missing'; field: 'password' | StepUpMethod }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidFactor'; method: StepUpMethod }
  | { kind: 'webauthnCancelled' }
  | { kind: 'methodsChanged' };

/** Why the primary cannot act yet; `unavailable` is a state no input can complete. */
export type StepUpMissing = 'reading' | 'password' | 'code' | 'unavailable';

/**
 * What the surface's request came to, mapped from its own result union.
 *
 * - `answered`: the server answered with something else (a partial purge, a
 *   5xx). The code may have been spent.
 * - `transport`: no answer arrived; the request may have reached the server.
 * - `aborted`: the request provably never left (apiFetch's pre-dispatch
 *   fence). Nothing was spent.
 */
export type StepUpSubmitOutcome =
  | { kind: 'success' }
  | { kind: 'refusal'; refusal: StepUpRefusal }
  | { kind: 'answered' }
  | { kind: 'transport' }
  | { kind: 'aborted' };

/**
 * Sends the gated request, once. It only transmits: preparation happens before
 * `run` (C9). `mfa` is the TOTP or backup code, the WebAuthn token, or
 * undefined when no method is offered; `context` is the capture the request
 * must be admitted against.
 */
export type StepUpSubmit = (
  mfa: string | undefined,
  context: ApiRequestContext
) => Promise<StepUpSubmitOutcome>;

export interface StepUpFactor {
  status: StepUpStatus;
  /** Every method the picker may show: strongest first, a backup code last. */
  methods: readonly StepUpMethod[];
  /** The active panel, or null when no method is offered. */
  method: StepUpMethod | null;
  /** The surface shows its password field. */
  passwordLegShown: boolean;
  code: string;
  /** The code input's `key`. Bumped after a submit that may have spent the code (#3466). */
  attempt: number;
  phase: StepUpPhase;
  notice: StepUpNotice | null;
  /** The latest background re-read's result (C13, C25), for PR 6's exhaustion check. */
  reread: StepUpRequirementsResult | null;
  setCode: (code: string) => void;
  /** Switches the panel. Clears the code and aborts a running ceremony. */
  switchTo: (method: StepUpMethod) => void;
  /** Re-runs a failed read. Only the `blocked` status offers it. */
  retryRead: () => void;
  /** The first thing the primary still needs, or null when it can act. */
  firstMissing: (password: string) => StepUpMissing | null;
  /** The activation guard: shows the status line for `missing`. Queues nothing. */
  announceMissing: (missing: StepUpMissing) => void;
  /**
   * The activation: proves the active factor and calls `submit` once.
   * Resolves to `submit`'s outcome, or null when nothing was submitted.
   */
  run: (submit: StepUpSubmit, capture?: ApiRequestContext) => Promise<StepUpSubmitOutcome | null>;
}

// ── State and pure transitions ───────────────────────────────────────────

interface FactorConfig {
  purpose: StepUpPurpose | null;
  readFailure: StepUpReadFailure;
  passwordLeg: StepUpPasswordLeg;
  allowBackup: boolean;
}

interface FactorState {
  /** The open instance this state belongs to; a change resets it. */
  instance: string;
  status: StepUpStatus;
  offered: readonly InlineStepUpMethod[];
  serverDefault: InlineStepUpMethod | null;
  backupCodeAvailable: boolean;
  method: StepUpMethod | null;
  code: string;
  attempt: number;
  phase: StepUpPhase;
  notice: StepUpNotice | null;
  reread: StepUpRequirementsResult | null;
}

type RunGate = 'live' | 'stale' | 'sessionExpired';

/** How a WebAuthn ceremony ended, when it minted no token. */
type CeremonyStop =
  | { kind: 'stale' }
  | { kind: 'sessionExpired' }
  | { kind: 'failed'; notice: StepUpNotice; reread: boolean };

const INVALID_WEBAUTHN: StepUpNotice = { kind: 'invalidFactor', method: 'webauthn' };
const WEBAUTHN_CANCELLED: StepUpNotice = { kind: 'webauthnCancelled' };

function initialState(instance: string): FactorState {
  return {
    instance,
    status: { kind: 'reading' },
    offered: [],
    serverDefault: null,
    backupCodeAvailable: false,
    method: null,
    code: '',
    attempt: 0,
    phase: 'idle',
    notice: null,
    reread: null,
  };
}

/** The inline methods in `methods` this route can verify: no key without a purpose. */
function offerable(
  methods: readonly string[],
  purpose: StepUpPurpose | null
): InlineStepUpMethod[] {
  return intersectInline(methods).filter((method) => purpose !== null || method !== 'webauthn');
}

function availableMethods(
  offered: readonly InlineStepUpMethod[],
  backupCodeAvailable: boolean,
  config: FactorConfig
): StepUpMethod[] {
  const backup = config.allowBackup && backupCodeAvailable && offered.includes('totp');
  return backup ? [...offered, 'backup'] : [...offered];
}

/** True while `method` is one the picker may show for `state`. */
function stillAvailable(state: FactorState, method: StepUpMethod, config: FactorConfig): boolean {
  return availableMethods(state.offered, state.backupCodeAvailable, config).includes(method);
}

/** `state` with `offered` replaced, keeping the active method while it is still offered. */
function withOffered(
  state: FactorState,
  offered: readonly InlineStepUpMethod[],
  config: FactorConfig
): FactorState {
  const next = { ...state, offered };
  const kept = state.method !== null && stillAvailable(next, state.method, config);
  return { ...next, method: kept ? state.method : pickDefaultMethod(offered, state.serverDefault) };
}

/** After a submit that may have spent the code: drop it and remount the input. */
function spent(state: FactorState): FactorState {
  return { ...state, code: '', attempt: state.attempt + 1 };
}

function expired(state: FactorState): FactorState {
  return { ...spent(state), status: { kind: 'sessionExpired' }, phase: 'idle', notice: null };
}

/** A notice the read answers drops when it lands; any other stands. */
function afterRead(notice: StepUpNotice | null): StepUpNotice | null {
  return notice?.kind === 'checking' ? null : notice;
}

/** The first read of an open instance. */
function readTransition(
  state: FactorState,
  result: StepUpRequirementsResult,
  config: FactorConfig
): FactorState {
  const settled = { ...state, notice: afterRead(state.notice) };
  switch (result.kind) {
    case 'ready': {
      const offered = offerable(result.methods, config.purpose);
      return {
        ...settled,
        status: { kind: 'ready' },
        offered,
        serverDefault: result.defaultMethod,
        backupCodeAvailable: result.backupCodeAvailable,
        method: pickDefaultMethod(offered, result.defaultMethod),
      };
    }
    case 'refused':
      return { ...settled, status: { kind: 'refused', reason: result.reason } };
    case 'unsupported':
      // An old server is permanent: the password alone on every route (§2).
      return { ...settled, status: { kind: 'ready' } };
    case 'unavailable':
    case 'aborted':
      // A live read on an unchanged account can still be fenced by apiFetch (a
      // session rotated twice under it). Treated as unavailable, so the
      // instance never hangs on "reading".
      return config.readFailure === 'block'
        ? { ...settled, status: { kind: 'blocked' } }
        : { ...settled, status: { kind: 'ready' } };
  }
}

/**
 * The background re-read (C13, C25). A `ready` answer is newer than any
 * refusal, so it replaces the set; when the active panel left it, the panel
 * moves to the new default and says so. `refused` is terminal. Anything else
 * changes nothing. It applies only to an idle, non-terminal instance: a
 * re-read that lands mid-activation is exposed but not applied.
 */
function rereadTransition(
  state: FactorState,
  result: StepUpRequirementsResult,
  config: FactorConfig
): FactorState {
  // An abort is discarded, never exposed: nothing was learned.
  if (result.kind === 'aborted') return state;
  const exposed = { ...state, reread: result };
  if (state.status.kind !== 'ready' || state.phase !== 'idle') return exposed;
  if (result.kind === 'refused') {
    return { ...spent(exposed), status: { kind: 'refused', reason: result.reason }, notice: null };
  }
  if (result.kind !== 'ready') return exposed;
  const offered = offerable(result.methods, config.purpose);
  const next = {
    ...exposed,
    offered,
    serverDefault: result.defaultMethod,
    backupCodeAvailable: result.backupCodeAvailable,
  };
  if (state.method !== null && stillAvailable(next, state.method, config)) return next;
  return {
    ...next,
    method: pickDefaultMethod(offered, result.defaultMethod),
    code: '',
    notice: { kind: 'methodsChanged' },
  };
}

/**
 * A refusal of the gated request. `submitted` is the method that was active.
 * The seam checks the password before the code, so a password refusal keeps
 * the code (#3466); every other refusal may have spent it.
 */
function refusalTransition(
  state: FactorState,
  refusal: StepUpRefusal,
  submitted: StepUpMethod | null,
  config: FactorConfig
): FactorState {
  const idle = { ...state, phase: 'idle' as const };
  switch (refusal.kind) {
    case 'mfaRequired': {
      // Authoritative over the read; email and SMS never join the set (G1).
      const offered = offerable(refusal.methods, config.purpose);
      if (offered.length === 0) {
        const none = { ...spent(idle), offered, method: null, notice: null };
        return { ...none, status: { kind: 'noUsableMethod' } };
      }
      const next = withOffered(spent(idle), offered, config);
      return {
        ...next,
        notice: next.method === null ? null : { kind: 'missing', field: next.method },
      };
    }
    case 'passwordRequired': {
      // On `whenNoMfa` the server asks for the password only when it found
      // no inline method at submit time, so the set is now empty (C2).
      const leg = config.passwordLeg;
      if (leg === 'whenNoMfa') {
        const none = { ...spent(idle), offered: [], method: null };
        return { ...none, notice: { kind: 'missing', field: 'password' } };
      }
      return { ...idle, notice: { kind: 'missing', field: 'password' } };
    }
    case 'invalidPassword':
      return { ...idle, notice: { kind: 'invalidPassword' } };
    case 'invalidMfaCode':
      return {
        ...spent(idle),
        notice: submitted === null ? null : { kind: 'invalidFactor', method: submitted },
      };
    default:
      // The surface renders everything else.
      return { ...spent(idle), notice: null };
  }
}

/**
 * The one refusal that earns a background re-read (C13): the seam refuses a
 * factor removed elsewhere exactly as it refuses a wrong code. Never any other
 * refusal, so one response causes at most one read (C32).
 */
function refusalRereads(refusal: StepUpRefusal): boolean {
  return refusal.kind === 'invalidMfaCode';
}

/**
 * The state an answered submit leaves: a refusal goes through
 * `refusalTransition`; an unsent request leaves the code unspent, or ends the
 * instance when the account or server changed; anything else may have spent
 * the code, so it is cleared.
 */
function outcomeUpdate(
  outcome: StepUpSubmitOutcome,
  method: StepUpMethod | null,
  config: FactorConfig,
  contextCurrent: boolean
): (state: FactorState) => FactorState {
  switch (outcome.kind) {
    case 'refusal': {
      const { refusal } = outcome;
      return (s) => refusalTransition(s, refusal, method, config);
    }
    case 'aborted':
      return contextCurrent ? (s) => ({ ...s, phase: 'idle' }) : expired;
    default:
      return (s) => ({ ...spent(s), phase: 'idle', notice: null });
  }
}

/** True when the request may have spent a code it carried (C30, C41). */
function mayHaveSpentCode(outcome: StepUpSubmitOutcome): boolean {
  switch (outcome.kind) {
    case 'refusal':
      return !codeProvenUnspent(outcome.refusal);
    case 'aborted':
      return false;
    default:
      return true;
  }
}

/**
 * Records a TOTP acceptance for the S2a hint. The value's shape, not the
 * panel, says it was TOTP (C12). An account or server change since the
 * capture drops it: `gracefulReset` has already cleared the store for it.
 */
function recordTotpAcceptance(
  submitted: string | undefined,
  outcome: StepUpSubmitOutcome,
  accountId: string | null,
  context: ApiRequestContext
): void {
  if (submitted === undefined || accountId === null || !isTotpShaped(submitted)) return;
  if (!mayHaveSpentCode(outcome) || !apiRequestContextIsCurrent(context)) return;
  useTotpAcceptedStore.getState().noteTotpAccepted(accountId, Date.now());
}

/** The value the surface sends for a code panel; undefined for the others. */
function codeToSend(method: StepUpMethod | null, code: string): string | undefined {
  if (method === 'totp') return code.replace(/[ -]/g, '');
  if (method === 'backup') return code.trim();
  return undefined;
}

/**
 * True when a code panel's value has its factor's shape, so a partial code is
 * never sent: each send spends a unit of the route's rate-limited budget.
 */
function codeComplete(method: StepUpMethod | null, code: string): boolean {
  if (method === 'totp') return isTotpShaped(code);
  if (method === 'backup') return isBackupShaped(code);
  return true;
}

function ceremonyFailure(err: unknown): CeremonyStop {
  if (err instanceof WebAuthnInlineError) {
    if (err.status === 401) return { kind: 'sessionExpired' };
    if (err.serverError === NO_INLINE_SESSION) {
      return { kind: 'failed', notice: WEBAUTHN_CANCELLED, reread: false };
    }
    // The account's last key was removed elsewhere: re-read (C25).
    const keyGone = err.step === 'begin' && err.serverError === NO_WEBAUTHN_CREDENTIALS;
    return { kind: 'failed', notice: INVALID_WEBAUTHN, reread: keyGone };
  }
  if (isAbortError(err) || (err instanceof DOMException && err.name === 'NotAllowedError')) {
    return { kind: 'failed', notice: WEBAUTHN_CANCELLED, reread: false };
  }
  return { kind: 'failed', notice: INVALID_WEBAUTHN, reread: false };
}

/**
 * Begin, ceremony, finish, with the run's checks before each step that sends
 * or signs (C75). A check that fails stops here with nothing further sent.
 */
async function webauthnProof(
  purpose: StepUpPurpose | null,
  context: ApiRequestContext,
  signal: AbortSignal,
  gate: () => RunGate
): Promise<{ kind: 'token'; token: string } | CeremonyStop> {
  try {
    let check = gate();
    if (check !== 'live') return { kind: check };
    const options = await beginWebAuthnInlineVerification(purpose, context, signal);
    check = gate();
    if (check !== 'live') return { kind: check };
    const credential = await performWebAuthnAssertion(options, signal);
    check = gate();
    if (check !== 'live') return { kind: check };
    return { kind: 'token', token: await finishWebAuthnVerification(credential, context, signal) };
  } catch (err) {
    // A switch, a close, or apiFetch's fence for an account change all
    // surface as an abort: the gate says which it was.
    const check = gate();
    return check === 'live' ? ceremonyFailure(err) : { kind: check };
  }
}

/**
 * Calls `submit` once. It is handed no signal, so an AbortError out of it can
 * only be apiFetch's pre-dispatch fence: nothing was sent. Any other throw may
 * have reached the server.
 */
async function submitOnce(
  submit: StepUpSubmit,
  mfa: string | undefined,
  context: ApiRequestContext
): Promise<StepUpSubmitOutcome> {
  try {
    return await submit(mfa, context);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'transport' };
  }
}

function missingNotice(missing: StepUpMissing, method: StepUpMethod | null): StepUpNotice | null {
  switch (missing) {
    case 'reading':
      return { kind: 'checking' };
    case 'password':
      return { kind: 'missing', field: 'password' };
    case 'code':
      return method === null ? null : { kind: 'missing', field: method };
    case 'unavailable':
      return null;
  }
}

function showsPassword(state: FactorState, config: FactorConfig): boolean {
  const leg = config.passwordLeg;
  if (leg === 'always') {
    return state.status.kind === 'reading' || state.status.kind === 'ready';
  }
  return state.status.kind === 'ready' && state.offered.length === 0;
}

function firstMissingIn(
  state: FactorState,
  passwordShown: boolean,
  password: string
): StepUpMissing | null {
  const { kind } = state.status;
  if (kind !== 'reading' && kind !== 'ready') return 'unavailable';
  if (passwordShown && password === '') return 'password';
  if (kind === 'reading') return 'reading';
  return codeComplete(state.method, state.code) ? null : 'code';
}

// ── The hook ─────────────────────────────────────────────────────────────

export function useStepUpFactor({
  enabled,
  purpose,
  passwordLeg,
  readFailure,
  allowBackup,
}: StepUpFactorProps): StepUpFactor {
  const config = useMemo<FactorConfig>(
    () => ({ purpose, readFailure, passwordLeg, allowBackup }),
    [purpose, readFailure, passwordLeg, allowBackup]
  );
  // An open instance is `enabled` plus the configuration it opened with; any
  // change starts a fresh one, and the result lives only for that instance.
  const instance = `${enabled}|${purpose}|${readFailure}|${passwordLeg}|${allowBackup}`;

  const [factorState, setFactorState] = useState(() => initialState(instance));
  let state = factorState;
  if (factorState.instance !== instance) {
    state = initialState(instance);
    setFactorState(state);
  }

  const runningRef = useRef(false);
  const runAttemptRef = useRef(0);
  const ceremonyRef = useRef<AbortController | null>(null);
  const readSeqRef = useRef(0);
  const readAbortRef = useRef<AbortController | null>(null);

  /** Ends the current attempt: its ceremony aborts and its proof is dropped (C56). */
  const endAttempt = useCallback(() => {
    runAttemptRef.current += 1;
    ceremonyRef.current?.abort();
    ceremonyRef.current = null;
  }, []);

  const startRead = useCallback(
    (mode: 'initial' | 'reread') => {
      readAbortRef.current?.abort();
      const controller = new AbortController();
      readAbortRef.current = controller;
      const seq = ++readSeqRef.current;
      // apiFetch admits the read against the account and server current now.
      const context = captureApiRequestContext();
      void fetchStepUpRequirements(controller.signal).then((result) => {
        // C57: only the instance's current read applies.
        if (seq !== readSeqRef.current || controller.signal.aborted) return;
        readAbortRef.current = null;
        if (!apiRequestContextIsCurrent(context)) {
          // Answered, or fenced, for an account or server that is no longer
          // current (C57): whatever it says belongs to the old one.
          setFactorState(expired);
          return;
        }
        setFactorState((s) =>
          mode === 'initial'
            ? readTransition(s, result, config)
            : rereadTransition(s, result, config)
        );
      });
    },
    [config]
  );

  useEffect(() => {
    if (!enabled) return undefined;
    startRead('initial');
    return () => {
      // Disabled or unmounted: nothing in flight may land (C57).
      readSeqRef.current += 1;
      readAbortRef.current?.abort();
      readAbortRef.current = null;
    };
  }, [enabled, startRead]);

  // The attempt ends in the commit that disables the instance, not at the
  // later passive flush: a finish that resolves in between would otherwise
  // pass the pre-submit gate after the dialog closed (C56). A running
  // ceremony stops with it.
  useLayoutEffect(() => {
    if (!enabled) return undefined;
    return endAttempt;
  }, [enabled, endAttempt]);

  const { offered, backupCodeAvailable } = state;
  const methods = useMemo(
    () => availableMethods(offered, backupCodeAvailable, config),
    [offered, backupCodeAvailable, config]
  );
  const passwordLegShown = enabled && showsPassword(state, config);

  const setCode = useCallback((code: string) => {
    setFactorState((s) => (s.phase === 'submitting' ? s : { ...s, code }));
  }, []);

  const switchTo = useCallback(
    (next: StepUpMethod) => {
      if (state.phase === 'submitting' || next === state.method || !methods.includes(next)) return;
      endAttempt();
      setFactorState((s) => ({ ...s, method: next, code: '', phase: 'idle', notice: null }));
    },
    [state.phase, state.method, methods, endAttempt]
  );

  const retryRead = useCallback(() => {
    if (!enabled || state.status.kind !== 'blocked') return;
    setFactorState((s) => ({ ...s, status: { kind: 'reading' }, notice: null }));
    startRead('initial');
  }, [enabled, state.status.kind, startRead]);

  const firstMissing = useCallback(
    (password: string) => firstMissingIn(state, passwordLegShown, password),
    [state, passwordLegShown]
  );

  const announceMissing = useCallback((missing: StepUpMissing) => {
    setFactorState((s) => {
      const notice = missingNotice(missing, s.method);
      return notice === null ? s : { ...s, notice };
    });
  }, []);

  const run = useCallback(
    async (submit: StepUpSubmit, capture?: ApiRequestContext) => {
      // Nothing is queued: an incomplete or busy activation starts nothing.
      if (runningRef.current || !enabled || state.phase !== 'idle') return null;
      if (state.status.kind !== 'ready') return null;
      const method = state.method;
      const code = codeToSend(method, state.code);
      if (code === '') return null;

      runningRef.current = true; // C6: set before the first await.
      // This run's answer is newer than any re-read an earlier refusal started,
      // so that read must not land over it (C13, C32).
      readSeqRef.current += 1;
      readAbortRef.current?.abort();
      readAbortRef.current = null;
      const context = capture ?? captureApiRequestContext(); // C82
      const accountId = useUserStore.getState().user?.id ?? null;
      const attempt = ++runAttemptRef.current;
      const controller = new AbortController();
      ceremonyRef.current = controller;
      const gate = (): RunGate => {
        if (attempt !== runAttemptRef.current || controller.signal.aborted) return 'stale';
        return apiRequestContextIsCurrent(context) ? 'live' : 'sessionExpired';
      };
      const stop = (ended: CeremonyStop): null => {
        // A stale attempt belongs to an instance that has moved on: silent.
        if (ended.kind === 'stale' || attempt !== runAttemptRef.current) return null;
        if (ended.kind === 'sessionExpired') {
          setFactorState(expired);
          return null;
        }
        setFactorState((s) => ({ ...s, phase: 'idle', notice: ended.notice }));
        if (ended.reread) startRead('reread');
        return null;
      };

      try {
        let mfa = code;
        if (method === 'webauthn') {
          setFactorState((s) => ({ ...s, phase: 'ceremony', notice: null }));
          const proof = await webauthnProof(config.purpose, context, controller.signal, gate);
          if (proof.kind !== 'token') return stop(proof);
          mfa = proof.token;
        }
        // C45/C56: the last check before anything carrying the proof leaves.
        const check = gate();
        if (check !== 'live') return stop({ kind: check });
        setFactorState((s) => ({ ...s, phase: 'submitting', notice: null }));

        const outcome = await submitOnce(submit, mfa, context);
        recordTotpAcceptance(code, outcome, accountId, context);
        if (attempt !== runAttemptRef.current) return outcome;
        setFactorState(outcomeUpdate(outcome, method, config, apiRequestContextIsCurrent(context)));
        if (outcome.kind === 'refusal' && refusalRereads(outcome.refusal)) startRead('reread');
        return outcome;
      } finally {
        runningRef.current = false;
        if (ceremonyRef.current === controller) ceremonyRef.current = null;
      }
    },
    [enabled, state, config, startRead]
  );

  return {
    status: state.status,
    methods,
    method: state.method,
    passwordLegShown,
    code: state.code,
    attempt: state.attempt,
    phase: state.phase,
    notice: state.notice,
    reread: state.reread,
    setCode,
    switchTo,
    retryRead,
    firstMissing,
    announceMissing,
    run,
  };
}
