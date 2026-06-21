import React, { useState, useEffect } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { apiFetch } from '../../services/apiClient';
import { ChannelGroup } from '../../types/chat';
import './CreateChannelModal.css';

interface EditCategoryModalProps {
  isOpen: boolean;
  group: ChannelGroup;
  onClose: () => void;
}

const NAME_MIN = 1;
const NAME_MAX = 100;

const EditCategoryModal: React.FC<EditCategoryModalProps> = ({ isOpen, group, onClose }) => {
  const [name, setName] = useState(group.name);
  const [error, setError] = useState<string | null>(null);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);

  const activeServerId = useServerStore((state) => state.activeServerId);
  const updateChannelGroup = useChannelStore((state) => state.updateChannelGroup);

  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets name from group prop when group changes; not a render loop
    setName(group.name);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears error when group changes; not a render loop
    setError(null);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: clears success message when group changes; not a render loop
    setSuccessMessage(null);
  }, [group]);

  const handleClose = () => {
    if (!isSubmitting) {
      setError(null);
      setSuccessMessage(null);
      onClose();
    }
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    const trimmed = name.trim();
    if (!trimmed) {
      setError('Category name is required');
      return;
    }
    if (trimmed.length < NAME_MIN) {
      setError(`Category name must be at least ${NAME_MIN} character`);
      return;
    }
    if (trimmed.length > NAME_MAX) {
      setError(`Category name must be at most ${NAME_MAX} characters`);
      return;
    }

    if (trimmed === group.name) {
      handleClose();
      return;
    }

    if (!activeServerId) return;

    setIsSubmitting(true);
    setError(null);

    try {
      const response = await apiFetch(
        `/api/v1/servers/${activeServerId}/channel-groups/${group.id}`,
        {
          method: 'PATCH',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ name: trimmed }),
        }
      );

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Failed to update category');
      }

      updateChannelGroup(group.id, { name: trimmed });
      setSuccessMessage('Category updated!');

      setTimeout(() => {
        handleClose();
      }, 800);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to update category');
    } finally {
      setIsSubmitting(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Edit Category">
      <form onSubmit={handleSubmit} className="create-channel-form">
        {error && <div className="channel-form-error-banner">{error}</div>}
        {successMessage && <div className="channel-form-success-banner">{successMessage}</div>}

        <div className="channel-form-group">
          <label htmlFor="edit-category-name" className="channel-form-label">
            Category Name <span className="required">*</span>
          </label>
          <input
            id="edit-category-name"
            type="text"
            className={`channel-form-input ${error ? 'error' : ''}`}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="General"
            maxLength={NAME_MAX}
            disabled={isSubmitting}
            autoFocus
          />
          <span className="channel-form-hint">
            {name.length}/{NAME_MAX} characters
          </span>
        </div>

        <div className="form-actions">
          <button
            type="button"
            className="btn btn-secondary"
            onClick={handleClose}
            disabled={isSubmitting}
          >
            Cancel
          </button>
          <button
            type="submit"
            className="btn btn-primary"
            disabled={isSubmitting || !activeServerId}
          >
            {isSubmitting ? (
              <>
                <LoadingSpinner size="small" />
                <span>Saving...</span>
              </>
            ) : (
              'Save Changes'
            )}
          </button>
        </div>
      </form>
    </Modal>
  );
};

export default EditCategoryModal;
