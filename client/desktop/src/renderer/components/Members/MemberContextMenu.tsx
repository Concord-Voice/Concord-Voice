import React, { useEffect, useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { ServerMember, useMemberStore } from '../../stores/memberStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useDMStore } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { Permissions, MUTE_MEMBERS, DEAFEN_MEMBERS } from '../../utils/permissions';
import ContextMenu from '../ui/ContextMenu';
import { errorMessage } from '../../utils/redactError';
import { EnforcementMenuItems } from '../ui/EnforcementMenuItems';
import { useFriendRequestState } from '../../hooks/useFriendRequestState';

interface MemberContextMenuProps {
  member: ServerMember;
  position: { x: number; y: number };
  serverId: string;
  ownerUserId: string;
  onClose: () => void;
  onViewProfile: () => void;
  onBan: (member: ServerMember) => void;
  onKick: (member: ServerMember) => void;
}

const MemberContextMenu: React.FC<MemberContextMenuProps> = ({
  member,
  position,
  serverId,
  ownerUserId,
  onClose,
  onViewProfile,
  onBan,
  onKick,
}) => {
  const [showRolePicker, setShowRolePicker] = useState(false);
  const [rolePickerClosing, setRolePickerClosing] = useState(false);
  const [togglingRoleId, setTogglingRoleId] = useState<string | null>(null);

  const navigate = useNavigate();
  const currentUserId = useUserStore((s) => s.user?.id);
  const hasServerPerm = usePermissionStore((s) => s.hasServerPermission);
  const serverRoles = usePermissionStore((s) => s.serverRoles[serverId]);
  const fetchRoles = usePermissionStore((s) => s.fetchRoles);
  const assignRole = usePermissionStore((s) => s.assignRole);
  const unassignRole = usePermissionStore((s) => s.unassignRole);

  // Friend-request relationship state + send action, shared with the member
  // profile card and chat profile card via the same hook (single source of
  // truth for the Friends / Request Pending / Send Friend Request labels).
  const friendReq = useFriendRequestState(member.user_id);
  const { isFriend, hasPendingRequest } = friendReq;

  const isSelf = member.user_id === currentUserId;
  const targetIsOwner = member.user_id === ownerUserId;

  // Permission checks
  const canAssignRole =
    hasServerPerm(serverId, Permissions.MANAGE_ROLES_ASSIGN) && !targetIsOwner && !isSelf;
  const canBan = hasServerPerm(serverId, Permissions.BAN) && !targetIsOwner && !isSelf;
  const canKick = hasServerPerm(serverId, Permissions.KICK) && !targetIsOwner && !isSelf;
  const canMute = hasServerPerm(serverId, MUTE_MEMBERS) && !targetIsOwner && !isSelf;
  const canDeafen = hasServerPerm(serverId, DEAFEN_MEMBERS) && !targetIsOwner && !isSelf;

  // Check if target is currently in a voice channel on this server
  const channelVoiceMembers = useVoiceStore((s) => s.channelVoiceMembers);
  const targetIsInVoice = Object.values(channelVoiceMembers).some((members) =>
    members.some((m) => m.userId === member.user_id)
  );

  // Fetch RBAC roles only when role picker is needed and not yet loaded
  useEffect(() => {
    if (canAssignRole && showRolePicker && !serverRoles) {
      fetchRoles(serverId);
    }
  }, [canAssignRole, showRolePicker, serverRoles, fetchRoles, serverId]);

  // RBAC roles sorted by position (highest first), excluding the default @everyone role
  const assignableRoles = (serverRoles ?? [])
    .filter((r) => !r.is_default)
    .sort((a, b) => b.position - a.position);

  // Set of role IDs the member currently has
  const memberRoleIds = new Set(member.roles.map((r) => r.role_id));

  const handleRoleToggle = async (roleId: string) => {
    if (togglingRoleId) return;
    setTogglingRoleId(roleId);

    try {
      const hasRole = memberRoleIds.has(roleId);
      const ok = hasRole
        ? await unassignRole(serverId, member.user_id, roleId)
        : await assignRole(serverId, member.user_id, roleId);

      if (ok) {
        // Refetch members so the role badges update immediately
        await useMemberStore.getState().fetchMembers(serverId);
      } else {
        console.error('Failed to toggle role: server returned non-OK');
      }
    } catch (error) {
      console.error('Failed to toggle role:', errorMessage(error));
    } finally {
      setTogglingRoleId(null);
    }
  };

  const handleSendDM = async () => {
    try {
      await useDMStore.getState().openDM(member.user_id);
      navigate('/app/dms');
      onClose();
    } catch (error) {
      console.error('Failed to open DM:', errorMessage(error));
      onClose();
    }
  };

  const handleSendFriendRequest = () => {
    // Fire-and-forget; the hook records the error internally. The context menu
    // closes immediately (matching every other menu action) — rich inline
    // feedback lives on the profile-card surface, not the transient menu.
    void friendReq.send();
    onClose();
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Header>
        <span
          style={{
            fontWeight: 600,
            color: 'var(--text-primary)',
            fontSize: 'calc(14px * var(--font-scale, 1))',
          }}
        >
          {member.display_name || member.username}
        </span>
      </ContextMenu.Header>

      <ContextMenu.Separator />

      {/* View Profile — always available */}
      <ContextMenu.Item
        icon={
          <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
            <circle cx="8" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
            <path
              d="M2 14c0-2.76 2.69-5 6-5s6 2.24 6 5"
              stroke="currentColor"
              strokeWidth="1.5"
              strokeLinecap="round"
            />
          </svg>
        }
        label="View Profile"
        onClick={() => {
          onViewProfile();
          onClose();
        }}
      />

      {/* Send DM — hidden for self */}
      {!isSelf && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M2 3h12v9H4l-2 2V3z"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Send DM"
          onClick={handleSendDM}
        />
      )}

      {/* Friend Request — hidden for self */}
      {!isSelf && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <circle cx="7" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
              <path
                d="M1 14c0-2.76 2.24-5 6-5"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
              <path
                d="M12 10v4M10 12h4"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label={friendReq.label}
          disabled={isFriend || hasPendingRequest}
          onClick={handleSendFriendRequest}
        />
      )}

      {/* Assign Role — requires MANAGE_ROLES_ASSIGN, hidden for owner/self */}
      {canAssignRole && (
        <>
          <ContextMenu.Separator />
          <div className="ctx-menu-item-wrapper">
            <ContextMenu.Item
              icon={
                <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
                  <path
                    d="M8 1l2 4h4l-3.5 3 1.5 4.5L8 10l-4 2.5L5.5 8 2 5h4l2-4z"
                    stroke="currentColor"
                    strokeWidth="1.5"
                    strokeLinecap="round"
                    strokeLinejoin="round"
                  />
                </svg>
              }
              label={togglingRoleId ? 'Updating...' : 'Assign Role'}
              disabled={!!togglingRoleId}
              hasSubMenu
              onClick={() => {
                if (showRolePicker) {
                  setRolePickerClosing(true);
                  setTimeout(() => {
                    setShowRolePicker(false);
                    setRolePickerClosing(false);
                  }, 150);
                } else {
                  setShowRolePicker(true);
                }
              }}
            />

            {/* Role picker flyout submenu — shows RBAC roles */}
            {showRolePicker && !togglingRoleId && (
              <ContextMenu.SubMenu closing={rolePickerClosing}>
                {assignableRoles.length === 0 ? (
                  <ContextMenu.Item
                    icon={<span style={{ width: 16 }} />}
                    label="No roles available"
                    disabled
                    onClick={() => {}}
                  />
                ) : (
                  assignableRoles.map((role) => {
                    const hasRole = memberRoleIds.has(role.id);
                    return (
                      <ContextMenu.Item
                        key={role.id}
                        icon={
                          <span
                            style={{
                              width: 16,
                              textAlign: 'center',
                              fontSize: 12,
                              display: 'inline-flex',
                              alignItems: 'center',
                              justifyContent: 'center',
                            }}
                          >
                            {hasRole
                              ? '✓'
                              : role.emoji || (
                                  <span
                                    style={{
                                      display: 'inline-block',
                                      width: 10,
                                      height: 10,
                                      borderRadius: '50%',
                                      backgroundColor: role.color || 'var(--text-secondary)',
                                    }}
                                  />
                                )}
                          </span>
                        }
                        label={role.name}
                        onClick={() => handleRoleToggle(role.id)}
                      />
                    );
                  })
                )}
              </ContextMenu.SubMenu>
            )}
          </div>
        </>
      )}

      {/* Voice enforcement — mute/deafen controls */}
      {(canMute || canDeafen) && (
        <EnforcementMenuItems
          targetUserId={member.user_id}
          targetServerMuted={member.server_muted || false}
          targetServerDeafened={member.server_deafened || false}
          targetIsMuted={false}
          targetIsInVoice={targetIsInVoice}
          context={{
            type: 'server',
            serverId,
            canMute,
            canDeafen,
            canModerate: !targetIsOwner,
          }}
          onClose={onClose}
        />
      )}

      {/* Danger zone separator */}
      {(canKick || canBan) && <ContextMenu.Separator />}

      {/* Kick */}
      {canKick && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <path
                d="M10 2l4 4-4 4M14 6H6M2 2v12"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
                strokeLinejoin="round"
              />
            </svg>
          }
          label="Kick"
          danger
          onClick={() => {
            onKick(member);
            onClose();
          }}
        />
      )}

      {/* Ban */}
      {canBan && (
        <ContextMenu.Item
          icon={
            <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
              <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="1.5" />
              <path
                d="M3.75 3.75l8.5 8.5"
                stroke="currentColor"
                strokeWidth="1.5"
                strokeLinecap="round"
              />
            </svg>
          }
          label="Ban"
          danger
          onClick={() => {
            onBan(member);
            onClose();
          }}
        />
      )}
    </ContextMenu>
  );
};

export default MemberContextMenu;
