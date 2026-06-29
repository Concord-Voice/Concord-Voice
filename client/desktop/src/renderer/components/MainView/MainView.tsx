import React, { useState, useRef, useCallback, useEffect, useMemo } from 'react';
import { useSettingsOverlayStore } from '../../stores/settingsOverlayStore';
import { PipSignalingProxy } from '../../services/pipSignalingProxy';
import { createResizeKeyHandler } from '../../utils/resizeKeyboard';
import ChannelList from '../Channels/ChannelList';
import ServerActionBar from '../Channels/ServerActionBar';
import MainViewModals from './MainViewModals';
import MainViewContextMenus from './MainViewContextMenus';
import ServerE2eeIndicator from './ServerE2eeIndicator';
import ConnectionStatus from '../ConnectionStatus/ConnectionStatus';
import { ChatView } from '../Chat';
import VoiceView from '../Voice/VoiceView';
import PersistentVoiceBar from '../Voice/PersistentVoiceBar';
import VoiceTextChat from '../Voice/VoiceTextChat';
import UserPanel from '../User/UserPanel';
import { useVoiceStore } from '../../stores/voiceStore';
import { useLayoutStore } from '../../stores/layoutStore';

// New layout components
import AppLayout from '../Layout/AppLayout';
import ServerBar from '../Layout/ServerBar';
import FolderBar from '../Layout/FolderBar';
import ChannelPanel from '../Layout/ChannelPanel';
import MemberFlexSpace from '../Layout/MemberFlexSpace';

import { useServerStore } from '../../stores/serverStore';
import { useChannelStore } from '../../stores/channelStore';
import { usePermissionStore } from '../../stores/permissionStore';
import { Permissions, hasPermission } from '../../utils/permissions';
import { useServerChannelSubscriptions } from '../../hooks/useServerChannelSubscriptions';
import { useKeyboardShortcuts } from '../../hooks/useKeyboardShortcuts';
import { useDNDTransitionRefresh } from '../../hooks/useDNDTransitionRefresh';
import ShortcutOverlay from '../ui/ShortcutOverlay';
import ChannelSwitcher from '../ui/ChannelSwitcher';
import { ServerWithRole } from '../../types/server';
import { Channel, ChannelGroup } from '../../types/chat';
import './MainView.css';

interface MainPrimaryContentProps {
  activeServer: ServerWithRole | null;
  channelCount: number;
  activeChannel: Channel | undefined;
  activeChannelId: string | null;
}

/**
 * Selects which view fills the chat area based on the currently-active server +
 * channel. Extracted from MainView so MainView's cognitive complexity stays
 * below the rule threshold; each branch here is one early-return so this
 * helper itself is shallow.
 */
const MainPrimaryContent: React.FC<MainPrimaryContentProps> = ({
  activeServer,
  channelCount,
  activeChannel,
  activeChannelId,
}) => {
  if (activeServer && channelCount === 0) {
    return (
      <div className="main-content-placeholder">
        <div className="empty-server-graphic">
          <svg
            width="48"
            height="48"
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.5"
            strokeLinecap="round"
            strokeLinejoin="round"
          >
            <path d="M21 15a2 2 0 0 1-2 2H7l-4 4V5a2 2 0 0 1 2-2h14a2 2 0 0 1 2 2z" />
            <line x1="12" y1="8" x2="12" y2="12" />
            <line x1="10" y1="10" x2="14" y2="10" />
          </svg>
        </div>
        <h2>This server appears empty.</h2>
        <p className="empty-server-hint">Add some channels to start chatting!</p>
      </div>
    );
  }
  if (activeChannel?.type === 'voice') {
    return <VoiceView channelId={activeChannel.id} channelName={activeChannel.name} />;
  }
  if (activeChannelId) {
    return <ChatView />;
  }
  if (activeServer) {
    return (
      <div className="main-content-placeholder">
        <h2>{activeServer.name}</h2>
        <p className="empty-server-hint">Select a channel to start chatting.</p>
      </div>
    );
  }
  return (
    <div className="main-content-placeholder">
      <h2>Welcome to Concord Voice</h2>
      <p>Privacy-first, self-hostable voice communication.</p>
    </div>
  );
};

/**
 * Owns the PiP signaling proxy lifecycle: created when the user is connected to
 * voice, disposed on disconnect/unmount. Extracted from MainView so the async
 * import + cleanup branching lives in a dedicated hook rather than inflating
 * MainView's cognitive complexity.
 */
const usePipSignalingProxy = (isInVoice: boolean): void => {
  useEffect(() => {
    if (!isInVoice) return;
    let cancelled = false;
    let proxy: PipSignalingProxy | null = null;
    // Capture voiceService ref synchronously for cleanup (avoids async race)
    let capturedVoiceService:
      | (typeof import('../../services/voiceService'))['voiceService']
      | null = null;

    import('../../services/voiceService')
      .then(({ voiceService }) => {
        if (cancelled) return;
        capturedVoiceService = voiceService;
        // eslint-disable-next-line @typescript-eslint/no-explicit-any -- PipSignalingProxy's voiceService parameter expects an internal interface that isn't exported from voiceService's public module; typing it would require re-exporting internal mediasoup handler types
        proxy = new PipSignalingProxy(voiceService as any);

        // Wire producer lifecycle events so PiP windows learn about new/removed producers
        voiceService.onProducerAdded = (pid: string, uid: string, src: string) => {
          proxy?.broadcastProducerAdded(pid, uid, src);
        };
        voiceService.onProducerClosed = (pid: string, uid: string) => {
          proxy?.broadcastProducerClosed(pid, uid);
        };

        const cleanup = globalThis.electron?.onPipClosed?.((pipId: string) => {
          proxy?.onPipClosed(pipId);
        });

        (proxy as PipSignalingProxy & { _ipcCleanup?: () => void })._ipcCleanup = cleanup;
      })
      .catch(() => {
        /* voiceService import failed — nothing to proxy */
      });

    return () => {
      cancelled = true;
      if (proxy) {
        // eslint-disable-next-line @typescript-eslint/no-explicit-any -- `_ipcCleanup` is a private cleanup-callback slot stored on the proxy instance via `(proxy as PipSignalingProxy & { _ipcCleanup? })` at attach time; optional chaining here handles the case where the IPC bridge was never registered
        (proxy as any)._ipcCleanup?.();
        proxy.dispose();
      }
      // Clear producer lifecycle callbacks synchronously to avoid race
      if (capturedVoiceService) {
        capturedVoiceService.onProducerAdded = null;
        capturedVoiceService.onProducerClosed = null;
      }
    };
  }, [isInVoice]);
};

const MainView: React.FC = () => {
  // Subscribe to ALL channels in the active server for unread tracking
  useServerChannelSubscriptions();
  useKeyboardShortcuts();
  // When the user turns DND off, refetch unread counts for the active
  // server so stale state from the suppressed-notify window resolves.
  useDNDTransitionRefresh();

  const [isServerActionModalOpen, setIsServerActionModalOpen] = useState(false);
  const [isCreateServerModalOpen, setIsCreateServerModalOpen] = useState(false);
  const [isJoinServerModalOpen, setIsJoinServerModalOpen] = useState(false);
  const [isCreateChannelModalOpen, setIsCreateChannelModalOpen] = useState(false);
  const [isCreateCategoryModalOpen, setIsCreateCategoryModalOpen] = useState(false);
  const [emptyContextMenu, setEmptyContextMenu] = useState<{
    position: { x: number; y: number };
  } | null>(null);
  const [serverContextMenu, setServerContextMenu] = useState<{
    server: ServerWithRole;
    position: { x: number; y: number };
  } | null>(null);
  const [channelContextMenu, setChannelContextMenu] = useState<{
    channel: Channel;
    position: { x: number; y: number };
  } | null>(null);
  const [deletingServer, setDeletingServer] = useState<ServerWithRole | null>(null);
  const [leavingServer, setLeavingServer] = useState<ServerWithRole | null>(null);
  const [editingChannel, setEditingChannel] = useState<Channel | null>(null);
  const [deletingChannel, setDeletingChannel] = useState<Channel | null>(null);
  const [categoryContextMenu, setCategoryContextMenu] = useState<{
    group: ChannelGroup;
    position: { x: number; y: number };
  } | null>(null);
  const [editingCategory, setEditingCategory] = useState<ChannelGroup | null>(null);
  const [deletingCategory, setDeletingCategory] = useState<ChannelGroup | null>(null);
  const [invitingServer, setInvitingServer] = useState<ServerWithRole | null>(null);
  const [channelPermissions, setChannelPermissions] = useState<Channel | null>(null);
  const [categoryPermissions, setCategoryPermissions] = useState<ChannelGroup | null>(null);

  const servers = useServerStore((state) => state.servers);
  const activeServerId = useServerStore((state) => state.activeServerId);
  const setActiveServer = useServerStore((state) => state.setActiveServer);
  const activeServer = servers.find((s) => s.id === activeServerId) || null;
  const activeChannelId = useChannelStore((state) => state.activeChannelId);
  const channels = useChannelStore((state) => state.channels);
  const setActiveChannel = useChannelStore((state) => state.setActiveChannel);
  const activeChannel = channels.find((c) => c.id === activeChannelId);
  const voiceActiveChannelId = useVoiceStore((s) => s.activeChannelId);
  const voiceConnectionState = useVoiceStore((s) => s.connectionState);
  const voiceShowTextChat = useVoiceStore((s) => s.showVoiceTextChat);
  const voiceTextChatLayout = useVoiceStore((s) => s.voiceTextChatLayout);
  const voiceTextChatWidth = useVoiceStore((s) => s.voiceTextChatWidth);
  const setVoiceTextChatWidth = useVoiceStore((s) => s.setVoiceTextChatWidth);
  const voiceControlsPinned = useVoiceStore((s) => s.voiceControlsPinned);
  const channelPanelPinned = useLayoutStore((s) => s.channelPanelPinned);
  const getLinkedTextChannel = useChannelStore((s) => s.getLinkedTextChannel);
  const fetchServerPermissions = usePermissionStore((s) => s.fetchServerPermissions);
  const activeServerPerms = usePermissionStore(
    (s) => (activeServerId ? s.serverPermissions[activeServerId] : undefined) ?? 0n
  );
  const canManageChannels = hasPermission(activeServerPerms, Permissions.MANAGE_CHANNELS);

  // Fetch permissions when active server changes
  useEffect(() => {
    if (activeServerId) {
      fetchServerPermissions(activeServerId);
    }
  }, [activeServerId, fetchServerPermissions]);

  const handleCreateServerSuccess = (server: ServerWithRole) => {
    setActiveServer(server.id);
  };

  const handleCreateChannelSuccess = (channel: Channel) => {
    setActiveChannel(channel.id);
  };

  const handleServerContextMenu = (server: ServerWithRole, position: { x: number; y: number }) => {
    setServerContextMenu({ server, position });
  };

  const handleEditServer = (server: ServerWithRole) => {
    setServerContextMenu(null);
    useSettingsOverlayStore.getState().openSettings('server', { serverId: server.id });
  };

  const handleDeleteServer = (server: ServerWithRole) => {
    setServerContextMenu(null);
    setDeletingServer(server);
  };

  const handleInviteServer = (server: ServerWithRole) => {
    setServerContextMenu(null);
    setInvitingServer(server);
  };

  const handleLeaveServer = (server: ServerWithRole) => {
    setServerContextMenu(null);
    setLeavingServer(server);
  };

  const handleChannelContextMenu = (channel: Channel, position: { x: number; y: number }) => {
    setChannelContextMenu({ channel, position });
  };

  const handleEditChannel = (channel: Channel) => {
    setChannelContextMenu(null);
    setEditingChannel(channel);
  };

  const handleCategoryContextMenu = (group: ChannelGroup, position: { x: number; y: number }) => {
    setCategoryContextMenu({ group, position });
  };

  // ─── Channel panel header content ───
  const channelHeader = (
    <div className="channel-header-info">
      <div className="channel-header-name-row">
        <h3>{activeServer ? activeServer.name : 'Channels'}</h3>
        {activeServer && <ServerE2eeIndicator />}
      </div>
      <ConnectionStatus />
    </div>
  );

  // ─── Voice & layout detection ───
  // Include 'reconnecting' so voice UI and PiP proxy survive transient
  // network blips — only tear them down on an explicit disconnect/reset.
  const isInVoice =
    Boolean(voiceActiveChannelId) &&
    (voiceConnectionState === 'connected' || voiceConnectionState === 'reconnecting');
  const isViewingOwnVoiceChannel =
    activeChannel?.type === 'voice' && activeChannel.id === voiceActiveChannelId;
  const showPersistentBar = !!(isInVoice && !isViewingOwnVoiceChannel);

  // PiP signaling proxy: create when connected to voice, dispose when disconnected.
  // Kept at MainView level so it persists across navigation (VoiceView unmounts on nav).
  usePipSignalingProxy(isInVoice);

  // Vertical voice text chat panel — rendered as a side column in MainView
  // instead of inside PersistentVoiceBar
  const voiceLinkedText = voiceActiveChannelId ? getLinkedTextChannel(voiceActiveChannelId) : null;
  const showVerticalSideChat = !!(
    showPersistentBar &&
    voiceControlsPinned &&
    voiceShowTextChat &&
    voiceLinkedText &&
    voiceTextChatLayout === 'vertical'
  );

  const sideResizingRef = useRef(false);
  const mainContentRef = useRef<HTMLDivElement>(null);

  const handleSideChatResizeStart = useCallback(
    (e: React.MouseEvent) => {
      e.preventDefault();
      sideResizingRef.current = true;
      const startX = e.clientX;
      const startWidth = voiceTextChatWidth;

      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';

      const onMouseMove = (moveEvent: MouseEvent) => {
        if (!sideResizingRef.current || !mainContentRef.current) return;
        const rect = mainContentRef.current.getBoundingClientRect();
        const maxWidth = rect.width * 0.5;
        const delta = startX - moveEvent.clientX;
        const newWidth = Math.min(maxWidth, Math.max(250, startWidth + delta));
        setVoiceTextChatWidth(newWidth);
      };

      const onMouseUp = () => {
        sideResizingRef.current = false;
        document.body.style.cursor = '';
        document.body.style.userSelect = '';
        document.removeEventListener('mousemove', onMouseMove);
        document.removeEventListener('mouseup', onMouseUp);
      };

      document.addEventListener('mousemove', onMouseMove);
      document.addEventListener('mouseup', onMouseUp);
    },
    [voiceTextChatWidth, setVoiceTextChatWidth]
  );

  const handleSideChatResizeKeyDown = useMemo(
    () =>
      createResizeKeyHandler({
        axis: 'horizontal',
        direction: 'shrink',
        min: 250,
        max: 600,
        getValue: () => voiceTextChatWidth,
        setValue: setVoiceTextChatWidth,
      }),
    [voiceTextChatWidth, setVoiceTextChatWidth]
  );

  // Determine if any MessageInput (which renders compact UserPanel) is on screen.
  // VoiceTextChat contains a MessageInput that shows compact UserPanel when channel panel is unpinned.
  const voiceLinkedTextExists = !!voiceLinkedText;
  const voiceTextChatVisible =
    isInVoice &&
    voiceShowTextChat &&
    voiceLinkedTextExists &&
    (isViewingOwnVoiceChannel || (showPersistentBar && voiceControlsPinned));
  const hasVisibleMessageInput =
    (activeChannel?.type === 'text' && !!activeChannelId) || voiceTextChatVisible;
  const showFloatingAvatar = !channelPanelPinned && !hasVisibleMessageInput;

  // ─── Channel panel body content ───
  const channelBody = (
    <>
      {activeServer && (
        <ServerActionBar
          server={activeServer}
          onOpenCreateModal={() => setIsCreateChannelModalOpen(true)}
          onOpenCreateCategoryModal={() => setIsCreateCategoryModalOpen(true)}
          onOpenSettings={() =>
            activeServer &&
            useSettingsOverlayStore.getState().openSettings('server', { serverId: activeServer.id })
          }
        />
      )}
      <ChannelList
        onContextMenu={handleChannelContextMenu}
        onEmptyContextMenu={(pos) => setEmptyContextMenu({ position: pos })}
        onCategoryContextMenu={handleCategoryContextMenu}
      />
    </>
  );

  // ─── Chat area content ───
  const chatContent = (
    <div
      ref={mainContentRef}
      className="main-content"
      data-has-persistent-bar={showPersistentBar || undefined}
    >
      <div
        className={`main-content__body ${showVerticalSideChat ? 'main-content__body--with-side-chat' : ''}`}
      >
        <div className="main-content__primary">
          <MainPrimaryContent
            activeServer={activeServer}
            channelCount={channels.length}
            activeChannel={activeChannel}
            activeChannelId={activeChannelId}
          />

          {/* Persistent voice controls bar — shown when in voice but viewing another channel */}
          {showPersistentBar && <PersistentVoiceBar />}
        </div>

        {/* Vertical voice text chat side panel */}
        {showVerticalSideChat && (
          <>
            <button
              type="button"
              className="main-content__side-resize"
              onMouseDown={handleSideChatResizeStart}
              onKeyDown={handleSideChatResizeKeyDown}
              tabIndex={0}
              aria-label="Resize voice text chat"
            >
              <div className="main-content__side-resize-grip" />
            </button>
            <div className="main-content__side-chat" style={{ width: voiceTextChatWidth }}>
              <VoiceTextChat />
            </div>
          </>
        )}
      </div>

      {/* Floating user avatar — shown when channel panel is unpinned and no message input visible */}
      {showFloatingAvatar && (
        <div className="floating-user-avatar">
          <UserPanel compact />
        </div>
      )}
    </div>
  );

  return (
    <div className="view-container main-view">
      <AppLayout
        forceChannelPin={!!activeServer && channels.length === 0}
        serverBar={
          <ServerBar
            onOpenActionModal={() => setIsServerActionModalOpen(true)}
            onContextMenu={handleServerContextMenu}
          />
        }
        folderBar={<FolderBar />}
        channelPanel={
          <ChannelPanel header={channelHeader} forcePin={!!activeServer && channels.length === 0}>
            {channelBody}
          </ChannelPanel>
        }
        chatArea={chatContent}
        memberSpace={<MemberFlexSpace />}
      />

      {/* ─── Modals (z-index overlays, unaffected by layout) ─── */}

      <MainViewModals
        isServerActionModalOpen={isServerActionModalOpen}
        setIsServerActionModalOpen={setIsServerActionModalOpen}
        isCreateServerModalOpen={isCreateServerModalOpen}
        setIsCreateServerModalOpen={setIsCreateServerModalOpen}
        isJoinServerModalOpen={isJoinServerModalOpen}
        setIsJoinServerModalOpen={setIsJoinServerModalOpen}
        isCreateChannelModalOpen={isCreateChannelModalOpen}
        setIsCreateChannelModalOpen={setIsCreateChannelModalOpen}
        isCreateCategoryModalOpen={isCreateCategoryModalOpen}
        setIsCreateCategoryModalOpen={setIsCreateCategoryModalOpen}
        deletingServer={deletingServer}
        setDeletingServer={setDeletingServer}
        leavingServer={leavingServer}
        setLeavingServer={setLeavingServer}
        editingChannel={editingChannel}
        setEditingChannel={setEditingChannel}
        deletingChannel={deletingChannel}
        setDeletingChannel={setDeletingChannel}
        invitingServer={invitingServer}
        setInvitingServer={setInvitingServer}
        editingCategory={editingCategory}
        setEditingCategory={setEditingCategory}
        deletingCategory={deletingCategory}
        setDeletingCategory={setDeletingCategory}
        channelPermissions={channelPermissions}
        setChannelPermissions={setChannelPermissions}
        categoryPermissions={categoryPermissions}
        setCategoryPermissions={setCategoryPermissions}
        activeServer={activeServer}
        onCreateServerSuccess={handleCreateServerSuccess}
        onCreateChannelSuccess={handleCreateChannelSuccess}
      />

      <MainViewContextMenus
        serverContextMenu={serverContextMenu}
        setServerContextMenu={setServerContextMenu}
        channelContextMenu={channelContextMenu}
        setChannelContextMenu={setChannelContextMenu}
        categoryContextMenu={categoryContextMenu}
        setCategoryContextMenu={setCategoryContextMenu}
        emptyContextMenu={emptyContextMenu}
        setEmptyContextMenu={setEmptyContextMenu}
        activeServer={activeServer}
        canManageChannels={canManageChannels}
        onEditServer={handleEditServer}
        onDeleteServer={handleDeleteServer}
        onLeaveServer={handleLeaveServer}
        onInviteServer={handleInviteServer}
        onEditChannel={handleEditChannel}
        onDeleteChannel={(channel) => {
          setChannelContextMenu(null);
          setDeletingChannel(channel);
        }}
        onChannelPermissions={(channel) => {
          setChannelContextMenu(null);
          setChannelPermissions(channel);
        }}
        onEditCategory={(group) => {
          setCategoryContextMenu(null);
          setEditingCategory(group);
        }}
        onDeleteCategory={(group) => {
          setCategoryContextMenu(null);
          setDeletingCategory(group);
        }}
        onCategoryPermissions={(group) => {
          setCategoryContextMenu(null);
          setCategoryPermissions(group);
        }}
        onOpenCreateChannelModal={() => setIsCreateChannelModalOpen(true)}
        onOpenCreateCategoryModal={() => setIsCreateCategoryModalOpen(true)}
      />

      <ShortcutOverlay />
      <ChannelSwitcher />
    </div>
  );
};

export default MainView;
