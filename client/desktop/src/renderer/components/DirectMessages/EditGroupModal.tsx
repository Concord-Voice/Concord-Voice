import { useState, useEffect, useCallback } from 'react';
import { X } from 'lucide-react';
import { apiFetch } from '../../services/apiClient';
import { useDMStore } from '../../stores/dmStore';
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
  }, [isOpen, currentName]);

  const handleSave = useCallback(async () => {
    if (isSaving) return;
    setIsSaving(true);
    setError(null);

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

      // Update local state
      useDMStore.getState().updateConversation(conversationId, {
        name: name.trim() || null,
      });

      onClose();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update group');
    } finally {
      setIsSaving(false);
    }
  }, [conversationId, name, isSaving, onClose]);

  const handleKeyDown = useCallback(
    (e: React.KeyboardEvent) => {
      if (e.key === 'Enter' && !e.shiftKey) {
        e.preventDefault();
        handleSave();
      }
    },
    [handleSave]
  );

  if (!isOpen) return null;

  return (
    <dialog className="edit-group-modal-overlay" open aria-label="Edit Group">
      <div className="edit-group-modal">
        <div className="edit-group-modal-header">
          <h3>Edit Group</h3>
          <button type="button" className="create-group-close-btn" onClick={onClose}>
            <X size={18} />
          </button>
        </div>

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
            onKeyDown={handleKeyDown}
            maxLength={100}
            autoFocus
          />

          {error && <div className="create-group-error">{error}</div>}
        </div>

        <div className="edit-group-modal-footer">
          <button type="button" className="edit-group-cancel-btn" onClick={onClose}>
            Cancel
          </button>
          <button
            type="button"
            className="create-group-create-btn"
            onClick={handleSave}
            disabled={isSaving}
          >
            {isSaving ? 'Saving...' : 'Save'}
          </button>
        </div>
      </div>
    </dialog>
  );
};

export default EditGroupModal;
