import React, { useEffect, useState } from 'react';
import { Clock, TimerOff } from 'lucide-react';
import { useNavigate } from 'react-router-dom';
import { ServerMember, useMemberStore } from '../../stores/memberStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { useDMStore } from '../../stores/dmStore';
import { useUserStore } from '../../stores/userStore';
import { useVoiceStore } from '../../stores/voiceStore';
import {
  Permissions,
  MUTE_MEMBERS,
  DEAFEN_MEMBERS,
  TIMEOUT_MEMBERS,
} from '../../utils/permissions';
import ContextMenu from '../ui/ContextMenu';
import { errorMessage } from '../../utils/redactError';
import { EnforcementMenuItems } from '../ui/EnforcementMenuItems';
import { useFriendRequestState } from '../../hooks/useFriendRequestState';
import { apiFetch, safeJson } from '../../services/apiClient';

const TIMEOUT_DURATIONS = [
  { label: '1 minute', seconds: 60 },
  { label: '5 minutes', seconds: 300 },
  { label: '10 minutes', seconds: 600 },
  { label: '1 hour', seconds: 3600 },
  { label: '1 day', seconds: 86400 },
  { label: '1 week', seconds: 604800 },
] as const;

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
  const [showTimeoutPicker, setShowTimeoutPicker] = useState(false);
  const [timeoutPickerClosing, setTimeoutPickerClosing] = useState(false);
  const [togglingRoleId, setTogglingRoleId] = useState<string | null>(null);
  const [timeoutActionPending, setTimeoutActionPending] = useState(false);
  const [renderedAtMs] = useState(() => Date.now());

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
  const canTimeout = hasServerPerm(serverId, TIMEOUT_MEMBERS) && !targetIsOwner && !isSelf;
  const isTimedOut = Boolean(
    member.timed_out_until && new Date(member.timed_out_until).getTime() > renderedAtMs
  );

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

  const toggleTimeoutPicker = () => {
    if (showTimeoutPicker) {
      setTimeoutPickerClosing(true);
      setTimeout(() => {
        setShowTimeoutPicker(false);
        setTimeoutPickerClosing(false);
      }, 150);
    } else {
      setShowTimeoutPicker(true);
    }
  };

  const applyTimeout = async (durationSeconds: number) => {
    if (timeoutActionPending) return;
    setTimeoutActionPending(true);
    try {
      const res = await apiFetch(
        '/api/v1/servers/' + serverId + '/members/' + member.user_id + '/timeout',
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ duration_seconds: durationSeconds }),
        }
      );
      const data = await safeJson<{ error?: string; timed_out_until?: string }>(res);
      if (!res.ok) throw new Error(data.error || 'Failed to timeout member');
      useMemberStore.getState().setMemberTimeout(member.user_id, data.timed_out_until ?? null);
      onClose();
    } catch (error) {
      console.error('Failed to timeout member:', errorMessage(error));
    } finally {
      setTimeoutActionPending(false);
    }
  };

  const removeTimeout = async () => {
    if (timeoutActionPending) return;
    setTimeoutActionPending(true);
    try {
      const res = await apiFetch(
        '/api/v1/servers/' + serverId + '/members/' + member.user_id + '/timeout',
        {
          method: 'DELETE',
        }
      );
      const data = await safeJson<{ error?: string }>(res);
      if (!res.ok) throw new Error(data.error || 'Failed to remove timeout');
      useMemberStore.getState().setMemberTimeout(member.user_id, null);
      onClose();
    } catch (error) {
      console.error('Failed to remove timeout:', errorMessage(error));
    } finally {
      setTimeoutActionPending(false);
    }
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

      {/* Timeout controls require TIMEOUT_MEMBERS and are hidden for owner/self */}
      {canTimeout && (
        <>
          <ContextMenu.Separator />
          <div className="ctx-menu-item-wrapper">
            <ContextMenu.Item
              icon={<Clock size={16} />}
              label={timeoutActionPending ? 'Updating...' : 'Timeout'}
              disabled={timeoutActionPending}
              hasSubMenu
              onClick={toggleTimeoutPicker}
            />
            {showTimeoutPicker && !timeoutActionPending && (
              <ContextMenu.SubMenu closing={timeoutPickerClosing}>
                {TIMEOUT_DURATIONS.map((duration) => (
                  <ContextMenu.Item
                    key={duration.seconds}
                    icon={<span style={{ width: 16 }} />}
                    label={duration.label}
                    onClick={() => {
                      void applyTimeout(duration.seconds);
                    }}
                  />
                ))}
              </ContextMenu.SubMenu>
            )}
          </div>
          {isTimedOut && (
            <ContextMenu.Item
              icon={<TimerOff size={16} />}
              label="Remove Timeout"
              disabled={timeoutActionPending}
              onClick={() => {
                void removeTimeout();
              }}
            />
          )}
        </>
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
