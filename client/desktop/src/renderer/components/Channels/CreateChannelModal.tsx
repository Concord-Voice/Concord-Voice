import React, { useState } from 'react';
import { useFormState } from '../../hooks/useFormState';
import { NAME_MAX, validateChannelName, getChannelTypeIcon } from '../../utils/channelHelpers';
import ChannelEmojiField from './ChannelEmojiField';
import Modal from '../ui/Modal';
import CustomSelect from '../ui/CustomSelect';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { useMemberStore } from '../../stores/memberStore';
import { apiFetch } from '../../services/apiClient';
import { e2eeService } from '../../services/e2eeService';
import { Channel } from '../../types/chat';
import './CreateChannelModal.css';

interface CreateChannelModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSuccess?: (channel: Channel) => void;
}

interface FormErrors {
  name?: string;
  type?: string;
  general?: string;
}

interface ServerMemberLike {
  user_id: string;
}

/** Fetch public keys for all server members and generate wrapped E2EE channel keys. */
async function generateWrappedKeysForMembers(
  members: ServerMemberLike[]
): Promise<Record<string, string>> {
  if (!e2eeService.isInitialized) {
    throw new Error('Setting up secure messaging — try again in a moment.');
  }

  const memberPublicKeys = new Map<string, string>();
  for (const member of members) {
    try {
      const pkRes = await apiFetch(`/api/v1/users/${member.user_id}/public-key`);
      if (pkRes.ok) {
        const pkData = await pkRes.json();
        memberPublicKeys.set(member.user_id, pkData.public_key);
      }
    } catch {
      // Skip members without public keys — they won't have E2EE access until key distribution
    }
  }

  if (memberPublicKeys.size === 0) {
    throw new Error('No member public keys available for E2EE channel creation');
  }

  const wrappedKeyMap = await e2eeService.createChannelKeys(memberPublicKeys);
  return Object.fromEntries(wrappedKeyMap);
}

const CreateChannelModal: React.FC<CreateChannelModalProps> = ({ isOpen, onClose, onSuccess }) => {
  const [name, setName] = useState('');
  const [emoji, setEmoji] = useState('');
  const [type, setType] = useState<'text' | 'voice' | 'bulletin'>('text');
  const [groupId, setGroupId] = useState<string>('');
  const {
    errors,
    setErrors,
    isSubmitting,
    setIsSubmitting,
    successMessage,
    setSuccessMessage,
    reset: resetFormState,
  } = useFormState<FormErrors>();
  const activeServerId = useServerStore((state) => state.activeServerId);
  const addChannel = useChannelStore((state) => state.addChannel);
  const channelGroups = useChannelStore((state) => state.channelGroups);
  const members = useMemberStore((state) => state.members);

  const resetForm = () => {
    setName('');
    setEmoji('');
    setType('text');
    setGroupId('');
    resetFormState();
  };

  const handleClose = () => {
    if (!isSubmitting) {
      resetForm();
      onClose();
    }
  };

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};
    const nameError = validateChannelName(name);
    if (nameError) {
      newErrors.name = nameError;
    }
    if (!type) {
      newErrors.type = 'Channel type is required';
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    if (!validateForm() || !activeServerId) {
      return;
    }

    setIsSubmitting(true);
    setErrors({});

    try {
      const wrappedKeys = await generateWrappedKeysForMembers(members);

      const response = await apiFetch('/api/v1/channels', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          server_id: activeServerId,
          name: name.trim(),
          type,
          emoji: emoji.trim() || undefined,
          group_id: groupId || undefined,
          wrapped_keys: wrappedKeys,
        }),
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Failed to create channel');
      }

      // Add channel to store
      addChannel(data.channel);

      // Show success message
      setSuccessMessage('Channel created successfully!');

      // Call onSuccess callback if provided
      onSuccess?.(data.channel);

      // Close modal after short delay
      setTimeout(() => {
        handleClose();
      }, 1000);
    } catch (error) {
      setErrors({
        general: error instanceof Error ? error.message : 'Failed to create channel',
      });
    } finally {
      setIsSubmitting(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Create Channel">
      <form onSubmit={handleSubmit} className="create-channel-form">
        {/* General Error */}
        {errors.general && <div className="channel-form-error-banner">{errors.general}</div>}

        {/* Success Message */}
        {successMessage && <div className="channel-form-success-banner">{successMessage}</div>}

        {/* Channel Name */}
        <div className="channel-form-group">
          <label htmlFor="channel-name" className="channel-form-label">
            Channel Name <span className="required">*</span>
          </label>
          <input
            id="channel-name"
            type="text"
            className={`channel-form-input ${errors.name ? 'error' : ''}`}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="general-chat"
            maxLength={NAME_MAX}
            disabled={isSubmitting}
            autoFocus
          />
          {errors.name && <span className="channel-form-error">{errors.name}</span>}
          <span className="channel-form-hint">
            {name.length}/{NAME_MAX} characters
          </span>
        </div>

        <ChannelEmojiField emoji={emoji} onChange={setEmoji} disabled={isSubmitting} />

        {/* Channel Type */}
        <div className="channel-form-group">
          <span className="channel-form-label">
            Channel Type <span className="required">*</span>
          </span>
          <div className="channel-type-selector">
            <button
              type="button"
              className={`channel-type-option ${type === 'text' ? 'selected' : ''}`}
              onClick={() => setType('text')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('text')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Text</span>
                <span className="channel-type-desc">Send messages, images, and files</span>
              </div>
            </button>

            <button
              type="button"
              className={`channel-type-option ${type === 'voice' ? 'selected' : ''}`}
              onClick={() => setType('voice')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('voice')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Voice</span>
                <span className="channel-type-desc">Voice and video conversations</span>
              </div>
            </button>

            <button
              type="button"
              className={`channel-type-option ${type === 'bulletin' ? 'selected' : ''}`}
              onClick={() => setType('bulletin')}
              disabled={isSubmitting}
            >
              <span className="channel-type-icon">{getChannelTypeIcon('bulletin')}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">Bulletin</span>
                <span className="channel-type-desc">Important announcements only</span>
              </div>
            </button>
          </div>
          {errors.type && <span className="channel-form-error">{errors.type}</span>}
          <span className="channel-form-hint">Channel type cannot be changed after creation</span>
        </div>

        {/* Channel Group */}
        {channelGroups.length > 0 && (
          <div className="channel-form-group">
            <label htmlFor="channel-group" className="channel-form-label">
              Channel Group
            </label>
            <CustomSelect
              options={[
                { value: '', label: 'Uncategorized' },
                ...channelGroups.map((g) => ({ value: g.id, label: g.name })),
              ]}
              value={groupId}
              onChange={(v) => setGroupId(v)}
              disabled={isSubmitting}
              className="channel-form-input"
            />
            <span className="channel-form-hint">Organize this channel into a group</span>
          </div>
        )}

        {/* Submit Button */}
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
              'Create Channel'
            )}
          </button>
        </div>
      </form>
    </Modal>
  );
};

export default CreateChannelModal;
