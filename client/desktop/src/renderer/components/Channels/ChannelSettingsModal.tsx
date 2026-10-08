import React, { useEffect, useState, useCallback } from 'react';
import { Eraser } from 'lucide-react';
import Modal from '../ui/Modal';
import OverridePanel, { type OverrideUpsertArgs } from '../Permissions/OverridePanel';
import PurgeMessagesModal from '../Purge/PurgeMessagesModal';
import {
  usePermissionStore,
  ChannelOverride,
  NO_PERMISSION_WRITES,
} from '../../stores/chat/permissionStore';
import { useMemberStore } from '../../stores/chat/memberStore';
import { useChannelStore } from '../../stores/chat/channelStore';
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

  // The channel prop is a snapshot taken when the modal opened; the store holds
  // the current flag, which a sync started before a close and reopen may have
  // changed since (#3406 review, round 8).
  const storedSync = useChannelStore(
    (s) => s.channels.find((c) => c.id === channel.id)?.sync_permissions
  );
  const channelSync = storedSync ?? channel.sync_permissions ?? false;
  const [synced, setSynced] = useState(channelSync);
  const [isPurgeModalOpen, setIsPurgeModalOpen] = useState(false);
  // The category sync and the override panel's writes exclude each other
  // (#3406): a sync that lands while an override write is in flight would hide
  // that write's failure with the editor, and the two writes race on the
  // server.
  const [isOverrideWritePending, setIsOverrideWritePending] = useState(false);
  const [isSyncPending, setIsSyncPending] = useState(false);
  const isSyncLocked = isOverrideWritePending || isSyncPending;
  // Both flags above die with this modal when it is closed, while the request
  // can still commit. The store's record of this channel's writes, including a
  // sync, survives a close and reopen; the panel inherits the ones in flight
  // when it mounts and reports them through onWritePendingChange, which locks
  // the switch too (#3406 review, round 6).
  const writesInFlight = usePermissionStore(
    (s) => s.permissionWritesInFlight[channel.id] ?? NO_PERMISSION_WRITES
  );

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

  // Reset synced state when modal opens, and follow the stored flag after
  useEffect(() => {
    if (isOpen) {
      // eslint-disable-next-line @eslint-react/set-state-in-effect -- intentional: resets synced from the channel's stored flag when the modal opens or the flag changes; not a render loop
      setSynced(channelSync);
    }
  }, [isOpen, channel.id, channelSync]);

  const handleSyncToggle = useCallback(async () => {
    if (isSyncLocked) return;
    const newSync = !synced;
    setIsSyncPending(true);
    try {
      // The store records the new flag and re-reads replaced overrides itself.
      const success = await setCategorySync(channel.id, newSync);
      if (success) setSynced(newSync);
    } finally {
      setIsSyncPending(false);
    }
  }, [isSyncLocked, synced, channel.id, setCategorySync]);

  const handleUpsert = (...args: OverrideUpsertArgs) => upsertChannelOverride(channel.id, ...args);

  const handleDelete = (overrideId: string) => deleteChannelOverride(channel.id, overrideId);

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
            aria-disabled={isSyncLocked}
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
        locked={isSyncPending}
        onWritePendingChange={setIsOverrideWritePending}
        writesInFlight={writesInFlight}
        stepUpPurpose="overrides.channel_upsert"
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
