import { useState } from 'react';
import { useNavigate } from 'react-router-dom';
import { PhoneOff, MoveRight } from 'lucide-react';
import ContextMenu from '../ui/ContextMenu';
import { EnforcementMenuItems } from '../ui/EnforcementMenuItems';
import { ParticipantVolumeRow } from './ParticipantVolumeRow';
import { usePermissionStore } from '../../stores/permissionStore';
import { useChannelStore } from '../../stores/channelStore';
import { useUserStore } from '../../stores/userStore';
import { useDMStore } from '../../stores/dmStore';
import { useFriendRequestState } from '../../hooks/useFriendRequestState';
import { Permissions, MUTE_MEMBERS, DEAFEN_MEMBERS, MOVE_MEMBERS } from '../../utils/permissions';
import { errorMessage } from '../../utils/redactError';
import { moveVoiceParticipant, disconnectVoiceParticipant } from '../../services/voiceParticipantApi';
import './VoiceParticipantContextMenu.css';

/**
 * Minimal participant shape shared by both render sites: the channel-sidebar
 * voice list (`ChannelVoiceMember`) and the in-call `ParticipantTile`
 * (`VoiceParticipant`). Only the fields the menu reads are required.
 */
export interface VoiceMenuParticipant {
  userId: string;
  username: string;
  displayName?: string;
  isMuted: boolean;
  serverMuted: boolean;
  serverDeafened: boolean;
}

export interface VoiceParticipantContextMenuProps {
  participant: VoiceMenuParticipant;
  /** Server hosting this voice channel. */
  serverId: string;
  /** Voice channel the participant is currently in. */
  channelId: string;
  position: { x: number; y: number };
  onClose: () => void;
  /** Opens the profile card / full profile for this participant. */
  onViewProfile?: () => void;
  /**
   * Optional kick/ban handoff (mirrors MemberContextMenu). The danger-zone
   * confirm flow is owned by the host surface; these render only when supplied
   * AND the actor holds the matching permission.
   */
  onKick?: () => void;
  onBan?: () => void;
  /** Owner of the server — owner is never a kick/ban target. */
  ownerUserId?: string;
  /**
   * Render the per-participant volume slider (ParticipantVolumeRow). Only the
   * in-call tile surface (ParticipantTile) wants this — the channel-sidebar
   * surface lists participants we may not be in a call with, so it omits it.
   */
  showVolumeControl?: boolean;
}

const profileIcon = (
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
    <circle cx="8" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
    <path
      d="M2 14c0-2.76 2.69-5 6-5s6 2.24 6 5"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
    />
  </svg>
);

const dmIcon = (
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
    <path
      d="M2 3h12v9H4l-2 2V3z"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  </svg>
);

const friendIcon = (
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
    <circle cx="7" cy="5" r="3" stroke="currentColor" strokeWidth="1.5" />
    <path d="M1 14c0-2.76 2.24-5 6-5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
    <path d="M12 10v4M10 12h4" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
  </svg>
);

const kickIcon = (
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
    <path
      d="M10 2l4 4-4 4M14 6H6M2 2v12"
      stroke="currentColor"
      strokeWidth="1.5"
      strokeLinecap="round"
      strokeLinejoin="round"
    />
  </svg>
);

const banIcon = (
  <svg width="16" height="16" viewBox="0 0 16 16" fill="none">
    <circle cx="8" cy="8" r="6" stroke="currentColor" strokeWidth="1.5" />
    <path d="M3.75 3.75l8.5 8.5" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" />
  </svg>
);

export function VoiceParticipantContextMenu({
  participant,
  serverId,
  channelId,
  position,
  onClose,
  onViewProfile,
  onKick,
  onBan,
  ownerUserId,
  showVolumeControl = false,
}: Readonly<VoiceParticipantContextMenuProps>) {
  const navigate = useNavigate();
  const currentUserId = useUserStore((s) => s.user?.id);
  const hasServerPerm = usePermissionStore((s) => s.hasServerPermission);
  const channels = useChannelStore((s) => s.channels);

  const friendReq = useFriendRequestState(participant.userId);
  const { isFriend, hasPendingRequest } = friendReq;

  const [showMoveTargets, setShowMoveTargets] = useState(false);

  const isSelf = participant.userId === currentUserId;
  const targetIsOwner = !!ownerUserId && participant.userId === ownerUserId;

  // Permission gates. Voice actions are hidden for self; the server is
  // authoritative (these are UX affordances, not the enforcement boundary).
  const canMute = hasServerPerm(serverId, MUTE_MEMBERS) && !isSelf;
  const canDeafen = hasServerPerm(serverId, DEAFEN_MEMBERS) && !isSelf;
  const canMove = hasServerPerm(serverId, MOVE_MEMBERS) && !isSelf;
  const canKick =
    !!onKick && hasServerPerm(serverId, Permissions.KICK) && !isSelf && !targetIsOwner;
  const canBan = !!onBan && hasServerPerm(serverId, Permissions.BAN) && !isSelf && !targetIsOwner;

  // Same-server voice channels, excluding the participant's current one.
  const moveTargets = channels.filter(
    (c) => c.type === 'voice' && c.server_id === serverId && c.id !== channelId
  );

  const handleSendDM = async () => {
    try {
      await useDMStore.getState().openDM(participant.userId);
      navigate('/app/dms');
    } catch (error) {
      console.error('Failed to open DM:', errorMessage(error));
    }
    onClose();
  };

  const handleSendFriendRequest = () => {
    void friendReq.send();
    onClose();
  };

  const handleDisconnect = async () => {
    try {
      await disconnectVoiceParticipant(serverId, participant.userId);
    } catch (error) {
      console.error('Disconnect participant failed:', errorMessage(error));
    }
    onClose();
  };

  const handleMove = async (targetChannelId: string) => {
    try {
      await moveVoiceParticipant(serverId, participant.userId, targetChannelId);
    } catch (error) {
      console.error('Move participant failed:', errorMessage(error));
    }
    onClose();
  };

  return (
    <ContextMenu position={position} onClose={onClose}>
      <ContextMenu.Header>{participant.displayName || participant.username}</ContextMenu.Header>

      {/* Per-participant volume — only on the in-call tile surface, and never for self. */}
      {showVolumeControl && !isSelf && <ParticipantVolumeRow userId={participant.userId} />}

      <ContextMenu.Separator />

      {/* View Profile — always available */}
      {onViewProfile && (
        <ContextMenu.Item
          icon={profileIcon}
          label="View Profile"
          onClick={() => {
            onViewProfile();
            onClose();
          }}
        />
      )}

      {/* Send DM / Friend Request — hidden for self */}
      {!isSelf && <ContextMenu.Item icon={dmIcon} label="Send DM" onClick={handleSendDM} />}
      {!isSelf && (
        <ContextMenu.Item
          icon={friendIcon}
          label={friendReq.label}
          disabled={isFriend || hasPendingRequest}
          onClick={handleSendFriendRequest}
        />
      )}

      {/* Voice enforcement — mute/deafen (already voice-aware) */}
      {(canMute || canDeafen) && (
        <EnforcementMenuItems
          targetUserId={participant.userId}
          targetServerMuted={participant.serverMuted}
          targetServerDeafened={participant.serverDeafened}
          targetIsMuted={participant.isMuted}
          targetIsInVoice={true}
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

      {/* Move-to submenu + Disconnect — gated on MOVE_MEMBERS */}
      {canMove && (
        <>
          <ContextMenu.Separator />
          <div className="ctx-menu-item-wrapper">
            <ContextMenu.Item
              icon={<MoveRight size={16} />}
              label="Move to"
              hasSubMenu
              onClick={() => setShowMoveTargets((v) => !v)}
            />
            {showMoveTargets && (
              <ContextMenu.SubMenu>
                {moveTargets.length === 0 ? (
                  <ContextMenu.Item
                    icon={<span style={{ width: 16 }} />}
                    label="No other voice channels"
                    disabled
                    onClick={() => {}}
                  />
                ) : (
                  moveTargets.map((c) => (
                    <ContextMenu.Item
                      key={c.id}
                      icon={<span style={{ width: 16 }} />}
                      label={c.name}
                      onClick={() => handleMove(c.id)}
                    />
                  ))
                )}
              </ContextMenu.SubMenu>
            )}
          </div>
          <ContextMenu.Item
            icon={<PhoneOff size={16} />}
            label="Disconnect"
            danger
            onClick={handleDisconnect}
          />
        </>
      )}

      {/* Danger zone — kick/ban (only when host supplies handlers + perms) */}
      {(canKick || canBan) && <ContextMenu.Separator />}
      {canKick && (
        <ContextMenu.Item
          icon={kickIcon}
          label="Kick"
          danger
          onClick={() => {
            onKick?.();
            onClose();
          }}
        />
      )}
      {canBan && (
        <ContextMenu.Item
          icon={banIcon}
          label="Ban"
          danger
          onClick={() => {
            onBan?.();
            onClose();
          }}
        />
      )}
    </ContextMenu>
  );
}

export default VoiceParticipantContextMenu;
