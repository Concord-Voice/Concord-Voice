import { useState, useEffect, useCallback, useRef } from 'react';
import { apiFetch } from '../../services/system/apiClient';
import { useDMStore } from '../../stores/chat/dmStore';
import Modal from '../ui/Modal';
import { useExpirationPolicy } from '../../hooks/messaging/useExpirationPolicy';
import MessageExpirationEditor from '../Expiration/MessageExpirationEditor';
import { useAuthStore } from '../../stores/auth/authStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import './DirectMessages.css';

interface EditGroupModalProps {
  isOpen: boolean;
  onClose: () => void;
  conversationId: string;
  currentName: string | null;
}

const EditGroupModal: React.FC<EditGroupModalProps> = ({
  isOpen,
  onClose,
  conversationId,
  currentName,
}) => {
  const [name, setName] = useState(currentName || '');
  const [isSaving, setIsSaving] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const expiration = useExpirationPolicy(isOpen ? { kind: 'dm', id: conversationId } : null);
  const operationRef = useRef(0);
  const mountedRef = useRef(true);
  const nameInputRef = useRef<HTMLInputElement>(null);
  const viewRef = useRef({ conversationId, authGeneration, isOpen });
  viewRef.current = { conversationId, authGeneration, isOpen };

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      operationRef.current += 1;
    };
  }, []);

  // Reset name when modal opens
  useEffect(() => {
    if (isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets name from currentName prop when modal opens; not a render loop
      setName(currentName || '');
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when modal opens; not a render loop
      setError(null);
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets isSaving when modal opens; not a render loop
      setIsSaving(false);
    }
    operationRef.current += 1;
  }, [authGeneration, conversationId, currentName, isOpen]);

  const handleClose = useCallback(() => {
    if (isSaving) return;
    operationRef.current += 1;
    onClose();
  }, [isSaving, onClose]);

  const closeExpirationEditor = useCallback(() => {
    operationRef.current += 1;
    onClose();
  }, [onClose]);

  const handleSave = useCallback(async () => {
    if (isSaving) return;
    setIsSaving(true);
    setError(null);
    const operation = ++operationRef.current;
    const lifecycle = captureAuthLifecycle();
    const capturedConversationId = conversationId;
    const stillCurrent = () =>
      mountedRef.current &&
      operation === operationRef.current &&
      viewRef.current.conversationId === capturedConversationId &&
      viewRef.current.authGeneration === authGeneration &&
      viewRef.current.isOpen &&
      isSameAuthLifecycle(lifecycle);

    try {
      const response = await apiFetch(`/api/v1/dm/conversations/${conversationId}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ name: name.trim() || null }),
      });

      if (!response.ok) {
        const data = await response.json();
        throw new Error(data.error || 'Failed to update group');
      }

      if (!stillCurrent()) return;
      useDMStore.getState().updateConversation(conversationId, {
        name: name.trim() || null,
      });

      operationRef.current += 1;
      onClose();
    } catch (err) {
      if (stillCurrent()) setError(err instanceof Error ? err.message : 'Failed to update group');
    } finally {
      if (stillCurrent()) setIsSaving(false);
    }
  }, [authGeneration, conversationId, isSaving, name, onClose]);

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Edit Group" initialFocusRef={nameInputRef}>
      <form
        className="edit-group-modal"
        onSubmit={(event) => {
          event.preventDefault();
          void handleSave();
        }}
      >
        <div className="edit-group-modal-body">
          <label className="edit-group-label" htmlFor="edit-group-name">
            Group Name
          </label>
          <input
            id="edit-group-name"
            type="text"
            className="create-group-name-input"
            placeholder="Group Name (optional)"
            value={name}
            onChange={(e) => setName(e.target.value)}
            maxLength={100}
            ref={nameInputRef}
          />

          {error && <div className="create-group-error">{error}</div>}
        </div>

        <div className="edit-group-modal-footer">
          <button type="button" className="edit-group-cancel-btn" onClick={handleClose}>
            Cancel
          </button>
          <button type="submit" className="create-group-create-btn" disabled={isSaving}>
            {isSaving ? 'Saving...' : 'Save'}
          </button>
        </div>
      </form>
      <>
        <h3 className="edit-group-label">Message expiration</h3>
        <MessageExpirationEditor
          scope={{ kind: 'dm', id: conversationId }}
          policy={expiration.policy}
          policyState={expiration.policyState}
          canEdit={expiration.canEdit && !isSaving}
          lockedDescription={expiration.lockedDescription}
          onRefresh={expiration.onRefresh}
          onApplyPolicy={expiration.onApplyPolicy}
          onMarkSeen={expiration.onMarkSeen}
          onClose={closeExpirationEditor}
        />
      </>
    </Modal>
  );
};

export default EditGroupModal;
