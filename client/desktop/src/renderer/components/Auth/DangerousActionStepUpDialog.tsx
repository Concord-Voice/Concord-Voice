import React, { useEffect, useId, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import ErrorBanner from '../Settings/ErrorBanner';
import { STEP_UP_RETRY_TEXT, stepUpBanner } from '../Settings/mfaStepUp';
import StepUpCredentials, { stepUpActivation } from './StepUpCredentials';
import type { StepUpPurpose } from './stepUpPurpose';
import {
  FACTOR_ONLY_LEG,
  useStepUpFactor,
  type StepUpFactorRefusal,
  type StepUpPhase,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import {
  apiRequestContextIsCurrent,
  isAbortError,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import { adaptDangerousActionRefusal } from '../../services/system/stepUpRouteAdapters';
// ErrorBanner's rule lives there; Settings is lazy, and this dialog also opens from chat.
import '../Settings/MFA.css';
import './DangerousActionStepUpDialog.css';

/**
 * What the host's request came to. `refused` is any non-2xx, with its parsed
 * body (null when it did not parse). `aborted` is apiFetch's pre-dispatch
 * fence: nothing was sent.
 */
export type DangerousActionSendResult =
  | { kind: 'ok' }
  | { kind: 'refused'; status: number; body: unknown }
  | { kind: 'transport' }
  | { kind: 'aborted' };

export interface DangerousActionStepUpDialogProps {
  isOpen: boolean;
  /** The gated route's purpose: a security-key token is accepted only there. */
  purpose: StepUpPurpose;
  /**
   * The refusal of the code-less first send that opened the dialog, read once
   * when it opens (G2). Omitted, the dialog is proactive.
   */
  seed?: StepUpFactorRefusal | null;
  /** The sentence that names the action. It also describes the dialog. */
  intro: string;
  /** The primary's label, named for the action ("Delete Channel"). */
  primaryLabel: string;
  /** The primary's label while the request is out ("Deleting…"). */
  busyLabel: string;
  /**
   * Re-sends the request the host froze at its first send, plus `mfa_code`
   * when a factor was proven, admitted against `context`. Never re-reads the
   * host's form.
   */
  send: (
    mfaCode: string | undefined,
    context: ApiRequestContext
  ) => Promise<DangerousActionSendResult>;
  /**
   * The account and server the host's first send went out as (C82). Given, the
   * re-send and "Set up verification" work against it and nothing later: a
   * refusal that asked one account for a code never re-sends its request as
   * another. Omitted (a proactive dialog), the dialog's own opening capture.
   */
  capture?: ApiRequestContext;
  /** The host's sentence for a refusal the dangerous-action adapter does not own. */
  describeFailure: (status: number, body: unknown) => string;
  /** The stage's sentence for an expired session or an account or server change. */
  sessionMessage?: string;
  /** The action succeeded for the account and server that are still current. */
  onSuccess: () => void;
  /** Cancel, Escape, the close button, and "Set up verification" all land here. */
  onClose: () => void;
  /**
   * Opens verification setup. Given, the enrolment state offers it. The dialog
   * does not close first: the host calls `openVerificationSetup` with its own
   * close as `closeHost`, which runs only after any discard guard has passed
   * (D-4), so a declined discard leaves this dialog as it was.
   */
  onSetUpVerification?: () => void;
  /**
   * Where focus goes on close when `ui/Modal` could not return it to whatever
   * opened the dialog (a swapped confirmation, a closed menu): the channel
   * list, the server rail, the role list. Never `<body>`.
   */
  focusFallback: () => HTMLElement | null;
}

/** Constant: `ui/Modal` binds the title to `aria-labelledby`. */
const TITLE = "Confirm it's you";
// `stepUpBanner`'s words, so a spent budget and a lost connection read the same
// on every step-up surface.
const RATE_LIMITED_TEXT = stepUpBanner({ kind: 'rateLimited' }) ?? '';
const NETWORK_TEXT = stepUpBanner({ kind: 'networkError' }) ?? '';

/** The `none` leg shows no password field; the stage still takes the props. */
const NO_PASSWORD = '';
function ignorePassword(): void {
  // Nothing to hold: the stage never renders a password field on this leg.
}

/** What one send came to: the hook's outcome, and the banner beside it ('' for none). */
interface Answer {
  outcome: StepUpSubmitOutcome;
  banner: string;
}

/**
 * The banner for a refusal the hook applies. A field refusal and the
 * enrolment state are the stage's; so is a dead session, which the stage's
 * terminal state words with `sessionMessage` (a banner would say it twice).
 */
function refusalBanner(refusal: StepUpFactorRefusal): string {
  switch (refusal.kind) {
    case 'rateLimited':
      return RATE_LIMITED_TEXT;
    case 'unavailable':
      return STEP_UP_RETRY_TEXT;
    default:
      return '';
  }
}

function answerFor(
  result: DangerousActionSendResult,
  describeFailure: DangerousActionStepUpDialogProps['describeFailure']
): Answer {
  switch (result.kind) {
    case 'ok':
      return { outcome: { kind: 'success' }, banner: '' };
    case 'refused': {
      const refusal = adaptDangerousActionRefusal(result.status, result.body);
      // Not a step-up answer: the host's own words, and the code may be spent.
      if (refusal === null) {
        return {
          outcome: { kind: 'answered' },
          banner: describeFailure(result.status, result.body),
        };
      }
      return { outcome: { kind: 'refusal', refusal }, banner: refusalBanner(refusal) };
    }
    case 'transport':
      return { outcome: { kind: 'transport' }, banner: NETWORK_TEXT };
    // Nothing was sent. Shown only while the context is current (the stale
    // case ends the stage), so the click never visibly does nothing.
    case 'aborted':
      return { outcome: { kind: 'aborted' }, banner: STEP_UP_RETRY_TEXT };
  }
}

/** `send`, with a throw read as the hook reads one: an abort sent nothing, anything else may have. */
async function sendOnce(
  send: DangerousActionStepUpDialogProps['send'],
  mfa: string | undefined,
  context: ApiRequestContext
): Promise<DangerousActionSendResult> {
  try {
    return await send(mfa, context);
  } catch (err) {
    return isAbortError(err) ? { kind: 'aborted' } : { kind: 'transport' };
  }
}

function primaryText(phase: StepUpPhase, primaryLabel: string, busyLabel: string): string {
  switch (phase) {
    case 'idle':
      return primaryLabel;
    case 'ceremony':
      return 'Waiting…';
    case 'submitting':
      return busyLabel;
  }
}

/**
 * A banner and the answer that set it: a repeat of the same words is still a
 * new answer. `locks` is a rate limit: no retry can pass until the budget
 * resets, so the primary stays down until the dialog is reopened (D-8,
 * `isStepUpLocked`'s rule) and focus goes to Cancel instead.
 */
interface Banner {
  text: string;
  id: number;
  locks: boolean;
}

type StageProps = Omit<DangerousActionStepUpDialogProps, 'isOpen' | 'focusFallback'>;

/**
 * The open dialog. Mounted only while open, so the factor instance, the code
 * and the banner die with each close.
 */
const Stage: React.FC<StageProps> = ({
  purpose,
  seed = null,
  intro,
  primaryLabel,
  busyLabel,
  send,
  capture,
  describeFailure,
  sessionMessage,
  onSuccess,
  onClose,
  onSetUpVerification,
}) => {
  const introId = useId();
  const primaryRef = useRef<HTMLButtonElement>(null);
  const cancelRef = useRef<HTMLButtonElement>(null);
  const bannerSeqRef = useRef(0);
  const [banner, setBanner] = useState<Banner | null>(null);

  // No D1 gate reads a password (#3456 V18). A failed read keeps the set the
  // seed named (G2), and a gate accepts a backup code wherever TOTP is active.
  const factor = useStepUpFactor({
    enabled: true,
    purpose,
    passwordLeg: FACTOR_ONLY_LEG,
    readFailure: 'passwordOnly',
    allowBackup: true,
    seed,
  });
  const submitting = factor.phase === 'submitting';
  // Once locked, the dialog stays locked: a later banner never lifts it.
  const [locked, setLocked] = useState(false);

  // After a banner the retry is one click away, so focus goes to the primary;
  // after a rate limit there is no retry, so it goes to Cancel (D-8). A dead
  // session sets no banner: the stage's terminal state places its own focus.
  useLayoutEffect(() => {
    if (banner === null) return;
    (banner.locks ? cancelRef : primaryRef).current?.focus();
  }, [banner]);

  // Cancel goes natively disabled while the request is out, and a security
  // key's ceremony leaves it reachable until then. Focus on it would drop to
  // <body>, so it moves to the primary, which `aria-disabled` keeps focusable.
  useLayoutEffect(() => {
    if (submitting && document.activeElement === cancelRef.current) primaryRef.current?.focus();
  }, [submitting]);

  const submit: StepUpSubmit = async (mfa, context) => {
    const { outcome, banner: text } = answerFor(
      await sendOnce(send, mfa, context),
      describeFailure
    );
    // An answer for an account or server that is no longer current belongs to
    // the old one: the hook ends the stage, and nothing here shows or acts on it.
    if (!apiRequestContextIsCurrent(context)) return outcome;
    bannerSeqRef.current += 1;
    const locks = outcome.kind === 'refusal' && outcome.refusal.kind === 'rateLimited';
    if (locks) setLocked(true);
    setBanner(text === '' ? null : { text, id: bannerSeqRef.current, locks });
    if (outcome.kind === 'success') onSuccess();
    return outcome;
  };
  const activation = stepUpActivation(factor, NO_PASSWORD, submit, { capture });
  const ariaDisabled = locked || activation.ariaDisabled;
  const activate = () => {
    if (locked) return;
    // A banner speaks for the attempt that set it; a new attempt that ends
    // without reaching `submit` (a cancelled key) must not leave it standing.
    setBanner(null);
    activation.activate();
  };

  return (
    <Modal
      isOpen
      onClose={onClose}
      title={TITLE}
      width="small"
      dismissable={!submitting}
      describedById={introId}
    >
      <div className="dangerous-step-up">
        <p id={introId} className="dangerous-step-up__intro">
          {intro}
        </p>
        <StepUpCredentials
          factor={factor}
          password={NO_PASSWORD}
          onPasswordChange={ignorePassword}
          primaryRef={primaryRef}
          sessionMessage={sessionMessage}
          focusOnReady
          onSetUpVerification={onSetUpVerification}
          capture={capture}
        />
        {/* Keyed by answer, so a repeated sentence is announced again. */}
        {banner !== null && <ErrorBanner key={banner.id} error={banner.text} />}
        <div className="dangerous-step-up__actions">
          <button
            ref={cancelRef}
            type="button"
            className="dangerous-step-up__cancel"
            disabled={submitting}
            onClick={onClose}
          >
            Cancel
          </button>
          {/* aria-disabled, never native `disabled`: the guarded click names what
              is missing, and a natively disabled button would drop focus to <body>. */}
          <button
            ref={primaryRef}
            type="button"
            className="dangerous-step-up__confirm"
            aria-disabled={ariaDisabled || undefined}
            onClick={activate}
          >
            {primaryText(factor.phase, primaryLabel, busyLabel)}
          </button>
        </div>
      </div>
    </Modal>
  );
};

/**
 * The step-up for a dangerous action on an MFA-enforcing server, and for
 * turning that enforcement off (#3456 §3.3): one wrapper over the shared
 * `StepUpCredentials` stage for every gated host.
 *
 * The host owns the request. It freezes what it sent first and re-sends
 * exactly that through `send`; this dialog owns the factor, reads each refusal
 * through `adaptDangerousActionRefusal`, and words what is not about a field.
 * The code lives only in the factor hook, is never stored and never logged.
 */
const DangerousActionStepUpDialog: React.FC<DangerousActionStepUpDialogProps> = ({
  isOpen,
  focusFallback,
  ...stage
}) => {
  // On close, `ui/Modal`'s cleanup has already tried to return focus to what
  // opened the dialog; this setup runs after it in the same commit. When that
  // element has left the document, focus is on <body>, and the host's
  // fallback takes it instead.
  const wasOpenRef = useRef(isOpen);
  useEffect(() => {
    const closed = wasOpenRef.current && !isOpen;
    wasOpenRef.current = isOpen;
    if (!closed) return;
    const active = document.activeElement;
    if (active === null || active === document.body) focusFallback()?.focus();
  }, [isOpen, focusFallback]);

  return isOpen ? <Stage {...stage} /> : null;
};

export default DangerousActionStepUpDialog;
