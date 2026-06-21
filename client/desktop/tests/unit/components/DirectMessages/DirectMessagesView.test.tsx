import { render, screen, fireEvent } from '../../../test-utils';
import { resetAllStores } from '../../../helpers/store-helpers';
import { useDMStore } from '@/renderer/stores/dmStore';
import { useLayoutStore } from '@/renderer/stores/layoutStore';
import { useVoiceStore } from '@/renderer/stores/voiceStore';
import { vi } from 'vitest';

// Mock all heavy child components
vi.mock('@/renderer/components/Layout/AppLayout', () => ({
  default: ({
    serverBar,
    folderBar,
    channelPanel,
    chatArea,
    memberSpace,
  }: {
    serverBar: React.ReactNode;
    folderBar: React.ReactNode;
    channelPanel: React.ReactNode;
    chatArea: React.ReactNode;
    memberSpace: React.ReactNode;
  }) => (
    <div data-testid="app-layout">
      <div data-testid="server-bar-slot">{serverBar}</div>
      <div data-testid="folder-bar-slot">{folderBar}</div>
      <div data-testid="channel-panel-slot">{channelPanel}</div>
      <div data-testid="chat-area-slot">{chatArea}</div>
      <div data-testid="member-space-slot">{memberSpace}</div>
    </div>
  ),
}));

vi.mock('@/renderer/components/Layout/ServerBar', () => ({
  default: ({ onOpenActionModal }: { onOpenActionModal: () => void }) => (
    <div data-testid="server-bar">
      <button onClick={onOpenActionModal}>Open Action Modal</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/Layout/FolderBar', () => ({
  default: () => <div data-testid="folder-bar" />,
}));

vi.mock('@/renderer/components/Layout/ChannelPanel', () => ({
  default: ({ header, children }: { header: React.ReactNode; children: React.ReactNode }) => (
    <div data-testid="channel-panel">
      <div data-testid="channel-panel-header">{header}</div>
      {children}
    </div>
  ),
}));

vi.mock('@/renderer/components/DirectMessages/ConversationList', () => ({
  default: ({
    selectedThreadId,
    onSelectThread,
  }: {
    selectedThreadId: string | null;
    onSelectThread: (id: string) => void;
  }) => (
    <div data-testid="conversation-list" data-selected={selectedThreadId}>
      <button onClick={() => onSelectThread('conv-1')}>Select Thread</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/DirectMessages/DMChatArea', () => ({
  default: ({ selectedThreadId }: { selectedThreadId: string | null }) => (
    <div data-testid="dm-chat-area" data-thread={selectedThreadId} />
  ),
}));

vi.mock('@/renderer/components/DirectMessages/FriendsFlexSpace', () => ({
  default: ({ onFriendClick }: { onFriendClick?: (userId: string) => void }) => (
    <div data-testid="friends-flex-space">
      <button onClick={() => onFriendClick?.('friend-user-1')}>Click Friend</button>
    </div>
  ),
}));

vi.mock('@/renderer/components/User/UserPanel', () => ({
  default: ({ compact }: { compact?: boolean }) => (
    <div data-testid="user-panel" data-compact={compact} />
  ),
}));

vi.mock('@/renderer/components/Voice/PersistentVoiceBar', () => ({
  default: () => <div data-testid="persistent-voice-bar" />,
}));

vi.mock('@/renderer/components/Servers/ServerActionModal', () => ({
  default: ({
    isOpen,
    onClose,
    onCreateServer,
    onJoinServer,
  }: {
    isOpen: boolean;
    onClose: () => void;
    onCreateServer: () => void;
    onJoinServer: () => void;
  }) =>
    isOpen ? (
      <div data-testid="server-action-modal">
        <button onClick={onClose}>Close Action</button>
        <button onClick={onCreateServer}>Create Server</button>
        <button onClick={onJoinServer}>Join Server</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/components/Servers/CreateServerModal', () => ({
  default: ({
    isOpen,
    onClose,
    onSuccess,
  }: {
    isOpen: boolean;
    onClose: () => void;
    onSuccess: () => void;
  }) =>
    isOpen ? (
      <div data-testid="create-server-modal">
        <button onClick={onClose}>Close Create</button>
        <button onClick={onSuccess}>Create Success</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/components/Servers/JoinServerModal', () => ({
  default: ({
    isOpen,
    onClose,
    onSuccess,
  }: {
    isOpen: boolean;
    onClose: () => void;
    onSuccess: () => void;
  }) =>
    isOpen ? (
      <div data-testid="join-server-modal">
        <button onClick={onClose}>Close Join</button>
        <button onClick={onSuccess}>Join Success</button>
      </div>
    ) : null,
}));

vi.mock('@/renderer/components/Servers/ServerContextMenu', () => ({
  default: () => <div data-testid="server-context-menu" />,
}));

import DirectMessagesView from '@/renderer/components/DirectMessages/DirectMessagesView';

describe('DirectMessagesView', () => {
  beforeEach(() => {
    resetAllStores();
    vi.clearAllMocks();
    useDMStore.setState({
      activeConversationId: null,
      setActiveConversation: vi.fn(),
      conversations: [],
      openDM: vi.fn().mockResolvedValue({ id: 'dm-conv-1' }),
    });
    useLayoutStore.setState({ channelPanelPinned: true });
    useVoiceStore.setState({
      activeChannelId: null,
      connectionState: 'disconnected',
    });
  });

  // --- Basic Layout ---

  it('renders the AppLayout with all slots', () => {
    render(<DirectMessagesView />);
    expect(screen.getByTestId('app-layout')).toBeInTheDocument();
    expect(screen.getByTestId('server-bar-slot')).toBeInTheDocument();
    expect(screen.getByTestId('folder-bar-slot')).toBeInTheDocument();
    expect(screen.getByTestId('channel-panel-slot')).toBeInTheDocument();
    expect(screen.getByTestId('chat-area-slot')).toBeInTheDocument();
    expect(screen.getByTestId('member-space-slot')).toBeInTheDocument();
  });

  it('renders "Direct Messages" in channel panel header', () => {
    render(<DirectMessagesView />);
    expect(screen.getByText('Direct Messages')).toBeInTheDocument();
  });

  it('renders ConversationList in channel panel', () => {
    render(<DirectMessagesView />);
    expect(screen.getByTestId('conversation-list')).toBeInTheDocument();
  });

  it('renders DMChatArea in chat area', () => {
    render(<DirectMessagesView />);
    expect(screen.getByTestId('dm-chat-area')).toBeInTheDocument();
  });

  it('renders FriendsFlexSpace in member space', () => {
    render(<DirectMessagesView />);
    expect(screen.getByTestId('friends-flex-space')).toBeInTheDocument();
  });

  // --- Active Conversation ---

  it('passes activeConversationId to DMChatArea', () => {
    useDMStore.setState({ activeConversationId: 'conv-42' });
    render(<DirectMessagesView />);
    expect(screen.getByTestId('dm-chat-area')).toHaveAttribute('data-thread', 'conv-42');
  });

  it('passes activeConversationId to ConversationList as selectedThreadId', () => {
    useDMStore.setState({ activeConversationId: 'conv-42' });
    render(<DirectMessagesView />);
    expect(screen.getByTestId('conversation-list')).toHaveAttribute('data-selected', 'conv-42');
  });

  // --- Voice Bar ---

  it('does not render PersistentVoiceBar when not in voice', () => {
    render(<DirectMessagesView />);
    expect(screen.queryByTestId('persistent-voice-bar')).not.toBeInTheDocument();
  });

  it('renders PersistentVoiceBar when in voice', () => {
    useVoiceStore.setState({
      activeChannelId: 'voice-ch-1',
      connectionState: 'connected',
    });
    render(<DirectMessagesView />);
    expect(screen.getByTestId('persistent-voice-bar')).toBeInTheDocument();
  });

  // --- Floating Avatar ---

  it('shows floating UserPanel when channel panel is unpinned and no active conversation', () => {
    useLayoutStore.setState({ channelPanelPinned: false });
    useDMStore.setState({ activeConversationId: null });
    render(<DirectMessagesView />);
    expect(screen.getByTestId('user-panel')).toBeInTheDocument();
  });

  it('does not show floating UserPanel when channel panel is pinned', () => {
    useLayoutStore.setState({ channelPanelPinned: true });
    useDMStore.setState({ activeConversationId: null });
    render(<DirectMessagesView />);
    expect(screen.queryByTestId('user-panel')).not.toBeInTheDocument();
  });

  it('does not show floating UserPanel when a conversation is active', () => {
    useLayoutStore.setState({ channelPanelPinned: false });
    useDMStore.setState({ activeConversationId: 'conv-1' });
    render(<DirectMessagesView />);
    expect(screen.queryByTestId('user-panel')).not.toBeInTheDocument();
  });

  // --- Server Modals ---

  it('opens ServerActionModal from ServerBar add button', () => {
    render(<DirectMessagesView />);
    expect(screen.queryByTestId('server-action-modal')).not.toBeInTheDocument();

    fireEvent.click(screen.getByText('Open Action Modal'));
    expect(screen.getByTestId('server-action-modal')).toBeInTheDocument();
  });

  it('closes ServerActionModal via onClose', () => {
    render(<DirectMessagesView />);
    fireEvent.click(screen.getByText('Open Action Modal'));
    expect(screen.getByTestId('server-action-modal')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Close Action'));
    expect(screen.queryByTestId('server-action-modal')).not.toBeInTheDocument();
  });

  it('opens CreateServerModal from ServerActionModal', () => {
    render(<DirectMessagesView />);
    fireEvent.click(screen.getByText('Open Action Modal'));
    fireEvent.click(screen.getByText('Create Server'));
    expect(screen.getByTestId('create-server-modal')).toBeInTheDocument();
  });

  it('opens JoinServerModal from ServerActionModal', () => {
    render(<DirectMessagesView />);
    fireEvent.click(screen.getByText('Open Action Modal'));
    fireEvent.click(screen.getByText('Join Server'));
    expect(screen.getByTestId('join-server-modal')).toBeInTheDocument();
  });

  it('closes both modals on CreateServerModal success', () => {
    render(<DirectMessagesView />);
    // Open action modal, then create server modal
    fireEvent.click(screen.getByText('Open Action Modal'));
    fireEvent.click(screen.getByText('Create Server'));
    expect(screen.getByTestId('create-server-modal')).toBeInTheDocument();

    // Trigger success
    fireEvent.click(screen.getByText('Create Success'));
    expect(screen.queryByTestId('create-server-modal')).not.toBeInTheDocument();
    expect(screen.queryByTestId('join-server-modal')).not.toBeInTheDocument();
  });

  it('closes both modals on JoinServerModal success', () => {
    render(<DirectMessagesView />);
    fireEvent.click(screen.getByText('Open Action Modal'));
    fireEvent.click(screen.getByText('Join Server'));
    expect(screen.getByTestId('join-server-modal')).toBeInTheDocument();

    fireEvent.click(screen.getByText('Join Success'));
    expect(screen.queryByTestId('join-server-modal')).not.toBeInTheDocument();
    expect(screen.queryByTestId('create-server-modal')).not.toBeInTheDocument();
  });

  // --- Friend Click -> Open DM ---

  it('opens DM conversation when friend is clicked', async () => {
    const mockOpenDM = vi.fn().mockResolvedValue({ id: 'dm-conv-1' });
    const mockSetActive = vi.fn();
    useDMStore.setState({
      openDM: mockOpenDM,
      setActiveConversation: mockSetActive,
    });
    render(<DirectMessagesView />);

    fireEvent.click(screen.getByText('Click Friend'));

    await vi.waitFor(() => {
      expect(mockOpenDM).toHaveBeenCalledWith('friend-user-1');
      expect(mockSetActive).toHaveBeenCalledWith('dm-conv-1');
    });
  });

  it('handles friend click DM open error gracefully', async () => {
    const consoleSpy = vi.spyOn(console, 'error').mockImplementation(() => {});
    const mockOpenDM = vi.fn().mockRejectedValue(new Error('Failed'));
    useDMStore.setState({ openDM: mockOpenDM });
    render(<DirectMessagesView />);

    fireEvent.click(screen.getByText('Click Friend'));

    await vi.waitFor(() => {
      expect(consoleSpy).toHaveBeenCalledWith('Failed to open DM:', expect.any(String));
    });
    consoleSpy.mockRestore();
  });
});
