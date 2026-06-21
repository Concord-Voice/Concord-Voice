import React, { useState } from 'react';
import Modal from '../ui/Modal';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { apiFetch } from '../../services/apiClient';
import './CreateChannelModal.css';

interface CreateCategoryModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess?: () => void;
}

const NAME_MIN = 1;
const NAME_MAX = 100;

const CreateCategoryModal: React.FC<CreateCategoryModalProps> = ({
  isOpen,
  onClose,
  onSuccess,
}) => {
  const [name, setName] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [isSubmitting, setIsSubmitting] = useState(false);
  const [successMessage, setSuccessMessage] = useState<string | null>(null);

  const activeServerId = useServerStore((state) => state.activeServerId);
  const addChannelGroup = useChannelStore((state) => state.addChannelGroup);
  const channelGroups = useChannelStore((state) => state.channelGroups);

  const resetForm = () => {
    setName('');
    setError(null);
    setIsSubmitting(false);
    setSuccessMessage(null);
  };

  const handleClose = () => {
    if (!isSubmitting) {
      resetForm();
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

    if (!activeServerId) return;

    setIsSubmitting(true);
    setError(null);

    try {
      const response = await apiFetch(`/api/v1/servers/${activeServerId}/channel-groups`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: trimmed,
          position: channelGroups.length,
        }),
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Failed to create category');
      }

      addChannelGroup(data.channel_group || data.group || data);
      setSuccessMessage('Category created!');

      if (onSuccess) {
        onSuccess();
      }

      setTimeout(() => {
        handleClose();
      }, 800);
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create category');
    } finally {
      setIsSubmitting(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Create Category">
      <form onSubmit={handleSubmit} className="create-channel-form">
        {error && <div className="channel-form-error-banner">{error}</div>}
        {successMessage && <div className="channel-form-success-banner">{successMessage}</div>}

        <div className="channel-form-group">
          <label htmlFor="category-name" className="channel-form-label">
            Category Name <span className="required">*</span>
          </label>
          <input
            id="category-name"
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
                <span>Creating...</span>
              </>
            ) : (
              'Create Category'
            )}
          </button>
        </div>
      </form>
    </Modal>
  );
};

export default CreateCategoryModal;
