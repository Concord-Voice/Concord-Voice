import React, { useState, useEffect, useRef } from 'react';
import { useFormState } from '../../hooks/ui/useFormState';
import {
  NAME_MAX,
  validateChannelName,
  getChannelTypeIcon,
} from '../../utils/messaging/channelHelpers';
import ChannelEmojiField from './ChannelEmojiField';
import Modal from '../ui/Modal';
import CustomSelect from '../ui/CustomSelect';
import LoadingSpinner from '../Auth/LoadingSpinner';
import { useChannelStore } from '../../stores/chat/channelStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { apiFetch } from '../../services/system/apiClient';
import ChannelAudioQualitySlider from './ChannelAudioQualitySlider';
import { Channel } from '../../types/chat';
import { useExpirationPolicy } from '../../hooks/messaging/useExpirationPolicy';
import MessageExpirationEditor from '../Expiration/MessageExpirationEditor';
import { useAuthStore } from '../../stores/auth/authStore';
import {
  captureAuthLifecycle,
  isSameAuthLifecycle,
} from '../../services/system/postLoginHydrationLifecycle';
import './CreateChannelModal.css';

interface EditChannelModalProps {
  isOpen: boolean;
  channel: Channel;
  onClose: () => void;
  onSuccess?: (channel: Channel) => void;
}

interface FormErrors {
  name?: string;
  general?: string;
}

const EditChannelModal: React.FC<EditChannelModalProps> = ({
  isOpen,
  channel,
  onClose,
  onSuccess,
}) => {
  const [name, setName] = useState(channel.name);
  const [emoji, setEmoji] = useState(channel.emoji || '');
  const [groupId, setGroupId] = useState<string>(channel.group_id || '');
  const [audioQualityTier, setAudioQualityTier] = useState<string | null>(
    channel.audio_quality_tier ?? null
  );
  const [closePending, setClosePending] = useState(false);
  const {
    errors,
    setErrors,
    isSubmitting,
    setIsSubmitting,
    successMessage,
    setSuccessMessage,
    reset: resetFormState,
  } = useFormState<FormErrors>();

  const updateChannel = useChannelStore((state) => state.updateChannel);
  const channelGroups = useChannelStore((state) => state.channelGroups);
  const serverTier = useServerStore(
    (s) => s.servers.find((sv) => sv.id === channel.server_id)?.server_tier
  );
  const authGeneration = useAuthStore((state) => state.authGeneration);
  const expiration = useExpirationPolicy(
    isOpen && channel.type === 'text' ? { kind: 'channel', id: channel.id } : null,
    channel.server_id
  );
  const operationRef = useRef(0);
  const mountedRef = useRef(true);
  const viewRef = useRef({ channelId: channel.id, authGeneration, isOpen });
  viewRef.current = { channelId: channel.id, authGeneration, isOpen };

  useEffect(() => {
    mountedRef.current = true;
    return () => {
      mountedRef.current = false;
      operationRef.current += 1;
    };
  }, []);

  // Reset form when channel changes
  useEffect(() => {
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets name from channel prop when channel changes; not a render loop
    setName(channel.name);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets emoji from channel prop when channel changes; not a render loop
    setEmoji(channel.emoji || '');
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets groupId from channel prop when channel changes; not a render loop
    setGroupId(channel.group_id || '');
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets audioQualityTier from channel prop when channel changes; not a render loop
    setAudioQualityTier(channel.audio_quality_tier ?? null);
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- channel/auth changes cancel the delayed success close state
    setClosePending(false);
    resetFormState();
  }, [
    channel.id,
    channel.name,
    channel.emoji,
    channel.group_id,
    channel.audio_quality_tier,
    isOpen,
    authGeneration,
    resetFormState,
  ]);

  useEffect(() => {
    operationRef.current += 1;
    if (!isOpen) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- opening a fresh modal must clear stale delayed-close state
    setClosePending(false);
    setIsSubmitting(false);
  }, [authGeneration, channel.id, isOpen, setIsSubmitting]);

  const handleClose = () => {
    if (!isSubmitting && !closePending) {
      operationRef.current += 1;
      setClosePending(false);
      resetFormState();
      onClose();
    }
  };

  const closeExpirationEditor = () => {
    operationRef.current += 1;
    setClosePending(false);
    resetFormState();
    onClose();
  };

  const validateForm = (): boolean => {
    const newErrors: FormErrors = {};
    const nameError = validateChannelName(name);
    if (nameError) {
      newErrors.name = nameError;
    }
    setErrors(newErrors);
    return Object.keys(newErrors).length === 0;
  };

  const hasChanges = (): boolean => {
    return (
      name.trim() !== channel.name ||
      (emoji.trim() || '') !== (channel.emoji || '') ||
      groupId !== (channel.group_id || '') ||
      audioQualityTier !== (channel.audio_quality_tier ?? null)
    );
  };

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault();

    if (!validateForm()) {
      return;
    }

    if (!hasChanges()) {
      handleClose();
      return;
    }

    setIsSubmitting(true);
    setErrors({});
    const operation = ++operationRef.current;
    const lifecycle = captureAuthLifecycle();
    const channelId = channel.id;
    const stillCurrent = () =>
      mountedRef.current &&
      operation === operationRef.current &&
      viewRef.current.channelId === channelId &&
      viewRef.current.authGeneration === authGeneration &&
      viewRef.current.isOpen &&
      isSameAuthLifecycle(lifecycle);

    try {
      const response = await apiFetch(`/api/v1/channels/${channel.id}`, {
        method: 'PATCH',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({
          name: name.trim(),
          type: channel.type,
          emoji: emoji.trim() || null,
          group_id: groupId || null,
          audio_quality_tier: audioQualityTier || null,
        }),
      });

      const data = await response.json();

      if (!response.ok) {
        throw new Error(data.error || 'Failed to update channel');
      }

      if (!stillCurrent()) return;

      updateChannel(channel.id, data.channel);

      // Show success message
      setSuccessMessage('Channel updated successfully!');

      // Call onSuccess callback if provided
      if (onSuccess) {
        onSuccess(data.channel);
      }

      setClosePending(true);
      // Close modal after short delay
      setTimeout(() => {
        if (stillCurrent()) closeExpirationEditor();
      }, 1000);
    } catch (error) {
      if (stillCurrent())
        setErrors({
          general: error instanceof Error ? error.message : 'Failed to update channel',
        });
    } finally {
      if (stillCurrent()) setIsSubmitting(false);
    }
  };

  return (
    <Modal isOpen={isOpen} onClose={handleClose} title="Edit Channel">
      <form onSubmit={handleSubmit} className="create-channel-form">
        {/* General Error */}
        {errors.general && <div className="channel-form-error-banner">{errors.general}</div>}

        {/* Success Message */}
        {successMessage && <div className="channel-form-success-banner">{successMessage}</div>}

        {/* Channel Name */}
        <div className="channel-form-group">
          <label htmlFor="edit-channel-name" className="channel-form-label">
            Channel Name <span className="required">*</span>
          </label>
          <input
            id="edit-channel-name"
            type="text"
            className={`channel-form-input ${errors.name ? 'error' : ''}`}
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="general-chat"
            maxLength={NAME_MAX}
            disabled={isSubmitting || closePending}
            autoFocus
          />
          {errors.name && <span className="channel-form-error">{errors.name}</span>}
          <span className="channel-form-hint">
            {name.length}/{NAME_MAX} characters
          </span>
        </div>

        <ChannelEmojiField
          emoji={emoji}
          onChange={setEmoji}
          disabled={isSubmitting || closePending}
        />

        {/* Channel Type (read-only) */}
        <div className="channel-form-group">
          <span className="channel-form-label">Channel Type</span>
          <div className="channel-type-selector">
            <div className="channel-type-option selected" style={{ cursor: 'default' }}>
              <span className="channel-type-icon">{getChannelTypeIcon(channel.type)}</span>
              <div className="channel-type-info">
                <span className="channel-type-name">
                  {(() => {
                    if (channel.type === 'text') return 'Text';
                    if (channel.type === 'voice') return 'Voice';
                    return 'Bulletin';
                  })()}
                </span>
                <span className="channel-type-desc">
                  {(() => {
                    if (channel.type === 'text') return 'Send messages, images, and files';
                    if (channel.type === 'voice') return 'Voice and video conversations';
                    return 'Important announcements only';
                  })()}
                </span>
              </div>
            </div>
          </div>
          <span className="channel-form-hint">Channel type cannot be changed after creation</span>
        </div>

        {/* Channel Group */}
        {channelGroups.length > 0 && (
          <div className="channel-form-group">
            <label htmlFor="edit-channel-group" className="channel-form-label">
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
            <span className="channel-form-hint">Move this channel to a different group</span>
          </div>
        )}

        {/* Audio Quality Standard (voice channels only) */}
        {channel.type === 'voice' && (
          <div className="channel-form-group">
            <span className="channel-form-label">Audio Quality</span>
            <span className="channel-form-hint" style={{ marginBottom: 8 }}>
              Set a channel-wide audio standard for everyone, or leave on Personal so each member
              uses their own setting.
            </span>
            <ChannelAudioQualitySlider
              value={audioQualityTier}
              onChange={setAudioQualityTier}
              serverTier={serverTier}
              disabled={isSubmitting}
            />
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
            disabled={isSubmitting || !hasChanges()}
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
      {channel.type === 'text' && (
        <>
          <h3 className="channel-form-label">Message expiration</h3>
          <MessageExpirationEditor
            scope={{ kind: 'channel', id: channel.id }}
            policy={expiration.policy}
            policyState={expiration.policyState}
            canEdit={expiration.canEdit && !isSubmitting && !closePending}
            lockedDescription={expiration.lockedDescription}
            onRefresh={expiration.onRefresh}
            onApplyPolicy={expiration.onApplyPolicy}
            onMarkSeen={expiration.onMarkSeen}
            onClose={closeExpirationEditor}
          />
        </>
      )}
    </Modal>
  );
};

export default EditChannelModal;
