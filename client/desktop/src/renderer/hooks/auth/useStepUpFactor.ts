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
 *   the first await; `phase` cannot be, because two activations can land
 *   before React commits it, and a second WebAuthn begin replaces the first
 *   one's server session. The guard holds the id of the attempt that set it:
 *   ending the attempt releases it, and a run's `finally` releases it only
 *   while it still holds that run's id, so a request still out from an ended
 *   attempt never latches the instance that replaced it.
 * - Nothing is sent as another account or to another server (C45, C75, C82).
 *   An instance captures the account and server it opened for, and `run`
 *   works against ONE `ApiRequestContext`: that capture, or the caller's taken
 *   before preparation. It checks both before begin, before the browser
 *   ceremony, before finish, and immediately before `submit`, so a change
 *   since the instance opened ends in the terminal `sessionExpired` status
 *   with nothing further sent, even when it landed before the activation. A
 *   token refresh is no change: the account is its generation. Begin, finish
 *   and the surface's request are also admitted against the run's context, so
 *   apiFetch refuses a change that lands after the last check.
 * - No proof outlives its attempt (C56). Switching methods, disabling,
 *   changing the instance's configuration, and unmounting bump the attempt and
 *   abort the ceremony; a token whose finish resolved anyway is dropped at the
 *   pre-`submit` check, unsent, and a request already out applies nothing.
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

/**
 * When the surface collects the password. `'none'` never does: its route reads
 * no password (the dangerous-action gates, #3456 V18).
 */
export type StepUpPasswordLeg = 'always' | 'whenNoMfa' | 'none'; // pragma: allowlist secret

/**
 * The `whenNoMfa` leg by name. Hosts pass this, not the quoted literal, which
 * beside a `passwordLeg` key reads to static analysis as a hard-coded
 * credential (Sonar S2068, detect-secrets).
 */
export const LEG_ONLY_WITHOUT_MFA = 'whenNoMfa' satisfies StepUpPasswordLeg;

/**
 * The `none` leg by name, for the same reason as `LEG_ONLY_WITHOUT_MFA`. No
 * refusal names a password field on it, and with nothing offered the primary
 * sends with no factor: the server's answer, not a client guess, decides
 * whether one is needed (#3456 §3.3).
 */
export const FACTOR_ONLY_LEG = 'none' satisfies StepUpPasswordLeg;

/** What a failed read does on this route (§2): block with Retry, or the password alone. */
export type StepUpReadFailure = 'block' | 'passwordOnly';

/**
 * A refusal the hook applies: the classifier's, or a route adapter's
 * `mfaRequired` whose list is not the step-up set (#7). Its `methods: null`
 * leaves the read's set standing (§2, Q4).
 */
export type StepUpFactorRefusal = StepUpRefusal | { kind: 'mfaRequired'; methods: null };

export interface StepUpFactorProps {
  /** The surface is open and its action needs a step-up. The read starts on true. */
  enabled: boolean;
  /** The request the WebAuthn token is minted for. `null` offers no security key. */
  purpose: StepUpPurpose | null;
  passwordLeg: StepUpPasswordLeg;
  readFailure: StepUpReadFailure;
  /** Offer a backup code when the read reports one and TOTP is offered. */
  allowBackup: boolean;
  /**
   * The refusal that opened a refusal-triggered surface (#7, #17). It applies
   * when an instance starts and is never read again (G2): a `ready` read
   * replaces the set it seeds, and a failed or unsupported read keeps it, with
   * no backup code.
   */
  seed?: StepUpFactorRefusal | null;
  /**
   * Methods offered whatever the read or a refusal says (C4, C26): a read
   * only adds to them. With `purpose: null` no read runs, because only a
   * security key could join the floor and it needs a purpose.
   */
  floorMethods?: readonly InlineStepUpMethod[];
  /**
   * The host is still preparing what its `submit` sends (#6, #10; D11, Q6). It
   * keeps the primary down through `firstMissing`, and is not part of the open
   * instance, so toggling it never resets the dialog. Omitted, nothing is.
   */
  preparing?: boolean;
}

/**
 * What is known about the requirement.
 *
 * - `reading`: the read is in flight.
 * - `ready`: the offered set is known. It may be empty: the password alone,
 *   which is also where an `unsupported` read, or an `unavailable` one on a
 *   `passwordOnly` route, lands.
 * - `blocked`: an `unavailable` read on a `block` route. Retry is offered.
 * - `refused`, `noUsableMethod`, `enrollmentRequired`, `sessionExpired`:
 *   terminal for this instance. `noUsableMethod` is an `mfa_required` naming
 *   nothing this app can collect (G1); `enrollmentRequired` is an account with
 *   no inline factor at all (E8); `sessionExpired` is an account or server
 *   change during `run` (C45), or a `sessionExpired` refusal: the route
 *   answered 401 after `apiFetch`'s refresh retry, so nothing sent under this
 *   session can pass.
 */
export type StepUpStatus =
  | { kind: 'reading' }
  | { kind: 'ready' }
  | { kind: 'blocked' }
  | { kind: 'refused'; reason: StepUpReadRefusalReason }
  | { kind: 'noUsableMethod' }
  | { kind: 'enrollmentRequired' }
  | { kind: 'sessionExpired' };

/** `ceremony`: the WebAuthn exchange is running. `submitting`: the request is out. */
export type StepUpPhase = 'idle' | 'ceremony' | 'submitting';

/**
 * The one status line the credential UI shows, by kind (copy is §4.2's):
 *
 * - `checking`: activation while the read is in flight.
 * - `preparing`: activation while the host prepares. Shown only while it does.
 * - `missing`: an empty required field, from the activation guard or from an
 *   `mfa_required` / `password_required` refusal (C2, C6).
 * - `tokenExpired`: a `password_required` refusal of a minted password token
 *   (#3509). The password field asks again, saying the confirmation expired.
 * - `invalidPassword`, `invalidFactor`: a refusal of what was entered.
 * - `webauthnCancelled`: cancelled, timed out, or the server's session expired.
 * - `webauthnRateLimited`: begin or finish answered 429 (D19). A `rateLimited`
 *   refusal of the gated request is not this: its surface words that one.
 * - `methodsChanged`: a re-read moved the active panel (C13).
 */
export type StepUpNotice =
  | { kind: 'checking' }
  | { kind: 'preparing' }
  | { kind: 'missing'; field: 'password' | StepUpMethod }
  | { kind: 'tokenExpired' }
  | { kind: 'invalidPassword' }
  | { kind: 'invalidFactor'; method: StepUpMethod }
  | { kind: 'webauthnCancelled' }
  | { kind: 'webauthnRateLimited' }
  | { kind: 'methodsChanged' };

/** Why the primary cannot act yet; `unavailable` is a state no input can complete. */
export type StepUpMissing = 'reading' | 'password' | 'code' | 'preparing' | 'unavailable';

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
  | { kind: 'refusal'; refusal: StepUpFactorRefusal }
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
   * `capture` is a host's own (C82); omitted, the run works against the
   * account and server the instance opened for.
   */
  run: (submit: StepUpSubmit, capture?: ApiRequestContext) => Promise<StepUpSubmitOutcome | null>;
  /**
   * For a control that leaves the surface rather than submitting ("Set up
   * verification"): true while the account and server the instance opened for
   * (and `capture`, a host's own, when given) are still current. Otherwise the
   * instance ends as `sessionExpired` and the caller does nothing, so a
   * terminal state left standing through a change cannot act for the new one.
   */
  confirmCurrent: (capture?: ApiRequestContext) => boolean;
}

// ── State and pure transitions ───────────────────────────────────────────

interface FactorConfig {
  purpose: StepUpPurpose | null;
  readFailure: StepUpReadFailure;
  passwordLeg: StepUpPasswordLeg;
  allowBackup: boolean;
  /** `floorMethods`, strongest first, without a security key when there is no purpose. */
  floor: readonly InlineStepUpMethod[];
}

interface FactorState {
  /** The open instance this state belongs to; a change resets it. */
  instance: string;
  /**
   * The account and server the instance opened for. A password typed into it
   * was typed for them, so `run` sends nothing once they are not current.
   */
  opened: ApiRequestContext;
  /** This instance runs the requirements read: it started neither terminal nor read-free. */
  reads: boolean;
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
const WEBAUTHN_RATE_LIMITED: StepUpNotice = { kind: 'webauthnRateLimited' };
const PREPARING: StepUpNotice = { kind: 'preparing' };
const TOKEN_EXPIRED: StepUpNotice = { kind: 'tokenExpired' };

/** The inline methods in `methods` a route with `purpose` can verify: no key without one. */
function verifiable(
  methods: readonly string[],
  purpose: StepUpPurpose | null
): InlineStepUpMethod[] {
  return intersectInline(methods).filter((method) => purpose !== null || method !== 'webauthn');
}

/**
 * The set this route offers for `methods`: its floor plus what it can verify.
 * Every write of `offered` goes through here, so nothing removes a floor
 * member (C26).
 */
function offerable(methods: readonly string[], config: FactorConfig): InlineStepUpMethod[] {
  return verifiable([...config.floor, ...methods], config.purpose);
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

/** `state` offering its floor alone: what the route offers when no inline method is known. */
function toFloor(state: FactorState, config: FactorConfig): FactorState {
  return withOffered(state, offerable([], config), config);
}

/**
 * `state` with the set an authoritative `mfa_required` list names (G1). A list
 * naming nothing this route can collect is the terminal no-usable-method state.
 */
function withRequired(
  state: FactorState,
  methods: readonly string[],
  config: FactorConfig
): FactorState {
  const offered = offerable(methods, config);
  if (offered.length > 0) return withOffered(state, offered, config);
  return { ...state, offered, method: null, status: { kind: 'noUsableMethod' } };
}

/**
 * Applies a refusal-triggered surface's seed (G2). The seeded set is the
 * strongest-first default with no backup code, until a `ready` read replaces
 * it. `methods: null` seeds nothing; enrolment ends the instance at once.
 */
function seeded(
  state: FactorState,
  seed: StepUpFactorRefusal | null | undefined,
  config: FactorConfig
): FactorState {
  switch (seed?.kind) {
    case 'enrollmentRequired':
      return { ...state, status: { kind: 'enrollmentRequired' } };
    case 'mfaRequired':
      return seed.methods === null ? state : withRequired(state, seed.methods, config);
    case 'passwordRequired': {
      // No field to ask in: the default arm, and the host words it.
      if (config.passwordLeg === FACTOR_ONLY_LEG) return state;
      // On `whenNoMfa` the server found no inline method (C2).
      const leg = config.passwordLeg;
      const next = leg === 'whenNoMfa' ? toFloor(state, config) : state;
      // A plain seed is the challenge that opened the surface, so nothing was
      // refused and it says nothing; an expired confirmation says so (#3509).
      return seed.tokenExpired === true ? { ...next, notice: TOKEN_EXPIRED } : next;
    }
    default:
      return state;
  }
}

/**
 * The state an open instance starts in: its floor, then its seed. Only an
 * instance that starts `reading` runs the read, so a terminal seed sends
 * nothing, and neither does a floor route with no purpose.
 */
function initialState(
  instance: string,
  config: FactorConfig,
  seed: StepUpFactorRefusal | null | undefined
): FactorState {
  const readFree = config.purpose === null && config.floor.length > 0;
  const blank: FactorState = {
    instance,
    opened: captureApiRequestContext(),
    reads: false,
    status: readFree ? { kind: 'ready' } : { kind: 'reading' },
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
  const started = seeded(toFloor(blank, config), seed, config);
  return { ...started, reads: started.status.kind === 'reading' };
}

/** After a submit that may have spent the code: drop it and remount the input. */
function spent(state: FactorState): FactorState {
  return { ...state, code: '', attempt: state.attempt + 1 };
}

/** A refusal the surface renders itself: the code may be spent, and nothing is noticed. */
function unrendered(state: FactorState): FactorState {
  return { ...spent(state), notice: null };
}

/** A refusal about the password field, which the `none` leg does not have. */
function namesPassword(refusal: StepUpFactorRefusal): boolean {
  return refusal.kind === 'passwordRequired' || refusal.kind === 'invalidPassword';
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
      const offered = offerable(result.methods, config);
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
  const offered = offerable(result.methods, config);
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
  refusal: StepUpFactorRefusal,
  submitted: StepUpMethod | null,
  config: FactorConfig
): FactorState {
  const idle = { ...state, phase: 'idle' as const };
  // A notice aimed at a field that does not exist would announce nothing and
  // move focus nowhere: the default arm.
  if (config.passwordLeg === FACTOR_ONLY_LEG && namesPassword(refusal)) return unrendered(idle);
  switch (refusal.kind) {
    case 'mfaRequired': {
      // Authoritative over the read; email and SMS never join the set (G1).
      // `null` (#7) names no set, so the read's stands (§2).
      const { methods } = refusal;
      const next = methods === null ? spent(idle) : withRequired(spent(idle), methods, config);
      return {
        ...next,
        notice: next.method === null ? null : { kind: 'missing', field: next.method },
      };
    }
    case 'passwordRequired': {
      // On `whenNoMfa` the server asks for the password only when it found
      // no inline method at submit time, so the set is now the floor (C2).
      const leg = config.passwordLeg;
      const next = leg === 'whenNoMfa' ? toFloor(spent(idle), config) : idle;
      // A refused minted token (#3509) is not an empty field: the password was
      // accepted and the confirmation it bought expired, so the field says so.
      const notice: StepUpNotice =
        refusal.tokenExpired === true ? TOKEN_EXPIRED : { kind: 'missing', field: 'password' };
      return { ...next, notice };
    }
    case 'enrollmentRequired':
      // No code this account could send would pass (E8).
      return { ...spent(idle), status: { kind: 'enrollmentRequired' }, notice: null };
    case 'invalidPassword':
      return { ...idle, notice: { kind: 'invalidPassword' } };
    case 'invalidMfaCode':
      return {
        ...spent(idle),
        notice: submitted === null ? null : { kind: 'invalidFactor', method: submitted },
      };
    case 'rateLimited':
      // The limiter answers before anything is read (C30), so the code is still
      // the user's to send once it lifts; clearing it would cost a backup code
      // the user must find again. The surface words the limit.
      return { ...idle, notice: null };
    case 'sessionExpired':
      // The server declared the session dead after the refresh retry: nothing
      // sent from this instance can pass, so it ends as an account switch does.
      return expired(idle);
    default:
      // The surface renders everything else.
      return unrendered(idle);
  }
}

/**
 * The one refusal that earns a background re-read (C13): the seam refuses a
 * factor removed elsewhere exactly as it refuses a wrong code. Never any other
 * refusal, so one response causes at most one read (C32).
 */
function refusalRereads(refusal: StepUpFactorRefusal): boolean {
  return refusal.kind === 'invalidMfaCode';
}

/**
 * The state an answered submit leaves. An answer that lands once the account
 * or server has changed ends the instance, whatever it was: it belongs to the
 * old one, and the surface must not stand asking for a code for an action that
 * may already have gone through. Otherwise a refusal goes through
 * `refusalTransition`; an unsent request leaves the code unspent; anything
 * else may have spent the code, so it is cleared.
 */
function outcomeUpdate(
  outcome: StepUpSubmitOutcome,
  method: StepUpMethod | null,
  config: FactorConfig,
  contextCurrent: boolean
): (state: FactorState) => FactorState {
  if (!contextCurrent) return expired;
  switch (outcome.kind) {
    case 'refusal': {
      const { refusal } = outcome;
      return (s) => refusalTransition(s, refusal, method, config);
    }
    case 'aborted':
      return (s) => ({ ...s, phase: 'idle' });
    default:
      return (s) => unrendered({ ...s, phase: 'idle' });
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
    // A spent begin or finish quota says nothing about the key (D19): no re-read.
    if (err.status === 429) return { kind: 'failed', notice: WEBAUTHN_RATE_LIMITED, reread: false };
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

/**
 * Clears `ref` only while it still holds `owner`, so a run that settles late
 * never clears what a newer run claimed.
 */
function releaseIfHeld<T>(ref: { current: T | null }, owner: T): void {
  if (ref.current === owner) ref.current = null;
}

function missingNotice(missing: StepUpMissing, method: StepUpMethod | null): StepUpNotice | null {
  switch (missing) {
    case 'reading':
      return { kind: 'checking' };
    case 'password':
      return { kind: 'missing', field: 'password' };
    case 'code':
      return method === null ? null : { kind: 'missing', field: method };
    case 'preparing':
      return PREPARING;
    case 'unavailable':
      return null;
  }
}

function showsPassword(state: FactorState, config: FactorConfig): boolean {
  switch (config.passwordLeg) {
    case 'always':
      return state.status.kind === 'reading' || state.status.kind === 'ready';
    case 'whenNoMfa':
      return state.status.kind === 'ready' && state.offered.length === 0;
    case 'none':
      return false;
  }
}

function firstMissingIn(
  state: FactorState,
  passwordShown: boolean,
  password: string,
  preparing: boolean
): StepUpMissing | null {
  const { kind } = state.status;
  if (kind !== 'reading' && kind !== 'ready') return 'unavailable';
  if (passwordShown && password === '') return 'password';
  if (kind === 'reading') return 'reading';
  if (!codeComplete(state.method, state.code)) return 'code';
  // Last, so the guard names what the user can still fill in first.
  return preparing ? 'preparing' : null;
}

/** A `preparing` notice left over from a preparation that has finished is not shown. */
function shownNotice(notice: StepUpNotice | null, preparing: boolean): StepUpNotice | null {
  return notice?.kind === 'preparing' && !preparing ? null : notice;
}

// ── The hook ─────────────────────────────────────────────────────────────

export function useStepUpFactor({
  enabled,
  purpose,
  passwordLeg,
  readFailure,
  allowBackup,
  seed,
  floorMethods,
  preparing = false,
}: StepUpFactorProps): StepUpFactor {
  // The floor by value: an array literal is a new array on every render.
  const floorKey = floorMethods?.join(',') ?? '';
  const config = useMemo<FactorConfig>(
    () => ({
      purpose,
      readFailure,
      passwordLeg,
      allowBackup,
      floor: verifiable(floorKey.split(','), purpose),
    }),
    [purpose, readFailure, passwordLeg, allowBackup, floorKey]
  );
  // An open instance is `enabled` plus the configuration it opened with; any
  // change starts a fresh one, and the result lives only for that instance.
  // The seed is not part of it: it is read only when an instance starts.
  // Neither is `preparing`, which only holds the primary down.
  const instance = `${enabled}|${purpose}|${readFailure}|${passwordLeg}|${allowBackup}|${floorKey}`;

  const [factorState, setFactorState] = useState(() => initialState(instance, config, seed));
  let state = factorState;
  if (factorState.instance !== instance) {
    state = initialState(instance, config, seed);
    setFactorState(state);
  }

  /** The attempt that holds the single-flight latch (C6), or null. */
  const runningRef = useRef<number | null>(null);
  const runAttemptRef = useRef(0);
  const ceremonyRef = useRef<AbortController | null>(null);
  const readSeqRef = useRef(0);
  const readAbortRef = useRef<AbortController | null>(null);

  /**
   * Ends the current attempt: its ceremony aborts, its proof is dropped (C56),
   * and the latch it held is released.
   */
  const endAttempt = useCallback(() => {
    runAttemptRef.current += 1;
    runningRef.current = null;
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

  const { reads } = state;
  useEffect(() => {
    if (!enabled || !reads) return undefined;
    startRead('initial');
    return () => {
      // Disabled or unmounted: nothing in flight may land (C57).
      readSeqRef.current += 1;
      readAbortRef.current?.abort();
      readAbortRef.current = null;
    };
  }, [enabled, reads, startRead]);

  // The attempt ends in the commit that disables the instance or replaces it
  // with another, not at the later passive flush: a finish that resolves in
  // between would otherwise pass the pre-submit gate after the dialog closed
  // (C56), and an older run's outcome would land on the new instance. A
  // running ceremony stops with it.
  useLayoutEffect(() => {
    if (!enabled) return undefined;
    return endAttempt;
  }, [enabled, instance, endAttempt]);

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
    (password: string) => firstMissingIn(state, passwordLegShown, password, preparing),
    [state, passwordLegShown, preparing]
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
      if (runningRef.current !== null || !enabled || state.phase !== 'idle') return null;
      if (state.status.kind !== 'ready') return null;
      const method = state.method;
      const code = codeToSend(method, state.code);
      if (code === '') return null;

      const attempt = ++runAttemptRef.current;
      runningRef.current = attempt; // C6: set before the first await.
      // This run's answer is newer than any re-read an earlier refusal started,
      // so that read must not land over it (C13, C32).
      readSeqRef.current += 1;
      readAbortRef.current?.abort();
      readAbortRef.current = null;
      const { opened } = state;
      const context = capture ?? opened; // C82
      // A host's capture counts only while the instance's own does: one taken
      // after a change must not carry what was typed before it.
      const current = () =>
        apiRequestContextIsCurrent(context) && apiRequestContextIsCurrent(opened);
      const accountId = useUserStore.getState().user?.id ?? null;
      const controller = new AbortController();
      ceremonyRef.current = controller;
      const gate = (): RunGate => {
        if (attempt !== runAttemptRef.current || controller.signal.aborted) return 'stale';
        return current() ? 'live' : 'sessionExpired';
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
        setFactorState(outcomeUpdate(outcome, method, config, current()));
        // An instance that runs no read runs no re-read either (C4).
        if (outcome.kind === 'refusal' && refusalRereads(outcome.refusal) && state.reads) {
          startRead('reread');
        }
        return outcome;
      } finally {
        releaseIfHeld(runningRef, attempt);
        releaseIfHeld(ceremonyRef, controller);
      }
    },
    [enabled, state, config, startRead]
  );

  const confirmCurrent = useCallback(
    (capture?: ApiRequestContext) => {
      const { opened } = state;
      const current =
        apiRequestContextIsCurrent(capture ?? opened) && apiRequestContextIsCurrent(opened);
      if (!current) setFactorState(expired);
      return current;
    },
    [state]
  );

  return {
    status: state.status,
    methods,
    method: state.method,
    passwordLegShown,
    code: state.code,
    attempt: state.attempt,
    phase: state.phase,
    notice: shownNotice(state.notice, preparing),
    reread: state.reread,
    setCode,
    switchTo,
    retryRead,
    firstMissing,
    announceMissing,
    run,
    confirmCurrent,
  };
}
