import React, { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import type {
  StepUpFactor,
  StepUpMethod,
  StepUpNotice,
  StepUpSubmit,
} from '../../hooks/auth/useStepUpFactor';
import {
  isTotpHintActive,
  totpHintExpiresAt,
  type StepUpReadRefusalReason,
} from '../../services/system/stepUpRequirements';
import { useTotpAcceptedStore } from '../../stores/auth/totpAcceptedStore';
import { useUserStore } from '../../stores/auth/userStore';
import MFAFactorPicker, { StepUpFieldError, type MFAFactorPickerHandle } from './MFAFactorPicker';
// The stage's own rules (.step-up, .step-up__region, .step-up__status) share the
// picker's sheet; imported here too so the stage never depends on the picker
// import to be styled.
import './MFAFactorPicker.css';

/**
 * The credential fields a gated action's modal shows before its primary button
 * (design 2026-09-26-mfa-factor-picker §4.1, §4.2; plan 2026-10-07 §3).
 *
 * The host calls `useStepUpFactor`, because it owns `run`, and passes the result
 * in. Everything here is rendering plus the focus table: the host owns the
 * heading, the intro sentence, the footer and the primary button, and wires the
 * activation guard with `stepUpActivation` below.
 */

export interface StepUpCredentialsProps {
  factor: StepUpFactor;
  /** The host's password value. Sent by the host's own request, never stored here. */
  password: string;
  onPasswordChange: (value: string) => void;
  /** The host's primary button. A passkey refusal, cancel or timeout returns focus to it. */
  primaryRef: React.RefObject<HTMLElement | null>;
  /**
   * What takes focus on a terminal state (`refused`, no usable method, session
   * expired): the stage heading, `tabIndex={-1}`. Omitted, the enclosing
   * `<dialog>` takes it, which is `ui/Modal`'s own focus target.
   */
  headingRef?: React.RefObject<HTMLElement | null>;
  /** Lets a host aim `Modal`'s `initialFocusRef` at the password field. */
  passwordRef?: React.RefObject<HTMLInputElement | null>;
  /** The sentence for a `session` refusal and for an account or server change mid-run. */
  sessionMessage?: string;
}

/** What the host needs to wire its primary button. */
export interface StepUpActivation {
  /** The primary's `aria-disabled`: something is missing, or an activation is running. */
  ariaDisabled: boolean;
  /**
   * The primary's click handler. It queues nothing: a busy primary does
   * nothing, an incomplete one shows what is missing, and only a complete one
   * runs the activation.
   */
  activate: () => void;
}

/**
 * The primary button's activation guard (design §4.2), in one place so the
 * three hosts cannot drift. `aria-disabled` alone does not stop a click, so the
 * handler guards the same condition (C6).
 */
export function stepUpActivation(
  factor: StepUpFactor,
  password: string,
  submit: StepUpSubmit
): StepUpActivation {
  const missing = factor.firstMissing(password);
  const busy = factor.phase !== 'idle';
  return {
    ariaDisabled: busy || missing !== null,
    activate: () => {
      if (busy) return;
      if (missing === null) void factor.run(submit);
      else factor.announceMissing(missing);
    },
  };
}

const READING_NOTICE_DELAY_MS = 400;
const CHECKING_TEXT = 'Checking your verification methods…';
const DEFAULT_SESSION_MESSAGE = 'Sign in again to continue.';

// ── Copy (design §4.2; the UI never says "factor" or "step-up") ──────────

function refusedText(reason: StepUpReadRefusalReason, sessionMessage: string): string {
  switch (reason) {
    case 'account':
      return "Your account can't do this right now.";
    case 'emailUnverified':
      return 'Verify your email address to do this.';
    case 'client':
      return "This isn't available right now.";
    case 'session':
      return sessionMessage;
  }
}

/** An empty-field message, by the panel it names (C6). */
const MISSING_TEXT: Record<StepUpMethod, string> = {
  totp: 'Enter the 6-digit code from your authenticator app to continue.',
  backup: 'Enter a backup code to continue.',
  webauthn: 'This also needs your passkey or security key. Try again to use it.',
};

const INVALID_TEXT: Record<StepUpMethod, string> = {
  totp: "That code didn't work. It may be mistyped or already used. Enter the next code your app shows.",
  backup: 'That backup code is not correct, or it was already used.',
  webauthn: "We couldn't verify your passkey or security key. Try again.",
};

/** Where a notice shows: on the password field, on the active panel, or in the status line. */
interface NoticePlacement {
  slot: 'password' | 'panel' | 'status';
  text: string;
}

function textIn(placement: NoticePlacement | null, slot: NoticePlacement['slot']): string | null {
  return placement?.slot === slot ? placement.text : null;
}

function placeNotice(
  notice: StepUpNotice | null,
  method: StepUpMethod | null
): NoticePlacement | null {
  if (notice === null) return null;
  switch (notice.kind) {
    case 'checking':
      return { slot: 'status', text: CHECKING_TEXT };
    case 'webauthnCancelled':
      return {
        slot: 'status',
        text: 'Passkey or security key request was cancelled or timed out. Try again.',
      };
    case 'methodsChanged':
      return { slot: 'status', text: 'Your verification methods changed. Use the one shown.' };
    case 'invalidPassword':
      return {
        slot: 'password',
        text:
          method === 'webauthn'
            ? "That password is not correct. You'll be asked for your passkey or security key again."
            : 'That password is not correct.',
      };
    case 'missing':
      return notice.field === 'password'
        ? { slot: 'password', text: 'Enter your password to continue.' }
        : { slot: 'panel', text: MISSING_TEXT[notice.field] };
    case 'invalidFactor':
      return { slot: 'panel', text: INVALID_TEXT[notice.method] };
  }
}

/** The one line the `<output>` carries: a terminal or blocked sentence, else a ceremony or notice status. */
function statusLine(
  factor: StepUpFactor,
  noticeStatus: string | null,
  readingSlow: boolean,
  sessionMessage: string
): string {
  const { status } = factor;
  switch (status.kind) {
    case 'blocked':
      return "We couldn't check your verification methods. Check your connection and try again.";
    case 'refused':
      return refusedText(status.reason, sessionMessage);
    case 'noUsableMethod':
      return "Your account's verification method can't be used here. Add an authenticator app or security key in Settings.";
    case 'sessionExpired':
      return sessionMessage;
    case 'reading':
    case 'ready':
      break;
  }
  if (factor.phase === 'ceremony') return 'Waiting for your passkey or security key…';
  if (noticeStatus !== null) return noticeStatus;
  return status.kind === 'reading' && readingSlow ? CHECKING_TEXT : '';
}

// ── Focus (plan §3's table, for everything the factor does on its own) ───

type FocusTarget = 'password' | 'panel' | 'primary';

/**
 * Where focus goes when a notice appears. A passkey refusal, cancel or timeout
 * goes to the primary, because retrying needs a touch; a code refusal goes to
 * the remounted input; a password refusal to the password field.
 */
function noticeFocus(notice: StepUpNotice | null): FocusTarget | null {
  switch (notice?.kind) {
    case 'invalidPassword':
      return 'password';
    case 'missing':
      if (notice.field === 'password') return 'password';
      return notice.field === 'webauthn' ? 'primary' : 'panel';
    case 'invalidFactor':
      return notice.method === 'webauthn' ? 'primary' : 'panel';
    case 'webauthnCancelled':
      return 'primary';
    case 'methodsChanged':
      return 'panel';
    default:
      return null;
  }
}

function focusStageHeading(
  heading: React.RefObject<HTMLElement | null> | undefined,
  root: HTMLElement | null
): void {
  (heading?.current ?? root?.closest('dialog'))?.focus();
}

// ── Time-driven state ────────────────────────────────────────────────────

/** True once `active` has held for `ms`, and false again as soon as it lets go. */
function useAfterDelay(active: boolean, ms: number): boolean {
  const [elapsed, setElapsed] = useState(false);
  useEffect(() => {
    if (!active) return undefined;
    const timer = setTimeout(() => setElapsed(true), ms);
    return () => {
      clearTimeout(timer);
      setElapsed(false);
    };
  }, [active, ms]);
  return active && elapsed;
}

/**
 * True while this account's last accepted TOTP code is still in the period the
 * server will refuse a repeat from (S2a). `now` moves only at the hint's expiry,
 * so the hint withdraws itself without a ticking clock.
 */
function useRecentlyUsedTotp(): boolean {
  const accountId = useUserStore((s) => s.user?.id);
  const acceptedAt = useTotpAcceptedStore((s) =>
    accountId === undefined ? undefined : s.acceptedAt[accountId]
  );
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (acceptedAt === undefined) return undefined;
    const expiresAt = totpHintExpiresAt(acceptedAt);
    const remaining = expiresAt - Date.now();
    if (remaining <= 0) return undefined;
    // Never earlier than the expiry: a timer that fires a millisecond early
    // would otherwise leave the hint up, with nothing left to reschedule it.
    const timer = setTimeout(() => setNow(Math.max(Date.now(), expiresAt)), remaining);
    return () => clearTimeout(timer);
  }, [acceptedAt]);
  return isTotpHintActive(acceptedAt, now);
}

// ── Component ────────────────────────────────────────────────────────────

const StepUpCredentials: React.FC<StepUpCredentialsProps> = ({
  factor,
  password,
  onPasswordChange,
  primaryRef,
  headingRef,
  passwordRef: hostPasswordRef,
  sessionMessage = DEFAULT_SESSION_MESSAGE,
}) => {
  const { status, phase, notice, method, methods } = factor;
  const ownPasswordRef = useRef<HTMLInputElement>(null);
  const passwordRef = hostPasswordRef ?? ownPasswordRef;
  const rootRef = useRef<HTMLFieldSetElement>(null);
  const pickerRef = useRef<MFAFactorPickerHandle>(null);
  const previousMethodRef = useRef(method);
  const baseId = useId();
  const passwordId = `${baseId}-password`;
  const passwordErrorId = `${passwordId}-error`;

  const readingSlow = useAfterDelay(status.kind === 'reading', READING_NOTICE_DELAY_MS);
  const recentlyUsedCode = useRecentlyUsedTotp();

  const placement = placeNotice(notice, method);
  const passwordText = textIn(placement, 'password');
  const panelText = textIn(placement, 'panel');
  const showPicker = status.kind === 'ready' && method !== null && methods.length > 0;
  const terminal =
    status.kind === 'refused' ||
    status.kind === 'noUsableMethod' ||
    status.kind === 'sessionExpired';

  // A switch, user-made or automatic, lands on the new panel; an empty offered
  // set lands on the heading. Declared before the notice effect, which wins
  // when a refusal changes the panel and also names where focus goes.
  useLayoutEffect(() => {
    const before = previousMethodRef.current;
    previousMethodRef.current = method;
    if (before === null || before === method) return;
    if (method === null) focusStageHeading(headingRef, rootRef.current);
    else pickerRef.current?.focus();
  }, [method, headingRef]);

  useLayoutEffect(() => {
    const target = noticeFocus(notice);
    if (target === 'password') passwordRef.current?.focus();
    else if (target === 'primary') primaryRef.current?.focus();
    else if (target === 'panel') {
      if (pickerRef.current === null) focusStageHeading(headingRef, rootRef.current);
      else pickerRef.current.focus();
    }
  }, [notice, passwordRef, primaryRef, headingRef]);

  useLayoutEffect(() => {
    if (terminal) focusStageHeading(headingRef, rootRef.current);
  }, [terminal, headingRef]);

  // A terminal stage can send nothing, so the password it hid is dropped
  // rather than held until the dialog closes.
  useEffect(() => {
    if (terminal && password !== '') onPasswordChange('');
  }, [terminal, password, onPasswordChange]);

  // The Retry button unmounts when the read restarts; focus moves to the heading
  // first so it never falls to <body>.
  const retry = () => {
    focusStageHeading(headingRef, rootRef.current);
    factor.retryRead();
  };

  return (
    <fieldset className="step-up" ref={rootRef} disabled={phase === 'submitting'}>
      {factor.passwordLegShown && (
        <div className="step-up__field">
          <label htmlFor={passwordId} className="step-up__label">
            Password
          </label>
          <input
            id={passwordId}
            ref={passwordRef}
            className="step-up__input"
            type="password"
            autoComplete="current-password"
            value={password}
            onChange={(e) => onPasswordChange(e.target.value)}
            // `run` captured the value at activation, so an edit mid-ceremony would not be the one sent.
            readOnly={phase === 'ceremony'}
            aria-invalid={passwordText !== null || undefined}
            aria-describedby={passwordText === null ? undefined : passwordErrorId}
          />
          {passwordText !== null && (
            <StepUpFieldError id={passwordErrorId} message={passwordText} />
          )}
        </div>
      )}

      <div
        className={
          status.kind === 'reading' || showPicker
            ? 'step-up__region step-up__region--reserved'
            : 'step-up__region'
        }
      >
        {showPicker && (
          <MFAFactorPicker
            ref={pickerRef}
            methods={methods}
            method={method}
            code={factor.code}
            attempt={factor.attempt}
            onCodeChange={factor.setCode}
            onSwitch={factor.switchTo}
            recentlyUsedCode={recentlyUsedCode}
            error={panelText}
          />
        )}
        <output className="step-up__status">
          {statusLine(factor, textIn(placement, 'status'), readingSlow, sessionMessage)}
        </output>
        {status.kind === 'blocked' && (
          <button type="button" className="step-up__link" onClick={retry}>
            Retry
          </button>
        )}
      </div>
    </fieldset>
  );
};

export default StepUpCredentials;
