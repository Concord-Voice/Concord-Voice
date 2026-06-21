import React from 'react';
import { ServerWithRole } from '../../types/server';
import { useUnreadStore } from '../../stores/unreadStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useUserStore } from '../../stores/userStore';
import { Permissions } from '../../utils/permissions';
import { apiFetch } from '../../services/apiClient';
import ContextMenu from '../ui/ContextMenu';
import MuteContextMenuItem from '../Notifications/MuteContextMenuItem';
import './ServerContextMenu.css';

interface ServerContextMenuProps {
  server: ServerWithRole;
  position: { x: number; y: number };
  onClose: () => void;
  onEditServer: (server: ServerWithRole) => void;
  onDeleteServer: (server: ServerWithRole) => void;
  onLeaveServer: (server: ServerWithRole) => void;
  onInvite: (server: ServerWithRole) => void;
}

const ServerContextMenu: React.FC<ServerContextMenuProps> = ({
  server,
  position,
  onClose,
  onEditServer,
  onDeleteServer,
  onLeaveServer,
  onInvite,
}) => {
  const hasServerPerm = usePermissionStore((s) => s.hasServerPermission);
  const currentUserId = useUserStore((s) => s.user?.id);
  const canEdit = hasServerPerm(server.id, Permissions.MANAGE_SERVER);
  const isOwner = currentUserId === server.owner_id;

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Header>
        <span className="server-ctx-name">{server.name}</span>
        <span className={`server-ctx-role role-${server.role}`}>{server.role}</span>
      </ContextMenu.Header>

      <ContextMenu.Separator />

      {/* Mark All as Read */}
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
        label="Mark All as Read"
        onClick={() => {
          useUnreadStore.getState().clearAll();
          useUnreadStore.getState().clearServerUnread(server.id);
          apiFetch(`/api/v1/servers/${server.id}/read`, { method: 'POST' }).catch(() => {});
          onClose();
        }}
      />

      {/* Mute / Unmute Server — duration submenu when muting, instant unmute. */}
      <MuteContextMenuItem
        targetType="server"
        targetId={server.id}
        kindLabel="Server"
        onAction={onClose}
      />

      {/* Server Settings — admin/owner only */}
      {canEdit && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <circle cx="8" cy="8" r="2" stroke="currentColor" strokeWidth="1.5" />
              <path
                d="M8 1v1.5M8 13.5V15M15 8h-1.5M2.5 8H1M12.95 3.05l-1.06 1.06M4.11 11.89l-1.06 1.06M12.95 12.95l-1.06-1.06M4.11 4.11L3.05 3.05"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="Server Settings"
          onClick={() => {
            onEditServer(server);
            onClose();
          }}
        />
      )}

      {/* Invite to Server */}
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <circle cx="6" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
            <path
              d="M1 14c0-2.76 2.24-5 5-5s5 2.24 5 5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
            <path
              d="M12 5v4M10 7h4"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        }
        label="Invite to Server"
        onClick={() => {
          onInvite(server);
          onClose();
        }}
      />

      <ContextMenu.Separator />

      {/* Leave Server — everyone except owner */}
      {!isOwner && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M6 2H4a2 2 0 00-2 2v8a2 2 0 002 2h2"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
              <path
                d="M10.5 11.5L14 8l-3.5-3.5M6 8h8"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Leave Server"
          danger
          onClick={() => {
            onLeaveServer(server);
            onClose();
          }}
        />
      )}

      {/* Delete Server — owner only */}
      {isOwner && (
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
          label="Delete Server"
          danger
          onClick={() => {
            onDeleteServer(server);
            onClose();
          }}
        />
      )}
    </ContextMenu>
  );
};

export default ServerContextMenu;
