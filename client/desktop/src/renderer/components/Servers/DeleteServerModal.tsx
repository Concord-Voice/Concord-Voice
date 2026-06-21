import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { useServerStore } from '../../stores/serverStore';
import { apiFetch } from '../../services/apiClient';
import { ServerWithRole } from '../../types/server';
import './DeleteServerModal.css';

interface DeleteServerModalProps {
  isOpen: boolean;
  server: ServerWithRole;
  onClose: () => void;
}

const DeleteServerModal: React.FC<DeleteServerModalProps> = ({ isOpen, server, onClose }) => {
  const handleDelete = async () => {
    const response = await apiFetch(`/api/v1/servers/${server.id}`, {
      method: 'DELETE',
    });

    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.error || 'Failed to delete server');
    }

    useServerStore.getState().removeServer(server.id);
  };

  return (
    <ConfirmActionModal
      isOpen={isOpen}
      onClose={onClose}
      title="Delete Server"
      message={
        <>
          Are you sure you want to delete <strong>{server.name}</strong>? This action cannot be
          undone. All channels and messages in this server will be permanently deleted.
        </>
      }
      confirmLabel="Delete Server"
      loadingLabel="Deleting..."
      onConfirm={handleDelete}
      confirmationInput={{
        label: (
          <>
            Type <strong>{server.name}</strong> to confirm
          </>
        ),
        expectedValue: server.name,
      }}
    />
  );
};

export default DeleteServerModal;
