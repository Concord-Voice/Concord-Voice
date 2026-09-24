import React, { useEffect, useRef, useState } from 'react';
import Modal from '../ui/Modal';
import {
  clearDMHistory,
  hideDMThread,
  type ClearFactor,
} from '../../services/messaging/dmVisibilityApi';
import { useDMStore, type DMConversation } from '../../stores/chat/dmStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
  type AuthLifecycleSnapshot,
} from '../../services/system/postLoginHydrationLifecycle';
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

type ClearStage = 'confirm' | 'password' | 'mfa' | 'uncertain';

const CLEAR_UNCERTAIN_ERROR =
  'Could not confirm whether history was cleared. Check the thread before retrying.';

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

function clearFactor(stage: ClearStage, credential: string): ClearFactor | undefined {
  if (stage === 'password') return { kind: 'password', value: credential };
  if (stage === 'mfa') return { kind: 'mfa', value: credential };
  return undefined;
}

function removalActionLabel(action: DMThreadRemovalTarget['action'], isVerifying: boolean): string {
  if (action === 'clear') return isVerifying ? 'Verify and clear' : 'Continue';
  return REMOVAL_TITLES[action];
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
  const cancelRef = useRef<HTMLButtonElement>(null);
  const factorRef = useRef<HTMLInputElement>(null);
  const requestInFlightRef = useRef(false);
  const [busy, setBusy] = useState(false);
  const [clearStage, setClearStage] = useState<ClearStage>('confirm');
  const [credential, setCredential] = useState('');
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    if (!busy && (clearStage === 'password' || clearStage === 'mfa')) factorRef.current?.focus();
  }, [busy, clearStage, error]);

  if (
    target.conversation.isPersonal ||
    (target.action === 'leave' && !target.conversation.isGroup)
  ) {
    return null;
  }

  const isVerifying = clearStage === 'password' || clearStage === 'mfa';
  const factorLabel = clearStage === 'password' ? 'Password' : 'Authentication code';

  const invalidateAndRefetch = async (lifecycle: AuthLifecycleSnapshot): Promise<boolean> => {
    if (!isSameAuthLifecycle(lifecycle)) return false;
    globalThis.dispatchEvent(
      new CustomEvent('messages-purged', { detail: { scopeId: target.conversation.id } })
    );
    await useDMStore.getState().fetchConversations();
    return isSameAuthLifecycle(lifecycle);
  };

  const setUncertainClear = async (lifecycle: AuthLifecycleSnapshot) => {
    try {
      await invalidateAndRefetch(lifecycle);
    } catch {
      // fetchConversations normally reports errors in the store. A thrown
      // implementation still leaves this operation unresolved and retry-blocked.
    }
    if (!isSameAuthLifecycle(lifecycle)) return;
    setCredential('');
    setClearStage('uncertain');
    setError(CLEAR_UNCERTAIN_ERROR);
  };

  const submitClear = async () => {
    if (requestInFlightRef.current || clearStage === 'uncertain') return;

    requestInFlightRef.current = true;
    setBusy(true);
    setError(null);
    let closed = false;
    const lifecycle = captureAuthLifecycle();
    const factor = clearFactor(clearStage, credential);

    try {
      const result = await clearDMHistory(target.conversation.id, factor);
      if (!isSameAuthLifecycle(lifecycle)) return;
      switch (result.kind) {
        case 'passwordRequired':
        case 'mfaRequired':
          setCredential('');
          setClearStage(result.kind === 'passwordRequired' ? 'password' : 'mfa');
          return;
        case 'invalidPassword':
          setCredential('');
          setError('That password is not correct.');
          return;
        case 'invalidMfaCode':
          setCredential('');
          setError('That code is not correct or has expired.');
          return;
        case 'success':
          if (!(await invalidateAndRefetch(lifecycle))) return;
          closed = true;
          onClose();
          return;
        case 'uncertain':
          await setUncertainClear(lifecycle);
          return;
        case 'rateLimited':
          setCredential('');
          setError(
            result.retryAfterSeconds === undefined
              ? 'Try again later.'
              : `Try again in ${result.retryAfterSeconds} seconds.`
          );
          return;
        case 'sessionExpired':
          setCredential('');
          setError('Sign in again to clear history.');
          return;
        case 'notFound':
          setCredential('');
          setError('This thread is no longer available.');
          return;
        case 'stepUpImpossible':
          setCredential('');
          setError(
            'Set a password, enable MFA, or turn off purge protection in Privacy & Security.'
          );
          return;
        case 'refused':
          setCredential('');
          setError('History could not be cleared.');
          return;
      }
    } catch {
      if (isSameAuthLifecycle(lifecycle)) await setUncertainClear(lifecycle);
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
      useDMStore.getState().discardConversationView(target.conversation.id);
      if (!(await hideDMThread(target.conversation.id))) throw new Error('Hide failed');
      if (!isSameAuthLifecycle(lifecycle)) return false;
      onClose();
      onRemoved();
      return true;
    } catch {
      if (!isSameAuthLifecycle(lifecycle)) return false;
      await restoreHiddenConversation(target.conversation.id, wasActive, lifecycle);
      if (!isSameAuthLifecycle(lifecycle)) return false;
      setError('Could not confirm the hide. Check the thread list before retrying.');
      return false;
    }
  };

  const submitLeave = async (lifecycle: AuthLifecycleSnapshot): Promise<boolean> => {
    try {
      await useDMStore.getState().leaveGroup(target.conversation.id);
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
    if (target.action === 'clear') {
      await submitClear();
      return;
    }
    if (requestInFlightRef.current) return;

    requestInFlightRef.current = true;
    setBusy(true);
    setError(null);
    const lifecycle = captureAuthLifecycle();
    const wasActive = useDMStore.getState().activeConversationId === target.conversation.id;
    let removed: boolean;
    if (target.action === 'hide') {
      removed = await submitHide(lifecycle, wasActive);
    } else {
      removed = await submitLeave(lifecycle);
    }
    if (isSameAuthLifecycle(lifecycle)) {
      requestInFlightRef.current = false;
      if (!removed) setBusy(false);
    }
  };

  const disabled = busy || clearStage === 'uncertain' || (isVerifying && credential.trim() === '');

  return (
    <Modal
      isOpen={true}
      onClose={onClose}
      title={REMOVAL_TITLES[target.action]}
      width="small"
      dismissable={!busy}
      initialFocusRef={cancelRef}
    >
      <div className="delete-server-content">
        <div className="delete-server-warning">
          <div className="confirm-action-message">
            <p>{REMOVAL_COPY[target.action]}</p>
          </div>
        </div>

        {isVerifying && (
          <div className="delete-server-confirm">
            <label className="form-label" htmlFor="dm-thread-removal-factor">
              {factorLabel}
            </label>
            <input
              id="dm-thread-removal-factor"
              ref={factorRef}
              className="form-input"
              type={clearStage === 'password' ? 'password' : 'text'}
              autoComplete={clearStage === 'password' ? 'current-password' : 'one-time-code'}
              inputMode={clearStage === 'mfa' ? 'numeric' : undefined}
              value={credential}
              aria-invalid={error !== null}
              onChange={(event) => {
                setCredential(event.target.value);
                setError(null);
              }}
              disabled={busy}
            />
          </div>
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
            disabled={busy}
          >
            Cancel
          </button>
          <button
            type="button"
            className="delete-server-confirm-btn"
            onClick={() => void submitRemoval()}
            disabled={disabled}
          >
            {removalActionLabel(target.action, isVerifying)}
          </button>
        </div>
      </div>
    </Modal>
  );
};

export default DMThreadRemovalDialog;
