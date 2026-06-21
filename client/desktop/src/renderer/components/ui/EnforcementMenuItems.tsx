import { MicOff, HeadphoneOff, Lock } from 'lucide-react';
import ContextMenu from './ContextMenu';
import { apiFetch } from '../../services/apiClient';
import { errorMessage } from '../../utils/redactError';
import './EnforcementMenuItems.css';

interface ServerContext {
  type: 'server';
  serverId: string;
  canMute: boolean;
  canDeafen: boolean;
  canModerate: boolean;
}

interface DM1on1Context {
  type: 'dm_1on1';
  conversationId: string;
}

interface DMGroupContext {
  type: 'dm_group';
  conversationId: string;
  isAdmin: boolean;
}

type EnforcementContext = ServerContext | DM1on1Context | DMGroupContext;

export interface EnforcementMenuItemsProps {
  targetUserId: string;
  targetServerMuted: boolean;
  targetServerDeafened: boolean;
  targetIsMuted: boolean;
  targetIsInVoice: boolean;
  context: EnforcementContext;
  onClose: () => void;
}

function getIconClass(active: boolean, hard: boolean): string {
  if (!active) return 'ctx-enforcement-icon--inactive';
  if (hard) return 'ctx-enforcement-icon--active-hard';
  return 'ctx-enforcement-icon--active-user';
}

function EnforcementIcon({
  icon: Icon,
  active,
  hard,
}: Readonly<{
  icon: typeof MicOff;
  active: boolean;
  hard: boolean;
}>) {
  return (
    <span className="ctx-enforcement-icon-group">
      <Icon size={16} className={getIconClass(active, hard)} />
      {hard && <Lock size={8} className="ctx-enforcement-lock" />}
    </span>
  );
}

interface ItemVisibility {
  userMute: boolean;
  userDeafen: boolean;
  hardMute: boolean;
  hardDeafen: boolean;
}

function computeVisibility(context: EnforcementContext, targetIsInVoice: boolean): ItemVisibility {
  if (context.type === 'server') {
    return {
      userMute: targetIsInVoice && context.canMute,
      userDeafen: targetIsInVoice && context.canDeafen,
      hardMute: context.canMute && context.canModerate,
      hardDeafen: context.canDeafen && context.canModerate,
    };
  }
  if (context.type === 'dm_group') {
    return {
      userMute: targetIsInVoice,
      userDeafen: false,
      hardMute: context.isAdmin,
      hardDeafen: context.isAdmin,
    };
  }
  // dm_1on1
  return {
    userMute: targetIsInVoice,
    userDeafen: false,
    hardMute: false,
    hardDeafen: false,
  };
}

function buildBasePath(context: EnforcementContext, targetUserId: string): string {
  if (context.type === 'server') {
    return `/api/v1/servers/${context.serverId}/voice/${targetUserId}`;
  }
  return `/api/v1/dm/conversations/${context.conversationId}/voice/${targetUserId}`;
}

export function EnforcementMenuItems({
  targetUserId,
  targetServerMuted,
  targetServerDeafened,
  targetIsMuted,
  targetIsInVoice,
  context,
  onClose,
}: Readonly<EnforcementMenuItemsProps>) {
  const handleAction = async (path: string, method: string) => {
    try {
      await apiFetch(path, { method });
    } catch (err) {
      console.error('Enforcement action failed', errorMessage(err));
    }
    onClose();
  };

  const vis = computeVisibility(context, targetIsInVoice);

  if (!vis.userMute && !vis.userDeafen && !vis.hardMute && !vis.hardDeafen) {
    return null;
  }

  const basePath = buildBasePath(context, targetUserId);

  return (
    <>
      <ContextMenu.Separator />

      {/* User-level (soft) actions */}
      {vis.userMute && (
        <ContextMenu.Item
          icon={<EnforcementIcon icon={MicOff} active={targetIsMuted} hard={false} />}
          label="Mute"
          onClick={() => handleAction(`${basePath}/user-mute`, 'POST')}
        />
      )}
      {vis.userDeafen && (
        <ContextMenu.Item
          icon={<EnforcementIcon icon={HeadphoneOff} active={false} hard={false} />}
          label="Deafen"
          onClick={() => handleAction(`${basePath}/user-deafen`, 'POST')}
        />
      )}

      {/* Server-wide (hard) enforcement */}
      {(vis.hardMute || vis.hardDeafen) && <ContextMenu.Separator />}
      {vis.hardMute && (
        <ContextMenu.Item
          icon={<EnforcementIcon icon={MicOff} active={targetServerMuted} hard={true} />}
          label={targetServerMuted ? 'Remove Server Mute' : 'Server Mute'}
          onClick={() => handleAction(`${basePath}/mute`, targetServerMuted ? 'DELETE' : 'POST')}
        />
      )}
      {vis.hardDeafen && (
        <ContextMenu.Item
          icon={<EnforcementIcon icon={HeadphoneOff} active={targetServerDeafened} hard={true} />}
          label={targetServerDeafened ? 'Remove Server Deafen' : 'Server Deafen'}
          onClick={() =>
            handleAction(`${basePath}/deafen`, targetServerDeafened ? 'DELETE' : 'POST')
          }
        />
      )}
    </>
  );
}
