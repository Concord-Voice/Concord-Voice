import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { apiFetch } from '../../services/apiClient';
import { ChannelGroup } from '../../types/chat';

interface DeleteCategoryModalProps {
  isOpen: boolean;
  group: ChannelGroup;
  onClose: () => void;
}

const DeleteCategoryModal: React.FC<DeleteCategoryModalProps> = ({ isOpen, group, onClose }) => {
  const activeServerId = useServerStore((state) => state.activeServerId);
  const removeChannelGroup = useChannelStore((state) => state.removeChannelGroup);

  const handleDelete = async () => {
    if (!activeServerId) return;

    const response = await apiFetch(
      `/api/v1/servers/${activeServerId}/channel-groups/${group.id}`,
      { method: 'DELETE' }
    );

    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.error || 'Failed to delete category');
    }

    removeChannelGroup(group.id);
  };

  return (
    <ConfirmActionModal
      isOpen={isOpen}
      onClose={onClose}
      title="Delete Category"
      message={
        <>
          Are you sure you want to delete the <strong>{group.name}</strong> category? Channels in
          this category will become uncategorized. No channels or messages will be deleted.
        </>
      }
      confirmLabel="Delete Category"
      loadingLabel="Deleting..."
      onConfirm={handleDelete}
    />
  );
};

export default DeleteCategoryModal;
