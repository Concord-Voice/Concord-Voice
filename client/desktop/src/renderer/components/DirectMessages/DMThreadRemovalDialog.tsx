import React, { useLayoutEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import StepUpCredentials, { stepUpActivation } from '../Auth/StepUpCredentials';
import {
  LEG_ONLY_WITHOUT_MFA,
  useStepUpFactor,
  type StepUpPhase,
  type StepUpSubmit,
  type StepUpSubmitOutcome,
} from '../../hooks/auth/useStepUpFactor';
import {
  clearDMHistory,
  hideDMThread,
  type ClearFactor,
  type ClearHistoryResult,
} from '../../services/messaging/dmVisibilityApi';
import { useDMStore, type DMConversation } from '../../stores/chat/dmStore';
import { usePrivacyStore } from '../../stores/ui/privacyStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
  type AuthLifecycleSnapshot,
} from '../../services/system/postLoginHydrationLifecycle';
import {
  apiRequestContextIsCurrent,
  captureApiRequestContext,
  type ApiRequestContext,
} from '../../services/system/requestContext';
import '../ui/ConfirmActionModal.css';

export type DMThreadRemovalTarget = {
  conversation: DMConversation;
  action: 'hide' | 'clear' | 'leave';
};

interface DMThreadRemovalDialogProps {
  target: DMThreadRemovalTarget;
  onClose: () => void;
  onRemoved: () => void;
}

type ClearStage = 'confirm' | 'credentials' | 'uncertain';

const CLEAR_UNCERTAIN_ERROR =
  'Could not confirm whether history was cleared. Check the thread before retrying.';

/**
 * Names the credential stage. The dialog title stays constant (ui/Modal binds
 * it to aria-labelledby), so this heading is what announces the stage change
 * and takes focus.
 */
const CREDENTIALS_HEADING = 'Confirm it is you';
/**
 * The words for a session that is gone: the read's `session` refusal, an
 * account or server change during the activation, and Clear's own 401.
 */
const SESSION_MESSAGE = 'Sign in again to clear history.';

const REMOVAL_TITLES: Record<DMThreadRemovalTarget['action'], string> = {
  hide: 'Hide thread',
  clear: 'Clear history for me',
  leave: 'Leave group',
};

const REMOVAL_COPY: Record<DMThreadRemovalTarget['action'], string> = {
  hide: "Hide this thread? You'll still receive new messages. The thread will reappear when someone sends a message.",
  clear:
    "Clear history for me? This permanently removes this thread's message history from your view. Other participants will not be notified and keep their history.",
  leave: "Leave this group? You will lose access to this group's messages and encryption keys.",
};

/**
 * The factor Clear is sent. The hook hands over a code or a security-key token
 * when a method is offered; with none offered, the password leg is showing.
 */
function clearFactor(mfa: string | undefined, password: string): ClearFactor {
  return mfa === undefined ? { kind: 'password', value: password } : { kind: 'mfa', value: mfa };
}

/**
 * Clear's answer as the factor hook reads it. A refusal of what was entered, or
 * of the budget, is a `refusal` (the hook re-offers the credentials); any other
 * reply means the server answered and may have read the code.
 */
function submitOutcome(result: ClearHistoryResult): StepUpSubmitOutcome {
  switch (result.kind) {
    case 'success':
      return { kind: 'success' };
    case 'uncertain':
      return { kind: 'transport' };
    case 'aborted':
      return { kind: 'aborted' };
    case 'passwordRequired':
      // #3509: a refused token keeps its expiry, so the password field says so.
      return {
        kind: 'refusal',
        refusal: result.tokenExpired
          ? { kind: 'passwordRequired', tokenExpired: true }
          : { kind: 'passwordRequired' },
      };
    case 'invalidPassword':
    case 'invalidMfaCode':
    case 'sessionExpired':
      return { kind: 'refusal', refusal: { kind: result.kind } };
    case 'mfaRequired':
      return { kind: 'refusal', refusal: { kind: 'mfaRequired', methods: result.methods } };
    case 'rateLimited':
      return { kind: 'refusal', refusal: { kind: 'rateLimited' } };
    case 'passwordRefused':
    case 'notFound':
    case 'stepUpImpossible':
    case 'refused':
      return { kind: 'answered' };
  }
}

/**
 * The banner for an answer that no credential field owns, or null. A refusal
 * of the password, the code or the security key is worded by the credential
 * stage itself, so it has no text here.
 */
function clearErrorText(result: ClearHistoryResult): string | null {
  switch (result.kind) {
    case 'rateLimited':
      return result.retryAfterSeconds === undefined
        ? 'Try again later.'
        : `Try again in ${result.retryAfterSeconds} seconds.`;
    case 'passwordRefused':
      // #3509: the mint refused the password.
      return result.message;
    case 'sessionExpired':
      return SESSION_MESSAGE;
    case 'notFound':
      return 'This thread is no longer available.';
    case 'stepUpImpossible':
      return 'Set a password, enable MFA, or turn off purge protection in Privacy & Security.';
    case 'refused':
      return 'History could not be cleared.';
    default:
      return null;
  }
}

/** The primary's label: the action, or for Clear the stage and activation it is in. */
function primaryLabel(
  action: DMThreadRemovalTarget['action'],
  stage: ClearStage,
  phase: StepUpPhase
): string {
  if (action !== 'clear') return REMOVAL_TITLES[action];
  if (stage !== 'credentials') return 'Continue';
  if (phase === 'ceremony') return 'Waiting…';
  return phase === 'submitting' ? 'Clearing…' : 'Verify and clear';
}

/**
 * True while Clear's answer still belongs to the account and server that sent
 * it (D20). The lifecycle covers the account; the activation's context covers
 * the runtime server too, which a switch changes without touching the account.
 */
function answerIsCurrent(lifecycle: AuthLifecycleSnapshot, context: ApiRequestContext): boolean {
  return isSameAuthLifecycle(lifecycle) && apiRequestContextIsCurrent(context);
}

async function restoreHiddenConversation(
  id: string,
  wasActive: boolean,
  lifecycle: AuthLifecycleSnapshot
): Promise<void> {
  if (!isSameAuthLifecycle(lifecycle)) return;
  try {
    await useDMStore.getState().fetchConversations();
    if (!isSameAuthLifecycle(lifecycle)) return;
    if (
      wasActive &&
      useDMStore.getState().conversations.some((conversation) => conversation.id === id)
    ) {
      useDMStore.getState().setActiveConversation(id);
    }
  } catch {
    // The store normally handles refresh errors; preserve the unresolved outcome either way.
  }
}

const DMThreadRemovalDialog: React.FC<DMThreadRemovalDialogProps> = ({
  target,
  onClose,
  onRemoved,
}) => {
  const { conversation, action } = target;
  const cancelRef = useRef<HTMLButtonElement>(null);
  const headingRef = useRef<HTMLHeadingElement>(null);
  const primaryRef = useRef<HTMLButtonElement>(null);
  const requestInFlightRef = useRef(false);
  const [busy, setBusy] = useState(false);
  const [clearStage, setClearStage] = useState<ClearStage>('confirm');
  // Wire secret. Component-local state only: never a store, never a log.
  const [password, setPassword] = useState('');
  const [error, setError] = useState<string | null>(null);
  const requireAuthBeforePurge = usePrivacyStore((s) => s.settings.requireAuthBeforePurge);

  const removable = !(conversation.isPersonal || (action === 'leave' && !conversation.isGroup));
  // Fail closed on unknown: internal/dm/visibility.go reads the same setting
  // that way, and a server too old to expose the field omits it entirely. With
  // the setting off, Clear stays one click and sends no factor.
  const stepUpRequired = action === 'clear' && requireAuthBeforePurge !== false;

  // passwordLeg set to whenNoMfa: the server asks for the password only when the
  // account has no inline method (#3509), so the field shows for that account
  // alone. `readFailure: 'block'`: a Clear sent with the wrong factor spends a
  // purge unit that is never refunded, so a failed read blocks with Retry
  // rather than guessing. The read starts when the dialog opens, so the stage
  // is ready on arrival; `clearStage` also enables it for a Clear the server
  // refused although the local setting said it would not (a stale setting).
  const factor = useStepUpFactor({
    enabled: removable && (stepUpRequired || clearStage === 'credentials'),
    purpose: 'dm.clear',
    passwordLeg: LEG_ONLY_WITHOUT_MFA,
    readFailure: 'block',
    allowBackup: true,
  });
  const stepUpSubmitting = factor.phase === 'submitting';
  const cancelEnabled = !busy && !stepUpSubmitting;

  // A stage change moves focus to the stage heading, never to the primary.
  useLayoutEffect(() => {
    if (clearStage === 'credentials') headingRef.current?.focus();
  }, [clearStage]);

  // An unresolved Clear leaves nothing but Cancel enabled, and the request that
  // ended in it may still be settling when the stage changes, so wait for it.
  useLayoutEffect(() => {
    if (clearStage === 'uncertain' && cancelEnabled) cancelRef.current?.focus();
  }, [clearStage, cancelEnabled]);

  if (!removable) return null;

  const inCredentials = clearStage === 'credentials';

  const invalidateAndRefetch = async (
    lifecycle: AuthLifecycleSnapshot,
    context: ApiRequestContext
  ): Promise<boolean> => {
    if (!answerIsCurrent(lifecycle, context)) return false;
    globalThis.dispatchEvent(
      new CustomEvent('messages-purged', { detail: { scopeId: conversation.id } })
    );
    await useDMStore.getState().fetchConversations();
    return answerIsCurrent(lifecycle, context);
  };

  const setUncertainClear = async (
    lifecycle: AuthLifecycleSnapshot,
    context: ApiRequestContext
  ) => {
    try {
      await invalidateAndRefetch(lifecycle, context);
    } catch {
      // fetchConversations normally reports errors in the store. A thrown
      // implementation still leaves this operation unresolved and retry-blocked.
    }
    if (!answerIsCurrent(lifecycle, context)) return;
    setPassword('');
    setClearStage('uncertain');
    setError(CLEAR_UNCERTAIN_ERROR);
  };

  /** Acts on Clear's answer for the current account. Resolves true once the dialog is closed. */
  const applyClearResult = async (
    result: ClearHistoryResult,
    lifecycle: AuthLifecycleSnapshot,
    context: ApiRequestContext
  ): Promise<boolean> => {
    if (result.kind === 'success') {
      if (!(await invalidateAndRefetch(lifecycle, context))) return false;
      onClose();
      return true;
    }
    if (result.kind === 'uncertain') {
      await setUncertainClear(lifecycle, context);
      return false;
    }
    const text = clearErrorText(result);
    if (text !== null) setError(text);
    return false;
  };

  /** The credential stage's request, sent once by the hook after it proves the factor. */
  const submitWithFactor: StepUpSubmit = async (mfa, context) => {
    setError(null);
    const lifecycle = captureAuthLifecycle();
    try {
      const result = await clearDMHistory(conversation.id, clearFactor(mfa, password), context);
      // Nothing left for `aborted`, so what was typed is still what the user means to send.
      if (result.kind !== 'aborted') setPassword('');
      // The stage's terminal state words a dead session; a banner would say it twice.
      if (result.kind !== 'sessionExpired' && answerIsCurrent(lifecycle, context)) {
        await applyClearResult(result, lifecycle, context);
      }
      return submitOutcome(result);
    } catch {
      if (answerIsCurrent(lifecycle, context)) await setUncertainClear(lifecycle, context);
      return { kind: 'transport' };
    }
  };

  /** Clear with the setting off: no factor, one click. */
  const submitWithoutFactor = async () => {
    if (requestInFlightRef.current) return;

    requestInFlightRef.current = true;
    setBusy(true);
    setError(null);
    let closed = false;
    const lifecycle = captureAuthLifecycle();
    // The request carries no factor; the capture still fences it, so a switch
    // before it leaves sends nothing (D20).
    const context = captureApiRequestContext();

    try {
      const result = await clearDMHistory(conversation.id, undefined, context);
      // An answer from the old server opens nothing and says nothing here (D20).
      if (!answerIsCurrent(lifecycle, context)) return;
      if (result.kind === 'passwordRequired' || result.kind === 'mfaRequired') {
        // The server asks although the local setting did not.
        setClearStage('credentials');
        return;
      }
      closed = await applyClearResult(result, lifecycle, context);
    } catch {
      if (answerIsCurrent(lifecycle, context)) await setUncertainClear(lifecycle, context);
    } finally {
      if (isSameAuthLifecycle(lifecycle)) {
        requestInFlightRef.current = false;
        if (!closed) setBusy(false);
      }
    }
  };

  const submitHide = async (
    lifecycle: AuthLifecycleSnapshot,
    wasActive: boolean
  ): Promise<boolean> => {
    try {
      useDMStore.getState().discardConversationView(conversation.id);
      if (!(await hideDMThread(conversation.id))) throw new Error('Hide failed');
      if (!isSameAuthLifecycle(lifecycle)) return false;
      onClose();
      onRemoved();
      return true;
    } catch {
      if (!isSameAuthLifecycle(lifecycle)) return false;
      await restoreHiddenConversation(conversation.id, wasActive, lifecycle);
      if (!isSameAuthLifecycle(lifecycle)) return false;
      setError('Could not confirm the hide. Check the thread list before retrying.');
      return false;
    }
  };

  const submitLeave = async (lifecycle: AuthLifecycleSnapshot): Promise<boolean> => {
    try {
      await useDMStore.getState().leaveGroup(conversation.id);
      if (!isSameAuthLifecycle(lifecycle)) return false;
      onClose();
      onRemoved();
      return true;
    } catch {
      if (!isSameAuthLifecycle(lifecycle)) return false;
      setError('Could not leave this group.');
      return false;
    }
  };

  const submitRemoval = async () => {
    if (action === 'clear') {
      // With the setting on, Continue is a local stage change: the hook has
      // already read what the account will be asked for, so no factor-less
      // Clear is sent to find out.
      if (stepUpRequired) setClearStage('credentials');
      else await submitWithoutFactor();
      return;
    }
    if (requestInFlightRef.current) return;

    requestInFlightRef.current = true;
    setBusy(true);
    setError(null);
    const lifecycle = captureAuthLifecycle();
    const wasActive = useDMStore.getState().activeConversationId === conversation.id;
    let removed: boolean;
    if (action === 'hide') {
      removed = await submitHide(lifecycle, wasActive);
    } else {
      removed = await submitLeave(lifecycle);
    }
    if (isSameAuthLifecycle(lifecycle)) {
      requestInFlightRef.current = false;
      if (!removed) setBusy(false);
    }
  };

  // Hide is reversible (the thread respawns on the next message), so it must not
  // borrow the destructive styling Clear and Leave share with Delete Server.
  const tone = action === 'hide' ? ' dm-removal-neutral' : '';
  // The credential stage's primary is never natively disabled: it is
  // `aria-disabled` while something is missing or a request is in flight, and
  // its guarded click does nothing then, so focus stays on it (frontend.md).
  const activation = inCredentials ? stepUpActivation(factor, password, submitWithFactor) : null;
  const primaryDisabled = activation === null && (busy || clearStage === 'uncertain');

  return (
    <Modal
      isOpen={true}
      onClose={onClose}
      title={REMOVAL_TITLES[action]}
      width="small"
      dismissable={cancelEnabled}
      initialFocusRef={cancelRef}
    >
      <div className="delete-server-content">
        {inCredentials && (
          <h3 className="dm-removal-stage-heading" tabIndex={-1} ref={headingRef}>
            {CREDENTIALS_HEADING}
          </h3>
        )}

        <div className={`delete-server-warning${tone}`}>
          <div className="confirm-action-message">
            <p>{REMOVAL_COPY[action]}</p>
          </div>
        </div>

        {inCredentials && (
          <StepUpCredentials
            factor={factor}
            password={password}
            onPasswordChange={(value) => {
              setPassword(value);
              setError(null);
            }}
            primaryRef={primaryRef}
            headingRef={headingRef}
            sessionMessage={SESSION_MESSAGE}
          />
        )}

        {error && (
          <div className="form-error-banner" role="alert">
            <span>{error}</span>
          </div>
        )}

        <div className="delete-server-actions">
          <button
            ref={cancelRef}
            type="button"
            className="delete-server-cancel-btn"
            onClick={onClose}
            disabled={!cancelEnabled}
          >
            Cancel
          </button>
          <button
            ref={primaryRef}
            type="button"
            className={`delete-server-confirm-btn${tone}`}
            onClick={activation ? activation.activate : () => void submitRemoval()}
            aria-disabled={activation?.ariaDisabled || undefined}
            disabled={primaryDisabled}
          >
            {activation !== null && factor.phase !== 'idle' && (
              <>
                <LoadingSpinner size="small" inline />{' '}
              </>
            )}
            {primaryLabel(action, clearStage, factor.phase)}
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default DMThreadRemovalDialog;
