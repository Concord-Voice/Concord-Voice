import React, { useState } from 'react';
import { Hash, Volume2, Pin, Eraser } from 'lucide-react';
import { useUnreadStore } from '../../stores/chat/unreadStore';
import { useServerStore } from '../../stores/chat/serverStore';
import { hasUnmutedChannel } from '../../stores/ui/notificationPrefsStore';
import { usePermissionStore } from '../../stores/chat/permissionStore';
import {
  MANAGE_ALL_MESSAGES,
  MANAGE_OWN_MESSAGES,
  Permissions,
  hasPermission,
} from '../../utils/policy/permissions';
import { apiFetch } from '../../services/system/apiClient';
import { useRotateKey } from '../../hooks/voice/useRotateKey';
import { Channel } from '../../types/chat';
import ContextMenu from '../ui/ContextMenu';
import MuteContextMenuItem from '../Notifications/MuteContextMenuItem';
import './ChannelContextMenu.css';

interface ChannelContextMenuProps {
  channel: Channel;
  position: { x: number; y: number };
  serverId: string;
  onClose: () => void;
  onEditChannel: (channel: Channel) => void;
  onDeleteChannel: (channel: Channel) => void;
  onChannelPermissions?: (channel: Channel) => void;
  /**
   * Parent-owned purge modal host. The menu unmounts on close, so the modal
   * lives in MainViewModals — the same lifted-state shape as Delete Channel.
   */
  onPurgeMessages?: (channel: Channel) => void;
}

const ChannelContextMenu: React.FC<ChannelContextMenuProps> = ({
  channel,
  position,
  serverId,
  onClose,
  onEditChannel,
  onDeleteChannel,
  onChannelPermissions,
  onPurgeMessages,
}) => {
  const [copiedLink, setCopiedLink] = useState(false);
  const hasServerPerm = usePermissionStore((s) => s.hasServerPermission);
  const canEdit = hasServerPerm(serverId, Permissions.MANAGE_CHANNELS);
  const canDelete = hasServerPerm(serverId, Permissions.MANAGE_CHANNELS);
  const canRotateKey = hasServerPerm(serverId, Permissions.MANAGE_CRYPTO_ROTATION);
  // Per-channel effective permissions when they have already been fetched for
  // this channel (MessageInput fetches them for the open channel), server-level
  // grant otherwise. EITHER manage bit authorizes a purge: a ManageOwn-only
  // actor is genuinely permitted and the server self-scopes their purge, so
  // hiding the item would deny a permitted action (spec §4.2). The client gate
  // is UX-only — every purge is re-authorized server-side.
  const channelPerms = usePermissionStore((s) => s.channelPermissions[channel.id]);
  const serverPerms = usePermissionStore((s) => s.serverPermissions[serverId]);
  const purgePerms = channelPerms ?? serverPerms ?? 0n;
  const canPurge =
    hasPermission(purgePerms, MANAGE_OWN_MESSAGES) ||
    hasPermission(purgePerms, MANAGE_ALL_MESSAGES);

  const { rotateStatus, rotateMessage, handleRotate } = useRotateKey(
    `/api/v1/channels/${channel.id}/rotate-key`,
    () => setTimeout(() => onClose(), 800)
  );

  const getChannelTypeIcon = (type: Channel['type']) => {
    switch (type) {
      case 'text':
        return <Hash size={16} />;
      case 'voice':
        return <Volume2 size={16} />;
      case 'bulletin':
        return <Pin size={16} />;
      default:
        return <Hash size={16} />;
    }
  };

  const handleCopyLink = async () => {
    await navigator.clipboard.writeText(channel.id);
    setCopiedLink(true);
    setTimeout(() => onClose(), 800);
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Header>
        <span className="channel-ctx-type-icon">{getChannelTypeIcon(channel.type)}</span>
        {channel.emoji && <span className="channel-ctx-emoji">{channel.emoji}</span>}
        <span className="channel-ctx-name">{channel.name}</span>
      </ContextMenu.Header>

      <ContextMenu.Separator />

      {/* Mark as Read */}
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path
              d="M2 8.5l3.5 3.5L14 3"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
              strokeLinejoin="round"
            />
          </svg>
        }
        label="Mark as Read"
        onClick={() => {
          useUnreadStore.getState().clearUnread(channel.id);
          apiFetch(`/api/v1/channels/${channel.id}/read`, { method: 'POST' }).catch(() => {});
          // Clear the server dot only when no UNMUTED channel unread remains.
          // A plain `size === 0` check would leave the dot lit when the only
          // remaining unreads are muted channels (#84 / epic #1029 close
          // audit, P2 follow-up).
          const { unreadCounts } = useUnreadStore.getState();
          const activeServerId = useServerStore.getState().activeServerId;
          if (activeServerId && !hasUnmutedChannel(unreadCounts.keys(), activeServerId)) {
            useUnreadStore.getState().clearServerUnread(activeServerId);
          }
          onClose();
        }}
      />

      {/* Mute / Unmute Channel — duration submenu when muting. */}
      <MuteContextMenuItem
        targetType="channel"
        targetId={channel.id}
        kindLabel="Channel"
        onAction={onClose}
      />

      {/* Copy Link */}
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <path d="M6.5 9.5l3-3" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
            <path
              d="M9 10.5l1.5-1.5a2.83 2.83 0 000-4l0 0a2.83 2.83 0 00-4 0L5 6.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
            <path
              d="M7 5.5L5.5 7a2.83 2.83 0 000 4l0 0a2.83 2.83 0 004 0L11 9.5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        }
        label={copiedLink ? 'Copied!' : 'Copy Link'}
        onClick={handleCopyLink}
      />

      {/* Edit Channel — admin/owner only */}
      {canEdit && (
        <>
          <ContextMenu.Separator />
          <ContextMenu.Item
            icon={
              <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                <path
                  d="M11.5 1.5l3 3L5 14H2v-3L11.5 1.5z"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            }
            label="Edit Channel"
            onClick={() => {
              onEditChannel(channel);
              onClose();
            }}
          />
          <ContextMenu.Item
            icon={
              <svg width="16" height="16" viewBox="0 0 24 24" fill="none">
                <path
                  d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"
                  stroke="currentColor"
                  strokeWidth="1.5"
                  strokeLinecap="round"
                  strokeLinejoin="round"
                />
              </svg>
            }
            label="Channel Permissions"
            onClick={() => {
              onClose();
              onChannelPermissions?.(channel);
            }}
          />
        </>
      )}

      {/* Rotate Encryption Key — encrypted channels + MANAGE_CRYPTO_ROTATION */}
      {canRotateKey &&
        (() => {
          let rotateLabel = 'Rotate Encryption Key';
          if (rotateStatus === 'success') rotateLabel = 'Key Rotated!';
          else if (rotateStatus === 'error') rotateLabel = rotateMessage;

          return (
            <ContextMenu.Item
              icon={
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M11 1.5a3.5 3.5 0 00-3.5 3.5c0 .47.1.92.27 1.33L2 12.1V14h1.9l5.77-5.77c.41.17.86.27 1.33.27A3.5 3.5 0 0014.5 5 3.5 3.5 0 0011 1.5zm0 2a1.5 1.5 0 110 3 1.5 1.5 0 010-3z"
                    fill="currentColor"
                  />
                  <path
                    d="M1.5 8.5l2-2"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                  />
                </svg>
              }
              label={rotateLabel}
              disabled={rotateStatus === 'success'}
              onClick={handleRotate}
            />
          );
        })()}

      {/* Purge Messages — above Delete Channel so the destructive cluster
          reads in ascending severity. The separator is only needed when the
          Edit Channel block above did not already open the cluster. */}
      {canPurge && onPurgeMessages && (
        <>
          {!canEdit && <ContextMenu.Separator />}
          <ContextMenu.Item
            icon={<Eraser size={16} />}
            label="Purge Messages"
            danger
            onClick={() => {
              onPurgeMessages(channel);
              onClose();
            }}
          />
        </>
      )}

      {/* Delete Channel — admin/owner only */}
      {canDelete && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M2 4h12M5.33 4V2.67a1.33 1.33 0 011.34-1.34h2.66a1.33 1.33 0 011.34 1.34V4M12.67 4v9.33a1.33 1.33 0 01-1.34 1.34H4.67a1.33 1.33 0 01-1.34-1.34V4"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Delete Channel"
          danger
          onClick={() => {
            onDeleteChannel(channel);
            onClose();
          }}
        />
      )}
    </ContextMenu>
  );
};

export default ChannelContextMenu;
