import { useState, useCallback, useEffect, useRef } from 'react';
import AppLayout from '../Layout/AppLayout';
import ServerBar from '../Layout/ServerBar';
import FolderBar from '../Layout/FolderBar';
import ChannelPanel from '../Layout/ChannelPanel';
import ConversationList from './ConversationList';
import DMChatArea from './DMChatArea';
import FriendsFlexSpace from './FriendsFlexSpace';
import UserPanel from '../User/UserPanel';
import PersistentVoiceBar from '../Voice/PersistentVoiceBar';
import { errorMessage } from '../../utils/runtime/redactError';
import ServerActionModal from '../Servers/ServerActionModal';
import CreateServerModal from '../Servers/CreateServerModal';
import JoinServerModal from '../Servers/JoinServerModal';
import ServerContextMenu from '../Servers/ServerContextMenu';
import { selectSidebarDock, useLayoutStore } from '../../stores/ui/layoutStore';
import { useVoiceStore } from '../../stores/voice/voiceStore';
import { useDMStore } from '../../stores/chat/dmStore';
import { useAuthStore } from '../../stores/auth/authStore';
import { ServerWithRole } from '../../types/server';
import { DIRECT_MESSAGES_CONTEXT_AREA } from '../ui/ContextMenuProvider';
import DMThreadRemovalDialog, { type DMThreadRemovalTarget } from './DMThreadRemovalDialog';

const DirectMessagesView: React.FC = () => {
  const activeConversationId = useDMStore((s) => s.activeConversationId);
  const setActiveConversation = useDMStore((s) => s.setActiveConversation);

  const leftPinned = useLayoutStore((state) => selectSidebarDock(state, 'dm', 'left').pinned);
  const voiceActiveChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceConnectionState = useVoiceStore((s) => s.connectionState);
  const voiceIsDMCall = useVoiceStore((s) => s.isDMCall);
  const voiceDMConversationId = useVoiceStore((s) => s.dmConversationId);
  const isInVoice = !!(voiceActiveChannelId && voiceConnectionState === 'connected');
  const isViewingOwnDMCall =
    !!activeConversationId && voiceIsDMCall && voiceDMConversationId === activeConversationId;
  const showPersistentVoiceBar = isInVoice && !isViewingOwnDMCall;

  // Show floating avatar when channel panel is unpinned and no conversation is active
  // (when a conversation IS active, MessageInput provides the UserPanel)
  const showFloatingAvatar = !leftPinned && !activeConversationId;

  // Server modals (triggered by ServerBar add button or stale server placeholder)
  const [isServerActionModalOpen, setIsServerActionModalOpen] = useState(false);
  const [isCreateServerModalOpen, setIsCreateServerModalOpen] = useState(false);
  const [isJoinServerModalOpen, setIsJoinServerModalOpen] = useState(false);
  const [removalTarget, setRemovalTarget] = useState<DMThreadRemovalTarget | null>(null);
  const removalTargetRef = useRef<DMThreadRemovalTarget | null>(null);
  removalTargetRef.current = removalTarget;
  const focusAfterRemovalRef = useRef<string | null>(null);
  const authGeneration = useAuthStore((s) => s.authGeneration);

  useEffect(() => {
    const conversationId = focusAfterRemovalRef.current;
    if (removalTarget || conversationId === null) return;
    focusAfterRemovalRef.current = null;
    const inThreadTrigger =
      activeConversationId === conversationId
        ? document.querySelector<HTMLButtonElement>('.dm-chat-header-thread-actions-btn')
        : null;
    const survivingInvoker = Array.from(
      document.querySelectorAll<HTMLButtonElement>(
        `[data-context-area="${DIRECT_MESSAGES_CONTEXT_AREA}"] .conversation-item[data-conversation-id]`
      )
    ).find((element) => element.dataset.conversationId === conversationId);
    const search =
      document.querySelector<HTMLInputElement>(
        '.conversation-list:not(.conversation-list--compact) .conversation-search input'
      ) ??
      document.querySelector<HTMLButtonElement>(
        '.conversation-list--compact .conversation-search-trigger'
      );
    let focusTarget = search;
    if (survivingInvoker?.isConnected) focusTarget = survivingInvoker;
    if (inThreadTrigger?.isConnected) focusTarget = inThreadTrigger;
    focusTarget?.focus();
  }, [activeConversationId, removalTarget]);

  useEffect(() => {
    focusAfterRemovalRef.current = null;
    if (!removalTargetRef.current) return;
    // eslint-disable-next-line @eslint-react/set-state-in-effect -- a destructive target belongs only to its captured account
    setRemovalTarget(null);
  }, [authGeneration]);

  const requestRemoval = (target: DMThreadRemovalTarget) => {
    focusAfterRemovalRef.current = target.conversation.id;
    setRemovalTarget(target);
  };

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

  const channelHeader = (compact: boolean) => (
    <span style={{ fontWeight: 600, fontSize: 'calc(14px * var(--font-scale, 1))' }}>
      {compact ? 'DMs' : 'Direct Messages'}
    </span>
  );

  return (
    <div className="view-container main-view">
      <AppLayout
        context="dm"
        serverBar={
          <ServerBar
            onOpenActionModal={() => setIsServerActionModalOpen(true)}
            onContextMenu={handleServerContextMenu}
          />
        }
        folderBar={<FolderBar />}
        channelPanel={
          <ChannelPanel
            context="dm"
            renderHeader={channelHeader}
            renderContent={(compact) => (
              <ConversationList
                compact={compact}
                selectedThreadId={activeConversationId}
                onSelectThread={setActiveConversation}
              />
            )}
          />
        }
        chatArea={
          <div
            className="main-content"
            data-has-persistent-bar={showPersistentVoiceBar || undefined}
          >
            <DMChatArea selectedThreadId={activeConversationId} onRequestRemoval={requestRemoval} />
            {showPersistentVoiceBar && <PersistentVoiceBar />}
            {showFloatingAvatar && (
              <div className="floating-user-avatar">
                <UserPanel compact />
              </div>
            )}
          </div>
        }
        memberSpace={<FriendsFlexSpace onFriendClick={handleFriendClick} />}
      />

      {removalTarget && (
        <DMThreadRemovalDialog
          target={removalTarget}
          onClose={() => setRemovalTarget(null)}
          onRemoved={() => {
            setRemovalTarget(null);
          }}
        />
      )}

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
