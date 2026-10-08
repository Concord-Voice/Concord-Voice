import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import {
  LEG_ONLY_WITHOUT_MFA,
  useStepUpFactor,
  type StepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import type { DeleteRefusalState, DeleteStepUp } from '../../hooks/messaging/useChatController';
import { softLockSeed, type DeleteRefusalView } from '../../services/messaging/deleteRefusal';
import type { ApiRequestContext } from '../../services/system/requestContext';
import { findSurfaceComposer, findSurfaceMessageRow } from './chatSurface';
import './DeleteRefusalModal.css';

export interface DeleteRefusalModalProps {
  /** The hook's one refusal slot. `null` renders nothing. */
  refusal: DeleteRefusalState | null;
  /**
   * Re-sends the same delete with a factor, against the activation's own
   * request context. Resolves to the factor hook's reading of the retry.
   */
  onConfirm: (step: DeleteStepUp, context?: ApiRequestContext) => Promise<StepUpSubmitOutcome>;
  onDismiss: () => void;
  /** `dm.message_delete` in a DM, `messages.delete` everywhere else. */
  purpose: StepUpPurpose;
  /** The owning chat panel's id (#1959): focus returns to its row or its composer, never to
   *  another panel's, where the next message would go to a different conversation. */
  surfaceId: string;
}

/**
 * The views that hold a credential stage; the rest are Close-only. Enrolment is
 * one of them because it is the stage's own terminal state (E8): it shares the
 * title and the intro, and renders its sentence through the stage.
 */
type CredentialView = Extract<DeleteRefusalView, { view: 'confirm' | 'password' | 'enroll' }>;

function isCredentialView(view: DeleteRefusalView): view is CredentialView {
  return view.view === 'confirm' || view.view === 'password' || view.view === 'enroll';
}

const TITLES: Record<DeleteRefusalState['view']['view'], string> = {
  confirm: "Confirm it's you",
  password: "Confirm it's you",
  enroll: "Confirm it's you",
  wait: 'Deleting too quickly',
  unavailable: "Can't delete right now",
  failed: "Couldn't delete that message",
};

/** The two 429s read differently: the route limiter is about the delete rate,
 *  a spent step-up budget is about wrong codes or passwords, and blaming the
 *  delete rate for the second misleads for up to its 15-minute window. */
function titleFor(view: DeleteRefusalView): string {
  if (view.view === 'wait' && view.reason === 'verification') return 'Too many attempts';
  return TITLES[view.view];
}

const VERIFICATION_WAIT_COPY = 'Too many verification attempts.';

/**
 * Whole seconds left of `retryAfterSeconds`, measured from `openedAt` against
 * the wall clock rather than by counting interval ticks, so a throttled timer
 * cannot drift the displayed value (design spec §2.10 / handoff T7). `null`
 * when there is nothing to count down. `openedAt` is the moment the refusal
 * that carried the header arrived, so the value never exceeds the header.
 */
function useCountdown(
  openedAt: number | undefined,
  retryAfterSeconds: number | undefined
): number | null {
  const [now, setNow] = useState(() => Date.now());
  const counting = openedAt !== undefined && retryAfterSeconds !== undefined;

  useEffect(() => {
    if (!counting) return;
    const id = setInterval(() => setNow(Date.now()), 250);
    return () => clearInterval(id);
  }, [counting]);

  if (openedAt === undefined || retryAfterSeconds === undefined) return null;
  const elapsedMs = Math.max(0, now - openedAt);
  return Math.max(0, Math.ceil(retryAfterSeconds - elapsedMs / 1000));
}

const CHALLENGE_COPY = "You've deleted several messages quickly. Confirm it's you to keep going.";

/** Body text of the three Close-only views. The ticking countdown lives here,
 *  outside any live region. */
function closedBodyCopy(
  view: Extract<DeleteRefusalView, { view: 'wait' | 'unavailable' | 'failed' }>,
  remaining: number | null
): string {
  if (view.view === 'unavailable') {
    return 'Deleting messages is temporarily unavailable. Try again in a moment.';
  }
  if (view.view === 'wait') {
    // The budget sends no Retry-After and its window is 15 minutes, so
    // "shortly" would undersell it.
    if (remaining === null) {
      return view.reason === 'verification' ? 'Try again in a few minutes.' : 'Try again shortly.';
    }
    return remaining > 0 ? `Try again in ${remaining}s.` : 'You can try again now.';
  }
  const base = view.message ?? 'Something went wrong. Try again.';
  return remaining !== null && remaining > 0 ? `${base} You can try again in ${remaining}s.` : base;
}

/** The primary's label by phase. Text only: this modal carries no glyphs (see its stylesheet). */
const PRIMARY_LABEL: Record<StepUpPhase, string> = {
  idle: 'Confirm',
  ceremony: 'Waiting…',
  submitting: 'Confirming…',
};

/**
 * What the retry carries: the code or security-key token the hook proved, else
 * the password the leg collected. The password never reaches the delete route;
 * the controller exchanges it for a token first (#3509).
 */
function stepFor(mfa: string | undefined, password: string): DeleteStepUp {
  return mfa === undefined ? { currentPassword: password } : { mfaCode: mfa };
}

interface CredentialStageProps {
  factor: StepUpFactor;
  describedById: string;
  onConfirm: DeleteRefusalModalProps['onConfirm'];
  onCancel: () => void;
}

/**
 * The credential stage: the intro, the picker's fields, and the footer. It owns
 * the password, so the one wire secret dies with the stage whenever the refusal
 * closes or gives way to a Close-only view. The code lives in the factor hook.
 */
const CredentialStage: React.FC<CredentialStageProps> = ({
  factor,
  describedById,
  onConfirm,
  onCancel,
}) => {
  // Component-local state only: never a store, never logged
  // ([internal]rules/observability.md).
  const [password, setPassword] = useState('');
  const primaryRef = useRef<HTMLButtonElement>(null);
  const submitting = factor.phase === 'submitting';

  const submit: StepUpSubmit = async (mfa, context) => {
    const outcome = await onConfirm(stepFor(mfa, password), context);
    // Sent once, to the mint; nothing keeps it after that, whatever view the
    // attempt ends on (#3509 frontend review). `aborted` sent nothing, so what
    // was typed is still what the person means to send.
    if (outcome.kind !== 'aborted') setPassword('');
    return outcome;
  };
  const { ariaDisabled, activate } = stepUpActivation(factor, password, submit);

  return (
    <div className="delete-refusal-modal__stage">
      <p id={describedById} className="delete-refusal-modal__body">
        {CHALLENGE_COPY}
      </p>
      <StepUpCredentials
        factor={factor}
        password={password}
        onPasswordChange={setPassword}
        primaryRef={primaryRef}
        focusOnReady
      />
      <div className="delete-refusal-modal__actions">
        <button
          type="button"
          className="delete-refusal-modal__cancel"
          onClick={onCancel}
          disabled={submitting}
        >
          Cancel
        </button>
        {/* aria-disabled, never native `disabled`: the guarded click names what
            is missing, and a natively disabled button would drop focus to <body>. */}
        <button
          ref={primaryRef}
          type="button"
          className="delete-refusal-modal__confirm"
          aria-disabled={ariaDisabled || undefined}
          onClick={activate}
        >
          {PRIMARY_LABEL[factor.phase]}
        </button>
      </div>
    </div>
  );
};

const DeleteRefusalModal: React.FC<DeleteRefusalModalProps> = ({
  refusal,
  onConfirm,
  onDismiss,
  purpose,
  surfaceId,
}) => {
  const view = refusal?.view;
  const messageId = refusal?.messageId ?? null;

  // `seed` is read only when an instance starts, so it travels in the render
  // that flips `enabled`. The refusal that opened the dialog already carries the
  // account's methods, which stand through a failed read (G2). A password view
  // seeds the empty set on this `whenNoMfa` leg, and enrolment ends the instance
  // at once. `readFailure: 'passwordOnly'`: a failed read must not lock out a
  // delete the server would accept, and the retry is the check that counts.
  const factor = useStepUpFactor({
    enabled: view !== undefined && isCredentialView(view),
    purpose,
    passwordLeg: LEG_ONLY_WITHOUT_MFA,
    readFailure: 'passwordOnly',
    allowBackup: true,
    seed: view === undefined ? null : softLockSeed(view),
  });
  const submitting = factor.phase === 'submitting';

  const countdownRetryAfter =
    view?.view === 'wait' || view?.view === 'failed' ? view.retryAfterSeconds : undefined;
  const remaining = useCountdown(refusal?.openedAt, countdownRetryAfter);
  const announceZero = remaining === 0;

  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const initialFocusRef = useRef<HTMLElement | null>(null);

  // Runs before Modal's own mount-focus effect (useLayoutEffect vs useEffect),
  // so a dialog that opens on a Close-only view lands on Close; the button is
  // mounted only on those views, so a credential view leaves this null and the
  // dialog takes focus (the stage hands it on once it has a field). Modal only
  // focuses on OPEN, so a later change of view (confirm -> wait, which unmounts
  // the focused field) re-focuses here rather than letting focus fall to <body>.
  const prevViewKindRef = useRef<DeleteRefusalView['view'] | null>(null);
  useLayoutEffect(() => {
    const kind = view?.view ?? null;
    initialFocusRef.current = closeButtonRef.current;
    if (prevViewKindRef.current !== null && prevViewKindRef.current !== kind) {
      closeButtonRef.current?.focus();
    }
    prevViewKindRef.current = kind;
  }, [view]);

  // T9 focus return: the row `[data-message-id]`, else the composer, both in
  // this modal's own chat panel (#1959), never body. This runs in the PARENT's effect, which fires after Modal's own
  // cleanup unmounts it (child effects clean up before the parent's run) — so
  // it overrides Modal's own restore-to-invoker behaviour, which would
  // otherwise land on <body>: the trigger (a context-menu item, or
  // DeleteMessageModal's Delete button) is long detached by the time a 403
  // arrives.
  const lastMessageIdRef = useRef<string | null>(null);
  useEffect(() => {
    if (messageId) {
      lastMessageIdRef.current = messageId;
      return;
    }
    const closedId = lastMessageIdRef.current;
    if (closedId === null) return;
    lastMessageIdRef.current = null;
    const row = findSurfaceMessageRow(surfaceId, closedId);
    const target = row ?? findSurfaceComposer(surfaceId);
    target?.focus();
  }, [messageId, surfaceId]);

  if (!refusal || !view) return null;

  const describedById = `delete-refusal-body-${refusal.messageId}`;

  return (
    <Modal
      isOpen
      onClose={onDismiss}
      title={titleFor(view)}
      width="small"
      dismissable={!submitting}
      initialFocusRef={initialFocusRef}
      describedById={describedById}
    >
      <div className="delete-refusal-modal">
        {isCredentialView(view) ? (
          <CredentialStage
            factor={factor}
            describedById={describedById}
            onConfirm={onConfirm}
            onCancel={onDismiss}
          />
        ) : (
          <>
            {view.view === 'wait' && view.reason === 'verification' && (
              <p className="delete-refusal-modal__body">{VERIFICATION_WAIT_COPY}</p>
            )}
            <p id={describedById} className="delete-refusal-modal__body">
              {closedBodyCopy(view, remaining)}
            </p>
            <div className="delete-refusal-modal__actions">
              <button
                type="button"
                className="delete-refusal-modal__cancel"
                ref={closeButtonRef}
                onClick={onDismiss}
              >
                Close
              </button>
            </div>
          </>
        )}

        {/* Written only at zero (design spec §2.10 / handoff T7): the ticking
            text above sits outside any live region, so a screen reader is not
            interrupted every second. */}
        <div className="sr-only" role="status" aria-live="polite">
          {announceZero ? 'You can try again now.' : ''}
        </div>
      </div>
    </Modal>
  );
};

export default DeleteRefusalModal;
