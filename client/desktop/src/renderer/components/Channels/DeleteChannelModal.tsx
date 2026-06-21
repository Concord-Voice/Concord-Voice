import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { useChannelStore } from '../../stores/channelStore';
import { apiFetch } from '../../services/apiClient';
import { Channel } from '../../types/chat';

interface DeleteChannelModalProps {
  isOpen: boolean;
  channel: Channel;
  onClose: () => void;
}

const DeleteChannelModal: React.FC<DeleteChannelModalProps> = ({ isOpen, channel, onClose }) => {
  const handleDelete = async () => {
    const response = await apiFetch(`/api/v1/channels/${channel.id}`, {
      method: 'DELETE',
    });

    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.error || 'Failed to delete channel');
    }

    useChannelStore.getState().removeChannel(channel.id);
  };

  return (
    <ConfirmActionModal
      isOpen={isOpen}
      onClose={onClose}
      title="Delete Channel"
      message={
        <>
          Are you sure you want to delete <strong>#{channel.name}</strong>? This action cannot be
          undone. All messages in this channel will be permanently deleted.
        </>
      }
      confirmLabel="Delete Channel"
      loadingLabel="Deleting..."
      onConfirm={handleDelete}
    />
  );
};

export default DeleteChannelModal;
