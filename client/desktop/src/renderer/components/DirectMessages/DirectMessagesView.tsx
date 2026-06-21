import { useState, useCallback } from 'react';
import AppLayout from '../Layout/AppLayout';
import ServerBar from '../Layout/ServerBar';
import FolderBar from '../Layout/FolderBar';
import ChannelPanel from '../Layout/ChannelPanel';
import ConversationList from './ConversationList';
import DMChatArea from './DMChatArea';
import FriendsFlexSpace from './FriendsFlexSpace';
import UserPanel from '../User/UserPanel';
import PersistentVoiceBar from '../Voice/PersistentVoiceBar';
import { errorMessage } from '../../utils/redactError';
import ServerActionModal from '../Servers/ServerActionModal';
import CreateServerModal from '../Servers/CreateServerModal';
import JoinServerModal from '../Servers/JoinServerModal';
import ServerContextMenu from '../Servers/ServerContextMenu';
import { useLayoutStore } from '../../stores/layoutStore';
import { useVoiceStore } from '../../stores/voiceStore';
import { useDMStore } from '../../stores/dmStore';
import { ServerWithRole } from '../../types/server';

const DirectMessagesView: React.FC = () => {
  const activeConversationId = useDMStore((s) => s.activeConversationId);
  const setActiveConversation = useDMStore((s) => s.setActiveConversation);

  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  const voiceActiveChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceConnectionState = useVoiceStore((s) => s.connectionState);
  const isInVoice = !!(voiceActiveChannelId && voiceConnectionState === 'connected');

  // Show floating avatar when channel panel is unpinned and no conversation is active
  // (when a conversation IS active, MessageInput provides the UserPanel)
  const showFloatingAvatar = !channelPanelPinned && !activeConversationId;

  // Server modals (triggered by ServerBar add button or stale server placeholder)
  const [isServerActionModalOpen, setIsServerActionModalOpen] = useState(false);
  const [isCreateServerModalOpen, setIsCreateServerModalOpen] = useState(false);
  const [isJoinServerModalOpen, setIsJoinServerModalOpen] = useState(false);

  // Server context menu
  const [contextMenu, setContextMenu] = useState<{
    server: ServerWithRole;
    position: { x: number; y: number };
  } | null>(null);

  const handleServerContextMenu = (server: ServerWithRole, position: { x: number; y: number }) => {
    setContextMenu({ server, position });
  };

  const handleCreateServerSuccess = () => {
    setIsCreateServerModalOpen(false);
    setIsJoinServerModalOpen(false);
  };

  const handleFriendClick = useCallback(
    async (userId: string) => {
      try {
        const conv = await useDMStore.getState().openDM(userId);
        setActiveConversation(conv.id);
      } catch (err) {
        console.error('Failed to open DM:', errorMessage(err));
      }
    },
    [setActiveConversation]
  );

  const channelHeader = (
    <span style={{ fontWeight: 600, fontSize: 'calc(14px * var(--font-scale, 1))' }}>
      Direct Messages
    </span>
  );

  return (
    <div className="view-container main-view">
      <AppLayout
        serverBar={
          <ServerBar
            onOpenActionModal={() => setIsServerActionModalOpen(true)}
            onContextMenu={handleServerContextMenu}
          />
        }
        folderBar={<FolderBar />}
        channelPanel={
          <ChannelPanel header={channelHeader}>
            <ConversationList
              selectedThreadId={activeConversationId}
              onSelectThread={setActiveConversation}
            />
          </ChannelPanel>
        }
        chatArea={
          <div className="main-content" data-has-persistent-bar={isInVoice || undefined}>
            <DMChatArea selectedThreadId={activeConversationId} />
            {isInVoice && <PersistentVoiceBar />}
            {showFloatingAvatar && (
              <div className="floating-user-avatar">
                <UserPanel compact />
              </div>
            )}
          </div>
        }
        memberSpace={<FriendsFlexSpace onFriendClick={handleFriendClick} />}
        forceMemberExpanded
      />

      {/* Server management modals */}
      <ServerActionModal
        isOpen={isServerActionModalOpen}
        onClose={() => setIsServerActionModalOpen(false)}
        onCreateServer={() => setIsCreateServerModalOpen(true)}
        onJoinServer={() => setIsJoinServerModalOpen(true)}
      />
      <CreateServerModal
        isOpen={isCreateServerModalOpen}
        onClose={() => setIsCreateServerModalOpen(false)}
        onSuccess={handleCreateServerSuccess}
      />
      <JoinServerModal
        isOpen={isJoinServerModalOpen}
        onClose={() => setIsJoinServerModalOpen(false)}
        onSuccess={handleCreateServerSuccess}
      />

      {/* Server context menu */}
      {contextMenu && (
        <ServerContextMenu
          server={contextMenu.server}
          position={contextMenu.position}
          onClose={() => setContextMenu(null)}
          onEditServer={() => setContextMenu(null)}
          onDeleteServer={() => setContextMenu(null)}
          onLeaveServer={() => setContextMenu(null)}
          onInvite={() => setContextMenu(null)}
        />
      )}
    </div>
  );
};

export default DirectMessagesView;
