import React, { useEffect, useState, useCallback } from 'react';
import Modal from '../ui/Modal';
import OverridePanel from '../Permissions/OverridePanel';
import { usePermissionStore, ChannelOverride } from '../../stores/permissionStore';
import { useMemberStore } from '../../stores/memberStore';
import { Channel } from '../../types/chat';
import './ChannelSettingsModal.css';

interface ChannelSettingsModalProps {
  isOpen: boolean;
  channel: Channel;
  serverId: string;
  onClose: () => void;
}

const ChannelSettingsModal: React.FC<ChannelSettingsModalProps> = ({
  isOpen,
  channel,
  serverId,
  onClose,
}) => {
  const fetchChannelOverrides = usePermissionStore((s) => s.fetchChannelOverrides);
  const upsertChannelOverride = usePermissionStore((s) => s.upsertChannelOverride);
  const deleteChannelOverride = usePermissionStore((s) => s.deleteChannelOverride);
  const setCategorySync = usePermissionStore((s) => s.setCategorySync);
  const fetchRoles = usePermissionStore((s) => s.fetchRoles);
  const serverRoles = usePermissionStore((s) => s.serverRoles);
  const channelOverrides = usePermissionStore((s) => s.channelOverrides);
  const members = useMemberStore((s) => s.members);

  const [synced, setSynced] = useState(channel.sync_permissions ?? false);

  const overrides: ChannelOverride[] = channelOverrides[channel.id] ?? [];
  const roles = serverRoles[serverId] ?? [];

  useEffect(() => {
    if (isOpen) {
      fetchChannelOverrides(channel.id);
      fetchRoles(serverId);
    }
  }, [isOpen, channel.id, serverId, fetchChannelOverrides, fetchRoles]);

  // Reset synced state when modal opens
  useEffect(() => {
    if (isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets synced from channel props when modal opens or channel changes; not a render loop
      setSynced(channel.sync_permissions ?? false);
    }
  }, [isOpen, channel.id, channel.sync_permissions]);

  const handleSyncToggle = useCallback(async () => {
    const newSync = !synced;
    const success = await setCategorySync(channel.id, newSync);
    if (success) {
      setSynced(newSync);
      if (newSync) {
        // Refetch overrides since they may have been replaced
        fetchChannelOverrides(channel.id);
      }
    }
  }, [synced, channel.id, setCategorySync, fetchChannelOverrides]);

  const handleUpsert = async (data: {
    target_type: 'user' | 'role';
    target_id: string;
    allow: string;
    deny: string;
  }) => {
    await upsertChannelOverride(channel.id, data);
  };

  const handleDelete = async (overrideId: string) => {
    await deleteChannelOverride(channel.id, overrideId);
  };

  return (
    <Modal
      isOpen={isOpen}
      onClose={onClose}
      title={`Channel Permissions — #${channel.name}`}
      width="large"
    >
      {/* Category Sync Toggle */}
      {channel.group_id && (
        <div className="sync-section">
          <div className="sync-label">
            <span className="sync-label-text">Sync with category permissions</span>
            {synced && (
              <span className="sync-label-hint">
                Channel permissions will be replaced with category permissions and kept in sync.
              </span>
            )}
          </div>
          <div
            className={`sync-toggle${synced ? ' active' : ''}`}
            onClick={handleSyncToggle}
            role="switch"
            aria-checked={synced}
            tabIndex={0}
            onKeyDown={(e) => {
              if (e.key === 'Enter' || e.key === ' ') {
                e.preventDefault();
                handleSyncToggle();
              }
            }}
          />
        </div>
      )}

      {synced && (
        <div className="synced-notice">
          Permissions are synced with the parent category. Changes to category permissions will
          automatically apply to this channel.
        </div>
      )}

      <OverridePanel
        overrides={overrides}
        roles={roles}
        members={members}
        onUpsert={handleUpsert}
        onDelete={handleDelete}
        disabled={synced}
        emptyMessage="No permission overrides configured for this channel."
      />
    </Modal>
  );
};

export default ChannelSettingsModal;
