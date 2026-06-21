import React from 'react';
import ConfirmActionModal from '../ui/ConfirmActionModal';
import { useServerStore } from '../../stores/serverStore';
import { useUserStore } from '../../stores/userStore';
import { apiFetch } from '../../services/apiClient';
import { ServerWithRole } from '../../types/server';

interface LeaveServerModalProps {
  isOpen: boolean;
  server: ServerWithRole;
  onClose: () => void;
}

const LeaveServerModal: React.FC<LeaveServerModalProps> = ({ isOpen, server, onClose }) => {
  const userId = useUserStore((s) => s.user?.id);

  const handleLeave = async () => {
    if (!userId) return;

    const response = await apiFetch(`/api/v1/servers/${server.id}/members/${userId}`, {
      method: 'DELETE',
    });

    if (!response.ok) {
      const data = await response.json();
      throw new Error(data.error || 'Failed to leave server');
    }

    useServerStore.getState().removeServer(server.id);
  };

  return (
    <ConfirmActionModal
      isOpen={isOpen}
      onClose={onClose}
      title="Leave Server"
      message={
        <>
          Are you sure you want to leave <strong>{server.name}</strong>? You will lose access to all
          channels and messages in this server. You will need a new invite to rejoin.
        </>
      }
      confirmLabel="Leave Server"
      loadingLabel="Leaving..."
      onConfirm={handleLeave}
    />
  );
};

export default LeaveServerModal;
