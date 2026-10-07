import React, { useEffect, useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import MFAVerifyPrompt from '../Auth/MFAVerifyPrompt';
import type { StepUpPurpose } from '../Auth/stepUpPurpose';
import type { DeleteRefusalState, DeleteStepUp } from '../../hooks/messaging/useChatController';
import type { DeleteRefusalView } from '../../services/messaging/deleteRefusal';
import { findSurfaceComposer, findSurfaceMessageRow } from './chatSurface';
import './DeleteRefusalModal.css';

export interface DeleteRefusalModalProps {
  /** The hook's one refusal slot. `null` renders nothing. */
  refusal: DeleteRefusalState | null;
  /** Re-sends the same delete with a factor. */
  onConfirm: (step: DeleteStepUp) => void;
  onDismiss: () => void;
  /** `dm.message_delete` in a DM, `messages.delete` everywhere else. */
  purpose: StepUpPurpose;
  /** The owning chat panel's id (#1959): focus returns to its row or its composer, never to
   *  another panel's, where the next message would go to a different conversation. */
  surfaceId: string;
}

const TITLES: Record<DeleteRefusalState['view']['view'], string> = {
  confirm: "Confirm it's you",
  password: "Confirm it's you",
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

function SubmitActions({
  submitting,
  canSubmit,
  onCancel,
}: Readonly<{ submitting: boolean; canSubmit: boolean; onCancel: () => void }>) {
  return (
    <div className="delete-refusal-modal__actions">
      <button
        type="button"
        className="delete-refusal-modal__cancel"
        onClick={onCancel}
        disabled={submitting}
      >
        Cancel
      </button>
      <button
        type="submit"
        className="delete-refusal-modal__confirm"
        disabled={!canSubmit || submitting}
      >
        {submitting ? 'Confirming…' : 'Confirm'}
      </button>
    </div>
  );
}

const DeleteRefusalModal: React.FC<DeleteRefusalModalProps> = ({
  refusal,
  onConfirm,
  onDismiss,
  purpose,
  surfaceId,
}) => {
  const view = refusal?.view;
  const submitting = refusal?.submitting ?? false;
  const messageId = refusal?.messageId ?? null;

  // Wire secrets. Component-local state only — never a store, never logged
  // ([internal]rules/observability.md).
  const [password, setPassword] = useState('');
  const [mfaCode, setMfaCode] = useState('');
  // A typed factor never outlives its attempt: a new delete target, or a
  // refusal that remounts the prompt (`promptKey`, #3466 — a code is spent by a
  // try; a refused password is cleared), starts from empty. Reset during render
  // rather than in an effect, as MFAVerifyPrompt does for its own refusal text.
  const attemptKey = `${messageId ?? ''}:${refusal?.promptKey ?? 0}`;
  const [seenAttemptKey, setSeenAttemptKey] = useState(attemptKey);
  if (attemptKey !== seenAttemptKey) {
    setSeenAttemptKey(attemptKey);
    setPassword('');
    setMfaCode('');
  }

  const countdownRetryAfter =
    view?.view === 'wait' || view?.view === 'failed' ? view.retryAfterSeconds : undefined;
  const remaining = useCountdown(refusal?.openedAt, countdownRetryAfter);
  const announceZero = remaining === 0;

  const formRef = useRef<HTMLFormElement>(null);
  const closeButtonRef = useRef<HTMLButtonElement>(null);
  const initialFocusRef = useRef<HTMLElement | null>(null);

  // Runs before Modal's own mount-focus effect (useLayoutEffect vs
  // useEffect), so Modal's containerRef.focus() never overrides the first
  // field's autofocus. Modal only focuses on OPEN, so a later change of view
  // (confirm -> wait, which unmounts the focused field) or a refused password
  // (the field was disabled while submitting) re-targets and re-focuses here.
  const focusToken = view ? `${view.view}:${refusal?.promptKey ?? 0}` : null;
  const prevFocusTokenRef = useRef<string | null>(null);
  useLayoutEffect(() => {
    if (!view || focusToken === null) {
      prevFocusTokenRef.current = null;
      return;
    }
    initialFocusRef.current =
      view.view === 'confirm' || view.view === 'password'
        ? (formRef.current?.querySelector<HTMLElement>('input, button') ?? null)
        : closeButtonRef.current;
    if (prevFocusTokenRef.current !== null && prevFocusTokenRef.current !== focusToken) {
      initialFocusRef.current?.focus();
    }
    prevFocusTokenRef.current = focusToken;
  }, [view, focusToken]);

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

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    if (view.view === 'confirm') {
      if (!mfaCode) return;
      onConfirm({ mfaCode });
    } else if (view.view === 'password') {
      if (!password) return;
      onConfirm({ currentPassword: password });
      // Sent once, to the mint; nothing keeps it after that, whatever view
      // the attempt ends on (#3509 frontend review).
      setPassword('');
    }
  };

  const canSubmit =
    view.view === 'confirm' ? mfaCode !== '' : view.view === 'password' && password !== '';

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
        {view.view === 'confirm' && (
          <form ref={formRef} onSubmit={handleSubmit}>
            <p id={describedById} className="delete-refusal-modal__body">
              {CHALLENGE_COPY}
            </p>
            <MFAVerifyPrompt
              key={refusal.promptKey}
              methods={view.methods}
              purpose={purpose}
              onVerify={setMfaCode}
              onCodeChange={setMfaCode}
              disabled={submitting}
              error={view.error}
            />
            <SubmitActions submitting={submitting} canSubmit={canSubmit} onCancel={onDismiss} />
          </form>
        )}

        {view.view === 'password' && (
          <form ref={formRef} onSubmit={handleSubmit}>
            <p id={describedById} className="delete-refusal-modal__body">
              {CHALLENGE_COPY}
            </p>
            <div className="delete-refusal-modal__field">
              <label htmlFor="delete-refusal-password">Password</label>
              <input
                id="delete-refusal-password"
                type="password"
                autoComplete="current-password"
                value={password}
                onChange={(e) => setPassword(e.target.value)}
                disabled={submitting}
                aria-invalid={view.error !== undefined || undefined}
                aria-describedby={
                  view.error === undefined ? undefined : 'delete-refusal-password-error'
                }
              />
              {view.error !== undefined && (
                <p
                  className="delete-refusal-modal__field-error"
                  id="delete-refusal-password-error"
                  role="alert"
                >
                  {view.error}
                </p>
              )}
            </div>
            <SubmitActions submitting={submitting} canSubmit={canSubmit} onCancel={onDismiss} />
          </form>
        )}

        {(view.view === 'wait' || view.view === 'unavailable' || view.view === 'failed') && (
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
