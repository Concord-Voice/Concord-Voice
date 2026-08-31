import React, { useEffect, useState, useCallback } from 'react';
import { Eraser } from 'lucide-react';
import Modal from '../ui/Modal';
import OverridePanel from '../Permissions/OverridePanel';
import PurgeMessagesModal from '../Purge/PurgeMessagesModal';
import { usePermissionStore, ChannelOverride } from '../../stores/chat/permissionStore';
import { useMemberStore } from '../../stores/chat/memberStore';
import {
  MANAGE_ALL_MESSAGES,
  MANAGE_OWN_MESSAGES,
  hasPermission,
} from '../../utils/policy/permissions';
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
  const [isPurgeModalOpen, setIsPurgeModalOpen] = useState(false);

  // Either manage-messages bit authorizes a purge; a ManageOwn-only actor gets
  // a self-scoped one rather than no entry point at all (spec §4.2). Per-channel
  // effective permissions when known, server-level grant otherwise.
  const channelPerms = usePermissionStore((s) => s.channelPermissions[channel.id]);
  const serverPerms = usePermissionStore((s) => s.serverPermissions[serverId]);
  const purgePerms = channelPerms ?? serverPerms ?? 0n;
  const canPurge =
    hasPermission(purgePerms, MANAGE_OWN_MESSAGES) ||
    hasPermission(purgePerms, MANAGE_ALL_MESSAGES);
  const purgeSelfScopeOnly = canPurge && !hasPermission(purgePerms, MANAGE_ALL_MESSAGES);

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

      {/* Destructive cluster. The purge dialog nests inside this one — ui/Modal
          maintains a depth stack, so the child owns focus and Escape while it
          is topmost (#2087). */}
      {canPurge && (
        <div className="sync-section">
          <button
            type="button"
            className="channel-settings-purge-btn"
            onClick={() => setIsPurgeModalOpen(true)}
          >
            <Eraser size={16} />
            Purge Messages
          </button>
        </div>
      )}

      <PurgeMessagesModal
        context="channel"
        isOpen={isPurgeModalOpen}
        onClose={() => setIsPurgeModalOpen(false)}
        scopeId={channel.id}
        scopeName={channel.name}
        selfScopeOnly={purgeSelfScopeOnly}
      />
    </Modal>
  );
};

export default ChannelSettingsModal;
